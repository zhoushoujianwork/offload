package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SetProfileDisabled updates only the administrative switch, preserving all other fields.
func SetProfileDisabled(path, name string, disabled bool) error {
	fields, err := modeSettings(path)
	if err != nil {
		return err
	}
	var def string
	if raw := fields["default"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &def); err != nil {
			return err
		}
	}
	if disabled && name == def {
		return fmt.Errorf("set another default first")
	}
	var profiles map[string]json.RawMessage
	if err := json.Unmarshal(fields["profiles"], &profiles); err != nil {
		return err
	}
	raw, ok := profiles[name]
	if !ok {
		return fmt.Errorf("unknown profile")
	}
	var p map[string]json.RawMessage
	if err := json.Unmarshal(raw, &p); err != nil || p == nil {
		return fmt.Errorf("profile must be a JSON object")
	}
	p["disabled"], _ = json.Marshal(disabled)
	profiles[name], _ = json.Marshal(p)
	fields["profiles"], _ = json.Marshal(profiles)
	return writeSettings(path, fields)
}

// DeleteProfile removes a profile and its aliases without rewriting its neighbors.
func DeleteProfile(path, name string) error {
	fields, err := modeSettings(path)
	if err != nil {
		return err
	}
	var def string
	if raw := fields["default"]; len(raw) != 0 {
		if err := json.Unmarshal(raw, &def); err != nil {
			return err
		}
	}
	if name == def {
		return fmt.Errorf("set another default first")
	}
	var profiles map[string]json.RawMessage
	if err := json.Unmarshal(fields["profiles"], &profiles); err != nil {
		return err
	}
	if _, ok := profiles[name]; !ok {
		return fmt.Errorf("unknown profile")
	}
	delete(profiles, name)
	fields["profiles"], _ = json.Marshal(profiles)
	return writeSettings(path, fields)
}

// RemovableAccountHome validates directory ownership without requiring a sign-in.
func RemovableAccountHome(name string) (string, error) {
	if !accountName.MatchString(name) {
		return "", fmt.Errorf("invalid managed account name")
	}
	root := filepath.Join(Dir(), "codex-accounts")
	home := filepath.Join(root, name)
	for _, path := range []string{root, home} {
		st, err := os.Lstat(path)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("managed account must be a real directory")
		}
	}
	return home, nil
}

// RemoveManagedAccount removes the directory tree; RemoveAll unlinks symlinks, never their targets.
func RemoveManagedAccount(name string) error {
	home, err := RemovableAccountHome(name)
	if err != nil {
		return err
	}
	return os.RemoveAll(home)
}
