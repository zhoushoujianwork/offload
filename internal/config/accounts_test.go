package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManagedAccountLifecycle(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := CreateManagedAccount("../escape"); err == nil {
		t.Fatal("accepted traversal")
	}
	home, err := CreateManagedAccount("work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ManagedAccountHome("work"); err == nil {
		t.Fatal("accepted unfinished login")
	}
	if err := ImportManagedAuth(home, []byte(`{"OPENAI_API_KEY":"fixture"}`)); err == nil {
		t.Fatal("accepted API key as OAuth")
	}
	auth := []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"fixture-access","refresh_token":"fixture-refresh","account_id":"fixture-account"}}`)
	if err := ImportManagedAuth(home, auth); err != nil {
		t.Fatal(err)
	}
	if got, err := ManagedAccountHome("work"); err != nil || got != home {
		t.Fatalf("%s %v", got, err)
	}
	if _, err := CreateManagedAccount("work"); err == nil {
		t.Fatal("replaced existing account")
	}
	st, _ := os.Stat(filepath.Join(home, "auth.json"))
	if st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
	if len(ManagedAccountNames()) != 1 {
		t.Fatal(ManagedAccountNames())
	}
	if err := os.Symlink(home, filepath.Join(filepath.Dir(home), "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := ManagedAccountHome("alias"); err == nil {
		t.Fatal("accepted symlink")
	}
}
