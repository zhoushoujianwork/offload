package history

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Stats periods accepted by Aggregate.
const (
	PeriodToday = "today"
	PeriodWeek  = "week"
	PeriodMonth = "month"
	PeriodAll   = "all"
)

const (
	// RecentLimit is the most runs listed in Stats.Recent.
	RecentLimit = 20
	// RecentResultRunes bounds a recent run's result, including the truncation marker.
	RecentResultRunes = 4000

	truncationMarker = "…[truncated]"
	recordTimeLayout = "2006-01-02 15:04:05" // layout of Record.TS, written in local time
)

// StatsTotals sums the recorded accounting of the runs it covers. Usage and CostUSD are nil when
// no covered run recorded them, so missing is distinct from a measured zero. UsageRuns, CostRuns
// and ElapsedRuns say how many of Runs the sums actually cover.
type StatsTotals struct {
	Runs                    int         `json:"runs"`
	Sessions                int         `json:"sessions"`
	OK                      int         `json:"ok"`
	Error                   int         `json:"error"`
	Timeout                 int         `json:"timeout"`
	Salvaged                int         `json:"salvaged"`
	Canceled                int         `json:"canceled"`
	Interrupted             int         `json:"interrupted"`
	Killed                  int         `json:"killed"`
	ElapsedMs               int64       `json:"elapsed_ms"`
	ElapsedRuns             int         `json:"elapsed_runs"`
	Usage                   *TokenUsage `json:"usage"`
	UsageRuns               int         `json:"usage_runs"`
	CostUSD                 *float64    `json:"cost_usd"`
	CostRuns                int         `json:"cost_runs"`
	ExcludedSalvageCostRuns int         `json:"excluded_salvage_cost_runs"`
	// ToolRestrictionNotEnforced counts runs that explicitly ignored unsupported restrictions.
	ToolRestrictionNotEnforced int `json:"tool_restriction_not_enforced"`
}

// StatsGroup is the totals of one project or model.
type StatsGroup struct {
	Key    string      `json:"key"`
	Name   string      `json:"name"`
	Totals StatsTotals `json:"totals"`
}

// RecentRun is one finished run with its accounting. A salvaged run's CostUSD and Usage are always
// nil here: its recorded accounting is incomplete and may hold session totals, so it is not a
// reliable full-run figure.
type RecentRun struct {
	TS       string      `json:"ts"`
	Status   string      `json:"status"`
	Dir      string      `json:"dir"`
	Task     string      `json:"task"`
	Session  string      `json:"session"`
	Model    string      `json:"model"`
	CostUSD  *float64    `json:"cost_usd"`
	Usage    *TokenUsage `json:"usage"`
	Result   string      `json:"result"`
	Salvaged bool        `json:"salvaged"`
	// TerminationClass is canceled, interrupted or killed when the worker ended without a result.
	TerminationClass string `json:"termination_class,omitempty"`
	// ToolRestriction is "not_enforced" for a run that ignored its requested tool restriction.
	ToolRestriction string `json:"tool_restriction,omitempty"`
}

// Trend interval names.
const (
	IntervalHour = "hour"
	IntervalDay  = "day"
)

// trendAllDays is the number of calendar days the trend of the "all" period covers.
const trendAllDays = 30

// TrendPoint is the totals of the completed runs that started in one bucket. A bucket without
// runs has Runs 0 and null usage and cost.
type TrendPoint struct {
	Start  string      `json:"start"` // RFC3339 in the local zone
	Label  string      `json:"label"`
	Totals StatsTotals `json:"totals"`
}

// Trend splits a window of the period into hourly or daily buckets, oldest first, from the
// window start up to and including the bucket that contains now. Each bucket ends at the next
// bucket's start, or at now for the last one. The window is the period itself, except for "all",
// which charts only the last 30 calendar days while the overall totals keep the whole history.
type Trend struct {
	Interval string       `json:"interval"`
	Label    string       `json:"label"`
	Points   []TrendPoint `json:"points"`
}

// Stats is the aggregate of the completed runs in one period.
type Stats struct {
	Period   string
	Totals   StatsTotals
	Trend    Trend
	Projects []StatsGroup
	Models   []StatsGroup
	Recent   []RecentRun
	Warnings []string
	// LegacyCostRuns counts runs whose recorded cost was left out of every total because it may
	// be a session-cumulative figure (see runCost).
	LegacyCostRuns int
}

// ValidPeriod reports whether p is a period Aggregate accepts.
func ValidPeriod(p string) bool {
	switch p {
	case PeriodToday, PeriodWeek, PeriodMonth, PeriodAll:
		return true
	}
	return false
}

// PeriodStart is the first instant of the period in now's location: the calendar day, the week
// starting Monday, or the calendar month. For "all" it is the zero time.
func PeriodStart(period string, now time.Time) (time.Time, error) {
	y, m, d := now.Date()
	switch period {
	case PeriodToday:
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()), nil
	case PeriodWeek:
		return time.Date(y, m, d-(int(now.Weekday())+6)%7, 0, 0, 0, 0, now.Location()), nil
	case PeriodMonth:
		return time.Date(y, m, 1, 0, 0, 0, 0, now.Location()), nil
	case PeriodAll:
		return time.Time{}, nil
	}
	return time.Time{}, fmt.Errorf("unknown period %q; use today, week, month or all", period)
}

// ParseTS reads a record timestamp: the local "2006-01-02 15:04:05" layout (interpreted in loc)
// or RFC3339.
func ParseTS(ts string, loc *time.Location) (time.Time, bool) {
	if t, err := time.ParseInLocation(recordTimeLayout, ts, loc); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		return t.In(loc), true
	}
	return time.Time{}, false
}

type statsAcc struct {
	t        StatsTotals
	sessions map[string]bool
}

func (a *statsAcc) add(r Record, elapsedOK bool) {
	t := &a.t
	t.Runs++
	if r.Session != "" {
		if a.sessions == nil {
			a.sessions = map[string]bool{}
		}
		a.sessions[r.Session] = true
	}
	switch r.Status {
	case "ok":
		t.OK++
	case "error":
		t.Error++
	case "timeout":
		t.Timeout++
	}
	switch r.TerminationClass() {
	case ClassCanceled:
		t.Canceled++
	case ClassInterrupted:
		t.Interrupted++
	case ClassKilled:
		t.Killed++
	}
	if elapsedOK {
		t.ElapsedMs += *r.ElapsedMs
		t.ElapsedRuns++
	}
	if r.ToolRestriction == ToolRestrictionNotEnforced {
		t.ToolRestrictionNotEnforced++
	}
	if r.Salvaged {
		// Salvaged accounting is incomplete and may contain session totals (Claude reports a
		// session-cumulative cost even for the report step), so it is not a reliable full-run
		// figure. The raw record stays untouched; it is only left out of the aggregates.
		t.Salvaged++
		if r.CostUSD != nil || r.Usage != nil {
			t.ExcludedSalvageCostRuns++
		}
		return
	}
	if u := runUsage(r); u != nil {
		if t.Usage == nil {
			t.Usage = &TokenUsage{}
		}
		t.Usage.Input += u.Input
		t.Usage.Cached += u.Cached
		t.Usage.CacheWrite += u.CacheWrite
		t.Usage.Output += u.Output
		t.UsageRuns++
	}
	if c := runCost(r); c != nil {
		if t.CostUSD == nil {
			t.CostUSD = new(float64)
		}
		*t.CostUSD += *c
		t.CostRuns++
	}
}

// runCost returns the cost of this run alone, or nil when it cannot be trusted. Claude's
// total_cost_usd is a session-cumulative figure, so a Claude record from before the accounting
// recorded CumulativeCostUSD may hold an overlapping session total, and summing such records
// double counts. A salvaged run's cost is never accepted: it is incomplete and may be a session
// total. A cost is accepted only for Codex runs, or when the record carries a
// CumulativeCostUSD snapshot (the run cost was then derived and validated against it).
func runCost(r Record) *float64 {
	if r.Salvaged || r.CostUSD == nil || !ValidCost(*r.CostUSD) {
		return nil
	}
	if r.Backend != "codex" && r.CumulativeCostUSD == nil {
		return nil
	}
	v := *r.CostUSD
	return &v
}

// legacyCost reports a valid recorded cost that runCost refuses.
func legacyCost(r Record) bool {
	return r.CostUSD != nil && ValidCost(*r.CostUSD) && runCost(r) == nil
}

// runUsage returns the run's own usage. CumulativeUsage is a session total and is never used.
func runUsage(r Record) *TokenUsage {
	if r.Salvaged || r.Usage == nil || !r.Usage.Valid() || (r.UsageScope != "" && r.UsageScope != "run") {
		return nil
	}
	u := *r.Usage
	return &u
}

// trendStarts returns the trend's interval, label and bucket start times for period at now.
// Hourly buckets advance by real elapsed hours, so a DST change drops or repeats a local hour.
// Daily buckets advance by calendar days.
func trendStarts(period string, now time.Time) (interval, label string, starts []time.Time) {
	y, m, d := now.Date()
	today := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	switch period {
	case PeriodToday:
		interval, label = IntervalHour, "Today"
		for t := today; !t.After(now); t = t.Add(time.Hour) {
			starts = append(starts, t)
		}
		return
	case PeriodWeek:
		label = "This week"
	case PeriodMonth:
		label = "This month"
	default:
		label = fmt.Sprintf("Last %d days", trendAllDays)
	}
	first := today
	switch period {
	case PeriodWeek:
		first = today.AddDate(0, 0, -((int(now.Weekday()) + 6) % 7))
	case PeriodMonth:
		first = time.Date(y, m, 1, 0, 0, 0, 0, now.Location())
	default:
		first = today.AddDate(0, 0, 1-trendAllDays)
	}
	for i := 0; ; i++ {
		t := first.AddDate(0, 0, i)
		if t.After(now) {
			break
		}
		starts = append(starts, t)
	}
	return IntervalDay, label, starts
}

// trendLabels names the buckets "14:00" or "Oct 29". An hourly label that occurs twice (the
// repeated hour when DST ends) gets the zone offset so the two stay distinguishable.
func trendLabels(interval string, starts []time.Time) []string {
	labels := make([]string, len(starts))
	count := map[string]int{}
	for i, t := range starts {
		layout := "Jan 2"
		if interval == IntervalHour {
			layout = "15:04"
		}
		labels[i] = t.Format(layout)
		count[labels[i]]++
	}
	for i, t := range starts {
		if count[labels[i]] > 1 {
			labels[i] += " " + t.Format("-0700")
		}
	}
	return labels
}

// trendIndex returns the bucket containing at, or -1 when at precedes the first bucket.
func trendIndex(starts []time.Time, at time.Time) int {
	return sort.Search(len(starts), func(i int) bool { return starts[i].After(at) }) - 1
}

func (a *statsAcc) totals() StatsTotals {
	t := a.t
	t.Sessions = len(a.sessions)
	return t
}

// Aggregate summarizes the records of one period. now fixes the period, the time zone (its
// location) and the upper bound: records with an unreadable or future timestamp are skipped and
// reported in Warnings. Every total comes from Record.Usage, CostUSD and ElapsedMs as recorded;
// nothing is estimated for runs that lack them.
func Aggregate(rs []Record, period string, now time.Time) (Stats, error) {
	start, err := PeriodStart(period, now)
	if err != nil {
		return Stats{}, err
	}
	type entry struct {
		r    Record
		at   time.Time
		line int
	}
	var in []entry
	skipped := 0
	for i, r := range rs {
		at, ok := ParseTS(r.TS, now.Location())
		if !ok || at.After(now) {
			skipped++
			continue
		}
		if at.Before(start) {
			continue
		}
		in = append(in, entry{r, at, i})
	}

	var total statsAcc
	legacy := 0
	interval, trendLabel, starts := trendStarts(period, now)
	buckets := make([]statsAcc, len(starts))
	projects, models := map[string]*statsAcc{}, map[string]*statsAcc{}
	projectName, modelName := map[string]string{}, map[string]string{}
	for _, e := range in {
		r := e.r
		elapsedOK := r.ElapsedMs != nil && *r.ElapsedMs >= 0
		total.add(r, elapsedOK)
		if i := trendIndex(starts, e.at); i >= 0 {
			buckets[i].add(r, elapsedOK)
		}
		if legacyCost(r) {
			legacy++
		}
		pk := r.Dir
		if projects[pk] == nil {
			projects[pk] = &statsAcc{}
			projectName[pk] = "unknown project"
			if pk != "" {
				projectName[pk] = filepath.Base(pk)
			}
		}
		projects[pk].add(r, elapsedOK)
		mk := r.Backend + "/" + r.Model
		if models[mk] == nil {
			models[mk] = &statsAcc{}
			modelName[mk] = firstNonEmpty(strings.TrimSpace(r.Backend+" "+r.Model), "unknown model")
		}
		models[mk].add(r, elapsedOK)
	}

	labels := trendLabels(interval, starts)
	points := make([]TrendPoint, len(starts))
	for i, t := range starts {
		points[i] = TrendPoint{Start: t.Format(time.RFC3339), Label: labels[i], Totals: buckets[i].totals()}
	}

	st := Stats{Period: period, Totals: total.totals(), Trend: Trend{Interval: interval, Label: trendLabel, Points: points}, Projects: groups(projects, projectName),
		Models: groups(models, modelName), Recent: []RecentRun{}, Warnings: []string{}, LegacyCostRuns: legacy}

	sort.SliceStable(in, func(i, j int) bool {
		if !in[i].at.Equal(in[j].at) {
			return in[i].at.After(in[j].at)
		}
		return in[i].line > in[j].line
	})
	for i := 0; i < len(in) && i < RecentLimit; i++ {
		r := in[i].r
		st.Recent = append(st.Recent, RecentRun{TS: r.TS, Status: r.Status, Dir: r.Dir, Task: r.Task,
			Session: r.Session, Model: r.Model, CostUSD: runCost(r), Usage: runUsage(r),
			Result: boundResult(r.Result), Salvaged: r.Salvaged, TerminationClass: r.TerminationClass(), ToolRestriction: r.ToolRestriction})
	}

	t := st.Totals
	if t.Runs > t.UsageRuns {
		st.Warnings = append(st.Warnings, fmt.Sprintf("token usage is recorded for %d of %d runs; token totals cover only those runs", t.UsageRuns, t.Runs))
	}
	if t.Runs > t.CostRuns {
		st.Warnings = append(st.Warnings, fmt.Sprintf("cost is recorded for %d of %d runs; the cost total covers only those runs", t.CostRuns, t.Runs))
	}
	if t.ExcludedSalvageCostRuns > 0 {
		st.Warnings = append(st.Warnings, fmt.Sprintf("%d salvaged run(s) have cost or usage left out of the totals and the recent list: salvaged accounting is incomplete and may contain session totals, so it is not a reliable full-run figure", t.ExcludedSalvageCostRuns))
	}
	if legacy > 0 {
		st.Warnings = append(st.Warnings, fmt.Sprintf("%d Claude run(s) recorded before cumulative cost snapshots are left out of cost totals and the recent list: their cost may be a session-cumulative total that overlaps other runs of the same session and cannot be recomputed", legacy))
	}
	st.Warnings = append(st.Warnings, "costs are worker or API-baseline estimates, not billing, and exclude the supervising agent's own spending")
	if skipped > 0 {
		st.Warnings = append(st.Warnings, fmt.Sprintf("skipped %d record(s) with an unreadable or future timestamp", skipped))
	}
	return st, nil
}

func groups(m map[string]*statsAcc, names map[string]string) []StatsGroup {
	out := make([]StatsGroup, 0, len(m))
	for k, a := range m {
		out = append(out, StatsGroup{Key: k, Name: names[k], Totals: a.totals()})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Totals.Runs != out[j].Totals.Runs {
			return out[i].Totals.Runs > out[j].Totals.Runs
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func boundResult(s string) string {
	r := []rune(s)
	if len(r) <= RecentResultRunes {
		return s
	}
	return string(r[:RecentResultRunes-len([]rune(truncationMarker))]) + truncationMarker
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
