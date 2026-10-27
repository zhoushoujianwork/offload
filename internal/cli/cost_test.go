package cli

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
)

func TestCodexResumeUsageCLI(t *testing.T) {
	const session = "01234567-89ab-cdef-0123-456789abcdef"
	const baseline = `{"input_tokens":28594,"cached_input_tokens":13184,"cache_write_input_tokens":0,"output_tokens":5,"reasoning_output_tokens":0}`
	const final = `{"input_tokens":57208,"cached_input_tokens":41600,"cache_write_input_tokens":0,"output_tokens":10,"reasoning_output_tokens":0}`
	reply := `{"type":"thread.started","thread_id":"` + session + `"}
{"type":"item.completed","item":{"id":"msg","type":"agent_message","text":"CODEX REPORT"}}
{"type":"turn.completed","usage":` + final + `}`
	work, _ := setupCodex(t, reply)
	cfg, err := config.Load(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Profiles["codex"]
	p.Model, p.ServiceTier = "gpt-6.1-sol", "default"
	if err := config.SaveProfile(config.Path(), "codex", p, false); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	dir := filepath.Join(home, "sessions", "2026", "10", "26")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	log := func(usage string) string {
		return `{"type":"session_meta","payload":{"id":"` + session + `"}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":` + usage + `}}}` + "\n"
	}
	rollout := filepath.Join(dir, "rollout-date-"+session+".jsonl")
	updated := filepath.Join(home, "updated.jsonl")
	for path, contents := range map[string]string{rollout: log(baseline), updated: log(final)} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// The fake native process updates its rollout before emitting stdout. This
	// proves offload captured the old baseline before starting the process.
	fake := os.Getenv("OFFLOAD_CODEX")
	script, err := os.ReadFile(fake)
	if err != nil {
		t.Fatal(err)
	}
	script = []byte(strings.Replace(string(script), "cat <<'EOF'", "cp \"$FAKE_UPDATED_ROLLOUT\" \"$FAKE_ROLLOUT\"\ncat <<'EOF'", 1))
	if err := os.WriteFile(fake, script, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_UPDATED_ROLLOUT", updated)
	t.Setenv("FAKE_ROLLOUT", rollout)
	code, out, stderr := runCLI("-json", "-p", "codex", "-r", session, "-d", work, "inspect")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	r := decodeRun(t, out)
	want := history.TokenUsage{Input: 28614, Cached: 28416, Output: 5}
	if r.Usage == nil || *r.Usage != want || r.CumulativeUsage == nil || r.CumulativeUsage.Input != 57208 || r.Pricing == nil || r.CostUSD == nil {
		t.Fatalf("%+v", r)
	}
	if math.Abs(*r.CostUSD-.0032876) > 1e-12 {
		t.Fatal(*r.CostUSD)
	}
	if !strings.Contains(stderr, "estimated_cost=API baseline est. $") {
		t.Fatal(stderr)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"usage", "cumulative_usage", "usage_source", "usage_scope", "cost_basis", "pricing"} {
		if raw[key] == nil {
			t.Fatal("missing " + key)
		}
	}
	_, shown, _ := runCLI("show", session)
	for _, text := range []string{"tokens in=28614 cached=28416 write=0 out=5", "Session cumulative tokens: input=57208", "gpt-6.1-sol / default", "input=2 cached=0.1 cache_write=2.5 output=10", "2026-10-26", "https://developers.openai.com/api/docs/models/gpt-6.1-sol", "30m", "subscription"} {
		if !strings.Contains(shown, text) {
			t.Fatalf("missing %q: %s", text, shown)
		}
	}
	appendListRecord(t, history.Record{Session: "old-codex", Backend: "codex"})
	_, listed, _ := runCLI("ls")
	if strings.Count(listed, "N/A: see offload show") != 1 || !strings.Contains(listed, "API baseline est. $") || !strings.Contains(listed, "tokens in=28614") || !strings.Contains(listed, " | N/A\n") {
		t.Fatal(listed)
	}
	_, missing, _ := runCLI("show", "old-codex")
	if !strings.Contains(missing, "Cost unavailable: Codex does not report USD cost") {
		t.Fatal(missing)
	}
	// A native log created only after launch cannot supply a resume baseline.
	if err := os.Remove(rollout); err != nil {
		t.Fatal(err)
	}
	code, out, stderr = runCLI("-json", "-p", "codex", "-r", session, "-d", work, "inspect again")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	r = decodeRun(t, out)
	if r.Usage != nil || r.CostUSD != nil || r.CumulativeUsage == nil || !strings.Contains(r.CostUnavailable, "baseline") || strings.Contains(stderr, "$") {
		t.Fatalf("missing baseline guessed usage/cost: %+v %s", r, stderr)
	}
}
