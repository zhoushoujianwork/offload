package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProfileDisableLifecycle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	initial := `{"default":"a","unknown":{"keep":true},"profiles":{"a":{"backend":"codex","future":17},"b":{"backend":"codex","aliases":["bee"],"future":42}}}`
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SetProfileDisabled(path, "a", true); err == nil {
		t.Fatal("disabled default")
	}
	unchanged, _ := os.ReadFile(path)
	if string(unchanged) != initial {
		t.Fatal("refusal mutated file")
	}
	if err := SetProfileDisabled(path, "b", true); err != nil {
		t.Fatal(err)
	}
	cfg, _ := Load(path)
	if !cfg.Profiles["b"].Disabled || cfg.Profiles["b"].Available() {
		t.Fatal(cfg)
	}
	if _, _, err := cfg.Resolve("bee"); err != nil {
		t.Fatal("inspection must resolve disabled", err)
	}
	if _, _, err := cfg.ResolveForRun("bee"); err == nil || !strings.Contains(err.Error(), `profile "b" is disabled`) {
		t.Fatal(err)
	}
	if err := SetDefault(path, "bee"); err == nil {
		t.Fatal("selected disabled default")
	}
	if err := SetUnavailable(path, "b", "old", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := MarkAvailable(path, "b"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = Load(path)
	if !cfg.Profiles["b"].Disabled || cfg.Profiles["b"].Unavailable != nil {
		t.Fatal(cfg)
	}
	if err := SetProfileDisabled(path, "b", false); err != nil {
		t.Fatal(err)
	}
	cfg, _ = Load(path)
	if !cfg.Profiles["b"].Available() {
		t.Fatal("enable failed")
	}
	p := cfg.Profiles["b"]
	p.Disabled = true
	if err := SaveProfile(path, "b", p, false); err != nil {
		t.Fatal(err)
	}
	p.Disabled = false
	if err := SaveProfile(path, "b", p, false); err != nil {
		t.Fatal(err)
	}
	cfg, _ = Load(path)
	if cfg.Profiles["b"].Disabled {
		t.Fatal("false patch did not clear disabled")
	}
	if err := DeleteProfile(path, "a"); err == nil {
		t.Fatal("deleted default")
	}
	if err := DeleteProfile(path, "b"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"unknown"`) || !strings.Contains(string(raw), `"future": 17`) || strings.Contains(string(raw), "bee") {
		t.Fatal(string(raw))
	}
}

func TestRemoveAccountFilesystemSafety(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, name := range []string{"", "../escape", "UPPER", "a/b", strings.Repeat("a", 33)} {
		if err := RemoveManagedAccount(name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	outside := t.TempDir()
	target := filepath.Join(outside, "auth.json")
	if err := os.WriteFile(target, []byte("keep target"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, linked := range []bool{false, true} {
		home, err := CreateManagedAccount("work")
		if err != nil {
			t.Fatal(err)
		}
		if linked {
			if err := os.Symlink(target, filepath.Join(home, "auth.json")); err != nil {
				t.Fatal(err)
			}
		}
		if err := RemoveManagedAccount("work"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(home); !os.IsNotExist(err) {
			t.Fatal("home survived", err)
		}
		data, err := os.ReadFile(target)
		if err != nil || string(data) != "keep target" {
			t.Fatal("link target changed", err)
		}
	}
	root := filepath.Join(Dir(), "codex-accounts")
	if err := os.Symlink(outside, filepath.Join(root, "work")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveManagedAccount("work"); err == nil {
		t.Fatal("followed symlink home")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := os.MkdirAll(Dir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(Dir(), "codex-accounts")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveManagedAccount("work"); err == nil {
		t.Fatal("followed symlink root")
	}
}

func TestDisabledSelectionSkipsDefaultAndPool(t *testing.T) {
	cfg := Config{Default: "a", Profiles: map[string]Profile{
		"a": {Backend: "codex", Disabled: true}, "b": {Backend: "codex"}, "c": {Backend: "codex", Disabled: true}}}
	if cfg.EffectiveDefault() != "b" {
		t.Fatal("selected disabled default")
	}
	for name, p := range cfg.Profiles {
		if p.Available() && name != "b" {
			t.Fatal("disabled pool member", name)
		}
	}
	t.Setenv("OFFLOAD_PROFILE", "a")
	if _, _, err := cfg.ResolveForRun(""); err == nil {
		t.Fatal("environment selected disabled profile")
	}
}
