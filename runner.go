package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	Budget      float64
	Timeout     time.Duration
}

// claudeResult is the subset of `claude -p --output-format json` we use.
type claudeResult struct {
	Result     string  `json:"result"`
	IsError    bool    `json:"is_error"`
	NumTurns   int     `json:"num_turns"`
	CostUSD    float64 `json:"total_cost_usd"`
	DurationMs int64   `json:"duration_ms"`
	SessionID  string  `json:"session_id"`
}

func buildArgs(o RunOpts) []string {
	settings, _ := json.Marshal(map[string]any{
		// The token reaches claude via env, never argv (visible in ps).
		"apiKeyHelper": `printf %s "$OFFLOAD_TOKEN"`,
		// --settings outranks ~/.claude/settings.json env, which would otherwise redirect the gateway.
		"env": map[string]string{"ANTHROPIC_BASE_URL": o.Profile.BaseURL, "ANTHROPIC_MODEL": o.Model},
	})
	args := []string{"-p", "--bare", "--output-format", "json", "--model", o.Model,
		"--max-budget-usd", fmt.Sprint(o.Budget), "--settings", string(settings)}
	if o.Resume != "" {
		args = append(args, "--resume", o.Resume)
	}
	// --bare skips CLAUDE.md discovery; hand the worker the project's own instructions explicitly.
	for _, f := range []string{"CLAUDE.md", "AGENTS.md"} {
		if p := filepath.Join(o.Dir, f); fileExists(p) {
			args = append(args, "--append-system-prompt-file", p)
		}
	}
	sys := workerPrompt
	if o.Write {
		args = append(args, "--permission-mode", "acceptEdits", "--allowedTools", writeTools)
	} else {
		sys += "\nRead-only task: do not modify files."
		args = append(args, "--allowedTools", readOnlyTools)
	}
	return append(args, "--append-system-prompt", sys, o.Task)
}

// childEnv drops the caller's gateway credentials so the worker only sees the profile's.
func childEnv(base []string, o RunOpts, token string) []string {
	env := make([]string, 0, len(base)+2)
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "OFFLOAD_TOKEN":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "ANTHROPIC_BASE_URL="+o.Profile.BaseURL, "OFFLOAD_TOKEN="+token)
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
func run(ctx context.Context, o RunOpts) (Record, error) {
	rec := Record{TS: time.Now().Format("2006-01-02 15:04:05"), Profile: o.ProfileName, Model: o.Model,
		Dir: o.Dir, Task: o.Task, Status: "error"}
	token, err := o.Profile.token()
	if err != nil {
		return rec, err
	}
	bin, err := claudeBin()
	if err != nil {
		return rec, fmt.Errorf("claude CLI not found: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, buildArgs(o)...)
	cmd.Dir = o.Dir
	cmd.Env = childEnv(os.Environ(), o, token)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, os.Stderr
	runErr := cmd.Run()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		rec.Status, rec.Result = "timeout", fmt.Sprintf("timed out after %s", o.Timeout)
		return rec, nil
	}
	var res claudeResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		rec.Result = strings.TrimSpace(out.String())
		if rec.Result == "" && runErr != nil {
			rec.Result = runErr.Error()
		}
		return rec, nil
	}
	rec.Session, rec.Turns, rec.CostUSD, rec.DurationMs, rec.Result =
		res.SessionID, res.NumTurns, res.CostUSD, res.DurationMs, res.Result
	if !res.IsError && runErr == nil {
		rec.Status = "ok"
	}
	return rec, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
