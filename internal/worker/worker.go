// Package worker runs one task through Claude Code or Codex and reports what the worker did.
package worker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/live"
	"github.com/zhoushoujianwork/offload/internal/text"
)

// Prompt is appended to the worker's system prompt.
const Prompt = `You are a worker agent delegated a task by a supervising agent. Work autonomously; do not wait for interactive answers. Report blockers to the supervisor.
Finish with a concise report: what you did or found, exact file:line references, commands run with their results, and anything unverified. No preamble.
Nobody can approve permission prompts. Shell is limited to single simple commands: pipes, &&, ;, redirects, heredocs and python -c are refused and waste a turn. Prefer Read, Grep and Glob. When a command is refused, switch tools instead of retrying it.
rtk-prefixed read commands (rtk ls, rtk read, rtk grep, rtk git log) are allowed when the task asks for them; rtk run, proxy, err, summary and pipe are not.`

// Shared by both backends so neither worker turns an implementation into open-ended research.
const taskDiscipline = `
Follow the assigned design, scope and allowed files. Do not redesign the task or delegate to other workers.
Start with the smallest targeted inspection needed for the next edit or answer. Use the supplied working directory and relevant installed API declarations; do not scan the filesystem root or unrelated repositories.
For implementation, move from initial localization to a small patch and focused checks once the path is clear. If a critical API assumption is missing or contradicts the installed API, make one focused feasibility check. If that does not resolve it, stop and return a BLOCKED report rather than expanding the search or guessing a replacement design.
The BLOCKED report must state the assumption that failed, exact evidence/file:line references, any edits already made, checks run, and the decision or information needed from the supervisor. This is the final report, not an interactive question.
Keep the final report compact: outcome, changed files or key file:line references, checks run with results, unresolved issues. Do not narrate your actions step by step; the full history stays available to the supervisor. When resumed, treat the follow-up's corrections and acceptance criteria as authoritative and re-read files rather than rely on remembered contents, since earlier context may have been compacted.
Before another exploratory call, identify what it will resolve. If it only repeats an inconclusive search, return the findings and blocker instead. After a permission denial, switch to an allowed tool; if the required evidence remains inaccessible, report it rather than trying more command variants.
`

// rtk compresses command output to save tokens. Only its read-only subcommands are allowed:
// rtk run, proxy, err, summary, pipe and test execute arbitrary commands and would bypass the
// tool lists below, and rtk find accepts find's -exec.
const (
	readOnlyTools = "Read Grep Glob Bash(git log:*) Bash(git diff:*) Bash(git show:*) Bash(ls:*) Bash(rg:*) " +
		"Bash(rtk ls:*) Bash(rtk tree:*) Bash(rtk read:*) Bash(rtk grep:*) Bash(rtk rg:*) Bash(rtk wc:*) " +
		"Bash(rtk git log:*) Bash(rtk git diff:*) Bash(rtk git show:*) Bash(rtk git status:*) Bash(rtk diff:*) Bash(rtk recall:*)"
	// Write mode: edits stay inside the working dir (acceptEdits); shell is limited to build/test/inspect.
	// Deliberately no commit, push, rm, or network tools.
	writeTools = "Read Grep Glob Edit Write Bash(go build:*) Bash(go test:*) Bash(go vet:*) Bash(gofmt:*) " +
		"Bash(make:*) Bash(npm test:*) Bash(npm run:*) Bash(pytest:*) Bash(cargo build:*) Bash(cargo test:*) " +
		"Bash(idf.py build:*) Bash(git status:*) Bash(git diff:*) Bash(ls:*) Bash(rg:*) " +
		"Bash(rtk ls:*) Bash(rtk read:*) Bash(rtk grep:*) Bash(rtk rg:*) Bash(rtk wc:*) Bash(rtk git status:*) Bash(rtk git diff:*) " +
		"Bash(rtk go build:*) Bash(rtk go test:*) Bash(rtk go vet:*) Bash(rtk cargo build:*) Bash(rtk cargo test:*) Bash(rtk pytest:*)"
)

// IdleLimit stops a worker that has produced no output for this long. A hung model request
// otherwise holds the run until the full timeout. It is a variable only so tests can shorten it.
var IdleLimit = 10 * time.Minute

var errIdle = errors.New("worker idle")

// activityWriter collects the worker's stdout and remembers when it last wrote. The buffer is a
// named field, not embedded: an embedded bytes.Buffer would promote ReadFrom, io.Copy would call
// that directly, and Write (hence the timestamp) would never run.
type activityWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	last atomic.Int64 // unix nanoseconds of the latest write
}

func (w *activityWriter) Write(p []byte) (int, error) {
	w.last.Store(time.Now().UnixNano())
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// Bytes returns a copy, because the live publisher reads while the worker is still writing.
func (w *activityWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.buf.Bytes())
}

func (w *activityWriter) String() string { return string(w.Bytes()) }

// watchIdle cancels ctx with errIdle once nothing was written for limit.
func watchIdle(ctx context.Context, cancel context.CancelCauseFunc, last *atomic.Int64, limit time.Duration) {
	t := time.NewTicker(limit / 10)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if time.Since(time.Unix(0, last.Load())) > limit {
				cancel(errIdle)
				return
			}
		}
	}
}

// Opts is one worker run.
type Opts struct {
	ProfileName string
	Profile     config.Profile
	Model       string
	Write       bool
	Resume      string
	Dir         string
	Task        string
	Timeout     time.Duration
	StartedAt   time.Time
	NoSalvage   bool // do not ask a stopped worker for a report from what it already read
	Salvage     bool // internal: this run is the tool-less report of a stopped worker
}

// claudeResult is the subset of `claude -p --output-format json` we use.
type claudeResult struct {
	Result     string   `json:"result"`
	IsError    bool     `json:"is_error"`
	Subtype    string   `json:"subtype"`
	Errors     []string `json:"errors"`
	NumTurns   int      `json:"num_turns"`
	CostUSD    *float64 `json:"total_cost_usd"`
	DurationMs int64    `json:"duration_ms"`
	SessionID  string   `json:"session_id"`
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

	if o.Salvage {
		args = append(args, "--tools", "")
	} else if o.Write {
		args = append(args, "--permission-mode", "acceptEdits", "--allowedTools", writeTools)
	} else {
		args = append(args, "--allowedTools", readOnlyTools)
	}
	return append(args, "--append-system-prompt-file", promptPath, o.Task)
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
	if o.Profile.BackendName() == "codex" {
		prompt.WriteString(codexPrompt)
	} else {
		prompt.WriteString(Prompt)
	}
	prompt.WriteString(taskDiscipline)
	if !o.Write {
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

// Run executes the worker and always returns a Record describing what happened.
func Run(ctx context.Context, o Opts) (rec history.Record, err error) {
	started := o.StartedAt
	if started.IsZero() {
		started = time.Now()
	}
	rec = history.Record{TS: started.Format("2006-01-02 15:04:05"), Profile: o.ProfileName, Model: o.Model,
		Backend: o.Profile.BackendName(), Dir: o.Dir, Task: o.Task, Status: "error", CostIsEstimate: true}
	defer func() {
		elapsed := time.Since(started).Milliseconds()
		rec.ElapsedMs = &elapsed
		code := 1
		if err != nil {
			code, rec.Error, rec.Result = 2, err.Error(), err.Error()
		} else if rec.Status == "ok" {
			code = 0
		} else if rec.Status == "timeout" {
			code = 124
		}
		rec.ExitCode = &code
	}()
	promptPath, err := writeWorkerPrompt(o)
	if err != nil {
		return rec, err
	}
	defer os.Remove(promptPath)
	var token, bin string
	if o.Profile.BackendName() == "codex" {
		bin, err = codexBin()
		if err != nil {
			return rec, fmt.Errorf("codex CLI not found: %w", err)
		}
	} else {
		token, err = o.Profile.ResolveToken()
		if err != nil {
			return rec, err
		}
		bin, err = claudeBin()
		if err != nil {
			return rec, fmt.Errorf("claude CLI not found: %w", err)
		}
		if err := Preflight(o.Profile.BaseURL); err != nil {
			return rec, err
		}
	}
	pub := newPublisher(o, started)
	defer func() { pub.finish(rec) }()
	a := runAttempt(ctx, bin, o, promptPath, token, o.Timeout, pub.update)
	s := a.s
	rec.Session, rec.Turns, rec.Denials = s.Session, s.ToolCalls, s.Denials
	stopped := errors.Is(a.cause, errIdle) || errors.Is(a.cause, context.DeadlineExceeded)
	if stopped {
		rec.Status = "timeout"
		rec.Error = "timed out after " + o.Timeout.String()
		if errors.Is(a.cause, errIdle) {
			rec.Error = "worker produced no output for " + IdleLimit.String()
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
			rec.Error = "worker returned invalid JSON"
			if a.runErr != nil {
				rec.Error += ": " + a.runErr.Error()
			}
			return rec, nil
		}
		rec.Error = "worker ended without a result"
		if a.runErr != nil {
			rec.Error += ": " + a.runErr.Error()
		}
	}
	if stopped || s.Result == nil {
		rec.Error += s.resumeHint()
		rec.Result = rec.Error + s.progress()
		// A stopped worker has usually read plenty. Ask it for the report without tools instead
		// of leaving the supervisor with a progress sketch. Not after the user pressed Ctrl-C.
		if !o.NoSalvage && o.Profile.BackendName() == "claude" && s.Session != "" && ctx.Err() == nil {
			pub.phase(live.Salvaging)
			if report, cost, ok := salvage(ctx, bin, o, promptPath, token, s.Session, pub.keepAlive); ok {
				rec.Salvaged, rec.CostUSD = true, cost
				rec.Error += "; the report is salvaged from what the worker read before it stopped, not a finished run (cost covers only that step)"
				rec.Result = "[SALVAGED REPORT: " + rec.Error + "]\n\n" + report
			}
		}
		return rec, nil
	}
	res := *s.Result
	rec.Session, rec.Turns, rec.CostUSD, rec.DurationMs, rec.Result =
		firstNonEmpty(res.SessionID, s.Session), res.NumTurns, res.CostUSD, res.DurationMs, res.Result
	if rec.CostUSD != nil && !history.ValidCost(*rec.CostUSD) {
		rec.CostUSD = nil
	}
	if !res.IsError && a.runErr == nil {
		rec.Status = "ok"
	} else {
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
	s              streamSummary
	cause          error
	runErr         error
	stdout, stderr string
}

// runAttempt starts one worker process and waits for it. It stops early on the timeout or when the
// worker goes silent, and calls tick every publishEvery with what the worker has done so far.
func runAttempt(ctx context.Context, bin string, o Opts, promptPath, token string, timeout time.Duration,
	tick func(streamSummary, time.Time)) attempt {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx, cancelCause := context.WithCancelCause(ctx)
	defer cancelCause(nil)
	var cmd *exec.Cmd
	parse := parseStream
	if o.Profile.BackendName() == "codex" {
		cmd = exec.CommandContext(ctx, bin, buildCodexArgs(o)...)
		cmd.Dir = o.Dir
		cmd.Env = os.Environ()
		prompt, err := os.ReadFile(promptPath)
		if err != nil {
			return attempt{runErr: err}
		}
		cmd.Stdin = strings.NewReader(string(prompt) + "\nTask:\n" + o.Task)
		parse = parseCodexStream
	} else {
		cmd = exec.CommandContext(ctx, bin, buildArgs(o, promptPath)...)
		cmd.Dir = o.Dir
		cmd.Env = childEnv(os.Environ(), o, token)
	}
	out := &activityWriter{}
	out.last.Store(time.Now().UnixNano())
	var workerErr bytes.Buffer
	cmd.Stdout, cmd.Stderr = out, &workerErr
	go watchIdle(ctx, cancelCause, &out.last, IdleLimit)
	if tick != nil {
		go func() {
			t := time.NewTicker(publishEvery)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					tick(parse(out.Bytes()), time.Unix(0, out.last.Load()))
				}
			}
		}()
	}
	// Do not wait indefinitely for a worker descendant to close inherited pipes.
	cmd.WaitDelay = 100 * time.Millisecond
	runErr := cmd.Run()
	return attempt{s: parse(out.Bytes()), cause: context.Cause(ctx), runErr: runErr,
		stdout: out.String(), stderr: workerErr.String()}
}

// salvage resumes a stopped session with no tools and returns the report it writes.
func salvage(ctx context.Context, bin string, o Opts, promptPath, token, session string, tick func(streamSummary, time.Time)) (string, *float64, bool) {
	so := o
	so.Resume, so.Salvage, so.Write, so.Task = session, true, false, salvageTask
	a := runAttempt(ctx, bin, so, promptPath, token, SalvageTimeout, tick)
	r := a.s.Result
	if r == nil || r.IsError || strings.TrimSpace(r.Result) == "" {
		return "", nil, false
	}
	cost := r.CostUSD
	if cost != nil && !history.ValidCost(*cost) {
		cost = nil
	}
	return r.Result, cost, true
}

// publishEvery is how often a running worker refreshes its live state.
const publishEvery = 2 * time.Second

// publisher keeps this process's live state current. It is advisory; failures are ignored.
type publisher struct {
	mu sync.Mutex
	st live.State
}

func newPublisher(o Opts, started time.Time) *publisher {
	p := &publisher{st: live.State{PID: os.Getpid(), Phase: live.Running, Dir: o.Dir,
		Task: text.Truncate(text.OneLine(o.Task), 200), Profile: o.ProfileName, Model: o.Model,
		StartedAt: started, LastEventAt: started}}
	live.Publish(p.st)
	return p
}

func (p *publisher) update(s streamSummary, last time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.st.Session, p.st.ToolCalls, p.st.Denials, p.st.LastEventAt = s.Session, s.ToolCalls, s.Denials, last
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
	p.st.Phase = phase
	live.Publish(p.st)
}

func (p *publisher) finish(rec history.Record) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.st.Phase, p.st.Status, p.st.Session, p.st.ToolCalls, p.st.Denials = live.Done, rec.Status, rec.Session, rec.Turns, rec.Denials
	if rec.ExitCode != nil {
		p.st.ExitCode = *rec.ExitCode
	}
	live.Publish(p.st)
}

// Preflight is swapped out in tests.
var Preflight = CheckReachable

// CheckReachable fails fast when the gateway cannot be dialed. Without it, a worker inside a
// network-less sandbox (e.g. Codex workspace-write) silently retries until the timeout.
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
