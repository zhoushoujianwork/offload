//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package proc

import (
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

func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// readPID waits for a complete (newline-terminated) pid file written by the child shell.
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
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("pid file %s not written", path)
	return 0
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// startGroup starts `sh -c script` as the leader of its own process group. The script receives
// the pid file path as $1. The group is force-killed on cleanup so a failing test leaks nothing.
func startGroup(t *testing.T, script string) (cmd *exec.Cmd, g Group, descendant int, done *exited) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "child")
	cmd = exec.Command("sh", "-c", script, "sh", pidFile)
	Prepare(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g = Started(cmd)
	ex := &exited{ch: make(chan struct{})}
	go func() { ex.err = cmd.Wait(); close(ex.ch) }()
	t.Cleanup(func() {
		g.Kill()
		select {
		case <-ex.ch:
		case <-time.After(bound):
		}
	})
	descendant = readPID(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(descendant, syscall.SIGKILL) })
	return cmd, g, descendant, ex
}

// exited reports the result of cmd.Wait; ch is closed once err is set.
type exited struct {
	ch  chan struct{}
	err error
}

func waitExit(t *testing.T, done *exited) error {
	t.Helper()
	select {
	case <-done.ch:
		return done.err
	case <-time.After(bound):
		t.Fatal("leader did not exit")
		return nil
	}
}

func TestPrepareStartsChildInOwnProcessGroup(t *testing.T) {
	cmd, g, _, _ := startGroup(t, `sleep 60 & echo $! > "$1"; wait`)

	if g.PID != cmd.Process.Pid || g.PGID != g.PID {
		t.Fatalf("group = %+v, want PID == PGID == %d", g, cmd.Process.Pid)
	}
	pgid, err := syscall.Getpgid(g.PID)
	if err != nil {
		t.Fatal(err)
	}
	if pgid != g.PID {
		t.Fatalf("child pgid = %d, want its own pid %d", pgid, g.PID)
	}
	if own := syscall.Getpgrp(); pgid == own {
		t.Fatalf("child shares the test's process group %d", own)
	}
	if want := StartToken(g.PID); g.Start != want {
		t.Fatalf("Start = %q, want %q", g.Start, want)
	}
	if !g.Exists() || !g.Alive() {
		t.Fatalf("running group reported Exists=%v Alive=%v", g.Exists(), g.Alive())
	}
}

func TestTermStopsWholeGroup(t *testing.T) {
	_, g, descendant, done := startGroup(t, `sleep 60 & echo $! > "$1"; wait`)
	if descendantPGID, err := syscall.Getpgid(descendant); err != nil || descendantPGID != g.PGID {
		t.Fatalf("descendant pgid = %d (err %v), want group %d", descendantPGID, err, g.PGID)
	}

	g.Term()

	if err := waitExit(t, done); err == nil {
		t.Fatal("terminated leader reported success")
	}
	waitFor(t, "descendant to exit", func() bool { return !pidAlive(descendant) })
	waitFor(t, "group to disappear", func() bool { return !g.Exists() })
	if g.Alive() {
		t.Fatal("Alive reported true after the group was stopped")
	}
}

func TestKillStopsGroupThatIgnoresTerm(t *testing.T) {
	// The shell ignores SIGTERM before it starts the background child, and ignored signals
	// are inherited across exec, so neither process can be stopped by the first signal.
	_, g, descendant, done := startGroup(t, `trap '' TERM; sleep 60 & echo $! > "$1"; wait`)

	g.Term()

	// kill(2) has already posted the signal when Term returns, and an ignored signal is
	// discarded, so no waiting is needed to observe that the group survived.
	select {
	case <-done.ch:
		t.Fatalf("leader exited after SIGTERM despite ignoring it: %v", done.err)
	default:
	}
	if !g.Exists() || !g.Alive() || !pidAlive(descendant) {
		t.Fatalf("group did not survive SIGTERM: Exists=%v Alive=%v descendant=%v", g.Exists(), g.Alive(), pidAlive(descendant))
	}

	g.Kill()

	if err := waitExit(t, done); err == nil {
		t.Fatal("killed leader reported success")
	}
	waitFor(t, "descendant to exit", func() bool { return !pidAlive(descendant) })
	waitFor(t, "group to disappear", func() bool { return !g.Exists() })
}

func TestSignalsToExitedGroupAreHarmless(t *testing.T) {
	cmd := exec.Command("true")
	Prepare(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	g := Started(cmd)
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}

	// The leader has been reaped, so the group id no longer exists.
	waitFor(t, "group to disappear", func() bool { return !g.Exists() })
	if g.Alive() {
		t.Fatal("Alive reported true for an exited group")
	}
	g.Term()
	g.Kill()
	if g.Exists() || g.Alive() {
		t.Fatal("signalling an exited group changed its state")
	}
}

func TestAliveRejectsRecycledLeaderPID(t *testing.T) {
	_, g, _, _ := startGroup(t, `sleep 60 & echo $! > "$1"; wait`)
	if g.Start == "" {
		t.Skip("process start time unavailable (ps not usable)")
	}

	stale := g
	stale.Start = "Thu Jan  1 00:00:00 1970"
	if !stale.Exists() {
		t.Fatal("Exists must not depend on the start token")
	}
	if stale.Alive() {
		t.Fatal("a different start time must mean the original group is gone")
	}
	if !g.Alive() {
		t.Fatal("matching start time must be alive")
	}
}

func TestInvalidGroupIDsAreNeverSignalled(t *testing.T) {
	for _, pgid := range []int{-1, 0, 1} {
		g := Group{PID: pgid, PGID: pgid}
		if g.Exists() || g.Alive() {
			t.Errorf("Group{PGID: %d} reported as existing", pgid)
		}
	}
	// Term and Kill on PGID 1 are no-ops; a regression would signal init or the caller's own group.
	Group{PGID: 1}.Term()
	Group{PGID: 1}.Kill()
}

func TestStartTokenOfMissingProcess(t *testing.T) {
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if got := StartToken(cmd.Process.Pid); got != "" {
		t.Fatalf("StartToken of an exited process = %q, want empty", got)
	}
}
