// Package threads maps a caller-chosen thread key to the latest backend session of that
// conversation, so a caller can continue related work by name instead of tracking session IDs.
//
// Each thread is one small JSON file in the Offload state directory, named by a hash of the key
// and the working directory. A damaged file therefore affects only its own thread. A per-thread
// lock file, held for the whole run, keeps two invocations from driving one session.
package threads

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/slots"
)

const (
	version = 1
	// MaxKeyBytes bounds a thread key.
	MaxKeyBytes = 128
)

// ErrBusy means another offload process holds the thread.
var ErrBusy = errors.New("already has an active run")

// ErrCorrupt means the thread's mapping file exists but cannot be trusted.
var ErrCorrupt = errors.New("thread mapping is unreadable")

// Entry is the saved mapping of one thread.
type Entry struct {
	Version int    `json:"version"`
	Key     string `json:"key"`
	Dir     string `json:"dir"`
	Backend string `json:"backend"`
	Profile string `json:"profile"`
	Session string `json:"session"`
	Created string `json:"created"`
	Updated string `json:"updated"`
}

// Dir is where thread files live: next to the run log in the Offload state directory.
func Dir() string { return filepath.Join(filepath.Dir(history.Path()), "threads") }

// ValidateKey rejects keys that are empty, too long, not UTF-8 or contain whitespace or control
// characters. The key is hashed for file names, so any other character is safe.
func ValidateKey(key string) error {
	switch {
	case key == "":
		return errors.New("--thread: empty key")
	case len(key) > MaxKeyBytes:
		return fmt.Errorf("--thread: key longer than %d bytes", MaxKeyBytes)
	case !utf8.ValidString(key):
		return errors.New("--thread: key is not valid UTF-8")
	}
	for _, r := range key {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("--thread: key must not contain whitespace or control characters")
		}
	}
	return nil
}

func fileBase(dir, key, wd string) string {
	sum := sha256.Sum256([]byte(key + "\x00" + wd))
	return filepath.Join(dir, hex.EncodeToString(sum[:12]))
}

// Handle is an exclusive claim on one thread. It stays valid until Release or process exit.
type Handle struct {
	lock *slots.SessionLock
	path string
	key  string
	wd   string
}

// Acquire claims the thread (key within working directory wd). It returns ErrBusy, wrapped with the
// holder's pid when known, if another process holds it.
func Acquire(dir, key, wd string) (*Handle, error) {
	base := fileBase(dir, key, wd)
	lock, busy, err := slots.LockFile(base + ".lock")
	if err != nil {
		return nil, fmt.Errorf("lock thread: %w", err)
	}
	if busy {
		pid := 0
		if b, rerr := os.ReadFile(base + ".lock"); rerr == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		if pid > 0 {
			return nil, fmt.Errorf("%w (pid %d)", ErrBusy, pid)
		}
		return nil, ErrBusy
	}
	return &Handle{lock: lock, path: base + ".json", key: key, wd: wd}, nil
}

// Path is the mapping file of the thread.
func (h *Handle) Path() string { return h.path }

// Load returns the saved entry, or nil when the thread has none. A file that cannot be parsed or
// does not describe this thread yields ErrCorrupt; nothing is modified.
func (h *Handle) Load() (*Entry, error) {
	b, err := os.ReadFile(h.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if e.Version != version || e.Key != h.key || e.Dir != h.wd || e.Session == "" || e.Backend == "" || e.Profile == "" {
		return nil, fmt.Errorf("%w: unexpected content", ErrCorrupt)
	}
	return &e, nil
}

// Save replaces the mapping atomically. created is kept from prev when given.
func (h *Handle) Save(prev *Entry, backend, profile, session string, now time.Time) error {
	ts := now.UTC().Format(time.RFC3339)
	e := Entry{Version: version, Key: h.key, Dir: h.wd, Backend: backend, Profile: profile, Session: session, Created: ts, Updated: ts}
	if prev != nil && prev.Created != "" {
		e.Created = prev.Created
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(h.path), ".thread-*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), h.path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Release drops the claim.
func (h *Handle) Release() {
	if h != nil {
		h.lock.Release()
	}
}
