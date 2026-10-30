package history

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSubscriptionHistoryAPIEquivalent(t *testing.T) {
	r := Record{TS: "2026-10-30 10:00:00", Backend: "codex", Model: "gpt-6.1-sol", ServiceTier: "priority", CostBasis: "subscription", UsageScope: "run", Usage: &TokenUsage{Input: 1000, Cached: 200, CacheWrite: 100, Output: 50}, UsageUnavailable: "stale"}
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	b, _ := json.Marshal(r)
	if err := os.WriteFile(path, append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	records, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	streamed, streamErr := ReadStats(path, time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC))
	if streamErr != nil || len(streamed.Records) != 1 || streamed.Records[0].CostUSD == nil {
		t.Fatalf("stats enrichment failed: %+v %v", streamed, streamErr)
	}
	got := records[0]
	want := 2 * (700*2 + 200*.1 + 100*2.5 + 50*10) / 1e6
	if got.CostUSD == nil || math.Abs(*got.CostUSD-want) > 1e-12 || got.CostBasis != "api_baseline" || got.UsageUnavailable != "" {
		t.Fatalf("%+v", got)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(append(b, '\n')) {
		t.Fatal("read modified history")
	}
	for _, change := range []func(*Record){func(r *Record) { r.Model = "other" }, func(r *Record) { r.ServiceTier = "" }, func(r *Record) { r.Salvaged = true }, func(r *Record) { r.Usage = nil }, func(r *Record) { r.Usage = &TokenUsage{Input: 1, Cached: 2} }} {
		candidate := r
		change(&candidate)
		EstimateCodexCost(&candidate)
		if candidate.CostUSD != nil {
			t.Fatal("priced unsupported record")
		}
	}
}
