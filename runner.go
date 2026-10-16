package main

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
	"strings"
	"time"
)

const workerPrompt = `You are a worker agent delegated a task by a supervising agent. Work autonomously; never ask questions.
Finish with a concise report: what you did or found, exact file:line references, commands run with their results, and anything unverified. No preamble.`

var (
	readOnlyTools = "Read Grep Glob Bash(git log:*) Bash(git diff:*) Bash(git show:*) Bash(ls:*) Bash(rg:*)"
	// Write mode: edits stay inside the working dir (acceptEdits); shell is limited to build/test/inspect.
	// Deliberately no commit, push, rm, or network tools.
	writeTools = "Read Grep Glob Edit Write Bash(go build:*) Bash(go test:*) Bash(go vet:*) Bash(gofmt:*) " +
		"Bash(make:*) Bash(npm test:*) Bash(npm run:*) Bash(pytest:*) Bash(cargo build:*) Bash(cargo test:*) " +
		"Bash(idf.py build:*) Bash(git status:*) Bash(git diff:*) Bash(ls:*) Bash(rg:*)"
)

type RunOpts struct {
	ProfileName string
	Profile     Profile
	Model       string
	Write       bool
	Resume      string
	Dir         string
	Task        string
	Timeout     time.Duration
	StartedAt   time.Time
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

func buildArgs(o RunOpts, promptPath string) []string {
	// Gateway, token and model reach the worker only through env (see childEnv), never argv.
	// --bare still loads settings; disable user/project/local sources so their env cannot
	// override the selected profile's gateway, credentials or model.
	args := []string{"-p", "--bare", "--setting-sources", "", "--output-format", "json"}
	if o.Resume != "" {
		args = append(args, "--resume", o.Resume)
	}

	if o.Write {
		args = append(args, "--permission-mode", "acceptEdits", "--allowedTools", writeTools)
	} else {
		args = append(args, "--allowedTools", readOnlyTools)
	}
	return append(args, "--append-system-prompt-file", promptPath, o.Task)
}

// writeWorkerPrompt combines explicit project instructions and delegated-task constraints.
// Claude accepts only one append prompt mechanism; a private file keeps its contents out of argv.
func writeWorkerPrompt(o RunOpts) (string, error) {
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
	prompt.WriteString(workerPrompt)
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
func childEnv(base []string, o RunOpts, token string) []string {
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

// run executes the worker and always returns a Record describing what happened.
func run(ctx context.Context, o RunOpts) (rec Record, err error) {
	started := o.StartedAt
	if started.IsZero() {
		started = time.Now()
	}
	rec = Record{TS: started.Format("2006-01-02 15:04:05"), Profile: o.ProfileName, Model: o.Model,
		Dir: o.Dir, Task: o.Task, Status: "error", CostIsEstimate: true}
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
	token, err := o.Profile.token()
	if err != nil {
		return rec, err
	}
	bin, err := claudeBin()
	if err != nil {
		return rec, fmt.Errorf("claude CLI not found: %w", err)
	}
	if err := preflight(o.Profile.BaseURL); err != nil {
		return rec, err
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, buildArgs(o, promptPath)...)
	cmd.Dir = o.Dir
	cmd.Env = childEnv(os.Environ(), o, token)
	var out, workerErr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &workerErr
	// Do not wait indefinitely for a worker descendant to close inherited pipes.
	cmd.WaitDelay = 100 * time.Millisecond
	runErr := cmd.Run()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		rec.Status, rec.Result = "timeout", fmt.Sprintf("timed out after %s", o.Timeout)
		rec.Error = rec.Result
		return rec, nil
	}
	var res claudeResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		rec.Result = strings.TrimSpace(out.String())
		if rec.Result == "" {
			rec.Result = strings.TrimSpace(workerErr.String())
		}
		if rec.Result == "" && runErr != nil {
			rec.Result = runErr.Error()
		}
		rec.Error = "worker returned invalid JSON"
		if runErr != nil {
			rec.Error += ": " + runErr.Error()
		}
		return rec, nil
	}
	rec.Session, rec.Turns, rec.CostUSD, rec.DurationMs, rec.Result =
		res.SessionID, res.NumTurns, res.CostUSD, res.DurationMs, res.Result
	if rec.CostUSD != nil && !validCost(*rec.CostUSD) {
		rec.CostUSD = nil
	}
	if !res.IsError && runErr == nil {
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
			if runErr != nil {
				rec.Error = runErr.Error()
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

// preflight is swapped out in tests.
var preflight = checkReachable

// checkReachable fails fast when the gateway cannot be dialed. Without it, a worker inside a
// network-less sandbox (e.g. Codex workspace-write) silently retries until the timeout.
func checkReachable(baseURL string) error {
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
