package execution

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zhoushoujianwork/offload/internal/config"
)

// ProfileInfo describes one saved profile without any credential. It carries
// neither a token nor a token command; AuthMode only names which kind is set.
type ProfileInfo struct {
	Name, Backend, Model, Endpoint, ReasoningEffort, ServiceTier string
	CodexAccount                                                 string
	// BaseURL is empty when the stored value carries userinfo, a query or a
	// fragment; BaseURLRedacted then reports that a value was withheld.
	BaseURL         string
	BaseURLRedacted bool
	// AuthMode is "token", "token_cmd" or "none".
	AuthMode string
	// ToolRestrictions reports whether the backend enforces tool restrictions.
	ToolRestrictions bool
}

// DefaultConfigPath returns the standard config path that the offload CLI uses
// (honoring XDG_CONFIG_HOME). The functions below take the path explicitly.
func DefaultConfigPath() string { return config.Path() }

var errProfilePath = errors.New("config path must be absolute")
var errConfigUnreadable = errors.New("config could not be read or is invalid")

// ListProfiles lists saved profiles sorted by name, like `offload profiles --json`.
// A missing config file yields an empty list. It never runs token commands.
func ListProfiles(path string) ([]ProfileInfo, error) {
	if !filepath.IsAbs(path) {
		return nil, errProfilePath
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return []ProfileInfo{}, nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, errConfigUnreadable
	}
	names := make([]string, 0, len(cfg.Profiles))
	for n := range cfg.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]ProfileInfo, 0, len(names))
	for _, n := range names {
		p := cfg.Profiles[n]
		base, redacted := safeBaseURL(p.BaseURL)
		out = append(out, ProfileInfo{Name: n, Backend: p.BackendName(), Model: p.Model, Endpoint: p.Endpoint,
			ReasoningEffort: p.ReasoningEffort, ServiceTier: p.ServiceTier, CodexAccount: p.CodexAccount,
			BaseURL: base, BaseURLRedacted: redacted, AuthMode: profileAuthMode(p),
			ToolRestrictions: p.SupportsToolRestrictions()})
	}
	return out, nil
}

// DefaultProfile returns the saved default profile name, as `offload default` prints it.
func DefaultProfile(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errProfilePath
	}
	cfg, err := config.Load(path)
	if err != nil {
		return "", err
	}
	return cfg.Default, nil
}

// SetDefaultProfile saves the default profile through the same code path as
// `offload default <profile>`. An unknown profile fails and leaves the file unchanged.
func SetDefaultProfile(path, name string) error {
	if !filepath.IsAbs(path) {
		return errProfilePath
	}
	return config.SetDefault(path, name)
}

func profileAuthMode(p config.Profile) string {
	switch {
	case p.TokenCmd != "":
		return "token_cmd"
	case p.Token != "":
		return "token"
	}
	return "none"
}

// safeBaseURL mirrors the CLI listing: withhold URLs that may carry private parameters.
func safeBaseURL(raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		strings.ContainsAny(raw, "?#") || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", true
	}
	return raw, false
}
