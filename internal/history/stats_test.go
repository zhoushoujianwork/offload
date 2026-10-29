package history

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func fp(v float64) *float64 { return &v }
func ip(v int64) *int64     { return &v }

func at(t *testing.T, loc *time.Location, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04:05", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAggregateUsageCostAndCoverage(t *testing.T) {
	loc := time.FixedZone("X", 3600)
	now := at(t, loc, "2026-10-29 12:00:00")
	rs := []Record{
		// Two runs of one Codex session: Usage is per run; CumulativeUsage must never be summed.
		{TS: "2026-10-29 09:00:00", Status: "ok", Session: "s1", Dir: "/p/a", Backend: "codex", Model: "m", ElapsedMs: ip(1000),
			Usage: &TokenUsage{Input: 100, Cached: 40, CacheWrite: 10, Output: 5}, UsageScope: "run",
			CumulativeUsage: &TokenUsage{Input: 100, Cached: 40, CacheWrite: 10, Output: 5}, CostUSD: fp(0.5)},
		{TS: "2026-10-29 10:00:00", Status: "ok", Session: "s1", Dir: "/p/a", Backend: "codex", Model: "m", ElapsedMs: ip(2000),
			Usage: &TokenUsage{Input: 50, Cached: 20, Output: 7}, UsageScope: "run",
			CumulativeUsage: &TokenUsage{Input: 150, Cached: 60, CacheWrite: 10, Output: 12}, CostUSD: fp(0.25)},
		// A real zero cost on a Codex run is a measured zero, not missing.
		{TS: "2026-10-29 11:00:00", Status: "error", Session: "s2", Dir: "/p/b", Backend: "codex", Model: "c", CostUSD: fp(0)},
		// Measured zero usage stays a real (zero) sum, distinct from none.
		{TS: "2026-10-29 11:30:00", Status: "timeout", Session: "", Dir: "/p/b", Backend: "codex", Model: "c", Usage: &TokenUsage{}},
		// Invalid usage and non-run scope are ignored.
		{TS: "2026-10-29 11:31:00", Status: "ok", Session: "s3", Dir: "/p/b", Backend: "codex", Model: "c",
			Usage: &TokenUsage{Input: 1, Cached: 5}},
		{TS: "2026-10-29 11:32:00", Status: "ok", Session: "s3", Dir: "/p/b", Backend: "codex", Model: "c",
			Usage: &TokenUsage{Input: 9}, UsageScope: "session"},
	}
	st, err := Aggregate(rs, PeriodToday, now)
	if err != nil {
		t.Fatal(err)
	}
	tot := st.Totals
	if tot.Runs != 6 || tot.Sessions != 3 || tot.OK != 4 || tot.Error != 1 || tot.Timeout != 1 {
		t.Fatalf("counts: %+v", tot)
	}
	if *tot.Usage != (TokenUsage{Input: 150, Cached: 60, CacheWrite: 10, Output: 12}) || tot.UsageRuns != 3 {
		t.Fatalf("usage: %+v runs=%d", *tot.Usage, tot.UsageRuns)
	}
	if tot.CostUSD == nil || *tot.CostUSD != 0.75 || tot.CostRuns != 3 {
		t.Fatalf("cost: %v runs=%d", tot.CostUSD, tot.CostRuns)
	}
	if tot.ElapsedMs != 3000 || tot.ElapsedRuns != 2 {
		t.Fatalf("elapsed: %d/%d", tot.ElapsedMs, tot.ElapsedRuns)
	}
	if len(st.Projects) != 2 || st.Projects[0].Key != "/p/b" || st.Projects[0].Totals.Runs != 4 || st.Projects[0].Name != "b" {
		t.Fatalf("projects: %+v", st.Projects)
	}
	b := st.Projects[0].Totals
	if b.Usage == nil || *b.Usage != (TokenUsage{}) || b.UsageRuns != 1 {
		t.Fatalf("measured zero usage: %+v", b.Usage)
	}
	if len(st.Models) != 2 || st.Models[0].Key != "codex/c" || st.Models[1].Key != "codex/m" || st.Models[1].Name != "codex m" {
		t.Fatalf("models: %+v", st.Models)
	}
	joined := strings.Join(st.Warnings, "\n")
	for _, want := range []string{"usage is recorded for 3 of 6", "cost is recorded for 3 of 6", "not billing"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings missing %q:\n%s", want, joined)
		}
	}
}

func TestLegacyClaudeCostsExcluded(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	rs := []Record{
		// Overlapping session totals from before cumulative snapshots: 0.10, then 0.25 including it.
		{TS: "2026-10-29 09:00:00", Status: "ok", Session: "s", Backend: "claude", CostUSD: fp(0.10)},
		{TS: "2026-10-29 10:00:00", Status: "ok", Session: "s", Backend: "claude", CostUSD: fp(0.25)},
		// Records that predate the backend field are legacy as well.
		{TS: "2026-10-29 10:30:00", Status: "ok", Session: "old", CostUSD: fp(0.40)},
		// A new run with a validated per-run cost and its session snapshot is accepted.
		{TS: "2026-10-29 11:00:00", Status: "ok", Session: "s", Backend: "claude", CostUSD: fp(0.15), CumulativeCostUSD: fp(0.40)},
		{TS: "2026-10-29 11:30:00", Status: "ok", Session: "n", Backend: "claude"},
	}
	st, _ := Aggregate(rs, PeriodToday, now)
	tot := st.Totals
	if tot.CostUSD == nil || *tot.CostUSD != 0.15 || tot.CostRuns != 1 || tot.Runs != 5 || st.LegacyCostRuns != 3 {
		t.Fatalf("cost=%v runs=%d legacy=%d", tot.CostUSD, tot.CostRuns, st.LegacyCostRuns)
	}
	for _, r := range st.Recent {
		legacy := r.TS < "2026-10-29 11:00:00"
		if legacy && r.CostUSD != nil {
			t.Errorf("legacy cost in recent: %+v", r)
		}
		if r.TS == "2026-10-29 11:00:00" && (r.CostUSD == nil || *r.CostUSD != 0.15) {
			t.Errorf("snapshot cost missing in recent: %+v", r)
		}
	}
	joined := strings.Join(st.Warnings, "\n")
	for _, want := range []string{"3 Claude run(s) recorded before cumulative cost snapshots", "overlaps other runs", "cost is recorded for 1 of 5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "Claude runs record none") {
		t.Errorf("stale warning: %s", joined)
	}
	// An invalid cost is never accepted, even with a snapshot.
	if runCost(Record{Backend: "codex", CostUSD: fp(-1)}) != nil || runCost(Record{Backend: "claude", CostUSD: fp(1), CumulativeCostUSD: fp(1)}) == nil {
		t.Error("runCost validity")
	}
}

func TestAggregateMissingStaysNull(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	st, err := Aggregate([]Record{{TS: "2026-10-29 01:00:00", Status: "ok"}}, PeriodAll, now)
	if err != nil {
		t.Fatal(err)
	}
	if st.Totals.Usage != nil || st.Totals.CostUSD != nil || st.Totals.ElapsedRuns != 0 || st.Totals.Sessions != 0 {
		t.Fatalf("%+v", st.Totals)
	}
	b, _ := json.Marshal(st.Totals)
	if !strings.Contains(string(b), `"usage":null`) || !strings.Contains(string(b), `"cost_usd":null`) {
		t.Fatalf("json: %s", b)
	}
}

func TestAggregateExcludesSalvage(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	rs := []Record{
		{TS: "2026-10-29 09:00:00", Status: "error", Salvaged: true, Session: "s", Backend: "codex", CostUSD: fp(0.3), Result: "report"},
		{TS: "2026-10-29 09:30:00", Status: "timeout", Salvaged: true, Session: "s", Usage: &TokenUsage{Input: 3}},
		{TS: "2026-10-29 10:00:00", Status: "ok", Session: "s", Backend: "codex", CostUSD: fp(0.1)},
	}
	st, _ := Aggregate(rs, PeriodToday, now)
	tot := st.Totals
	if tot.Salvaged != 2 || tot.ExcludedSalvageCostRuns != 2 || tot.CostUSD == nil || *tot.CostUSD != 0.1 || tot.CostRuns != 1 || tot.Usage != nil {
		t.Fatalf("%+v", tot)
	}
	if !strings.Contains(strings.Join(st.Warnings, "\n"), "2 salvaged run(s)") {
		t.Errorf("warnings: %v", st.Warnings)
	}
	// The recent list shows no cost or usage for salvaged runs, but keeps the flag and report.
	var salvaged int
	for _, r := range st.Recent {
		if !r.Salvaged {
			continue
		}
		salvaged++
		if r.CostUSD != nil || r.Usage != nil {
			t.Errorf("salvaged recent run shows accounting: %+v", r)
		}
	}
	if salvaged != 2 {
		t.Errorf("recent salvaged runs = %d: %+v", salvaged, st.Recent)
	}
	for _, r := range st.Recent {
		if r.Result == "report" && !r.Salvaged {
			t.Errorf("report lost its salvaged flag: %+v", r)
		}
	}
	if runCost(Record{Salvaged: true, Backend: "codex", CostUSD: fp(1)}) != nil || runUsage(Record{Salvaged: true, Usage: &TokenUsage{Input: 1}}) != nil {
		t.Error("salvaged accounting accepted")
	}
	if w := strings.Join(st.Warnings, "\n"); !strings.Contains(w, "may contain session totals") {
		t.Errorf("warnings: %s", w)
	}
}

func TestPeriodBoundariesAndTimezone(t *testing.T) {
	loc := time.FixedZone("UTC+8", 8*3600)
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, loc) // Thursday
	cases := []struct{ period, start string }{
		{PeriodToday, "2026-10-29 00:00:00"},
		{PeriodWeek, "2026-10-26 00:00:00"}, // Monday
		{PeriodMonth, "2026-10-01 00:00:00"},
	}
	for _, c := range cases {
		got, err := PeriodStart(c.period, now)
		if err != nil || !got.Equal(at(t, loc, c.start)) {
			t.Errorf("%s: %v %v", c.period, got, err)
		}
	}
	if s, _ := PeriodStart(PeriodWeek, time.Date(2026, 11, 1, 5, 0, 0, 0, loc)); !s.Equal(at(t, loc, "2026-10-26 00:00:00")) { // Sunday
		t.Errorf("sunday week start %v", s)
	}
	rs := []Record{
		{TS: "2026-10-28 23:59:59", Status: "ok", Session: "yesterday"},
		{TS: "2026-10-29 00:00:00", Status: "ok", Session: "midnight"},
		// RFC3339 in UTC is 2026-10-29 07:30 +08:00 local, so it belongs to today.
		{TS: "2026-10-28T23:30:00Z", Status: "ok", Session: "rfc"},
		// Local 2026-10-29 02:00 is 2026-10-28T18:00Z: today locally only.
		{TS: "2026-10-28T18:00:00Z", Status: "ok", Session: "rfc-early"},
		{TS: "2026-10-29 12:00:01", Status: "ok", Session: "future"},
		{TS: "garbage", Status: "ok", Session: "bad"},
		{TS: "", Status: "ok"},
	}
	st, _ := Aggregate(rs, PeriodToday, now)
	if st.Totals.Runs != 3 || st.Totals.Sessions != 3 {
		t.Fatalf("today: %+v", st.Totals)
	}
	if !strings.Contains(strings.Join(st.Warnings, "\n"), "skipped 3 record(s)") {
		t.Errorf("warnings: %v", st.Warnings)
	}
	if st, _ := Aggregate(rs, PeriodAll, now); st.Totals.Runs != 4 {
		t.Errorf("all: %+v", st.Totals)
	}
	if st, _ := Aggregate(rs, PeriodWeek, now); st.Totals.Runs != 4 {
		t.Errorf("week: %+v", st.Totals)
	}
	// The same wall-clock record falls on different days in different zones.
	utc := now.In(time.UTC)
	if st, _ := Aggregate([]Record{{TS: "2026-10-29 03:00:00"}}, PeriodToday, utc); st.Totals.Runs != 1 {
		t.Errorf("utc: %+v", st.Totals)
	}
	if _, err := Aggregate(nil, "year", now); err == nil {
		t.Error("invalid period accepted")
	}
}

func TestRecentOrderLimitAndTruncation(t *testing.T) {
	now := time.Date(2026, 10, 29, 23, 0, 0, 0, time.UTC)
	var rs []Record
	for i := 0; i < 25; i++ {
		rs = append(rs, Record{TS: time.Date(2026, 10, 29, 1, i, 0, 0, time.UTC).Format("2006-01-02 15:04:05"), Status: "ok", Task: "t"})
	}
	// Same timestamp: the later log line comes first.
	rs = append(rs, Record{TS: "2026-10-29 01:24:00", Status: "ok", Task: "later-line", Result: strings.Repeat("é", 5000)})
	st, _ := Aggregate(rs, PeriodToday, now)
	if len(st.Recent) != RecentLimit || st.Recent[0].Task != "later-line" || st.Recent[1].TS != "2026-10-29 01:24:00" || st.Recent[2].TS != "2026-10-29 01:23:00" {
		t.Fatalf("recent: %d %+v", len(st.Recent), st.Recent[:3])
	}
	res := []rune(st.Recent[0].Result)
	if len(res) != RecentResultRunes || !strings.HasSuffix(st.Recent[0].Result, truncationMarker) {
		t.Fatalf("result runes=%d", len(res))
	}
	empty, _ := Aggregate(nil, PeriodAll, now)
	b, _ := json.Marshal(empty.Recent)
	if string(b) != "[]" || empty.Projects == nil || empty.Models == nil || empty.Warnings == nil {
		t.Fatalf("nil slices: %s", b)
	}
}

func TestGroupSortDeterministic(t *testing.T) {
	now := time.Date(2026, 10, 29, 23, 0, 0, 0, time.UTC)
	rs := []Record{
		{TS: "2026-10-29 01:00:00", Dir: "/b"}, {TS: "2026-10-29 01:00:00", Dir: "/a"},
		{TS: "2026-10-29 01:00:00", Dir: "/c"}, {TS: "2026-10-29 01:00:00", Dir: "/c"}, {TS: "2026-10-29 01:00:00"},
	}
	st, _ := Aggregate(rs, PeriodAll, now)
	var keys []string
	for _, g := range st.Projects {
		keys = append(keys, g.Key)
	}
	if strings.Join(keys, ",") != "/c,,/a,/b" || st.Projects[1].Name != "unknown project" {
		t.Fatalf("%q %+v", keys, st.Projects)
	}
	if st.Models[0].Name != "unknown model" {
		t.Fatalf("%+v", st.Models)
	}
}

func pointRuns(tr Trend) []int {
	out := make([]int, len(tr.Points))
	for i, p := range tr.Points {
		out[i] = p.Totals.Runs
	}
	return out
}

func TestTrendTodayHourlyGapsAndCoverage(t *testing.T) {
	loc := time.FixedZone("X", 3600)
	now := at(t, loc, "2026-10-29 04:30:00")
	rs := []Record{
		{TS: "2026-10-29 00:10:00", Status: "ok", Session: "s", Backend: "codex", Usage: &TokenUsage{Input: 10, Cached: 4, Output: 1},
			UsageScope: "run", CostUSD: fp(0.5), CumulativeUsage: &TokenUsage{Input: 10, Cached: 4, Output: 1}},
		// Same session resumed in a later bin: per-run usage only, cumulative never added.
		{TS: "2026-10-29 02:05:00", Status: "ok", Session: "s", Backend: "codex", Usage: &TokenUsage{Input: 5, Output: 2},
			UsageScope: "run", CostUSD: fp(0.25), CumulativeUsage: &TokenUsage{Input: 15, Cached: 4, Output: 3}},
		{TS: "2026-10-29 02:40:00", Status: "ok", Session: "s", Backend: "codex"},
		// Legacy Claude cost and salvage accounting are excluded from the buckets too.
		{TS: "2026-10-29 04:00:00", Status: "ok", Session: "c", Backend: "claude", CostUSD: fp(9)},
		{TS: "2026-10-29 04:10:00", Status: "error", Salvaged: true, Session: "c", Backend: "claude", CostUSD: fp(9), CumulativeCostUSD: fp(9), Usage: &TokenUsage{Input: 7}},
		// Before today and in the future: outside the trend.
		{TS: "2026-10-28 23:59:59", Status: "ok", Session: "old"},
		{TS: "2026-10-29 05:00:00", Status: "ok", Session: "future"},
	}
	st, _ := Aggregate(rs, PeriodToday, now)
	tr := st.Trend
	if tr.Interval != "hour" || tr.Label != "Today" || len(tr.Points) != 5 {
		t.Fatalf("trend: %+v", tr)
	}
	if got := pointRuns(tr); !equalInts(got, []int{1, 0, 2, 0, 2}) {
		t.Fatalf("runs per hour: %v", got)
	}
	var labels []string
	for _, p := range tr.Points {
		labels = append(labels, p.Label)
	}
	if strings.Join(labels, ",") != "00:00,01:00,02:00,03:00,04:00" || tr.Points[0].Start != "2026-10-29T00:00:00+01:00" {
		t.Fatalf("labels %v start %s", labels, tr.Points[0].Start)
	}
	p0, p1, p2, p4 := tr.Points[0].Totals, tr.Points[1].Totals, tr.Points[2].Totals, tr.Points[4].Totals
	if *p0.Usage != (TokenUsage{Input: 10, Cached: 4, Output: 1}) || p0.UsageRuns != 1 || *p0.CostUSD != 0.5 {
		t.Errorf("hour 0: %+v", p0)
	}
	// An empty bucket has real zero runs and null usage and cost, never invented zeros.
	if p1.Runs != 0 || p1.Usage != nil || p1.CostUSD != nil || p1.UsageRuns != 0 || p1.CostRuns != 0 || p1.Sessions != 0 {
		t.Errorf("empty hour: %+v", p1)
	}
	if *p2.Usage != (TokenUsage{Input: 5, Output: 2}) || p2.UsageRuns != 1 || *p2.CostUSD != 0.25 || p2.CostRuns != 1 || p2.Runs != 2 || p2.Sessions != 1 {
		t.Errorf("hour 2: %+v", p2)
	}
	if p4.Usage != nil || p4.CostUSD != nil || p4.Runs != 2 || p4.Salvaged != 1 || p4.ExcludedSalvageCostRuns != 1 || p4.Sessions != 1 {
		t.Errorf("hour 4: %+v", p4)
	}
	// Overall totals do not depend on the trend and keep the same run count.
	if st.Totals.Runs != 5 || st.Totals.Sessions != 2 {
		t.Errorf("totals: %+v", st.Totals)
	}
	var sum int
	for _, n := range pointRuns(tr) {
		sum += n
	}
	if sum != st.Totals.Runs {
		t.Errorf("bucket runs %d != total %d", sum, st.Totals.Runs)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTrendWeekMonthAllWindows(t *testing.T) {
	loc := time.FixedZone("X", 3600)
	now := at(t, loc, "2026-10-29 12:00:00") // Thursday
	rs := []Record{
		{TS: "2026-09-30 10:00:00", Status: "ok", Session: "a"}, // 29 days before: in the all trend
		{TS: "2026-09-29 23:59:59", Status: "ok", Session: "b"}, // 30 days before: outside the all trend
		{TS: "2026-10-01 09:00:00", Status: "ok", Session: "c"},
		{TS: "2026-10-25 23:00:00", Status: "ok", Session: "d"}, // Sunday before the week
		{TS: "2026-10-26 00:00:00", Status: "ok", Session: "e"},
		{TS: "2026-10-29 11:00:00", Status: "ok", Session: "f"},
	}
	week, _ := Aggregate(rs, PeriodWeek, now)
	if week.Trend.Interval != "day" || week.Trend.Label != "This week" || len(week.Trend.Points) != 4 ||
		week.Trend.Points[0].Label != "Oct 26" || week.Trend.Points[3].Label != "Oct 29" || !equalInts(pointRuns(week.Trend), []int{1, 0, 0, 1}) {
		t.Errorf("week: %+v", week.Trend)
	}
	month, _ := Aggregate(rs, PeriodMonth, now)
	if len(month.Trend.Points) != 29 || month.Trend.Label != "This month" || month.Trend.Points[0].Label != "Oct 1" || month.Trend.Points[28].Label != "Oct 29" ||
		month.Trend.Points[0].Totals.Runs != 1 || month.Trend.Points[24].Totals.Runs != 1 {
		t.Errorf("month: %d points, runs %v", len(month.Trend.Points), pointRuns(month.Trend))
	}
	all, _ := Aggregate(rs, PeriodAll, now)
	tr := all.Trend
	if tr.Interval != "day" || tr.Label != "Last 30 days" || len(tr.Points) != 30 || tr.Points[0].Label != "Sep 30" || tr.Points[29].Label != "Oct 29" {
		t.Fatalf("all: %d points %s..%s", len(tr.Points), tr.Points[0].Label, tr.Points[len(tr.Points)-1].Label)
	}
	var sum int
	for _, n := range pointRuns(tr) {
		sum += n
	}
	if sum != 5 || all.Totals.Runs != 6 || all.Totals.Sessions != 6 {
		t.Errorf("all trend runs %d, overall %+v", sum, all.Totals)
	}
	// Monday itself: the week is one bin; the first of the month: the month is one bin.
	mon := at(t, loc, "2026-10-26 08:00:00")
	if w, _ := Aggregate(nil, PeriodWeek, mon); len(w.Trend.Points) != 1 {
		t.Errorf("monday week: %d", len(w.Trend.Points))
	}
	first := at(t, loc, "2026-11-01 00:00:00")
	if m, _ := Aggregate(nil, PeriodMonth, first); len(m.Trend.Points) != 1 || m.Trend.Points[0].Start != "2026-11-01T00:00:00+01:00" {
		t.Errorf("first of month: %+v", m.Trend.Points)
	}
	// Empty history still yields the full set of empty, null-valued buckets and a non-nil slice.
	empty, _ := Aggregate(nil, PeriodAll, now)
	b, _ := json.Marshal(empty.Trend.Points[3])
	if len(empty.Trend.Points) != 30 || !strings.Contains(string(b), `"usage":null`) || !strings.Contains(string(b), `"cost_usd":null`) || !strings.Contains(string(b), `"runs":0`) {
		t.Errorf("empty: %d %s", len(empty.Trend.Points), b)
	}
}

func TestTrendDSTRepeatedHourAndDayBounds(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database: ", err)
	}
	// 2026-11-01: clocks go back at 02:00 EDT, so local 01:00-02:00 happens twice.
	now := time.Date(2026, 11, 1, 3, 0, 0, 0, loc)
	rs := []Record{
		{TS: "2026-11-01T05:30:00Z", Status: "ok", Session: "a"}, // 01:30 EDT
		{TS: "2026-11-01T06:30:00Z", Status: "ok", Session: "b"}, // 01:30 EST
		{TS: "2026-11-01T06:45:00Z", Status: "ok", Session: "b"},
	}
	st, _ := Aggregate(rs, PeriodToday, now)
	tr := st.Trend
	var labels []string
	for _, p := range tr.Points {
		labels = append(labels, p.Label)
	}
	if got := strings.Join(labels, ","); got != "00:00,01:00 -0400,01:00 -0500,02:00,03:00" {
		t.Fatalf("labels: %s", got)
	}
	if !equalInts(pointRuns(tr), []int{0, 1, 2, 0, 0}) || tr.Points[2].Totals.Sessions != 1 {
		t.Errorf("runs: %v", pointRuns(tr))
	}
	if tr.Points[1].Start != "2026-11-01T01:00:00-04:00" || tr.Points[2].Start != "2026-11-01T01:00:00-05:00" {
		t.Errorf("starts: %s %s", tr.Points[1].Start, tr.Points[2].Start)
	}

	// A month that crosses the change: daily bins keep local midnights (Nov 1 00:00 is still EDT,
	// Nov 2 00:00 is EST) and a record at the last second of a day stays in that day.
	now = time.Date(2026, 11, 3, 9, 0, 0, 0, loc)
	rs = []Record{
		{TS: "2026-11-01T03:59:59Z", Status: "ok", Session: "x"}, // Oct 31 23:59:59 EDT
		{TS: "2026-11-01T04:00:00Z", Status: "ok", Session: "y"}, // Nov 1 00:00 EDT
		{TS: "2026-11-02T04:59:59Z", Status: "ok", Session: "z"}, // Nov 1 23:59:59 EST
		{TS: "2026-11-02T05:00:00Z", Status: "ok", Session: "w"}, // Nov 2 00:00 EST
	}
	mo, _ := Aggregate(rs, PeriodMonth, now)
	p := mo.Trend.Points
	if len(p) != 3 || p[0].Start != "2026-11-01T00:00:00-04:00" || p[1].Start != "2026-11-02T00:00:00-05:00" || p[2].Start != "2026-11-03T00:00:00-05:00" {
		t.Fatalf("month points: %d %+v", len(p), p)
	}
	// Nov 1 is 25 hours long: it holds the record at 00:00 EDT and the one at 23:59:59 EST.
	if !equalInts(pointRuns(mo.Trend), []int{2, 1, 0}) || mo.Totals.Runs != 3 {
		t.Errorf("month runs: %v total %d", pointRuns(mo.Trend), mo.Totals.Runs)
	}
}
