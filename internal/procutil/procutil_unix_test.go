//go:build unix

package procutil

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const bound = 5 * time.Second

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.HasSuffix(string(b), "\n") {
			n, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid file %s not written", path)
	return 0
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still alive", pid)
}

// sibling starts an unrelated process in the test's own group and kills it on cleanup.
func sibling(t *testing.T) *exec.Cmd {
	t.Helper()
	c := exec.Command("sleep", "60")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Process.Kill(); _ = c.Wait() })
	return c
}

func TestCancelKillsChildAndDescendantsOnly(t *testing.T) {
	dir := t.TempDir()
	other := sibling(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := CommandContext(ctx, "sh", "-c", `sleep 60 & echo $! > "$1"; echo $$ > "$2"; wait`, "sh",
		filepath.Join(dir, "child"), filepath.Join(dir, "self"))
	cmd.WaitDelay = 100 * time.Millisecond
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- cmd.Wait() }()
	desc, self := readPID(t, filepath.Join(dir, "child")), readPID(t, filepath.Join(dir, "self"))
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled command reported success")
		}
	case <-time.After(bound):
		t.Fatal("Wait did not return after cancel")
	}
	waitDead(t, desc)
	waitDead(t, self)
	if d := time.Since(start); d > bound {
		t.Fatalf("took %s", d)
	}
	if !alive(other.Process.Pid) {
		t.Fatal("unrelated process was killed")
	}
}

func TestNormalExitKeepsBackgroundProcess(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "bg")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := CommandContext(ctx, "sh", "-c", `sleep 60 >/dev/null 2>&1 </dev/null & echo $! > "$1"`, "sh", pidFile)
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	pid := readPID(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel() // after Wait the context no longer owns the command
	time.Sleep(100 * time.Millisecond)
	if !alive(pid) {
		t.Fatal("intentional background process was killed by normal completion")
	}
}

func TestPreCancelledContextDoesNotLaunch(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := CommandContext(ctx, "sh", "-c", `echo x > "$1"`, "sh", marker).Run()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("command ran")
	}
}

func TestChildLeadsOwnGroup(t *testing.T) {
	cmd := CommandContext(context.Background(), "sh", "-c", "exit 0")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Fatal("Setpgid not set")
	}
	if cmd.Cancel == nil {
		t.Fatal("Cancel not set")
	}
}

func TestKillGroupPIDGuards(t *testing.T) {
	for _, pid := range []int{-5, 0, 1} {
		if err := killGroupPID(pid); err == nil || errors.Is(err, os.ErrProcessDone) {
			t.Errorf("pid %d: err = %v, want refusal", pid, err)
		}
	}
	if err := killGroup(nil); err == nil {
		t.Error("nil process accepted")
	}
	// A finished child's group no longer exists: ESRCH becomes ErrProcessDone.
	cmd := CommandContext(context.Background(), "true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if err := killGroupPID(cmd.Process.Pid); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("err = %v, want os.ErrProcessDone", err)
	}
}
