package worker

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
)

const usageSession = "01234567-89ab-cdef-0123-456789abcdef"
const freshUsage = `{"input_tokens":28594,"cached_input_tokens":13184,"cache_write_input_tokens":0,"output_tokens":5,"reasoning_output_tokens":0}`
const resumedUsage = `{"input_tokens":57208,"cached_input_tokens":41600,"cache_write_input_tokens":0,"output_tokens":10,"reasoning_output_tokens":0}`

func usageStream(usage string) []byte {
	return []byte(`{"type":"thread.started","thread_id":"` + usageSession + `"}
{"type":"item.completed","item":{"id":"msg","type":"agent_message","text":"REPORT"}}
{"type":"turn.completed","usage":` + usage + `}`)
}

func TestDecodeCodexUsage(t *testing.T) {
	valid := `{"input_tokens":0,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":0}`
	for _, raw := range []string{valid, freshUsage, resumedUsage} {
		if decodeCodexUsage(json.RawMessage(raw)) == nil {
			t.Fatal(raw)
		}
	}
	for _, raw := range []string{
		`null`, `{}`, `{"input_tokens":0,"cached_input_tokens":0,"output_tokens":0}`,
		strings.Replace(valid, `"input_tokens":0`, `"input_tokens":null`, 1),
		strings.Replace(valid, `"input_tokens":0`, `"input_tokens":-1`, 1),
		strings.Replace(valid, `"input_tokens":0`, `"input_tokens":1.5`, 1),
		strings.Replace(valid, `"input_tokens":0`, `"input_tokens":"0"`, 1),
		strings.Replace(valid, `"output_tokens":0`, `"output_tokens":9223372036854775808`, 1),
		strings.Replace(valid, `"cached_input_tokens":0`, `"cached_input_tokens":1`, 1),
		`{"input_tokens":9223372036854775807,"cached_input_tokens":9223372036854775807,"cache_write_input_tokens":1,"output_tokens":0}`,
	} {
		if decodeCodexUsage(json.RawMessage(raw)) != nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func writeBaseline(t *testing.T, content string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	dir := filepath.Join(home, "sessions", "2026", "10", "26")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-date-"+usageSession+".jsonl")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func baselineLog(usage string) string {
	return `{"type":"session_meta","payload":{"id":"` + usageSession + `"}}
{"type":"response_item","payload":{"text":"not exposed"}}
{"type":"event_msg","payload":{"type":"token_count","info":null}}
{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":` + usage + `}}}` + "\n"
}

func TestCodexBaselineAndResumeDelta(t *testing.T) {
	// The later native snapshot replaces the earlier one; snapshots are not summed.
	writeBaseline(t, baselineLog(`{"input_tokens":1,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":1}`)+baselineLog(freshUsage))
	baseline, reason := codexBaseline(usageSession)
	if baseline == nil || reason != "" {
		t.Fatalf("%+v %s", baseline, reason)
	}
	o := Opts{Resume: usageSession, Model: "gpt-6.1-sol", Profile: config.Profile{ServiceTier: "default"}}
	var rec history.Record
	applyCodexAccounting(&rec, parseCodexStream(usageStream(resumedUsage)), baseline, reason, o, true)
	want := history.TokenUsage{Input: 28614, Cached: 28416, Output: 5}
	if rec.Usage == nil || *rec.Usage != want || rec.CumulativeUsage.Input != 57208 || rec.CostUSD == nil {
		t.Fatalf("%+v", rec)
	}
	if math.Abs(*rec.CostUSD-(198*2+28416*.1+5*10)/1e6) > 1e-12 {
		t.Fatal(*rec.CostUSD)
	}
}

func TestCodexBaselineRejectsCorruption(t *testing.T) {
	for name, content := range map[string]string{
		"wrong identity":   strings.Replace(baselineLog(freshUsage), usageSession, "wrong", 1),
		"truncated":        baselineLog(freshUsage) + `{"type":`,
		"invalid snapshot": baselineLog(`{"input_tokens":1}`),
		"no snapshot":      `{"type":"session_meta","payload":{"id":"` + usageSession + `"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			writeBaseline(t, content)
			if u, r := codexBaseline(usageSession); u != nil || r == "" {
				t.Fatalf("%+v %s", u, r)
			}
		})
	}
	t.Setenv("CODEX_HOME", t.TempDir())
	for _, id := range []string{usageSession, "../../escape", "not-a-uuid"} {
		if u, r := codexBaseline(id); u != nil || r == "" {
			t.Fatalf("%s: %+v %s", id, u, r)
		}
	}
}

func TestCodexAccountingAvailabilityAndLatest(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	o := Opts{Model: "gpt-6.1-sol", Profile: config.Profile{ServiceTier: "default"}}
	zero := `{"input_tokens":0,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":0}`
	for _, tc := range []struct {
		name      string
		stream    []byte
		baseline  *history.TokenUsage
		completed bool
		available bool
	}{
		{"zero", usageStream(zero), &history.TokenUsage{}, true, true},
		{"nonzero", usageStream(freshUsage), &history.TokenUsage{}, true, true},
		{"latest", append(usageStream(freshUsage), append([]byte("\n"), []byte(`{"type":"turn.completed","usage":`+resumedUsage+`}`)...)...), &history.TokenUsage{}, true, true},
		{"missing", usageStream(`null`), &history.TokenUsage{}, true, false},
		{"malformed", usageStream(`{"input_tokens":-1}`), &history.TokenUsage{}, true, false},
		{"baseline missing", usageStream(resumedUsage), nil, true, false},
		{"regressed", usageStream(freshUsage), decodeCodexUsage(json.RawMessage(resumedUsage)), true, false},
		{"invalid delta subset", usageStream(freshUsage), &history.TokenUsage{Input: 28000, Cached: 0}, true, false},
		{"interrupted", usageStream(freshUsage), &history.TokenUsage{}, false, false},
		{"unfinished next turn", append(usageStream(freshUsage), []byte("\n"+`{"type":"turn.started"}`)...), &history.TokenUsage{}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var r history.Record
			applyCodexAccounting(&r, parseCodexStream(tc.stream), tc.baseline, "resume baseline missing or corrupt", o, tc.completed)
			if (r.Usage != nil) != tc.available || (r.CostUSD != nil) != tc.available {
				t.Fatalf("%+v", r)
			}
			if !tc.available && (r.CostUnavailable == "" || r.UsageUnavailable == "") {
				t.Fatalf("missing reason: %+v", r)
			}
			if tc.name == "latest" && r.Usage.Input != 57208 {
				t.Fatal(r.Usage)
			}
			if tc.name == "zero" && *r.CostUSD != 0 {
				t.Fatal(r.CostUSD)
			}
		})
	}
}

func TestCodexReferencePricing(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	for _, tier := range []string{"default", "priority", "", "unknown"} {
		for _, model := range []string{"gpt-6.1-sol", "", "other", "gpt-6.1-sol-variant"} {
			t.Run(model+"/"+tier, func(t *testing.T) {
				o := Opts{Model: model, Profile: config.Profile{ServiceTier: tier}}
				var r history.Record
				stream := usageStream(`{"input_tokens":1000,"cached_input_tokens":200,"cache_write_input_tokens":100,"output_tokens":50,"reasoning_output_tokens":20}`)
				applyCodexAccounting(&r, parseCodexStream(stream), &history.TokenUsage{}, "", o, true)
				if r.Usage == nil {
					t.Fatal("tokens lost")
				}
				available := model == "gpt-6.1-sol" && (tier == "default" || tier == "priority")
				if (r.CostUSD != nil) != available {
					t.Fatalf("%+v", r)
				}
				if !available {
					if r.CostUnavailable == "" {
						t.Fatal("no reason")
					}
					return
				}
				want := (700*2 + 200*.1 + 100*2.5 + 50*10) / 1e6
				if tier == "priority" {
					want *= 2
				}
				if math.Abs(*r.CostUSD-want) > 1e-12 {
					t.Fatalf("%g != %g", *r.CostUSD, want)
				}
				if r.Pricing.Model != model || r.Pricing.Tier != tier || r.Pricing.Date != "2026-10-26" || !strings.Contains(r.Pricing.Assumptions, "30m") {
					t.Fatal(fmt.Sprintf("%+v", r.Pricing))
				}
			})
		}
	}
}
