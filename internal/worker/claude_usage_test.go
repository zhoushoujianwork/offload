package worker

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
)

const claudeSession = "df3fb00c-714d-4b3c-bf1f-d7a5847aafc3"

// The two lines are the final results of a real fresh run and its --resume (trimmed to the used fields).
const (
	measuredFresh  = `{"type":"result","subtype":"success","is_error":false,"result":"one","session_id":"` + claudeSession + `","num_turns":1,"usage":{"input_tokens":2381,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":6},"modelUsage":{"claude-sonnet-5":{"inputTokens":2381,"outputTokens":6,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.004822}},"total_cost_usd":0.004822}`
	measuredResume = `{"type":"result","subtype":"success","is_error":false,"result":"two","session_id":"` + claudeSession + `","num_turns":1,"usage":{"input_tokens":2431,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":6},"modelUsage":{"claude-sonnet-5":{"inputTokens":4812,"outputTokens":12,"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.009744}},"total_cost_usd":0.009744}`
)

func resultOf(t *testing.T, line string) *claudeResult {
	t.Helper()
	var r claudeResult
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

func fakeResultClaude(t *testing.T, line string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nprintf '%s\\n' '" + line + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func runFake(t *testing.T, line, resume string) history.Record {
	t.Helper()
	t.Setenv("OFFLOAD_CLAUDE", fakeResultClaude(t, line))
	mockPreflight(t)
	o := testOpts(t)
	o.Resume = resume
	rec, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func stateEnv(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
}

func appendRec(t *testing.T, r history.Record) {
	t.Helper()
	if err := history.Append(history.Path(), r); err != nil {
		t.Fatal(err)
	}
}

func near(a *float64, b float64) bool { return a != nil && math.Abs(*a-b) < 1e-12 }

func TestMeasuredFreshThenResume(t *testing.T) {
	stateEnv(t)
	fresh := runFake(t, measuredFresh, "")
	if fresh.Usage == nil || *fresh.Usage != (history.TokenUsage{Input: 2381, Output: 6}) ||
		fresh.UsageSource != "claude_result" || fresh.UsageScope != "run" {
		t.Fatalf("%+v", fresh)
	}
	if !near(fresh.CostUSD, .004822) || !near(fresh.CumulativeCostUSD, .004822) || *fresh.CumulativeUsage != *fresh.Usage || !fresh.CostIsEstimate {
		t.Fatalf("%+v", fresh)
	}
	appendRec(t, fresh)

	resumed := runFake(t, measuredResume, claudeSession)
	if *resumed.Usage != (history.TokenUsage{Input: 2431, Output: 6}) || *resumed.CumulativeUsage != (history.TokenUsage{Input: 4812, Output: 12}) {
		t.Fatalf("%+v", resumed)
	}
	if !near(resumed.CostUSD, .004922) || !near(resumed.CumulativeCostUSD, .009744) || resumed.CostUnavailable != "" {
		t.Fatalf("cost=%v cum=%v %s", resumed.CostUSD, resumed.CumulativeCostUSD, resumed.CostUnavailable)
	}
	// A second resume continues from the resumed record, not the fresh one.
	appendRec(t, resumed)
	if b := captureClaudeBaseline(history.Path(), claudeSession); b.Usage == nil || b.Usage.Input != 4812 || !near(b.Cost, .009744) {
		t.Fatalf("%+v", b)
	}
}

func TestClaudeCacheMapping(t *testing.T) {
	line := `{"type":"result","usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":1000,"output_tokens":5},` +
		`"modelUsage":{"a":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":100,"cacheCreationInputTokens":1000,"costUSD":1},` +
		`"b":{"inputTokens":1,"outputTokens":2,"cacheReadInputTokens":3,"cacheCreationInputTokens":4,"costUSD":1}},"total_cost_usd":2}`
	r := resultOf(t, line)
	u, _ := decodeClaudeUsage(r.RawUsage)
	if u == nil || *u != (history.TokenUsage{Input: 1110, Cached: 100, CacheWrite: 1000, Output: 5}) {
		t.Fatalf("%+v", u)
	}
	c, _ := decodeClaudeModelUsage(r.RawModelUsage)
	if c == nil || *c != (history.TokenUsage{Input: 1118, Cached: 103, CacheWrite: 1004, Output: 7}) {
		t.Fatalf("%+v", c)
	}
	// Absent or null caches default to 0 when input and output are present.
	for _, raw := range []string{`{"input_tokens":4,"output_tokens":1}`, `{"input_tokens":4,"output_tokens":1,"cache_read_input_tokens":null,"cache_creation_input_tokens":null}`} {
		if u, _ := decodeClaudeUsage(json.RawMessage(raw)); u == nil || *u != (history.TokenUsage{Input: 4, Output: 1}) {
			t.Fatalf("%s: %+v", raw, u)
		}
	}
}

func TestClaudeUsageRejectsInvalid(t *testing.T) {
	for _, raw := range []string{
		``, `null`, `[]`, `{}`, `{"input_tokens":1}`, `{"output_tokens":1}`,
		`{"input_tokens":null,"output_tokens":1}`, `{"input_tokens":-1,"output_tokens":1}`,
		`{"input_tokens":1,"output_tokens":-1}`, `{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":-1}`,
		`{"input_tokens":1.5,"output_tokens":1}`, `{"input_tokens":"1","output_tokens":1}`,
		`{"input_tokens":9223372036854775808,"output_tokens":1}`,
		`{"input_tokens":9223372036854775807,"output_tokens":1,"cache_read_input_tokens":1}`,
		`{"input_tokens":1,"output_tokens":1,"cache_read_input_tokens":9223372036854775807,"cache_creation_input_tokens":9223372036854775807}`,
	} {
		if u, reason := decodeClaudeUsage(json.RawMessage(raw)); u != nil || reason == "" {
			t.Errorf("accepted %q: %+v", raw, u)
		}
	}
	for _, raw := range []string{
		``, `null`, `{}`, `{"m":{}}`, `{"m":{"inputTokens":1}}`, `{"m":{"inputTokens":-1,"outputTokens":1}}`,
		`{"m":{"inputTokens":1,"outputTokens":1,"costUSD":-1}}`,
		`{"a":{"inputTokens":9223372036854775807,"outputTokens":1},"b":{"inputTokens":1,"outputTokens":1}}`,
		`{"a":{"inputTokens":1,"outputTokens":1},"b":{"inputTokens":1}}`,
	} {
		if u, reason := decodeClaudeModelUsage(json.RawMessage(raw)); u != nil || reason == "" {
			t.Errorf("accepted %q: %+v", raw, u)
		}
	}
}

func claudeRec(session string, cum *history.TokenUsage, cost *float64) history.Record {
	return history.Record{Session: session, Backend: "claude", Status: "ok", CumulativeUsage: cum, CumulativeCostUSD: cost, CostIsEstimate: true}
}

func f(v float64) *float64 { return &v }

func TestBaselineUsesOnlyLatestRecordOfSession(t *testing.T) {
	stateEnv(t)
	good := claudeRec(claudeSession, &history.TokenUsage{Input: 10, Output: 1}, f(.01))
	appendRec(t, good)
	if b := captureClaudeBaseline(history.Path(), claudeSession); b.Usage == nil {
		t.Fatal(b.Reason)
	}
	for name, newer := range map[string]history.Record{
		"no cumulative usage": claudeRec(claudeSession, nil, f(.02)),
		"no cumulative cost":  claudeRec(claudeSession, &history.TokenUsage{Input: 20, Output: 2}, nil),
		"invalid cost":        claudeRec(claudeSession, &history.TokenUsage{Input: 20, Output: 2}, f(-1)),
		"codex":               {Session: claudeSession, Backend: "codex"},
	} {
		t.Run(name, func(t *testing.T) {
			stateEnv(t)
			appendRec(t, good)
			appendRec(t, newer)
			b := captureClaudeBaseline(history.Path(), claudeSession)
			if b.Usage != nil || b.Cost != nil || b.Reason == "" {
				t.Fatalf("stale baseline used: %+v", b)
			}
		})
	}
	stateEnv(t)
	other := claudeRec("other", &history.TokenUsage{Input: 1}, f(1))
	appendRec(t, other)
	if b := captureClaudeBaseline(history.Path(), claudeSession); b.Usage != nil || b.Reason == "" {
		t.Fatalf("%+v", b)
	}
}

func TestResumeCostUnavailableCases(t *testing.T) {
	base := claudeRec(claudeSession, &history.TokenUsage{Input: 2381, Output: 6}, f(.004822))
	for _, tc := range []struct {
		name, line string
		baseline   *history.Record
		wantReason string
	}{
		{"absent baseline", measuredResume, nil, "no earlier record"},
		{"legacy baseline", measuredResume, &history.Record{Session: claudeSession, Backend: "claude", CostUSD: f(.004822), CostIsEstimate: true}, "no valid cumulative usage"},
		{"intervening activity", strings.Replace(measuredResume, `"inputTokens":4812`, `"inputTokens":9000`, 1), &base, "does not match"},
		{"usage field mismatch", strings.Replace(measuredResume, `"outputTokens":12`, `"outputTokens":13`, 1), &base, "does not match"},
		{"cache mismatch", strings.Replace(measuredResume, `"cacheReadInputTokens":0,"cacheCreationInputTokens":0,"costUSD":0.009744`, `"cacheReadInputTokens":7,"cacheCreationInputTokens":0,"costUSD":0.009744`, 1), &base, "does not match"},
		{"cost regression", strings.Replace(measuredResume, `"total_cost_usd":0.009744`, `"total_cost_usd":0.001`, 1), &base, "lower than the baseline"},
		{"invalid cost", strings.Replace(measuredResume, `"total_cost_usd":0.009744`, `"total_cost_usd":-1`, 1), &base, "no valid total_cost_usd"},
		{"missing cost", strings.Replace(measuredResume, `,"total_cost_usd":0.009744`, ``, 1), &base, "no valid total_cost_usd"},
		{"missing cumulative usage", strings.Replace(measuredResume, `"modelUsage"`, `"modelUsageX"`, 1), &base, "session totals unavailable"},
		{"missing per-run usage", strings.Replace(measuredResume, `"usage"`, `"usageX"`, 1), &base, "per-run usage unavailable"},
		{"regressed counters", strings.Replace(measuredResume, `"inputTokens":4812`, `"inputTokens":100`, 1), &base, "does not match"},
		{"different session", strings.Replace(measuredResume, `"session_id":"`+claudeSession, `"session_id":"other`, 1), &base, "differs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateEnv(t)
			if tc.baseline != nil {
				appendRec(t, *tc.baseline)
			}
			rec := runFake(t, tc.line, claudeSession)
			if rec.CostUSD != nil || !strings.Contains(rec.CostUnavailable, tc.wantReason) {
				t.Fatalf("cost=%v reason=%q", rec.CostUSD, rec.CostUnavailable)
			}
			if tc.name == "invalid cost" && rec.CumulativeCostUSD != nil {
				t.Fatal("invalid cumulative cost stored")
			}
			if tc.name == "intervening activity" && (rec.Usage == nil || rec.CumulativeUsage == nil || !near(rec.CumulativeCostUSD, .009744)) {
				t.Fatalf("snapshots must still be stored: %+v", rec)
			}
		})
	}
}

func TestFreshRunWithBadAccounting(t *testing.T) {
	stateEnv(t)
	bad := strings.Replace(measuredFresh, `"input_tokens":2381`, `"input_tokens":-5`, 1)
	rec := runFake(t, bad, "")
	if rec.Usage != nil || rec.UsageUnavailable == "" || rec.Status != "ok" {
		t.Fatalf("%+v", rec)
	}
	if !near(rec.CostUSD, .004822) { // cost is independent of usage on a fresh run
		t.Fatalf("%v", rec.CostUSD)
	}
	rec = runFake(t, strings.Replace(measuredFresh, `"usage"`, `"usageX"`, 1), "")
	if rec.Usage != nil || rec.UsageUnavailable != reasonNoUsage {
		t.Fatalf("%+v", rec)
	}
	rec = runFake(t, strings.Replace(measuredFresh, `,"total_cost_usd":0.004822`, ``, 1), "")
	if rec.CostUSD != nil || rec.CostUnavailable == "" || rec.Usage == nil || rec.CumulativeCostUSD != nil {
		t.Fatalf("%+v", rec)
	}
}

func TestNoResultLeavesAccountingUnavailable(t *testing.T) {
	stateEnv(t)
	t.Setenv("OFFLOAD_CLAUDE", fakeClaude(t))
	t.Setenv("FAKE_SALVAGE_MODE", "noresult")
	mockPreflight(t)
	rec, err := Run(context.Background(), testOpts(t))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Usage != nil || rec.CostUSD != nil || rec.CumulativeUsage != nil || rec.CumulativeCostUSD != nil ||
		rec.UsageUnavailable != reasonNoResult || rec.CostUnavailable != reasonNoResult || rec.Salvaged {
		t.Fatalf("%+v", rec)
	}
}

func TestSalvageIsPartialNotFullRun(t *testing.T) {
	stateEnv(t)
	t.Setenv("OFFLOAD_CLAUDE", fakeClaude(t))
	t.Setenv("FAKE_SALVAGE_MODE", "ok")
	mockPreflight(t)
	rec, err := Run(context.Background(), testOpts(t))
	if err != nil || !rec.Salvaged {
		t.Fatalf("%v %+v", err, rec)
	}
	if !near(rec.CostUSD, .02) { // existing recorded salvage cost is preserved
		t.Fatalf("%v", rec.CostUSD)
	}
	if rec.Usage != nil || rec.UsageScope != "report_step" || !strings.Contains(rec.UsageUnavailable, "full-run usage unavailable") ||
		!strings.Contains(rec.Error, "not a full-run cost") {
		t.Fatalf("%+v", rec)
	}
	// The stats code must not count it as a full run.
	now := time.Now()
	rec.TS = now.Add(-time.Minute).Format("2006-01-02 15:04:05")
	st, err := history.Aggregate([]history.Record{rec}, history.PeriodAll, now)
	if err != nil || st.Totals.CostUSD != nil || st.Totals.Usage != nil || st.Totals.Salvaged != 1 {
		t.Fatalf("salvage counted as a full run: %v %+v", err, st.Totals)
	}
}

func TestCodexAccountingUnaffectedByClaudeFields(t *testing.T) {
	// A Codex terminal usage keeps its own decoder and the Claude raw fields stay empty.
	s := parseCodexStream(usageStream(freshUsage))
	if s.Result == nil || s.Result.RawUsage != nil || s.Result.RawModelUsage != nil || s.Result.CumulativeUsage == nil {
		t.Fatalf("%+v", s.Result)
	}
	var rec history.Record
	o := Opts{Model: "gpt-6.1-sol", Profile: config.Profile{Backend: "codex", ServiceTier: "default"}}
	applyCodexAccounting(&rec, s, &history.TokenUsage{}, "", o, true)
	if rec.CumulativeCostUSD != nil || rec.UsageSource != codexUsageSource || rec.Usage == nil {
		t.Fatalf("%+v", rec)
	}
	markClaudeNoResult(&history.Record{Backend: "codex", UsageSource: "keep"})
	r := history.Record{Backend: "codex", UsageSource: "keep"}
	markClaudeNoResult(&r)
	if r.UsageSource != "keep" || r.UsageUnavailable != "" {
		t.Fatalf("%+v", r)
	}
}
