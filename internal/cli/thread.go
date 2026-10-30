package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/zhoushoujianwork/offload/internal/slots"
	"github.com/zhoushoujianwork/offload/internal/thread"
)

// threadRun is the state of one -thread invocation between choosing its session and recording it.
type threadRun struct {
	key     string
	dir     string // absolute invocation directory the binding is scoped to
	start   string // fresh | resumed | reset | fallback
	session string // saved session to resume, empty for a new one
	entry   *thread.Entry
	lock    *slots.SessionLock
}

// beginThread claims the key and decides whether this run starts a session or resumes the saved one.
// The returned lock is held until the process exits or the caller releases it.
func beginThread(key, dir string, reset bool) (*threadRun, error) {
	if err := thread.ValidKey(key); err != nil {
		return nil, err
	}
	abs := dir
	if abs == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		abs = wd
	}
	abs, err := filepath.Abs(abs)
	if err != nil {
		return nil, err
	}
	lock, busy, pid, err := thread.Lock(key, abs)
	if err != nil {
		return nil, fmt.Errorf("lock thread %s: %w", key, err)
	}
	if busy {
		return nil, fmt.Errorf("thread %s already has an active run (pid %d); do not send overlapping follow-ups to one conversation. "+
			"Wait for it (offload watch -t 10m), then run again", key, pid)
	}
	t := &threadRun{key: key, dir: abs, start: "fresh", lock: lock}
	if reset {
		t.start = "reset"
		return t, nil
	}
	entry, err := thread.Load(key, abs)
	if err != nil {
		lock.Release()
		return nil, err
	}
	if entry != nil {
		t.start, t.session, t.entry = "resumed", entry.Session, entry
	}
	return t, nil
}
