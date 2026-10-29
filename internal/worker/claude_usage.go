package worker

import (
	"bytes"
	"encoding/json"
	"math"
	"sort"

	"github.com/zhoushoujianwork/offload/internal/history"
)

const claudeUsageSource = "claude_result"

// Measured on real CLI results: result.usage covers one invocation, while modelUsage and
// total_cost_usd are session totals that keep growing across --resume. Claude reports input_tokens
// uncached, so history's inclusive Input is input + cache read + cache creation.
const (
	reasonNoUsage      = "result has no usage object"
	reasonBadUsage     = "result usage is incomplete, negative, overflowing or malformed"
	reasonNoModelUsage = "result has no usable modelUsage session totals"
	reasonNoResult     = "worker ended without a final result"
	reasonNoCost       = "result has no valid total_cost_usd"
)

func isNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// claudeCounters converts Claude counters to inclusive history usage. Input and output are
// required; absent or null cache counters count as 0. Any negative value or int64 overflow rejects
// the whole usage, so unavailable is never reported as zero.
func claudeCounters(input, output, read, write *int64) *history.TokenUsage {
	if input == nil || output == nil {
		return nil
	}
	var r, w int64
	if read != nil {
		r = *read
	}
	if write != nil {
		w = *write
	}
	if *input < 0 || *output < 0 || r < 0 || w < 0 {
		return nil
	}
	total, ok := addInt64(*input, r)
	if !ok {
		return nil
	}
	if total, ok = addInt64(total, w); !ok {
		return nil
	}
	u := &history.TokenUsage{Input: total, Cached: r, CacheWrite: w, Output: *output}
	if !u.Valid() {
		return nil
	}
	return u
}

func addInt64(a, b int64) (int64, bool) {
	if a < 0 || b < 0 || a > math.MaxInt64-b {
		return 0, false
	}
	return a + b, true
}

// decodeClaudeUsage reads result.usage (this invocation only).
func decodeClaudeUsage(raw json.RawMessage) (*history.TokenUsage, string) {
	if isNull(raw) {
		return nil, reasonNoUsage
	}
	var v struct {
		Input  *int64 `json:"input_tokens"`
		Output *int64 `json:"output_tokens"`
		Read   *int64 `json:"cache_read_input_tokens"`
		Write  *int64 `json:"cache_creation_input_tokens"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return nil, reasonBadUsage
	}
	if u := claudeCounters(v.Input, v.Output, v.Read, v.Write); u != nil {
		return u, ""
	}
	return nil, reasonBadUsage
}

// decodeClaudeModelUsage sums the per-model session totals from modelUsage. Every model entry
// must be valid, and a costUSD that is present must be a valid cost.
func decodeClaudeModelUsage(raw json.RawMessage) (*history.TokenUsage, string) {
	if isNull(raw) {
		return nil, reasonNoModelUsage
	}
	var models map[string]struct {
		Input  *int64   `json:"inputTokens"`
		Output *int64   `json:"outputTokens"`
		Read   *int64   `json:"cacheReadInputTokens"`
		Write  *int64   `json:"cacheCreationInputTokens"`
		Cost   *float64 `json:"costUSD"`
	}
	if json.Unmarshal(raw, &models) != nil || len(models) == 0 {
		return nil, reasonNoModelUsage
	}
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	var sum history.TokenUsage
	for _, name := range names {
		m := models[name]
		if m.Cost != nil && !history.ValidCost(*m.Cost) {
			return nil, reasonNoModelUsage
		}
		u := claudeCounters(m.Input, m.Output, m.Read, m.Write)
		if u == nil {
			return nil, reasonNoModelUsage
		}
		var ok bool
		if sum.Input, ok = addInt64(sum.Input, u.Input); !ok {
			return nil, reasonNoModelUsage
		}
		if sum.Cached, ok = addInt64(sum.Cached, u.Cached); !ok {
			return nil, reasonNoModelUsage
		}
		if sum.CacheWrite, ok = addInt64(sum.CacheWrite, u.CacheWrite); !ok {
			return nil, reasonNoModelUsage
		}
		if sum.Output, ok = addInt64(sum.Output, u.Output); !ok {
			return nil, reasonNoModelUsage
		}
	}
	if !sum.Valid() {
		return nil, reasonNoModelUsage
	}
	return &sum, ""
}

// claudeBaseline is the session total recorded by the previous run of a session, taken before a
// resumed worker starts.
type claudeBaseline struct {
	Usage  *history.TokenUsage
	Cost   *float64
	Reason string // why there is no usable baseline
}

// captureClaudeBaseline uses only the latest record of the session. If that record has no valid
// session totals (an interrupted run, a legacy record), an older record is not used: the newer run
// may have changed the session, so the older totals would be stale.
func captureClaudeBaseline(logPath, session string) claudeBaseline {
	rs, err := history.Read(logPath)
	if err != nil {
		return claudeBaseline{Reason: "run history unreadable, no resume baseline"}
	}
	var latest *history.Record
	for i := range rs {
		if rs[i].Session == session {
			latest = &rs[i]
		}
	}
	switch {
	case latest == nil:
		return claudeBaseline{Reason: "no earlier record of this session, no resume baseline"}
	case latest.Backend != "" && latest.Backend != "claude":
		return claudeBaseline{Reason: "latest record of this session is not a Claude run, no resume baseline"}
	case latest.CumulativeUsage == nil || !latest.CumulativeUsage.Valid():
		return claudeBaseline{Reason: "latest record of this session has no valid cumulative usage, no resume baseline"}
	case latest.CumulativeCostUSD == nil || !history.ValidCost(*latest.CumulativeCostUSD):
		return claudeBaseline{Reason: "latest record of this session has no valid cumulative cost, no resume baseline"}
	}
	u, c := *latest.CumulativeUsage, *latest.CumulativeCostUSD
	return claudeBaseline{Usage: &u, Cost: &c}
}

func subUsage(a, b history.TokenUsage) history.TokenUsage {
	return history.TokenUsage{Input: a.Input - b.Input, Cached: a.Cached - b.Cached, CacheWrite: a.CacheWrite - b.CacheWrite, Output: a.Output - b.Output}
}

func validTotalCost(c *float64) *float64 {
	if c == nil || !history.ValidCost(*c) {
		return nil
	}
	v := *c
	return &v
}

// setSnapshots stores the session totals so the next resume can take them as its baseline, even
// when this run's own cost is unavailable.
func setSnapshots(rec *history.Record, res *claudeResult) string {
	cum, reason := decodeClaudeModelUsage(res.RawModelUsage)
	rec.CumulativeUsage = cum
	rec.CumulativeCostUSD = validTotalCost(res.CostUSD)
	return reason
}

// applyClaudeAccounting records usage and cost of a normal Claude run that ended with a result.
func applyClaudeAccounting(rec *history.Record, res *claudeResult, o Opts, base claudeBaseline) {
	rec.UsageSource, rec.UsageScope = claudeUsageSource, "run"
	rec.CostUSD = nil
	rec.CostUnavailable, rec.UsageUnavailable = "", ""

	var reason string
	if rec.Usage, reason = decodeClaudeUsage(res.RawUsage); rec.Usage == nil {
		rec.UsageUnavailable = reason
	}
	cumReason := setSnapshots(rec, res)
	total := rec.CumulativeCostUSD

	if o.Resume == "" {
		if total == nil {
			rec.CostUnavailable = reasonNoCost
			return
		}
		c := *total
		rec.CostUSD = &c
		return
	}
	// total_cost_usd is a session total here; only a verified delta is this run's cost.
	switch {
	case total == nil:
		rec.CostUnavailable = reasonNoCost
	case base.Usage == nil || base.Cost == nil:
		rec.CostUnavailable = firstNonEmpty(base.Reason, "resume baseline unavailable")
	case firstNonEmpty(res.SessionID, rec.Session) != o.Resume:
		rec.CostUnavailable = "resumed session id differs from the baseline session"
	case rec.Usage == nil:
		rec.CostUnavailable = "per-run usage unavailable, cannot verify the session delta: " + rec.UsageUnavailable
	case rec.CumulativeUsage == nil:
		rec.CostUnavailable = "session totals unavailable, cannot verify the session delta: " + cumReason
	case *total < *base.Cost:
		rec.CostUnavailable = "session cost total is lower than the baseline"
	default:
		delta := subUsage(*rec.CumulativeUsage, *base.Usage)
		if !delta.Valid() || delta != *rec.Usage {
			rec.CostUnavailable = "session usage delta does not match this run's usage (other activity in the session, or counters regressed)"
			return
		}
		c := math.Round((*total-*base.Cost)*1e12) / 1e12
		if !history.ValidCost(c) {
			rec.CostUnavailable = "invalid session cost delta"
			return
		}
		rec.CostUSD = &c
	}
}

// markClaudeNoResult records that a normal Claude run ended without a final result, so neither
// usage nor cost is known. Codex accounting is applied separately.
func markClaudeNoResult(rec *history.Record) {
	if rec.Backend == "codex" {
		return
	}
	rec.UsageSource = claudeUsageSource
	rec.UsageUnavailable, rec.CostUnavailable = reasonNoResult, reasonNoResult
	rec.CostUSD = nil
}

// applyClaudeSalvageAccounting keeps the report step's own reported cost, labelled as partial, and
// the session totals. The stopped run's full usage and cost stay unavailable.
func applyClaudeSalvageAccounting(rec *history.Record, res *claudeResult) {
	setSnapshots(rec, res)
	rec.UsageSource, rec.UsageScope = claudeUsageSource, "report_step"
	rec.Usage = nil
	rec.UsageUnavailable = "full-run usage unavailable: the run was stopped before its final result; cost_usd is the report step's reported value, not a full-run cost"
	rec.CostUnavailable = ""
	rec.CostUSD = validTotalCost(res.CostUSD)
}
