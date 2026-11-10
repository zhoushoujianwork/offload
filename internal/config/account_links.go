package config

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// ResolveOAuthFile validates without writing, chmodding, or exposing credentials.
// The canonical pathname (not an open inode) survives atomic refresh replacement.
func ResolveOAuthFile(path string) (string, string, error) {
	invalid := errors.New("authentication must resolve to a private 0600 regular Codex OAuth file")
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", "", invalid
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", invalid
	}
	before, err := os.Lstat(resolved)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm() != 0600 || before.Size() > 1<<20 {
		return "", "", invalid
	}
	f, err := os.Open(resolved)
	if err != nil {
		return "", "", invalid
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0600 {
		return "", "", invalid
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return "", "", invalid
	}
	account, err := OAuthAccountID(data)
	if err != nil {
		return "", "", invalid
	}
	return resolved, account, nil
}

// LinkAuthFile changes only the auth leaf. Existing authentication and its old
// target mapping are retained in a private .auth-link-backup-* directory.
func LinkAuthFile(home, source string) error {
	st, err := os.Lstat(home)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("authentication home must be a real directory")
	}
	resolved, account, err := ResolveOAuthFile(source)
	if err != nil {
		return err
	}
	canonicalHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return err
	}
	path := filepath.Join(canonicalHome, "auth.json")
	if resolved == path {
		return errors.New("authentication cannot link to itself")
	}
	oldTarget := ""
	previous, err := os.Lstat(path)
	if err == nil {
		_, previousAccount, err := ResolveOAuthFile(path)
		if err != nil {
			return errors.New("cannot verify existing authentication identity; restore valid OAuth authentication or use a new account name")
		}
		if previousAccount != account {
			return errors.New("authentication account does not match existing account; use a new account name")
		}
		if previous.Mode()&os.ModeSymlink != 0 {
			oldTarget, err = os.Readlink(path)
			if err != nil {
				return err
			}
			if oldTarget == resolved {
				return nil
			}
		} else if !previous.Mode().IsRegular() || previous.Mode().Perm() != 0600 {
			return errors.New("existing authentication must be a private regular file or leaf link")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Prepare the replacement before touching the existing leaf. Rename either
	// commits the link in one step or leaves the original authentication intact.
	tmp, err := os.MkdirTemp(canonicalHome, ".auth-link-pending-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	link := filepath.Join(tmp, "auth.json")
	if err := os.Symlink(resolved, link); err != nil {
		return err
	}
	if previous != nil {
		backup, err := os.MkdirTemp(canonicalHome, ".auth-link-backup-")
		if err != nil {
			return err
		}
		mapping, _ := json.Marshal(struct {
			PreviousTarget string `json:"previous_target,omitempty"`
			Target         string `json:"target"`
		}{oldTarget, resolved})
		if err := os.WriteFile(filepath.Join(backup, "mapping.json"), mapping, 0600); err != nil {
			return err
		}
		if oldTarget == "" {
			// Read a verified regular inode, never follow an unexpected leaf link.
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			opened, statErr := f.Stat()
			if statErr != nil || !os.SameFile(previous, opened) || previous.Size() > 1<<20 {
				f.Close()
				return errors.New("authentication changed during backup")
			}
			data, readErr := io.ReadAll(io.LimitReader(f, (1<<20)+1))
			f.Close()
			if readErr != nil || len(data) > 1<<20 {
				return errors.New("could not back up authentication")
			}
			if err := os.WriteFile(filepath.Join(backup, "auth.json"), data, 0600); err != nil {
				return err
			}
		}
	}
	return os.Rename(link, path)
}

// LinkManagedAccount creates a private home or explicitly converts its auth leaf.
func LinkManagedAccount(name, source string) (string, error) {
	if !accountName.MatchString(name) {
		return "", errors.New("invalid managed account name")
	}
	if _, _, err := ResolveOAuthFile(source); err != nil {
		return "", err
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
	created := false
	if _, err := os.Lstat(home); errors.Is(err, os.ErrNotExist) {
		home, err = CreateManagedAccount(name)
		if err != nil {
			return "", err
		}
		created = true
	} else if err != nil {
		return "", err
	}
	if err := LinkAuthFile(home, source); err != nil {
		if created {
			_ = os.RemoveAll(home)
		}
		return "", err
	}
	return home, nil
}
