package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/zhoushoujianwork/offload/internal/live"
	"github.com/zhoushoujianwork/offload/internal/text"
)

// watchEvery, stallAfter and heartbeat are variables only so tests can shorten them.
var (
	watchEvery = 2 * time.Second
	stallAfter = 60 * time.Second // no event from a running worker for this long is worth a line
	heartbeat  = 60 * time.Second // a running worker that made progress is reported at most this often
)

func describe(s live.State) string {
	idle := time.Since(s.LastEventAt).Round(time.Second)
	line := fmt.Sprintf("pid=%d %s tool_calls=%d permission_denials=%d idle=%s session=%s dir=%s task=%q",
		s.PID, s.Phase, s.ToolCalls, s.Denials, idle, firstNonEmpty(s.Session, "-"), s.Dir, text.Truncate(s.Task, 60))
	if s.LastTool != "" {
		line += " last=" + text.Truncate(s.LastTool, 80)
	}
	return line
}

// cmdStatus prints one line per known worker: what is running, and what finished recently.
func cmdStatus(stdout, stderr io.Writer) int {
	states, err := live.List()
	if err != nil {
		return fail(stderr, err)
	}
	if len(states) == 0 {
		fmt.Fprintln(stdout, "no workers")
		return 0
	}
	for _, s := range states {
		line := describe(s)
		if s.Phase == live.Done {
			line = fmt.Sprintf("pid=%d done status=%s exit_code=%d tool_calls=%d session=%s task=%q",
				s.PID, s.Status, s.ExitCode, s.ToolCalls, firstNonEmpty(s.Session, "-"), text.Truncate(s.Task, 60))
		}
		fmt.Fprintln(stdout, line)
	}
	return 0
}

// watcher turns changes in the workers' live state into one-line events. A supervisor can run
// `offload watch` under a monitor and be told the moment a worker stalls or finishes, instead of
// polling or waiting for the timeout.
type watcher struct {
	seen map[int]*seenWorker
	out  io.Writer
}

type seenWorker struct {
	phase     string
	toolCalls int
	stalled   bool
	reported  time.Time
}

func (w *watcher) emit(format string, a ...any) { fmt.Fprintf(w.out, format+"\n", a...) }

// step reports what changed since the last call and returns how many workers are still active.
func (w *watcher) step(states []live.State) int {
	active := 0
	for _, s := range states {
		prev := w.seen[s.PID]
		if prev == nil {
			prev = &seenWorker{reported: time.Now()}
			w.seen[s.PID] = prev
			if s.Phase == live.Done && !s.Active() {
				prev.phase = s.Phase // finished before we looked; not news
				continue
			}
			w.emit("[offload] started %s", describe(s))
		}
		if s.Active() {
			active++
		}
		idle := time.Since(s.LastEventAt)
		switch {
		case s.Phase != prev.phase && s.Phase == live.Salvaging:
			w.emit("[offload] stopped, asking the worker for a report from what it read: %s", describe(s))
		case s.Phase != prev.phase && s.Phase == live.Lost:
			w.emit("[offload] LOST (offload died or was killed, no updates): %s", describe(s))
		case s.Phase != prev.phase && s.Phase == live.Done:
			w.emit("[offload] finished status=%s exit_code=%d tool_calls=%d permission_denials=%d session=%s task=%q",
				s.Status, s.ExitCode, s.ToolCalls, s.Denials, firstNonEmpty(s.Session, "-"), text.Truncate(s.Task, 60))
		case s.Phase == live.Running && idle >= stallAfter && !prev.stalled:
			prev.stalled = true
			w.emit("[offload] STALLED %s", describe(s))
		case s.Phase == live.Running && prev.stalled && idle < stallAfter:
			prev.stalled = false
			w.emit("[offload] resumed %s", describe(s))
		case s.Phase == live.Running && s.ToolCalls != prev.toolCalls && time.Since(prev.reported) >= heartbeat:
			prev.reported = time.Now()
			w.emit("[offload] progress %s", describe(s))
		}
		prev.phase, prev.toolCalls = s.Phase, s.ToolCalls
	}
	return active
}

// cmdWatch prints events until every worker has finished, then exits 0. With no active worker at
// the start it says so and exits at once, so a monitor never hangs on nothing.
func cmdWatch(ctx context.Context, stdout, stderr io.Writer) int {
	w := &watcher{seen: map[int]*seenWorker{}, out: stdout}
	started := false
	for {
		states, err := live.List()
		if err != nil {
			return fail(stderr, err)
		}
		active := w.step(states)
		if active > 0 {
			started = true
		} else if !started {
			fmt.Fprintln(stdout, "[offload] no active workers")
			return 0
		} else {
			fmt.Fprintln(stdout, "[offload] all workers finished")
			return 0
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				// A bounded wait ended with work still running: not a failure of the worker.
				fmt.Fprintf(stdout, "[offload] watch limit reached, %d worker(s) still active; run offload status or watch again\n", active)
				return 124
			}
			return 130
		case <-time.After(watchEvery):
		}
	}
}
