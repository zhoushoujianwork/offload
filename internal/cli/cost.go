package cli

import (
	"fmt"
	"io"

	"github.com/zhoushoujianwork/offload/internal/history"
)

func compactUsage(r history.Record) string {
	u := r.Usage
	return fmt.Sprintf("tokens in=%d cached=%d write=%d out=%d", u.Input, u.Cached, u.CacheWrite, u.Output)
}

func showCostDetails(w io.Writer, r history.Record) {
	if r.ModelSource != "" {
		fmt.Fprintf(w, "Model source: %s\n", r.ModelSource)
	}
	if r.ModelUnavailable != "" {
		fmt.Fprintf(w, "Model unavailable: %s\n", r.ModelUnavailable)
	}
	if r.Backend == "codex" && r.UsageSource != "" {
		fmt.Fprintf(w, "Selected model/tier: %s / %s\n", firstNonEmpty(r.Model, "inherited"), firstNonEmpty(r.ServiceTier, "inherited"))
	}
	if r.Usage != nil {
		fmt.Fprintln(w, compactUsage(r))
	}
	if r.CumulativeUsage != nil {
		u := r.CumulativeUsage
		fmt.Fprintf(w, "Session cumulative tokens: input=%d cached=%d cache_write=%d output=%d\n", u.Input, u.Cached, u.CacheWrite, u.Output)
	}
	if r.UsageSource != "" {
		fmt.Fprintf(w, "Usage source: %s; scope: %s\n", r.UsageSource, r.UsageScope)
	}
	if r.UsageUnavailable != "" {
		fmt.Fprintf(w, "Usage unavailable: %s\n", r.UsageUnavailable)
	}
	if r.CostUSD == nil {
		fmt.Fprintf(w, "Cost unavailable: %s\n", firstNonEmpty(r.CostUnavailable, firstNonEmpty(r.CostNote, history.CostNoteNotReport)))
	}
	if p := r.Pricing; p != nil {
		fmt.Fprintf(w, "Pricing: %s / %s; %s per %d tokens: input=%g cached=%g cache_write=%g output=%g\nSource: %s (verified %s)\nAssumptions: %s\n", p.Model, p.Tier, p.Currency, p.PerTokens, p.Input, p.Cached, p.CacheWrite, p.Output, p.Source, p.Date, p.Assumptions)
	}
}
