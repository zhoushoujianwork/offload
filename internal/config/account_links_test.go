package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var linkedFixture = []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"fake-access","refresh_token":"fake-refresh","account_id":"fake-account"}}`)

func writeLinkedFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, linkedFixture, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLinkedAccountLifecycle(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	source := writeLinkedFixture(t)
	home, err := LinkManagedAccount("linked", source)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ManagedAccountHome("linked"); err != nil || got != home {
		t.Fatalf("home=%s err=%v", got, err)
	}
	if names := ManagedAccountNames(); len(names) != 1 || names[0] != "linked" {
		t.Fatal(names)
	}
	if err := ImportManagedAuth(home, linkedFixture); err == nil {
		t.Fatal("import wrote through link")
	}
	configPath := filepath.Join(home, "config.toml")
	before, _ := os.ReadFile(configPath)
	if _, err := LinkManagedAccount("linked", source); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(filepath.Join(home, ".auth-link-backup-*"))
	if len(backups) != 0 {
		t.Fatal("idempotent operation backed up a link")
	}
	// An external store can replace its file atomically; the managed link must
	// continue to read the new pathname, rather than a stale copied inode.
	replacement := source + ".new"
	updated := bytes.ReplaceAll(linkedFixture, []byte("fake-access"), []byte("new-access"))
	if err := os.WriteFile(replacement, updated, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, source); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(home, "auth.json"))
	if !bytes.Equal(got, updated) {
		t.Fatal("atomic external replacement was not followed")
	}
	after, _ := os.ReadFile(configPath)
	if !bytes.Equal(before, after) {
		t.Fatal("private config changed")
	}

	other := writeLinkedFixture(t)
	if _, err := LinkManagedAccount("linked", other); err != nil {
		t.Fatal(err)
	}
	backups, _ = filepath.Glob(filepath.Join(home, ".auth-link-backup-*"))
	if len(backups) != 1 {
		t.Fatalf("old mapping missing: %v", backups)
	}
	var mapping struct {
		PreviousTarget string `json:"previous_target"`
		Target         string `json:"target"`
	}
	raw, err := os.ReadFile(filepath.Join(backups[0], "mapping.json"))
	if err != nil || json.Unmarshal(raw, &mapping) != nil {
		t.Fatal("unreadable recovery mapping")
	}
	canonical, _ := filepath.EvalSymlinks(source)
	if mapping.PreviousTarget != canonical {
		t.Fatal("previous source mapping lost")
	}
}

func TestLinkMigrationBackupAndFailurePreservesAuth(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	home, err := CreateManagedAccount("existing")
	if err != nil {
		t.Fatal(err)
	}
	if err := ImportManagedAuth(home, linkedFixture); err != nil {
		t.Fatal(err)
	}
	auth := filepath.Join(home, "auth.json")
	if _, err := LinkManagedAccount("existing", auth); err == nil {
		t.Fatal("accepted self link")
	}
	bad := writeLinkedFixture(t)
	if err := os.Chmod(bad, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LinkManagedAccount("existing", bad); err == nil {
		t.Fatal("accepted public credentials")
	}
	got, _ := os.ReadFile(auth)
	if !bytes.Equal(got, linkedFixture) {
		t.Fatal("failed migration damaged original")
	}
	source := writeLinkedFixture(t)
	if _, err := LinkManagedAccount("existing", source); err != nil {
		t.Fatal(err)
	}
	backups, _ := filepath.Glob(filepath.Join(home, ".auth-link-backup-*"))
	if len(backups) != 1 {
		t.Fatalf("backup missing: %v", backups)
	}
	got, err = os.ReadFile(filepath.Join(backups[0], "auth.json"))
	if err != nil || !bytes.Equal(got, linkedFixture) {
		t.Fatal("original not recoverable")
	}
	for _, p := range []string{backups[0], filepath.Join(backups[0], "auth.json"), filepath.Join(backups[0], "mapping.json")} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm()&0077 != 0 {
			t.Fatal("backup is not private")
		}
	}
	if _, err := LinkManagedAccount("existing", source); err != nil {
		t.Fatal(err)
	}
	again, _ := filepath.Glob(filepath.Join(home, ".auth-link-backup-*"))
	if len(again) != len(backups) {
		t.Fatal("repeated migration added backup")
	}
	current, _ := os.ReadFile(source)
	if !bytes.Equal(current, linkedFixture) {
		t.Fatal("source was changed")
	}
}

func TestLinkedAuthRejectsUnsafePaths(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	dir := t.TempDir()
	dangling, cycle := filepath.Join(dir, "dangling"), filepath.Join(dir, "cycle")
	if err := os.Symlink(filepath.Join(dir, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cycle, cycle); err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(dir, "invalid")
	if err := os.WriteFile(invalid, []byte(`{"OPENAI_API_KEY":"fake"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, dangling, cycle, invalid, "relative/auth.json", dir + "/../auth.json"} {
		if _, err := LinkManagedAccount("unsafe", path); err == nil {
			t.Fatalf("accepted %s", path)
		}
	}
	source := writeLinkedFixture(t)
	home, err := LinkManagedAccount("real", source)
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(home), "alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := LinkManagedAccount("alias", source); err == nil {
		t.Fatal("accepted account home link")
	}
	if _, err := ManagedAccountHome("alias"); err == nil {
		t.Fatal("listed account home link")
	}
	root := filepath.Dir(home)
	if err := os.Rename(root, root+"-real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(root+"-real", root); err != nil {
		t.Fatal(err)
	}
	if _, err := LinkManagedAccount("new", source); err == nil {
		t.Fatal("accepted root link")
	}
	if _, err := ManagedAccountHome("real"); err == nil {
		t.Fatal("listed root link")
	}
}

func TestLinkFilesystemFailurePreservesOriginal(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("requires unprivileged filesystem permission enforcement")
	}
	home := t.TempDir()
	auth := filepath.Join(home, "auth.json")
	if err := os.WriteFile(auth, linkedFixture, 0600); err != nil {
		t.Fatal(err)
	}
	source := writeLinkedFixture(t)
	if err := os.Chmod(home, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0700) })
	if err := LinkAuthFile(home, source); err == nil {
		t.Fatal("expected filesystem write failure")
	}
	st, err := os.Lstat(auth)
	if err != nil || !st.Mode().IsRegular() {
		t.Fatal("failure replaced original auth")
	}
	got, err := os.ReadFile(auth)
	if err != nil || !bytes.Equal(got, linkedFixture) {
		t.Fatal("failure damaged original auth")
	}
}

func TestLinkPreservesExistingIdentity(t *testing.T) {
	for _, kind := range []string{"regular", "link", "invalid-file", "invalid-link", "dangling-link"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			home, err := CreateManagedAccount("existing")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, "auth.json")
			original := linkedFixture
			if strings.HasPrefix(kind, "invalid") {
				original = []byte(`{"tokens":null}`)
			}
			oldTarget := ""
			if strings.HasSuffix(kind, "link") {
				oldTarget = filepath.Join(t.TempDir(), "auth.json")
				if kind != "dangling-link" {
					if err := os.WriteFile(oldTarget, original, 0600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(oldTarget, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			// Keep an existing backup to verify rejection preserves the count too.
			if err := os.Mkdir(filepath.Join(home, ".auth-link-backup-existing"), 0700); err != nil {
				t.Fatal(err)
			}
			source := writeLinkedFixture(t)
			if kind == "regular" || kind == "link" {
				other := bytes.ReplaceAll(linkedFixture, []byte("fake-account"), []byte("other-account"))
				if err := os.WriteFile(source, other, 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err = LinkManagedAccount("existing", source)
			want := "cannot verify existing authentication identity"
			if kind == "regular" || kind == "link" {
				want = "account does not match"
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("expected %q; got %v", want, err)
			}
			backups, _ := filepath.Glob(filepath.Join(home, ".auth-link-backup-*"))
			if len(backups) != 1 {
				t.Fatalf("rejected link changed backups: %v", backups)
			}
			if oldTarget != "" {
				got, err := os.Readlink(path)
				if err != nil || got != oldTarget {
					t.Fatal("rejection changed original link")
				}
			} else {
				st, err := os.Lstat(path)
				if err != nil || !st.Mode().IsRegular() {
					t.Fatal("rejection replaced original regular file")
				}
			}
			if kind != "dangling-link" {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, original) {
					t.Fatal("rejection changed original credentials")
				}
			}
		})
	}
}

func TestLinkExistingUnsignedAccount(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := CreateManagedAccount("unsigned"); err != nil {
		t.Fatal(err)
	}
	if _, err := LinkManagedAccount("unsigned", writeLinkedFixture(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := ManagedAccountHome("unsigned"); err != nil {
		t.Fatal(err)
	}
}
