package worker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
)

func TestIdleLimits(t *testing.T) {
	zero, override, negative := time.Duration(0), 5*time.Minute, -time.Second
	for _, tc := range []struct {
		name, policy, profile string
		override              *time.Duration
		timeout, want         time.Duration
		bad                   bool
	}{
		{"default", "", "", nil, time.Hour, 10 * time.Minute, false},
		{"managed", "managed-development", "", nil, time.Hour, 30 * time.Minute, false},
		{"profile", "managed-development", "12m", nil, time.Hour, 12 * time.Minute, false},
		{"cli", "", "12m", &override, time.Hour, override, false},
		{"disabled", "", "12m", &zero, time.Hour, 0, false},
		{"profile-disabled", "", "0", nil, time.Hour, 0, false},
		{"cap", "", "12m", nil, time.Minute, time.Minute, false},
		{"immediate", "", "12m", nil, 0, 0, false},
		{"negative", "", "", &negative, time.Hour, 0, true},
		{"invalid", "", "bad", nil, time.Hour, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (Opts{ExecutionPolicy: tc.policy, Profile: config.Profile{IdleTimeout: tc.profile}, IdleTimeout: tc.override, Timeout: tc.timeout}).idleLimit()
			if (err != nil) != tc.bad || !tc.bad && got != tc.want {
				t.Fatalf("%s %v", got, err)
			}
		})
	}
}

func TestIdleAttemptActivity(t *testing.T) {
	for _, source := range []string{"silent", "stderr", "stdout", "session", "unrelated", "deadline", "disabled"} {
		t.Run(source, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CODEX_HOME", dir)
			logDir := filepath.Join(dir, "sessions", "2026", "10", "27")
			if err := os.MkdirAll(logDir, 0700); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(logDir, "rollout-session.jsonl")
			script := "#!/bin/sh\necho '{\"type\":\"thread.started\",\"thread_id\":\"session\"}'\n"
			action := ":"
			switch source {
			case "stderr":
				action = "echo tick >&2"
			case "stdout":
				action = "echo tick"
			case "session":
				action = "echo tick >> '" + log + "'"
			case "unrelated":
				action = "echo tick >> '" + log + ".other'"
			}
			script += "i=0\nwhile [ $i -lt 30 ]; do\n" + action + "\nsleep 0.1\ni=$((i+1))\ndone\n"
			bin := filepath.Join(dir, "child")
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			prompt := filepath.Join(dir, "prompt")
			if err := os.WriteFile(prompt, nil, 0600); err != nil {
				t.Fatal(err)
			}
			idle, timeout := 2*time.Second, 8*time.Second
			want := ""
			if source == "silent" || source == "unrelated" {
				idle = 500 * time.Millisecond
				want = "idle"
			}
			if source == "deadline" {
				timeout = 250 * time.Millisecond
				want = "deadline"
			}
			if source == "disabled" {
				idle = 0
			}
			start := time.Now()
			a := runAttempt(context.Background(), bin, Opts{Dir: dir, Profile: config.Profile{Backend: "codex"}, IdleTimeout: &idle, Timeout: timeout}, prompt, "", timeout, nil, hooks{})
			if got := stopReason(a.cause); got != want || a.leftover != nil {
				t.Fatalf("reason=%q want=%q err=%v leftover=%v", got, want, a.runErr, a.leftover)
			}
			if want == "" && (a.runErr != nil || time.Since(start) <= idle) {
				t.Fatalf("activity did not outlive idle limit: elapsed=%s err=%v", time.Since(start), a.runErr)
			}
			if want == "idle" && (time.Since(start) < idle || time.Since(start) > 3*time.Second) {
				t.Fatal(time.Since(start))
			}
		})
	}
}

func TestStopReasons(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  string
	}{
		{context.Canceled, "canceled"}, {ErrSignal, "signal"}, {context.DeadlineExceeded, "deadline"}, {errIdle, "idle"}, {errors.New("other"), ""},
	} {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(tc.cause)
		if got := stopReason(context.Cause(ctx)); got != tc.want {
			t.Fatal(got)
		}
	}
}

func TestSessionBaseline(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	logDir := filepath.Join(dir, "projects", "project")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(logDir, "session.jsonl")
	if err := os.WriteFile(log, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	a := newSessionActivity(Opts{Resume: "session"})
	if a.grew("session") {
		t.Fatal("existing data counted as activity")
	}
	if err := os.WriteFile(log, []byte("old plus new"), 0600); err != nil {
		t.Fatal(err)
	}
	if !a.grew("session") || a.grew("session") || a.grew("../session") {
		t.Fatal("incorrect growth tracking")
	}
}
