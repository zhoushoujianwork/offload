package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeAuthValidation(t *testing.T) {
	t.Setenv("OFFLOAD_PROFILE", "")
	for _, tc := range []struct {
		name    string
		profile Profile
		want    string // empty means valid
	}{
		{"native sonnet", Profile{AuthMode: "native", Model: "sonnet"}, ""},
		{"explicit claude backend", Profile{Backend: "claude", AuthMode: "native", Model: "sonnet"}, ""},
		{"missing model", Profile{AuthMode: "native"}, "model is required"},
		{"blank model", Profile{AuthMode: "native", Model: "  "}, "model is required"},
		{"base url", Profile{AuthMode: "native", Model: "sonnet", BaseURL: "https://gw"}, "cannot be combined"},
		{"token", Profile{AuthMode: "native", Model: "sonnet", Token: "t"}, "cannot be combined"},
		{"token cmd", Profile{AuthMode: "native", Model: "sonnet", TokenCmd: "echo t"}, "cannot be combined"},
		{"endpoint", Profile{AuthMode: "native", Model: "sonnet", Endpoint: "cc"}, "cannot be combined"},
		{"codex", Profile{Backend: "codex", AuthMode: "native", Model: "m"}, "only to claude"},
		{"unknown mode", Profile{AuthMode: "oauth", Model: "sonnet"}, "unknown profile auth_mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Default: "p", Profiles: map[string]Profile{"p": tc.profile}}
			_, _, err := c.Resolve("")
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
	// Legacy gateway profiles keep their requirement for a base URL.
	c := Config{Default: "g", Profiles: map[string]Profile{"g": {Model: "m"}}}
	if _, _, err := c.Resolve(""); err == nil || !strings.Contains(err.Error(), "needs base_url and model") {
		t.Fatalf("gateway profile without base_url: %v", err)
	}
}

func TestNativeAuthIsClaudeOnlyAndExact(t *testing.T) {
	for _, p := range []Profile{{AuthMode: "native", Model: "m"}, {Backend: "claude", AuthMode: "native", Model: "m"}} {
		if !p.UsesNativeAuth() {
			t.Fatalf("%+v should use native auth", p)
		}
	}
	for _, p := range []Profile{{}, {Backend: "codex", AuthMode: "native"}, {AuthMode: "", BaseURL: "https://gw", Model: "m"}} {
		if p.UsesNativeAuth() {
			t.Fatalf("%+v must not use native auth", p)
		}
	}
}

func TestSaveNativeProfile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	original := `{"default":"gw","future":{"keep":true},"profiles":{"gw":{"base_url":"https://gateway","model":"m","token":"secret"}}}`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveProfile(path, "claude-sonnet", Profile{AuthMode: "native", Model: "sonnet", UseFor: "official"}, true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{`"auth_mode": "native"`, `"model": "sonnet"`, `"keep": true`, `"token": "secret"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("saved config lost %q:\n%s", want, b)
		}
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	name, p, err := c.Resolve("claude-sonnet")
	if err != nil || name != "claude-sonnet" || !p.UsesNativeAuth() || p.BaseURL != "" || p.Token != "" || p.TokenCmd != "" {
		t.Fatalf("%q %+v %v", name, p, err)
	}
	if c.Default != "claude-sonnet" {
		t.Fatalf("adding selects the new profile: %q", c.Default)
	}
	for name, bad := range map[string]Profile{
		"base url":  {AuthMode: "native", Model: "sonnet", BaseURL: "https://gw"},
		"token":     {AuthMode: "native", Model: "sonnet", Token: "t"},
		"no model":  {AuthMode: "native"},
		"codex":     {Backend: "codex", AuthMode: "native"},
		"bad value": {AuthMode: "oauth", Model: "sonnet"},
	} {
		if err := SaveProfile(path, "bad-"+strings.ReplaceAll(name, " ", "-"), bad, true); err == nil {
			t.Errorf("%s: saved an invalid native profile", name)
		}
	}
	after, _ := os.ReadFile(path)
	if strings.Contains(string(after), "bad-") {
		t.Fatalf("a rejected profile reached the file:\n%s", after)
	}
}

func TestSaveGatewayProfileStillRequiresGatewaySettings(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := Path()
	for name, p := range map[string]Profile{
		"no base":  {Model: "m", Token: "t"},
		"no model": {BaseURL: "https://gw", Token: "t"},
		"no auth":  {BaseURL: "https://gw", Model: "m"},
	} {
		if err := SaveProfile(path, "g-"+strings.ReplaceAll(name, " ", "-"), p, true); err == nil {
			t.Errorf("%s: gateway profile saved without its requirements", name)
		}
	}
}

const callerCfg = `{"default":"gw","future":{"keep":true},"profiles":{` +
	`"gw":{"base_url":"https://gw","model":"m","token":"t"},` +
	`"sonnet":{"auth_mode":"native","model":"sonnet","aliases":["son"]},` +
	`"cx":{"backend":"codex","aliases":["codex-alias"]}}}`

func writeCallerCfg(t *testing.T, body string) string {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("OFFLOAD_PROFILE", "")
	path := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSetCallerDefaultStoresCanonicalNamePreservesFieldsAndNormalizes(t *testing.T) {
	path := writeCallerCfg(t, callerCfg)
	if err := SetCallerDefault(path, "Claude", "son"); err != nil {
		t.Fatal(err)
	}
	if err := SetCallerDefault(path, "codex", "codex-alias"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), `"keep": true`) {
		t.Fatalf("unknown fields lost:\n%s", b)
	}
	var stored struct {
		CallerDefaults map[string]string `json:"caller_defaults"`
	}
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.CallerDefaults["claude"] != "sonnet" || stored.CallerDefaults["codex"] != "cx" {
		t.Fatalf("an alias was stored instead of the canonical name: %v", stored.CallerDefaults)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.CallerDefaults["claude"] != "sonnet" || c.CallerDefaults["codex"] != "cx" || len(c.CallerDefaults) != 2 {
		t.Fatalf("aliases must be saved as canonical names with lowercase callers: %+v", c.CallerDefaults)
	}
	if c.Default != "gw" {
		t.Fatalf("a caller default must not change the global default: %q", c.Default)
	}
}

func TestSetCallerDefaultRejectsBadInputAndLeavesConfigUntouched(t *testing.T) {
	path := writeCallerCfg(t, callerCfg)
	before, _ := os.ReadFile(path)
	for _, tc := range []struct{ caller, profile string }{
		{"claude", "missing"},
		{"claude", ""},
		{"", "gw"},
		{"has space", "gw"},
		{"unspecified", "gw"},
		{"UNSPECIFIED", "gw"},
	} {
		if err := SetCallerDefault(path, tc.caller, tc.profile); err == nil {
			t.Errorf("SetCallerDefault(%q, %q) succeeded", tc.caller, tc.profile)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("rejected input modified the config:\n%s", after)
	}
	// A disabled profile cannot become a caller default.
	disabled := strings.Replace(callerCfg, `"cx":{"backend":"codex"`, `"cx":{"disabled":true,"backend":"codex"`, 1)
	path = writeCallerCfg(t, disabled)
	if err := SetCallerDefault(path, "codex", "cx"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled profile accepted: %v", err)
	}
}

func TestDefaultForOnlyMapsAnExplicitCaller(t *testing.T) {
	path := writeCallerCfg(t, callerCfg)
	if err := SetCallerDefault(path, "claude", "sonnet"); err != nil {
		t.Fatal(err)
	}
	c, _ := Load(path)
	for _, tc := range []struct {
		caller string
		want   string
		mapped bool
	}{
		{"claude", "sonnet", true},
		{"codex", "gw", false}, // unknown caller: global default
		{"", "gw", false},
		{"unspecified", "gw", false}, // the synthetic caller never matches
	} {
		got, mapped := c.DefaultFor(tc.caller)
		if got != tc.want || mapped != tc.mapped {
			t.Errorf("DefaultFor(%q) = %q,%v want %q,%v", tc.caller, got, mapped, tc.want, tc.mapped)
		}
	}
	// A stored unspecified key is refused at load, so it can never be hand-edited into effect.
	bad := strings.Replace(callerCfg, `"future"`, `"caller_defaults":{"unspecified":"gw"},"future"`, 1)
	if _, err := Load(writeCallerCfg(t, bad)); err == nil || !strings.Contains(err.Error(), "caller_defaults") {
		t.Fatalf("unspecified key accepted: %v", err)
	}
	for _, body := range []string{`"caller_defaults":{"Claude":"gw"}`, `"caller_defaults":{"claude":""}`} {
		if _, err := Load(writeCallerCfg(t, strings.Replace(callerCfg, `"future"`, body+`,"future"`, 1))); err == nil {
			t.Errorf("accepted %s", body)
		}
	}
}

func TestCallerDefaultBlocksDisableAndDelete(t *testing.T) {
	path := writeCallerCfg(t, callerCfg)
	if err := SetCallerDefault(path, "claude", "son"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := SetProfileDisabled(path, "sonnet", true); err != ErrCallerDefaultInUse {
		t.Fatalf("disable: %v", err)
	}
	if err := DeleteProfile(path, "sonnet"); err != ErrCallerDefaultInUse {
		t.Fatalf("delete: %v", err)
	}
	if err := SaveProfile(path, "sonnet", Profile{AuthMode: "native", Model: "sonnet", Disabled: true}, false); err != ErrCallerDefaultInUse {
		t.Fatalf("save disabled: %v", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("a refused change modified the config:\n%s", after)
	}
	// Enabling and editing stay allowed, and an unreferenced profile can still be disabled.
	if err := SetProfileDisabled(path, "sonnet", false); err != nil {
		t.Fatal(err)
	}
	if err := SetProfileDisabled(path, "cx", true); err != nil {
		t.Fatal(err)
	}
	c, _ := Load(path)
	if got := c.CallersUsing("sonnet"); len(got) != 1 || got[0] != "claude" {
		t.Fatalf("CallersUsing: %v", got)
	}
}
