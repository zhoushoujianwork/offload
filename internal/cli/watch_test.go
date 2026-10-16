package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/live"
)

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
	t.Setenv("FAKE_SLEEP", "1")
	done := make(chan struct{})
	go func() { defer close(done); runCLI("-d", work, "task") }()
	time.Sleep(500 * time.Millisecond) // let the worker publish its first state

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	code := cmdWatch(ctx, &out, &out)
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
	live.StaleAfter = 3 * time.Second // above the 2s refresh interval, below the salvage run
	t.Cleanup(func() { live.StaleAfter = origStale })
	t.Setenv("FAKE_PREFIX", `{"type":"system","subtype":"init","session_id":"s-killed"}`)
	t.Setenv("FAKE_SLEEP", "15")
	t.Setenv("FAKE_SALVAGE", "REPORT")
	t.Setenv("FAKE_SALVAGE_SLEEP", "7")
	done := make(chan struct{})
	go func() { defer close(done); runCLI("-d", work, "-t", "4s", "slow") }()

	lost, salvaging := false, false
	for end := time.Now().Add(25 * time.Second); time.Now().Before(end); time.Sleep(150 * time.Millisecond) {
		states, _ := live.List()
		for _, s := range states {
			lost = lost || s.Phase == live.Lost
			salvaging = salvaging || s.Phase == live.Salvaging
		}
		select {
		case <-done:
			end = time.Now()
		default:
		}
	}
	<-done
	if !salvaging || lost {
		t.Fatalf("salvaging seen=%v lost seen=%v; a refreshed state must not go stale", salvaging, lost)
	}
}
