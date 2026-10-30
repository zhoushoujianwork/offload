package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/live"
)

type workloadDoc struct {
	Workload struct {
		Status           string `json:"status"`
		Processing       *int   `json:"processing"`
		Salvaging        *int   `json:"salvaging"`
		Stalled          *int   `json:"stalled"`
		Queued           *int   `json:"queued"`
		Cleaning         *int   `json:"cleaning"`
		Unresolved       *int   `json:"unresolved"`
		Lost             *int   `json:"lost"`
		UnreadableStates int    `json:"unreadable_states"`
		AdmissionLimit   *int   `json:"admission_limit"`
		AdmissionStatus  string `json:"admission_status"`
	} `json:"workload"`
	Goals struct {
		Processing int `json:"processing"`
		DailyCalls int `json:"daily_calls"`
	} `json:"goals"`
	Daily struct {
		Today struct {
			RecordedCalls *int   `json:"recorded_calls"`
			Peak          *int   `json:"peak_processing"`
			PeakCoverage  string `json:"peak_coverage"`
		} `json:"today"`
		Days    []struct{ Date string }
		Average struct {
			Calls       *float64 `json:"recorded_calls_per_day"`
			Coverage    string   `json:"coverage"`
			CoveredDays int      `json:"covered_days"`
			GoalMet     bool     `json:"goal_met"`
			WindowStart string   `json:"window_start"`
			WindowEnd   string   `json:"window_end"`
		} `json:"average"`
	} `json:"daily"`
	Active   []struct{ PID int }
	Totals   struct{ Runs int }
	Warnings []string
}

func statsDoc(t *testing.T, now time.Time) (workloadDoc, string) {
	t.Helper()
	code, out, _ := statsRun(t, now, "--json")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var d workloadDoc
	if err := json.Unmarshal([]byte(out), &d); err != nil {
		t.Fatal(err)
	}
	return d, out
}

func ival(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

func fresh(t *testing.T) time.Time {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("OFFLOAD_MAX_WORKERS", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // never read the developer's real config
	return time.Now()
}

func TestWorkloadClassifiesPhases(t *testing.T) {
	now := fresh(t)
	t.Cleanup(func(s, e time.Duration) func() { return func() { live.StaleAfter, live.ExpireAfter = s, e } }(live.StaleAfter, live.ExpireAfter))
	for _, s := range []live.State{
		{PID: 1, RunID: "a", Phase: live.Running, StartedAt: now, LastEventAt: now},
		{PID: 2, RunID: "b", Phase: live.Running, StartedAt: now, LastEventAt: now.Add(-5 * time.Minute)}, // stalled but live
		{PID: 3, RunID: "c", Phase: live.Salvaging, StartedAt: now, LastEventAt: now},
		{PID: 4, RunID: "d", Phase: live.Queued, StartedAt: now, LastEventAt: now},
		{PID: 5, RunID: "e", Phase: live.Queued, StartedAt: now, LastEventAt: now},
		{PID: 6, RunID: "f", Phase: live.Cleaning, StartedAt: now, LastEventAt: now},
		{PID: 7, RunID: "g", Phase: live.Unresolved, StartedAt: now, LastEventAt: now}, // leftover workers: neither processing nor cleaning
		{PID: 8, RunID: "h", Phase: live.Done, Status: "ok", StartedAt: now, LastEventAt: now},
	} {
		if err := live.Publish(s); err != nil {
			t.Fatal(err)
		}
	}
	d, _ := statsDoc(t, now)
	w := d.Workload
	if ival(w.Processing) != 3 || ival(w.Salvaging) != 1 || ival(w.Stalled) != 1 || ival(w.Queued) != 2 || ival(w.Cleaning) != 1 ||
		ival(w.Unresolved) != 1 || ival(w.Lost) != 0 || w.Status != "ok" {
		t.Fatalf("workload %+v", w)
	}
	if len(d.Active) != 6 {
		t.Errorf("active list keeps queued/cleaning entries: %d", len(d.Active))
	}
	if d.Goals.Processing != 100 || d.Goals.DailyCalls != 10000 {
		t.Errorf("goals %+v", d.Goals)
	}
}

func TestWorkloadLostAndUnresolvedAreSeparate(t *testing.T) {
	now := fresh(t)
	states := []live.State{
		{PID: 1, Phase: live.Lost, StartedAt: now},
		{PID: 2, Phase: live.Unresolved, StartedAt: now},
		{PID: 3, Phase: live.Running, Unresolved: true, StartedAt: now, LastEventAt: now}, // heartbeat stopped, group alive
		{PID: 4, Phase: live.Done, StartedAt: now},
	}
	w := classifyWorkload(states, 0, nil, now)
	if ival(w.Processing) != 0 || ival(w.Lost) != 1 || ival(w.Unresolved) != 2 || ival(w.Queued) != 0 {
		t.Fatalf("%+v", w)
	}
}

func TestWorkloadReadFailureIsUnavailableNotZero(t *testing.T) {
	now := fresh(t)
	// The live directory path is a regular file, so listing it fails.
	dir := live.Dir()
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	d, out := statsDoc(t, now)
	if d.Workload.Status != "unavailable" || d.Workload.Processing != nil || d.Workload.Queued != nil {
		t.Fatalf("workload %+v", d.Workload)
	}
	if !strings.Contains(out, `"processing":null`) {
		t.Errorf("expected null: %s", out)
	}
	_, text, _ := statsRun(t, now)
	if !strings.Contains(text, "processing now: unavailable / 100 goal") {
		t.Errorf("text:\n%s", text)
	}
}

func TestWorkloadUnreadableFileMakesCountsLowerBounds(t *testing.T) {
	now := fresh(t)
	live.Publish(live.State{PID: 1, Phase: live.Running, StartedAt: now, LastEventAt: now})
	os.WriteFile(filepath.Join(live.Dir(), "99.json"), []byte("{broken"), 0o600)
	d, _ := statsDoc(t, now)
	if d.Workload.Status != "partial" || d.Workload.UnreadableStates != 1 || ival(d.Workload.Processing) != 1 {
		t.Fatalf("%+v", d.Workload)
	}
}

func TestWorkloadLiveToCompletedIsNotDoubleCounted(t *testing.T) {
	now := fresh(t)
	// Keep both completed fixtures in today's window, including when the
	// suite runs during the first hour after midnight.
	now = time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location())
	live.Publish(live.State{PID: 1, RunID: "r", Phase: live.Running, StartedAt: now, LastEventAt: now})
	appendListRecord(t, history.Record{TS: now.Add(-time.Hour).Format("2006-01-02 15:04:05"), RunID: "old", Status: "ok"})
	d, _ := statsDoc(t, now)
	if ival(d.Workload.Processing) != 1 || d.Totals.Runs != 1 || ival(d.Daily.Today.RecordedCalls) != 1 {
		t.Fatalf("in flight run must not be a recorded call: %+v runs=%d", d.Workload, d.Totals.Runs)
	}
	// The run finishes: the live file says done and the run log gains its record.
	live.Publish(live.State{PID: 1, RunID: "r", Phase: live.Done, Status: "ok", StartedAt: now, LastEventAt: now})
	appendListRecord(t, history.Record{TS: now.Add(-time.Minute).Format("2006-01-02 15:04:05"), RunID: "r", Status: "ok"})
	d, _ = statsDoc(t, now)
	if ival(d.Workload.Processing) != 0 || d.Totals.Runs != 2 || ival(d.Daily.Today.RecordedCalls) != 2 {
		t.Fatalf("after completion: %+v runs=%d", d.Workload, d.Totals.Runs)
	}
}

func TestAdmissionLimitIsSeparateFromGoal(t *testing.T) {
	now := fresh(t)
	// Without a readable config the limit is unavailable, not invented.
	d, _ := statsDoc(t, now)
	if d.Workload.AdmissionStatus != "unavailable" || d.Workload.AdmissionLimit != nil {
		t.Fatalf("%+v", d.Workload)
	}
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(config.Path(), []byte(`{"profiles":{}}`), 0o600)
	t.Setenv("OFFLOAD_MAX_WORKERS", "3")
	d, _ = statsDoc(t, now)
	if d.Workload.AdmissionStatus != "limited" || ival(d.Workload.AdmissionLimit) != 3 || d.Goals.Processing != 100 {
		t.Fatalf("limit %+v goals %+v", d.Workload, d.Goals)
	}
	t.Setenv("OFFLOAD_MAX_WORKERS", "-1")
	d, _ = statsDoc(t, now)
	if d.Workload.AdmissionStatus != "unlimited" || d.Workload.AdmissionLimit != nil {
		t.Fatalf("%+v", d.Workload)
	}
}

func TestDailyPeakFromProcessingLogSurvivesExpiredLiveFiles(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := history.ProcessingPath()
	at := func(h, m int) time.Time { return time.Date(2026, 10, 29, h, m, 0, 0, time.UTC) }
	for _, id := range []string{"a", "b", "c"} {
		history.AppendProcessing(path, id, 1, history.ProcessingStart, at(9, 0))
	}
	history.AppendProcessing(path, "a", 1, history.ProcessingEnd, at(9, 1))
	history.AppendProcessing(path, "b", 1, history.ProcessingEnd, at(9, 2))
	history.AppendProcessing(path, "c", 1, history.ProcessingEnd, at(9, 3))
	// No live files remain, and statsRun was never polling at 09:01.
	appendListRecord(t, history.Record{TS: "2026-10-29 09:00:00", RunID: "a"})
	d, _ := statsDoc(t, now)
	// The log begins today, so earlier days are unavailable and today is a lower bound.
	if ival(d.Daily.Today.Peak) != 3 || d.Daily.Today.PeakCoverage != "partial" {
		t.Fatalf("today %+v", d.Daily.Today)
	}
}

func TestDailyAverageCoverageAndGoal(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// Empty log: unavailable, not zero.
	d, _ := statsDoc(t, now)
	if d.Daily.Average.Calls != nil || d.Daily.Average.Coverage != "unavailable" || d.Daily.Average.GoalMet || ival(d.Daily.Today.RecordedCalls) != 0 {
		t.Fatalf("empty: %+v", d.Daily)
	}
	if d.Daily.Today.Peak != nil || d.Daily.Today.PeakCoverage != "unavailable" {
		t.Fatalf("empty peak: %+v", d.Daily.Today)
	}
	if d.Daily.Average.WindowStart != "2026-10-22" || d.Daily.Average.WindowEnd != "2026-10-28" || len(d.Daily.Days) != 7 {
		t.Fatalf("window %+v", d.Daily.Average)
	}
	// A log that began three days ago: partial, even though the covered days exceed the goal.
	for i := 0; i < 10; i++ {
		appendListRecord(t, history.Record{TS: "2026-10-26 10:00:00"})
	}
	d, _ = statsDoc(t, now)
	if d.Daily.Average.Coverage != "partial" || d.Daily.Average.CoveredDays != 3 || d.Daily.Average.Calls == nil || d.Daily.Average.GoalMet {
		t.Fatalf("partial: %+v", d.Daily.Average)
	}
}

func TestDailyAverageCompleteMeetsGoalOnlyWhenComplete(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// Oct 21 has one record so Oct 22..28 are all covered; Oct 22..28 each hold 10,000 calls (Oct 28 one more).
	var lines []string
	add := func(ts string, n int) {
		for i := 0; i < n; i++ {
			b, _ := json.Marshal(history.Record{TS: ts, Status: "ok", RunID: ts})
			lines = append(lines, string(b))
		}
	}
	add("2026-10-21 12:00:00", 1)
	for d := 22; d <= 28; d++ {
		add("2026-10-"+string(rune('0'+d/10))+string(rune('0'+d%10))+" 12:00:00", 10000)
	}
	add("2026-10-29 01:00:00", 20000) // partial today must not count toward the average
	os.MkdirAll(filepath.Dir(history.Path()), 0o700)
	os.WriteFile(history.Path(), []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	start := time.Now()
	d, _ := statsDoc(t, now)
	t.Logf("stats --json over %d records: %v", len(lines), time.Since(start))
	a := d.Daily.Average
	if a.Coverage != "complete" || a.Calls == nil || *a.Calls != 10000 || !a.GoalMet || ival(d.Daily.Today.RecordedCalls) != 20000 {
		t.Fatalf("%+v today=%+v", a, d.Daily.Today)
	}
	_, text, _ := statsRun(t, now)
	for _, want := range []string{"recorded calls today: 20000 / 10000 goal", "7-day average: 10000.0 recorded calls/day (2026-10-22 to 2026-10-28, 7 of 7 days covered, complete", "peak processing today: unavailable"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q:\n%s", want, text)
		}
	}
}

func TestSyntheticHundredProcessingAndStatsStayCompatible(t *testing.T) {
	now := fresh(t)
	for i := 1; i <= 100; i++ {
		live.Publish(live.State{PID: i, RunID: "r", Phase: live.Running, StartedAt: now, LastEventAt: now})
	}
	live.Publish(live.State{PID: 200, Phase: live.Queued, StartedAt: now, LastEventAt: now})
	start := time.Now()
	d, out := statsDoc(t, now)
	t.Logf("100 live states: %v", time.Since(start))
	if ival(d.Workload.Processing) != 100 || ival(d.Workload.Queued) != 1 {
		t.Fatalf("%+v", d.Workload)
	}
	var m map[string]json.RawMessage
	json.Unmarshal([]byte(out), &m)
	for _, k := range []string{"schema_version", "generated_at", "period", "totals", "trend", "projects", "models", "active", "recent", "warnings"} {
		if _, ok := m[k]; !ok {
			t.Errorf("existing field %s missing", k)
		}
	}
	if string(m["schema_version"]) != "1" {
		t.Errorf("schema_version must stay 1: %s", m["schema_version"])
	}
}

// A damaged run-log line may have been a call on any day: the window is partial, never seven known zeros.
func TestTruncatedRunLogIsPartialNotKnownZeroDays(t *testing.T) {
	now := fresh(t)
	appendListRecord(t, history.Record{TS: now.AddDate(0, 0, -8).Format("2006-01-02 15:04:05"), RunID: "old", Status: "ok"})
	f, err := os.OpenFile(history.Path(), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{\"ts\":\"" + now.AddDate(0, 0, -3).Format("2006-01-02 15:04:05") + "\",\"run_i\n")
	f.Close()
	d, out := statsDoc(t, now)
	if d.Daily.Average.Coverage != "partial" || d.Daily.Average.GoalMet {
		t.Fatalf("truncated log must be partial: %+v", d.Daily.Average)
	}
	if !strings.Contains(out, "damaged") {
		t.Errorf("expected a damaged-line warning: %s", out)
	}
}

// An unfinished span from yesterday is an unknown end, so today's peak is a lower bound, not an exact zero.
func TestCrashAcrossMidnightIsNotExactZeroToday(t *testing.T) {
	t.Setenv("TZ", "UTC")
	now := fresh(t)
	now = time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, now.Location())
	prev := time.Date(now.Year(), now.Month(), now.Day()-1, 23, 59, 0, 0, now.Location())
	if err := history.AppendProcessing(history.ProcessingPath(), "crashed", 4242, history.ProcessingStart, prev); err != nil {
		t.Fatal(err)
	}
	d, _ := statsDoc(t, now)
	if d.Daily.Today.PeakCoverage != "partial" || ival(d.Daily.Today.Peak) != 0 {
		t.Fatalf("today after a crash that began yesterday: %+v", d.Daily.Today)
	}
}
