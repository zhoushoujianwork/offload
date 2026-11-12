package execution

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisabledProfilesRefuseExecution(t *testing.T) {
	if _, err := New(Config{ProfileName: "blocked", Profile: Profile{Backend: "codex", Disabled: true}}); err == nil || !strings.Contains(err.Error(), "profile enable blocked") {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"default":"active","profiles":{"active":{"backend":"codex"},"blocked":{"backend":"codex","disabled":true,"aliases":["stop"]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path, "stop"); err == nil || !strings.Contains(err.Error(), `profile "blocked" is disabled`) {
		t.Fatal(err)
	}
	profiles, err := ListProfiles(path)
	if err != nil || len(profiles) != 2 || !profiles[1].Disabled || profiles[1].Available || profiles[1].UnavailableReason != "" {
		t.Fatal(profiles, err)
	}
}
