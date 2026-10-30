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
	CodexAccount    string       `json:"codex_account,omitempty"`
	CodexHome       string       `json:"codex_home,omitempty"`
	ProfileAlias    string       `json:"profile_alias,omitempty"`
	StopReason      string       `json:"stop_reason,omitempty"`
	Termination     *Termination `json:"termination,omitempty"`
	IdleTimeout     string       `json:"idle_timeout"`
	RunID           string       `json:"run_id,omitempty"`
	CallerRunID     string       `json:"caller_run_id,omitempty"`
	ExecutionPolicy string       `json:"execution_policy,omitempty"`
	ErrorKind       string       `json:"error_kind,omitempty"`
	// ToolRestriction marks requested restrictions that the backend did not enforce.
	ToolRestriction string `json:"tool_restriction,omitempty"`
	// Transcript is set only when -capture-transcript was given: captured, not_created (the run
	// failed before capture started) or failed (the transcript could not be created).
	Transcript       string           `json:"transcript,omitempty"`
	Caller           string           `json:"caller"`
	TS               string           `json:"ts"`
	Status           string           `json:"status"` // ok | error | timeout
	ExitCode         *int             `json:"exit_code"`
	Profile          string           `json:"profile"`
	Backend          string           `json:"backend,omitempty"`
	Model            string           `json:"model"`
	ModelSource      string           `json:"model_source,omitempty"`
	ModelUnavailable string           `json:"model_unavailable,omitempty"`
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
	AddDirs          []string         `json:"add_dirs,omitempty"` // effective absolute extra directories
	// Session cumulative cost is a resume baseline, never a per-run cost.
	CumulativeCostUSD *float64 `json:"cumulative_cost_usd,omitempty"`
	// CostNote explains why CostUSD is nil. Read derives it for display only; it is never persisted.
	CostNote   string `json:"-"`
	DurationMs int64  `json:"duration_ms"` // duration reported by the worker (legacy field)
	ElapsedMs  *int64 `json:"elapsed_ms"`  // measured end-to-end, including queue wait and setup; nil for older records
	// WaitMs is time spent queued for a worker slot, ExecMs the time from admission to finish
	// (worker, salvage and cleanup). The -t timeout applies to the worker alone, not to WaitMs.
	WaitMs *int64 `json:"wait_ms,omitempty"`
	ExecMs *int64 `json:"exec_ms,omitempty"`
	// Cleanup is set when worker processes could not be confirmed stopped. Their slot stays
	// unavailable until they exit.
	Cleanup  string `json:"cleanup,omitempty"`
	Result   string `json:"result"`
	Error    string `json:"error,omitempty"`
	Salvaged bool   `json:"salvaged,omitempty"` // Result is a report written after the worker was stopped
}

// Termination classes. They describe why a worker ended before producing a result, so automation
// can decide whether to resume or give up.
const (
	// ClassCanceled: the stop was requested (Ctrl-C or a cancelled caller context).
	ClassCanceled = "canceled"
	// ClassInterrupted: a graceful shutdown signal (host or supervisor shutdown, service upgrade).
	ClassInterrupted = "interrupted"
	// ClassKilled: the worker was ended by a signal nobody asked offload to relay.
	ClassKilled = "killed"
)

// Termination records why a worker ended without a result, where that can be known.
type Termination struct {
	Class string `json:"class"`
	// Signal is the signal offload itself received that made it stop the worker.
	Signal string `json:"signal,omitempty"`
	// WorkerSignal is the signal that ended the worker process, from its wait status.
	WorkerSignal string `json:"worker_signal,omitempty"`
	// SupervisorRequested is true when offload, the worker's parent, asked the worker to stop.
	SupervisorRequested bool `json:"supervisor_requested"`
	// SystemKilled is true when the worker died of SIGKILL that offload did not send, which is
	// what the kernel's out-of-memory killer and a forced host shutdown send.
	SystemKilled bool `json:"system_killed"`
	// GraceExpired is true when the worker ignored the stop request for the whole grace period
	// and was force-killed, so it may not have flushed its own state.
	GraceExpired bool `json:"grace_expired,omitempty"`
	// ResumeCommand is set only when the session is known and can be resumed.
	ResumeCommand string `json:"resume_command,omitempty"`
}

// TerminationClass returns the record's termination class, or "" when it has none.
func (r Record) TerminationClass() string {
	if r.Termination == nil {
		return ""
	}
	return r.Termination.Class
}

// ToolRestrictionNotEnforced marks a run whose requested tool restriction was ignored.
const ToolRestrictionNotEnforced = "not_enforced"

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
	r.Caller = CallerOrUnspecified(r.Caller)
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
			r.Caller = CallerOrUnspecified(r.Caller)
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
