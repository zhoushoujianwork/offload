package schedule

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/slots"
)

// Dir holds schedule definitions and results, next to the run log.
func Dir() string { return filepath.Join(filepath.Dir(history.Path()), "schedules") }

// Misfire policies decide what happens to an occurrence the scheduler finds overdue by more than
// MisfireGrace (the machine slept, or no scheduler was running).
const (
	MisfireSkip    = "skip"     // record it as missed and wait for the next occurrence
	MisfireRunOnce = "run-once" // run once now, however many occurrences were missed
)

// Result statuses.
const (
	StatusOK      = "ok"
	StatusFailed  = "failed"
	StatusTimeout = "timeout"
	StatusMissed  = "missed"  // overdue beyond the grace period under the skip policy; not run
	StatusSkipped = "skipped" // due, but not started: global disablement or a previous run still active
	StatusLost    = "lost"    // the invocation vanished without leaving a result (host crash or kill -9)
)

// DefaultKeep is how many results are retained per task; older ones are deleted.
const DefaultKeep = 50

// MisfireGrace is how late an occurrence may start and still count as on time. A variable only so
// tests can change it.
var MisfireGrace = 2 * time.Minute

// Task is one stored scheduled task. It holds the prompt and run options but never a credential:
// Profile is only a name resolved through the normal profile mechanism when the task runs.
type Task struct {
	ID              string   `json:"id"`
	Name            string   `json:"name,omitempty"`
	Prompt          string   `json:"prompt"`
	Dir             string   `json:"dir"`
	Profile         string   `json:"profile,omitempty"`
	Model           string   `json:"model,omitempty"`
	Write           bool     `json:"write,omitempty"`
	Policy          string   `json:"execution_policy"`
	Timeout         string   `json:"timeout,omitempty"`
	Idle            string   `json:"idle,omitempty"`
	AddDirs         []string `json:"add_dirs,omitempty"`
	Sandbox         string   `json:"codex_sandbox,omitempty"`
	NoSalvage       bool     `json:"no_salvage,omitempty"`
	ToolsDisabled   bool     `json:"tools_disabled,omitempty"`
	AllowedTools    string   `json:"allowed_tools,omitempty"`
	DisallowedTools string   `json:"disallowed_tools,omitempty"`
	// OnUnsupportedTools is "fail" (default) or "ignore"; see offload -on-unsupported-tools.
	OnUnsupportedTools string `json:"on_unsupported_tools,omitempty"`

	// Keep is how many results to retain (default DefaultKeep).
	Keep int `json:"keep,omitempty"`

	Schedule Spec   `json:"schedule"`
	Misfire  string `json:"misfire"`
	Paused   bool   `json:"paused"`
	// NextRun is the next unclaimed occurrence; nil while paused or once a one-time task has fired.
	NextRun *time.Time `json:"next_run,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	Active *Active `json:"active,omitempty"` // the invocation in flight, if any
	Last   *Result `json:"last,omitempty"`   // most recent outcome of any kind
	// ConsecutiveFailures counts failed, timed-out and lost runs since the last ok run.
	ConsecutiveFailures int `json:"consecutive_failures"`
}

// Active is a claimed invocation. It is stored before the process starts, so an occurrence is
// claimed exactly once even if the scheduler dies mid-launch.
type Active struct {
	RunID        string    `json:"run_id"`
	ScheduledFor time.Time `json:"scheduled_for"`
	StartedAt    time.Time `json:"started_at"`
	PID          int       `json:"pid,omitempty"`
	PIDStart     string    `json:"pid_start,omitempty"`
}

// Result is one retained outcome: a run, or an occurrence that was missed or skipped.
type Result struct {
	RunID        string          `json:"run_id"`
	TaskID       string          `json:"task_id"`
	Status       string          `json:"status"`
	Reason       string          `json:"reason,omitempty"`
	ScheduledFor time.Time       `json:"scheduled_for"`
	StartedAt    *time.Time      `json:"started_at,omitempty"`
	FinishedAt   time.Time       `json:"finished_at"`
	ExitCode     *int            `json:"exit_code,omitempty"`
	Session      string          `json:"session,omitempty"`
	Error        string          `json:"error,omitempty"`
	Report       string          `json:"report,omitempty"` // the worker's final report
	Stderr       string          `json:"stderr,omitempty"` // tail of offload's own stderr summary
	Record       *history.Record `json:"record,omitempty"` // the full offload run record
}

// State is the lifecycle state shown to users.
func (t Task) State() string {
	switch {
	case t.Active != nil:
		return "running"
	case t.Paused:
		return "paused"
	case t.NextRun == nil:
		return "completed"
	}
	return "scheduled"
}

// Store persists tasks as one file each, so a crash can damage at most the file being written.
type Store struct{ Root string }

// NewStore uses the default directory when root is empty.
func NewStore(root string) *Store {
	if root == "" {
		root = Dir()
	}
	return &Store{Root: root}
}

func (s *Store) taskPath(id string) string { return filepath.Join(s.Root, "tasks", id+".json") }
func (s *Store) runsDir(id string) string  { return filepath.Join(s.Root, "runs", id) }

// ErrNotFound means no task matches a reference.
var ErrNotFound = errors.New("no such scheduled task")

// Lock serializes read-modify-write cycles across processes (CLI commands and the scheduler).
func (s *Store) Lock() (func(), error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		l, busy, err := slots.LockFile(filepath.Join(s.Root, "store.lock"))
		if err != nil {
			return nil, err
		}
		if !busy {
			return l.Release, nil
		}
		if time.Now().After(deadline) {
			return nil, errors.New("schedule store is locked by another offload process")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// NewID returns a short random task id.
func NewID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "t-" + hex.EncodeToString(b[:])
}

// NewRunID sorts chronologically by name.
func NewRunID(now time.Time) string {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// Save writes a task atomically.
func (s *Store) Save(t *Task) error {
	return writeJSON(s.taskPath(t.ID), t)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path) // CreateTemp files are 0600
}

// Load reads one task by exact id.
func (s *Store) Load(id string) (*Task, error) {
	b, err := os.ReadFile(s.taskPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, err
	}
	var t Task
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.taskPath(id), err)
	}
	return &t, nil
}

// List returns every readable task ordered by creation time. A damaged file is reported in
// warnings and does not hide the others.
func (s *Store) List() (tasks []*Task, warnings []string, err error) {
	ents, err := os.ReadDir(filepath.Join(s.Root, "tasks"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		t, err := s.Load(strings.TrimSuffix(name, ".json"))
		if err != nil {
			warnings = append(warnings, err.Error())
			continue
		}
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool {
		if !tasks[i].CreatedAt.Equal(tasks[j].CreatedAt) {
			return tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
		}
		return tasks[i].ID < tasks[j].ID
	})
	return tasks, warnings, nil
}

// Find resolves an id, a name, or an unambiguous id prefix.
func (s *Store) Find(ref string) (*Task, error) {
	if ref == "" {
		return nil, errors.New("task id or name required")
	}
	if !strings.ContainsAny(ref, `/\`) {
		if t, err := s.Load(ref); err == nil {
			return t, nil
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	all, _, err := s.List()
	if err != nil {
		return nil, err
	}
	var hits []*Task
	for _, t := range all {
		if t.Name == ref {
			hits = append(hits, t)
		}
	}
	if len(hits) == 0 {
		for _, t := range all {
			if strings.HasPrefix(t.ID, ref) {
				hits = append(hits, t)
			}
		}
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
	case 1:
		return hits[0], nil
	}
	return nil, fmt.Errorf("%q matches %d tasks; use the full id", ref, len(hits))
}

// Delete removes a task definition and its retained results.
func (s *Store) Delete(id string) error {
	if err := os.Remove(s.taskPath(id)); err != nil {
		return err
	}
	return os.RemoveAll(s.runsDir(id))
}

// SaveResult stores a result and prunes the oldest beyond keep.
func (s *Store) SaveResult(r *Result, keep int) error {
	if err := writeJSON(filepath.Join(s.runsDir(r.TaskID), r.RunID+".json"), r); err != nil {
		return err
	}
	if keep <= 0 {
		keep = DefaultKeep
	}
	return s.prune(r.TaskID, keep)
}

// ran reports whether a result is a real invocation rather than a missed or skipped occurrence.
func (r *Result) ran() bool { return r.Status != StatusMissed && r.Status != StatusSkipped }

// prune keeps the newest keep invocations and, separately, the newest keep missed or skipped
// records, so a long outage cannot push real results out of the retained set.
func (s *Store) prune(id string, keep int) error {
	names, err := s.resultNames(id)
	if err != nil || len(names) <= keep {
		return err
	}
	var ran, other int
	for i := len(names) - 1; i >= 0; i-- {
		r, err := s.Result(id, names[i])
		if err != nil {
			continue
		}
		n := &other
		if r.ran() {
			n = &ran
		}
		if *n++; *n > keep {
			_ = os.Remove(filepath.Join(s.runsDir(id), names[i]+".json"))
		}
	}
	return nil
}

// resultNames lists run ids oldest first.
func (s *Store) resultNames(id string) ([]string, error) {
	ents, err := os.ReadDir(s.runsDir(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if n := e.Name(); !e.IsDir() && strings.HasSuffix(n, ".json") && !strings.HasPrefix(n, ".") {
			names = append(names, strings.TrimSuffix(n, ".json"))
		}
	}
	sort.Strings(names)
	return names, nil
}

// Results returns the newest n results, newest first (n <= 0 means all retained).
func (s *Store) Results(id string, n int) ([]*Result, error) {
	names, err := s.resultNames(id)
	if err != nil {
		return nil, err
	}
	var out []*Result
	for i := len(names) - 1; i >= 0 && (n <= 0 || len(out) < n); i-- {
		r, err := s.Result(id, names[i])
		if err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// Result loads one result by run id.
func (s *Store) Result(id, runID string) (*Result, error) {
	if strings.ContainsAny(runID, `/\`) || strings.HasPrefix(runID, ".") {
		return nil, fmt.Errorf("invalid run id %q", runID)
	}
	b, err := os.ReadFile(filepath.Join(s.runsDir(id), runID+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no retained result %s for task %s", runID, id)
	}
	if err != nil {
		return nil, err
	}
	var r Result
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// SchedulerStatus reports whether a scheduler process currently holds the scheduler lock.
func (s *Store) SchedulerStatus() (running bool, pid int) {
	path := filepath.Join(s.Root, "scheduler.lock")
	l, busy, err := slots.LockFile(path)
	if err == nil && !busy {
		l.Release()
		return false, 0
	}
	if !busy {
		return false, 0
	}
	b, _ := os.ReadFile(path)
	pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	return true, pid
}

// LockScheduler claims the single-scheduler lock, retrying briefly so a status probe that holds it
// for an instant does not cause a spurious refusal. busy reports another scheduler (with its pid).
func (s *Store) LockScheduler() (release func(), busyPID int, busy bool, err error) {
	path := filepath.Join(s.Root, "scheduler.lock")
	for i := 0; ; i++ {
		l, b, err := slots.LockFile(path)
		if err != nil {
			return nil, 0, false, err
		}
		if !b {
			return l.Release, 0, false, nil
		}
		if i >= 20 {
			raw, _ := os.ReadFile(path)
			pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
			return nil, pid, true, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}
