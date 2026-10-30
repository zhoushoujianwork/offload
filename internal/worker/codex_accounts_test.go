package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexAccountEnvironmentIsolation(t *testing.T) {
	base := []string{"CODEX_HOME=/system", "OPENAI_API_KEY=system-secret", "CODEX_ACCESS_TOKEN=system-token", "PATH=/bin"}
	for _, home := range []string{"/account-a", "/account-b"} {
		env := strings.Join(codexChildEnv(base, home), "\n")
		if strings.Contains(env, "system") || !strings.Contains(env, "CODEX_HOME="+home) || !strings.Contains(env, "PATH=/bin") {
			t.Fatal(env)
		}
	}
	if base[0] != "CODEX_HOME=/system" {
		t.Fatal("mutated caller environment")
	}
}

func TestCodexAccountModelAndAuthIsolation(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	a, b := t.TempDir(), t.TempDir()
	for home, model := range map[string]string{a: "account-a", b: "account-b"} {
		if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("model = \""+model+"\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if got := codexConfigModel(home); got != model {
			t.Fatalf("%s != %s", got, model)
		}
	}
	if err := os.WriteFile(filepath.Join(a, "auth.json"), []byte(`{"auth_mode":"chatgpt"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if !codexSubscription(a) || codexSubscription(b) {
		t.Fatal("authentication scope leaked")
	}
}
