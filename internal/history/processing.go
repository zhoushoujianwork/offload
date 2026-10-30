package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Processing events record when an invocation really occupies a worker: from admission (running)
// until it stops processing (cleanup, finish). Queue wait, cleanup and unresolved leftovers are
// outside the interval. They are the durable evidence behind the daily peak concurrency; live
// files are overwritten and expire, so they cannot rebuild a peak.
const (
	ProcessingStart = "start"
	ProcessingEnd   = "end"

	processingVersion = 1
)

// ProcessingEvent is one line of processing.jsonl. TS is RFC3339 with nanoseconds.
type ProcessingEvent struct {
	Version int    `json:"v"`
	RunID   string `json:"run_id"`
	PID     int    `json:"pid"`
	Event   string `json:"event"`
	TS      string `json:"ts"`
}

// ProcessingPath sits next to the run log.
func ProcessingPath() string { return filepath.Join(filepath.Dir(Path()), "processing.jsonl") }

// AppendProcessing adds one event. Each event is a single O_APPEND write of one short line.
func AppendProcessing(path, runID string, pid int, event string, at time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(ProcessingEvent{Version: processingVersion, RunID: runID, PID: pid, Event: event, TS: at.Format(time.RFC3339Nano)})
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// Interval is a half-open processing span [Start, End). An Incomplete interval never recorded
// its end (a crash, or a kill); it is kept as a one-nanosecond span at Start so it counts only
// at the instant it is known to have been processing, which makes any peak a lower bound.
type Interval struct {
	Start, End time.Time
	Incomplete bool
}

// ProcessingLog is the streamed content of processing.jsonl. Closed intervals that ended before
// the requested window are dropped while reading, so memory stays bounded by the window.
type ProcessingLog struct {
	HasEvents bool
	Since     time.Time // earliest event: peaks before it are unknown
	Closed    []Interval
	Open      map[string]time.Time // run_id -> start of a span with no end yet
	Seen      map[string]bool      // run_id of every start at or after the window start
	Unmatched int                  // end without start, start twice, end before start
	Corrupt   int                  // unreadable lines
}

// ReadProcessing streams the log. A missing file is an empty log.
func ReadProcessing(path string, windowStart time.Time) (ProcessingLog, error) {
	log := ProcessingLog{Open: map[string]time.Time{}, Seen: map[string]bool{}}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return log, nil
	}
	if err != nil {
		return log, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e ProcessingEvent
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.RunID == "" || (e.Event != ProcessingStart && e.Event != ProcessingEnd) {
			log.Corrupt++
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, e.TS)
		if err != nil {
			log.Corrupt++
			continue
		}
		if !log.HasEvents || at.Before(log.Since) {
			log.Since = at
		}
		log.HasEvents = true
		start, open := log.Open[e.RunID]
		switch e.Event {
		case ProcessingStart:
			if !at.Before(windowStart) {
				log.Seen[e.RunID] = true
			}
			if open {
				// The earlier span never ended; keep it as a point.
				log.Unmatched++
				log.addClosed(Interval{Start: start, End: start.Add(time.Nanosecond), Incomplete: true}, windowStart)
			}
			log.Open[e.RunID] = at
		case ProcessingEnd:
			if !open || at.Before(start) {
				log.Unmatched++
				delete(log.Open, e.RunID)
				continue
			}
			delete(log.Open, e.RunID)
			log.addClosed(Interval{Start: start, End: at}, windowStart)
		}
	}
	return log, sc.Err()
}

func (l *ProcessingLog) addClosed(iv Interval, windowStart time.Time) {
	if iv.End.After(windowStart) {
		l.Closed = append(l.Closed, iv)
	}
}

// Resolve returns the spans of the log at now. A span with no end is open until now when its
// run is confirmed still processing (alive), and otherwise Incomplete.
func (l ProcessingLog) Resolve(now time.Time, alive map[string]bool) []Interval {
	out := append([]Interval(nil), l.Closed...)
	for id, start := range l.Open {
		if alive[id] {
			out = append(out, Interval{Start: start, End: now})
		} else {
			out = append(out, Interval{Start: start, End: start.Add(time.Nanosecond), Incomplete: true})
		}
	}
	return out
}

// Peak coverage values.
const (
	CoverageFull        = "full"
	CoverageComplete    = "complete"
	CoveragePartial     = "partial"
	CoverageUnavailable = "unavailable"
)

// DayPeak is the highest number of overlapping processing spans within one calendar day.
// Value is nil when unavailable. A partial peak is a lower bound.
type DayPeak struct {
	Value          *int
	At             *time.Time
	Coverage       string
	IncompleteRuns int
}

// PeakInDay computes the peak over [ds, de) from resolved spans. Spans are half-open, so one
// that ends at the instant another starts does not overlap it. A span crossing midnight counts
// in both days.
func (l ProcessingLog) PeakInDay(spans []Interval, ds, de time.Time) DayPeak {
	if !l.HasEvents || !de.After(l.Since) {
		return DayPeak{Coverage: CoverageUnavailable}
	}
	type edge struct {
		at    time.Time
		delta int
	}
	var edges []edge
	incomplete := 0
	for _, s := range spans {
		start, end := s.Start, s.End
		if !end.After(ds) || !start.Before(de) {
			continue
		}
		if s.Incomplete {
			incomplete++
		}
		if start.Before(ds) {
			start = ds
		}
		if end.After(de) {
			end = de
		}
		if !end.After(start) {
			continue
		}
		edges = append(edges, edge{start, 1}, edge{end, -1})
	}
	sort.Slice(edges, func(i, j int) bool {
		if !edges[i].at.Equal(edges[j].at) {
			return edges[i].at.Before(edges[j].at)
		}
		return edges[i].delta < edges[j].delta // ends before starts at the same instant
	})
	cur, best := 0, 0
	var bestAt time.Time
	for _, e := range edges {
		cur += e.delta
		if cur > best {
			best, bestAt = cur, e.at
		}
	}
	p := DayPeak{Value: &best, Coverage: CoverageFull, IncompleteRuns: incomplete}
	if best > 0 {
		p.At = &bestAt
	}
	if ds.Before(l.Since) || incomplete > 0 || l.Unmatched > 0 || l.Corrupt > 0 {
		p.Coverage = CoveragePartial
	}
	return p
}
