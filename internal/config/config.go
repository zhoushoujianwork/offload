// Package config loads worker backend profiles.
package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zhoushoujianwork/offload/internal/procutil"
)

// Profile selects a worker backend and its model/configuration.
type Profile struct {
	Aliases         []string `json:"aliases,omitempty"`
	IdleTimeout     string   `json:"idle_timeout,omitempty"`
	Backend         string   `json:"backend,omitempty"` // claude (default) or codex
	BaseURL         string   `json:"base_url"`
	Model           string   `json:"model"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
	ServiceTier     string   `json:"service_tier,omitempty"`
	// Endpoint is the gateway kind: "cc" (Claude Code proxy) or "llm" (generic LLM gateway); claude backend only, informational.
	Endpoint string `json:"endpoint,omitempty"`
	// TokenCmd prints the token on stdout (e.g. `security find-generic-password -s offload-minimax -w`).
	// Preferred over Token so secrets stay out of the config file.
	TokenCmd string `json:"token_cmd,omitempty"`
	Token    string `json:"token,omitempty"`
}

type Config struct {
	Default  string             `json:"default"`
	Profiles map[string]Profile `json:"profiles"`
	// DelegationMode controls how proactively a supervising agent is instructed to
	// delegate work via offload: "basic" or "proactive". Empty means unset.
	DelegationMode string `json:"delegation_mode,omitempty"`
	// Limits caps concurrent workers and sets build defaults for their child processes.
	Limits Limits `json:"limits,omitempty"`
}

// Gateway kinds accepted in Profile.Endpoint.
const (
	EndpointCC  = "cc"
	EndpointLLM = "llm"
)

// ValidEndpoint reports whether s is an accepted endpoint value (empty means unset).
func ValidEndpoint(s string) bool {
	return s == "" || s == EndpointCC || s == EndpointLLM
}

// Delegation modes understood by `offload mode` and `install-agent -mode`.
const (
	ModeBasic     = "basic"
	ModeProactive = "proactive"
)

// ValidMode reports whether m is a supported delegation mode.
func ValidMode(m string) bool {
	return m == ModeBasic || m == ModeProactive
}

// EffectiveMode returns the configured delegation mode, defaulting to ModeProactive
// when unset. c may be nil (no config loaded), which also defaults to ModeProactive
// for backward compatibility.
func (c *Config) EffectiveMode() string {
	if c == nil || c.DelegationMode == "" {
		return ModeProactive
	}
	return c.DelegationMode
}

// Dir is the directory holding offload's config.
func Dir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "offload")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "offload")
}

// Path is the config file location.
func Path() string { return filepath.Join(Dir(), "config.json") }

// Load reads and parses the config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no config at %s (see README: Configuration)", path)
	}
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := c.ValidateAliases(); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &c, nil
}

// Resolve picks a profile by name or alias ("" means OFFLOAD_PROFILE, then config default).
func (c *Config) Resolve(name string) (string, Profile, error) {
	if name == "" {
		name = os.Getenv("OFFLOAD_PROFILE")
	}
	if name == "" {
		name = c.Default
	}
	p, ok := c.Profiles[name]
	if !ok {
		match := ""
		for full, candidate := range c.Profiles {
			for _, alias := range candidate.Aliases {
				if alias == name {
					if match != "" {
						return name, Profile{}, fmt.Errorf("ambiguous profile %q; available: %s", name, c.ProfileChoices())
					}
					match, p = full, candidate
				}
			}
		}
		if match == "" {
			return name, p, fmt.Errorf("unknown profile %q; available: %s", name, c.ProfileChoices())
		}
		name = match
	}
	if p.BackendName() != "claude" && p.BackendName() != "codex" {
		return name, p, fmt.Errorf("profile %q has unknown backend %q", name, p.Backend)
	}
	if p.BackendName() == "claude" && (p.BaseURL == "" || p.Model == "") {
		return name, p, fmt.Errorf("profile %q needs base_url and model", name)
	}
	if err := p.ValidateCodexSettings(); err != nil {
		return name, p, err
	}
	if !ValidEndpoint(p.Endpoint) {
		return name, p, fmt.Errorf("profile %q has unknown endpoint %q (want cc or llm)", name, p.Endpoint)
	}
	if p.Endpoint != "" && p.BackendName() != "claude" {
		return name, p, fmt.Errorf("profile %q: endpoint applies only to claude profiles", name)
	}
	return name, p, nil
}

// SupportsToolRestrictions reports whether the backend can enforce explicit tool allow or deny
// lists. Claude forwards them to the CLI; Codex has no equivalent control.
func (p Profile) SupportsToolRestrictions() bool {
	return p.BackendName() != "codex"
}

func (p Profile) BackendName() string {
	if p.Backend == "" {
		return "claude"
	}
	return p.Backend
}

// SetDefault updates only the default field, preserving other configuration fields.
func SetDefault(path, name string) error {
	c, err := Load(path)
	if err != nil {
		return err
	}
	if name == "" {
		return errors.New("default profile name must not be empty")
	}
	resolved, _, err := c.Resolve(name)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	fields["default"], _ = json.Marshal(resolved)
	return writeSettings(path, fields)
}

// ResolveToken returns the profile's token, running token_cmd when set.
func (p Profile) ResolveToken() (string, error) {
	return p.ResolveTokenContext(context.Background())
}

// ResolveTokenContext is ResolveToken, except that cancelling ctx stops token_cmd and its
// descendants and returns promptly. A cancelled ctx never starts token_cmd.
func (p Profile) ResolveTokenContext(ctx context.Context) (string, error) {
	if p.TokenCmd == "" {
		if p.Token == "" {
			return "", errors.New("profile has neither token_cmd nor token")
		}
		return p.Token, nil
	}
	cmd := procutil.CommandContext(ctx, "sh", "-c", p.TokenCmd)
	// Do not wait indefinitely for a descendant that holds the output pipe after cancellation.
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("token_cmd cancelled: %w", ctx.Err())
		}
		return "", fmt.Errorf("token_cmd failed: %w", err)
	}
	t := strings.TrimSpace(string(out))
	if t == "" {
		return "", errors.New("token_cmd printed nothing")
	}
	return t, nil
}
