// Package config loads the gateway profiles a worker can run against.
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

// Profile is one Anthropic-compatible gateway the worker can run against.
type Profile struct {
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
	if p.BaseURL == "" || p.Model == "" {
		return name, p, fmt.Errorf("profile %q needs base_url and model", name)
	}
	return name, p, nil
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
