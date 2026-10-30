package history

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)

func writeEvents(t *testing.T, path string, evs ...[3]string) {
	t.Helper()
	for _, e := range evs {
		at, err := time.Parse(time.RFC3339Nano, e[2])
		if err != nil {
			t.Fatal(err)
		}
		if err := AppendProcessing(path, e[0], 1, e[1], at); err != nil {
			t.Fatal(err)
		}
	}
}

func peakOf(t *testing.T, path string, now, ds, de time.Time, alive map[string]bool) DayPeak {
	t.Helper()
	log, err := ReadProcessing(path, ds.AddDate(0, 0, -7))
	if err != nil {
		t.Fatal(err)
	}
	return log.PeakInDay(log.Resolve(now, alive), ds, de)
}

func val(p DayPeak) int {
	if p.Value == nil {
		return -1
	}
	return *p.Value
}

func TestPeakHalfOpenBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.jsonl")
	// a ends exactly when b starts: no overlap. c overlaps b by one second.
	writeEvents(t, path,
		[3]string{"early", "start", "2026-10-19T23:00:00Z"}, [3]string{"early", "end", "2026-10-19T23:30:00Z"},
		[3]string{"a", "start", "2026-10-20T10:00:00Z"}, [3]string{"a", "end", "2026-10-20T10:00:05Z"},
		[3]string{"b", "start", "2026-10-20T10:00:05Z"}, [3]string{"b", "end", "2026-10-20T10:00:09Z"},
		[3]string{"c", "start", "2026-10-20T10:00:08Z"}, [3]string{"c", "end", "2026-10-20T10:00:10Z"},
	)
	now := t0.Add(24 * time.Hour)
	p := peakOf(t, path, now, t0, t0.Add(24*time.Hour), nil)
	if val(p) != 2 || p.Coverage != CoverageFull || p.At == nil || !p.At.Equal(time.Date(2026, 10, 20, 10, 0, 8, 0, time.UTC)) {
		t.Fatalf("peak %+v", p)
	}
	// Only touching spans: peak is 1.
	path2 := filepath.Join(t.TempDir(), "p.jsonl")
	writeEvents(t, path2, [3]string{"a", "start", "2026-10-19T00:00:00Z"}, [3]string{"a", "end", "2026-10-20T10:00:00Z"},
		[3]string{"b", "start", "2026-10-20T10:00:00Z"}, [3]string{"b", "end", "2026-10-20T11:00:00Z"})
	if p := peakOf(t, path2, now, t0, t0.Add(24*time.Hour), nil); val(p) != 1 {
		t.Fatalf("touching spans overlapped: %+v", p)
	}
}

func TestPeakCrossingMidnightCountsInBothDays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.jsonl")
	writeEvents(t, path,
		[3]string{"x", "start", "2026-10-20T23:59:00Z"}, [3]string{"x", "end", "2026-10-21T00:01:00Z"},
		[3]string{"y", "start", "2026-10-21T00:00:30Z"}, [3]string{"y", "end", "2026-10-21T00:00:40Z"})
	now := t0.Add(72 * time.Hour)
	d1 := peakOf(t, path, now, t0, t0.Add(24*time.Hour), nil)
	d2 := peakOf(t, path, now, t0.Add(24*time.Hour), t0.Add(48*time.Hour), nil)
	if val(d1) != 1 || val(d2) != 2 {
		t.Fatalf("day1 %+v day2 %+v", d1, d2)
	}
}

// Local days are calendar days, so a DST day is 23 or 25 hours long.
func TestPeakUsesCalendarDayAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	ds := time.Date(2026, 11, 1, 0, 0, 0, 0, loc) // fall back: 25 hours
	de := time.Date(2026, 11, 2, 0, 0, 0, 0, loc)
	if de.Sub(ds) != 25*time.Hour {
		t.Fatalf("fixture day is %v", de.Sub(ds))
	}
	path := filepath.Join(t.TempDir(), "p.jsonl")
	// Two spans that overlap only in the repeated hour (05:30Z is 01:30 EST, 05:30Z-1h is 01:30 EDT).
	writeEvents(t, path,
		[3]string{"a", "start", "2026-11-01T05:10:00Z"}, [3]string{"a", "end", "2026-11-01T05:50:00Z"},
		[3]string{"b", "start", "2026-11-01T05:20:00Z"}, [3]string{"b", "end", "2026-11-01T05:30:00Z"},
		[3]string{"z", "start", "2026-11-02T04:59:59Z"}, [3]string{"z", "end", "2026-11-02T05:30:00Z"}) // 23:59:59 EST: last second of the day
	now := de.Add(time.Hour)
	if p := peakOf(t, path, now, ds, de, nil); val(p) != 2 {
		t.Fatalf("dst day peak %+v", p)
	}
	next := time.Date(2026, 11, 3, 0, 0, 0, 0, loc)
	if p := peakOf(t, path, next.Add(time.Hour), de, next, nil); val(p) != 1 {
		t.Fatalf("next day: z must count after midnight %+v", p)
	}
}

func TestPeakResumedInvocationsAreSeparateRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.jsonl")
	// The same session resumed three times, sequentially, and a salvaging phase that re-opens a span
	// of the same run after a gap: spans never merge or double up.
	writeEvents(t, path,
		[3]string{"pre", "start", "2026-10-19T12:00:00Z"}, [3]string{"pre", "end", "2026-10-19T12:00:01Z"}, // evidence precedes the day
		[3]string{"r1", "start", "2026-10-20T10:00:00Z"}, [3]string{"r1", "end", "2026-10-20T10:01:00Z"},
		[3]string{"r2", "start", "2026-10-20T10:01:00Z"}, [3]string{"r2", "end", "2026-10-20T10:02:00Z"},
		[3]string{"r3", "start", "2026-10-20T10:02:00Z"}, [3]string{"r3", "end", "2026-10-20T10:03:00Z"},
		[3]string{"r3", "start", "2026-10-20T10:03:30Z"}, [3]string{"r3", "end", "2026-10-20T10:04:00Z"})
	if p := peakOf(t, path, t0.Add(24*time.Hour), t0, t0.Add(24*time.Hour), nil); val(p) != 1 || p.Coverage != CoverageFull {
		t.Fatalf("%+v", p)
	}
}

func TestPeakCrashLeavesIncompleteLowerBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.jsonl")
	writeEvents(t, path,
		[3]string{"live", "start", "2026-10-20T10:00:00Z"},
		[3]string{"crashed", "start", "2026-10-20T10:00:10Z"},
		[3]string{"ok", "start", "2026-10-20T10:00:20Z"}, [3]string{"ok", "end", "2026-10-20T10:00:30Z"})
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	p := peakOf(t, path, now, t0, t0.Add(24*time.Hour), map[string]bool{"live": true})
	// live runs until now: with ok overlapping it the peak is 2; the crashed one counts only at its start instant.
	if val(p) != 2 || p.Coverage != CoveragePartial || p.IncompleteRuns != 1 {
		t.Fatalf("%+v", p)
	}
	// Reopening (a fresh read) gives the same answer: the evidence is on disk.
	p2 := peakOf(t, path, now, t0, t0.Add(24*time.Hour), map[string]bool{"live": true})
	if val(p2) != val(p) || p2.Coverage != p.Coverage {
		t.Fatalf("reopen %+v vs %+v", p2, p)
	}
}

func TestPeakShortTaskBetweenPollsIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.jsonl")
	var evs [][3]string
	for i := 0; i < 5; i++ {
		id := fmt.Sprint("s", i)
		evs = append(evs, [3]string{id, "start", "2026-10-20T10:00:00.100Z"}, [3]string{id, "end", "2026-10-20T10:00:00.350Z"})
	}
	writeEvents(t, path, evs...)
	if p := peakOf(t, path, t0.Add(24*time.Hour), t0, t0.Add(24*time.Hour), nil); val(p) != 5 {
		t.Fatalf("sub-second overlap lost: %+v", p)
	}
}

func TestPeakUnavailableAndPartialForLegacyAndMissing(t *testing.T) {
	dir := t.TempDir()
	now := t0.Add(24 * time.Hour)
	// No file at all: unavailable, never zero.
	missing := peakOf(t, filepath.Join(dir, "none.jsonl"), now, t0, now, nil)
	if missing.Value != nil || missing.Coverage != CoverageUnavailable {
		t.Fatalf("missing: %+v", missing)
	}
	// Empty file is the same.
	empty := filepath.Join(dir, "empty.jsonl")
	os.WriteFile(empty, nil, 0o600)
	if p := peakOf(t, empty, now, t0, now, nil); p.Value != nil {
		t.Fatalf("empty: %+v", p)
	}
	// Events begin mid-window: the day is a lower bound. A day wholly before them is unavailable.
	path := filepath.Join(dir, "p.jsonl")
	writeEvents(t, path, [3]string{"a", "start", "2026-10-20T12:00:00Z"}, [3]string{"a", "end", "2026-10-20T12:01:00Z"})
	if p := peakOf(t, path, now, t0, t0.Add(24*time.Hour), nil); val(p) != 1 || p.Coverage != CoveragePartial {
		t.Fatalf("starts mid-day: %+v", p)
	}
	before := t0.AddDate(0, 0, -3)
	if p := peakOf(t, path, now, before, before.Add(24*time.Hour), nil); p.Value != nil || p.Coverage != CoverageUnavailable {
		t.Fatalf("before first event: %+v", p)
	}
	// Corrupt and orphan lines degrade coverage instead of being ignored.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("not json\n")
	f.Close()
	writeEvents(t, path, [3]string{"ghost", "end", "2026-10-20T13:00:00Z"})
	log, _ := ReadProcessing(path, t0)
	if log.Corrupt != 1 || log.Unmatched != 1 {
		t.Fatalf("corrupt=%d unmatched=%d", log.Corrupt, log.Unmatched)
	}
}

func TestAppendProcessingKeepsNanosecondPrecision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.jsonl")
	at := time.Date(2026, 10, 20, 10, 0, 0, 123456789, time.UTC)
	if err := AppendProcessing(path, "r", 7, ProcessingStart, at); err != nil {
		t.Fatal(err)
	}
	log, err := ReadProcessing(path, t0)
	if err != nil || !log.Open["r"].Equal(at) {
		t.Fatalf("%v %v", log.Open, err)
	}
}

// A crash whose span began on an earlier day leaves the later day's peak unknown, not an exact 0.
func TestPeakCrashAcrossMidnightIsLowerBoundOnLaterDays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "p.jsonl")
	writeEvents(t, path,
		[3]string{"crashed", "start", "2026-10-20T23:59:00Z"},
		[3]string{"later", "start", "2026-10-21T10:00:00Z"}, [3]string{"later", "end", "2026-10-21T10:00:05Z"})
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	d1 := t0.Add(24 * time.Hour)
	p := peakOf(t, path, now, d1, d1.Add(24*time.Hour), nil)
	if val(p) != 1 || p.Coverage != CoveragePartial || p.IncompleteRuns != 1 {
		t.Fatalf("day after the crash: %+v", p)
	}
	// With no other run the later day must still not be an exact zero.
	d2 := d1.Add(24 * time.Hour)
	p = peakOf(t, path, now, d2, d2.Add(24*time.Hour), nil)
	if val(p) != 0 || p.Coverage != CoveragePartial {
		t.Fatalf("second day after the crash: %+v", p)
	}
	// A run that is confirmed alive is open until now and is not incomplete.
	p = peakOf(t, path, now, d1, d1.Add(24*time.Hour), map[string]bool{"crashed": true})
	if val(p) != 2 || p.Coverage != CoverageFull || p.IncompleteRuns != 0 {
		t.Fatalf("alive: %+v", p)
	}
}
