package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// LoadMode does not require worker profiles or create a config file.
func LoadMode(path string) (string, error) {
	fields, err := modeSettings(path)
	if err != nil {
		return "", err
	}
	var c Config
	if raw, ok := fields["delegation_mode"]; ok {
		if err := json.Unmarshal(raw, &c.DelegationMode); err != nil {
			return "", fmt.Errorf("invalid delegation_mode: %w", err)
		}
	}
	mode := c.EffectiveMode()
	if !ValidMode(mode) {
		return "", fmt.Errorf("unknown delegation mode %q (want basic or proactive)", mode)
	}
	return mode, nil
}

func modeSettings(path string) (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if fields == nil {
		return nil, fmt.Errorf("config %s must be a JSON object", path)
	}
	return fields, nil
}

// SetMode preserves every existing field, including unknown provider settings.
func SetMode(path, mode string) error {
	if !ValidMode(mode) {
		return fmt.Errorf("unknown delegation mode %q (want basic or proactive)", mode)
	}
	fields, err := modeSettings(path)
	if err != nil {
		return err
	}
	var existing string
	if raw, ok := fields["delegation_mode"]; ok && json.Unmarshal(raw, &existing) == nil && existing == mode {
		return nil
	}
	fields["delegation_mode"], _ = json.Marshal(mode)
	return writeSettings(path, fields)
}

func writeSettings(path string, fields map[string]json.RawMessage) error {
	b, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
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

// ErrDisabled is stable across profiles and delegation modes.
var ErrDisabled = errors.New("offload is globally disabled; complete work directly or run offload enable")

func LoadDisabled(path string) (bool, error) {
	fields, err := modeSettings(path)
	if err != nil {
		return false, err
	}
	var disabled bool
	if raw, ok := fields["disabled"]; ok {
		if err := json.Unmarshal(raw, &disabled); err != nil {
			return false, fmt.Errorf("invalid disabled: %w", err)
		}
	}
	return disabled, nil
}

func CheckEnabled() error {
	disabled, err := LoadDisabled(Path())
	if err != nil {
		return err
	}
	if disabled {
		return ErrDisabled
	}
	return nil
}

// SetDisabled preserves profiles, credentials, mode and unknown settings.
func SetDisabled(path string, disabled bool) error {
	fields, err := modeSettings(path)
	if err != nil {
		return err
	}
	fields["disabled"], _ = json.Marshal(disabled)
	return writeSettings(path, fields)
}
