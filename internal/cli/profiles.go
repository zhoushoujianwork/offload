package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/zhoushoujianwork/offload/internal/config"
)

const (
	profilesSchemaVersion = 1
	maxProfileRequest     = 64 << 10
	profilesUsage         = "usage: offload profiles [--json]"
	profileSaveUsage      = "usage: offload profile save --json   (JSON request on stdin)"
)

// profileEntry is one profile in the machine-readable list. It never carries token or token_cmd.
type profileEntry struct {
	Name            string `json:"name"`
	Backend         string `json:"backend"`
	BaseURL         string `json:"base_url"`
	BaseURLRedacted bool   `json:"base_url_redacted"`
	Model           string `json:"model"`
	Endpoint        string `json:"endpoint"`
	ReasoningEffort string `json:"reasoning_effort"`
	ServiceTier     string `json:"service_tier"`
	AuthMode        string `json:"auth_mode"`
}

// profileSnapshot is the `offload profiles --json` document (schema_version 1).
type profileSnapshot struct {
	SchemaVersion     int            `json:"schema_version"`
	ConfigExists      bool           `json:"config_exists"`
	ConfiguredDefault string         `json:"configured_default"`
	Active            string         `json:"active"`
	ActiveSource      string         `json:"active_source"`
	DelegationMode    string         `json:"delegation_mode"`
	Profiles          []profileEntry `json:"profiles"`
	Warnings          []string       `json:"warnings"`
}

var errConfigUnreadable = errors.New("config could not be read or is invalid")

// safeBaseURL returns the URL only when it carries no userinfo, query or fragment, so private
// parameters never leave the config. The second result reports that a non-empty value was withheld.
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

func authMode(p config.Profile) string {
	switch {
	case p.TokenCmd != "":
		return "token_cmd"
	case p.Token != "":
		return "token"
	}
	return "none"
}

// buildProfileSnapshot reads the config at path. A missing file is not an error. It does not
// run token_cmd and never copies credentials into the result.
func buildProfileSnapshot(path string) (profileSnapshot, error) {
	snap := profileSnapshot{SchemaVersion: profilesSchemaVersion, ActiveSource: "none",
		DelegationMode: config.ModeProactive, Profiles: []profileEntry{}, Warnings: []string{}}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return snap, nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return snap, errConfigUnreadable
	}
	snap.ConfigExists = true
	snap.ConfiguredDefault = cfg.Default
	if cfg.DelegationMode != "" {
		if config.ValidMode(cfg.DelegationMode) {
			snap.DelegationMode = cfg.DelegationMode
		} else {
			snap.Warnings = append(snap.Warnings, "The configured delegation mode is not recognized.")
		}
	}
	names := make([]string, 0, len(cfg.Profiles))
	for n := range cfg.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := cfg.Profiles[n]
		base, redacted := safeBaseURL(p.BaseURL)
		snap.Profiles = append(snap.Profiles, profileEntry{Name: n, Backend: p.BackendName(), BaseURL: base,
			BaseURLRedacted: redacted, Model: p.Model, Endpoint: p.Endpoint,
			ReasoningEffort: p.ReasoningEffort, ServiceTier: p.ServiceTier, AuthMode: authMode(p)})
	}
	envName := os.Getenv("OFFLOAD_PROFILE")
	switch {
	case envName != "":
		snap.ActiveSource = "environment"
	case cfg.Default != "":
		snap.ActiveSource = "config"
	}
	if name, _, err := cfg.Resolve(""); err == nil {
		snap.Active = name
	} else if snap.ActiveSource == "environment" {
		snap.Warnings = append(snap.Warnings, "OFFLOAD_PROFILE does not select a usable profile.")
	} else {
		snap.Warnings = append(snap.Warnings, "The configured default profile is missing or not usable.")
	}
	return snap, nil
}

func writeSnapshot(stdout, stderr io.Writer, snap profileSnapshot) int {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(snap); err != nil {
		return 2
	}
	return writeAll(stdout, stderr, buf.Bytes())
}

// writeAll reports a failed or short write with a fixed message that carries no output content.
func writeAll(w, stderr io.Writer, b []byte) int {
	if n, err := w.Write(b); err != nil || n != len(b) {
		return fail(stderr, errors.New("could not write output"))
	}
	return 0
}

// cmdProfilesArgs keeps the plain-text listing for no arguments and adds --json.
func cmdProfilesArgs(args []string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 0:
		return cmdProfiles(stdout, stderr)
	case len(args) == 1 && args[0] == "--json":
		snap, err := buildProfileSnapshot(config.Path())
		if err != nil {
			return fail(stderr, err)
		}
		return writeSnapshot(stdout, stderr, snap)
	}
	return fail(stderr, errors.New(profilesUsage))
}

// profileSaveRequest is the stdin document of `offload profile save --json`. Pointer fields
// are patches: omitted (or null) keeps the stored value.
type profileSaveRequest struct {
	Name            string  `json:"name"`
	Create          bool    `json:"create"`
	Backend         *string `json:"backend"`
	BaseURL         *string `json:"base_url"`
	Model           *string `json:"model"`
	Endpoint        *string `json:"endpoint"`
	ReasoningEffort *string `json:"reasoning_effort"`
	ServiceTier     *string `json:"service_tier"`
	AuthMode        *string `json:"auth_mode"`
	Secret          *string `json:"secret"`
}

var (
	errRequestTooLarge = errors.New("request exceeds 64 KiB")
	errRequestInvalid  = errors.New("invalid profile request: expected one JSON object with known fields")
)

func readProfileRequest(stdin io.Reader) (profileSaveRequest, error) {
	var req profileSaveRequest
	b, err := io.ReadAll(io.LimitReader(stdin, maxProfileRequest+1))
	if err != nil {
		return req, errRequestInvalid
	}
	if len(b) > maxProfileRequest {
		return req, errRequestTooLarge
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return profileSaveRequest{}, errRequestInvalid
	}
	if _, err := dec.Token(); err != io.EOF {
		return profileSaveRequest{}, errRequestInvalid
	}
	return req, nil
}

// safeSaveErrors are the fixed config validation messages that may be shown to the caller.
// Anything else is replaced so request values, paths and parser details never reach the output.
var safeSaveErrors = map[string]bool{
	"profile name must not be empty":                              true,
	"profile already exists":                                      true,
	"unknown profile":                                             true,
	"unknown backend":                                             true,
	"invalid Codex reasoning effort":                              true,
	"invalid Codex service tier":                                  true,
	"endpoint applies only to claude profiles":                    true,
	"base URL must be http(s) with a host":                        true,
	"model is required":                                           true,
	"authentication is required":                                  true,
	"unknown endpoint (want cc or llm)":                           true,
	"secret is required for token or token_cmd":                   true,
	"secret is only allowed with token or token_cmd":              true,
	"unknown auth_mode (want preserve, token, token_cmd or none)": true,
}

func safeSaveError(err error) error {
	if safeSaveErrors[err.Error()] {
		return err
	}
	return errors.New("profile could not be saved")
}

// applyProfileRequest builds the complete profile from the stored one plus the request patch.
func applyProfileRequest(p config.Profile, req profileSaveRequest) (config.Profile, error) {
	if req.Create && req.Backend == nil {
		p.Backend = "claude"
	}
	if req.Backend != nil {
		p.Backend = *req.Backend
	}
	if req.BaseURL != nil {
		p.BaseURL = strings.TrimSpace(*req.BaseURL)
	}
	if req.Model != nil {
		p.Model = strings.TrimSpace(*req.Model)
	}
	if req.Endpoint != nil {
		p.Endpoint = *req.Endpoint
	}
	if !config.ValidEndpoint(p.Endpoint) {
		return p, errors.New("unknown endpoint (want cc or llm)")
	}
	if req.ReasoningEffort != nil {
		p.ReasoningEffort = *req.ReasoningEffort
	}
	if req.ServiceTier != nil {
		p.ServiceTier = *req.ServiceTier
	}
	mode := "preserve"
	if req.AuthMode != nil {
		mode = *req.AuthMode
	}
	secret := ""
	if req.Secret != nil {
		secret = strings.TrimSpace(*req.Secret)
	}
	switch mode {
	case "preserve", "none":
		if req.Secret != nil {
			return p, errors.New("secret is only allowed with token or token_cmd")
		}
		if mode == "none" {
			p.Token, p.TokenCmd = "", ""
		}
	case "token", "token_cmd":
		if secret == "" {
			return p, errors.New("secret is required for token or token_cmd")
		}
		p.Token, p.TokenCmd = "", ""
		if mode == "token" {
			p.Token = secret
		} else {
			p.TokenCmd = secret
		}
	default:
		return p, errors.New("unknown auth_mode (want preserve, token, token_cmd or none)")
	}
	return p, nil
}

// cmdProfile handles `offload profile save --json`. Creating selects the new profile as default
// (SaveProfile semantics); editing keeps the saved default. Names are exact and immutable.
func cmdProfile(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 2 || args[0] != "save" || args[1] != "--json" {
		return fail(stderr, errors.New(profileSaveUsage))
	}
	req, err := readProfileRequest(stdin)
	if err != nil {
		return fail(stderr, err)
	}
	path := config.Path()
	var base config.Profile
	if !req.Create {
		cfg, err := config.Load(path)
		if err != nil {
			return fail(stderr, errors.New("unknown profile"))
		}
		p, ok := cfg.Profiles[req.Name]
		if !ok {
			return fail(stderr, errors.New("unknown profile"))
		}
		base = p
	}
	profile, err := applyProfileRequest(base, req)
	if err != nil {
		return fail(stderr, safeSaveError(err))
	}
	if err := config.SaveProfile(path, req.Name, profile, req.Create); err != nil {
		return fail(stderr, safeSaveError(err))
	}
	snap, err := buildProfileSnapshot(path)
	if err != nil {
		return fail(stderr, err)
	}
	return writeSnapshot(stdout, stderr, snap)
}
