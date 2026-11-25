package execution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fingerprints of profiles that predate AuthMode. A new field must not change them, or every saved
// task that stores a fingerprint would refuse to resume after an upgrade.
const (
	legacyGatewayFingerprint = "fcfb909c380c62a1ef3e485b4591c6eba9c25176de69483dac602ecde3c25a0e"
	legacyCodexFingerprint   = "4504328f523fc5a739cb638c839b64f31bf92305596251e88031f07383d61357"
)

func TestProfileFingerprintIsStableForExistingProfilesAndSeparatesNative(t *testing.T) {
	gateway := Config{ProfileName: "p", Profile: Profile{Backend: "claude", BaseURL: "https://gw.example", Model: "m", Token: "tok"}}
	codex := Config{ProfileName: "c", Profile: Profile{Backend: "codex", Model: "m2", ServiceTier: "priority"}}
	if got := ProfileFingerprint(gateway); got != legacyGatewayFingerprint {
		t.Fatalf("gateway fingerprint changed: %s", got)
	}
	if got := ProfileFingerprint(codex); got != legacyCodexFingerprint {
		t.Fatalf("codex fingerprint changed: %s", got)
	}
	native := Config{ProfileName: "p", Profile: Profile{Backend: "claude", Model: "m", AuthMode: "native"}}
	bare := Config{ProfileName: "p", Profile: Profile{Backend: "claude", Model: "m"}}
	if ProfileFingerprint(native) == ProfileFingerprint(bare) {
		t.Fatal("a native profile must not share a fingerprint with the same profile in gateway mode")
	}
}

func TestNewValidatesNativeProfile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile Profile
		want    string
	}{
		{"valid", Profile{Backend: "claude", Model: "sonnet", AuthMode: "native"}, ""},
		{"legacy backend", Profile{Model: "sonnet", AuthMode: "native"}, ""},
		{"no model", Profile{Backend: "claude", AuthMode: "native"}, "model is required"},
		{"base url", Profile{Backend: "claude", Model: "sonnet", AuthMode: "native", BaseURL: "https://gw"}, "cannot be combined"},
		{"token", Profile{Backend: "claude", Model: "sonnet", AuthMode: "native", Token: "t"}, "cannot be combined"},
		{"codex", Profile{Backend: "codex", Model: "m", AuthMode: "native"}, "only to claude"},
		{"unknown", Profile{Backend: "claude", Model: "m", BaseURL: "https://gw", Token: "t", AuthMode: "oauth"}, "unknown profile auth_mode"},
		{"gateway still needs base url", Profile{Backend: "claude", Model: "m", Token: "t"}, "needs base_url and model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Config{ProfileName: "p", Profile: tc.profile})
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
}

func TestLoadConfigAndListProfilesCarryNativeAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"default":"gw","profiles":{` +
		`"gw":{"base_url":"https://gw.example","model":"m","token":"SECRET-TOKEN"},` +
		`"claude-sonnet":{"auth_mode":"native","model":"sonnet","aliases":["son"]}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, "son")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProfileName != "claude-sonnet" || cfg.Profile.AuthMode != "native" || cfg.Profile.Model != "sonnet" || cfg.Profile.BaseURL != "" {
		t.Fatalf("%+v", cfg)
	}
	if _, err := New(cfg); err != nil {
		t.Fatal(err)
	}
	gw, err := LoadConfig(path, "gw")
	if err != nil || gw.Profile.AuthMode != "" || ProfileFingerprint(gw) == ProfileFingerprint(cfg) {
		t.Fatalf("%+v %v", gw, err)
	}
	infos, err := ListProfiles(path)
	if err != nil {
		t.Fatal(err)
	}
	modes := map[string]string{}
	for _, i := range infos {
		modes[i.Name] = i.AuthMode
	}
	if modes["claude-sonnet"] != "native" || modes["gw"] != "token" {
		t.Fatalf("%v", modes)
	}
}

func TestNativeRunStartsWithoutGatewayAndStripsAmbientCredentials(t *testing.T) {
	t.Setenv("OFFLOAD_MAX_WORKERS", "-1")
	t.Setenv("ANTHROPIC_API_KEY", "ambient-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient-token")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1") // unreachable: a preflight would fail the run
	t.Setenv("ANTHROPIC_MODEL", "ambient-model")
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte(envDumpClaude), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CLAUDE", bin)
	dump := filepath.Join(t.TempDir(), "env")
	t.Setenv("ENV_DUMP", dump)
	c, err := New(Config{ProfileName: "claude-sonnet", Profile: Profile{Backend: "claude", Model: "sonnet", AuthMode: "native"}})
	if err != nil {
		t.Fatal(err)
	}
	timeout := time.Minute
	res, err := c.Run(context.Background(), Request{Directory: t.TempDir(), Task: "t", Timeout: &timeout})
	if err != nil || res.Status != "ok" || res.Backend != "claude" {
		t.Fatalf("%+v %v", res, err)
	}
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ANTHROPIC_") || strings.Contains(string(raw), "ambient") {
		t.Fatalf("a native run reached a provider variable:\n%s", raw)
	}
}
