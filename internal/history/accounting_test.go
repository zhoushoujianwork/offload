package history

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAccountingOverlayPreservesOriginalAndStatsParity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	r := Record{TS: "2026-10-29 10:00:00", Session: "s", Backend: "claude", Task: "keep task", Result: "keep report"}
	if err := Append(path, r); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	u := &TokenUsage{Input: 100, Cached: 40, Output: 10}
	cost := .25
	r.Usage, r.CumulativeUsage = u, u
	r.CostUSD, r.CumulativeCostUSD = &cost, &cost
	r.UsageSource, r.UsageScope, r.AccountingEvidence = "native_claude_session", "run", "sha256:evidence"
	p := NewAccountingPatch(bytes.TrimSuffix(before, []byte{'\n'}), 1, r)
	for range 2 {
		if err := AppendAccountingPatch(path, p); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("original was changed")
	}
	read, err := Read(path)
	if err != nil || len(read) != 1 {
		t.Fatalf("%v %+v", err, read)
	}
	stats, err := ReadStats(path, time.Date(2026, 10, 30, 0, 0, 0, 0, time.Local))
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Records) != 1 || stats.Records[0].Usage.Input != 100 || *stats.Records[0].CostUSD != .25 || stats.Records[0].Result != "keep report" {
		t.Fatalf("%+v", stats)
	}
	if read[0].Task != "keep task" || read[0].Result != "keep report" || read[0].Usage.Input != 100 {
		t.Fatalf("%+v", read)
	}
	wrong := p
	wrong.Fingerprint = AccountingFingerprint([]byte("different raw record"))
	wrong.Usage = &TokenUsage{Input: 999}
	if err := AppendAccountingPatch(path, wrong); err != nil {
		t.Fatal(err)
	}
	read, _ = Read(path)
	if read[0].Usage.Input != 100 {
		t.Fatal("correction escaped fingerprint binding")
	}
	// No prompt or report is copied into the sidecar.
	raw, _ := os.ReadFile(path + ".accounting.jsonl")
	if bytes.Contains(raw, []byte("keep task")) || bytes.Contains(raw, []byte("keep report")) {
		t.Fatal("sidecar contains user text")
	}
}

func TestPartialAccountingNeverBecomesAFullRunTotal(t *testing.T) {
	r := Record{TS: time.Now().Format(time.RFC3339), Backend: "codex", Usage: &TokenUsage{Input: 100}, UsageScope: "partial_run"}
	tot, err := Aggregate([]Record{r}, PeriodAll, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if tot.Totals.Usage != nil {
		t.Fatal("partial usage entered full total")
	}
	if tot.Totals.PartialUsage == nil || tot.Totals.PartialUsage.Input != 100 || tot.Totals.PartialUsageRuns != 1 {
		t.Fatal("partial usage not exposed separately")
	}
	p := AccountingPatch{Fingerprint: "bad", Usage: r.Usage, Scope: "run"}
	if err := AppendAccountingPatch(filepath.Join(t.TempDir(), "runs.jsonl"), p); err == nil {
		t.Fatal("accepted invalid correction")
	}
}
