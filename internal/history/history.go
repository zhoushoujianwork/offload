// Package history stores one record per run so reports survive the supervising session.
package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
)

// Record is one line of the run log.
type Record struct {
	RunID            string           `json:"run_id,omitempty"`
	CallerRunID      string           `json:"caller_run_id,omitempty"`
	ExecutionPolicy  string           `json:"execution_policy,omitempty"`
	ErrorKind        string           `json:"error_kind,omitempty"`
	TS               string           `json:"ts"`
	Status           string           `json:"status"` // ok | error | timeout
	ExitCode         *int             `json:"exit_code"`
	Profile          string           `json:"profile"`
	Backend          string           `json:"backend,omitempty"`
	Model            string           `json:"model"`
	ServiceTier      string           `json:"service_tier,omitempty"`
	Dir              string           `json:"dir"`
	Task             string           `json:"task"`
	Session          string           `json:"session"`
	Turns            int              `json:"turns"`                        // for an interrupted run: tool calls made
	Denials          int              `json:"permission_denials,omitempty"` // tool calls the worker was refused
	CostUSD          *float64         `json:"cost_usd"`                     // worker estimate, never verified billing; nil means unavailable
	CostIsEstimate   bool             `json:"cost_is_estimate"`
	Usage            *TokenUsage      `json:"usage,omitempty"`
	CumulativeUsage  *TokenUsage      `json:"cumulative_usage,omitempty"`
	UsageSource      string           `json:"usage_source,omitempty"`
	UsageScope       string           `json:"usage_scope,omitempty"`
	UsageUnavailable string           `json:"usage_unavailable,omitempty"`
	CostUnavailable  string           `json:"cost_unavailable,omitempty"`
	CostBasis        string           `json:"cost_basis,omitempty"`
	Pricing          *PricingSnapshot `json:"pricing,omitempty"`
	// CostNote explains why CostUSD is nil. Read derives it for display only; it is never persisted.
	CostNote   string `json:"-"`
	DurationMs int64  `json:"duration_ms"` // duration reported by the worker (legacy field)
	ElapsedMs  *int64 `json:"elapsed_ms"`  // measured end-to-end, including setup; nil for older records
	Result     string `json:"result"`
	Error      string `json:"error,omitempty"`
	Salvaged   bool   `json:"salvaged,omitempty"` // Result is a report written after the worker was stopped
}

// Path is the run log location.
func Path() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "offload", "runs.jsonl")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "offload", "runs.jsonl")
}

// Append adds one record to the log.
func Append(path string, r Record) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// Read returns all records in file order; a missing log is empty, not an error.
func Read(path string) ([]Record, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rs []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			normalizeCost(&r)
			rs = append(rs, r)
		}
	}
	return rs, sc.Err()
}

// Reasons a stored cost is unavailable, shown by `offload ls`.
const (
	CostNoteLegacyZero = "legacy zero is ambiguous"
	CostNoteInvalid    = "invalid value recorded"
	CostNoteCodex      = "Codex does not report USD cost"
	CostNoteNotReport  = "worker did not report cost"
)

// normalizeCost keeps genuine values and records why a missing one is missing.
// Old versions serialized a missing cost as 0 without cost_is_estimate, so an
// unmarked zero is ambiguous and is treated as unavailable rather than as $0.
func normalizeCost(r *Record) {
	r.CostNote = ""
	switch {
	case r.CostUSD != nil && !ValidCost(*r.CostUSD):
		r.CostUSD, r.CostNote = nil, CostNoteInvalid
	case r.CostUSD != nil && !r.CostIsEstimate && *r.CostUSD == 0:
		r.CostUSD, r.CostNote = nil, CostNoteLegacyZero
	case r.CostUSD == nil && r.Backend == "codex":
		r.CostNote = firstCostReason(r.CostUnavailable, CostNoteCodex)
	case r.CostUSD == nil:
		r.CostNote = CostNoteNotReport
	}
	// Every stored cost is a worker estimate; a missing cost is not an estimate of anything.
	r.CostIsEstimate = r.CostUSD != nil
}

// BySession filters records to one worker session.
func BySession(rs []Record, session string) []Record {
	var out []Record
	for _, r := range rs {
		if r.Session == session {
			out = append(out, r)
		}
	}
	return out
}

// ValidCost reports whether a worker-reported cost can be trusted as a number.
func ValidCost(cost float64) bool {
	return cost >= 0 && !math.IsNaN(cost) && !math.IsInf(cost, 0)
}
