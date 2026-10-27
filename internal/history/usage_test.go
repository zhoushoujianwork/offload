package history

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestUsageAndPricingHistoryRoundtrip(t *testing.T) {
	for _, cost := range []*float64{nil, ptr(0), ptr(.0032876)} {
		r := Record{Backend: "codex", Session: "session", CostUSD: cost, CostIsEstimate: cost != nil,
			Usage: &TokenUsage{Input: 28614, Cached: 28416, Output: 5}, CumulativeUsage: &TokenUsage{Input: 57208, Cached: 41600, Output: 10},
			UsageSource: "terminal", UsageScope: "run", CostBasis: "api_baseline",
			Pricing: &PricingSnapshot{Model: "gpt-6.1-sol", Tier: "priority", Currency: "USD", PerTokens: 1000000, Input: 4, Cached: .2, CacheWrite: 5, Output: 20, Source: "reference", Date: "2026-10-26", Assumptions: "frozen assumptions"}}
		if cost == nil {
			r.CostUnavailable = "unsupported rates"
		}
		path := filepath.Join(t.TempDir(), "runs.jsonl")
		if err := Append(path, r); err != nil {
			t.Fatal(err)
		}
		rs, err := Read(path)
		if err != nil || len(rs) != 1 {
			t.Fatalf("%v %+v", err, rs)
		}
		before, _ := json.Marshal(r)
		after, _ := json.Marshal(rs[0])
		if string(before) != string(after) {
			t.Fatalf("roundtrip changed snapshot: %s != %s", before, after)
		}
	}
}
