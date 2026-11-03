package history

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

// AccountingPatch is an append-only correction, bound to the exact original log
// line. It deliberately contains no prompts, reports, credentials or log paths.
type AccountingPatch struct {
	Fingerprint       string           `json:"fingerprint"`
	Line              int              `json:"line"`
	Backend           string           `json:"backend"`
	Model             string           `json:"model"`
	ModelSource       string           `json:"model_source,omitempty"`
	ServiceTier       string           `json:"service_tier,omitempty"`
	Usage             *TokenUsage      `json:"usage"`
	CumulativeUsage   *TokenUsage      `json:"cumulative_usage,omitempty"`
	CostUSD           *float64         `json:"cost_usd"`
	CumulativeCostUSD *float64         `json:"cumulative_cost_usd,omitempty"`
	Pricing           *PricingSnapshot `json:"pricing,omitempty"`
	Source            string           `json:"source"`
	Scope             string           `json:"scope"`
	CostBasis         string           `json:"cost_basis,omitempty"`
	CostUnavailable   string           `json:"cost_unavailable,omitempty"`
	Evidence          string           `json:"evidence"`
}

func AccountingFingerprint(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func NewAccountingPatch(raw []byte, line int, r Record) AccountingPatch {
	return AccountingPatch{Fingerprint: AccountingFingerprint(raw), Line: line, Backend: r.Backend,
		Model: r.Model, ModelSource: r.ModelSource, ServiceTier: r.ServiceTier, Usage: r.Usage, CumulativeUsage: r.CumulativeUsage,
		CostUSD: r.CostUSD, CumulativeCostUSD: r.CumulativeCostUSD, Pricing: r.Pricing,
		Source: r.UsageSource, Scope: r.UsageScope, CostBasis: r.CostBasis,
		CostUnavailable: r.CostUnavailable, Evidence: r.AccountingEvidence}
}

func (p AccountingPatch) valid() bool {
	_, err := hex.DecodeString(p.Fingerprint)
	return err == nil && len(p.Fingerprint) == 64 && p.Line > 0 && p.Usage != nil && p.Usage.Valid() &&
		(p.Scope == "run" || p.Scope == "partial_run") && p.Source != "" && p.Evidence != "" &&
		(p.Backend == "codex" || p.Backend == "claude") &&
		(p.CostUSD == nil || ValidCost(*p.CostUSD)) &&
		(p.Scope != "partial_run" || p.CostUSD == nil) &&
		(p.CumulativeUsage == nil || p.CumulativeUsage.Valid()) &&
		(p.CumulativeCostUSD == nil || ValidCost(*p.CumulativeCostUSD))
}

func accountingPatches(path string) (map[string]AccountingPatch, error) {
	f, err := os.Open(path + ".accounting.jsonl")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]AccountingPatch{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 1<<20)
	for sc.Scan() {
		var p AccountingPatch
		// An interrupted append cannot invent an accounting value. As with the
		// original run log, malformed lines are not accepted as records.
		if json.Unmarshal(sc.Bytes(), &p) == nil && p.valid() {
			out[strconv.Itoa(p.Line)+":"+p.Fingerprint] = p
		}
	}
	return out, sc.Err()
}

func applyAccountingPatch(r *Record, raw []byte, line int, patches map[string]AccountingPatch) {
	if len(patches) == 0 {
		return
	}
	p, ok := patches[strconv.Itoa(line)+":"+AccountingFingerprint(raw)]
	if !ok {
		return
	}
	r.Backend, r.Model, r.ServiceTier = p.Backend, p.Model, p.ServiceTier
	r.ModelSource = p.ModelSource
	if p.ModelSource == "native_session_turn" {
		r.ModelUnavailable = ""
	}
	r.Usage, r.CumulativeUsage = p.Usage, p.CumulativeUsage
	r.CostUSD, r.CumulativeCostUSD, r.Pricing = p.CostUSD, p.CumulativeCostUSD, p.Pricing
	r.UsageSource, r.UsageScope, r.CostBasis = p.Source, p.Scope, p.CostBasis
	r.AccountingEvidence, r.CostUnavailable = p.Evidence, p.CostUnavailable
	r.UsageUnavailable = ""
	if p.Scope == "partial_run" {
		r.UsageUnavailable = "native log contains only observed usage; full-run usage unavailable"
	}
}

// AppendAccountingPatch never changes runs.jsonl, so concurrent workers can keep
// appending runs. Each correction is a single append; replay is idempotent by hash.
func AppendAccountingPatch(path string, p AccountingPatch) error {
	if !p.valid() {
		return errors.New("invalid accounting correction")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".accounting.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}
