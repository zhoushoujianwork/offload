package history

// TokenUsage counts input inclusively: cached and cache-write input are subsets.
// A nil *TokenUsage means unavailable, distinct from measured zero usage.
type TokenUsage struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
}

func (u TokenUsage) Valid() bool {
	return u.Input >= 0 && u.Cached >= 0 && u.CacheWrite >= 0 && u.Output >= 0 &&
		u.Cached <= u.Input && u.CacheWrite <= u.Input-u.Cached
}

// PricingSnapshot freezes the reference rates and assumptions used for this run.
type PricingSnapshot struct {
	Model       string  `json:"model"`
	Tier        string  `json:"service_tier"`
	Currency    string  `json:"currency"`
	PerTokens   int64   `json:"per_tokens"`
	Input       float64 `json:"input"`
	Cached      float64 `json:"cached_input"`
	CacheWrite  float64 `json:"cache_write_input"`
	Output      float64 `json:"output"`
	Source      string  `json:"source"`
	Date        string  `json:"date"`
	Assumptions string  `json:"assumptions"`
}

func firstCostReason(reason, fallback string) string {
	if reason != "" {
		return reason
	}
	return fallback
}
