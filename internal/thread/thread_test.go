package thread

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

func TestValidKey(t *testing.T) {
	for _, k := range []string{"a", "issue-39", "A.b_c-1", "0abc", strings.Repeat("a", 128)} {
		if err := ValidKey(k); err != nil {
			t.Errorf("%q should be valid: %v", k, err)
		}
	}
	for _, k := range []string{"", "-a", ".a", "_a", "a b", "a/b", "../x", "a\x00b", "a\nb", "é", strings.Repeat("a", 129)} {
		if err := ValidKey(k); err == nil {
			t.Errorf("%q should be rejected", k)
		}
	}
}

func TestSaveLoadRoundTripAndScope(t *testing.T) {
	isolate(t)
	if e, err := Load("k", "/work"); e != nil || err != nil {
		t.Fatalf("unbound key: %+v %v", e, err)
	}
	if err := Save("k", "/work", "claude", "p1", "s-1"); err != nil {
		t.Fatal(err)
	}
	e, err := Load("k", "/work")
	if err != nil || e == nil || e.Session != "s-1" || e.Backend != "claude" || e.Profile != "p1" {
		t.Fatalf("%+v %v", e, err)
	}
	if other, _ := Load("k", "/other"); other != nil {
		t.Errorf("a different directory must not see the binding: %+v", other)
	}
	if other, _ := Load("k2", "/work"); other != nil {
		t.Errorf("a different key must not see the binding: %+v", other)
	}
	if err := Save("k", "/work", "claude", "p1", "s-2"); err != nil {
		t.Fatal(err)
	}
	if e, _ := Load("k", "/work"); e == nil || e.Session != "s-2" {
		t.Errorf("save must replace the binding: %+v", e)
	}
	if err := Save("k", "/work", "claude", "p1", ""); err == nil {
		t.Error("an empty session must not be recorded")
	}
	if fi, err := os.Stat(filepath.Join(Dir(), id("k", "/work")+".json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("binding must be private: %v %v", fi, err)
	}
	left, _ := filepath.Glob(filepath.Join(Dir(), ".thread-*"))
	if len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}
}

func TestLoadRejectsDamagedOrMismatchedFiles(t *testing.T) {
	isolate(t)
	if err := Save("k", "/work", "claude", "p", "s-1"); err != nil {
		t.Fatal(err)
	}
	p := path("k", "/work")
	for name, body := range map[string]string{
		"not json":      "{nope",
		"empty":         "",
		"wrong key":     `{"version":1,"key":"other","dir":"/work","backend":"claude","profile":"p","session":"s-1"}`,
		"wrong dir":     `{"version":1,"key":"k","dir":"/else","backend":"claude","profile":"p","session":"s-1"}`,
		"wrong version": `{"version":9,"key":"k","dir":"/work","backend":"claude","profile":"p","session":"s-1"}`,
		"no session":    `{"version":1,"key":"k","dir":"/work","backend":"claude","profile":"p","session":""}`,
	} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if e, err := Load("k", "/work"); err == nil || e != nil {
			t.Errorf("%s: want an error, got %+v %v", name, e, err)
		}
	}
}

func TestCheckRefusesOtherBackendOrProfile(t *testing.T) {
	e := &Entry{Key: "k", Backend: "claude", Profile: "p"}
	if err := e.Check("claude", "p"); err != nil {
		t.Errorf("same binding: %v", err)
	}
	for _, c := range [][2]string{{"codex", "p"}, {"claude", "q"}} {
		if err := e.Check(c[0], c[1]); err == nil || !strings.Contains(err.Error(), "-thread-reset") {
			t.Errorf("%v must be refused with the reset hint: %v", c, err)
		}
	}
}

func TestSessionMissingMatchesOnlyTheBackendsOwnRefusal(t *testing.T) {
	yes := []struct{ backend, text string }{
		{"claude", "error_during_execution: No conversation found with session ID: 0000"},
		{"claude", "NO CONVERSATION FOUND WITH SESSION ID: x"},
		{"codex", "Error: thread/resume: thread/resume failed: no rollout found for thread id 0000 (code -32600)"},
	}
	no := []struct{ backend, text string }{
		{"claude", ""},
		{"claude", "build failed: syntax error"},
		{"claude", "rate_limit"},
		{"claude", "no rollout found for thread id 0000"},
		{"codex", "No conversation found with session ID: 0000"},
		{"codex", "timeout"},
	}
	for _, c := range yes {
		if !SessionMissing(c.backend, c.text) {
			t.Errorf("%s %q should be a missing session", c.backend, c.text)
		}
	}
	for _, c := range no {
		if SessionMissing(c.backend, c.text) {
			t.Errorf("%s %q must not trigger a fresh session", c.backend, c.text)
		}
	}
}
