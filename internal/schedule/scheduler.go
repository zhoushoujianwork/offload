package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/proc"
)

const (
	maxOutput   = 8 << 20 // largest child record read back
	stderrTail  = 4 << 10
	launchGrace = time.Minute // an Active claim without a pid is presumed dead after this
)

// Scheduler starts due tasks. All durable state is in the Store, so a restart loses nothing.
type Scheduler struct {
	Store *Store
	// Exe is the offload binary each occurrence runs; empty means the current executable.
	Exe string
	// Now and Enabled are replaceable for tests. Enabled returns the global-disable error.
	Now     func() time.Time
	Enabled func() error
	Keep    int
	// Grace is how late an occurrence may start and still count as on time (default MisfireGrace).
	Grace time.Duration
	Log   io.Writer

	mu      sync.Mutex
	tracked map[string]bool // run ids whose child this process waits for
	wg      sync.WaitGroup
}

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Scheduler) logf(format string, a ...any) {
	if s.Log != nil {
		fmt.Fprintf(s.Log, "%s "+format+"\n", append([]any{s.now().UTC().Format(time.RFC3339)}, a...)...)
	}
}

// Wait blocks until every child this process started has finished and been recorded.
func (s *Scheduler) Wait() { s.wg.Wait() }

// Run ticks until ctx ends. Polling the wall clock, rather than sleeping until the next
// occurrence, makes a suspended machine catch up correctly on wake.
func (s *Scheduler) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := s.Tick(); err != nil {
			s.logf("tick: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick reaps finished invocations and starts every due occurrence exactly once. An occurrence is
// claimed by saving the task, with its next run already advanced and the invocation recorded as
// active, before the process starts. A crash at any point therefore yields at most a "lost"
// result, never a second execution of the same occurrence.
func (s *Scheduler) Tick() error {
	unlock, err := s.Store.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	tasks, warnings, err := s.Store.List()
	if err != nil {
		return err
	}
	for _, w := range warnings {
		s.logf("skipping unreadable task: %v", w)
	}
	for _, t := range tasks {
		if err := s.tickTask(t); err != nil {
			s.logf("task %s: %v", t.ID, err)
		}
	}
	return nil
}

func (s *Scheduler) isTracked(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tracked[runID]
}

func activeAlive(a *Active, now time.Time) bool {
	if a.PID == 0 {
		return now.Sub(a.StartedAt) < launchGrace
	}
	return proc.Group{PID: a.PID, PGID: a.PID, Start: a.PIDStart}.Alive()
}

func (s *Scheduler) tickTask(t *Task) error {
	now := s.now()
	if t.Active != nil && !s.isTracked(t.Active.RunID) && !activeAlive(t.Active, now) {
		// The scheduler that started it is gone (restart or crash) and the process has ended.
		res := s.resultFromFiles(t, t.Active, nil, now)
		if err := s.record(t, res); err != nil {
			return err
		}
		s.logf("task %s: recovered run %s as %s", t.ID, res.RunID, res.Status)
	}
	if t.Paused || t.NextRun == nil || t.NextRun.After(now) {
		return nil
	}
	due := *t.NextRun
	missed := countOccurrences(t.Schedule, due, now)
	next, ok, err := t.Schedule.Next(now)
	if err != nil {
		return err
	}
	if ok {
		t.NextRun = &next
	} else {
		t.NextRun = nil
	}
	grace := s.Grace
	if grace <= 0 {
		grace = MisfireGrace
	}
	onTime := now.Sub(due) <= grace
	skip := func(status, reason string) error {
		r := &Result{RunID: NewRunID(now), TaskID: t.ID, Status: status, Reason: reason, ScheduledFor: due, FinishedAt: now}
		t.Last = r
		if err := s.Store.Save(t); err != nil {
			return err
		}
		s.logf("task %s: %s: %s", t.ID, status, reason)
		return s.Store.SaveResult(r, s.keep(t))
	}
	switch {
	case s.enabledErr() != nil:
		return skip(StatusSkipped, s.enabledErr().Error())
	case t.Active != nil:
		return skip(StatusSkipped, fmt.Sprintf("previous invocation %s is still running; overlapping runs of one task are never started", t.Active.RunID))
	case !onTime && t.Misfire != MisfireRunOnce:
		return skip(StatusMissed, fmt.Sprintf("%d occurrence(s) were overdue by more than %s (first due %s); policy %q does not catch up",
			missed, grace, due.UTC().Format(time.RFC3339), MisfireSkip))
	}
	note := ""
	if !onTime {
		note = fmt.Sprintf("catch-up run for %d overdue occurrence(s) (first due %s); policy %q", missed, due.UTC().Format(time.RFC3339), MisfireRunOnce)
	}
	return s.launch(t, due, note, now)
}

func (s *Scheduler) keep(t *Task) int {
	if t.Keep > 0 {
		return t.Keep
	}
	return s.Keep
}

func (s *Scheduler) enabledErr() error {
	if s.Enabled == nil {
		return nil
	}
	return s.Enabled()
}

// countOccurrences counts occurrences from first through now, stopping at a bound.
func countOccurrences(sp Spec, first, now time.Time) int {
	n, at := 1, first
	for n < 10000 {
		next, ok, err := sp.Next(at)
		if err != nil || !ok || next.After(now) {
			break
		}
		n++
		at = next
	}
	return n
}

// BuildArgs maps a stored task to offload CLI flags. The prompt travels on stdin, so it is
// neither visible in the process list nor bounded by the argument length limit.
func BuildArgs(t *Task, runID string) []string {
	args := []string{"-json", "-d", t.Dir, "--execution-policy", t.Policy, "--caller-run-id", "sched:" + t.ID + ":" + runID}
	add := func(flag, v string) {
		if v != "" {
			args = append(args, flag, v)
		}
	}
	add("-p", t.Profile)
	add("-m", t.Model)
	add("-t", t.Timeout)
	add("-idle", t.Idle)
	add("--codex-sandbox", t.Sandbox)
	add("--allowed-tools", t.AllowedTools)
	add("--disallowed-tools", t.DisallowedTools)
	if t.Write {
		args = append(args, "-w")
	}
	if t.NoSalvage {
		args = append(args, "--no-salvage")
	}
	if t.ToolsDisabled {
		args = append(args, "--tools-disabled")
	}
	for _, d := range t.AddDirs {
		args = append(args, "--add-dir", d)
	}
	return args
}

func (s *Scheduler) exe() (string, error) {
	if s.Exe != "" {
		return s.Exe, nil
	}
	return os.Executable()
}

func (s *Scheduler) outPath(t *Task, runID, ext string) string {
	return filepath.Join(s.Store.runsDir(t.ID), runID+ext)
}

// launch claims the occurrence, then starts the child. The caller holds the store lock.
func (s *Scheduler) launch(t *Task, due time.Time, note string, now time.Time) error {
	runID := NewRunID(now)
	t.Active = &Active{RunID: runID, ScheduledFor: due, StartedAt: now}
	if err := s.Store.Save(t); err != nil {
		return err
	}
	fail := func(err error) error {
		res := &Result{RunID: runID, TaskID: t.ID, Status: StatusFailed, ScheduledFor: due, Reason: note, StartedAt: &now,
			FinishedAt: s.now(), Error: "start offload: " + err.Error()}
		return s.record(t, res)
	}
	exe, err := s.exe()
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(s.Store.runsDir(t.ID), 0o700); err != nil {
		return fail(err)
	}
	out, err := os.OpenFile(s.outPath(t, runID, ".out"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return fail(err)
	}
	defer out.Close()
	errf, err := os.OpenFile(s.outPath(t, runID, ".err"), os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return fail(err)
	}
	defer errf.Close()
	// The prompt goes in through a private file used as stdin, then the file is unlinked. The child
	// holds its own descriptor, so a scheduler that dies right after Start cannot truncate it.
	promptPath := s.outPath(t, runID, ".prompt")
	if err := os.WriteFile(promptPath, []byte(t.Prompt), 0o600); err != nil {
		return fail(err)
	}
	prompt, err := os.Open(promptPath)
	_ = os.Remove(promptPath)
	if err != nil {
		return fail(err)
	}
	defer prompt.Close()
	cmd := exec.Command(exe, BuildArgs(t, runID)...)
	cmd.Stdin = prompt
	cmd.Stdout, cmd.Stderr = out, errf
	proc.Prepare(cmd) // own process group: Ctrl-C on the scheduler does not kill an admitted worker
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	g := proc.Started(cmd)
	t.Active.PID, t.Active.PIDStart = g.PID, g.Start
	if err := s.Store.Save(t); err != nil {
		s.logf("task %s: record pid: %v", t.ID, err)
	}
	s.logf("task %s: started run %s (pid %d)", t.ID, runID, g.PID)
	s.mu.Lock()
	if s.tracked == nil {
		s.tracked = map[string]bool{}
	}
	s.tracked[runID] = true
	s.mu.Unlock()
	active := *t.Active
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		waitErr := cmd.Wait()
		s.finish(t.ID, active, note, waitErr)
	}()
	return nil
}

// finish records a child that this process waited for.
func (s *Scheduler) finish(taskID string, a Active, note string, waitErr error) {
	defer func() {
		s.mu.Lock()
		delete(s.tracked, a.RunID)
		s.mu.Unlock()
	}()
	unlock, err := s.Store.Lock()
	if err != nil {
		s.logf("task %s: cannot record run %s: %v", taskID, a.RunID, err)
		return
	}
	defer unlock()
	t, err := s.Store.Load(taskID)
	if err != nil || t.Active == nil || t.Active.RunID != a.RunID {
		return // deleted, or already recorded by a recovery pass
	}
	res := s.resultFromFiles(t, &a, &waitErr, s.now())
	if res.Reason == "" {
		res.Reason = note
	}
	if err := s.record(t, res); err != nil {
		s.logf("task %s: record run %s: %v", taskID, a.RunID, err)
		return
	}
	s.logf("task %s: run %s finished: %s", taskID, a.RunID, res.Status)
}

// resultFromFiles builds a Result from the child's captured output. waitErr is nil when the child
// was not waited for by this process (recovery), in which case a missing record means "lost".
func (s *Scheduler) resultFromFiles(t *Task, a *Active, waitErr *error, now time.Time) *Result {
	started := a.StartedAt
	res := &Result{RunID: a.RunID, TaskID: t.ID, ScheduledFor: a.ScheduledFor, StartedAt: &started, FinishedAt: now}
	outPath, errPath := s.outPath(t, a.RunID, ".out"), s.outPath(t, a.RunID, ".err")
	defer os.Remove(outPath)
	defer os.Remove(errPath)
	res.Stderr = tail(errPath, stderrTail)
	var rec history.Record
	b := readCapped(outPath, maxOutput)
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &rec); err != nil || len(strings.TrimSpace(string(b))) == 0 {
		if waitErr == nil {
			res.Status = StatusLost
			res.Error = "the invocation ended without a result record (host crash, reboot or kill)"
			return res
		}
		res.Status = StatusFailed
		res.Error = "offload produced no result record"
		if *waitErr != nil {
			res.Error += ": " + (*waitErr).Error()
			var ee *exec.ExitError
			if errors.As(*waitErr, &ee) {
				code := ee.ExitCode()
				res.ExitCode = &code
			}
		}
		return res
	}
	res.Record = &rec
	res.ExitCode, res.Session, res.Report, res.Error = rec.ExitCode, rec.Session, rec.Result, rec.Error
	switch {
	case rec.Status == "ok" && (rec.ExitCode == nil || *rec.ExitCode == 0):
		res.Status = StatusOK
	case rec.Status == "timeout":
		res.Status = StatusTimeout
	default:
		res.Status = StatusFailed
	}
	if res.Status != StatusOK && res.Error == "" {
		res.Error = firstLine(rec.Result)
	}
	return res
}

// record stores the result, clears the active claim and updates the failure counter.
func (s *Scheduler) record(t *Task, res *Result) error {
	t.Active = nil
	t.Last = res
	if res.Status == StatusOK {
		t.ConsecutiveFailures = 0
	} else if res.Status == StatusFailed || res.Status == StatusTimeout || res.Status == StatusLost {
		t.ConsecutiveFailures++
	}
	// The result file comes first: if only the task save fails, the result is still retained.
	if err := s.Store.SaveResult(res, s.keep(t)); err != nil {
		return err
	}
	return s.Store.Save(t)
}

func readCapped(path string, max int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, max))
	return b
}

func tail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > n {
		_, _ = f.Seek(-n, io.SeekEnd)
	}
	b, _ := io.ReadAll(io.LimitReader(f, n))
	return strings.TrimSpace(string(b))
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
