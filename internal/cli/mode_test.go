package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
)

func TestModeCommandDispatchAndProfileIndependence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("OFFLOAD_PROFILE", "other-model-profile")
	code, out, err := runCLI("mode")
	if code != 0 || out != "proactive\n" {
		t.Fatalf("%d %s %s", code, out, err)
	}
	if code, out, err = runCLI("mode", "basic"); code != 0 || !strings.Contains(out, "Delegation mode: basic") {
		t.Fatalf("%d %s %s", code, out, err)
	}
	if code, out, _ = runCLI("mode"); code != 0 || out != "basic\n" {
		t.Fatalf("%d %s", code, out)
	}
	b, readErr := os.ReadFile(config.Path())
	if readErr != nil || strings.Contains(string(b), "profiles") {
		t.Fatalf("unexpected profile creation: %s %v", b, readErr)
	}
	if code, _, _ = runCLI("mode", "invalid"); code != 2 {
		t.Fatal(code)
	}
}

func TestGlobalDisablePersistsAndPreservesMode(t *testing.T) {
	work, record := setup(t, okReply)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	if err := config.SetMode(config.Path(), "basic"); err != nil {
		t.Fatal(err)
	}
	if code, _, e := runCLI("disable"); code != 0 {
		t.Fatal(e)
	}
	for _, args := range [][]string{
		{"-json", "-d", work, "task"},
		{"-json", "-p", "missing-profile", "-d", t.TempDir(), "task"},
		{"-json", "-r", "old-session", "resume"},
	} {
		code, out, e := runCLI(args...)
		if code != 2 || decodeRun(t, out).ErrorKind != "globally_disabled" {
			t.Fatalf("%d %s %s", code, out, e)
		}
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("worker launched: %v", err)
	}
	if code, _, e := runCLI("mode", "proactive"); code != 0 {
		t.Fatal(e)
	}
	if err := config.CheckEnabled(); err != config.ErrDisabled {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(home, ".codex", "AGENTS.md"), filepath.Join(home, ".claude", "CLAUDE.md")} {
		b, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(b), "globally disabled") {
			t.Fatalf("%s %v", b, err)
		}
	}
	if code, _, e := runCLI("enable"); code != 0 {
		t.Fatal(e)
	}
	if mode, err := config.LoadMode(config.Path()); err != nil || mode != "proactive" {
		t.Fatalf("%s %v", mode, err)
	}
	if code, _, e := runCLI("-d", work, "task"); code != 0 {
		t.Fatal(e)
	}
}
