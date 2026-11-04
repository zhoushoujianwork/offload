package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// NewEnvironment creates an owned, persistent native home and empty workspace.
// Retention belongs to the caller; this is configuration isolation, not a sandbox.
func NewEnvironment() (string, error) {
	root, err := os.MkdirTemp("", "offload-environment-")
	if err != nil {
		return "", err
	}
	if err := initializeEnvironment(root, true); err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(root)
}

// NewEnvironmentAt exclusively creates an environment at a caller-selected root.
// It never removes or overwrites a root that existed before this call.
func NewEnvironmentAt(root string) (string, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return "", errors.New("environment root must be a clean absolute path")
	}
	if err := initializeEnvironment(root, false); err != nil {
		return "", err
	}
	return root, nil
}

func initializeEnvironment(root string, exists bool) error {
	created, ready := root, false
	defer func() {
		if !ready {
			_ = os.RemoveAll(created)
		}
	}()
	if !exists {
		if err := os.Mkdir(root, 0700); err != nil {
			created = ""
			return err
		}
	} else {
		canonical, err := filepath.EvalSymlinks(root)
		if err != nil {
			return err
		}
		root, created = canonical, canonical
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || canonical != root {
		return errors.New("environment root must not contain symlinks")
	}
	for _, name := range []string{"workspace", "home", "codex", "claude", "config", "cache", "data"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(root, "environment.json"), []byte(`{"version":1}`), 0600); err != nil {
		return err
	}
	ready = true
	return nil
}

func privateEnvironmentFile(path string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || before.Size() > 1<<20 {
		return nil, errors.New("isolated environment requires a private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) {
		return nil, errors.New("isolated environment file changed while opening")
	}
	raw, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, errors.New("isolated environment file exceeds limit")
	}
	return raw, nil
}

func environmentDirectory(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return errors.New("environment root must be a clean absolute path")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || canonical != root {
		return errors.New("environment root must not contain symlinks")
	}
	for _, name := range []string{"", "workspace", "home", "codex", "claude", "config", "cache", "data"} {
		st, err := os.Lstat(filepath.Join(root, name))
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0 {
			return errors.New("environment directories must be private and not symlinks")
		}
	}
	raw, err := privateEnvironmentFile(filepath.Join(root, "environment.json"))
	if err != nil {
		return err
	}
	var metadata struct {
		Version int `json:"version"`
	}
	if json.Unmarshal(raw, &metadata) != nil || metadata.Version != 1 {
		return errors.New("unrecognized isolated environment")
	}
	return nil
}

func (o *Opts) prepareEnvironment(sourceHome string) (func() error, error) {
	if o.EnvironmentDirectory == "" {
		return func() error { return nil }, nil
	}
	root := o.EnvironmentDirectory
	if err := environmentDirectory(root); err != nil {
		return nil, err
	}
	if o.Native != nil && o.Native.ProfileDirectory != "" {
		return nil, errors.New("an explicit native profile cannot be combined with an isolated environment")
	}
	if o.Dir != filepath.Join(root, "workspace") {
		return nil, errors.New("isolated execution requires its empty owned workspace; mount task directories explicitly")
	}
	binding, _ := json.Marshal([]string{o.ProfileName, o.Profile.BackendName(), o.Profile.CodexAccount, sourceHome})
	sum := sha256.Sum256(binding)
	identity := []byte(hex.EncodeToString(sum[:]))
	path := filepath.Join(root, "binding")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		_, err = f.Write(identity)
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
	} else if errors.Is(err, os.ErrExist) {
		var existing []byte
		existing, err = privateEnvironmentFile(path)
		if err == nil && !bytes.Equal(existing, identity) {
			err = errors.New("isolated environment belongs to another execution profile or account")
		}
	}
	if err != nil {
		return nil, err
	}
	o.environment = []string{"HOME=" + filepath.Join(root, "home"), "XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache"), "XDG_DATA_HOME=" + filepath.Join(root, "data"), "XDG_STATE_HOME=" + filepath.Join(root, "data"), "CLAUDE_CONFIG_DIR=" + filepath.Join(root, "claude")}
	for _, key := range []string{"PATH", "TMPDIR", "LANG", "LC_ALL", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value, ok := os.LookupEnv(key); ok {
			o.environment = append(o.environment, key+"="+value)
		}
	}
	if o.Profile.BackendName() != "codex" {
		if o.Native != nil {
			snapshot := *o.Native
			snapshot.ProfileDirectory = filepath.Join(root, "claude")
			o.Native = &snapshot
		}
		return func() error { return nil }, nil
	}
	destination := filepath.Join(root, "codex")
	o.CodexHome = destination
	if o.Resume != "" {
		if _, err := codexRollout(o.Resume, destination); err != nil {
			return nil, errors.New("native session is not recorded in this isolated environment")
		}
	}
	target := filepath.Join(destination, "auth.json")
	if _, err := os.Lstat(target); err == nil {
		if _, err = privateEnvironmentFile(target); err != nil {
			return nil, err
		}
		return func() error { return nil }, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	source := filepath.Join(sourceHome, "auth.json")
	auth, err := privateEnvironmentFile(source)
	if err != nil {
		return nil, errors.New("isolated Codex execution requires file-backed authentication for the selected account")
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		_, err = file.Write(auth)
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
	} else if errors.Is(err, os.ErrExist) {
		_, err = privateEnvironmentFile(target)
	}
	if err != nil {
		return nil, err
	}
	// This environment owns its authentication snapshot and native refreshes.
	// Never race a source account's login/logout or overwrite its credentials.
	return func() error { return nil }, nil
}

func (o Opts) baseEnvironment() []string {
	if o.environment != nil {
		return append([]string(nil), o.environment...)
	}
	return os.Environ()
}
