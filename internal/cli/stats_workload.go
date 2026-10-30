package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/live"
)

// Observability goals for one person on one computer. They are targets to show progress against,
// not execution limits (see the admission limit), a load-test result, or proof of throughput.
const (
	goalProcessing = 100
	goalDailyCalls = 10000
	// averageDays is the number of completed local calendar days in the daily average.
	averageDays = 7
)

// Workload status values.
const (
	workloadOK          = "ok"
	workloadPartial     = "partial"     // some live files could not be read: counts are lower bounds
	workloadUnavailable = "unavailable" // the live directory could not be read: no counts
)

// statsWorkload counts the invocations the local state directory shows right now. Every state is
// in exactly one bucket: processing (running or salvaging; salvaging is the labelled subset of
// processing that is in its report step), queued, cleaning, unresolved (worker processes survive
// after the run ended or went silent) or lost (silent, with no surviving worker). Finished runs
// are in none. Counts are null when the live state could not be read: unavailable is not zero.
type statsWorkload struct {
	Scope            string `json:"scope"`
	StateDir         string `json:"state_dir"`
	Status           string `json:"status"`
	Processing       *int   `json:"processing"`
	Salvaging        *int   `json:"salvaging"`
	Stalled          *int   `json:"stalled"`
	Queued           *int   `json:"queued"`
	Cleaning         *int   `json:"cleaning"`
	Unresolved       *int   `json:"unresolved"`
	Lost             *int   `json:"lost"`
	UnreadableStates int    `json:"unreadable_states"`
	// AdmissionLimit is the configured worker limit, which is separate from the goal. It is null
	// when unlimited or unknown; AdmissionStatus says which.
	AdmissionLimit  *int   `json:"admission_limit"`
	AdmissionStatus string `json:"admission_status"` // limited | unlimited | unavailable
}

type statsGoals struct {
	Processing int    `json:"processing"`
	DailyCalls int    `json:"daily_calls"`
	Note       string `json:"note"`
}

type statsDay struct {
	Date string `json:"date"`
	// RecordedCalls counts run-log records whose invocation started that day. It is null for a
	// day the run log cannot vouch for (before its first recorded day).
	RecordedCalls      *int    `json:"recorded_calls"`
	Covered            bool    `json:"covered"`
	PeakProcessing     *int    `json:"peak_processing"`
	PeakAt             *string `json:"peak_at"`
	PeakCoverage       string  `json:"peak_coverage"` // full | partial (lower bound) | unavailable
	PeakIncompleteRuns int     `json:"peak_incomplete_runs"`
}

type statsAverage struct {
	WindowStart string   `json:"window_start"` // first completed day
	WindowEnd   string   `json:"window_end"`   // last completed day, inclusive; today is excluded
	Days        int      `json:"days"`
	CoveredDays int      `json:"covered_days"`
	Calls       *float64 `json:"recorded_calls_per_day"`
	Coverage    string   `json:"coverage"` // complete | partial | unavailable
	// GoalMet is true only for a complete window whose average reaches the daily goal.
	GoalMet bool   `json:"goal_met"`
	Note    string `json:"note,omitempty"`
}

type statsDaily struct {
	Timezone string       `json:"timezone"`
	Label    string       `json:"label"`
	Today    statsDay     `json:"today"`
	Days     []statsDay   `json:"days"`
	Average  statsAverage `json:"average"`
	Peak     string       `json:"peak_source"`
}

func intp(n int) *int { return &n }

// classifyWorkload buckets live states. unreadable is the number of state files that could not be
// read; readErr is a failure to read the directory at all.
func classifyWorkload(states []live.State, unreadable int, readErr error, now time.Time) statsWorkload {
	w := statsWorkload{Scope: "this computer's Offload state directory; other computers or state directories are not included",
		StateDir: live.Dir(), UnreadableStates: unreadable, Status: workloadOK, AdmissionStatus: "unavailable"}
	if readErr != nil {
		w.Status = workloadUnavailable
		return w
	}
	if unreadable > 0 {
		w.Status = workloadPartial
	}
	var processing, salvaging, stalled, queued, cleaning, unresolved, lost int
	for _, s := range states {
		switch {
		case s.Phase == live.Unresolved || s.Unresolved:
			unresolved++
		case s.Phase == live.Lost:
			lost++
		case s.Phase == live.Queued:
			queued++
		case s.Phase == live.Cleaning:
			cleaning++
		case s.Processing():
			processing++
			if s.Phase == live.Salvaging {
				salvaging++
			}
			if now.Sub(s.LastEventAt) >= statsStallAfter {
				stalled++
			}
		}
	}
	w.Processing, w.Salvaging, w.Stalled = intp(processing), intp(salvaging), intp(stalled)
	w.Queued, w.Cleaning, w.Unresolved, w.Lost = intp(queued), intp(cleaning), intp(unresolved), intp(lost)
	return w
}

// applyAdmission fills the configured limit; an unreadable config leaves it unavailable.
func (w *statsWorkload) applyAdmission() {
	cfg, err := config.Load(config.Path())
	if err != nil {
		return
	}
	n, err := cfg.Limits.EffectiveMaxWorkers()
	switch {
	case err != nil:
	case n == 0:
		w.AdmissionStatus = "unlimited"
	default:
		w.AdmissionLimit, w.AdmissionStatus = intp(n), "limited"
	}
}

// buildDaily combines recorded calls with the durable processing log. calls come from the run
// log; peaks come only from processing events, never from live files or elapsed times.
func buildDaily(rs []history.Record, log history.ProcessingLog, procErr error, alive map[string]bool, now time.Time) statsDaily {
	cw := history.CountDailyCalls(rs, now, averageDays)
	zone, _ := now.Zone()
	d := statsDaily{Timezone: now.Location().String() + " (" + zone + ")", Label: "recorded calls: one per run-log record, by invocation start time",
		Peak: "processing.jsonl events from admission to end of processing; live files and polling are not used"}
	spans := log.Resolve(now, alive)
	day := func(dc history.DayCalls, end time.Time) statsDay {
		sd := statsDay{Date: dc.Start.Format("2006-01-02"), Covered: dc.Covered, PeakCoverage: history.CoverageUnavailable}
		if dc.Covered {
			sd.RecordedCalls = intp(dc.Calls)
		}
		if procErr == nil {
			p := log.PeakInDay(spans, dc.Start, end)
			sd.PeakProcessing, sd.PeakCoverage, sd.PeakIncompleteRuns = p.Value, p.Coverage, p.IncompleteRuns
			// Runs that were admitted but have no processing events (older versions) are missing from the peak.
			if p.Value != nil && p.Coverage == history.CoverageFull && history.UncoveredBy(rs, dc.Start, end, log) > 0 {
				sd.PeakCoverage = history.CoveragePartial
			}
			if p.At != nil {
				at := p.At.Format(time.RFC3339Nano)
				sd.PeakAt = &at
			}
		}
		return sd
	}
	for i, dc := range cw.Days {
		end := cw.Today.Start
		if i+1 < len(cw.Days) {
			end = cw.Days[i+1].Start
		}
		d.Days = append(d.Days, day(dc, end))
	}
	d.Today = day(cw.Today, now.Add(time.Nanosecond))

	a := statsAverage{Days: averageDays, WindowStart: cw.Days[0].Start.Format("2006-01-02"),
		WindowEnd: cw.Days[len(cw.Days)-1].Start.Format("2006-01-02")}
	sum := 0
	for _, dc := range cw.Days {
		if dc.Covered {
			a.CoveredDays++
			sum += dc.Calls
		}
	}
	switch {
	case a.CoveredDays == 0:
		a.Coverage = "unavailable"
		a.Note = "no run-log coverage for any day in the window"
	default:
		v := float64(sum) / float64(a.CoveredDays)
		a.Calls = &v
		a.Coverage = "complete"
		if a.CoveredDays < averageDays || cw.Skipped > 0 {
			a.Coverage = "partial"
			a.Note = "partial coverage: the average covers only days the run log can vouch for and is not a seven-day figure"
			if cw.Skipped > 0 && a.CoveredDays == averageDays {
				a.Note = "partial coverage: some records have an unreadable or future timestamp and are in no day"
			}
		}
	}
	a.GoalMet = a.Coverage == "complete" && a.Calls != nil && *a.Calls >= goalDailyCalls
	d.Average = a
	return d
}

func countText(n *int) string {
	if n == nil {
		return "unavailable"
	}
	return fmt.Sprint(*n)
}

func printWorkload(w io.Writer, wl statsWorkload, d statsDaily) {
	fmt.Fprintf(w, "processing now: %s / %d goal (salvaging %s, stalled %s)", countText(wl.Processing), goalProcessing, countText(wl.Salvaging), countText(wl.Stalled))
	switch wl.AdmissionStatus {
	case "limited":
		fmt.Fprintf(w, " | admission limit %d", *wl.AdmissionLimit)
	case "unlimited":
		fmt.Fprint(w, " | admission limit none")
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "queued: %s | cleaning: %s | unresolved: %s | lost: %s (scope: this computer's state directory)\n",
		countText(wl.Queued), countText(wl.Cleaning), countText(wl.Unresolved), countText(wl.Lost))
	if wl.Status != workloadOK {
		fmt.Fprintf(w, "note: live worker state %s (%d unreadable file(s)); counts are not zero-proof\n", wl.Status, wl.UnreadableStates)
	}
	fmt.Fprintf(w, "recorded calls today: %s / %d goal | peak processing today: %s\n", countText(d.Today.RecordedCalls), goalDailyCalls, peakText(d.Today))
	if d.Average.Calls != nil {
		fmt.Fprintf(w, "7-day average: %.1f recorded calls/day (%s to %s, %d of %d days covered, %s; %s)\n", *d.Average.Calls,
			d.Average.WindowStart, d.Average.WindowEnd, d.Average.CoveredDays, d.Average.Days, d.Average.Coverage, d.Timezone)
	} else {
		fmt.Fprintf(w, "7-day average: unavailable (%s to %s; %s)\n", d.Average.WindowStart, d.Average.WindowEnd, d.Average.Note)
	}
	fmt.Fprintln(w, "goals are observability targets, not limits or measured capacity")
}

func peakText(d statsDay) string {
	if d.PeakProcessing == nil {
		return "unavailable"
	}
	if d.PeakCoverage != history.CoverageFull {
		return fmt.Sprintf("at least %d (partial coverage)", *d.PeakProcessing)
	}
	return fmt.Sprint(*d.PeakProcessing)
}
