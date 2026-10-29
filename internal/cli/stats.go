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
	rs, err := history.Read(history.Path())
	if err != nil {
		return fail(stderr, err)
	}
	st, err := history.Aggregate(rs, *period, now)
	if err != nil {
		return fail(stderr, err)
	}
	active, lerr := activeWorkers(now)
	if lerr != nil {
		st.Warnings = append(st.Warnings, "active workers unavailable: "+text.OneLine(lerr.Error()))
	}
	if *asJSON {
		out := statsOutput{SchemaVersion: statsSchemaVersion, GeneratedAt: now.Format(time.RFC3339), Period: st.Period,
			Totals: st.Totals, Trend: st.Trend, Projects: st.Projects, Models: st.Models, Active: active, Recent: st.Recent, Warnings: st.Warnings}
		if err := json.NewEncoder(stdout).Encode(out); err != nil {
			fmt.Fprintln(stderr, "offload: write JSON stats:", err)
			return 2
		}
		return 0
	}
	printStats(stdout, st, active, now)
	return 0
}

// activeWorkers lists running, salvaging and lost workers regardless of the period. They are
// never added to the completed-run totals.
func activeWorkers(now time.Time) ([]statsActive, error) {
	out := []statsActive{}
	states, err := live.List()
	if err != nil {
		return out, err
	}
	for _, s := range states {
		if !s.Active() && s.Phase != live.Lost {
			continue
		}
		out = append(out, statsActive{State: s, Stalled: s.Active() && now.Sub(s.LastEventAt) >= statsStallAfter})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, nil
}

func printStats(w io.Writer, st history.Stats, active []statsActive, now time.Time) {
	t := st.Totals
	fmt.Fprintf(w, "offload stats: %s (%s)\n", st.Period, now.Format("2006-01-02 15:04 MST"))
	fmt.Fprintf(w, "runs: %d (ok %d, error %d, timeout %d, salvaged %d) | sessions: %d\n", t.Runs, t.OK, t.Error, t.Timeout, t.Salvaged, t.Sessions)
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
	fmt.Fprintf(w, "active: %d\n", len(active))
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
