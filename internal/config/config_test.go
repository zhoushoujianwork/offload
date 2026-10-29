package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBackendValidation(t *testing.T) {
	t.Setenv("OFFLOAD_PROFILE", "")
	for _, tc := range []struct {
		name    string
		profile Profile
		valid   bool
	}{
		{"legacy Claude", Profile{BaseURL: "https://gateway", Model: "model"}, true},
		{"explicit Claude", Profile{Backend: "claude", BaseURL: "https://gateway", Model: "model"}, true},
		{"native Codex", Profile{Backend: "codex"}, true},
		{"unknown backend", Profile{Backend: "unknown"}, false},
		{"missing Claude config", Profile{Backend: "claude"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{Default: "test", Profiles: map[string]Profile{"test": tc.profile}}
			_, _, err := c.Resolve("")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestAliasesLoadAndSaveValidation(t *testing.T) {
	for _, tc := range []struct {
		aliases []string
		want    string
	}{
		{[]string{"cc", "cc"}, "duplicate alias"},
		{[]string{"shared"}, "duplicate alias"},
		{[]string{"a"}, "conflicts with a profile name"},
		{[]string{"b"}, "conflicts with a profile name"},
		{[]string{""}, "invalid alias"},
		{[]string{"UPPER"}, "invalid alias"},
		{[]string{"has_space "}, "invalid alias"},
		{[]string{strings.Repeat("x", 33)}, "invalid alias"},
	} {
		t.Run(fmt.Sprint(tc.aliases), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			cfg := Config{Profiles: map[string]Profile{
				"a": {Backend: "codex", Aliases: tc.aliases},
				"b": {Backend: "codex", Aliases: []string{"shared"}},
			}}
			data, _ := json.Marshal(cfg)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want %q", err, tc.want)
			}
			cfg.Profiles["a"] = Profile{Backend: "codex"}
			original, _ := json.Marshal(cfg)
			os.WriteFile(path, original, 0600)
			if err := SaveProfile(path, "a", Profile{Backend: "codex", Aliases: tc.aliases}, false); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("SaveProfile error = %v, want %q", err, tc.want)
			}
			got, _ := os.ReadFile(path)
			if string(got) != string(original) {
				t.Fatal("invalid aliases changed configuration")
			}
		})
	}
}

func TestNewProfileNameCannotShadowAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	original := `{"profiles":{"full-name":{"backend":"codex","aliases":["cc"]}}}`
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SaveProfile(path, "cc", Profile{Backend: "codex"}, true); err == nil || !strings.Contains(err.Error(), "conflicts with a profile name") {
		t.Fatalf("name shadows alias: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != original {
		t.Fatal("rejected name wrote configuration")
	}
}

func TestResolveAliases(t *testing.T) {
	cfg := Config{Default: "cc", Profiles: map[string]Profile{
		"full-name": {Backend: "codex", Aliases: []string{"cc", strings.Repeat("x", 32), "0-a"}},
	}}
	if err := cfg.ValidateAliases(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_PROFILE", "")
	for _, input := range []string{"", "cc", "full-name", "0-a"} {
		name, _, err := cfg.Resolve(input)
		if err != nil || name != "full-name" {
			t.Fatalf("Resolve(%q) = %q, %v", input, name, err)
		}
	}
	t.Setenv("OFFLOAD_PROFILE", "cc")
	if name, _, err := cfg.Resolve(""); err != nil || name != "full-name" {
		t.Fatalf("env: %q %v", name, err)
	}
	if _, _, err := cfg.Resolve("missing"); err == nil || !strings.Contains(err.Error(), "full-name (aliases: cc,") {
		t.Fatalf("missing aliases: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	data, _ := json.Marshal(cfg)
	os.WriteFile(path, data, 0600)
	if err := SetDefault(path, "cc"); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || loaded.Default != "full-name" {
		t.Fatalf("default: %+v %v", loaded, err)
	}
	// Defensively preserve exact-name precedence for configurations built in memory.
	cfg.Profiles["cc"] = Profile{Backend: "codex", Model: "exact"}
	if name, p, err := cfg.Resolve("cc"); err != nil || name != "cc" || p.Model != "exact" {
		t.Fatalf("exact name: %q %+v %v", name, p, err)
	}
	delete(cfg.Profiles, "cc")
	cfg.Profiles["other"] = Profile{Backend: "codex", Aliases: []string{"cc"}}
	if _, _, err := cfg.Resolve("cc"); err == nil || !strings.Contains(err.Error(), "ambiguous") || !strings.Contains(err.Error(), "other") {
		t.Fatalf("ambiguity: %v", err)
	}
}
