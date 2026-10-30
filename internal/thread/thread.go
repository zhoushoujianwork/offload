// Package thread maps a caller-chosen key to the latest backend session of a conversation, so a
// caller can continue related work without storing session IDs.
package thread

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/slots"
)

const version = 1

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ErrMismatch means the key is bound to a different backend or profile.
var ErrMismatch = errors.New("thread is bound to a different backend or profile")

// Entry is the saved binding of one key in one directory.
type Entry struct {
	Version   int    `json:"version"`
	Key       string `json:"key"`
	Dir       string `json:"dir"`
	Backend   string `json:"backend"`
	Profile   string `json:"profile"`
	Session   string `json:"session"`
	UpdatedAt string `json:"updated_at"`
}

// Dir is where bindings and their locks live.
func Dir() string { return filepath.Join(filepath.Dir(history.Path()), "threads") }

// ValidKey reports whether key is a safe thread name.
func ValidKey(key string) error {
	if !keyPattern.MatchString(key) {
		return fmt.Errorf("invalid thread key %q: use 1-128 characters from A-Z a-z 0-9 . _ - and start with a letter or digit", key)
	}
	return nil
}

func id(key, dir string) string {
	sum := sha256.Sum256([]byte(key + "\x00" + dir))
	return hex.EncodeToString(sum[:12])
}

func path(key, dir string) string { return filepath.Join(Dir(), id(key, dir)+".json") }

// Lock claims the key for this process until Release. busy is true when another process holds it.
func Lock(key, dir string) (lock *slots.SessionLock, busy bool, holder int, err error) {
	p := filepath.Join(Dir(), id(key, dir)+".lock")
	l, busy, err := slots.LockFile(p)
	if err != nil || busy {
		return nil, busy, holderPID(p), err
	}
	return l, false, 0, nil
}

func holderPID(p string) int {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	var n int
	fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &n)
	return n
}

// Load returns the binding for key in dir, or nil when none exists. An unreadable or malformed
// file is an error; it is never treated as "no binding", so a damaged file is not silently replaced.
func Load(key, dir string) (*Entry, error) {
	b, err := os.ReadFile(path(key, dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("thread %q binding is unreadable (%s): %w", key, path(key, dir), err)
	}
	if e.Version != version || e.Key != key || e.Dir != dir || e.Session == "" {
		return nil, fmt.Errorf("thread %q binding is not recognized (%s)", key, path(key, dir))
	}
	return &e, nil
}

// Check refuses a binding made for another backend or profile.
func (e *Entry) Check(backend, profile string) error {
	if e.Backend != backend || e.Profile != profile {
		return fmt.Errorf("%w: thread %q uses %s/%s, this run uses %s/%s; pass -thread-reset to start a new session for the key",
			ErrMismatch, e.Key, e.Backend, e.Profile, backend, profile)
	}
	return nil
}

// Save writes the binding atomically.
func Save(key, dir, backend, profile, session string) error {
	if session == "" {
		return errors.New("thread: no session to record")
	}
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(Entry{Version: version, Key: key, Dir: dir, Backend: backend, Profile: profile, Session: session,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(Dir(), ".thread-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path(key, dir)); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// SessionMissing reports a backend refusal that means the saved session no longer exists, as
// opposed to a failure of the work itself. Only these may be answered with a fresh session.
func SessionMissing(backend, text string) bool {
	t := strings.ToLower(text)
	switch backend {
	case "codex":
		return strings.Contains(t, "no rollout found for thread id")
	default:
		return strings.Contains(t, "no conversation found with session id")
	}
}
