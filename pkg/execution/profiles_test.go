package execution_test

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/cli"
	"github.com/zhoushoujianwork/offload/pkg/execution"
)

const profilesFixture = `{
  "default": "alpha",
  "profiles": {
    "alpha": {"backend": "claude", "model": "m1", "base_url": "https://example.test/v1", "token": "SECRET-TOKEN-1"},
    "beta": {"backend": "codex", "model": "m2", "base_url": "https://user:SECRET-URL@example.test/?k=SECRET-Q", "token_cmd": "echo SECRET-CMD"}
  }
}
`

func setupProfiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("OFFLOAD_PROFILE", "")
	path := execution.DefaultConfigPath()
	if path != filepath.Join(dir, "offload", "config.json") {
		t.Fatalf("path = %q", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(profilesFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCLI(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	code := cli.Run("test", args, strings.NewReader(""), &out, &errb)
	return out.String(), errb.String(), code
}

func TestProfilesMatchCLI(t *testing.T) {
	path := setupProfiles(t)
	out, _, code := runCLI(t, "profiles", "--json")
	if code != 0 {
		t.Fatalf("cli exit %d", code)
	}
	var snap struct {
		Profiles []struct {
			Name, Backend, Model, Endpoint, BaseURL, AuthMode string
			BaseURLRedacted                                   bool   `json:"base_url_redacted"`
			ReasoningEffort                                   string `json:"reasoning_effort"`
			ServiceTier                                       string `json:"service_tier"`
			CodexAccount                                      string `json:"codex_account"`
			ToolRestrictions                                  bool   `json:"tool_restrictions"`
		} `json:"profiles"`
		ConfiguredDefault string `json:"configured_default"`
	}
	if err := json.Unmarshal([]byte(out), &snap); err != nil {
		t.Fatal(err)
	}
	// Decode with the CLI's JSON key names for the multi-word fields.
	var raw struct {
		Profiles []map[string]any `json:"profiles"`
	}
	_ = json.Unmarshal([]byte(out), &raw)
	list, err := execution.ListProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != len(raw.Profiles) || len(list) != 2 {
		t.Fatalf("got %d profiles, cli %d", len(list), len(raw.Profiles))
	}
	for i, p := range list {
		c := raw.Profiles[i]
		if c["name"] != p.Name || c["backend"] != p.Backend || c["model"] != p.Model || c["base_url"] != p.BaseURL ||
			c["base_url_redacted"] != p.BaseURLRedacted || c["auth_mode"] != p.AuthMode ||
			c["endpoint"] != p.Endpoint || c["reasoning_effort"] != p.ReasoningEffort ||
			c["service_tier"] != p.ServiceTier || c["codex_account"] != p.CodexAccount ||
			c["tool_restrictions"] != p.ToolRestrictions {
			t.Fatalf("profile %d differs: lib %+v cli %v", i, p, c)
		}
	}
	def, err := execution.DefaultProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	cliDef, _, _ := runCLI(t, "default")
	if def != "alpha" || strings.TrimSpace(cliDef) != def || snap.ConfiguredDefault != def {
		t.Fatalf("default lib %q cli %q", def, cliDef)
	}
	if err := execution.SetDefaultProfile(path, "beta"); err != nil {
		t.Fatal(err)
	}
	cliDef, _, _ = runCLI(t, "default")
	if strings.TrimSpace(cliDef) != "beta" {
		t.Fatalf("cli default after set = %q", cliDef)
	}
}

func TestProfilesNoCredentials(t *testing.T) {
	path := setupProfiles(t)
	list, err := execution.ListProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(list)
	// Read the credential values from the fixture itself so the check does not depend on their text.
	raw, _ := os.ReadFile(path)
	var doc struct {
		Profiles map[string]map[string]string `json:"profiles"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var secrets []string
	for _, p := range doc.Profiles {
		for _, k := range []string{"token", "token_cmd"} {
			if p[k] != "" {
				secrets = append(secrets, p[k])
			}
		}
		if u, err := url.Parse(p["base_url"]); err == nil {
			if u.User != nil {
				secrets = append(secrets, u.User.Username())
			}
			if u.RawQuery != "" {
				secrets = append(secrets, u.RawQuery)
			}
		}
	}
	if len(secrets) != 4 {
		t.Fatalf("fixture credentials not found: %d", len(secrets))
	}
	check := func(s string) {
		t.Helper()
		for _, sec := range secrets {
			if strings.Contains(s, sec) {
				t.Fatalf("credential leaked in %q", s)
			}
		}
	}
	check(string(b))
	check(string(bytes.TrimSpace([]byte(strings.Join([]string{list[0].BaseURL, list[1].BaseURL}, " ")))))
	if !list[1].BaseURLRedacted || list[1].AuthMode != "token_cmd" || list[0].AuthMode != "token" {
		t.Fatalf("unexpected %+v", list)
	}
	// Errors from a bad profile, an invalid file and a bad path stay credential-free.
	err = execution.SetDefaultProfile(path, "missing")
	if err == nil {
		t.Fatal("expected error")
	}
	check(err.Error())
	if err := os.WriteFile(path, []byte(`{"profiles": {"a": {"token": "SECRET-TOKEN-2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = execution.ListProfiles(path)
	if err == nil {
		t.Fatal("expected error")
	}
	check(err.Error())
	_, err = execution.DefaultProfile(path)
	if err == nil {
		t.Fatal("expected error")
	}
	check(err.Error())
	if _, err := execution.ListProfiles("relative.json"); err == nil {
		t.Fatal("expected relative path error")
	}
}

func TestSetDefaultProfileUnknownLeavesFileUnchanged(t *testing.T) {
	path := setupProfiles(t)
	before, _ := os.ReadFile(path)
	if err := execution.SetDefaultProfile(path, "nope"); err == nil {
		t.Fatal("expected error")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("config changed")
	}
	if err := execution.SetDefaultProfile(path, ""); err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestProfilesSymlinkAndPermissionsMatchCLI(t *testing.T) {
	path := setupProfiles(t)
	// The CLI follows a symlinked config and rewrites it by atomic rename; the library
	// shares that code, so both must behave identically.
	real := filepath.Join(t.TempDir(), "real.json")
	if err := os.Rename(path, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, path); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := os.Chmod(real, 0o644); err != nil {
		t.Fatal(err)
	}
	libDef, libErr := execution.DefaultProfile(path)
	cliOut, _, code := runCLI(t, "default")
	if (libErr == nil) != (code == 0) || (libErr == nil && libDef != strings.TrimSpace(cliOut)) {
		t.Fatalf("lib %q/%v cli %q/%d", libDef, libErr, cliOut, code)
	}
	libSet := execution.SetDefaultProfile(path, "beta")
	_, _, code = runCLI(t, "default", "alpha")
	if (libSet == nil) != (code == 0) {
		t.Fatalf("set lib %v cli %d", libSet, code)
	}
}

func TestListProfilesReportsAvailability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"default":"a","profiles":{"a":{"base_url":"https://a.example","model":"m","token_cmd":"echo a","unavailable":{"reason":"quota exhausted: out of credits","at":"2026-01-02T03:04:05Z"}},"b":{"base_url":"https://b.example","model":"m","token_cmd":"echo b"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err := execution.ListProfiles(path)
	if err != nil || len(list) != 2 {
		t.Fatalf("list=%v err=%v", list, err)
	}
	if list[0].Available || list[0].UnavailableReason != "quota exhausted: out of credits" || list[0].UnavailableAt != "2026-01-02T03:04:05Z" {
		t.Fatalf("a = %+v", list[0])
	}
	if !list[1].Available || list[1].UnavailableReason != "" || list[1].UnavailableAt != "" {
		t.Fatalf("b = %+v", list[1])
	}
}
