// Package slots limits how many offload workers run at once on the machine, across separate
// CLI processes and terminals, and keeps one resumed session from being driven twice.
//
// Capacity is a fixed set of slot files. A process owns a slot while it holds an exclusive flock on
// the file, so a crashed owner frees the lock automatically. That alone would be unsafe: the
// crashed owner's build and test processes can outlive it. The owner therefore records its worker
// process group in the slot file, and a slot whose recorded group still has live processes is
// reported as unresolved and is not reused until they are gone.
package slots

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/proc"
)

// Dir holds the slot and session lock files, next to the run log.
func Dir() string { return filepath.Join(filepath.Dir(history.Path()), "slots") }

// PollEvery is how often a queued run retries; a variable only so tests can shorten it.
var PollEvery = 250 * time.Millisecond

// Info is what a slot file records about its owner.
type Info struct {
	OwnerPID   int         `json:"owner_pid"`
	OwnerStart string      `json:"owner_start,omitempty"`
	Group      *proc.Group `json:"group,omitempty"` // worker process group still to be cleaned up
	AcquiredAt time.Time   `json:"acquired_at"`
}

// Wait describes why a queued run has not started.
type Wait struct {
	Max        int
	InUse      int      // slots owned by a live offload process
	Unresolved []string // slots whose owner is gone but whose worker processes are still alive
}

// Reason is a one-line explanation for status and watch.
func (w Wait) Reason() string {
	r := fmt.Sprintf("all %d worker slot(s) are busy (%d running", w.Max, w.InUse)
	if len(w.Unresolved) > 0 {
		r += fmt.Sprintf(", %d held by unresolved worker processes: %s", len(w.Unresolved), strings.Join(w.Unresolved, "; "))
	}
	return r + ")"
}

// Options configures Acquire.
type Options struct {
	Dir    string
	Max    int          // <= 0 disables admission control
	Check  func() error // checked before every admission attempt
	OnWait func(Wait)   // called on every failed attempt
}

// Slot is one admitted worker run. The zero value and the value returned when admission is
// off or unsupported are valid no-ops.
type Slot struct {
	idx   int
	lock  fileLock
	group *proc.Group
	info  Info
}

// Index is the slot number, or -1 when no slot is held.
func (s *Slot) Index() int {
	if s == nil || s.lock == nil {
		return -1
	}
	return s.idx
}

// Track records the worker process group this slot is responsible for.
func (s *Slot) Track(g proc.Group) {
	if s == nil || s.lock == nil {
		return
	}
	s.group = &g
	s.write()
}

// Clear says the tracked group was confirmed gone.
func (s *Slot) Clear() {
	if s == nil || s.lock == nil {
		return
	}
	s.group = nil
	s.write()
}

func (s *Slot) write() {
	s.info.Group = s.group
	if b, err := json.Marshal(s.info); err == nil {
		_ = s.lock.writeAll(b)
	}
}

// Release gives the slot up. If the tracked group still has live processes the slot file keeps
// recording it, so the capacity stays unavailable instead of being reported free. It returns
// false in that case.
func (s *Slot) Release() (clean bool) {
	if s == nil || s.lock == nil {
		return true
	}
	if s.group != nil && !s.group.Alive() {
		s.group = nil
	}
	s.write()
	s.lock.unlock()
	s.lock = nil
	return s.group == nil
}

// Acquire waits for a free slot. It returns as soon as ctx is cancelled, so a queued run that is
// interrupted never starts a backend.
func Acquire(ctx context.Context, o Options) (*Slot, error) {
	if o.Check != nil {
		if err := o.Check(); err != nil {
			return nil, err
		}
	}
	if o.Max <= 0 || !Supported {
		return &Slot{idx: -1}, nil
	}
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, err
	}
	for {
		if o.Check != nil {
			if err := o.Check(); err != nil {
				return nil, err
			}
		}
		s, wait, err := tryAcquire(o)
		if err != nil {
			return nil, err
		}
		if s != nil {
			return s, nil
		}
		if o.OnWait != nil {
			o.OnWait(wait)
		}
		// Jitter keeps separate processes from polling in lockstep.
		sleep := PollEvery/2 + time.Duration(rand.Int64N(int64(PollEvery)))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sleep):
		}
	}
}

func tryAcquire(o Options) (*Slot, Wait, error) {
	wait := Wait{Max: o.Max}
	for i := 0; i < o.Max; i++ {
		l, ok, err := openLock(filepath.Join(o.Dir, fmt.Sprintf("slot-%d.lock", i)))
		if err != nil {
			return nil, wait, err
		}
		if !ok {
			wait.InUse++
			continue
		}
		var prev Info
		if b, err := l.readAll(); err == nil && len(b) > 0 && json.Unmarshal(b, &prev) == nil && prev.Group != nil && prev.Group.Alive() {
			wait.Unresolved = append(wait.Unresolved, fmt.Sprintf("slot %d pgid %d (owner pid %d)", i, prev.Group.PGID, prev.OwnerPID))
			l.unlock()
			continue
		}
		s := &Slot{idx: i, lock: l, info: Info{OwnerPID: os.Getpid(), OwnerStart: proc.StartToken(os.Getpid()), AcquiredAt: time.Now()}}
		s.write()
		return s, wait, nil
	}
	return nil, wait, nil
}

// ErrSessionBusy means another offload process is driving the session.
var ErrSessionBusy = errors.New("session already has an active run")

// SessionLock is held while a resumed session is being driven.
type SessionLock struct{ lock fileLock }

// LockSession atomically claims a session for the calling process. The returned pid is the holder
// when ErrSessionBusy is returned. Unlike the live-state check, two simultaneous resumes cannot
// both succeed. The lock is released when the process exits, however it exits.
func LockSession(dir, session string) (*SessionLock, int, error) {
	if !Supported {
		return &SessionLock{}, 0, nil
	}
	sum := sha256.Sum256([]byte(session))
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		return nil, 0, err
	}
	l, ok, err := openLock(filepath.Join(dir, "sessions", hex.EncodeToString(sum[:8])+".lock"))
	if err != nil {
		return nil, 0, err
	}
	if !ok {
		return nil, 0, ErrSessionBusy
	}
	_ = l.writeAll([]byte(strconv.Itoa(os.Getpid())))
	return &SessionLock{lock: l}, 0, nil
}

// LockFile claims an exclusive lock on path for the life of the calling process (or until Release),
// for single-instance services such as the scheduler. busy is true when another process holds it.
// The holder's pid is written into the file. Without cross-process locking support the claim
// always succeeds and excludes nobody.
func LockFile(path string) (lock *SessionLock, busy bool, err error) {
	if !Supported {
		return &SessionLock{}, false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	l, ok, err := openLock(path)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, true, nil
	}
	_ = l.writeAll([]byte(strconv.Itoa(os.Getpid())))
	return &SessionLock{lock: l}, false, nil
}

// HolderPID reads the pid recorded by the session's lock holder, or 0 if unknown.
func HolderPID(dir, session string) int {
	sum := sha256.Sum256([]byte(session))
	b, err := os.ReadFile(filepath.Join(dir, "sessions", hex.EncodeToString(sum[:8])+".lock"))
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// Release drops the session lock.
func (l *SessionLock) Release() {
	if l != nil && l.lock != nil {
		l.lock.unlock()
		l.lock = nil
	}
}

type fileLock interface {
	readAll() ([]byte, error)
	writeAll([]byte) error
	unlock()
}
