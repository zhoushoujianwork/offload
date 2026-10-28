package worker

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Inspect only provider error envelopes, never tool output or successful reports.
// Unknown structured signals suppress message matching as well.
func providerErrorKind(line []byte, messages ...string) string {
	var event map[string]json.RawMessage
	if json.Unmarshal(line, &event) != nil {
		return ""
	}
	var inspect func(map[string]json.RawMessage, bool) string
	inspect = func(obj map[string]json.RawMessage, nested bool) string {
		for _, field := range []string{"status_code", "http_status", "http_status_code", "status", "code", "error_code", "type"} {
			if field == "type" && !nested {
				continue
			}
			raw := obj[field]
			var value string
			if json.Unmarshal(raw, &value) != nil {
				value = string(raw)
			}
			switch strings.ToLower(value) {
			case "529", "overloaded", "overloaded_error", "model_capacity_exceeded", "capacity_exceeded":
				return "capacity"
			case "429", "rate_limit", "rate_limit_error", "rate_limit_exceeded":
				return "rate_limit"
			}
		}
		unknown := false
		for _, field := range []string{"error", "codex_error_info"} {
			raw := obj[field]
			var child map[string]json.RawMessage
			if json.Unmarshal(raw, &child) == nil && child != nil {
				if kind := inspect(child, true); kind != "" {
					return kind
				}
			} else {
				var value string
				if json.Unmarshal(raw, &value) == nil && value != "" {
					return inspect(map[string]json.RawMessage{"code": raw}, true)
				}
			}
		}
		for _, field := range []string{"status_code", "http_status", "http_status_code", "status", "code", "error_code", "type"} {
			if field == "type" && !nested {
				continue
			}
			if raw := obj[field]; len(raw) > 0 && string(raw) != "null" && string(raw) != `""` {
				unknown = true
			}
		}
		if unknown {
			return "execution"
		}
		return ""
	}
	if kind := inspect(event, false); kind != "" {
		return kind
	}
	for _, message := range messages {
		if capacityMessage.MatchString(strings.TrimSpace(message)) {
			return "capacity"
		}
		if rateLimitMessage.MatchString(strings.TrimSpace(message)) {
			return "rate_limit"
		}
	}
	return ""
}

// Deliberately anchored: incidental mentions of capacity in task failures are not signals.
var capacityMessage = regexp.MustCompile(`(?i)^(selected model is at capacity|overloaded|overloaded_error|the model is currently overloaded)([.!:]|$)`)
var rateLimitMessage = regexp.MustCompile(`(?i)^(rate limit exceeded|rate_limit_error|too many requests)([.!:]|$)`)

func terminalProviderKind(line []byte, previous string, messages ...string) string {
	if kind := providerErrorKind(line); kind != "" {
		return kind
	}
	if previous != "" {
		return previous
	}
	return providerErrorKind(line, messages...)
}
