package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
)

func TestManagedAccountDeviceLogin(t *testing.T) {
	setupCodex(t, codexReply)
	fake := filepath.Join(t.TempDir(), "codex-login")
	script := `#!/bin/sh
test "$1" = login && test "$2" = --device-auth || exit 2
test -z "$OPENAI_API_KEY" || exit 3
cat > "$CODEX_HOME/auth.json" <<'AUTH'
{"auth_mode":"chatgpt","tokens":{"access_token":"fixture-access","refresh_token":"fixture-refresh","account_id":"fixture-account"}}
AUTH
`
	if err := os.WriteFile(fake, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CODEX", fake)
	t.Setenv("OPENAI_API_KEY", "ambient-secret")
	code, out, err := runProfileCLI([]string{"accounts", "add", "fresh"}, "")
	if code != 0 || strings.Contains(out, "fixture-access") {
		t.Fatalf("%d %s %s", code, out, err)
	}
	if _, err := config.ManagedAccountHome("fresh"); err != nil {
		t.Fatal(err)
	}
	code, _, _ = runProfileCLI([]string{"accounts", "import", "failed"}, "invalid")
	if code == 0 {
		t.Fatal("accepted invalid authentication")
	}
	if _, err := os.Stat(filepath.Join(config.Dir(), "codex-accounts", "failed")); !os.IsNotExist(err) {
		t.Fatal("failed setup was not cleaned up")
	}
}

func TestManagedAccountImportAndResumeBinding(t *testing.T) {
	work, record := setupCodex(t, codexReply)
	auth := `{"auth_mode":"chatgpt","tokens":{"access_token":"fixture-access","refresh_token":"fixture-refresh","account_id":"fixture-account"}}`
	for _, name := range []string{"first", "second"} {
		code, out, err := runProfileCLI([]string{"accounts", "import", name}, auth)
		if code != 0 || strings.Contains(out, "fixture-access") {
			t.Fatalf("%d %s %s", code, out, err)
		}
	}
	cfg, _ := config.Load(config.Path())
	profile := cfg.Profiles["codex"]
	profile.CodexAccount = "first"
	if err := config.SaveProfile(config.Path(), "codex", profile, false); err != nil {
		t.Fatal(err)
	}
	fake, _ := os.ReadFile(os.Getenv("OFFLOAD_CODEX"))
	fake = []byte(strings.Replace(string(fake), "#!/bin/sh\n", "#!/bin/sh\nprintf '%s' \"$CODEX_HOME\" > '"+record+".home'\nprintf '%s' \"$OPENAI_API_KEY\" > '"+record+".key'\n", 1))
	if err := os.WriteFile(os.Getenv("OFFLOAD_CODEX"), fake, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENAI_API_KEY", "ambient-secret")
	if code, out, err := runCLI("-p", "codex", "-d", work, "inspect"); code != 0 {
		t.Fatalf("%d %s %s", code, out, err)
	}
	home, _ := config.ManagedAccountHome("first")
	profile.CodexAccount = "second"
	if err := config.SaveProfile(config.Path(), "codex", profile, false); err != nil {
		t.Fatal(err)
	}
	if code, out, err := runCLI("-r", "codex-session", "follow up"); code != 0 {
		t.Fatalf("%d %s %s", code, out, err)
	}
	got, _ := os.ReadFile(record + ".home")
	if string(got) != home {
		t.Fatalf("resumed with %s", got)
	}
	key, _ := os.ReadFile(record + ".key")
	if len(key) != 0 {
		t.Fatal("inherited ambient API key")
	}
	records, err := history.Read(history.Path())
	if err != nil || len(records) != 2 {
		t.Fatalf("%v %v", records, err)
	}
	for _, r := range records {
		if r.CodexAccount != "first" || r.CodexHome != home {
			b, _ := json.Marshal(r)
			t.Fatal(string(b))
		}
	}
}
