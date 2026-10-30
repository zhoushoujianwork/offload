package worker

import (
	"encoding/json"
	"github.com/zhoushoujianwork/offload/internal/slots"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/live"
	"github.com/zhoushoujianwork/offload/internal/proc"
)

func processingEvents(t *testing.T) []history.ProcessingEvent {
	t.Helper()
	b, err := os.ReadFile(history.ProcessingPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []history.ProcessingEvent
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e history.ProcessingEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func kinds(evs []history.ProcessingEvent) string {
	s := ""
	for _, e := range evs {
		s += e.Event[:1]
	}
	return s
}

// Queue wait and cleanup are not processing. Running followed by salvaging is one span inside one
// invocation, and a finished run always closes its span.
func TestPublisherRecordsOnlyProcessingSpans(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	o := Opts{RunID: "run-1", Task: "t", Dir: "/w"}
	p := newPublisher(o, time.Now())
	if evs := processingEvents(t); len(evs) != 0 {
		t.Fatalf("queued run recorded processing: %v", evs)
	}
	p.waiting(slots.Wait{Max: 1, InUse: 1})
	p.admitted(0, time.Second)
	p.update(streamSummary{}, time.Now()) // heartbeats are not events
	p.phase(live.Running)                 // idempotent
	p.phase(live.Salvaging)               // salvage stays inside the same invocation's processing
	p.phase(live.Cleaning)
	p.phase(live.Running) // after cleaning is a no-op
	p.finish(history.Record{Status: "ok"}, nil)
	evs := processingEvents(t)
	if kinds(evs) != "se" {
		t.Fatalf("events %q", kinds(evs))
	}
	for _, e := range evs {
		if e.RunID != "run-1" || e.PID != os.Getpid() || e.Version != 1 {
			t.Errorf("event %+v", e)
		}
	}
	st, _ := live.List()
	if len(st) != 1 || st[0].RunID != "run-1" || st[0].Processing() {
		t.Fatalf("live %+v", st)
	}
}

func TestPublisherFinishWhileRunningClosesSpanAndUnresolvedIsNotProcessing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	p := newPublisher(Opts{RunID: "run-2"}, time.Now())
	p.admitted(0, 0)
	p.finish(history.Record{Status: "error"}, &proc.Group{PGID: 1})
	if got := kinds(processingEvents(t)); got != "se" {
		t.Fatalf("events %q", got)
	}
	if st, _ := live.List(); len(st) != 1 || st[0].Processing() {
		t.Fatalf("%+v", st)
	}
}

// A run cancelled while queued never records processing.
func TestPublisherCancelledQueuedRecordsNothing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	p := newPublisher(Opts{RunID: "run-3"}, time.Now())
	p.finish(history.Record{Status: "error", ErrorKind: "canceled"}, nil)
	if evs := processingEvents(t); len(evs) != 0 {
		t.Fatalf("%v", evs)
	}
}
