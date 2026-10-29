// Package live publishes the state of a running worker, so a supervisor can see it work, or
// notice it stuck, long before the run ends. One small file per offload process, rewritten
// every couple of seconds. It is advisory: nothing depends on it to run or to report.
package live

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/proc"
)

const (
	Queued     = "queued" // waiting for a free worker slot; no backend has started
	Running    = "running"
	Salvaging  = "salvaging" // stopped; asking the worker to report what it already read
	Cleaning   = "cleaning"  // worker stopped; making sure its child processes are gone
	Done       = "done"
	Unresolved = "unresolved" // finished, but worker processes are still alive and hold their slot
	Lost       = "lost"       // derived: a running state nobody has refreshed (offload was killed)
)

// StaleAfter and ExpireAfter are variables only so tests can shorten them.
var (
	StaleAfter  = 30 * time.Second
	ExpireAfter = 10 * time.Minute
)

type State struct {
	PID         int       `json:"pid"`
	RunID       string    `json:"run_id,omitempty"` // the invocation's run_id, as in the run log and processing log
	Phase       string    `json:"phase"`
	Status      string    `json:"status,omitempty"` // set once Done: ok | error | timeout
	ExitCode    int       `json:"exit_code,omitempty"`
	Dir         string    `json:"dir"`
	Task        string    `json:"task"`
	Profile     string    `json:"profile"`
	Model       string    `json:"model"`
	Session     string    `json:"session,omitempty"`
	Caller      string    `json:"caller"` // "unspecified" when not given (also for legacy state files)
	StartedAt   time.Time `json:"started_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	LastEventAt time.Time `json:"last_event_at"`
	ToolCalls   int       `json:"tool_calls"`
	Denials     int       `json:"permission_denials"`
	LastTool    string    `json:"last_tool,omitempty"`

	WaitReason string `json:"wait_reason,omitempty"` // why a queued run has not started
	WaitMs     int64  `json:"wait_ms,omitempty"`     // time spent queued
	Slot       *int   `json:"slot,omitempty"`        // worker slot held, once admitted
	Limits     string `json:"limits,omitempty"`      // effective limits, for status
	PGID       int    `json:"pgid,omitempty"`        // worker process group; the worker's PID is the group leader
	PGIDStart  string `json:"pgid_start,omitempty"`  // leader start time, to detect a recycled PID

	// Unresolved is derived by List: the worker's process group still has live processes although
	// this state is no longer being refreshed or the run is over.
	Unresolved bool `json:"-"`
}

// Processing reports whether the invocation currently occupies a worker: running, or salvaging
// inside that same invocation. Queued, cleaning, lost, unresolved and finished states do not.
func (s State) Processing() bool { return s.Phase == Running || s.Phase == Salvaging }

// Active reports whether the worker is still doing something.
func (s State) Active() bool {
	return s.Phase == Queued || s.Phase == Running || s.Phase == Salvaging || s.Phase == Cleaning
}

// Group is the recorded worker process group, if any.
func (s State) Group() (proc.Group, bool) {
	if s.PGID <= 1 {
		return proc.Group{}, false
	}
	return proc.Group{PID: s.PGID, PGID: s.PGID, Start: s.PGIDStart}, true
}

// Dir sits next to the run log.
func Dir() string { return filepath.Join(filepath.Dir(history.Path()), "live") }

// Publish atomically writes the state for its process.
func Publish(s State) error {
	s.UpdatedAt = time.Now()
	s.Caller = history.CallerOrUnspecified(s.Caller)
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	final := filepath.Join(Dir(), strconv.Itoa(s.PID)+".json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// List returns the known workers oldest first. Expired files are deleted, and an active state
// that stopped being refreshed is reported as Lost.
func List() ([]State, error) {
	out, _, err := ListChecked()
	return out, err
}

// ListChecked is List that also reports how many state files could not be read or parsed. Such a
// file may have been a worker, so a caller counting workers must treat its result as a lower bound.
func ListChecked() (out []State, unreadable int, err error) {
	entries, err := os.ReadDir(Dir())
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(Dir(), e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) { // a file that vanished between listing and reading was just expired
				unreadable++
			}
			continue
		}
		var s State
		if json.Unmarshal(b, &s) != nil {
			unreadable++
			continue
		}
		s.Caller = history.CallerOrUnspecified(s.Caller)
		age := time.Since(s.UpdatedAt)
		stale := s.Active() && age > StaleAfter
		// A heartbeat that stopped says nothing about the worker's processes. Check the recorded
		// group, and keep the state visible while it is alive instead of expiring it.
		if g, ok := s.Group(); ok && s.Phase != Done && (stale || s.Phase == Unresolved) {
			s.Unresolved = g.Alive()
			if s.Phase == Unresolved && !s.Unresolved {
				s.Phase = Done // the leftover processes exited after the owner gave up on them
			}
		}
		if age > ExpireAfter && !s.Unresolved {
			os.Remove(path)
			continue
		}
		if stale {
			s.Phase = Lost
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, unreadable, nil
}
