package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/nativeusage"
)

type accountingRepairResult struct {
	Applied         bool           `json:"applied"`
	Runs            int            `json:"runs"`
	RecoveredRuns   int            `json:"recovered_runs"`
	PartialRuns     int            `json:"partial_runs"`
	UsageAdded      int            `json:"usage_runs_added"`
	CostAdded       int            `json:"cost_runs_added"`
	Unresolved      map[string]int `json:"unresolved"`
	CorrectionsPath string         `json:"corrections_path,omitempty"`
}

func cmdAccounting(args []string, stdout, stderr io.Writer) int {
	const usage = "usage: offload accounting repair [--apply] [--json]"
	if len(args) == 0 || args[0] != "repair" {
		return fail(stderr, errors.New(usage))
	}
	fs := flag.NewFlagSet("offload accounting repair", flag.ContinueOnError)
	fs.SetOutput(stderr)
	apply := fs.Bool("apply", false, "persist verified corrections without modifying the original run log")
	asJSON := fs.Bool("json", false, "print a repair summary without prompts or reports")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		return fail(stderr, errors.New(usage))
	}
	result, err := repairAccounting(history.Path(), *apply)
	if err != nil {
		return fail(stderr, err)
	}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(result); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "Native accounting: %d runs checked; %d recovered, %d partial; token coverage +%d runs, cost coverage +%d runs\n", result.Runs, result.RecoveredRuns, result.PartialRuns, result.UsageAdded, result.CostAdded)
	if *apply {
		fmt.Fprintln(stdout, "Verified corrections saved; original run log unchanged:", result.CorrectionsPath)
	} else {
		fmt.Fprintln(stdout, "Dry run; use --apply to save verified corrections.")
	}
	return 0
}

func repairAccounting(path string, apply bool) (accountingRepairResult, error) {
	out := accountingRepairResult{Applied: apply, Unresolved: map[string]int{}}
	rs, err := history.Read(path)
	if err != nil {
		return out, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	defer f.Close()
	rd := nativeusage.NewReader()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	i := 0
	line := 0
	for sc.Scan() {
		line++
		var original history.Record
		if json.Unmarshal(sc.Bytes(), &original) != nil {
			continue
		}
		// Bind each correction to the original bytes even when a previous pass
		// already overlaid its accounting. Stop if the run log was rewritten.
		if i >= len(rs) {
			break
		} // concurrent append belongs to the next pass
		r := rs[i]
		i++
		if r.TS != original.TS || r.Session != original.Session || r.RunID != original.RunID {
			return out, errors.New("run log changed during repair; retry")
		}
		out.Runs++
		if !nativeusage.Needed(r) {
			continue
		}
		overlapped := false
		for j, other := range rs {
			if j != i-1 && nativeusage.SamePromptOverlap(r, other) {
				overlapped = true
				break
			}
		}
		if overlapped {
			out.Unresolved["overlapping invocations with the same session and prompt"]++
			continue
		}
		recovered, ok, reason := rd.Recover(r)
		if !ok {
			out.Unresolved[reason]++
			continue
		}
		patch := history.NewAccountingPatch(sc.Bytes(), line, recovered)
		if reflect.DeepEqual(patch, history.NewAccountingPatch(sc.Bytes(), line, r)) {
			continue
		}
		if recovered.UsageScope == "partial_run" {
			out.PartialRuns++
		} else {
			out.RecoveredRuns++
			if r.Usage == nil || r.UsageScope == "partial_run" {
				out.UsageAdded++
			}
			if recovered.CostUSD != nil && (r.CostUSD == nil || (r.Backend != "codex" && r.CumulativeCostUSD == nil)) {
				out.CostAdded++
			}
		}
		if apply {
			if err := history.AppendAccountingPatch(path, patch); err != nil {
				return out, err
			}
		}
	}
	if apply {
		out.CorrectionsPath = path + ".accounting.jsonl"
	}
	return out, sc.Err()
}
