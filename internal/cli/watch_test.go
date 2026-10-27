package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/live"
)

type watchWriter func([]byte) (int, error)

func (w watchWriter) Write(p []byte) (int, error) { return w(p) }

func newTestWatcher() (*watcher, *bytes.Buffer) {
	var buf bytes.Buffer
	return &watcher{seen: map[int]*seenWorker{}, out: &buf}, &buf
}

func fastWatch(t *testing.T) {
	t.Helper()
	oe, os_, oh := watchEvery, stallAfter, heartbeat
	t.Cleanup(func() { watchEvery, stallAfter, heartbeat = oe, os_, oh })
}

func TestWatcherReportsStallResumeAndFinish(t *testing.T) {
	fastWatch(t)
	stallAfter, heartbeat = time.Minute, time.Hour
	w, out := newTestWatcher()
	now := time.Now()
	run := live.State{PID: 9, Phase: live.Running, StartedAt: now, LastEventAt: now, ToolCalls: 2, Session: "s-9", Task: "t"}

	if n := w.step([]live.State{run}); n != 1 {
		t.Fatalf("active=%d", n)
	}
	stuck := run
	stuck.LastEventAt = now.Add(-2 * time.Minute)
	w.step([]live.State{stuck})
	w.step([]live.State{stuck}) // a stall is reported once, not every poll
	w.step([]live.State{run})   // events resumed
	done := run
	done.Phase, done.Status = live.Done, "ok"
	if n := w.step([]live.State{done}); n != 0 {
		t.Fatalf("active=%d after done", n)
	}
	got := out.String()
	for _, want := range []string{"started pid=9", "STALLED pid=9", "resumed pid=9", "finished status=ok"} {
		if strings.Count(got, want) != 1 {
			t.Errorf("want exactly one %q in:\n%s", want, got)
		}
	}
}

func TestWatcherIgnoresWorkersThatFinishedBeforeWatching(t *testing.T) {
	w, out := newTestWatcher()
	w.step([]live.State{{PID: 3, Phase: live.Done, Status: "ok", StartedAt: time.Now()}})
	if out.Len() != 0 {
		t.Fatalf("old finished worker is not news: %q", out.String())
	}
}

func TestWatcherReportsLostAndSalvaging(t *testing.T) {
	w, out := newTestWatcher()
	now := time.Now()
	w.step([]live.State{{PID: 4, Phase: live.Running, StartedAt: now, LastEventAt: now}})
	w.step([]live.State{{PID: 4, Phase: live.Salvaging, StartedAt: now, LastEventAt: now}})
	w.step([]live.State{{PID: 4, Phase: live.Lost, StartedAt: now, LastEventAt: now}})
	for _, want := range []string{"asking the worker for a report", "LOST"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %q", want, out.String())
		}
	}
}

func TestStatusAndWatchWithNothingRunning(t *testing.T) {
	setup(t, okReply)
	code, out, _ := runCLI("status")
	if code != 0 || !strings.Contains(out, "no workers") {
		t.Fatalf("status: %d %q", code, out)
	}
	code, out, _ = runCLI("watch")
	if code != 0 || !strings.Contains(out, "no active workers") {
		t.Fatalf("watch must not hang on nothing: %d %q", code, out)
	}
}

// watch must see a real run: started, then finished, then exit.
func TestWatchFollowsARealRunToTheEnd(t *testing.T) {
	fastWatch(t)
	watchEvery = 50 * time.Millisecond
	work, _ := setup(t, okReply)
	release := filepath.Join(t.TempDir(), "release")
	fake := os.Getenv("OFFLOAD_CLAUDE")
	script, err := os.ReadFile(fake)
	if err != nil {
		t.Fatal(err)
	}
	script = bytes.Replace(script, []byte("cat <<'EOF'"), []byte("while [ ! -f '"+release+"' ]; do sleep 0.02; done\ncat <<'EOF'"), 1)
	if err := os.WriteFile(fake, script, 0700); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); runCLI("-d", work, "-t", "30s", "task") }()
	t.Cleanup(func() {
		os.WriteFile(release, nil, 0600)
		<-done
	})
	waitFor(t, "running worker", 30*time.Second, func() bool { return phases(t)[live.Running] == 1 })

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// watch 实际报告启动后才放行后端。
	observed := watchWriter(func(p []byte) (int, error) {
		n, err := out.Write(p)
		if bytes.Contains(p, []byte("started pid=")) {
			if e := os.WriteFile(release, nil, 0600); e != nil {
				return n, e
			}
		}
		return n, err
	})
	code := cmdWatch(ctx, observed, &out)
	<-done
	got := out.String()
	if code != 0 || !strings.Contains(got, "started") || !strings.Contains(got, "finished status=ok") || !strings.Contains(got, "all workers finished") {
		t.Fatalf("watch did not follow the run: code=%d\n%s", code, got)
	}
}

// The tool-less report can take longer than live.StaleAfter. A worker in that phase is working,
// not lost, so its state must keep being refreshed.
func TestSalvagingWorkerIsNotReportedLost(t *testing.T) {
	work, _ := setup(t, okReply)
	origStale := live.StaleAfter
	live.StaleAfter = 10 * time.Second // 为 2s 刷新周期预留调度余量。
	t.Cleanup(func() { live.StaleAfter = origStale })
	release := filepath.Join(t.TempDir(), "release")
	fake := os.Getenv("OFFLOAD_CLAUDE")
	t.Setenv("FAKE_RELEASE", release)
	script := `#!/bin/sh
case " $* " in
*' --tools '*)
  while [ ! -f "$FAKE_RELEASE" ]; do sleep 0.02; done
  printf '%s\n' '{"type":"result","result":"REPORT","session_id":"s-killed"}'
  ;;
*)
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"s-killed"}'
  sleep 60
  ;;
esac
`
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); runCLI("-d", work, "-t", "10s", "slow") }()

	t.Cleanup(func() { os.WriteFile(release, nil, 0600); <-done })
	var first time.Time
	waitFor(t, "salvage heartbeat beyond the stale interval", 45*time.Second, func() bool {
		states, err := live.List()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range states {
			if s.Phase == live.Lost {
				t.Fatal("a refreshed salvage state went stale")
			}
			if s.Phase == live.Salvaging {
				if first.IsZero() {
					first = s.UpdatedAt
				}
				return s.UpdatedAt.Sub(first) > live.StaleAfter
			}
		}
		select {
		case <-done:
			t.Fatal("worker finished before salvage heartbeats were observed")
		default:
		}
		return false
	})
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	<-done
}

// A bounded watch is one long wait instead of many short polls: it returns 124 with a clear
// line when workers are still active, and still reports their events.
func TestWatchLimitReturnsWhileWorkerStillActive(t *testing.T) {
	fastWatch(t)
	watchEvery = 20 * time.Millisecond
	setup(t, okReply)
	now := time.Now()
	if err := live.Publish(live.State{PID: os.Getpid(), Phase: live.Running, StartedAt: now, LastEventAt: now, Session: "s-w"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	code := cmdWatch(ctx, &out, &out)
	if code != 124 || !strings.Contains(out.String(), "started pid=") || !strings.Contains(out.String(), "watch limit reached, 1 worker(s) still active") {
		t.Fatalf("code=%d\n%s", code, out.String())
	}
}

func TestWatchFlagValidation(t *testing.T) {
	setup(t, okReply)
	if code, _, _ := runCLI("watch", "extra"); code != 2 {
		t.Errorf("unexpected argument: code=%d", code)
	}
	if code, _, _ := runCLI("watch", "-t", "-1s"); code != 2 {
		t.Errorf("negative bound: code=%d", code)
	}
	if code, out, _ := runCLI("watch", "-t", "5s"); code != 0 || !strings.Contains(out, "no active workers") {
		t.Errorf("bounded watch with nothing running: %d %q", code, out)
	}
}

func TestWatcherReportsQueueCleanupAndUnresolved(t *testing.T) {
	var out bytes.Buffer
	w := &watcher{seen: map[int]*seenWorker{}, out: &out}
	now := time.Now()
	slot := 1
	w.step([]live.State{{PID: 8, Phase: live.Queued, WaitReason: "all 2 worker slot(s) are busy (2 running)", StartedAt: now, LastEventAt: now}})
	w.step([]live.State{{PID: 8, Phase: live.Running, Slot: &slot, WaitMs: 4000, PGID: 99, StartedAt: now, LastEventAt: now}})
	w.step([]live.State{{PID: 8, Phase: live.Cleaning, Slot: &slot, PGID: 99, StartedAt: now, LastEventAt: now}})
	active := w.step([]live.State{{PID: 8, Phase: live.Unresolved, PGID: 99, WaitReason: "still running", StartedAt: now, LastEventAt: now}})
	if active != 0 {
		t.Logf("unresolved phase counted as active: %d", active)
	}
	for _, want := range []string{"wait_reason=", "admitted after waiting 4s", "pgid=99", "slot=1", "stopping the worker's child processes", "UNRESOLVED"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
}
