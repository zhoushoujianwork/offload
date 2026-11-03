package nativeusage

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
)

const testSession = "01234567-89ab-cdef-0123-456789abcdef"

var epoch = time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)

func fixtureRecord() history.Record {
	ms := int64(6000)
	return history.Record{TS: epoch.Format(time.RFC3339), Session: testSession, Dir: "/project", Task: "任务", ElapsedMs: &ms}
}

func writeNative(t *testing.T, backend string, events []map[string]any) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "projects", "project")
	name := testSession + ".jsonl"
	if backend == "codex" {
		t.Setenv("CODEX_HOME", root)
		dir = filepath.Join(root, "sessions")
		name = "rollout-" + name
	} else {
		t.Setenv("CLAUDE_CONFIG_DIR", root)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var out []byte
	for _, e := range events {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
		out = append(out, '\n')
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, out, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func event(kind string, sec int, payload map[string]any) map[string]any {
	return map[string]any{"type": kind, "timestamp": epoch.Add(time.Duration(sec) * time.Second).Format(time.RFC3339), "payload": payload}
}

func cu(input, cached, output int) map[string]any {
	return map[string]any{"input_tokens": input, "cached_input_tokens": cached, "cache_write_input_tokens": 0, "output_tokens": output}
}

func codexEvents(complete bool) []map[string]any {
	e := []map[string]any{
		event("session_meta", 0, map[string]any{"id": testSession}),
		event("event_msg", 1, map[string]any{"type": "task_started", "turn_id": "turn-1"}),
		event("turn_context", 1, map[string]any{"turn_id": "turn-1", "cwd": "/project", "model": "gpt-6.1-sol"}),
		event("response_item", 1, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"text": "instructions\nTask:\n任务"}}}),
		event("event_msg", 1, map[string]any{"type": "thread_settings_applied", "thread_id": testSession, "thread_settings": map[string]any{"model": "gpt-6.1-sol", "service_tier": "priority"}}),
		event("token_usage_record", 2, map[string]any{"turn_id": "turn-1", "session_id": testSession, "turn_token_usage": cu(50, 20, 5), "thread_token_usage": cu(450, 200, 45)}),
		event("token_usage_record", 3, map[string]any{"turn_id": "turn-1", "session_id": testSession, "turn_token_usage": cu(100, 40, 10), "thread_token_usage": cu(500, 220, 50)}),
	}
	if complete {
		e = append(e, event("event_msg", 4, map[string]any{"type": "task_complete", "turn_id": "turn-1"}))
	}
	return e
}

func TestCodexNativeTurnDoesNotDoubleCountResumeOrRepeatedSnapshots(t *testing.T) {
	writeNative(t, "codex", codexEvents(true))
	r := fixtureRecord()
	r.Backend = "codex"
	got, ok, reason := NewReader().Recover(r)
	if !ok || got.Usage == nil || *got.Usage != (history.TokenUsage{Input: 100, Cached: 40, Output: 10}) {
		t.Fatalf("%s %+v", reason, got)
	}
	if got.CumulativeUsage.Input != 500 || got.ServiceTier != "priority" || got.Model != "gpt-6.1-sol" || got.CostUSD == nil || math.Abs(*got.CostUSD-.000448) > 1e-12 {
		t.Fatalf("%+v", got)
	}
	if !strings.HasPrefix(got.AccountingEvidence, "sha256:") {
		t.Fatal(got.AccountingEvidence)
	}
}

func TestNativeAttributionRejectsWrongIdentityWindowAndConflicts(t *testing.T) {
	for _, name := range []string{"session", "cwd", "prompt", "window", "usage", "corrupt", "ambiguous"} {
		t.Run(name, func(t *testing.T) {
			e := codexEvents(true)
			r := fixtureRecord()
			r.Backend = "codex"
			switch name {
			case "session":
				e[0]["payload"] = map[string]any{"id": "wrong"}
			case "cwd":
				r.Dir = "/different"
			case "prompt":
				r.Task = "different"
			case "window":
				r.TS = epoch.Add(10 * time.Second).Format(time.RFC3339)
			case "usage":
				r.Usage = &history.TokenUsage{Input: 1, Output: 1}
				r.UsageScope = "run"
			case "ambiguous":
				e = append(e, codexEvents(true)[1:]...)
			}
			path := writeNative(t, "codex", e)
			if name == "corrupt" {
				f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				_, _ = f.WriteString("{broken\n")
				_ = f.Close()
			}
			_, ok, _ := NewReader().Recover(r)
			if ok {
				t.Fatal("accepted unverified usage")
			}
		})
	}
}

func TestInterruptedNativeUsageIsOnlyALowerBound(t *testing.T) {
	writeNative(t, "codex", codexEvents(false))
	r := fixtureRecord()
	r.Backend = "codex"
	got, ok, _ := NewReader().Recover(r)
	if !ok || got.UsageScope != "partial_run" || got.CostUSD != nil || got.Usage.Input != 100 {
		t.Fatalf("%+v", got)
	}
	r.Usage = &history.TokenUsage{Input: 100, Cached: 40, Output: 10}
	r.UsageScope = "run"
	if _, ok, _ := NewReader().Recover(r); ok {
		t.Fatal("downgraded measured usage")
	}
}

func claudeEvent(kind string, sec int, fields map[string]any) map[string]any {
	e := map[string]any{"type": kind, "timestamp": epoch.Add(time.Duration(sec) * time.Second).Format(time.RFC3339), "sessionId": testSession, "cwd": "/project"}
	for k, v := range fields {
		e[k] = v
	}
	return e
}

func assistant(id string, sec, input, read, write, output int) map[string]any {
	return claudeEvent("assistant", sec, map[string]any{"message": map[string]any{"id": id, "model": "claude-test", "usage": map[string]any{"input_tokens": input, "cache_read_input_tokens": read, "cache_creation_input_tokens": write, "output_tokens": output}}})
}

func snapshot(sec, input, read, write, output int, cost float64) map[string]any {
	e := claudeEvent("cost-state", sec, map[string]any{"totalCostUSD": cost, "modelUsage": map[string]any{"claude-test": map[string]any{"inputTokens": input, "cacheReadInputTokens": read, "cacheCreationInputTokens": write, "outputTokens": output}}})
	delete(e, "timestamp") // Real cost-state has no timestamp; adjacent events bound it.
	return e
}

func claudeEvents() []map[string]any {
	return []map[string]any{
		claudeEvent("user", 1, map[string]any{"uuid": "prompt-1", "message": map[string]any{"content": "任务"}}),
		assistant("msg-1", 2, 10, 3, 2, 2),
		assistant("msg-1", 3, 10, 3, 2, 5),
		assistant("msg-2", 4, 20, 4, 0, 6),
		snapshot(4, 30, 7, 2, 11, .004),
		claudeEvent("user", 11, map[string]any{"uuid": "prompt-2", "parentUuid": "prior", "message": map[string]any{"content": "续聊"}}),
		assistant("msg-3", 12, 5, 2, 0, 1),
		snapshot(12, 35, 9, 2, 12, .006),
	}
}

func TestClaudeNativeDeduplicatesMessagesAndVerifiesCostDelta(t *testing.T) {
	writeNative(t, "claude", claudeEvents())
	r := fixtureRecord()
	r.Backend = "claude"
	rd := NewReader()
	got, ok, why := rd.Recover(r)
	if !ok || *got.Usage != (history.TokenUsage{Input: 39, Cached: 7, CacheWrite: 2, Output: 11}) || got.CostUSD == nil || *got.CostUSD != .004 {
		t.Fatalf("%s %+v", why, got)
	}
	r.TS = epoch.Add(10 * time.Second).Format(time.RFC3339)
	r.Task = "续聊"
	got, ok, why = rd.Recover(r)
	if !ok || got.Usage.Input != 7 || got.CostUSD == nil || *got.CostUSD != .002 || *got.CumulativeCostUSD != .006 {
		t.Fatalf("%s %+v", why, got)
	}
}

func TestClaudeSnapshotMismatchAndSyntheticErrorsAreNotCounted(t *testing.T) {
	for _, name := range []string{"mismatch", "synthetic", "sidechain", "negative"} {
		t.Run(name, func(t *testing.T) {
			e := claudeEvents()[:5]
			switch name {
			case "mismatch":
				e[4] = snapshot(4, 300, 7, 2, 11, .004)
			case "synthetic":
				for _, v := range e[1:4] {
					v["isApiErrorMessage"] = true
				}
			case "sidechain":
				for _, v := range e {
					v["isSidechain"] = true
				}
			case "negative":
				e[2] = assistant("msg-1", 3, -1, 3, 2, 5)
			}
			writeNative(t, "claude", e)
			r := fixtureRecord()
			r.Backend = "claude"
			if _, ok, _ := NewReader().Recover(r); ok {
				t.Fatal("accepted invalid accounting")
			}
		})
	}
}

func TestCountersRejectOverflowAndNull(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `{"input_tokens":null,"output_tokens":1}`, `{"input_tokens":9223372036854775807,"cache_read_input_tokens":1,"output_tokens":1}`} {
		if counters(json.RawMessage(raw), true) != nil {
			t.Fatal(raw)
		}
	}
}

func TestClaudeStructuredPromptAndScopedHome(t *testing.T) {
	e := claudeEvents()
	e[0]["message"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": "任务"}}}
	tool := claudeEvent("user", 3, map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "content": "result"}}}})
	e = append(e[:3], append([]map[string]any{tool}, e[3:]...)...)
	path := writeNative(t, "claude", e)
	r := fixtureRecord()
	r.Backend = "claude"
	r.ClaudeHome = filepath.Dir(filepath.Dir(filepath.Dir(path)))
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	got, ok, why := NewReader().Recover(r)
	if !ok || got.Usage.Input != 39 || got.CostUSD == nil {
		t.Fatalf("%s %+v", why, got)
	}
}

func TestClaudeLegacyTerminalReportProvesCompletionButNotCost(t *testing.T) {
	e := claudeEvents()[:4]
	e[3]["message"].(map[string]any)["content"] = []any{map[string]any{"type": "text", "text": "terminal report"}}
	writeNative(t, "claude", e)
	r := fixtureRecord()
	r.Backend = "claude"
	r.Status = "ok"
	r.Result = "terminal report"
	got, ok, why := NewReader().Recover(r)
	if !ok || got.UsageScope != "run" || got.Usage.Input != 39 || got.CostUSD != nil {
		t.Fatalf("%s %+v", why, got)
	}
	r.Result = "different report"
	got, ok, _ = NewReader().Recover(r)
	if !ok || got.UsageScope != "partial_run" {
		t.Fatal("missing terminal evidence was promoted")
	}
	cost := .004
	r.CostUSD, r.CumulativeCostUSD = &cost, &cost
	if _, ok, _ := NewReader().Recover(r); ok {
		t.Fatal("partial recovery discarded a verified terminal cost")
	}
}

func TestSameNativePromptCannotBelongToOverlappingHistoryRuns(t *testing.T) {
	a, b := fixtureRecord(), fixtureRecord()
	if !SamePromptOverlap(a, b) {
		t.Fatal("duplicate invocation not rejected")
	}
	b.TS = epoch.Add(10 * time.Second).Format(time.RFC3339)
	if SamePromptOverlap(a, b) {
		t.Fatal("sequential resumes overlap")
	}
	b = fixtureRecord()
	b.Task = "another task"
	if SamePromptOverlap(a, b) {
		t.Fatal("independent prompts overlap")
	}
	b = fixtureRecord()
	a.CodexHome = "/one"
	b.CodexHome = "/two"
	if SamePromptOverlap(a, b) {
		t.Fatal("independent account homes overlap")
	}
}

func TestNativeRecoveryExcludesQueueWait(t *testing.T) {
	writeNative(t, "codex", codexEvents(true))
	r := fixtureRecord()
	r.Backend = "codex"
	wait := int64(5000)
	r.WaitMs = &wait
	if _, ok, _ := NewReader().Recover(r); ok {
		t.Fatal("claimed a native turn that occurred while queued")
	}
}
