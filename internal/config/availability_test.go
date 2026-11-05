package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClassifyFailure(t *testing.T) {
	permanent := map[string]string{
		"auth":  "HTTP 401 Unauthorized: invalid x-api-key",
		"quota": "You exceeded your current quota, please check your plan",
		"model": "The model `foo-9` does not exist",
	}
	for name, msg := range permanent {
		if ok, _ := ClassifyFailure("execution", msg); !ok {
			t.Errorf("%s should be permanent: %q", name, msg)
		}
	}
	transient := []struct{ kind, msg string }{
		{"execution", "dial tcp: lookup api.example.com: no such host"},
		{"execution", "connection reset by peer"},
		{"timeout", "context deadline exceeded"},
		{"idle_timeout", "no output for 10m"},
		{"rate_limit", "rate_limit"},
		{"capacity", "overloaded"},
		{"execution", "429 Too Many Requests"},
		{"canceled", "401 unauthorized"},
	}
	for _, c := range transient {
		if ok, _ := ClassifyFailure(c.kind, c.msg); ok {
			t.Errorf("%s %q must not be permanent", c.kind, c.msg)
		}
	}
}

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const availCfg = `{"default":"a","future":{"x":1},"profiles":{
 "a":{"base_url":"https://a.example","model":"m","token":"tok-a","extra_field":true},
 "b":{"base_url":"https://b.example","model":"m","token_cmd":"echo b"}}}`

func TestUnavailablePersistsAndPreservesFields(t *testing.T) {
	path := writeCfg(t, availCfg)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := SetUnavailable(path, "a", "authentication failed: Bearer abc.def.ghi key sk-abcdefghijk", at); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	u := cfg.Profiles["a"].Unavailable
	if u == nil || u.At != "2026-01-02T03:04:05Z" || strings.Contains(u.Reason, "sk-abc") || strings.Contains(u.Reason, "abc.def") {
		t.Fatalf("unavailable = %+v", u)
	}
	raw, _ := os.ReadFile(path)
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(raw, &doc)
	if _, ok := doc["future"]; !ok {
		t.Fatal("unknown top-level field lost")
	}
	if !strings.Contains(string(raw), "extra_field") || !strings.Contains(string(raw), "tok-a") || !strings.Contains(string(raw), "echo b") {
		t.Fatalf("profile fields lost: %s", raw)
	}
	if err := MarkAvailable(path, "a"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = Load(path)
	if !cfg.Profiles["a"].Available() {
		t.Fatal("profile should be available again")
	}
	if err := SetUnavailable(path, "missing", "x", at); err == nil {
		t.Fatal("unknown profile must fail")
	}
}

func TestSelectionSkipsUnavailableDefaultButHonorsExplicit(t *testing.T) {
	path := writeCfg(t, availCfg)
	if err := SetUnavailable(path, "a", "quota exhausted: x", time.Now()); err != nil {
		t.Fatal(err)
	}
	cfg, _ := Load(path)
	t.Setenv("OFFLOAD_PROFILE", "")
	if name, _, err := cfg.Resolve(""); err != nil || name != "b" {
		t.Fatalf("default = %q, %v", name, err)
	}
	if name, _, err := cfg.Resolve("a"); err != nil || name != "a" {
		t.Fatalf("explicit = %q, %v", name, err)
	}
	t.Setenv("OFFLOAD_PROFILE", "a")
	if name, _, _ := cfg.Resolve(""); name != "a" {
		t.Fatalf("env-selected = %q", name)
	}
	t.Setenv("OFFLOAD_PROFILE", "")
	_ = SetUnavailable(path, "b", "quota exhausted: x", time.Now())
	cfg, _ = Load(path)
	if name, _, _ := cfg.Resolve(""); name != "a" {
		t.Fatalf("with none available the saved default stays: %q", name)
	}
}

func TestSanitizeReason(t *testing.T) {
	in := "invalid api_key=sk-secret1234567890 at https://u:p@host/v1?token=abc\nbody: {\"prompt\":\"private\"}"
	out := SanitizeReason(in)
	for _, bad := range []string{"sk-secret", "u:p@", "token=abc", "private", "\n"} {
		if strings.Contains(out, bad) {
			t.Errorf("%q leaked in %q", bad, out)
		}
	}
	if len([]rune(SanitizeReason(strings.Repeat("word ", 200)))) > maxReasonRunes+1 {
		t.Error("reason not truncated")
	}
}
