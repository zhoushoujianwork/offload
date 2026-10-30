package worker

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zhoushoujianwork/offload/internal/history"
)

// Pointer fields reject omitted/null counters; int64 decoding rejects overflow,
// fractional numbers, strings, and negative counts are rejected by Valid.
func decodeCodexUsage(raw json.RawMessage) *history.TokenUsage {
	var v struct {
		Input  *int64 `json:"input_tokens"`
		Cached *int64 `json:"cached_input_tokens"`
		Write  *int64 `json:"cache_write_input_tokens"`
		Output *int64 `json:"output_tokens"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Input == nil || v.Cached == nil || v.Write == nil || v.Output == nil {
		return nil
	}
	u := &history.TokenUsage{Input: *v.Input, Cached: *v.Cached, CacheWrite: *v.Write, Output: *v.Output}
	if !u.Valid() {
		return nil
	}
	return u
}

var codexSessionID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func codexRollout(session string, scoped ...string) (string, error) {
	if !codexSessionID.MatchString(session) {
		return "", errors.New("invalid session id")
	}
	home := codexHome(scoped...)
	if home == "" {
		return "", errors.New("codex home unavailable")
	}
	var match string
	err := filepath.WalkDir(filepath.Join(home, "sessions"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), "-"+session+".jsonl") {
			if match != "" || !d.Type().IsRegular() {
				return errors.New("ambiguous rollout")
			}
			match = path
		}
		return nil
	})
	if err != nil || match == "" {
		return "", errors.New("rollout unavailable")
	}
	return match, nil
}

// Capture before the child starts: a resumed exec emits the session total.
// Only filenames are searched; only the matching rollout is streamed. Never
// surface paths, payload text, prompts, or results in accounting errors.
func codexBaseline(session string, scoped ...string) (*history.TokenUsage, string) {
	if session == "" {
		return &history.TokenUsage{}, ""
	}
	const unavailable = "resume baseline missing or corrupt"
	if !codexSessionID.MatchString(session) {
		return nil, unavailable
	}
	match, err := codexRollout(session, scoped...)
	if err != nil {
		return nil, unavailable
	}
	f, err := os.Open(match)
	if err != nil {
		return nil, unavailable
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	verified := false
	var latest *history.TokenUsage
	for sc.Scan() {
		var e struct {
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			return nil, unavailable
		}
		switch e.Type {
		case "session_meta":
			var meta struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(e.Payload, &meta) != nil || meta.ID != session {
				return nil, unavailable
			}
			verified = true
		case "event_msg":
			var event struct {
				Type string `json:"type"`
				Info *struct {
					Total json.RawMessage `json:"total_token_usage"`
				} `json:"info"`
			}
			if json.Unmarshal(e.Payload, &event) != nil {
				return nil, unavailable
			}
			if event.Type == "token_count" && event.Info != nil {
				latest = decodeCodexUsage(event.Info.Total)
				if latest == nil {
					return nil, unavailable
				}
			}
		}
	}
	if sc.Err() != nil || !verified || latest == nil {
		return nil, unavailable
	}
	return latest, ""
}

const codexUsageSource = "codex exec turn.completed usage (session cumulative)"
const codexCostBasis = "api_baseline"
const codexPricingAssumptions = "API-equivalent short-context/30m cache-write baseline; excludes long-context (>272k per request), regional and tool surcharges and subscription accounting; actual service delivery may differ. Aggregated input cannot determine individual request context size."

func applyCodexAccounting(rec *history.Record, s streamSummary, baseline *history.TokenUsage, baselineError string, o Opts, completed bool) {
	rec.CostUSD, rec.CostIsEstimate = nil, false
	rec.UsageSource, rec.UsageScope = codexUsageSource, "run"
	if o.Resume != "" {
		rec.UsageSource += " minus pre-launch native rollout token_count info.total_token_usage"
	}
	rec.ServiceTier = o.Profile.ServiceTier
	rec.CostBasis = codexCostBasis
	if !completed || s.Result == nil || s.Result.CumulativeUsage == nil {
		rec.UsageUnavailable = "missing or invalid terminal usage (possibly interrupted)"
		if completed && s.Result != nil && s.Result.UsageUnavailable != "" {
			rec.UsageUnavailable = s.Result.UsageUnavailable
		}
	} else {
		rec.CumulativeUsage = s.Result.CumulativeUsage
		switch {
		case baseline == nil:
			rec.UsageUnavailable = baselineError
		case o.Resume != "" && s.Session != o.Resume:
			rec.UsageUnavailable = "resume session does not match baseline"
		default:
			final := rec.CumulativeUsage
			delta := &history.TokenUsage{Input: final.Input - baseline.Input, Cached: final.Cached - baseline.Cached, CacheWrite: final.CacheWrite - baseline.CacheWrite, Output: final.Output - baseline.Output}
			if !delta.Valid() {
				rec.UsageUnavailable = "cumulative usage regressed or invalid delta"
			} else {
				rec.Usage = delta
			}
		}
	}
	if codexSubscription(o.CodexHome) {
		rec.CostBasis = "subscription"
		rec.CostUnavailable = "subscription: Codex does not report USD cost"
		return
	}
	if rec.Usage == nil {
		rec.CostUnavailable = rec.UsageUnavailable
		return
	}
	if o.Model != "gpt-6.1-sol" || (o.Profile.ServiceTier != "default" && o.Profile.ServiceTier != "priority") {
		rec.CostUnavailable = "reference rates require explicit gpt-6.1-sol model and default/priority tier"
		return
	}
	p := &history.PricingSnapshot{Model: o.Model, Tier: o.Profile.ServiceTier, Currency: "USD", PerTokens: 1000000,
		Input: 2, Cached: .10, CacheWrite: 2.50, Output: 10,
		Source: "https://developers.openai.com/api/docs/models/gpt-6.1-sol", Date: "2026-10-26", Assumptions: codexPricingAssumptions}
	if p.Tier == "priority" {
		p.Input *= 2
		p.Cached *= 2
		p.CacheWrite *= 2
		p.Output *= 2
	}
	u := rec.Usage
	cost := (float64(u.Input-u.Cached-u.CacheWrite)*p.Input + float64(u.Cached)*p.Cached + float64(u.CacheWrite)*p.CacheWrite + float64(u.Output)*p.Output) / float64(p.PerTokens)
	if !history.ValidCost(cost) {
		rec.CostUnavailable = "invalid computed estimate"
		return
	}
	rec.Pricing, rec.CostUSD, rec.CostIsEstimate = p, &cost, true
}
