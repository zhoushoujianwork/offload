// Package worker runs one task through Claude Code or Codex and reports what the worker did.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/live"
	"github.com/zhoushoujianwork/offload/internal/proc"
	"github.com/zhoushoujianwork/offload/internal/slots"
	"github.com/zhoushoujianwork/offload/internal/text"
)

// Prompt is appended to the worker's system prompt.
const Prompt = `You are a worker agent delegated a task by a supervising agent. Work autonomously; do not wait for interactive answers. Report blockers to the supervisor.
Finish with a concise report: what you did or found, exact file:line references, commands run with their results, and anything unverified. No preamble.
Nobody can answer interactive prompts. offload imposes no command, file or network allowlist on you; the task text and the supervisor's authorization define what you may do, so stay inside that scope. The host operating system, network and credentials still apply and may make a step fail.
Use Read, Grep and Glob only when they are exposed to you. If Grep or Glob is missing, use a simple rg or rtk grep command and ls or rtk ls, and do not repeat a call to a tool that is unavailable. If a step fails for any reason, say so in the report instead of looping on it.`

// Shared by both backends so neither worker turns an implementation into open-ended research.
const taskDiscipline = `
Follow the assigned design, scope and allowed files. Do not redesign the task or delegate to other workers.
Start with the smallest targeted inspection needed for the next edit or answer. Use the supplied working directory and relevant installed API declarations; do not scan the filesystem root or unrelated repositories.
For implementation, move from initial localization to a small patch and focused checks once the path is clear. If a critical API assumption is missing or contradicts the installed API, make one focused feasibility check. If that does not resolve it, stop and return a BLOCKED report rather than expanding the search or guessing a replacement design.
The BLOCKED report must state the assumption that failed, exact evidence/file:line references, any edits already made, checks run, and the decision or information needed from the supervisor. This is the final report, not an interactive question.
Keep the final report compact: outcome, changed files or key file:line references, checks run with results, unresolved issues. Do not narrate your actions step by step; the full history stays available to the supervisor. When resumed, treat the follow-up's corrections and acceptance criteria as authoritative and re-read files rather than rely on remembered contents, since earlier context may have been compacted.
Before another exploratory call, identify what it will resolve. If it only repeats an inconclusive search, return the findings and blocker instead. If a call fails or is refused by the host, try at most one different approach; if the required evidence remains inaccessible, report it rather than trying more command variants.
`

// IdleLimit stops a worker that has produced no output for this long. A hung model request
// otherwise holds the run until the full timeout. It is a variable only so tests can shorten it.
var IdleLimit = 10 * time.Minute

var errIdle = errors.New("worker idle")

// Opts is one worker run.
type Opts struct {
	RunID            string
	CallerRunID      string
	ExecutionPolicy  string
	InstructionsFile string
	CodexSandbox     string

	Instructions                  []byte
	ToolsDisabled                 bool
	AllowedTools, DisallowedTools string
	GitRoots                      []string
	OnLine                        func([]byte) error

	ProfileName string
	Profile     config.Profile
	Model       string
	Write       bool
	Resume      string
	Dir         string
	AddDirs     []string // absolute extra directories forwarded to the backend; Dir stays the cwd
	Task        string
	IdleTimeout *time.Duration
	Timeout     time.Duration
	StartedAt   time.Time
	NoSalvage   bool // do not ask a stopped worker for a report from what it already read
	Salvage     bool // internal: this run is the tool-less report of a stopped worker

	// MaxWorkers is the machine-wide worker limit; <= 0 disables admission control. Limits adds
	// build defaults to the worker's environment.
	MaxWorkers int
	Limits     config.Limits
}

// claudeResult is the subset of `claude -p --output-format json` we use.
type claudeResult struct {
	ErrorKind        string              `json:"-"`
	CumulativeUsage  *history.TokenUsage `json:"-"`
	UsageUnavailable string              `json:"-"`
	RawUsage         json.RawMessage     `json:"usage"`      // Claude: this invocation only; decoded by claude_usage.go
	RawModelUsage    json.RawMessage     `json:"modelUsage"` // Claude: session cumulative; decoded by claude_usage.go
	Result           string              `json:"result"`
	IsError          bool                `json:"is_error"`
	Subtype          string              `json:"subtype"`
	Errors           []string            `json:"errors"`
	NumTurns         int                 `json:"num_turns"`
	CostUSD          *float64            `json:"total_cost_usd"`
	DurationMs       int64               `json:"duration_ms"`
	SessionID        string              `json:"session_id"`
}

// salvageTask makes a stopped worker write its report from what it already read.
const salvageTask = "You were stopped before finishing. You have no tools now. Do not ask to read or run anything. " +
	"Write the report the task asked for, using only what you already read. Mark anything you did not verify as unverified, " +
	"and say plainly which parts of the task you did not get to."

// SalvageTimeout bounds the tool-less report; it needs no tools, so it is fast (7-134s measured).
var SalvageTimeout = 4 * time.Minute

func buildArgs(o Opts, promptPath string) []string {
	// Gateway, token and model reach the worker only through env (see childEnv), never argv.
	// --bare still loads settings; disable user/project/local sources so their env cannot
	// override the selected profile's gateway, credentials or model.
	args := []string{"-p", "--bare", "--setting-sources", "", "--output-format", "stream-json", "--verbose"}
	if o.Resume != "" {
		args = append(args, "--resume", o.Resume)
	}

	// Repeated flags precede another known flag, so Claude's variadic --add-dir cannot swallow the task.
	for _, d := range o.AddDirs {
		args = append(args, "--add-dir", d)
	}

	// offload adds no permission barrier: nobody can answer a prompt in a headless run, so every
	// default-policy run without explicit tool controls skips them; -w states task intent. Host
	// OS, network and credential limits still apply. Only the report-only salvage step is
	// tool-less, by design, so it cannot start new work.
	if o.Salvage || (o.ExecutionPolicy != "managed-development" && o.AllowedTools == "" && o.DisallowedTools == "" && !o.ToolsDisabled) {
		args = append(args, "--dangerously-skip-permissions")
	}
	if o.Salvage {
		args = append(args, "--tools", "")
	} else if o.ToolsDisabled {
		args = append(args, "--tools", "")
	} else if o.AllowedTools != "" {
		// Explicit allowlists must not inherit automatic approval for edits.
		args = append(args, "--permission-mode", "default", "--allowedTools", strings.ReplaceAll(o.AllowedTools, ",", " "))
	} else if o.ExecutionPolicy == "managed-development" {
		args = append(args, "--permission-mode", "acceptEdits", "--allowedTools", "Bash Read Grep Glob Edit Write")
	}
	if o.DisallowedTools != "" {
		args = append(args, "--disallowedTools", strings.ReplaceAll(o.DisallowedTools, ",", " "))
	}
	args = append(args, "--append-system-prompt-file", promptPath)
	if o.ExecutionPolicy == "managed-development" {
		return args // managed task contents travel only through stdin
	}
	return append(args, o.Task)
}

// writeWorkerPrompt combines explicit project instructions and delegated-task constraints.
// Claude accepts only one append prompt mechanism; a private file keeps its contents out of argv.
func writeWorkerPrompt(o Opts) (string, error) {
	var prompt strings.Builder
	// --bare skips project instruction discovery, so read the files explicitly.
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		path := filepath.Join(o.Dir, name)
		contents, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read project instructions %s: %w", path, err)
		}
		fmt.Fprintf(&prompt, "Project instructions from %s:\n%s\n\n", path, contents)
	}
	if len(o.Instructions) > 0 {
		prompt.Write(o.Instructions)
		prompt.WriteString("\n\n")
	}
	if o.ExecutionPolicy == "managed-development" {
		prompt.WriteString(managedPrompt)
	} else if o.Profile.BackendName() == "codex" {
		prompt.WriteString(codexPrompt)
	} else {
		prompt.WriteString(Prompt)
	}
	prompt.WriteString(taskDiscipline)
	if !o.Write {
		// Task intent for the model only; offload enforces nothing here.
		prompt.WriteString("\nRead-only task: do not modify files.")
	}
	prompt.WriteByte('\n')

	f, err := os.CreateTemp("", "offload-prompt-*.md") // CreateTemp uses mode 0600.
	if err != nil {
		return "", fmt.Errorf("create worker prompt: %w", err)
	}
	path := f.Name()
	if _, err := f.WriteString(prompt.String()); err != nil {
		f.Close()
		os.Remove(path)
		return "", fmt.Errorf("write worker prompt: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close worker prompt: %w", err)
	}
	return path, nil
}

// childEnv makes the profile the worker's only gateway: every caller ANTHROPIC_* variable is
// dropped, then exactly base URL, token and model are set. buildArgs disables settings sources
// so user/project/local settings cannot override these.
func childEnv(base []string, o Opts, token string) []string {
	env := make([]string, 0, len(base)+3)
	for _, kv := range base {
		if !strings.HasPrefix(kv, "ANTHROPIC_") {
			env = append(env, kv)
		}
	}
	return append(env, "ANTHROPIC_BASE_URL="+o.Profile.BaseURL, "ANTHROPIC_AUTH_TOKEN="+token, "ANTHROPIC_MODEL="+o.Model)
}

func claudeBin() (string, error) {
	if b := os.Getenv("OFFLOAD_CLAUDE"); b != "" {
		return b, nil
	}
	home, _ := os.UserHomeDir()
	if p := filepath.Join(home, ".claude", "local", "claude"); fileExists(p) {
		return p, nil
	}
	return exec.LookPath("claude")
}

// StopGrace is how long a stopped worker's process group gets to exit after SIGTERM before it is
// killed; KillWait bounds the wait for the kernel after SIGKILL. Variables only so tests can shorten them.
var (
	StopGrace = 5 * time.Second
	KillWait  = 3 * time.Second
)

// Run executes the worker and always returns a Record describing what happened.
//
// The run first waits for a worker slot, so at most MaxWorkers run on the machine. It keeps the
// slot through the worker, any salvage step and cleanup of the worker's process group, and gives
// it up only once those processes are confirmed gone.
func Run(ctx context.Context, o Opts) (rec history.Record, err error) {
	if o.RunID == "" {
		o.RunID = NewRunID()
	}
	started := o.StartedAt
	if started.IsZero() {
		started = time.Now()
	}
	rec = history.Record{TS: started.Format("2006-01-02 15:04:05"), Profile: o.ProfileName, Model: o.Model,
		RunID: o.RunID, CallerRunID: o.CallerRunID, ExecutionPolicy: policyName(o.ExecutionPolicy), Backend: o.Profile.BackendName(), Dir: o.Dir, AddDirs: slices.Clone(o.AddDirs), Task: o.Task, Status: "error", CostIsEstimate: true}
	var pub *publisher
	var slot *slots.Slot
	var admitted time.Time
	var leftover *proc.Group // worker processes that survived cleanup
	defer func() {
		if rec.Backend == "codex" {
			resolveCodexModel(&rec, o.Model)
		}
		elapsed := time.Since(started).Milliseconds()
		rec.ElapsedMs = &elapsed
		if !admitted.IsZero() {
			wait, exec := admitted.Sub(started).Milliseconds(), time.Since(admitted).Milliseconds()
			rec.WaitMs, rec.ExecMs = &wait, &exec
		}
		code := 1
		if err != nil {
			code, rec.Error, rec.Result = 2, err.Error(), err.Error()
		} else if rec.Status == "ok" {
			code = 0
		} else if rec.Status == "timeout" {
			code = 124
		}
		rec.ExitCode = &code
		if rec.Status != "ok" && rec.ErrorKind == "" {
			rec.ErrorKind = "execution"
		}
		if rec.Status == "ok" {
			rec.ErrorKind = ""
		}
		if rec.ErrorKind == "capacity" || rec.ErrorKind == "rate_limit" {
			rec.Error, rec.Result = rec.ErrorKind, rec.ErrorKind
		}
		// Give the slot up only after cleanup. A group that is still alive keeps the slot file
		// recording it, so the capacity stays unavailable.
		if leftover != nil && !leftover.Alive() {
			leftover = nil
		}
		if !slot.Release() && leftover == nil {
			leftover = &proc.Group{}
		}
		if leftover != nil {
			rec.Cleanup = "unresolved: worker processes are still running"
			if leftover.PGID > 1 {
				rec.Cleanup += fmt.Sprintf(" in process group %d; stop them with: kill -TERM -- -%d", leftover.PGID, leftover.PGID)
			}
			rec.Cleanup += "; their worker slot stays unavailable until they exit"
		}
		// Publish only after the exit code is final, or watch/status would show 0 for a failure.
		if pub != nil {
			pub.finish(rec, leftover)
		}
	}()
	if err = config.CheckEnabled(); err != nil {
		rec.ErrorKind = ErrorKind(err)
		return rec, err
	}
	if err = Validate(&o); err != nil {
		rec.ErrorKind = ErrorKind(err)
		return rec, err
	}
	limit, limitErr := o.idleLimit()
	if limitErr != nil {
		rec.ErrorKind = "configuration"
		return rec, limitErr
	}
	rec.IdleTimeout = limit.String()
	rec.ErrorKind = "configuration"
	promptPath, err := writeWorkerPrompt(o)
	if err != nil {
		return rec, err
	}
	defer os.Remove(promptPath)
	var token, bin string
	if o.Profile.BackendName() == "codex" {
		rec.ErrorKind = "launch"
		bin, err = codexBin()
		if err != nil {
			return rec, fmt.Errorf("codex CLI not found: %w", err)
		}
	} else {
		token, err = o.Profile.ResolveTokenContext(ctx)
		if err != nil {
			return rec, err
		}
		rec.ErrorKind = "launch"
		bin, err = claudeBin()
		if err != nil {
			return rec, fmt.Errorf("claude CLI not found: %w", err)
		}
		if err := Preflight(o.Profile.BaseURL); err != nil {
			return rec, err
		}
	}
	rec.ErrorKind = "launch"
	pub = newPublisher(o, started)
	slot, err = slots.Acquire(ctx, slots.Options{Dir: slots.Dir(), Max: o.MaxWorkers, OnWait: pub.waiting, Check: config.CheckEnabled})
	if err != nil {
		if ctx.Err() != nil {
			rec.StopReason = stopReason(context.Cause(ctx))
			rec.ErrorKind = "canceled"
			return rec, errors.New("cancelled while waiting for a worker slot; no worker was started")
		}
		rec.ErrorKind = ErrorKind(err)
		return rec, fmt.Errorf("acquire worker slot: %w", err)
	}
	if err = config.CheckEnabled(); err != nil {
		rec.ErrorKind = ErrorKind(err)
		return rec, err
	}
	admitted = time.Now()
	pub.admitted(slot.Index(), admitted.Sub(started))
	hooks := hooks{started: func(g proc.Group) { slot.Track(g); pub.group(g) }, cleaning: func() { pub.phase(live.Cleaning) }, cleaned: slot.Clear}

	a := runAttempt(ctx, bin, o, promptPath, token, o.Timeout, pub.update, hooks)
	if a.gateErr != nil {
		rec.ErrorKind = ErrorKind(a.gateErr)
		return rec, a.gateErr
	}
	pub.phase(live.Running)
	if a.leftover != nil {
		leftover = a.leftover
	}
	s := a.s
	rec.StopReason = stopReason(a.cause)
	rec.ErrorKind = "execution"
	var launchErr *exec.Error
	var pathErr *os.PathError
	if errors.As(a.runErr, &launchErr) || errors.As(a.runErr, &pathErr) {
		rec.ErrorKind = "launch"
	}
	// 信号保留独立停止原因，兼容原有 canceled 错误分类。
	if rec.StopReason == "canceled" || rec.StopReason == "signal" {
		rec.ErrorKind = "canceled"
	}
	if errors.Is(a.cause, errEventsIO) {
		rec.ErrorKind = "events_io"
	}
	if o.Profile.BackendName() == "codex" {
		defer func() {
			applyCodexAccounting(&rec, s, a.baseline, a.baselineError, o, a.runErr == nil && a.cause == nil)
		}()
	}
	rec.Session, rec.Turns, rec.Denials = s.Session, s.ToolCalls, s.Denials
	stopped := errors.Is(a.cause, errIdle) || errors.Is(a.cause, context.DeadlineExceeded)
	if stopped {
		rec.Status = "timeout"
		rec.ErrorKind = "timeout"
		rec.Error = "timed out after " + o.Timeout.String()
		if errors.Is(a.cause, errIdle) {
			rec.Error = "worker produced no activity for " + limit.String()
		}
	} else if s.Result == nil {
		if s.Events == 0 {
			rec.Result = strings.TrimSpace(a.stdout)
			if rec.Result == "" {
				rec.Result = strings.TrimSpace(a.stderr)
			}
			if rec.Result == "" && a.runErr != nil {
				rec.Result = a.runErr.Error()
			}
			markClaudeNoResult(&rec)
			rec.Error = "worker returned invalid JSON"
			if a.runErr != nil {
				rec.Error += ": " + a.runErr.Error()
			}
			return rec, nil
		}
		rec.Error = "worker ended without a result"
		if errors.Is(a.cause, context.Canceled) {
			rec.Error = "interrupted before the worker finished"
		}
		if a.runErr != nil {
			rec.Error += ": " + a.runErr.Error()
		}
	}
	if stopped || s.Result == nil {
		rec.Error += s.resumeHint()
		rec.Result = rec.Error + s.progress()
		markClaudeNoResult(&rec)
		// A stopped worker has usually read plenty. Ask it for the report without tools instead
		// of leaving the supervisor with a progress sketch. Not after the user pressed Ctrl-C,
		// not while the first worker's processes are still alive, and never for managed development.
		if o.ExecutionPolicy != "managed-development" && !o.NoSalvage && o.Profile.BackendName() == "claude" && s.Session != "" && ctx.Err() == nil && leftover == nil {
			pub.phase(live.Salvaging)
			report, sres, left, serr := salvageAttempt(ctx, bin, o, promptPath, token, s.Session, pub.keepAlive, hooks)
			if left != nil {
				leftover = left
			}
			if errors.Is(serr, config.ErrDisabled) {
				rec.ErrorKind = ErrorKind(serr)
				rec.Error += "; automatic salvage refused: " + serr.Error()
			}
			if serr == nil {
				rec.Salvaged = true
				applyClaudeSalvageAccounting(&rec, sres)
				rec.Error += "; the report is salvaged from what the worker read before it stopped, not a finished run (cost_usd is the value the report step reported, not a full-run cost)"
				rec.Result = "[SALVAGED REPORT: " + rec.Error + "]\n\n" + report
			} else {
				// Keep the original failure and progress; add why the extra step produced no report.
				hint := s.resumeHint()
				rec.Error = strings.TrimSuffix(rec.Error, hint) + "; salvage failed: " +
					text.Truncate(text.OneLine(serr.Error()), 200) + hint
				rec.Result = rec.Error + s.progress()
			}
		}
		return rec, nil
	}
	res := *s.Result
	rec.Session, rec.Turns, rec.DurationMs, rec.Result =
		firstNonEmpty(res.SessionID, s.Session), res.NumTurns, res.DurationMs, res.Result
	applyClaudeAccounting(&rec, &res, o, a.claudeBase)
	if !res.IsError && a.runErr == nil && a.cause == nil {
		rec.Status = "ok"
	} else {
		if res.IsError && res.ErrorKind != "" && rec.ErrorKind == "execution" {
			rec.ErrorKind = res.ErrorKind
		}
		for _, message := range res.Errors {
			if message = strings.TrimSpace(message); message != "" {
				if rec.Error != "" {
					rec.Error += "; "
				}
				rec.Error += message
			}
		}
		if res.Subtype != "" && res.Subtype != "success" {
			rec.Error = res.Subtype + ": " + firstNonEmpty(rec.Error, "worker reported an error")
		}
		if rec.Error == "" {
			if a.runErr != nil {
				rec.Error = a.runErr.Error()
			} else {
				rec.Error = "worker reported an error"
			}
		}
		if rec.Result == "" {
			rec.Result = rec.Error
		}
	}
	return rec, nil
}

// attempt is the outcome of one worker process.
type attempt struct {
	gateErr        error // admission failure before any backend process starts
	baseline       *history.TokenUsage
	baselineError  string
	claudeBase     claudeBaseline // resumed normal Claude run only
	s              streamSummary
	cause          error
	runErr         error
	stdout, stderr string
	// leftover is the worker's process group when processes in it could not be stopped.
	leftover *proc.Group
}

// hooks lets the caller record the worker's process group while it is the slot's responsibility.
type hooks struct {
	started  func(proc.Group)
	cleaning func()
	cleaned  func()
}

// runAttempt starts one worker process and waits for it. It stops early on the timeout or when the
// worker goes silent, and calls tick every publishEvery with what the worker has done so far.
//
// The worker leads its own process group. When the attempt ends, by itself or by cancellation, the
// whole group is stopped (SIGTERM, then SIGKILL after StopGrace), so build and test children do not
// outlive the run. A descendant that left the group with setsid is not reachable this way.
func runAttempt(ctx context.Context, bin string, o Opts, promptPath, token string, timeout time.Duration,
	tick func(streamSummary, time.Time), h hooks) attempt {
	attemptOpts := o
	attemptOpts.Timeout = timeout
	limit, err := attemptOpts.idleLimit()
	if err != nil {
		return attempt{runErr: err}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx, cancelCause := context.WithCancelCause(ctx)
	defer cancelCause(nil)
	var cmd *exec.Cmd
	var parser eventParser = &claudeParser{}
	var baseline *history.TokenUsage
	var baselineError string
	var claudeBase claudeBaseline
	// Not CommandContext: that kills only the worker process and never its children.
	if o.Profile.BackendName() == "codex" {
		baseline, baselineError = codexBaseline(o.Resume)
		cmd = exec.Command(bin, buildCodexArgs(o)...)
		cmd.Dir = o.Dir
		cmd.Env = o.Limits.ApplyEnv(os.Environ())
		prompt, err := os.ReadFile(promptPath)
		if err != nil {
			return attempt{runErr: err}
		}
		cmd.Stdin = strings.NewReader(string(prompt) + "\nTask:\n" + o.Task)
		parser = &codexParser{}
	} else {
		if o.Resume != "" && !o.Salvage {
			// Capture before the child starts: the result's cost and modelUsage are session totals.
			claudeBase = captureClaudeBaseline(history.Path(), o.Resume)
		}
		cmd = exec.Command(bin, buildArgs(o, promptPath)...)
		cmd.Dir = o.Dir
		cmd.Env = o.Limits.ApplyEnv(childEnv(os.Environ(), o, token))
		if o.ExecutionPolicy == "managed-development" {
			cmd.Stdin = strings.NewReader(o.Task)
		}
	}
	proc.Prepare(cmd)
	out := newActivityWriter(parser)
	if o.OnLine != nil {
		out.onLine = func(line []byte) error {
			if err := o.OnLine(line); err != nil {
				cancelCause(errEventsIO)
				return fmt.Errorf("%w: %v", errEventsIO, err)
			}
			return nil
		}
	}
	var workerErr cappedBuffer
	cmd.Stdout, cmd.Stderr = out, &livenessWriter{w: &workerErr, last: &out.last}
	// Do not wait indefinitely for a worker descendant to close inherited pipes.
	cmd.WaitDelay = 100 * time.Millisecond
	files := newSessionActivity(o)
	// Every process launch, including automatic salvage, must observe the saved switch.
	if err := config.CheckEnabled(); err != nil {
		return attempt{runErr: err, gateErr: err}
	}
	if err := cmd.Start(); err != nil {
		return attempt{runErr: err}
	}
	group := proc.Started(cmd)
	if h.started != nil {
		h.started(group)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	if limit > 0 {
		go watchIdle(ctx, cancelCause, &out.last, limit, func() bool { return files.grew(firstNonEmpty(out.summary().Session, o.Resume)) })
	}
	if tick != nil {
		go func() {
			t := time.NewTicker(publishEvery)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					tick(out.summary(), activityEpoch.Add(time.Duration(out.last.Load())))
				}
			}
		}()
	}

	var runErr error
	select {
	case runErr = <-waited:
	case <-ctx.Done():
		// Timeout, idle limit or interrupt: stop the group, then collect the worker's exit.
		group.Term()
		select {
		case runErr = <-waited:
		case <-time.After(StopGrace):
			group.Kill()
			select {
			case runErr = <-waited:
			case <-time.After(KillWait):
			}
		}
	}
	cause := context.Cause(ctx)
	if h.cleaning != nil && group.Alive() {
		h.cleaning()
	}
	out.finish()
	a := attempt{s: out.summary(), cause: cause, runErr: runErr, baseline: baseline, baselineError: baselineError, claudeBase: claudeBase,
		stdout: out.String(), stderr: workerErr.String()}
	if !stopGroup(group) {
		g := group
		a.leftover = &g
	} else if h.cleaned != nil {
		h.cleaned()
	}
	return a
}

// stopGroup makes sure no process is left in the worker's group, escalating from SIGTERM to
// SIGKILL, and reports whether the group is confirmed gone. A normal exit usually leaves nothing
// to stop. It never signals anything outside the group offload created.
func stopGroup(g proc.Group) bool {
	if !proc.Supported {
		return true // nothing is tracked; see Known limits
	}
	if !g.Alive() {
		return true
	}
	g.Term()
	if waitGone(g, StopGrace) {
		return true
	}
	g.Kill()
	return waitGone(g, KillWait)
}

func waitGone(g proc.Group, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if !g.Alive() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// cappedBuffer keeps the start of what the worker wrote to stderr; the rest is dropped.
type cappedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

const maxStderrKept = 64 << 10

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := maxStderrKept - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// salvage resumes a stopped session with no tools and returns the report it writes. The error says
// why there is no report. It never contains worker stderr, which may echo gateway details. A report
// from a process that failed is discarded, because it may be cut short or come from a broken run.
func salvage(ctx context.Context, bin string, o Opts, promptPath, token, session string, tick func(streamSummary, time.Time)) (string, *float64, error) {
	report, r, err := salvageResult(ctx, bin, o, promptPath, token, session, tick)
	if err != nil {
		return "", nil, err
	}
	cost := r.CostUSD
	if cost != nil && !history.ValidCost(*cost) {
		cost = nil
	}
	return report, cost, nil
}

// salvageResult is salvage returning the whole final result, so Run can keep its session snapshots.
func salvageResult(ctx context.Context, bin string, o Opts, promptPath, token, session string, tick func(streamSummary, time.Time)) (string, *claudeResult, error) {
	report, result, _, err := salvageAttempt(ctx, bin, o, promptPath, token, session, tick, hooks{})
	return report, result, err
}

func salvageAttempt(ctx context.Context, bin string, o Opts, promptPath, token, session string, tick func(streamSummary, time.Time), h hooks) (string, *claudeResult, *proc.Group, error) {
	so := o
	so.Resume, so.Salvage, so.Write, so.Task = session, true, false, salvageTask
	so.Timeout = SalvageTimeout
	limit, _ := so.idleLimit()
	a := runAttempt(ctx, bin, so, promptPath, token, SalvageTimeout, tick, h)
	if a.gateErr != nil {
		return "", nil, a.leftover, a.gateErr
	}
	switch {
	case ctx.Err() != nil:
		return "", nil, a.leftover, errors.New("cancelled during the report step")
	case errors.Is(a.cause, errIdle):
		return "", nil, a.leftover, fmt.Errorf("the report step produced no output for %s", limit)
	case errors.Is(a.cause, context.DeadlineExceeded):
		return "", nil, a.leftover, fmt.Errorf("the report step timed out after %s", SalvageTimeout)
	case a.runErr != nil:
		var exit *exec.ExitError
		if errors.As(a.runErr, &exit) {
			return "", nil, a.leftover, fmt.Errorf("the report step process failed (%s), its output was discarded", exit.ProcessState)
		}
		return "", nil, a.leftover, errors.New("the report step process could not run")
	}
	r := a.s.Result
	switch {
	case r == nil:
		return "", nil, a.leftover, errors.New("the report step ended without a result")
	case r.IsError:
		reason := "worker reported an error"
		if r.Subtype != "" && r.Subtype != "success" {
			reason = r.Subtype
		}
		return "", nil, a.leftover, fmt.Errorf("the report step returned an error (%s)", reason)
	case strings.TrimSpace(r.Result) == "":
		return "", nil, a.leftover, errors.New("the report step returned an empty report")
	}
	return r.Result, r, a.leftover, nil
}

// publishEvery is how often a running worker refreshes its live state.
const publishEvery = 2 * time.Second

// publisher keeps this process's live state current. It is advisory; failures are ignored.
type publisher struct {
	mu         sync.Mutex
	st         live.State
	runID      string
	processing bool // a processing span is open in the processing log
}

// track keeps the durable processing log in step with the phase being published, so the daily
// peak can be rebuilt after live files are gone. Only running and salvaging occupy a worker;
// queue wait, cleanup and the finished states close the span. Failures are ignored: the log is
// advisory, and a missing end event is read back as an incomplete span. Callers hold p.mu.
func (p *publisher) track(phase string) {
	on := phase == live.Running || phase == live.Salvaging
	if on == p.processing || p.runID == "" {
		return
	}
	event := history.ProcessingEnd
	if on {
		event = history.ProcessingStart
	}
	if history.AppendProcessing(history.ProcessingPath(), p.runID, os.Getpid(), event, time.Now()) == nil {
		p.processing = on
	}
}

func newPublisher(o Opts, started time.Time) *publisher {
	// Queued first: the run holds no slot and has started no backend. A resumed run is already in
	// its session, so activeSessionRun can refuse an overlapping follow-up from the start.
	p := &publisher{runID: o.RunID, st: live.State{PID: os.Getpid(), RunID: o.RunID, Phase: live.Queued, Dir: o.Dir, Session: o.Resume,
		Task: text.Truncate(text.OneLine(o.Task), 200), Profile: o.ProfileName, Model: o.Model,
		StartedAt: started, LastEventAt: started, Limits: limitsSummary(o)}}
	live.Publish(p.st)
	return p
}

func limitsSummary(o Opts) string {
	return o.Limits.Summary(max(o.MaxWorkers, 0))
}

// waiting is called while the run is queued. It refreshes the state so the queue stays visible
// and the run is never reported lost.
func (p *publisher) waiting(w slots.Wait) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.st.WaitReason = w.Reason()
	p.st.WaitMs = time.Since(p.st.StartedAt).Milliseconds()
	live.Publish(p.st)
}

// admitted marks the move from queue to running. LastEventAt restarts here so queue time is not
// counted as worker idle time.
func (p *publisher) admitted(slot int, waited time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.track(live.Running)
	p.st.Phase, p.st.WaitReason, p.st.WaitMs, p.st.LastEventAt = live.Running, "", waited.Milliseconds(), time.Now()
	if slot >= 0 {
		p.st.Slot = &slot
	}
	live.Publish(p.st)
}

func (p *publisher) group(g proc.Group) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.st.PGID, p.st.PGIDStart = g.PGID, g.Start
	live.Publish(p.st)
}

func (p *publisher) update(s streamSummary, last time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s.Session != "" {
		p.st.Session = s.Session
	}
	p.st.ToolCalls, p.st.Denials, p.st.LastEventAt = s.ToolCalls, s.Denials, last
	if n := len(s.LastTools); n > 0 {
		p.st.LastTool = s.LastTools[n-1]
	}
	live.Publish(p.st)
}

// keepAlive refreshes the state without touching the first run's counters. The tool-less report
// takes a minute or more, and a state nobody refreshes is reported as lost.
func (p *publisher) keepAlive(_ streamSummary, _ time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	live.Publish(p.st)
}

func (p *publisher) phase(phase string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if phase == live.Running && p.st.Phase == live.Cleaning {
		return // the worker is already gone; finishing the record is not processing
	}
	p.track(phase)
	p.st.Phase = phase
	live.Publish(p.st)
}

// finish publishes the final state. A non-nil leftover means worker processes survived cleanup, so
// the state is Unresolved rather than Done and keeps the group id for status and for slot recovery.
func (p *publisher) finish(rec history.Record, leftover *proc.Group) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.track(live.Done)
	p.st.Phase, p.st.Status, p.st.ToolCalls, p.st.Denials = live.Done, rec.Status, rec.Turns, rec.Denials
	p.st.WaitReason = ""
	if leftover != nil {
		p.st.Phase = live.Unresolved
		p.st.WaitReason = rec.Cleanup
	} else {
		p.st.PGID, p.st.PGIDStart = 0, ""
	}
	if rec.Session != "" {
		p.st.Session = rec.Session
	}
	if rec.ExitCode != nil {
		p.st.ExitCode = *rec.ExitCode
	}
	live.Publish(p.st)
}

// Preflight is swapped out in tests.
var Preflight = CheckReachable

// CheckReachable fails fast when the gateway cannot be dialed. Without it, a worker inside a
// network-less sandbox inherited from the host silently retries until the timeout.
func CheckReachable(baseURL string) error {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		if os.Getenv(k) != "" {
			return nil // a proxy decides reachability; a direct dial would be a false negative
		}
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("bad base_url %q", baseURL)
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80"}[u.Scheme]
		if port == "" {
			port = "443"
		}
	}
	c, err := net.DialTimeout("tcp", net.JoinHostPort(u.Hostname(), port), 5*time.Second)
	if err != nil {
		return fmt.Errorf("cannot reach gateway %s (%v); if running inside a sandbox, allow network or run offload outside it", u.Host, err)
	}
	return c.Close()
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
