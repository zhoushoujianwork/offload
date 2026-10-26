package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadNormalizesCost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	fixtures := `{"session":"available","cost_usd":0.01,"cost_is_estimate":true}
{"session":"zero","cost_usd":0,"cost_is_estimate":true}
{"session":"missing","cost_usd":null,"cost_is_estimate":true}
{"session":"legacy-nonzero","cost_usd":0.25}
{"session":"legacy-zero","cost_usd":0}
{"session":"codex","backend":"codex","cost_usd":null}
{"session":"invalid","cost_usd":-2,"cost_is_estimate":true}
`
	if err := os.WriteFile(path, []byte(fixtures), 0o600); err != nil {
		t.Fatal(err)
	}
	rs, err := Read(path)
	if err != nil || len(rs) != 7 {
		t.Fatalf("read: %v len=%d", err, len(rs))
	}
	for i, want := range []struct {
		cost     *float64
		estimate bool
		note     string
	}{
		{ptr(0.01), true, ""},
		{ptr(0), true, ""},
		{nil, false, CostNoteNotReport},
		{ptr(0.25), true, ""},
		{nil, false, CostNoteLegacyZero},
		{nil, false, CostNoteCodex},
		{nil, false, CostNoteInvalid},
	} {
		r := rs[i]
		if (r.CostUSD == nil) != (want.cost == nil) || (want.cost != nil && *r.CostUSD != *want.cost) {
			t.Errorf("%s: cost=%v want %v", r.Session, r.CostUSD, want.cost)
		}
		if r.CostIsEstimate != want.estimate || r.CostNote != want.note {
			t.Errorf("%s: estimate=%v note=%q, want %v %q", r.Session, r.CostIsEstimate, r.CostNote, want.estimate, want.note)
		}
	}
}

// The explanation is display-only: it must not leak into persisted or -json records,
// and unavailable cost must stay null rather than becoming 0.
func TestCostNoteNotSerialized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	if err := Append(path, Record{Session: "s", CostNote: CostNoteCodex}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "Note") || strings.Contains(string(b), "note") || !strings.Contains(string(b), `"cost_usd":null`) {
		t.Fatalf("unexpected record: %s", b)
	}
}

func ptr(v float64) *float64 { return &v }
