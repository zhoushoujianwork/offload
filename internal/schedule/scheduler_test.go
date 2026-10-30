//go:build unix

package schedule

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/proc"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

var t0 = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

// fakeOffload stands in for the offload binary: it records its argv and stdin, then prints a run
// record shaped like `offload -json`. FAKE_* variables are set per test through the environment.
func fakeOffload(t *testing.T) (exe, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	exe = filepath.Join(dir, "offload")
	script := "#!/bin/sh\n" +
		"echo \"$$ $*\" >> '" + calls + "'\n" +
		"cat > '" + calls + ".stdin'\n" +
		"[ -n \"$FAKE_SLEEP\" ] && sleep \"$FAKE_SLEEP\"\n" +
		"if [ -n \"$FAKE_NORECORD\" ]; then echo boom >&2; exit 3; fi\n" +
		"if [ -n \"$FAKE_RECORD\" ]; then printf '%s\\n' \"$FAKE_RECORD\"; else\n" +
		"  printf '%s\\n' '{\"status\":\"ok\",\"exit_code\":0,\"result\":\"REPORT\",\"session\":\"s-1\"}'; fi\n" +
		"exit \"${FAKE_EXIT:-0}\"\n"
	if err := os.WriteFile(exe, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe, calls
}

func newSched(t *testing.T) (*Scheduler, *Store, *clock, string) {
	t.Helper()
	exe, calls := fakeOffload(t)
	clk := &clock{t: t0}
	st := NewStore(t.TempDir())
	return &Scheduler{Store: st, Exe: exe, Now: clk.now, Keep: 5}, st, clk, calls
}

func addTask(t *testing.T, st *Store, now time.Time, spec string, mutate func(*Task)) *Task {
	t.Helper()
	sp, err := NewSpec(KindEvery, spec, "UTC", now)
	if err != nil {
		t.Fatal(err)
	}
	next, _, _ := sp.Next(now)
	task := &Task{ID: NewID(), Prompt: "do the thing", Dir: t.TempDir(), Policy: "default", Schedule: sp, Misfire: MisfireSkip,
		NextRun: &next, CreatedAt: now, UpdatedAt: now}
	if mutate != nil {
		mutate(task)
	}
	if err := st.Save(task); err != nil {
		t.Fatal(err)
	}
	return task
}

func calls(t *testing.T, path string) []string {
	t.Helper()
	b, _ := os.ReadFile(path)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func nCalls(t *testing.T, path string) int {
	if b, _ := os.ReadFile(path); len(b) == 0 {
		return 0
	}
	return len(calls(t, path))
}

func mustLoad(t *testing.T, st *Store, id string) *Task {
	t.Helper()
	task, err := st.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestDueTaskRunsOnceAndRetainsResult(t *testing.T) {
	s, st, clk, callsFile := newSched(t)
	task := addTask(t, st, t0, "1h", func(x *Task) { x.Model = "m-2"; x.Write = true; x.Profile = "cheap"; x.AddDirs = []string{"/abs/lib"} })

	clk.set(t0.Add(30 * time.Minute))
	if err := s.Tick(); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	if n := nCalls(t, callsFile); n != 0 {
		t.Fatalf("not yet due, but %d runs started", n)
	}

	clk.set(t0.Add(time.Hour + 5*time.Second))
	if err := s.Tick(); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	// Another tick for the same occurrence must not run it again.
	if err := s.Tick(); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	if n := nCalls(t, callsFile); n != 1 {
		t.Fatalf("expected exactly one run, got %d", n)
	}
	line := calls(t, callsFile)[0]
	for _, want := range []string{"-json", "-d " + task.Dir, "--execution-policy default", "-p cheap", "-m m-2", "-w", "--add-dir /abs/lib", "--caller-run-id sched:" + task.ID + ":"} {
		if !strings.Contains(line, want) {
			t.Errorf("argv %q missing %q", line, want)
		}
	}
	if strings.Contains(line, "do the thing") {
		t.Error("prompt must travel on stdin, not argv")
	}
	if b, _ := os.ReadFile(callsFile + ".stdin"); string(b) != "do the thing" {
		t.Errorf("stdin = %q", b)
	}

	got := mustLoad(t, st, task.ID)
	if got.Active != nil || got.Last == nil || got.Last.Status != StatusOK || got.ConsecutiveFailures != 0 {
		t.Fatalf("task after run: active=%v last=%+v", got.Active, got.Last)
	}
	if want := t0.Add(2 * time.Hour); got.NextRun == nil || !got.NextRun.Equal(want) {
		t.Errorf("next run = %v, want %v", got.NextRun, want)
	}
	rs, _ := st.Results(task.ID, 0)
	if len(rs) != 1 || rs[0].Report != "REPORT" || rs[0].Session != "s-1" || rs[0].Record == nil || rs[0].ExitCode == nil || *rs[0].ExitCode != 0 {
		t.Fatalf("retained result: %+v", rs)
	}
	if !rs[0].ScheduledFor.Equal(t0.Add(time.Hour)) {
		t.Errorf("scheduled_for = %v", rs[0].ScheduledFor)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(st.runsDir(task.ID), "*.out")); len(leftovers) != 0 {
		t.Errorf("capture files not cleaned up: %v", leftovers)
	}
}

func TestFailuresAreRecordedAndCounted(t *testing.T) {
	cases := []struct {
		name   string
		env    map[string]string
		status string
	}{
		{"worker error", map[string]string{"FAKE_EXIT": "1", "FAKE_RECORD": `{"status":"error","exit_code":1,"result":"it broke","error":"worker failed"}`}, StatusFailed},
		{"timeout", map[string]string{"FAKE_EXIT": "124", "FAKE_RECORD": `{"status":"timeout","exit_code":124,"result":"slow"}`}, StatusTimeout},
		{"no record at all", map[string]string{"FAKE_NORECORD": "1"}, StatusFailed},
		{"disabled inside the child", map[string]string{"FAKE_EXIT": "2", "FAKE_RECORD": `{"status":"error","exit_code":2,"result":"offload is globally disabled","error":"offload is globally disabled"}`}, StatusFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			s, st, clk, _ := newSched(t)
			task := addTask(t, st, t0, "1h", nil)
			clk.set(t0.Add(time.Hour))
			s.Tick()
			s.Wait()
			clk.set(t0.Add(2 * time.Hour))
			s.Tick()
			s.Wait()
			got := mustLoad(t, st, task.ID)
			if got.Last == nil || got.Last.Status != tc.status || got.ConsecutiveFailures != 2 || got.Active != nil {
				t.Fatalf("last=%+v failures=%d active=%v", got.Last, got.ConsecutiveFailures, got.Active)
			}
			if got.Last.Error == "" {
				t.Error("failure has no error text")
			}
			if got.NextRun == nil || !got.NextRun.Equal(t0.Add(3*time.Hour)) {
				t.Errorf("a failure must not retry or stall the schedule; next=%v", got.NextRun)
			}
			rs, _ := st.Results(task.ID, 0)
			if len(rs) != 2 {
				t.Errorf("each failed occurrence keeps a result, got %d", len(rs))
			}
		})
	}
}

func TestFailureCounterResetsOnSuccess(t *testing.T) {
	s, st, clk, _ := newSched(t)
	task := addTask(t, st, t0, "1h", func(x *Task) { x.ConsecutiveFailures = 3 })
	clk.set(t0.Add(time.Hour))
	s.Tick()
	s.Wait()
	if got := mustLoad(t, st, task.ID); got.ConsecutiveFailures != 0 {
		t.Errorf("failures = %d", got.ConsecutiveFailures)
	}
}

func TestMissingExecutableIsAFailedResult(t *testing.T) {
	s, st, clk, _ := newSched(t)
	s.Exe = filepath.Join(t.TempDir(), "nope")
	task := addTask(t, st, t0, "1h", nil)
	clk.set(t0.Add(time.Hour))
	s.Tick()
	s.Wait()
	got := mustLoad(t, st, task.ID)
	if got.Active != nil || got.Last == nil || got.Last.Status != StatusFailed || !strings.Contains(got.Last.Error, "start offload") {
		t.Fatalf("%+v", got.Last)
	}
}

func TestOverlappingInvocationOfOneTaskIsNeverStarted(t *testing.T) {
	t.Setenv("FAKE_SLEEP", "1")
	s, st, clk, callsFile := newSched(t)
	task := addTask(t, st, t0, "1m", nil)
	s.Grace = time.Hour

	clk.set(t0.Add(time.Minute))
	s.Tick()
	// The first run is still going when the next occurrence comes due.
	clk.set(t0.Add(2 * time.Minute))
	s.Tick()
	got := mustLoad(t, st, task.ID)
	if got.Active == nil {
		t.Fatal("first run should still be active")
	}
	if got.Last == nil || got.Last.Status != StatusSkipped || !strings.Contains(got.Last.Reason, "still running") {
		t.Fatalf("overlapping occurrence must be recorded as skipped: %+v", got.Last)
	}
	s.Wait()
	if n := nCalls(t, callsFile); n != 1 {
		t.Fatalf("overlap started %d processes", n)
	}
	final := mustLoad(t, st, task.ID)
	if final.Last.Status != StatusOK {
		t.Errorf("finished run overwrites the skip marker: %+v", final.Last)
	}
	rs, _ := st.Results(task.ID, 0)
	statuses := []string{}
	for _, r := range rs {
		statuses = append(statuses, r.Status)
	}
	if len(rs) != 2 {
		t.Errorf("want one ok and one skipped result, got %v", statuses)
	}
}

func TestSecondSchedulerCannotStartTheSameOccurrence(t *testing.T) {
	t.Setenv("FAKE_SLEEP", "1")
	s1, st, clk, callsFile := newSched(t)
	s2 := &Scheduler{Store: st, Exe: s1.Exe, Now: clk.now}
	addTask(t, st, t0, "1h", nil)
	clk.set(t0.Add(time.Hour))
	var wg sync.WaitGroup
	for _, s := range []*Scheduler{s1, s2} {
		wg.Add(1)
		go func() { defer wg.Done(); s.Tick() }()
	}
	wg.Wait()
	s1.Wait()
	s2.Wait()
	if n := nCalls(t, callsFile); n != 1 {
		t.Fatalf("two schedulers started %d runs for one occurrence", n)
	}
}

func TestMissedOccurrencesSkipPolicyRecordsAndDoesNotRun(t *testing.T) {
	s, st, clk, callsFile := newSched(t)
	task := addTask(t, st, t0, "1h", nil)
	// The machine slept through three occurrences (+1h, +2h, +3h) and wakes at +3h30m.
	clk.set(t0.Add(3*time.Hour + 30*time.Minute))
	s.Tick()
	s.Wait()
	if n := nCalls(t, callsFile); n != 0 {
		t.Fatalf("skip policy ran %d times", n)
	}
	got := mustLoad(t, st, task.ID)
	if got.Last == nil || got.Last.Status != StatusMissed || !strings.Contains(got.Last.Reason, "3 occurrence") {
		t.Fatalf("missed result: %+v", got.Last)
	}
	if !got.NextRun.Equal(t0.Add(4 * time.Hour)) {
		t.Errorf("next run %v: schedule must continue from the next future occurrence", got.NextRun)
	}
	if got.ConsecutiveFailures != 0 {
		t.Error("a missed run is not a failure")
	}
	// Ticking again does not re-record or run anything.
	s.Tick()
	if rs, _ := st.Results(task.ID, 0); len(rs) != 1 {
		t.Errorf("results = %d", len(rs))
	}
}

func TestMissedOccurrencesRunOncePolicyRunsExactlyOnce(t *testing.T) {
	s, st, clk, callsFile := newSched(t)
	task := addTask(t, st, t0, "1h", func(x *Task) { x.Misfire = MisfireRunOnce })
	clk.set(t0.Add(3*time.Hour + 30*time.Minute))
	s.Tick()
	s.Wait()
	s.Tick()
	s.Wait()
	if n := nCalls(t, callsFile); n != 1 {
		t.Fatalf("run-once ran %d times for 3 missed occurrences", n)
	}
	got := mustLoad(t, st, task.ID)
	if got.Last.Status != StatusOK || !strings.Contains(got.Last.Reason, "catch-up run for 3 overdue") {
		t.Fatalf("%+v", got.Last)
	}
	if !got.Last.ScheduledFor.Equal(t0.Add(time.Hour)) || !got.NextRun.Equal(t0.Add(4*time.Hour)) {
		t.Errorf("scheduled_for=%v next=%v", got.Last.ScheduledFor, got.NextRun)
	}
}

func TestLateButWithinGraceStillRuns(t *testing.T) {
	s, st, clk, callsFile := newSched(t)
	addTask(t, st, t0, "1h", nil)
	clk.set(t0.Add(time.Hour + 90*time.Second))
	s.Tick()
	s.Wait()
	if n := nCalls(t, callsFile); n != 1 {
		t.Fatalf("runs = %d", n)
	}
}

func TestOneTimeTaskFiresOnceThenCompletes(t *testing.T) {
	s, st, clk, callsFile := newSched(t)
	at, err := NewSpec(KindAt, "2026-11-01T11:00:00Z", "UTC", t0)
	if err != nil {
		t.Fatal(err)
	}
	next, _, _ := at.Next(t0)
	task := &Task{ID: NewID(), Prompt: "once", Dir: t.TempDir(), Policy: "default", Schedule: at, Misfire: MisfireSkip, NextRun: &next, CreatedAt: t0}
	st.Save(task)
	clk.set(t0.Add(time.Hour))
	s.Tick()
	s.Wait()
	clk.set(t0.Add(5 * time.Hour))
	s.Tick()
	s.Wait()
	if n := nCalls(t, callsFile); n != 1 {
		t.Fatalf("runs = %d", n)
	}
	got := mustLoad(t, st, task.ID)
	if got.NextRun != nil || got.State() != "completed" {
		t.Errorf("state=%s next=%v", got.State(), got.NextRun)
	}
}

func TestPausedTaskNeverRuns(t *testing.T) {
	s, st, clk, callsFile := newSched(t)
	addTask(t, st, t0, "1h", func(x *Task) { x.Paused = true; x.NextRun = nil })
	clk.set(t0.Add(5 * time.Hour))
	s.Tick()
	s.Wait()
	if n := nCalls(t, callsFile); n != 0 {
		t.Fatalf("paused task ran %d times", n)
	}
}

func TestGlobalDisableSkipsAndRecordsWithoutLaunching(t *testing.T) {
	s, st, clk, callsFile := newSched(t)
	disabled := errors.New("offload is globally disabled")
	s.Enabled = func() error { return disabled }
	task := addTask(t, st, t0, "1h", nil)
	clk.set(t0.Add(time.Hour))
	s.Tick()
	s.Wait()
	if n := nCalls(t, callsFile); n != 0 {
		t.Fatalf("a disabled offload launched %d runs", n)
	}
	got := mustLoad(t, st, task.ID)
	if got.Last == nil || got.Last.Status != StatusSkipped || !strings.Contains(got.Last.Reason, "globally disabled") {
		t.Fatalf("%+v", got.Last)
	}
	// Enabling again resumes at the next occurrence; the skipped one is not replayed.
	s.Enabled = nil
	s.Tick()
	s.Wait()
	if n := nCalls(t, callsFile); n != 0 {
		t.Fatalf("skipped occurrence was replayed")
	}
	clk.set(t0.Add(2 * time.Hour))
	s.Tick()
	s.Wait()
	if n := nCalls(t, callsFile); n != 1 {
		t.Fatalf("runs after re-enable = %d", n)
	}
}

func TestRestartKeepsDefinitionAndContinuesSchedule(t *testing.T) {
	s, st, clk, callsFile := newSched(t)
	task := addTask(t, st, t0, "1h", func(x *Task) { x.Name = "nightly"; x.Model = "m-9"; x.Timeout = "20m" })
	clk.set(t0.Add(time.Hour))
	s.Tick()
	s.Wait()

	// A new Store and Scheduler over the same directory is a restarted scheduler.
	st2 := NewStore(st.Root)
	s2 := &Scheduler{Store: st2, Exe: s.Exe, Now: clk.now}
	got, err := st2.Find("nightly")
	if err != nil || got.ID != task.ID || got.Model != "m-9" || got.Timeout != "20m" || got.Prompt != "do the thing" || got.Last == nil {
		t.Fatalf("definition not preserved: %+v %v", got, err)
	}
	clk.set(t0.Add(2 * time.Hour))
	s2.Tick()
	s2.Wait()
	if n := nCalls(t, callsFile); n != 2 {
		t.Fatalf("runs = %d, want 2 (one per scheduler instance)", n)
	}
	if rs, _ := st2.Results(task.ID, 0); len(rs) != 2 {
		t.Errorf("results = %d", len(rs))
	}
}

func TestRestartRecoversRunFinishedWhileSchedulerWasDown(t *testing.T) {
	s, st, clk, callsFile := newSched(t)
	task := addTask(t, st, t0, "1h", nil)
	run := "20261101T110000Z-aaaa"
	task.Active = &Active{RunID: run, ScheduledFor: t0.Add(time.Hour), StartedAt: t0.Add(time.Hour), PID: 999999, PIDStart: "gone"}
	next := t0.Add(2 * time.Hour)
	task.NextRun = &next
	st.Save(task)
	os.MkdirAll(st.runsDir(task.ID), 0o700)
	os.WriteFile(filepath.Join(st.runsDir(task.ID), run+".out"), []byte(`{"status":"ok","exit_code":0,"result":"FROM DISK","session":"s-9"}`+"\n"), 0o600)

	clk.set(t0.Add(time.Hour + 10*time.Minute))
	s.Tick()
	s.Wait()
	got := mustLoad(t, st, task.ID)
	if got.Active != nil || got.Last == nil || got.Last.Status != StatusOK || got.Last.Report != "FROM DISK" {
		t.Fatalf("recovered: active=%v last=%+v", got.Active, got.Last)
	}
	if n := nCalls(t, callsFile); n != 0 {
		t.Fatalf("recovery re-ran the task (%d)", n)
	}
}

func TestRestartMarksVanishedRunLostWithoutRerunning(t *testing.T) {
	s, st, clk, callsFile := newSched(t)
	task := addTask(t, st, t0, "1h", nil)
	task.Active = &Active{RunID: "20261101T110000Z-bbbb", ScheduledFor: t0.Add(time.Hour), StartedAt: t0.Add(time.Hour), PID: 999999, PIDStart: "gone"}
	later := t0.Add(2 * time.Hour)
	task.NextRun = &later
	st.Save(task)
	clk.set(t0.Add(time.Hour + 10*time.Second))
	s.Tick()
	s.Wait()
	got := mustLoad(t, st, task.ID)
	if got.Last == nil || got.Last.Status != StatusLost || got.ConsecutiveFailures != 1 || got.Active != nil {
		t.Fatalf("%+v", got.Last)
	}
	if n := nCalls(t, callsFile); n != 0 {
		t.Fatalf("a lost occurrence must not be re-run, got %d", n)
	}
}

func TestRestartLeavesStillRunningInvocationAlone(t *testing.T) {
	s, st, clk, callsFile := newSched(t)
	task := addTask(t, st, t0, "1m", nil)
	sleeper := exec.Command("sleep", "30")
	proc.Prepare(sleeper)
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sleeper.Process.Kill(); sleeper.Wait() })
	g := proc.Started(sleeper)
	task.Active = &Active{RunID: "20261101T100100Z-cccc", ScheduledFor: t0.Add(time.Minute), StartedAt: t0.Add(time.Minute), PID: g.PID, PIDStart: g.Start}
	st.Save(task)
	s.Grace = time.Hour
	clk.set(t0.Add(3 * time.Minute))
	s.Tick()
	s.Wait()
	got := mustLoad(t, st, task.ID)
	if got.Active == nil || got.Active.RunID != "20261101T100100Z-cccc" {
		t.Fatalf("live invocation was taken over: %+v", got.Active)
	}
	if got.Last == nil || got.Last.Status != StatusSkipped {
		t.Fatalf("new occurrence should be skipped as overlapping: %+v", got.Last)
	}
	if n := nCalls(t, callsFile); n != 0 {
		t.Fatalf("overlap launched %d", n)
	}
}

func TestResultRetentionKeepsNewestAndSeparatesMissed(t *testing.T) {
	st := NewStore(t.TempDir())
	for i := 0; i < 6; i++ {
		id := NewRunID(t0.Add(time.Duration(i) * time.Minute))
		if err := st.SaveResult(&Result{RunID: id, TaskID: "t-x", Status: StatusOK, FinishedAt: t0}, 3); err != nil {
			t.Fatal(err)
		}
	}
	for i := 10; i < 20; i++ {
		id := NewRunID(t0.Add(time.Duration(i) * time.Minute))
		st.SaveResult(&Result{RunID: id, TaskID: "t-x", Status: StatusMissed, FinishedAt: t0}, 3)
	}
	rs, _ := st.Results("t-x", 0)
	ok, missed := 0, 0
	for _, r := range rs {
		if r.Status == StatusOK {
			ok++
		} else {
			missed++
		}
	}
	if ok != 3 || missed != 3 {
		t.Errorf("kept ok=%d missed=%d, want 3 and 3 (a long outage must not evict real results)", ok, missed)
	}
}

func TestBuildArgsKeepsCredentialsOut(t *testing.T) {
	task := &Task{ID: "t-1", Dir: "/w", Policy: "managed-development", Write: true, Profile: "p", Sandbox: "workspace-write", AllowedTools: "Read", NoSalvage: true}
	args := strings.Join(BuildArgs(task, "r1"), " ")
	for _, want := range []string{"--execution-policy managed-development", "-w", "-p p", "--codex-sandbox workspace-write", "--allowed-tools Read", "--no-salvage", "sched:t-1:r1"} {
		if !strings.Contains(args, want) {
			t.Errorf("%q missing %q", args, want)
		}
	}
	for _, f := range []string{"token", "Token", "secret"} {
		if strings.Contains(args, f) {
			t.Errorf("argv mentions %q", f)
		}
	}
}

func TestOnlyOneSchedulerHoldsTheLock(t *testing.T) {
	st := NewStore(t.TempDir())
	release, _, busy, err := st.LockScheduler()
	if err != nil || busy {
		t.Fatal(err, busy)
	}
	if running, pid := st.SchedulerStatus(); !running || pid != os.Getpid() {
		t.Errorf("status = %v %d", running, pid)
	}
	release()
	if running, _ := st.SchedulerStatus(); running {
		t.Error("still reported running after release")
	}
}
