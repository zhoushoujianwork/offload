package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/worker"
)

func TestSupervisorFinalJSONAndEvents(t *testing.T) {
	work, argv := setupCodex(t, codexReply)
	file := filepath.Join(t.TempDir(), "events.ndjson")
	instructions := filepath.Join(t.TempDir(), "operator.md")
	os.WriteFile(instructions, []byte("TASK_SECRET_OUTCOME_MARKER\n"), 0600)
	code, out, stderr := runCLI("-json", "-p", "codex", "-d", work, "-w", "-execution-policy", "managed-development", "-instructions-file", instructions, "-caller-run-id", "job:123", "-events-file", file, "inspect")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	rec := decodeRun(t, out)
	if rec.RunID == "" || rec.CallerRunID != "job:123" || rec.ExecutionPolicy != "managed-development" || rec.ErrorKind != "" {
		t.Fatal(rec)
	}
	args, _ := os.ReadFile(argv)
	prompt, _ := os.ReadFile(argv + ".stdin")
	if strings.Contains(string(args), "TASK_SECRET") || !strings.Contains(string(prompt), "TASK_SECRET_OUTCOME_MARKER") {
		t.Fatalf("%s %s", args, prompt)
	}
	b, _ := os.ReadFile(file)
	if !bytes.HasSuffix(b, []byte("\n")) {
		t.Fatal("not NDJSON")
	}
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	for i, line := range lines {
		var e supervisorEvent
		if err := json.Unmarshal(line, &e); err != nil {
			t.Fatal(err)
		}
		if e.SchemaVersion != 1 || e.RunID != rec.RunID || e.CallerRunID != "job:123" || e.Seq != i+1 || e.Backend != "codex" || e.Model != "test-model" {
			t.Fatalf("%s", line)
		}
		if i == 0 && e.Type != "start" || i == len(lines)-1 && (e.Type != "final" || e.Status != "ok" || e.ExitCode == nil || *e.ExitCode != 0) {
			t.Fatal(string(line))
		}
	}
	for _, secret := range []string{"rg retry", "CODEX REPORT", "TASK_SECRET", "inspect", "command_execution"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Fatalf("events leaked %s", secret)
		}
	}
	st, _ := os.Stat(file)
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
}

func TestSupervisorErrorsBeforeLaunch(t *testing.T) {
	for _, flags := range [][]string{
		{"-execution-policy", "managed-development"}, {"-execution-policy", "wrong"}, {"-caller-run-id", "bad id"},
		{"-instructions-file", "relative"}, {"-codex-sandbox", "workspace-write"}, {"-tools-disabled"}, {"-allowed-tools", "Read"}, {"-disallowed-tools", "Bash"}, {"-unknown-flag"},
	} {
		t.Run(strings.Join(flags, "_"), func(t *testing.T) {
			work, argv := setupCodex(t, codexReply)
			args := append([]string{"-json", "-p", "codex", "-d", work}, flags...)
			args = append(args, "inspect")
			code, out, _ := runCLI(args...)
			rec := decodeRun(t, out)
			if code != 2 || rec.ExitCode == nil || *rec.ExitCode != 2 || rec.ErrorKind == "" || rec.RunID == "" {
				t.Fatalf("%d %+v", code, rec)
			}
			if _, err := os.Stat(argv); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("provider launched")
			}
			if strings.Contains(strings.Join(flags, " "), "tools") && rec.ErrorKind != "unsupported_tools" {
				t.Fatal(rec.ErrorKind)
			}
		})
	}
	for _, symlink := range []bool{false, true} {
		t.Run("existing events", func(t *testing.T) {
			work, argv := setupCodex(t, codexReply)
			target := filepath.Join(t.TempDir(), "target")
			os.WriteFile(target, []byte("KEEP"), 0600)
			path := target
			if symlink {
				path = filepath.Join(t.TempDir(), "link")
				os.Symlink(target, path)
			}
			code, out, _ := runCLI("-json", "-p", "codex", "-d", work, "-events-file", path, "inspect")
			if code != 2 || decodeRun(t, out).ErrorKind != "configuration" {
				t.Fatal(code, out)
			}
			b, _ := os.ReadFile(target)
			if string(b) != "KEEP" {
				t.Fatal(string(b))
			}
			if _, err := os.Stat(argv); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("launched")
			}
		})
	}
}

type failedEventWriter struct{}

func (failedEventWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }
func TestEventBoundsAndFailure(t *testing.T) {
	var b bytes.Buffer
	e := &eventWriter{w: &b, base: supervisorEvent{SchemaVersion: 1}}
	for i := 0; i < 10010; i++ {
		if err := e.progress([]byte(`{"type":"SECRET_TYPE","message":"SECRET","item":{"type":"SECRET_TOOL"}}`)); err != nil {
			t.Fatal(err)
		}
	}
	if e.seq != 10001 || bytes.Contains(b.Bytes(), []byte("SECRET")) {
		t.Fatal(e.seq)
	}
	rec := history.Record{Status: "error", ErrorKind: "execution"}
	if err := e.emit("final", "", "", &rec); err != nil || e.seq != 10002 {
		t.Fatal(err, e.seq)
	}
	e = &eventWriter{w: failedEventWriter{}}
	if err := e.emit("start", "", "", nil); err == nil {
		t.Fatal("write failure ignored")
	}
}

func TestEventFailureCancelsWorker(t *testing.T) {
	work, _ := setupCodex(t, codexReply)
	// Emit a complete native event before sleeping, so the callback must terminate the process.
	fake := filepath.Join(t.TempDir(), "codex")
	os.WriteFile(fake, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"session\"}'\nsleep 10\n"), 0700)
	t.Setenv("OFFLOAD_CODEX", fake)
	start := time.Now()
	rec, err := worker.Run(context.Background(), worker.Opts{Dir: work, Profile: config.Profile{Backend: "codex"}, Timeout: time.Minute, OnLine: func([]byte) error { return errors.New("disk full") }})
	if err != nil || rec.ErrorKind != "events_io" || rec.Status != "error" || rec.Salvaged || time.Since(start) > 5*time.Second {
		t.Fatalf("%+v %v", rec, err)
	}
}

func TestManagedClaudeDisablesSalvageAndKeepsMarkers(t *testing.T) {
	for _, resume := range []string{"", "s-old"} {
		t.Run(resume, func(t *testing.T) {
			work, argv := setup(t, `{"type":"system","session_id":"s-killed"}`)
			t.Setenv("FAKE_SALVAGE", "SHOULD_NOT_APPEAR")
			file := filepath.Join(t.TempDir(), "instructions")
			os.WriteFile(file, []byte("EXACT_OPERATOR_MARKER: done\n"), 0600)
			args := []string{"-json", "-w", "-execution-policy", "managed-development", "-instructions-file", file, "-d", work}
			if resume != "" {
				args = append(args, "-r", resume)
			}
			args = append(args, "implement")
			code, out, _ := runCLI(args...)
			rec := decodeRun(t, out)
			if code != 1 || rec.Salvaged || rec.ErrorKind != "execution" {
				t.Fatal(code, rec)
			}
			if _, err := os.Stat(argv + ".salvage-args"); !os.IsNotExist(err) {
				t.Fatal("salvage launched")
			}
			prompt, _ := os.ReadFile(argv + ".prompt")
			if !bytes.Contains(prompt, []byte("EXACT_OPERATOR_MARKER: done\n")) || bytes.Contains(prompt, []byte("Shell is limited")) {
				t.Fatal(string(prompt))
			}
		})
	}
}

func TestStructuredErrorKinds(t *testing.T) {
	for _, tc := range []struct {
		name, reply, sleep, kind string
		canceled                 bool
	}{
		{"provider prose", `{"type":"turn.failed","error":{"message":"authentication failed"}}`, "", "execution", false},
		{"timeout", codexReply, "5", "timeout", false},
		{"canceled", codexReply, "5", "canceled", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work, _ := setupCodex(t, tc.reply)
			t.Setenv("FAKE_SLEEP", tc.sleep)
			ctx := context.Background()
			if tc.canceled {
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			}
			timeout := 5 * time.Second
			if tc.kind == "timeout" {
				timeout = 100 * time.Millisecond
			}
			rec, err := worker.Run(ctx, worker.Opts{Dir: work, Profile: config.Profile{Backend: "codex"}, Timeout: timeout})
			if err != nil || rec.ErrorKind != tc.kind || rec.RunID == "" {
				t.Fatal(rec, err)
			}
		})
	}
}

func TestClaudeProgressSanitizesToolMetadata(t *testing.T) {
	var b bytes.Buffer
	e := &eventWriter{w: &b}
	if err := e.progress([]byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"SECRET"}}]}}`)); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b.Bytes(), []byte("SECRET")) || !bytes.Contains(b.Bytes(), []byte(`"tool_category":"shell"`)) {
		t.Fatal(b.String())
	}
}

func TestClaudeTaskTransport(t *testing.T) {
	for _, policy := range []string{"default", "managed-development"} {
		for _, resume := range []string{"", "s-old"} {
			t.Run(policy+resume, func(t *testing.T) {
				work, record := setup(t, okReply)
				task := "TASK_SECRET: exact task\nsecond line"
				args := []string{"-json", "-w", "-execution-policy", policy, "-d", work}
				if resume != "" {
					args = append(args, "-r", resume)
				}
				args = append(args, task)
				code, out, stderr := runCLI(args...)
				if code != 0 {
					t.Fatalf("%d %s %s", code, out, stderr)
				}
				argv, err := os.ReadFile(record)
				if err != nil {
					t.Fatal(err)
				}
				stdin, err := os.ReadFile(record + ".stdin")
				if err != nil {
					t.Fatal(err)
				}
				if policy == "managed-development" {
					if bytes.Contains(argv, []byte("TASK_SECRET")) || string(stdin) != task {
						t.Fatalf("argv=%q stdin=%q", argv, stdin)
					}
				} else if !bytes.Contains(argv, []byte(task)) || len(stdin) != 0 {
					t.Fatalf("argv=%q stdin=%q", argv, stdin)
				}
			})
		}
	}
}

func TestConflictingToolsRejectedBeforeLaunch(t *testing.T) {
	for _, backend := range []string{"claude", "codex"} {
		t.Run(backend, func(t *testing.T) {
			var work, record string
			args := []string{"-json", "-w", "-execution-policy", "managed-development", "-tools-disabled", "-allowed-tools", "Read"}
			if backend == "codex" {
				work, record = setupCodex(t, codexReply)
				args = append(args, "-p", "codex")
			} else {
				work, record = setup(t, okReply)
			}
			args = append(args, "-d", work, "inspect")
			code, out, _ := runCLI(args...)
			rec := decodeRun(t, out)
			if code != 2 || rec.ErrorKind != "configuration" || !strings.Contains(rec.Error, "tools-disabled conflicts with allowed-tools") {
				t.Fatal(code, rec)
			}
			if _, err := os.Stat(record); !os.IsNotExist(err) {
				t.Fatal("provider launched")
			}
		})
	}
}

// The signal is delivered only to the isolated helper process, never the test host.
func TestSupervisorSIGTERM(t *testing.T) {
	for _, policy := range []string{"default", "managed-development"} {
		t.Run(policy, func(t *testing.T) {
			work, record := setup(t, okReply)
			ready := filepath.Join(t.TempDir(), "ready")
			t.Setenv("OFFLOAD_TEST_SIGTERM_HELPER", "1")
			t.Setenv("OFFLOAD_TEST_SIGTERM_WORK", work)
			t.Setenv("OFFLOAD_TEST_SIGTERM_POLICY", policy)
			t.Setenv("OFFLOAD_TEST_READY", ready)
			t.Setenv("FAKE_SALVAGE", "MUST_NOT_SALVAGE")
			script, err := os.ReadFile(os.Getenv("OFFLOAD_CLAUDE"))
			if err != nil {
				t.Fatal(err)
			}
			script = bytes.Replace(script, []byte("[ -n \"$FAKE_SLEEP\" ]"), []byte("printf '%s\\n' '{\"type\":\"system\",\"session_id\":\"s-killed\"}'\nprintf ready > \"$OFFLOAD_TEST_READY\"\n[ -n \"$FAKE_SLEEP\" ]"), 1)
			if err := os.WriteFile(os.Getenv("OFFLOAD_CLAUDE"), script, 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("FAKE_SLEEP", "10")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSupervisorSIGTERMHelper$")
			var out, stderr bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			// Always reap the isolated helper if a readiness check fails.
			defer func() {
				if cmd.ProcessState == nil {
					cmd.Process.Kill()
					cmd.Wait()
				}
			}()
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("worker did not become ready")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				t.Fatalf("err=%v stdout=%s stderr=%s", err, out.String(), stderr.String())
			}
			rec := decodeRun(t, out.String())
			if rec.Status != "error" || rec.ErrorKind != "canceled" || rec.Salvaged || rec.Session != "s-killed" || rec.ExitCode == nil || *rec.ExitCode != 1 {
				t.Fatal(rec)
			}
			if _, err := os.Stat(record + ".salvage-args"); !os.IsNotExist(err) {
				t.Fatal("salvage launched")
			}
		})
	}
}

func TestSupervisorSIGTERMHelper(t *testing.T) {
	if os.Getenv("OFFLOAD_TEST_SIGTERM_HELPER") != "1" {
		return
	}
	worker.Preflight = func(string) error { return nil }
	os.Exit(Run("test", []string{"-json", "-w", "-execution-policy", os.Getenv("OFFLOAD_TEST_SIGTERM_POLICY"), "-d", os.Getenv("OFFLOAD_TEST_SIGTERM_WORK"), "inspect"}, strings.NewReader(""), os.Stdout, os.Stderr))
}

func TestClaudeEventFailureCancelsWorker(t *testing.T) {
	work, _ := setup(t, okReply)
	t.Setenv("FAKE_PREFIX", `{"type":"system","session_id":"s-killed"}`)
	t.Setenv("FAKE_SLEEP", "10")
	t.Setenv("FAKE_SALVAGE", "MUST_NOT_SALVAGE")
	cfg, err := config.Load(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	rec, err := worker.Run(context.Background(), worker.Opts{Dir: work, Write: true, ExecutionPolicy: "managed-development", Profile: cfg.Profiles["cheap"], Timeout: time.Minute, OnLine: func([]byte) error { return errors.New("disk full") }})
	if err != nil || rec.ErrorKind != "events_io" || rec.Status != "error" || rec.Salvaged || time.Since(started) > 5*time.Second {
		t.Fatalf("%+v %v", rec, err)
	}
}
