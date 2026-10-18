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
	TS             string   `json:"ts"`
	Status         string   `json:"status"` // ok | error | timeout
	ExitCode       *int     `json:"exit_code"`
	Profile        string   `json:"profile"`
	Backend        string   `json:"backend,omitempty"`
	Model          string   `json:"model"`
	Dir            string   `json:"dir"`
	Task           string   `json:"task"`
	Session        string   `json:"session"`
	Turns          int      `json:"turns"`                        // for an interrupted run: tool calls made
	Denials        int      `json:"permission_denials,omitempty"` // tool calls the worker was refused
	CostUSD        *float64 `json:"cost_usd"`                     // worker estimate, never verified billing; nil means unavailable
	CostIsEstimate bool     `json:"cost_is_estimate"`
	DurationMs     int64    `json:"duration_ms"` // duration reported by the worker (legacy field)
	ElapsedMs      *int64   `json:"elapsed_ms"`  // measured end-to-end, including setup; nil for older records
	Result         string   `json:"result"`
	Error          string   `json:"error,omitempty"`
	Salvaged       bool     `json:"salvaged,omitempty"` // Result is a report written after the worker was stopped
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
			// Old versions serialized missing costs as zero. Without the new marker,
			// an old zero is ambiguous and must not be presented as a known estimate.
			if r.CostUSD != nil && (!ValidCost(*r.CostUSD) || (!r.CostIsEstimate && *r.CostUSD == 0)) {
				r.CostUSD = nil
			}
			r.CostIsEstimate = true
			rs = append(rs, r)
		}
	}
	return rs, sc.Err()
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
