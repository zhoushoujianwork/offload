//go:build unix

package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
)

const initLine = `printf '%s\n' '{"type":"system","subtype":"init","session_id":"s-term"}'` + "\n"

// terminationFake writes a fake claude. body runs after the init event has been printed; ready is
// created once the worker is running so tests can act on a worker that has a session.
func terminationFake(t *testing.T, ready, body string) {
	t.Helper()
	script := "#!/bin/sh\n" + initLine + `: > '` + ready + "'\n" + body
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CLAUDE", bin)
}

func waitExists(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s not created", path)
}

const resumeCmd = `offload -r s-term "stop exploring and report what you already found"`

func runWithStop(t *testing.T, body string, cause error) history.Record {
	t.Helper()
	stateEnv(t)
	mockPreflight(t)
	ready := filepath.Join(t.TempDir(), "ready")
	terminationFake(t, ready, body)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	if cause != nil {
		go func() {
			waitExists(t, ready)
			cancel(cause)
		}()
	}
	o := testOpts(t)
	o.NoSalvage = true
	rec, err := Run(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestTerminationClasses(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		cause        error
		class        string
		signal       string // signal offload received
		workerSignal string
		requested    bool
		systemKilled bool
		wantError    string
	}{
		{name: "supervisor requested stop", body: "exec sleep 30\n", cause: context.Canceled,
			class: history.ClassCanceled, workerSignal: "SIGTERM", requested: true, wantError: "canceled: stop requested"},
		{name: "SIGINT is a requested stop", body: "exec sleep 30\n", cause: &SignalError{Signal: syscall.SIGINT},
			class: history.ClassCanceled, signal: "SIGINT", workerSignal: "SIGTERM", requested: true, wantError: "canceled: stop requested (SIGINT)"},
		{name: "graceful shutdown signal", body: "exec sleep 30\n", cause: &SignalError{Signal: syscall.SIGTERM},
			class: history.ClassInterrupted, signal: "SIGTERM", workerSignal: "SIGTERM", requested: true, wantError: "interrupted: offload received SIGTERM"},
		{name: "shutdown signal sent to the worker only", body: "kill -TERM $$\nsleep 30\n",
			class: history.ClassInterrupted, workerSignal: "SIGTERM", wantError: "interrupted: the worker received SIGTERM from outside offload"},
		{name: "unrequested kill", body: "kill -KILL $$\nsleep 30\n",
			class: history.ClassKilled, workerSignal: "SIGKILL", systemKilled: true, wantError: "killed: the worker was ended by SIGKILL that offload did not send"},
		{name: "unrequested crash signal", body: "kill -SEGV $$\nsleep 30\n",
			class: history.ClassKilled, workerSignal: "SIGSEGV", wantError: "killed: the worker was ended by SIGSEGV"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := runWithStop(t, tc.body, tc.cause)
			tm := rec.Termination
			if tm == nil {
				t.Fatalf("no termination recorded: %+v", rec)
			}
			if tm.Class != tc.class || tm.Signal != tc.signal || tm.WorkerSignal != tc.workerSignal ||
				tm.SupervisorRequested != tc.requested || tm.SystemKilled != tc.systemKilled || tm.GraceExpired {
				t.Errorf("termination = %+v", *tm)
			}
			if tm.ResumeCommand != resumeCmd {
				t.Errorf("resume command = %q", tm.ResumeCommand)
			}
			if !strings.Contains(rec.Error, tc.wantError) || !strings.Contains(rec.Error, "resume with: "+resumeCmd) {
				t.Errorf("error = %q", rec.Error)
			}
			if rec.Status != "error" || rec.Session != "s-term" {
				t.Errorf("rec = %+v", rec)
			}
		})
	}
}

// Without a session id the run cannot be resumed, so no resume command is offered.
func TestTerminationWithoutSessionHasNoResumeCommand(t *testing.T) {
	stateEnv(t)
	mockPreflight(t)
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\"}'\nkill -KILL $$\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CLAUDE", bin)
	o := testOpts(t)
	o.NoSalvage = true
	rec, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Termination == nil || rec.Termination.Class != history.ClassKilled || rec.Termination.ResumeCommand != "" ||
		strings.Contains(rec.Error, "resume with") {
		t.Fatalf("rec = %+v", rec)
	}
}

// A normal failure, a timeout and a success carry no termination class.
func TestNoTerminationForOrdinaryEnds(t *testing.T) {
	rec := runWithStop(t, "exit 1\n", nil)
	if rec.Termination != nil {
		t.Fatalf("plain exit classified: %+v", *rec.Termination)
	}
	orig := StopGrace
	StopGrace = 2 * time.Second
	t.Cleanup(func() { StopGrace = orig })
	stateEnv(t)
	mockPreflight(t)
	terminationFake(t, filepath.Join(t.TempDir(), "ready"), "exec sleep 30\n")
	o := testOpts(t)
	o.NoSalvage = true
	o.Timeout = 300 * time.Millisecond
	rec, err := Run(context.Background(), o)
	if err != nil || rec.Status != "timeout" || rec.Termination != nil {
		t.Fatalf("err=%v rec=%+v", err, rec)
	}
}

// A worker that ignores the stop request is force-killed after the grace period, and the record
// says so, because it may not have flushed anything.
func TestTerminationRecordsExpiredGrace(t *testing.T) {
	orig := StopGrace
	StopGrace = 300 * time.Millisecond
	t.Cleanup(func() { StopGrace = orig })
	rec := runWithStop(t, "trap '' TERM\nwhile :; do sleep 0.05; done\n", &SignalError{Signal: syscall.SIGTERM})
	tm := rec.Termination
	if tm == nil || tm.Class != history.ClassInterrupted || !tm.GraceExpired || tm.WorkerSignal != "SIGKILL" || tm.SystemKilled {
		t.Fatalf("termination = %+v", tm)
	}
	if !strings.Contains(rec.Error, "grace period and was force-killed") {
		t.Errorf("error = %q", rec.Error)
	}
}

// On a graceful shutdown signal the worker keeps running for the grace period: everything it
// writes while shutting down (progress lines and a partial report) reaches the event callback
// and the record, and the run returns long before the grace period ends.
func TestGracefulShutdownFlushesEventsAndPartialReport(t *testing.T) {
	stateEnv(t)
	mockPreflight(t)
	origGrace := StopGrace
	StopGrace = 5 * time.Second
	t.Cleanup(func() { StopGrace = origGrace })
	ready := filepath.Join(t.TempDir(), "ready")
	terminationFake(t, ready, `on_term() {
  printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"flushing before exit"}]}}'
  sleep 0.2
  printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"PARTIAL REPORT: read a.go and b.go","session_id":"s-term"}'
  exit 143
}
trap on_term TERM
while :; do sleep 0.05; done
`)
	var mu sync.Mutex
	var lines []string
	o := testOpts(t)
	o.NoSalvage = true
	o.OnLine = func(b []byte) error {
		mu.Lock()
		lines = append(lines, string(b))
		mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	var stopped time.Time
	go func() {
		waitExists(t, ready)
		stopped = time.Now()
		cancel(&SignalError{Signal: syscall.SIGTERM})
	}()
	rec, err := Run(ctx, o)
	took := time.Since(stopped)
	if err != nil {
		t.Fatal(err)
	}
	if took >= StopGrace {
		t.Fatalf("shutdown took %s, not within the %s grace period", took, StopGrace)
	}
	mu.Lock()
	got := strings.Join(lines, "\n")
	mu.Unlock()
	for _, want := range []string{"flushing before exit", "PARTIAL REPORT"} {
		if !strings.Contains(got, want) {
			t.Errorf("event log missing %q written during the grace period:\n%s", want, got)
		}
	}
	if !strings.Contains(rec.Result, "PARTIAL REPORT: read a.go and b.go") {
		t.Errorf("partial report not kept: %q", rec.Result)
	}
	tm := rec.Termination
	if tm == nil || tm.Class != history.ClassInterrupted || tm.GraceExpired || tm.ResumeCommand != resumeCmd {
		t.Fatalf("termination = %+v", tm)
	}
	if !strings.Contains(rec.Error, "interrupted: offload received SIGTERM") || !strings.Contains(rec.Error, "resume with: "+resumeCmd) {
		t.Errorf("error = %q", rec.Error)
	}
	if rec.Status != "error" {
		t.Errorf("status = %q", rec.Status)
	}
}
