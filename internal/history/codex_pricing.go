package history

// EstimateCodexCost applies the documented API-equivalent baseline, including to
// subscription usage. It never represents an actual subscription charge.
func EstimateCodexCost(r *Record) {
	if r.Salvaged || r.Usage == nil || !r.Usage.Valid() {
		return
	}
	if r.Model != "gpt-6.1-sol" || (r.ServiceTier != "default" && r.ServiceTier != "priority") {
		r.CostUnavailable = "reference rates require explicit gpt-6.1-sol model and default/priority tier"
		return
	}
	p := &PricingSnapshot{Model: r.Model, Tier: r.ServiceTier, Currency: "USD", PerTokens: 1000000,
		Input: 2, Cached: .10, CacheWrite: 2.50, Output: 10,
		Source: "https://developers.openai.com/api/docs/models/gpt-6.1-sol", Date: "2026-10-26",
		Assumptions: "API-equivalent short-context/30m cache-write baseline; excludes long-context (>272k per request), regional and tool surcharges and subscription accounting; actual service delivery may differ. Aggregated input cannot determine individual request context size."}
	if p.Tier == "priority" {
		p.Input *= 2
		p.Cached *= 2
		p.CacheWrite *= 2
		p.Output *= 2
	}
	u := r.Usage
	cost := (float64(u.Input-u.Cached-u.CacheWrite)*p.Input + float64(u.Cached)*p.Cached + float64(u.CacheWrite)*p.CacheWrite + float64(u.Output)*p.Output) / float64(p.PerTokens)
	if !ValidCost(cost) {
		return
	}
	r.Pricing, r.CostUSD, r.CostIsEstimate = p, &cost, true
	r.CostBasis = "api_baseline"
	r.CostUnavailable, r.CostNote, r.UsageUnavailable = "", "", ""
}
