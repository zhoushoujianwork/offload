package config

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Unavailable records why a profile stopped being usable and when. A nil value means available.
type Unavailable struct {
	Reason string `json:"reason"`
	At     string `json:"at"`
}

// Available reports whether the profile takes part in default and pool selection.
func (p Profile) Available() bool { return p.Unavailable == nil }

// EffectiveDefault is the default profile name, except that an unavailable default is replaced
// by the first available profile in name order. With no available profile the saved default is
// returned unchanged, so the run fails on that profile instead of picking something unexpected.
func (c *Config) EffectiveDefault() string {
	if p, ok := c.Profiles[c.Default]; ok && p.Available() {
		return c.Default
	}
	if _, ok := c.Profiles[c.Default]; !ok {
		return c.Default
	}
	names := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if c.Profiles[n].Available() {
			return n
		}
	}
	return c.Default
}

// Resolve picks a profile by name or alias. An explicit name (argument or OFFLOAD_PROFILE) is
// honored even when the profile is unavailable; only the config default is substituted.
func (c *Config) Resolve(name string) (string, Profile, error) {
	if name == "" {
		name = os.Getenv("OFFLOAD_PROFILE")
	}
	if name == "" {
		name = c.EffectiveDefault()
	}
	return c.resolveNamed(name)
}

// SetUnavailable marks a profile unavailable, replacing any earlier reason and time. Unknown
// fields and other profiles are preserved. The reason is sanitized before it is stored.
func SetUnavailable(path, name, reason string, at time.Time) error {
	v, err := json.Marshal(Unavailable{Reason: SanitizeReason(reason), At: at.UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	return patchProfileField(path, name, v)
}

// MarkAvailable clears the unavailable state of a profile.
func MarkAvailable(path, name string) error { return patchProfileField(path, name, nil) }

func patchProfileField(path, name string, value json.RawMessage) error {
	fields, err := modeSettings(path)
	if err != nil {
		return err
	}
	profiles := map[string]json.RawMessage{}
	if raw, ok := fields["profiles"]; ok {
		if err := json.Unmarshal(raw, &profiles); err != nil || profiles == nil {
			return fmt.Errorf("profiles must be a JSON object")
		}
	}
	raw, ok := profiles[name]
	if !ok {
		return fmt.Errorf("unknown profile")
	}
	profile := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &profile); err != nil || profile == nil {
		return fmt.Errorf("profile must be a JSON object")
	}
	if value == nil {
		delete(profile, "unavailable")
	} else {
		profile["unavailable"] = value
	}
	if profiles[name], err = json.Marshal(profile); err != nil {
		return err
	}
	if fields["profiles"], err = json.Marshal(profiles); err != nil {
		return err
	}
	return writeSettings(path, fields)
}

var (
	reAuth  = regexp.MustCompile(`(?i)\b401\b|unauthori[sz]ed|authentication[_ ](failed|error)|invalid[_ ](api[_ ]?key|x-api-key|token|credentials?)|incorrect api key|token (has )?(expired|revoked)|refresh token was already used|please log out and sign in again|not logged in|please (log ?in|sign ?in)|permission_denied|\b403\b.*(forbidden|key)`)
	reQuota = regexp.MustCompile(`(?i)insufficient[_ ]quota|quota (exceeded|exhausted)|exceeded your (current )?quota|credit balance|out of credits|billing|usage limit|payment required|\b402\b`)
	reModel = regexp.MustCompile(`(?i)model[_ ]not[_ ]found|model .{0,80}(does not exist|not found|not supported|is not available|has been (deprecated|retired))|unknown model|invalid model|no such model|does not have access to model`)
	reTemp  = regexp.MustCompile(`(?i)rate[_ ]limit|too many requests|\b429\b|timed? ?out|deadline exceeded|connection (reset|refused|closed)|network|temporar|overloaded|\b50[234]\b|unavailable|\bEOF\b|no such host|dial tcp`)
)

// ClassifyFailure reports whether a failed run or test shows a condition that does not recover
// by itself: bad credentials, exhausted quota or a missing model. Timeouts, network errors,
// rate limits and capacity errors never qualify. category is a short human label.
func ClassifyFailure(errorKind, message string) (permanent bool, category string) {
	switch errorKind {
	case "rate_limit", "capacity", "canceled", "timeout", "idle_timeout", "events_io", "configuration", "interrupted", "monitor":
		return false, ""
	}
	switch {
	case reAuth.MatchString(message):
		return true, "authentication failed"
	case reQuota.MatchString(message):
		return true, "quota exhausted"
	case reModel.MatchString(message):
		return true, "model not available"
	}
	_ = reTemp // transient wording is the default outcome; kept for documentation of intent
	return false, ""
}

var redactions = []*regexp.Regexp{
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)(api[_-]?key|x-api-key|token|secret|password|authorization|credential)s?["']?\s*[:=]\s*["']?[^\s"',;]+`),
	regexp.MustCompile(`\b(sk|pk|rk|ghp|gho|xox[abp])[-_][A-Za-z0-9_-]{6,}`),
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]+\.?[A-Za-z0-9_-]*`),
	regexp.MustCompile(`[A-Za-z0-9_+/=-]{32,}`),
	regexp.MustCompile(`https?://\S+`),
}

const maxReasonRunes = 200

// SanitizeReason keeps one short line and removes keys, tokens, URLs and long opaque strings, so
// a stored reason never carries a credential or the request body.
func SanitizeReason(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	for _, re := range redactions {
		s = re.ReplaceAllString(s, "[redacted]")
	}
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxReasonRunes {
		s = string(r[:maxReasonRunes]) + "…"
	}
	if s == "" {
		s = "unavailable"
	}
	return s
}

// FailureReason builds the stored reason for a classified failure.
func FailureReason(category, message string) string {
	return category + ": " + SanitizeReason(message)
}
