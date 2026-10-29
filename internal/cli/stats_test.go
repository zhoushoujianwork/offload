package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/live"
)

func statsRun(t *testing.T, now time.Time, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runStats(args, now, &out, &errb)
	return code, out.String(), errb.String()
}

func TestStatsInvalidArgs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Now()
	for _, args := range [][]string{{"--period", "year"}, {"--bogus"}, {"extra"}, {"--period"}, {"--json", "now"}} {
		if code, out, _ := statsRun(t, now, args...); code != 2 || out != "" {
			t.Errorf("%v: code=%d out=%q", args, code, out)
		}
	}
	var out, errb bytes.Buffer
	if code := Run("v", []string{"stats", "--period", "x"}, strings.NewReader(""), &out, &errb); code != 2 {
		t.Errorf("dispatch code=%d", code)
	}
}

func TestStatsJSONEmptyContract(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.FixedZone("X", 3600))
	code, out, _ := statsRun(t, now, "--json")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schema_version", "generated_at", "period", "totals", "trend", "projects", "models", "active", "recent", "warnings"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
	for _, k := range []string{"projects", "models", "active", "recent"} {
		if string(m[k]) != "[]" {
			t.Errorf("%s = %s", k, m[k])
		}
	}
	if string(m["schema_version"]) != "1" || string(m["period"]) != `"today"` || string(m["generated_at"]) != `"2026-10-29T12:00:00+01:00"` {
		t.Errorf("header: %s %s %s", m["schema_version"], m["period"], m["generated_at"])
	}
	var trend struct {
		Interval string `json:"interval"`
		Label    string `json:"label"`
		Points   []struct {
			Start  string                     `json:"start"`
			Label  string                     `json:"label"`
			Totals map[string]json.RawMessage `json:"totals"`
		} `json:"points"`
	}
	if err := json.Unmarshal(m["trend"], &trend); err != nil {
		t.Fatal(err)
	}
	if trend.Interval != "hour" || trend.Label != "Today" || len(trend.Points) != 13 || trend.Points[12].Label != "12:00" ||
		trend.Points[0].Start != "2026-10-29T00:00:00+01:00" || string(trend.Points[3].Totals["usage"]) != "null" || string(trend.Points[3].Totals["runs"]) != "0" {
		t.Errorf("trend: %+v", trend)
	}
	var tot map[string]json.RawMessage
	json.Unmarshal(m["totals"], &tot)
	for _, k := range []string{"runs", "sessions", "ok", "error", "timeout", "salvaged", "elapsed_ms", "elapsed_runs", "usage", "usage_runs", "cost_usd", "cost_runs", "excluded_salvage_cost_runs"} {
		if _, ok := tot[k]; !ok {
			t.Errorf("totals missing %s", k)
		}
	}
	if string(tot["usage"]) != "null" || string(tot["cost_usd"]) != "null" {
		t.Errorf("nullable: %s %s", tot["usage"], tot["cost_usd"])
	}
}

func TestStatsJSONRecordsAndActive(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// A fixed instant keeps the record inside the period and the idle times deterministic. The
	// live files are fresh on disk, so live.List does not mark them lost.
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.FixedZone("X", 3600))
	ts := "2026-10-29 11:59:00"
	cost := 1.5
	appendListRecord(t, history.Record{TS: ts, Status: "ok", Session: "s", Dir: "/p/x", Backend: "codex", Model: "m",
		Usage: &history.TokenUsage{Input: 10, Cached: 4, Output: 2}, UsageScope: "run", CostUSD: &cost, CostIsEstimate: true})
	for _, s := range []live.State{
		{PID: 1, Phase: live.Running, StartedAt: now.Add(-time.Hour), LastEventAt: now.Add(-2 * time.Minute)},
		{PID: 2, Phase: live.Salvaging, StartedAt: now.Add(-time.Minute), LastEventAt: now},
		{PID: 3, Phase: live.Done, Status: "ok", StartedAt: now},
	} {
		if err := live.Publish(s); err != nil {
			t.Fatal(err)
		}
	}
	code, out, _ := statsRun(t, now, "--json", "--period", "week")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	var got struct {
		Period string `json:"period"`
		Totals struct {
			Runs  int `json:"runs"`
			Usage struct {
				Input int64 `json:"input_tokens"`
			} `json:"usage"`
			Cost float64 `json:"cost_usd"`
		} `json:"totals"`
		Active []struct {
			PID     int  `json:"pid"`
			Stalled bool `json:"stalled"`
		} `json:"active"`
		Projects []struct{ Key, Name string }
		Trend    struct {
			Interval string `json:"interval"`
			Points   []struct {
				Totals struct {
					Runs int `json:"runs"`
				} `json:"totals"`
			} `json:"points"`
		} `json:"trend"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Trend.Points) == 0 {
		t.Errorf("trend missing: %s", out)
	}
	if got.Period != "week" || got.Totals.Runs != 1 || got.Totals.Usage.Input != 10 || got.Totals.Cost != 1.5 {
		t.Errorf("totals: %+v", got)
	}
	if len(got.Active) != 2 || got.Active[0].PID != 1 || !got.Active[0].Stalled || got.Active[1].PID != 2 || got.Active[1].Stalled {
		t.Errorf("active: %+v", got.Active)
	}
	if len(got.Projects) != 1 || got.Projects[0].Key != "/p/x" || got.Projects[0].Name != "x" {
		t.Errorf("projects: %+v", got.Projects)
	}
	if !strings.Contains(out, `"pid":1`) || !strings.Contains(out, `"permission_denials"`) {
		t.Errorf("live.State fields missing: %s", out)
	}
}

func TestStatsHumanOutput(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.FixedZone("X", 3600))
	ts := "2026-10-29 11:59:00"
	appendListRecord(t, history.Record{TS: ts, Status: "ok", Session: "a"})
	code, out, _ := statsRun(t, now)
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"offload stats: today", "runs: 1 (ok 1", "tokens: N/A (usage for 0 of 1 runs)", "estimated cost: N/A (cost for 0 of 1 runs)", "elapsed: N/A", "active: 0", "not billing"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}
