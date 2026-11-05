//go:build unix

package execution

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/proc"
	"github.com/zhoushoujianwork/offload/internal/slots"
)

// The fake backend leaves a long-running child behind its own shell.
const hangingCodex = `#!/bin/sh
sleep 300 &
echo $! > "$RECLAIM_CHILD_PID"
sleep 300
`

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func readFile(path string) string {
	b, _ := os.ReadFile(path)
	return strings.TrimSpace(string(b))
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// TestReclaimHelperCaller is the crashing caller. It only runs as a child process.
func TestReclaimHelperCaller(t *testing.T) {
	dir := os.Getenv("RECLAIM_HELPER_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	c, err := New(Config{ProfileName: "fixture", Profile: Profile{Backend: "codex", Model: "fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	timeout := time.Hour
	_, _ = c.Run(context.Background(), Request{Directory: dir, Task: "t", Timeout: &timeout,
		OnRunEvent: func(e RunEvent) error {
			if e.Type == "start" {
				return os.WriteFile(filepath.Join(dir, "runid"), []byte(e.RunID), 0o600)
			}
			return nil
		}})
}

type crashedRun struct {
	runID    string
	childPID int
	caller   *exec.Cmd
	done     chan struct{}
	dir      string
}

func startCaller(t *testing.T) *crashedRun {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-codex")
	if err := os.WriteFile(bin, []byte(hangingCodex), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("OFFLOAD_MAX_WORKERS", "1")
	t.Setenv("OFFLOAD_CODEX", bin)
	t.Setenv("RECLAIM_CHILD_PID", filepath.Join(dir, "child"))
	t.Setenv("RECLAIM_HELPER_DIR", dir)
	cmd := exec.Command(os.Args[0], "-test.run=^TestReclaimHelperCaller$")
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	r := &crashedRun{caller: cmd, done: done, dir: dir}
	waitFor(t, "fake backend child", func() bool {
		n, err := strconv.Atoi(readFile(filepath.Join(dir, "child")))
		r.childPID = n
		return err == nil && n > 1
	})
	waitFor(t, "run ID in start event", func() bool { r.runID = readFile(filepath.Join(dir, "runid")); return r.runID != "" })
	t.Cleanup(func() { _ = syscall.Kill(r.childPID, syscall.SIGKILL) })
	// The caller may only crash once the slot records the process group. Before that, the
	// slot is in its starting window, which reclaim reports as unconfirmed.
	waitFor(t, "process group recorded in slot", func() bool {
		files, _ := filepath.Glob(filepath.Join(slots.Dir(), "slot-*.lock"))
		for _, f := range files {
			var in slots.Info
			if json.Unmarshal([]byte(readFile(f)), &in) == nil && in.RunID == r.runID && in.Group != nil && !in.Starting {
				return true
			}
		}
		return false
	})
	return r
}

func (r *crashedRun) crash(t *testing.T) {
	t.Helper()
	_ = r.caller.Process.Signal(syscall.SIGKILL)
	select {
	case <-r.done:
	case <-time.After(20 * time.Second):
		t.Fatal("caller did not exit")
	}
}

func slotUsable(t *testing.T) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := slots.Acquire(ctx, slots.Options{Dir: slots.Dir(), Max: 1})
	if err != nil {
		return false
	}
	s.Release()
	return true
}

func TestReclaimRunEndsCrashedCallersProcessTree(t *testing.T) {
	r := startCaller(t)
	// A live caller is refused and its backend is left alone.
	if out, err := ReclaimRun(context.Background(), r.runID); !errors.Is(err, ErrRunStillOwned) || out == ReclaimTerminated || out == ReclaimAlreadyEnded {
		t.Fatalf("live caller: outcome %q err %v", out, err)
	}
	if !alive(r.childPID) {
		t.Fatal("refused reclaim killed the backend")
	}
	r.crash(t)
	if !alive(r.childPID) {
		t.Fatal("backend should outlive the crashed caller")
	}
	if slotUsable(t) {
		t.Fatal("slot should be unresolved while the orphaned tree runs")
	}
	out, err := ReclaimRun(context.Background(), r.runID)
	if err != nil || out != ReclaimTerminated {
		t.Fatalf("reclaim: outcome %q err %v", out, err)
	}
	waitFor(t, "orphaned child to exit", func() bool { return !alive(r.childPID) })
	// Repeating the call is idempotent while the slot still records the run.
	if out, err := ReclaimRun(context.Background(), r.runID); err != nil || out != ReclaimAlreadyEnded {
		t.Fatalf("repeat: outcome %q err %v", out, err)
	}
	if !slotUsable(t) {
		t.Fatal("slot is still unresolved after reclaim")
	}
}

func TestReclaimRunUnknownRunIsNotSuccess(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	out, err := ReclaimRun(context.Background(), "no-such-run")
	if !errors.Is(err, ErrRunNotFound) || out != ReclaimUnconfirmed {
		t.Fatalf("outcome %q err %v", out, err)
	}
}

// A recorded group whose leader start time no longer matches belongs to someone
// else now. It must not be signalled.
func TestReclaimRunDoesNotKillReusedProcessGroup(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	victim := exec.Command("sleep", "60")
	proc.Prepare(victim)
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = victim.Wait(); close(exited) }()
	t.Cleanup(func() { _ = victim.Process.Kill(); <-exited })
	g := proc.Started(victim)
	g.Start = "Thu Jan  1 00:00:00 1970"
	if err := os.MkdirAll(slots.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(slots.Info{OwnerPID: 1 << 22, RunID: "stale-run", Group: &g, AcquiredAt: time.Now()})
	if err := os.WriteFile(filepath.Join(slots.Dir(), "slot-0.lock"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := ReclaimRun(context.Background(), "stale-run")
	if err != nil || out != ReclaimAlreadyEnded {
		t.Fatalf("outcome %q err %v", out, err)
	}
	time.Sleep(200 * time.Millisecond)
	select {
	case <-exited:
		t.Fatal("unrelated process was killed")
	default:
	}
}

func writeSlotInfo(t *testing.T, in slots.Info) {
	t.Helper()
	if err := os.MkdirAll(slots.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(in)
	if err := os.WriteFile(filepath.Join(slots.Dir(), "slot-0.lock"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The owner died after marking the start but before recording the group. A process may be
// running, so reclaim must stay unconfirmed on every call and Acquire must not reuse the slot.
func TestReclaimRunStartingWithoutGroupIsUnconfirmed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	writeSlotInfo(t, slots.Info{OwnerPID: 1 << 22, RunID: "starting-run", Starting: true, AcquiredAt: time.Now()})
	for i := 0; i < 2; i++ {
		out, err := ReclaimRun(context.Background(), "starting-run")
		if !errors.Is(err, ErrRunStartUnrecorded) || out != ReclaimUnconfirmed {
			t.Fatalf("call %d: outcome %q err %v", i, out, err)
		}
	}
	if slotUsable(t) {
		t.Fatal("slot with an unrecorded starting process must not be reused")
	}
}

func TestReclaimRunAfterStartFailureOrNormalEndIsAlreadyEnded(t *testing.T) {
	for _, name := range []string{"start failure", "normal end"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			s, err := slots.Acquire(context.Background(), slots.Options{Dir: slots.Dir(), Max: 1, RunID: "run-x"})
			if err != nil {
				t.Fatal(err)
			}
			s.Starting()
			if name == "start failure" {
				s.StartFailed()
			} else {
				cmd := exec.Command("true")
				proc.Prepare(cmd)
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				s.Track(proc.Started(cmd))
				_ = cmd.Wait()
				s.Clear()
			}
			s.Release()
			out, err := ReclaimRun(context.Background(), "run-x")
			if err != nil || out != ReclaimAlreadyEnded {
				t.Fatalf("outcome %q err %v", out, err)
			}
			if !slotUsable(t) {
				t.Fatal("slot should be free")
			}
		})
	}
}
