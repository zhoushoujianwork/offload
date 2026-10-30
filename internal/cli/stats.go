package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/live"
	"github.com/zhoushoujianwork/offload/internal/text"
)

// statsSchemaVersion changes only when an existing field of the stats JSON changes meaning.
const statsSchemaVersion = 1

// statsStallAfter is how long an active worker may be silent before stats marks it stalled.
const statsStallAfter = 60 * time.Second

type statsActive struct {
	live.State
	Stalled bool `json:"stalled"`
}

type statsOutput struct {
	SchemaVersion int                  `json:"schema_version"`
	GeneratedAt   string               `json:"generated_at"`
	Period        string               `json:"period"`
	Totals        history.StatsTotals  `json:"totals"`
	Trend         history.Trend        `json:"trend"`
	Projects      []history.StatsGroup `json:"projects"`
	Models        []history.StatsGroup `json:"models"`
	Active        []statsActive        `json:"active"`
	Recent        []history.RecentRun  `json:"recent"`
	Warnings      []string             `json:"warnings"`
	// Additive fields. Workload counts the invocations now processing; Active below also lists
	// queued and cleaning ones, so len(active) is not a processing count. Daily comes from the
	// run log and the processing log and does not depend on Period.
	Workload statsWorkload `json:"workload"`
	Goals    statsGoals    `json:"goals"`
	Daily    statsDaily    `json:"daily"`
}

const statsUsage = "usage: offload stats [--json] [--period today|week|month|all]"

func cmdStats(args []string, stdout, stderr io.Writer) int {
	return runStats(args, time.Now(), stdout, stderr)
}

func runStats(args []string, now time.Time, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("offload stats", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, statsUsage) }
	asJSON := fs.Bool("json", false, "print one JSON document")
	period := fs.String("period", history.PeriodToday, "today, week (from Monday), month or all")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		return fail(stderr, errors.New(statsUsage))
	}
	if !history.ValidPeriod(*period) {
		return fail(stderr, fmt.Errorf("unknown period %q; %s", *period, statsUsage))
	}
	// Report bodies are kept only for the newest runs that `recent` can list, so refreshing the
	// metadata metrics does not hold every historical report in memory.
	rlog, err := history.ReadStats(history.Path(), now)
	if err != nil {
		return fail(stderr, err)
	}
	rs := rlog.Records
	st, err := history.Aggregate(rs, *period, now)
	if err != nil {
		return fail(stderr, err)
	}
	states, unreadable, lerr := live.ListChecked()
	active := activeWorkers(states, now)
	if lerr != nil {
		st.Warnings = append(st.Warnings, "active workers unavailable: "+text.OneLine(lerr.Error()))
	}
	workload := classifyWorkload(states, unreadable, lerr, now)
	workload.applyAdmission()
	if unreadable > 0 && lerr == nil {
		st.Warnings = append(st.Warnings, fmt.Sprintf("%d live state file(s) could not be read; worker counts are lower bounds", unreadable))
	}
	alive := map[string]bool{}
	for _, s := range states {
		if s.RunID != "" && s.Active() && s.Phase != live.Lost {
			alive[s.RunID] = true
		}
	}
	days := history.CountDailyCalls(nil, now, averageDays)
	plog, perr := history.ReadProcessing(history.ProcessingPath(), days.Days[0].Start)
	dailyStats := buildDaily(rs, rlog.Corrupt, plog, perr, alive, now)
	if rlog.Corrupt > 0 {
		st.Warnings = append(st.Warnings, fmt.Sprintf("%d run-log line(s) are damaged and were skipped; daily call counts and the average are lower bounds and coverage is partial", rlog.Corrupt))
	}
	if perr != nil {
		st.Warnings = append(st.Warnings, "processing log unreadable; peak concurrency unavailable: "+text.OneLine(perr.Error()))
	}
	goals := statsGoals{Processing: goalProcessing, DailyCalls: goalDailyCalls,
		Note: "observability targets for one person on one computer; not execution limits, a load-test result, or proof of sustained throughput"}
	if *asJSON {
		out := statsOutput{SchemaVersion: statsSchemaVersion, GeneratedAt: now.Format(time.RFC3339), Period: st.Period,
			Totals: st.Totals, Trend: st.Trend, Projects: st.Projects, Models: st.Models, Active: active, Recent: st.Recent, Warnings: st.Warnings,
			Workload: workload, Goals: goals, Daily: dailyStats}
		if err := json.NewEncoder(stdout).Encode(out); err != nil {
			fmt.Fprintln(stderr, "offload: write JSON stats:", err)
			return 2
		}
		return 0
	}
	printStats(stdout, st, active, workload, dailyStats, now)
	return 0
}

// activeWorkers lists every active state (queued, running, salvaging, cleaning) and lost ones
// regardless of the period. They are never added to the completed-run totals. The list is not a
// processing count: queued and cleaning entries are in it; see classifyWorkload.
func activeWorkers(states []live.State, now time.Time) []statsActive {
	out := []statsActive{}
	for _, s := range states {
		if !s.Active() && s.Phase != live.Lost {
			continue
		}
		out = append(out, statsActive{State: s, Stalled: s.Active() && now.Sub(s.LastEventAt) >= statsStallAfter})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

func printStats(w io.Writer, st history.Stats, active []statsActive, wl statsWorkload, daily statsDaily, now time.Time) {
	t := st.Totals
	fmt.Fprintf(w, "offload stats: %s (%s)\n", st.Period, now.Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(w, "runs: %d (ok %d, error %d, timeout %d, salvaged %d) | sessions: %d\n", t.Runs, t.OK, t.Error, t.Timeout, t.Salvaged, t.Sessions)
	fmt.Fprintf(w, "ended without a result: canceled %d (requested), interrupted %d (shutdown, resumable), killed %d (unrequested)\n", t.Canceled, t.Interrupted, t.Killed)
	if t.ElapsedRuns > 0 {
		fmt.Fprintf(w, "elapsed: %s (%d of %d runs)\n", listElapsed(&t.ElapsedMs), t.ElapsedRuns, t.Runs)
	} else {
		fmt.Fprintf(w, "elapsed: N/A (0 of %d runs)\n", t.Runs)
	}
	if u := t.Usage; u != nil {
		fmt.Fprintf(w, "tokens: input %d (cached %d, cache write %d), output %d (usage for %d of %d runs)\n", u.Input, u.Cached, u.CacheWrite, u.Output, t.UsageRuns, t.Runs)
	} else {
		fmt.Fprintf(w, "tokens: N/A (usage for 0 of %d runs)\n", t.Runs)
	}
	if t.CostUSD != nil {
		fmt.Fprintf(w, "estimated cost: $%.4f (cost for %d of %d runs)\n", *t.CostUSD, t.CostRuns, t.Runs)
	} else {
		fmt.Fprintf(w, "estimated cost: N/A (cost for 0 of %d runs)\n", t.Runs)
	}
	printWorkload(w, wl, daily)
	fmt.Fprintf(w, "active: %d (entries that are queued, processing or cleaning; not the processing count)\n", len(active))
	for _, a := range active {
		stalled := ""
		if a.Stalled {
			stalled = " STALLED"
		}
		fmt.Fprintf(w, "  %s%s\n", describe(a.State), stalled)
	}
	for _, msg := range st.Warnings {
		fmt.Fprintf(w, "note: %s\n", msg)
	}
}
