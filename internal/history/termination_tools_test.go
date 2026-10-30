package history

import (
	"encoding/json"
	"testing"
	"time"
)

// A stopped run can also have ignored unsupported tool limits. Neither signal may be lost
// when the independently developed reporting changes are combined.
func TestTerminationAndToolRestrictionSurviveTogether(t *testing.T) {
	r := Record{TS: "2026-10-30 10:00:00", Status: "error", Session: "s", Dir: "/project", Model: "m",
		ToolRestriction: ToolRestrictionNotEnforced, Termination: &Termination{Class: ClassInterrupted}}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var restored Record
	if err := json.Unmarshal(b, &restored); err != nil {
		t.Fatal(err)
	}
	st, err := Aggregate([]Record{restored}, PeriodToday, time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	for _, totals := range []StatsTotals{st.Totals, st.Projects[0].Totals, st.Models[0].Totals, st.Trend.Points[10].Totals} {
		if totals.Runs != 1 || totals.Interrupted != 1 || totals.ToolRestrictionNotEnforced != 1 {
			t.Fatalf("reporting dimensions were lost: %+v", totals)
		}
	}
	if len(st.Recent) != 1 || st.Recent[0].TerminationClass != ClassInterrupted || st.Recent[0].ToolRestriction != ToolRestrictionNotEnforced {
		t.Fatalf("recent metadata lost: %+v", st.Recent)
	}
}
