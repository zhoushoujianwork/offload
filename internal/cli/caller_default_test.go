package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
)

// callerCfg: global default "glob"; "envp", "explicit", "claude-sonnet" (native, alias "son") and
// "cx" (Codex) are the other choices a run can land on.
const callerDefaultsCfg = `{"default":"glob","profiles":{` +
	`"glob":{"base_url":"https://glob.example","model":"mg","token":"t"},` +
	`"envp":{"base_url":"https://env.example","model":"me","token":"t"},` +
	`"explicit":{"base_url":"https://exp.example","model":"mx","token":"t"},` +
	`"claude-sonnet":{"auth_mode":"native","model":"sonnet","aliases":["son"]},` +
	`"off":{"disabled":true,"base_url":"https://off.example","model":"mo","token":"t"},` +
	`"cx":{"backend":"codex","aliases":["codex-alias"]}}}`

func writeCallerDefaultsCfg(t *testing.T, mutate func(string) string) {
	t.Helper()
	body := callerDefaultsCfg
	if mutate != nil {
		body = mutate(body)
	}
	if err := os.WriteFile(config.Path(), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func ranProfile(t *testing.T) string { t.Helper(); return lastRecord(t).Profile }

func TestCallerDefaultPrecedence(t *testing.T) {
	work, _ := setup(t, okReply)
	writeCallerDefaultsCfg(t, nil)
	for _, args := range [][]string{{"default", "--caller", "claude", "claude-sonnet"}} {
		if code, _, e := runCLI(args...); code != 0 {
			t.Fatal(e)
		}
	}
	run := func(env string, args ...string) string {
		t.Helper()
		t.Setenv("OFFLOAD_PROFILE", env)
		if code, out, e := runCLI(append(append([]string{"-d", work}, args...), "task")...); code != 0 {
			t.Fatalf("code=%d out=%q err=%q", code, out, e)
		}
		return ranProfile(t)
	}
	for _, tc := range []struct {
		name string
		env  string
		args []string
		want string
	}{
		{"explicit -p beats env, caller and global", "envp", []string{"-p", "explicit", "--caller", "claude"}, "explicit"},
		{"explicit alias is canonicalized", "", []string{"-p", "son", "--caller", "claude"}, "claude-sonnet"},
		{"OFFLOAD_PROFILE beats caller default", "envp", []string{"--caller", "claude"}, "envp"},
		{"caller default beats global", "", []string{"--caller", "claude"}, "claude-sonnet"},
		{"caller is normalized to lowercase", "", []string{"--caller", "CLAUDE"}, "claude-sonnet"},
		{"no caller uses the global default", "", nil, "glob"},
		{"unknown caller uses the global default", "", []string{"--caller", "other"}, "glob"},
		{"explicit unspecified never matches", "", []string{"--caller", "unspecified"}, "glob"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := run(tc.env, tc.args...); got != tc.want {
				t.Fatalf("ran on %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCallerDefaultNativeRunUsesOfficialLoginPath(t *testing.T) {
	work, record := setup(t, okReply)
	writeCallerDefaultsCfg(t, nil)
	if code, _, e := runCLI("default", "--caller", "claude", "son"); code != 0 {
		t.Fatal(e)
	}
	if code, _, e := runCLI("-d", work, "--caller", "claude", "inspect"); code != 0 {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(record)
	got := string(b)
	if strings.Contains(got, "--bare") || !strings.Contains(got, "--model\nsonnet\n") ||
		!strings.Contains(got, "token= base= model= key= hdr=") {
		t.Fatalf("native run reached a gateway, used --bare or sent a credential:\n%s", got)
	}
	if r := lastRecord(t); r.Profile != "claude-sonnet" || r.Backend != "claude" || r.Model != "sonnet" || r.Status != "ok" {
		t.Fatalf("%+v", r)
	}
}

func TestCallerDefaultMissingOrDisabledProfileFailsInsteadOfFallingBack(t *testing.T) {
	work, record := setup(t, okReply)
	for name, mutate := range map[string]func(string) string{
		"missing": func(s string) string {
			return strings.Replace(s, `"default":"glob"`, `"default":"glob","caller_defaults":{"claude":"gone"}`, 1)
		},
		"disabled": func(s string) string {
			return strings.Replace(s, `"default":"glob"`, `"default":"glob","caller_defaults":{"claude":"off"}`, 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			writeCallerDefaultsCfg(t, mutate)
			os.Remove(record)
			code, _, e := runCLI("-d", work, "--caller", "claude", "task")
			if code != 2 || !strings.Contains(e, "default profile for --caller claude") {
				t.Fatalf("code=%d err=%q", code, e)
			}
			if _, err := os.Stat(record); err == nil {
				t.Fatal("a worker started on a fallback profile")
			}
			// Without the caller the global default still runs.
			if code, _, e := runCLI("-d", work, "task"); code != 0 {
				t.Fatalf("global default broke: %d %s", code, e)
			}
		})
	}
}

func TestCallerDefaultUnavailableProfileIsStillSelected(t *testing.T) {
	work, _ := setup(t, okReply)
	writeCallerDefaultsCfg(t, func(s string) string {
		s = strings.Replace(s, `"explicit":{`, `"explicit":{"unavailable":{"reason":"quota exhausted","at":"2026-01-01T00:00:00Z"},`, 1)
		return strings.Replace(s, `"default":"glob"`, `"default":"glob","caller_defaults":{"claude":"explicit"}`, 1)
	})
	if code, _, e := runCLI("-d", work, "--caller", "claude", "task"); code != 0 {
		t.Fatal(e)
	}
	if got := ranProfile(t); got != "explicit" {
		t.Fatalf("an unavailable (not disabled) mapped profile is selected like -p, ran on %q", got)
	}
}

func TestResumeAndThreadRetainProfileAfterCallerDefaultChanges(t *testing.T) {
	work, record := setup(t, okReply)
	writeCallerDefaultsCfg(t, nil)
	if code, _, e := runCLI("default", "--caller", "claude", "explicit"); code != 0 {
		t.Fatal(e)
	}
	if code, _, e := runCLI("-d", work, "--caller", "claude", "-thread", "t1", "first"); code != 0 {
		t.Fatal(e)
	}
	first := lastRecord(t)
	if first.Profile != "explicit" {
		t.Fatalf("first run: %+v", first)
	}
	// Change the caller default and the global default; the saved session must keep its profile.
	if code, _, e := runCLI("default", "--caller", "claude", "envp"); code != 0 {
		t.Fatal(e)
	}
	if code, _, e := runCLI("default", "cx"); code != 0 {
		t.Fatal(e)
	}
	if code, _, e := runCLI("-d", work, "--caller", "claude", "-r", first.Session, "follow"); code != 0 {
		t.Fatal(e)
	}
	if r := lastRecord(t); r.Profile != "explicit" {
		t.Fatalf("resume moved to %q", r.Profile)
	}
	if b, _ := os.ReadFile(record); !strings.Contains(string(b), "base=https://exp.example") {
		t.Fatalf("resume reached another gateway:\n%s", b)
	}
	// A thread run resumes its saved session, so it keeps that session's profile even though the
	// caller default and the global default both changed.
	if code, _, e := runCLI("-d", work, "--caller", "claude", "-thread", "t1", "again"); code != 0 {
		t.Fatalf("thread: %s", e)
	}
	if r := lastRecord(t); r.Profile != "explicit" || r.ThreadStart != "resumed" || r.Thread != "t1" {
		t.Fatalf("a named thread moved to %q after the defaults changed: %+v", r.Profile, r)
	}
	// A fresh thread key has no session to retain, so it follows the caller default.
	if code, _, e := runCLI("-d", work, "--caller", "claude", "-thread", "t2", "new"); code != 0 {
		t.Fatalf("new thread: %s", e)
	}
	if r := lastRecord(t); r.Profile != "envp" || r.ThreadStart != "fresh" {
		t.Fatalf("a new thread must use the current caller default: %+v", r)
	}
	// offload resume (saved -p args) also keeps the profile.
	if code, _, e := runCLI("resume", "--id", first.RunID, "--json", "--caller", "claude"); code != 0 {
		t.Fatalf("resume: %s", e)
	}
	if r := lastRecord(t); r.Profile != "explicit" {
		t.Fatalf("offload resume moved to %q", r.Profile)
	}
}

func TestDefaultCallerCommand(t *testing.T) {
	setup(t, okReply)
	writeCallerDefaultsCfg(t, func(s string) string {
		return strings.Replace(s, `"default":"glob"`, `"default":"glob","future":{"keep":true}`, 1)
	})
	show := func(args ...string) (int, string, string) {
		code, out, e := runCLI(append([]string{"default", "--caller"}, args...)...)
		return code, strings.TrimSpace(out), e
	}
	if code, out, _ := show("claude"); code != 0 || out != "glob" {
		t.Fatalf("an unset caller prints the global default: %d %q", code, out)
	}
	if code, out, e := show("Claude", "son"); code != 0 || out != "claude-sonnet" {
		t.Fatalf("alias must be saved and printed canonically: %d %q %s", code, out, e)
	}
	if code, out, _ := show("claude"); code != 0 || out != "claude-sonnet" {
		t.Fatalf("show: %d %q", code, out)
	}
	if code, out, _ := runCLI("default"); code != 0 || strings.TrimSpace(out) != "glob" {
		t.Fatalf("the global default must not change: %q", out)
	}
	raw, _ := os.ReadFile(config.Path())
	var f struct {
		CallerDefaults map[string]string `json:"caller_defaults"`
		Future         json.RawMessage   `json:"future"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || f.CallerDefaults["claude"] != "claude-sonnet" || !strings.Contains(string(f.Future), "keep") {
		t.Fatalf("%v %s", err, raw)
	}
	before, _ := os.ReadFile(config.Path())
	for _, args := range [][]string{{}, {"unspecified", "glob"}, {"bad caller", "glob"}, {"claude", "missing"}, {"claude", "off"}, {"claude", "glob", "extra"}, {""}} {
		if code, _, _ := show(args...); code != 2 {
			t.Errorf("%q: code %d", args, code)
		}
	}
	if after, _ := os.ReadFile(config.Path()); string(after) != string(before) {
		t.Fatal("a rejected command modified the config")
	}
	if code, _, _ := runCLI("default", "glob", "extra"); code != 2 {
		t.Fatal("plain default keeps rejecting extra arguments")
	}
}

func TestProfileLifecycleRefusesCallerDefaultTargets(t *testing.T) {
	setup(t, okReply)
	writeCallerDefaultsCfg(t, nil)
	if code, _, e := runCLI("default", "--caller", "claude", "claude-sonnet"); code != 0 {
		t.Fatal(e)
	}
	before, _ := os.ReadFile(config.Path())
	for _, verb := range []string{"disable", "delete"} {
		for _, ref := range []string{"claude-sonnet", "son"} { // canonical name and alias
			code, _, e := runCLI("profile", verb, ref)
			if code != 2 || !strings.Contains(e, "default for caller claude") {
				t.Errorf("%s %s: code=%d err=%q", verb, ref, code, e)
			}
		}
	}
	// The JSON edit path refuses too.
	code, _, e := runProfileCLI([]string{"profile", "save", "--json"}, `{"name":"claude-sonnet","disabled":true}`)
	if code != 2 || !strings.Contains(e, "caller default") {
		t.Fatalf("save disabled: code=%d err=%q", code, e)
	}
	if after, _ := os.ReadFile(config.Path()); string(after) != string(before) {
		t.Fatal("a refused change modified the config")
	}
	// Unreferenced profiles are unaffected.
	if code, _, e := runCLI("profile", "disable", "envp"); code != 0 {
		t.Fatal(e)
	}
}

func TestProfileSaveNativeAndSnapshotReportsNative(t *testing.T) {
	profileCfg(t, `{"default":"gw","profiles":{"gw":{"base_url":"https://gw","model":"m","token":"secret-token"}}}`)
	code, out, e := runProfileCLI([]string{"profile", "save", "--json"},
		`{"name":"claude-sonnet","create":true,"backend":"claude","model":"sonnet","auth_mode":"native"}`)
	if code != 0 || e != "" {
		t.Fatalf("code=%d err=%q out=%s", code, e, out)
	}
	snap := decodeSnap(t, out)
	var got *profileEntry
	for i := range snap.Profiles {
		if snap.Profiles[i].Name == "claude-sonnet" {
			got = &snap.Profiles[i]
		}
	}
	if got == nil || got.AuthMode != "native" || got.BaseURL != "" || got.Model != "sonnet" || got.Backend != "claude" || !got.Available {
		t.Fatalf("%+v", got)
	}
	for _, p := range snap.Profiles {
		if p.Name == "gw" && p.AuthMode != "token" {
			t.Fatalf("gateway profile auth_mode changed: %+v", p)
		}
	}
	if snap.CallerDefaults == nil || len(snap.CallerDefaults) != 0 {
		t.Fatalf("caller_defaults must be an empty object, not null: %#v", snap.CallerDefaults)
	}
	noLeak(t, out, e)
	raw, _ := os.ReadFile(config.Path())
	if !strings.Contains(string(raw), `"auth_mode": "native"`) || !strings.Contains(string(raw), "secret-token") {
		t.Fatalf("native save must add the mode and keep other profiles:\n%s", raw)
	}
}

func TestProfileSaveNativeRejectsContradictions(t *testing.T) {
	for name, body := range map[string]string{
		"codex":        `{"name":"x","create":true,"backend":"codex","auth_mode":"native"}`,
		"no model":     `{"name":"x","create":true,"auth_mode":"native"}`,
		"base url":     `{"name":"x","create":true,"model":"sonnet","auth_mode":"native","base_url":"https://gw"}`,
		"endpoint":     `{"name":"x","create":true,"model":"sonnet","auth_mode":"native","endpoint":"cc"}`,
		"secret":       `{"name":"x","create":true,"model":"sonnet","auth_mode":"native","secret":"s"}`,
		"unknown mode": `{"name":"x","create":true,"model":"sonnet","auth_mode":"oauth"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := profileCfg(t, `{"default":"gw","profiles":{"gw":{"base_url":"https://gw","model":"m","token":"t"}}}`)
			before, _ := os.ReadFile(path)
			code, _, e := runProfileCLI([]string{"profile", "save", "--json"}, body)
			if code != 2 || e == "" {
				t.Fatalf("code=%d err=%q", code, e)
			}
			if after, _ := os.ReadFile(path); string(after) != string(before) {
				t.Fatal("a rejected save modified the config")
			}
		})
	}
}

func TestProfileSaveSwitchesBetweenNativeAndGateway(t *testing.T) {
	path := profileCfg(t, `{"default":"gw","profiles":{"gw":{"base_url":"https://gw","model":"m","token":"secret"}}}`)
	// Gateway to native drops the gateway settings.
	code, _, e := runProfileCLI([]string{"profile", "save", "--json"}, `{"name":"gw","auth_mode":"native"}`)
	if code != 0 {
		t.Fatalf("%s", e)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Profiles["gw"]; !p.UsesNativeAuth() || p.BaseURL != "" || p.Token != "" || p.TokenCmd != "" || p.Endpoint != "" || p.Model != "m" {
		t.Fatalf("native switch left gateway settings: %+v", p)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "secret") {
		t.Fatalf("the old token stayed on disk:\n%s", raw)
	}
	// Native back to a gateway needs a base URL and credential, then clears the native mode.
	if code, _, _ := runProfileCLI([]string{"profile", "save", "--json"}, `{"name":"gw","auth_mode":"token","secret":"s2"}`); code != 2 {
		t.Fatal("a credential without a base URL must be rejected")
	}
	code, _, e = runProfileCLI([]string{"profile", "save", "--json"}, `{"name":"gw","base_url":"https://gw2","auth_mode":"token","secret":"s2"}`)
	if code != 0 {
		t.Fatalf("%s", e)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "auth_mode") || !strings.Contains(string(raw), "https://gw2") {
		t.Fatalf("gateway switch left native mode:\n%s", raw)
	}
	if cfg, err = config.Load(path); err != nil || cfg.Profiles["gw"].UsesNativeAuth() || cfg.Profiles["gw"].Token != "s2" {
		t.Fatalf("%v %+v", err, cfg.Profiles["gw"])
	}
}

// The macOS editor switches an existing gateway profile with an endpoint to native by sending only
// auth_mode and an empty endpoint; the CLI must accept exactly that.
func TestProfileSaveNativeFromMacEditorRequestShape(t *testing.T) {
	path := profileCfg(t, `{"default":"gw","profiles":{"gw":{"base_url":"https://gw","model":"m","endpoint":"cc","token_cmd":"echo s"}}}`)
	code, out, e := runProfileCLI([]string{"profile", "save", "--json"}, `{"name":"gw","auth_mode":"native","endpoint":""}`)
	if code != 0 {
		t.Fatalf("%s", e)
	}
	if got := decodeSnap(t, out).Profiles[0]; got.AuthMode != "native" || got.BaseURL != "" || got.Endpoint != "" {
		t.Fatalf("%+v", got)
	}
	// A later edit that leaves the credential alone sends no auth_mode and must keep native.
	if code, _, e := runProfileCLI([]string{"profile", "save", "--json"}, `{"name":"gw","use_for":"official"}`); code != 0 {
		t.Fatalf("%s", e)
	}
	cfg, err := config.Load(path)
	if err != nil || !cfg.Profiles["gw"].UsesNativeAuth() || cfg.Profiles["gw"].UseFor != "official" {
		t.Fatalf("%v %+v", err, cfg.Profiles["gw"])
	}
}

func TestProfilesJSONCallerDefaultsAreAdditiveAndGlobalFieldsStayGlobal(t *testing.T) {
	setup(t, okReply)
	writeCallerDefaultsCfg(t, nil)
	if code, _, e := runCLI("default", "--caller", "claude", "claude-sonnet"); code != 0 {
		t.Fatal(e)
	}
	t.Setenv("OFFLOAD_PROFILE", "")
	_, out, _ := runCLI("profiles", "--json")
	snap := decodeSnap(t, out)
	if snap.CallerDefaults["claude"] != "claude-sonnet" || snap.ConfiguredDefault != "glob" || snap.Active != "glob" || snap.ActiveSource != "config" || snap.SchemaVersion != 1 {
		t.Fatalf("%+v", snap)
	}
	_, plain, _ := runCLI("profiles")
	if !strings.Contains(plain, "native official login") || strings.Contains(plain, "secret") {
		t.Fatalf("plain listing:\n%s", plain)
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &generic); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schema_version", "config_exists", "configured_default", "active", "active_source", "delegation_mode", "profiles", "warnings", "managed_accounts", "caller_defaults"} {
		if _, ok := generic[k]; !ok {
			t.Errorf("snapshot lost field %q", k)
		}
	}
}

func TestHelpDocumentsCallerDefaultAndRoutingOnly(t *testing.T) {
	_, out, e := runCLI("-h")
	text := out + e
	for _, want := range []string{"offload default --caller ID [profile]", "not authentication or access control", "--caller default"} {
		if !strings.Contains(text, want) {
			t.Errorf("help missing %q", want)
		}
	}
}

var _ = history.CallerUnspecified

// The interactive editor must not ask a native profile for a gateway or credential, and saving it
// must keep the native mode instead of failing on an empty base URL.
func TestSettingsWizardEditsNativeProfileWithoutGatewayPrompts(t *testing.T) {
	settingsEnv(t)
	writeSettingsConfig(t, `{"default":"claude-sonnet","profiles":{"claude-sonnet":{"auth_mode":"native","model":"sonnet"}}}`)
	// Edit current profile, model, aliases, then back.
	f := &fakePrompt{steps: []step{choose(0), choose(2), ask("opus"), ask("son"), choose(2)}}
	code, out, errOut := runSettingsTest(f)
	if code != 0 || errOut != "" {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	for _, c := range f.calls {
		switch c.label {
		case "Base URL", "Endpoint", "Authentication", "Token command", "API token":
			t.Errorf("a native profile was asked for %q", c.label)
		}
	}
	cfg, err := config.Load(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Profiles["claude-sonnet"]
	if !p.UsesNativeAuth() || p.Model != "opus" || len(p.Aliases) != 1 || p.Aliases[0] != "son" || p.BaseURL != "" || p.Token != "" {
		t.Fatalf("%+v", p)
	}
}
