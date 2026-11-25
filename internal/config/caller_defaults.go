package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/zhoushoujianwork/offload/internal/history"
)

// ErrCallerDefaultInUse is returned when disabling or deleting a profile that a caller default
// still names. It is a fixed message so the profile-save JSON path may show it.
var ErrCallerDefaultInUse = errors.New("set another caller default first")

// NormalizeCallerDefaultKey canonicalizes a caller ID for caller_defaults. The synthetic
// "unspecified" caller of an untagged run is refused, because a default for it could never apply.
func NormalizeCallerDefaultKey(caller string) (string, error) {
	n, err := history.NormalizeCaller(caller)
	if err != nil {
		return "", err
	}
	if n == history.CallerUnspecified {
		return "", errors.New(`caller "unspecified" cannot have a default profile`)
	}
	return n, nil
}

// ValidateCallerDefaults checks the shape of caller_defaults. It does not require the named
// profiles to exist, so a hand-deleted profile fails only the runs that need it.
func (c *Config) ValidateCallerDefaults() error {
	for _, caller := range sortedKeys(c.CallerDefaults) {
		n, err := NormalizeCallerDefaultKey(caller)
		if err != nil || n != caller {
			return fmt.Errorf("caller_defaults: invalid caller %q (want canonical lowercase [a-z0-9][a-z0-9._-]{0,63})", caller)
		}
		if strings.TrimSpace(c.CallerDefaults[caller]) == "" {
			return fmt.Errorf("caller_defaults[%q]: profile name must not be empty", caller)
		}
	}
	return nil
}

// DefaultFor returns the profile a new run uses when neither -p nor OFFLOAD_PROFILE chose one.
// A caller that was explicitly supplied and has a mapping wins, and mapped is then true. An empty
// caller or the synthetic "unspecified" never matches, so untagged runs keep the global default.
// The mapped name is returned as stored and is not checked here: the run resolves it like -p, so a
// missing or disabled profile fails the run instead of silently falling back.
func (c *Config) DefaultFor(caller string) (name string, mapped bool) {
	if caller != "" && caller != history.CallerUnspecified {
		if n := c.CallerDefaults[caller]; n != "" {
			return n, true
		}
	}
	return c.EffectiveDefault(), false
}

// CallersUsing lists, in order, the callers whose default is the profile, by canonical name or alias.
func (c *Config) CallersUsing(profile string) []string {
	var callers []string
	for _, caller := range sortedKeys(c.CallerDefaults) {
		if namesProfile(c.CallerDefaults[caller], profile, c.Profiles[profile].Aliases) {
			callers = append(callers, caller)
		}
	}
	return callers
}

func namesProfile(ref, canonical string, aliases []string) bool {
	return ref == canonical || slices.Contains(aliases, ref)
}

// SetCallerDefault saves the default profile for one caller. The profile may be an alias; its
// canonical name is stored. The profile must exist and be enabled. Other fields, including unknown
// JSON, are preserved.
func SetCallerDefault(path, caller, name string) error {
	key, err := NormalizeCallerDefaultKey(caller)
	if err != nil {
		return err
	}
	if strings.TrimSpace(name) == "" {
		return errors.New("default profile name must not be empty")
	}
	c, err := Load(path)
	if err != nil {
		return err
	}
	resolved, profile, err := c.Resolve(name)
	if err != nil {
		return err
	}
	if err := profile.CheckEnabled(resolved); err != nil {
		return err
	}
	fields, err := modeSettings(path)
	if err != nil {
		return err
	}
	defaults := map[string]string{}
	if raw, ok := fields["caller_defaults"]; ok {
		if err := json.Unmarshal(raw, &defaults); err != nil || defaults == nil {
			return errors.New("caller_defaults must be a JSON object of profile names")
		}
	}
	defaults[key] = resolved
	if fields["caller_defaults"], err = json.Marshal(defaults); err != nil {
		return err
	}
	return writeSettings(path, fields)
}

// callerDefaultsUse reports whether any stored caller default names the profile, by canonical name
// or alias.
func callerDefaultsUse(fields map[string]json.RawMessage, profile string) (bool, error) {
	raw, ok := fields["caller_defaults"]
	if !ok {
		return false, nil
	}
	var defaults map[string]string
	if err := json.Unmarshal(raw, &defaults); err != nil {
		return false, errors.New("caller_defaults must be a JSON object of profile names")
	}
	var profiles map[string]struct {
		Aliases []string `json:"aliases"`
	}
	if raw, ok := fields["profiles"]; ok {
		_ = json.Unmarshal(raw, &profiles) // a malformed profiles object is reported by the caller
	}
	for _, ref := range defaults {
		if namesProfile(ref, profile, profiles[profile].Aliases) {
			return true, nil
		}
	}
	return false, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
