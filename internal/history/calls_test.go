package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func rec(ts string) Record { return Record{TS: ts, Status: "ok"} }

func TestCountDailyCallsSevenCompleteDaysWithZeros(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	rs := []Record{rec("2026-10-20 00:00:00"), rec("2026-10-22 08:00:00"), rec("2026-10-22 09:00:00"), rec("2026-10-28 23:59:59"),
		rec("2026-10-29 01:00:00"), rec("2026-10-29 11:00:00")}
	w := CountDailyCalls(rs, now, 7)
	want := []int{2, 0, 0, 0, 0, 0, 1} // Oct 22..28
	if len(w.Days) != 7 || w.Days[0].Start.Day() != 22 || w.Days[6].Start.Day() != 28 {
		t.Fatalf("window %+v", w.Days)
	}
	for i, d := range w.Days {
		if d.Calls != want[i] || !d.Covered {
			t.Errorf("day %d = %+v, want %d covered", i, d, want[i])
		}
	}
	if w.Today.Calls != 2 || w.Today.Start.Day() != 29 {
		t.Errorf("today %+v", w.Today)
	}
}

func TestCountDailyCallsPartialCoverageAndEmpty(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	w := CountDailyCalls([]Record{rec("2026-10-26 10:00:00")}, now, 7)
	covered := 0
	for _, d := range w.Days {
		if d.Covered {
			covered++
		}
	}
	if covered != 3 || w.Days[2].Covered || !w.Days[4].Covered { // Oct 26,27,28 covered
		t.Fatalf("covered %d: %+v", covered, w.Days)
	}
	e := CountDailyCalls(nil, now, 7)
	for _, d := range e.Days {
		if d.Covered {
			t.Fatalf("empty log vouches for nothing: %+v", d)
		}
	}
	if !e.Today.Covered || e.Today.Calls != 0 {
		t.Fatalf("today of an empty log is a real zero: %+v", e.Today)
	}
}

func TestCountDailyCallsSkipsUnreadableAndFuture(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	w := CountDailyCalls([]Record{rec("garbage"), rec("2026-10-30 00:00:00"), rec("2026-10-29 10:00:00")}, now, 7)
	if w.Skipped != 2 || w.Today.Calls != 1 {
		t.Fatalf("%+v", w)
	}
}

func TestCountDailyCallsDSTDays(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, loc)
	rs := []Record{rec("2026-11-01 00:00:00"), rec("2026-11-01 23:59:59"), rec("2026-11-02 00:00:00"), rec("2026-03-08 12:00:00")}
	w := CountDailyCalls(rs, now, 7)
	// Window is Oct 27..Nov 2; Nov 1 is 25 hours long but is one calendar day.
	if w.Days[0].Start.Format("2006-01-02") != "2026-10-27" || w.Days[5].Calls != 2 || w.Days[6].Calls != 1 {
		t.Fatalf("%+v", w.Days)
	}
	// The spring-forward day is also a single calendar day.
	spring := time.Date(2026, 3, 9, 12, 0, 0, 0, loc)
	w = CountDailyCalls([]Record{rec("2026-03-08 00:00:00"), rec("2026-03-08 23:59:59")}, spring, 7)
	if w.Days[6].Calls != 2 {
		t.Fatalf("%+v", w.Days)
	}
}

func TestUncoveredByIgnoresNeverAdmittedRuns(t *testing.T) {
	exec := int64(5)
	rs := []Record{
		{TS: "2026-10-20 10:00:00", RunID: "seen", ExecMs: &exec},
		{TS: "2026-10-20 10:00:01", RunID: "old", ExecMs: &exec},
		{TS: "2026-10-20 10:00:02"},                           // setup failure: not admitted
		{TS: "2026-10-20 10:00:03", RunID: "", ExecMs: &exec}, // admitted legacy run without id
	}
	log := ProcessingLog{Seen: map[string]bool{"seen": true}}
	if n := UncoveredBy(rs, t0, t0.Add(24*time.Hour), log); n != 2 {
		t.Fatalf("uncovered %d", n)
	}
}

// 70,000 records (10,000 a day for a week) and 100 simultaneous spans aggregate in one pass each.
func TestSyntheticWeekAggregationIsBounded(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	var rs []Record
	for d := 7; d >= 1; d-- {
		day := time.Date(2026, 10, 29-d, 0, 0, 0, 0, time.UTC)
		for i := 0; i < 10000; i++ {
			rs = append(rs, Record{TS: day.Add(time.Duration(i) * 8 * time.Second).Format("2006-01-02 15:04:05"), RunID: fmt.Sprint(d, "-", i)})
		}
	}
	start := time.Now()
	w := CountDailyCalls(rs, now, 7)
	took := time.Since(start)
	for _, d := range w.Days {
		if d.Calls != 10000 {
			t.Fatalf("%+v", d)
		}
	}
	log := ProcessingLog{HasEvents: true, Since: now.AddDate(0, 0, -8), Open: map[string]time.Time{}}
	for i := 0; i < 100; i++ {
		log.Open[fmt.Sprint("w", i)] = now.Add(-time.Hour)
	}
	alive := map[string]bool{}
	for id := range log.Open {
		alive[id] = true
	}
	p := log.PeakInDay(log.Resolve(now, alive), now.Add(-12*time.Hour), now.Add(time.Nanosecond))
	if val(p) != 100 {
		t.Fatalf("%+v", p)
	}
	if took > 2*time.Second {
		t.Errorf("counting 70,000 records took %v", took)
	}
	t.Logf("CountDailyCalls over %d records: %v", len(rs), took)
}

func TestReadStatsKeepsOnlyRecentResultBodies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 50; i++ {
		ts := time.Date(2026, 10, 28, 0, 0, i, 0, time.UTC).Format("2006-01-02 15:04:05")
		if err := Append(path, Record{TS: ts, RunID: fmt.Sprint(i), Status: "ok", Result: fmt.Sprint("body-", i)}); err != nil {
			t.Fatal(err)
		}
	}
	// A damaged line and a blank line: only the damaged one counts as corrupt.
	if err := os.WriteFile(path, append(mustRead(t, path), []byte("{\"ts\":\"2026-10-28 01:\n\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := ReadStats(path, now)
	if err != nil || len(l.Records) != 50 || l.Corrupt != 1 {
		t.Fatalf("%d records corrupt=%d err=%v", len(l.Records), l.Corrupt, err)
	}
	withBody := 0
	for _, r := range l.Records {
		if r.Result != "" {
			withBody++
		}
	}
	if withBody != RecentLimit {
		t.Fatalf("bodies kept for %d records, want %d", withBody, RecentLimit)
	}
	full, _ := Read(path)
	a, _ := Aggregate(full, PeriodAll, now)
	b, _ := Aggregate(l.Records, PeriodAll, now)
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	if string(ja) != string(jb) {
		t.Fatalf("stats differ:\n%s\n%s", ja, jb)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Metadata refresh must not materialize historical report bodies: reading a log whose bodies are
// far larger than the listed ones allocates a small fraction of the log size.
func TestReadStatsDoesNotMaterializeHistoricalBodies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	body := strings.Repeat("x", 100<<10)
	const n = 300
	for i := 0; i < n; i++ {
		ts := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second).Format("2006-01-02 15:04:05")
		if err := Append(path, Record{TS: ts, RunID: fmt.Sprint(i), Status: "ok", Result: body}); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	l, err := ReadStats(path, now)
	runtime.ReadMemStats(&after)
	if err != nil || len(l.Records) != n {
		t.Fatalf("%d records, err=%v", len(l.Records), err)
	}
	withBody := 0
	for _, r := range l.Records {
		if r.Result != "" {
			withBody++
			if r.Result != body {
				t.Fatalf("listed body altered")
			}
		}
	}
	if withBody != RecentLimit {
		t.Fatalf("bodies kept for %d records, want %d", withBody, RecentLimit)
	}
	alloc := int64(after.TotalAlloc - before.TotalAlloc)
	// The log is ~60 MB; the listed bodies need about RecentLimit*100 KB (a few MB) in total.
	if alloc > fi.Size()/4 {
		t.Errorf("ReadStats allocated %d bytes for a %d byte log; report bodies were materialized", alloc, fi.Size())
	}
	t.Logf("ReadStats: %d bytes allocated for a %d byte log", alloc, fi.Size())
}
