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

func TestManagedAccountLinkCLI(t *testing.T) {
	setupCodex(t, codexReply)
	path := filepath.Join(t.TempDir(), "auth.json")
	auth := `{"tokens":{"access_token":"fake-access","refresh_token":"fake-refresh","account_id":"fake-account"}}`
	if err := os.WriteFile(path, []byte(auth), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		code, out, stderr := runProfileCLI([]string{"accounts", "link", "linked", path}, "")
		if code != 0 || !strings.Contains(out, `"linked"`) || strings.Contains(out+stderr, "fake-access") {
			t.Fatalf("code=%d out=%s err=%s", code, out, stderr)
		}
	}
	if _, err := config.ManagedAccountHome("linked"); err != nil {
		t.Fatal(err)
	}
	code, _, _ := runProfileCLI([]string{"accounts", "link", "linked", "relative"}, "")
	if code == 0 {
		t.Fatal("accepted relative target")
	}
}

func TestManagedAccountLinkCLIRejectsIdentityChange(t *testing.T) {
	for _, setup := range []string{"import", "link"} {
		t.Run(setup, func(t *testing.T) {
			setupCodex(t, codexReply)
			original := `{"tokens":{"access_token":"fake-access","refresh_token":"fake-refresh","account_id":"original-account"}}`
			oldSource := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(oldSource, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"accounts", setup, "existing"}
			if setup == "link" {
				args = append(args, oldSource)
			}
			if code, out, stderr := runProfileCLI(args, original); code != 0 {
				t.Fatalf("setup: %d %s %s", code, out, stderr)
			}
			home, err := config.ManagedAccountHome("existing")
			if err != nil {
				t.Fatal(err)
			}
			before, _ := filepath.Glob(filepath.Join(home, ".auth-link-backup-*"))
			source := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(source, []byte(strings.ReplaceAll(original, "original-account", "different-account")), 0600); err != nil {
				t.Fatal(err)
			}
			code, out, stderr := runProfileCLI([]string{"accounts", "link", "existing", source}, "")
			if code == 0 || !strings.Contains(stderr, "use a new account name") || strings.Contains(out+stderr, "fake-access") {
				t.Fatalf("%d %s %s", code, out, stderr)
			}
			after, _ := filepath.Glob(filepath.Join(home, ".auth-link-backup-*"))
			if len(before) != len(after) {
				t.Fatal("rejection created a backup")
			}
			path := filepath.Join(home, "auth.json")
			got, err := os.ReadFile(path)
			if err != nil || string(got) != original {
				t.Fatal("rejection changed authentication")
			}
			if setup == "link" {
				got, err := os.Readlink(path)
				want, _ := filepath.EvalSymlinks(oldSource)
				if err != nil || got != want {
					t.Fatal("rejection retargeted link")
				}
			}
		})
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
