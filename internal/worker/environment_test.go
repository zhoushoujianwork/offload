package worker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
)

func TestIsolatedEnvironmentSeedsOnlyAuthenticationAndRetainsRefresh(t *testing.T) {
	root, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	source := t.TempDir()
	for name, raw := range map[string]string{"auth.json": `{"token":"fixture"}`, "config.toml": "ambient", "history.jsonl": "history"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("UNRELATED_SECRET", "must-not-inherit")
	t.Setenv("BASH_ENV", "must-not-source")
	o := Opts{ProfileName: "isolated", Profile: config.Profile{Backend: "codex"}, Dir: filepath.Join(root, "workspace"), EnvironmentDirectory: root}
	if _, err := o.prepareEnvironment(source); err != nil {
		t.Fatal(err)
	}
	for _, value := range o.baseEnvironment() {
		if strings.HasPrefix(value, "UNRELATED_SECRET=") || strings.HasPrefix(value, "BASH_ENV=") {
			t.Fatal("ambient environment inherited")
		}
	}
	if o.CodexHome != filepath.Join(root, "codex") {
		t.Fatal("account home was reused")
	}
	for _, name := range []string{"config.toml", "history.jsonl"} {
		if _, err := os.Stat(filepath.Join(o.CodexHome, name)); !os.IsNotExist(err) {
			t.Fatal("ambient state copied")
		}
	}
	target := filepath.Join(o.CodexHome, "auth.json")
	if err := os.WriteFile(target, []byte(`{"token":"refreshed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := o.prepareEnvironment(source); err != nil {
		t.Fatal(err)
	}
	current, _ := os.ReadFile(target)
	original, _ := os.ReadFile(filepath.Join(source, "auth.json"))
	if !strings.Contains(string(current), "refreshed") || !strings.Contains(string(original), "fixture") {
		t.Fatal("authentication ownership lost")
	}
	o.ProfileName = "other"
	if _, err := o.prepareEnvironment(source); err == nil {
		t.Fatal("profile switch reused isolated environment")
	}
}

func TestIsolatedEnvironmentRejectsForeignWorkspaceAndSymlinkAuth(t *testing.T) {
	root, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	o := Opts{ProfileName: "isolated", Profile: config.Profile{Backend: "codex"}, Dir: t.TempDir(), EnvironmentDirectory: root}
	if _, err := o.prepareEnvironment(t.TempDir()); err == nil {
		t.Fatal("non-isolated workspace accepted")
	}
	source := t.TempDir()
	secret := filepath.Join(source, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(source, "auth.json")); err != nil {
		t.Fatal(err)
	}
	o.Dir = filepath.Join(root, "workspace")
	if _, err := o.prepareEnvironment(source); err == nil {
		t.Fatal("symlink authentication accepted")
	}
}

func TestNewEnvironmentAtIsExclusiveAndCleansOnlyItsCreation(t *testing.T) {
	parent := t.TempDir()
	parent, _ = filepath.EvalSymlinks(parent)
	root := filepath.Join(parent, "owned")
	got, err := NewEnvironmentAt(root)
	if err != nil || got != root {
		t.Fatalf("root=%q err=%v", got, err)
	}
	if err := environmentDirectory(root); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "keep")
	if err := os.WriteFile(marker, []byte("user"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEnvironmentAt(root); err == nil {
		t.Fatal("existing root accepted")
	}
	if raw, err := os.ReadFile(marker); err != nil || string(raw) != "user" {
		t.Fatal("existing root was changed")
	}
}

func TestCodexAppServerEnvironmentBindsExactSkillManifest(t *testing.T) {
	root, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(`{"token":"fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	first := t.TempDir()
	second := t.TempDir()
	for _, skillRoot := range []string{first, second} {
		if err := os.WriteFile(filepath.Join(skillRoot, "SKILL.md"), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	o := Opts{ProfileName: "isolated", Profile: config.Profile{Backend: "codex"}, Dir: filepath.Join(root, "workspace"), EnvironmentDirectory: root,
		Native: &NativeControls{CallerContext: true, CodexAppServer: true, PermissionMode: "bypassPermissions", SkillRoots: []string{first, second}}}
	if _, err := o.prepareEnvironment(source); err != nil {
		t.Fatal(err)
	}
	same := o
	same.Native = &NativeControls{CallerContext: true, CodexAppServer: true, PermissionMode: "bypassPermissions", SkillRoots: []string{second, first}}
	if _, err := same.prepareEnvironment(source); err != nil {
		t.Fatalf("identical exact manifest did not reuse its environment: %v", err)
	}
	drifted := o
	drifted.Native = &NativeControls{CallerContext: true, CodexAppServer: true, PermissionMode: "bypassPermissions", SkillRoots: []string{second}}
	if _, err := drifted.prepareEnvironment(source); err == nil || !strings.Contains(err.Error(), "another execution profile or account") {
		t.Fatalf("changed exact Skill manifest reused weaker binding: %v", err)
	}
}

func TestLinkedIsolatedAuthenticationMigration(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			root, err := NewEnvironment()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(root) })
			source := t.TempDir()
			external := filepath.Join(t.TempDir(), "auth.json")
			auth := []byte(`{"tokens":{"access_token":"fake-access","refresh_token":"fake-refresh","account_id":"same-account"}}`)
			if err := os.WriteFile(external, auth, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(external, filepath.Join(source, "auth.json")); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "codex", "auth.json")
			if existing {
				if err := os.WriteFile(target, auth, 0600); err != nil {
					t.Fatal(err)
				}
			}
			o := Opts{ProfileName: "linked", Profile: config.Profile{Backend: "codex"}, Dir: filepath.Join(root, "workspace"), EnvironmentDirectory: root}
			for i := 0; i < 2; i++ {
				if _, err := o.prepareEnvironment(source); err != nil {
					t.Fatal(err)
				}
			}
			got, err := filepath.EvalSymlinks(target)
			want, _ := filepath.EvalSymlinks(external)
			if err != nil || got != want {
				t.Fatal("isolated auth copied or misrouted")
			}
			backups, _ := filepath.Glob(filepath.Join(root, "codex", ".auth-link-backup-*"))
			if (existing && len(backups) != 1) || (!existing && len(backups) != 0) {
				t.Fatalf("backups: %v", backups)
			}
			other := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(other, auth, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(source, "auth.json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(other, filepath.Join(source, "auth.json")); err != nil {
				t.Fatal(err)
			}
			if _, err := o.prepareEnvironment(source); err == nil {
				t.Fatal("silently changed isolated source")
			}
			got, _ = filepath.EvalSymlinks(target)
			if got != want {
				t.Fatal("source mismatch changed auth")
			}
		})
	}
}

func TestLinkedIsolatedAuthenticationRejectsDifferentAccount(t *testing.T) {
	root, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	source := t.TempDir()
	external := filepath.Join(t.TempDir(), "auth.json")
	auth := []byte(`{"tokens":{"access_token":"fake","refresh_token":"fake","account_id":"new"}}`)
	if err := os.WriteFile(external, auth, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(source, "auth.json")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "codex", "auth.json")
	original := []byte(strings.ReplaceAll(string(auth), `"new"`, `"old"`))
	if err := os.WriteFile(target, original, 0600); err != nil {
		t.Fatal(err)
	}
	o := Opts{ProfileName: "linked", Profile: config.Profile{Backend: "codex"}, Dir: filepath.Join(root, "workspace"), EnvironmentDirectory: root}
	if _, err := o.prepareEnvironment(source); err == nil {
		t.Fatal("accepted a different account")
	}
	got, _ := os.ReadFile(target)
	if string(got) != string(original) {
		t.Fatal("mismatched auth overwritten")
	}
}
