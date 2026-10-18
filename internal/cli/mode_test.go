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
