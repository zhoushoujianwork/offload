package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSaveProfilePreservesJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"default":"other","future":{"keep":1},"profiles":{"other":{"backend":"codex","model":"other","future":true},"a":{"backend":"codex","model":"old","token":"secret","future":{"nested":true}}}}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	p := Profile{Backend: "codex", Model: "new", Token: "secret", ReasoningEffort: "max", ServiceTier: "default"}
	if err := SaveProfile(path, "a", p, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	var before, after map[string]any
	json.Unmarshal([]byte(original), &before)
	json.Unmarshal(b, &after)
	if !reflect.DeepEqual(before["future"], after["future"]) || after["default"] != "other" {
		t.Fatal(string(b))
	}
	oldProfiles, newProfiles := before["profiles"].(map[string]any), after["profiles"].(map[string]any)
	if !reflect.DeepEqual(oldProfiles["other"], newProfiles["other"]) {
		t.Fatal("unrelated profile changed")
	}
	a := newProfiles["a"].(map[string]any)
	if a["future"] == nil || a["token"] != "secret" || a["model"] != "new" {
		t.Fatal(a)
	}
	if err := SaveProfile(path, " b ", Profile{Backend: "codex"}, true); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil || cfg.Default != "b" {
		t.Fatalf("%+v %v", cfg, err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
}

func TestSaveProfileInvalidDoesNotWrite(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Profile
		add  bool
	}{
		{"", Profile{Backend: "codex"}, true}, {"  ", Profile{Backend: "codex"}, true},
		{"a", Profile{Backend: "codex"}, true}, {"missing", Profile{Backend: "codex"}, false},
		{"new", Profile{Backend: "unknown"}, true},
		{"new", Profile{Backend: "codex", ReasoningEffort: "wrong"}, true},
		{"new", Profile{Backend: "codex", ServiceTier: "wrong"}, true},
		{"new", Profile{BaseURL: "file:///tmp", Model: "m", TokenCmd: "command"}, true},
		{"new", Profile{BaseURL: "https:///no-host", Model: "m", TokenCmd: "command"}, true},
		{"new", Profile{BaseURL: "https://x", TokenCmd: "command"}, true},
		{"new", Profile{BaseURL: "https://x", Model: "m"}, true},
	} {
		t.Run(tc.name+tc.p.Backend+tc.p.BaseURL+tc.p.ReasoningEffort+tc.p.ServiceTier, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			original := `{"default":"a","profiles":{"a":{"backend":"codex"}},"future":true}`
			os.WriteFile(path, []byte(original), 0600)
			if err := SaveProfile(path, tc.name, tc.p, tc.add); err == nil {
				t.Fatal("accepted invalid profile")
			}
			b, _ := os.ReadFile(path)
			if string(b) != original {
				t.Fatal("invalid save wrote config")
			}
		})
	}
	for _, contents := range []string{"{", "null", `{"profiles":null}`, `{"profiles":[]}`} {
		path := filepath.Join(t.TempDir(), "config.json")
		os.WriteFile(path, []byte(contents), 0600)
		if err := SaveProfile(path, "a", Profile{Backend: "codex"}, true); err == nil {
			t.Fatal(contents)
		}
		b, _ := os.ReadFile(path)
		if string(b) != contents {
			t.Fatal("malformed config changed")
		}
	}
}

func TestOptionalCodexValidation(t *testing.T) {
	for _, effort := range []string{"", "low", "medium", "high", "xhigh", "max", "ultra"} {
		for _, tier := range []string{"", "default", "priority"} {
			cfg := Config{Profiles: map[string]Profile{"a": {Backend: "codex", ReasoningEffort: effort, ServiceTier: tier}}}
			if _, _, err := cfg.Resolve("a"); err != nil {
				t.Fatal(err)
			}
		}
	}
	p := Profile{BaseURL: "https://x", Model: "m", ReasoningEffort: "provider-specific", ServiceTier: "provider-specific"}
	if err := p.ValidateCodexSettings(); err != nil {
		t.Fatal(err)
	}
}
