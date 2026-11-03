package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
)

func TestAccountingRepairDryRunApplyAndReplay(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	const session = "01234567-89ab-cdef-0123-456789abcdef"
	ms := int64(6000)
	r := history.Record{Backend: "codex", TS: "2026-10-29T10:00:00Z", Session: session, Dir: "/project", Task: "private task", Result: "private report", ElapsedMs: &ms}
	if err := history.Append(history.Path(), r); err != nil {
		t.Fatal(err)
	}
	native := `{"type":"session_meta","payload":{"id":"` + session + `"}}
{"type":"event_msg","timestamp":"2026-10-29T10:00:01Z","payload":{"type":"task_started","turn_id":"t"}}
{"type":"turn_context","payload":{"turn_id":"t","cwd":"/project","model":"gpt-6.1-sol"}}
{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"private task"}]}}
{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"service_tier":"default"}}}
{"type":"token_usage_record","timestamp":"2026-10-29T10:00:02Z","payload":{"turn_id":"t","turn_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10},"thread_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":10}}}
{"type":"event_msg","timestamp":"2026-10-29T10:00:03Z","payload":{"type":"task_complete","turn_id":"t"}}
`
	dir := filepath.Join(home, "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-"+session+".jsonl"), []byte(native), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(history.Path())
	for _, apply := range []bool{false, true, true} {
		args := []string{"accounting", "repair", "--json"}
		if apply {
			args = append(args, "--apply")
		}
		code, out, stderr := runCLI(args...)
		if code != 0 {
			t.Fatalf("%d %s", code, stderr)
		}
		if bytes.Contains([]byte(out), []byte("private task")) || bytes.Contains([]byte(out), []byte("private report")) {
			t.Fatal("repair leaked source text")
		}
		var result accountingRepairResult
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatal(err)
		}
		if !apply {
			if result.UsageAdded != 1 || result.CostAdded != 1 {
				t.Fatalf("%+v", result)
			}
			if _, err := os.Stat(history.Path() + ".accounting.jsonl"); !os.IsNotExist(err) {
				t.Fatal("dry run wrote data")
			}
		}
	}
	after, _ := os.ReadFile(history.Path())
	if !bytes.Equal(before, after) {
		t.Fatal("original log changed")
	}
	patches, _ := os.ReadFile(history.Path() + ".accounting.jsonl")
	if bytes.Count(patches, []byte{'\n'}) != 1 {
		t.Fatal("replay duplicated correction")
	}
	stats, err := history.ReadStats(history.Path(), time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Records[0].Usage.Input != 100 || stats.Records[0].CostUSD == nil || stats.Records[0].AccountingEvidence == "" {
		t.Fatalf("%+v", stats)
	}
	// Even identical legacy JSON at a different line must not inherit the first
	// invocation's correction or claim the same native turn a second time.
	if err := history.Append(history.Path(), r); err != nil {
		t.Fatal(err)
	}
	result, err := repairAccounting(history.Path(), true)
	if err != nil || result.RecoveredRuns != 0 || result.Unresolved["overlapping invocations with the same session and prompt"] != 1 {
		t.Fatalf("%v %+v", err, result)
	}
	read, err := history.Read(history.Path())
	if err != nil || read[0].Usage == nil || read[1].Usage != nil {
		t.Fatal("one native turn was attributed twice")
	}
}
