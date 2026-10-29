package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	pjToken   = "tok-SECRET-123"
	pjCmd     = "security find-generic-password -s leak-me -w"
	pjPrivate = "https://user:pw@gw.example.com/v1?key=PRIVATE"
)

func profileCfg(t *testing.T, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("OFFLOAD_PROFILE", "")
	path := filepath.Join(dir, "offload", "config.json")
	if body != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func runProfileCLI(args []string, stdin string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Run("test", args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func decodeSnap(t *testing.T, s string) profileSnapshot {
	t.Helper()
	var snap profileSnapshot
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&snap); err != nil {
		t.Fatalf("%v: %s", err, s)
	}
	return snap
}

func noLeak(t *testing.T, outs ...string) {
	t.Helper()
	for _, o := range outs {
		for _, bad := range []string{pjToken, pjCmd, "leak-me", "PRIVATE", "user:pw", "SECRET"} {
			if strings.Contains(o, bad) {
				t.Fatalf("leaked %q in %q", bad, o)
			}
		}
	}
}

const profileListCfg = `{"default":"zeta","future":1,"delegation_mode":"basic","profiles":{
"zeta":{"base_url":"https://gw.example.com/v1","model":"m1","endpoint":"cc","token":"` + pjToken + `"},
"alpha":{"base_url":"` + pjPrivate + `","model":"m2","token_cmd":"` + pjCmd + `"},
"cx":{"backend":"codex","model":"gpt","reasoning_effort":"high","service_tier":"priority"}}}`

func TestProfilesJSONMissingConfig(t *testing.T) {
	profileCfg(t, "")
	code, out, errb := runProfileCLI([]string{"profiles", "--json"}, "")
	if code != 0 || errb != "" {
		t.Fatal(code, errb)
	}
	snap := decodeSnap(t, out)
	if snap.ConfigExists || snap.SchemaVersion != 1 || snap.ActiveSource != "none" || snap.Active != "" ||
		snap.Profiles == nil || snap.Warnings == nil || len(snap.Profiles) != 0 {
		t.Fatalf("%+v", snap)
	}
	if !strings.Contains(out, `"profiles":[]`) || !strings.Contains(out, `"warnings":[]`) {
		t.Fatal(out)
	}
	// Text behavior is unchanged for a missing config.
	if code, _, errb := runProfileCLI([]string{"profiles"}, ""); code != 2 || !strings.Contains(errb, "no config at") {
		t.Fatal(code, errb)
	}
}

func TestProfilesJSONInvalidConfigIsSafe(t *testing.T) {
	profileCfg(t, `{"default":"a","profiles":{"a":{"token":"`+pjToken+`"`)
	code, out, errb := runProfileCLI([]string{"profiles", "--json"}, "")
	if code == 0 || out != "" {
		t.Fatal(code, out)
	}
	noLeak(t, out, errb)
	if strings.Contains(errb, "config.json") {
		t.Fatal(errb)
	}
}

func TestProfilesJSONSortedRedactedAndSource(t *testing.T) {
	profileCfg(t, profileListCfg)
	code, out, errb := runProfileCLI([]string{"profiles", "--json"}, "")
	if code != 0 || errb != "" {
		t.Fatal(code, errb)
	}
	noLeak(t, out)
	snap := decodeSnap(t, out)
	var names []string
	for _, p := range snap.Profiles {
		names = append(names, p.Name)
	}
	if !reflect.DeepEqual(names, []string{"alpha", "cx", "zeta"}) {
		t.Fatal(names)
	}
	if snap.ConfiguredDefault != "zeta" || snap.Active != "zeta" || snap.ActiveSource != "config" ||
		snap.DelegationMode != "basic" || !snap.ConfigExists || len(snap.Warnings) != 0 {
		t.Fatalf("%+v", snap)
	}
	a, c, z := snap.Profiles[0], snap.Profiles[1], snap.Profiles[2]
	if a.BaseURL != "" || !a.BaseURLRedacted || a.AuthMode != "token_cmd" || a.Backend != "claude" {
		t.Fatalf("%+v", a)
	}
	if c.Backend != "codex" || c.AuthMode != "none" || c.ReasoningEffort != "high" || c.ServiceTier != "priority" {
		t.Fatalf("%+v", c)
	}
	if z.BaseURL != "https://gw.example.com/v1" || z.BaseURLRedacted || z.AuthMode != "token" || z.Endpoint != "cc" {
		t.Fatalf("%+v", z)
	}
}

func TestProfilesJSONEnvironmentActive(t *testing.T) {
	profileCfg(t, profileListCfg)
	t.Setenv("OFFLOAD_PROFILE", "cx")
	_, out, _ := runProfileCLI([]string{"profiles", "--json"}, "")
	snap := decodeSnap(t, out)
	if snap.Active != "cx" || snap.ActiveSource != "environment" || snap.ConfiguredDefault != "zeta" {
		t.Fatalf("%+v", snap)
	}
	t.Setenv("OFFLOAD_PROFILE", "missing-"+pjToken)
	_, out, _ = runProfileCLI([]string{"profiles", "--json"}, "")
	snap = decodeSnap(t, out)
	if snap.Active != "" || snap.ActiveSource != "environment" || len(snap.Warnings) != 1 {
		t.Fatalf("%+v", snap)
	}
	noLeak(t, out)
}

func TestProfilesJSONMissingDefaultWarns(t *testing.T) {
	profileCfg(t, `{"default":"gone","profiles":{"a":{"backend":"codex"}}}`)
	_, out, _ := runProfileCLI([]string{"profiles", "--json"}, "")
	snap := decodeSnap(t, out)
	if snap.Active != "" || snap.ActiveSource != "config" || snap.ConfiguredDefault != "gone" || len(snap.Warnings) != 1 {
		t.Fatalf("%+v", snap)
	}
}

func TestProfilesTextUnchangedAndBadArgs(t *testing.T) {
	profileCfg(t, profileListCfg)
	code, out, _ := runProfileCLI([]string{"profiles"}, "")
	if code != 0 || !strings.Contains(out, "* zeta") {
		t.Fatal(code, out)
	}
	if code, _, _ := runProfileCLI([]string{"profiles", "--bogus"}, ""); code != 2 {
		t.Fatal(code)
	}
}

func TestProfileSaveCreateAndEdit(t *testing.T) {
	path := profileCfg(t, `{"default":"zeta","future":{"k":1},"profiles":{
"zeta":{"base_url":"https://gw.example.com/v1","model":"m1","token":"`+pjToken+`","extra":true},
"alpha":{"base_url":"`+pjPrivate+`","model":"m2","token_cmd":"`+pjCmd+`","extra":[1]}}}`)
	// Edit preserves omitted fields, credentials, unknown keys and the default.
	code, out, errb := runProfileCLI([]string{"profile", "save", "--json"}, `{"name":"alpha","model":"m3","endpoint":"llm"}`)
	if code != 0 || errb != "" {
		t.Fatal(code, errb)
	}
	noLeak(t, out)
	snap := decodeSnap(t, out)
	if snap.ConfiguredDefault != "zeta" {
		t.Fatal(snap.ConfiguredDefault)
	}
	raw, _ := os.ReadFile(path)
	var cfg map[string]any
	json.Unmarshal(raw, &cfg)
	alpha := cfg["profiles"].(map[string]any)["alpha"].(map[string]any)
	if alpha["base_url"] != pjPrivate || alpha["token_cmd"] != pjCmd || alpha["model"] != "m3" ||
		alpha["endpoint"] != "llm" || alpha["extra"] == nil || cfg["future"] == nil {
		t.Fatal(alpha)
	}
	// Explicit replacement of credentials.
	code, out, errb = runProfileCLI([]string{"profile", "save", "--json"}, `{"name":"alpha","auth_mode":"token","secret":" `+pjToken+` "}`)
	if code != 0 {
		t.Fatal(errb)
	}
	noLeak(t, out, errb)
	raw, _ = os.ReadFile(path)
	json.Unmarshal(raw, &cfg)
	alpha = cfg["profiles"].(map[string]any)["alpha"].(map[string]any)
	if alpha["token"] != pjToken || alpha["token_cmd"] != nil {
		t.Fatal(alpha)
	}
	if a := decodeSnap(t, out).Profiles[0]; a.AuthMode != "token" {
		t.Fatalf("%+v", a)
	}
	// Create defaults to claude and selects the new profile.
	code, out, errb = runProfileCLI([]string{"profile", "save", "--json"},
		`{"name":" fresh ","create":true,"base_url":"https://new.example.com","model":"x","auth_mode":"token_cmd","secret":"`+pjCmd+`"}`)
	if code != 0 {
		t.Fatal(errb)
	}
	noLeak(t, out, errb)
	snap = decodeSnap(t, out)
	if snap.ConfiguredDefault != "fresh" {
		t.Fatalf("%+v", snap)
	}
	raw, _ = os.ReadFile(path)
	json.Unmarshal(raw, &cfg)
	if cfg["profiles"].(map[string]any)["fresh"].(map[string]any)["token_cmd"] != pjCmd {
		t.Fatal(string(raw))
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
}

func TestProfileSaveCodexAndAuthRules(t *testing.T) {
	path := profileCfg(t, `{"default":"a","profiles":{"a":{"base_url":"https://gw.example.com","model":"m","token":"`+pjToken+`"}}}`)
	code, out, errb := runProfileCLI([]string{"profile", "save", "--json"},
		`{"name":"cx","create":true,"backend":"codex","model":"gpt","reasoning_effort":"max","auth_mode":"none"}`)
	if code != 0 {
		t.Fatal(errb)
	}
	if snap := decodeSnap(t, out); snap.ConfiguredDefault != "cx" || snap.Profiles[1].AuthMode != "none" {
		t.Fatalf("%+v", snap)
	}
	before, _ := os.ReadFile(path)
	for name, body := range map[string]string{
		"claude none":      `{"name":"a","auth_mode":"none"}`,
		"secret preserve":  `{"name":"a","secret":"` + pjToken + `"}`,
		"secret none":      `{"name":"a","auth_mode":"none","secret":"` + pjToken + `"}`,
		"token no secret":  `{"name":"a","auth_mode":"token"}`,
		"blank secret":     `{"name":"a","auth_mode":"token_cmd","secret":"  "}`,
		"bad mode":         `{"name":"a","auth_mode":"` + pjToken + `"}`,
		"unknown profile":  `{"name":"` + pjToken + `","model":"x"}`,
		"exists":           `{"name":"a","create":true,"base_url":"https://x.example.com","model":"m","auth_mode":"token","secret":"` + pjToken + `"}`,
		"bad url":          `{"name":"a","base_url":"ftp://` + pjToken + `.example.com"}`,
		"bad endpoint":     `{"name":"a","endpoint":"` + pjToken + `"}`,
		"bad effort":       `{"name":"cx","reasoning_effort":"` + pjToken + `"}`,
		"unknown field":    `{"name":"a","token":"` + pjToken + `"}`,
		"malformed":        `{"name":"a","model":"` + pjToken,
		"trailing":         `{"name":"a"} {"name":"a"}`,
		"trailing garbage": `{"name":"a"} ` + pjToken,
		"array":            `[{"name":"a"}]`,
		"oversize":         `{"name":"a","model":"` + strings.Repeat("x", 70<<10) + `"}`,
		"empty":            ``,
	} {
		code, out, errb := runProfileCLI([]string{"profile", "save", "--json"}, body)
		if code == 0 || out != "" || errb == "" {
			t.Fatalf("%s: code=%d out=%q err=%q", name, code, out, errb)
		}
		noLeak(t, out, errb)
	}
	// Failed saves leave the earlier state intact.
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("config changed by rejected requests")
	}
}

func TestProfileSaveUsageAndMissingConfig(t *testing.T) {
	profileCfg(t, "")
	for _, args := range [][]string{{"profile"}, {"profile", "save"}, {"profile", "save", "--text"}, {"profile", "rm", "--json"}} {
		if code, _, _ := runProfileCLI(args, `{}`); code != 2 {
			t.Fatal(args, code)
		}
	}
	if code, _, errb := runProfileCLI([]string{"profile", "save", "--json"}, `{"name":"a","model":"m"}`); code != 2 || strings.Contains(errb, "config.json") {
		t.Fatal(code, errb)
	}
	code, out, errb := runProfileCLI([]string{"profile", "save", "--json"}, `{"name":"c","create":true,"backend":"codex"}`)
	if code != 0 {
		t.Fatal(errb)
	}
	if snap := decodeSnap(t, out); !snap.ConfigExists || snap.Active != "c" {
		t.Fatalf("%+v", snap)
	}
}

type failingWriter struct{ short bool }

func (w failingWriter) Write(p []byte) (int, error) {
	if w.short {
		return len(p) / 2, nil
	}
	return 0, errors.New("disk full " + pjToken)
}

func TestProfileSnapshotWriteFailureIsSafe(t *testing.T) {
	snap := profileSnapshot{SchemaVersion: 1, ActiveSource: "none", Profiles: []profileEntry{}, Warnings: []string{}}
	for _, w := range []failingWriter{{}, {short: true}} {
		var errb bytes.Buffer
		if code := writeSnapshot(w, &errb, snap); code == 0 || errb.String() != "offload: could not write output\n" {
			t.Fatalf("code=%d err=%q", code, errb.String())
		}
	}
	profileCfg(t, profileListCfg)
	var errb bytes.Buffer
	if code := Run("test", []string{"profiles", "--json"}, strings.NewReader(""), failingWriter{}, &errb); code == 0 {
		t.Fatal(code)
	}
	noLeak(t, errb.String())
	errb.Reset()
	body := `{"name":"zeta","model":"m9"}`
	if code := Run("test", []string{"profile", "save", "--json"}, strings.NewReader(body), failingWriter{}, &errb); code == 0 {
		t.Fatal(code)
	}
	noLeak(t, errb.String())
}
