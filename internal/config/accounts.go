package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

var accountName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ManagedAccountHome resolves only registered directories under offload's private root.
func ManagedAccountHome(name string) (string, error) {
	if !accountName.MatchString(name) {
		return "", errors.New("invalid managed account name")
	}
	root := filepath.Join(Dir(), "codex-accounts")
	home := filepath.Join(root, name)
	for _, p := range []string{root, home} {
		st, err := os.Lstat(p)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("managed account unavailable")
		}
	}
	if st, err := os.Lstat(filepath.Join(home, "auth.json")); err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return "", errors.New("managed account is not signed in")
	}
	return home, nil
}

func ManagedAccountNames() []string {
	names := []string{}
	entries, _ := os.ReadDir(filepath.Join(Dir(), "codex-accounts"))
	for _, e := range entries {
		if _, err := ManagedAccountHome(e.Name()); err == nil {
			names = append(names, e.Name())
		}
	}
	return names
}

// CreateManagedAccount reserves a new home without replacing existing credentials.
func CreateManagedAccount(name string) (string, error) {
	if !accountName.MatchString(name) {
		return "", errors.New("use 1-32 lowercase letters, digits or dashes for the account name")
	}
	root := filepath.Join(Dir(), "codex-accounts")
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	st, err := os.Lstat(root)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("unsafe managed account root")
	}
	home := filepath.Join(root, name)
	if err := os.Mkdir(home, 0700); err != nil {
		return "", errors.New("account already exists or cannot be created")
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("cli_auth_credentials_store = \"file\"\n"), 0600); err != nil {
		os.RemoveAll(home)
		return "", err
	}
	return home, nil
}

func ImportManagedAuth(home string, data []byte) error {
	var auth struct {
		Mode   string `json:"auth_mode"`
		Tokens *struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
			Account string `json:"account_id"`
		} `json:"tokens"`
	}
	if len(data) > 1<<20 || json.Unmarshal(data, &auth) != nil || auth.Tokens == nil || auth.Tokens.Access == "" || auth.Tokens.Refresh == "" || auth.Tokens.Account == "" || (auth.Mode != "" && auth.Mode != "chatgpt") {
		return errors.New("expected a Codex OAuth auth.json with account and refresh credentials")
	}
	path := filepath.Join(home, "auth.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}
