package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// ValidateCodexSettings checks optional overrides without requiring native defaults.
func (p Profile) ValidateCodexSettings() error {
	if p.BackendName() != "codex" {
		return nil
	}
	switch p.ReasoningEffort {
	case "", "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		return fmt.Errorf("invalid Codex reasoning effort")
	}
	switch p.ServiceTier {
	case "", "default", "priority":
	default:
		return fmt.Errorf("invalid Codex service tier")
	}
	return nil
}

// SaveProfile saves a complete bundle, preserving unknown JSON and unrelated profiles.
// Adding selects the new profile; editing preserves the saved default.
func SaveProfile(path, name string, p Profile, add bool) error {
	if add {
		name = strings.TrimSpace(name)
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("profile name must not be empty")
	}
	fields, err := modeSettings(path)
	if err != nil {
		return err
	}
	profiles := map[string]json.RawMessage{}
	if raw, ok := fields["profiles"]; ok {
		if err := json.Unmarshal(raw, &profiles); err != nil {
			return err
		}
		if profiles == nil {
			return fmt.Errorf("profiles must be a JSON object")
		}
	}
	raw, exists := profiles[name]
	if add && exists {
		return fmt.Errorf("profile already exists")
	}
	if !add && !exists {
		return fmt.Errorf("unknown profile")
	}
	if p.BackendName() != "codex" && p.BackendName() != "claude" {
		return fmt.Errorf("unknown backend")
	}
	if err := p.ValidateCodexSettings(); err != nil {
		return err
	}
	if !ValidEndpoint(p.Endpoint) {
		return fmt.Errorf("unknown endpoint %q (want cc or llm)", p.Endpoint)
	}
	if p.Endpoint != "" && p.BackendName() != "claude" {
		return fmt.Errorf("endpoint applies only to claude profiles")
	}
	if p.BackendName() == "claude" {
		u, err := url.Parse(p.BaseURL)
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("base URL must be http(s) with a host")
		}
		if strings.TrimSpace(p.Model) == "" {
			return fmt.Errorf("model is required")
		}
		if p.TokenCmd == "" && p.Token == "" {
			return fmt.Errorf("authentication is required")
		}
	}
	profile := map[string]json.RawMessage{}
	if exists {
		if err := json.Unmarshal(raw, &profile); err != nil {
			return err
		}
		if profile == nil {
			return fmt.Errorf("profile must be a JSON object")
		}
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return err
	}
	var known map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &known); err != nil {
		return err
	}
	for _, key := range []string{"backend", "base_url", "model", "token", "token_cmd", "reasoning_effort", "service_tier", "endpoint"} {
		delete(profile, key)
	}
	for key, value := range known {
		profile[key] = value
	}
	profiles[name], err = json.Marshal(profile)
	if err != nil {
		return err
	}
	fields["profiles"], err = json.Marshal(profiles)
	if err != nil {
		return err
	}
	if add {
		fields["default"], _ = json.Marshal(name)
	}
	return writeSettings(path, fields)
}
