package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
)

func TestCallerStatsGroupsFilterAndLegacy(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	for i, caller := range []string{"agh", "clawflow", "custom.app", ""} {
		r := history.Record{TS: "2026-10-30 10:00:00", RunID: caller, Status: "ok", Session: "same-resumed-session", Caller: caller, Result: "report " + caller}
		if i == 0 {
			r.Usage = &history.TokenUsage{Input: 10}
			r.UsageScope = "run"
		}
		appendListRecord(t, r)
	}
	before, _ := os.ReadFile(history.Path())
	for _, filter := range []string{"", "AGH", "clawflow", "custom.app", "unspecified", "missing"} {
		args := []string{"--json"}
		if filter != "" {
			args = append(args, "--caller", filter)
		}
		code, output, _ := statsRun(t, now, args...)
		if code != 0 {
			t.Fatal(code, output)
		}
		var d statsOutput
		if err := json.Unmarshal([]byte(output), &d); err != nil {
			t.Fatal(err)
		}
		want := 1
		if filter == "" {
			want = 4
		}
		if filter == "missing" {
			want = 0
		}
		if d.Totals.Runs != want || len(d.Callers) != 4 {
			t.Fatalf("filter %q: %+v", filter, d)
		}
		sum := 0
		for _, group := range d.Callers {
			sum += group.Totals.Runs
		}
		if sum != 4 {
			t.Fatal(d.Callers)
		}
		if filter != "" && filter != "AGH" && d.Totals.Usage != nil {
			t.Fatal("missing usage became measured", d.Totals)
		}
		for _, r := range d.Recent {
			if filter != "" && r.Caller != strings.ToLower(filter) {
				t.Fatal(filter, r)
			}
		}
	}
	after, _ := os.ReadFile(history.Path())
	if string(before) != string(after) {
		t.Fatal("stats rewrote history")
	}
	if code, _, _ := statsRun(t, now, "--caller", "bad label"); code == 0 {
		t.Fatal("invalid caller accepted")
	}
	if code, _, _ := statsRun(t, now, "--caller", "agh", "--caller", "clawflow"); code == 0 {
		t.Fatal("duplicate caller accepted")
	}
}

func TestCallerFilterKeepsOlderMatchingReport(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	appendListRecord(t, history.Record{TS: "2026-10-30 09:00:00", Caller: "agh", Result: "OLDER MATCHING REPORT"})
	for i := 0; i < history.RecentLimit+1; i++ {
		appendListRecord(t, history.Record{TS: "2026-10-30 10:00:00", Caller: "clawflow", Result: "other"})
	}
	_, output, _ := statsRun(t, now, "--json", "--caller", "agh")
	if !strings.Contains(output, "OLDER MATCHING REPORT") {
		t.Fatal(output)
	}
}
