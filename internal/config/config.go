// Package config loads worker backend profiles.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Profile selects a worker backend and its model/configuration.
type Profile struct {
	Backend string `json:"backend,omitempty"` // claude (default) or codex
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
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
	return &c, nil
}

// Resolve picks a profile by name ("" means OFFLOAD_PROFILE, then config default).
func (c *Config) Resolve(name string) (string, Profile, error) {
	if name == "" {
		name = os.Getenv("OFFLOAD_PROFILE")
	}
	if name == "" {
		name = c.Default
	}
	p, ok := c.Profiles[name]
	if !ok {
		return name, p, fmt.Errorf("unknown profile %q", name)
	}
	if p.BackendName() != "claude" && p.BackendName() != "codex" {
		return name, p, fmt.Errorf("profile %q has unknown backend %q", name, p.Backend)
	}
	if p.BackendName() == "claude" && (p.BaseURL == "" || p.Model == "") {
		return name, p, fmt.Errorf("profile %q needs base_url and model", name)
	}
	return name, p, nil
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
	if _, _, err := c.Resolve(name); err != nil {
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
	fields["default"], _ = json.Marshal(name)
	b, err = json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// ResolveToken returns the profile's token, running token_cmd when set.
func (p Profile) ResolveToken() (string, error) {
	if p.TokenCmd == "" {
		if p.Token == "" {
			return "", errors.New("profile has neither token_cmd nor token")
		}
		return p.Token, nil
	}
	out, err := exec.Command("sh", "-c", p.TokenCmd).Output()
	if err != nil {
		return "", fmt.Errorf("token_cmd failed: %w", err)
	}
	t := strings.TrimSpace(string(out))
	if t == "" {
		return "", errors.New("token_cmd printed nothing")
	}
	return t, nil
}
