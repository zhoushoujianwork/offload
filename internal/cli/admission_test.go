package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/live"
	"github.com/zhoushoujianwork/offload/internal/proc"
	"github.com/zhoushoujianwork/offload/internal/worker"
)

func needUnix(t *testing.T) {
	t.Helper()
	if !proc.Supported || runtime.GOOS == "windows" {
		t.Skip("needs process groups")
	}
}

// offloadBin builds the real binary once per test, so separate OS processes can race for slots.
func offloadBin(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "offload")
	out, err := exec.Command("go", "build", "-o", bin, "github.com/zhoushoujianwork/offload").CombinedOutput()
	if err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return bin
}

type child struct {
	cmd      *exec.Cmd
	out, err bytes.Buffer
}

func start(t *testing.T, bin string, env []string, args ...string) *child {
	t.Helper()
	c := &child{cmd: exec.Command(bin, args...)}
	// A configured proxy makes the gateway preflight skip its direct dial, which a subprocess cannot stub.
	c.cmd.Env = append(append(os.Environ(), "HTTPS_PROXY=http://127.0.0.1:9"), env...)
	c.cmd.Stdout, c.cmd.Stderr = &c.out, &c.err
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.cmd.Process.Kill() })
	return c
}

func (c *child) wait(t *testing.T) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		c.cmd.Process.Kill()
		t.Fatalf("offload did not exit\nstderr: %s", c.err.String())
	}
	return c.cmd.ProcessState.ExitCode()
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func pidFrom(path string) int {
	b, _ := os.ReadFile(path)
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

func phases(t *testing.T) map[string]int {
	t.Helper()
	m := map[string]int{}
	states, _ := live.List()
	for _, s := range states {
		m[s.Phase]++
	}
	return m
}

// The configured limit holds across separate CLI processes: excess runs are visibly queued, and
// all of them finish.
func TestConcurrentInvocationsNeverExceedLimit(t *testing.T) {
	needUnix(t)
	work, _ := setup(t, okReply)
	bin := offloadBin(t)
	mark := filepath.Join(t.TempDir(), "starts")
	env := []string{"OFFLOAD_MAX_WORKERS=2", "FAKE_SLEEP=1", "FAKE_MARK=" + mark}
	var runs []*child
	for i := 0; i < 5; i++ {
		runs = append(runs, start(t, bin, env, "-d", work, "task"+strconv.Itoa(i)))
	}
	waitFor(t, "two running and the rest queued", 20*time.Second, func() bool {
		p := phases(t)
		if p[live.Running] > 2 {
			t.Fatalf("more than 2 workers running: %v", p)
		}
		return p[live.Running] == 2 && p[live.Queued] == 3
	})
	_, out, _ := runCLI("status")
	if !strings.Contains(out, "queued") || !strings.Contains(out, "wait_reason=") || !strings.Contains(out, "max_workers=2") {
		t.Errorf("status does not show the queue and limits:\n%s", out)
	}
	for _, r := range runs {
		if p := phases(t)[live.Running]; p > 2 {
			t.Fatalf("more than 2 workers running: %d", p)
		}
		if code := r.wait(t); code != 0 {
			t.Fatalf("code=%d\n%s", code, r.err.String())
		}
	}
	b, _ := os.ReadFile(mark)
	if n := len(strings.Fields(string(b))) / 2; n != 5 {
		t.Fatalf("expected 5 backend starts, got %d", n)
	}
	waited := 0
	for _, r := range runs {
		if strings.Contains(r.err.String(), "wait=") && !strings.Contains(r.err.String(), "wait=0.0") {
			waited++
		}
	}
	if waited < 3 {
		t.Errorf("queued runs should report wait time separately; only %d did", waited)
	}
}

// A queued run that is cancelled never launches a backend, and says so.
func TestCancelledQueuedRunNeverLaunchesBackend(t *testing.T) {
	needUnix(t)
	work, record := setup(t, okReply)
	bin := offloadBin(t)
	env := []string{"OFFLOAD_MAX_WORKERS=1"}
	first := start(t, bin, append(env, "FAKE_SLEEP=20"), "-d", work, "holder")
	waitFor(t, "holder running", 10*time.Second, func() bool { return phases(t)[live.Running] == 1 })
	os.Remove(record)

	queued := start(t, bin, env, "-json", "-d", work, "queued")
	waitFor(t, "queued", 10*time.Second, func() bool { return phases(t)[live.Queued] == 1 })
	queued.cmd.Process.Signal(syscall.SIGTERM)
	if code := queued.wait(t); code != 2 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(queued.out.String(), "cancelled while waiting for a worker slot") {
		t.Errorf("output: %s", queued.out.String())
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("a cancelled queued run started a backend")
	}
	first.cmd.Process.Signal(syscall.SIGTERM)
	first.wait(t)
}

// Queue wait must not count against the worker's -t timeout.
func TestQueueWaitDoesNotConsumeWorkerTimeout(t *testing.T) {
	needUnix(t)
	work, _ := setup(t, okReply)
	bin := offloadBin(t)
	env := []string{"OFFLOAD_MAX_WORKERS=1"}
	holder := start(t, bin, append(env, "FAKE_SLEEP=3"), "-d", work, "holder")
	waitFor(t, "holder running", 10*time.Second, func() bool { return phases(t)[live.Running] == 1 })
	second := start(t, bin, env, "-json", "-d", work, "-t", "2s", "second")
	if code := second.wait(t); code != 0 {
		t.Fatalf("a 2s timeout was spent while queued: code=%d out=%s", code, second.out.String())
	}
	holder.wait(t)
	rec := decodeRun(t, second.out.String())
	if rec.WaitMs == nil || rec.ExecMs == nil || *rec.WaitMs < 1000 || *rec.ExecMs >= 2000 || *rec.ElapsedMs < *rec.WaitMs {
		t.Fatalf("wait/exec/elapsed: %+v", rec)
	}
	if !strings.Contains(second.err.String(), " wait=") || !strings.Contains(second.err.String(), " exec=") {
		t.Errorf("summary line: %s", second.err.String())
	}
}

// Two simultaneous resumes of one session cannot both launch.
func TestSimultaneousResumesCannotBothLaunch(t *testing.T) {
	needUnix(t)
	work, _ := setup(t, okReply)
	if code, _, e := runCLI("-d", work, "first"); code != 0 {
		t.Fatal(e)
	}
	bin := offloadBin(t)
	mark := filepath.Join(t.TempDir(), "starts")
	env := []string{"OFFLOAD_MAX_WORKERS=-1", "FAKE_SLEEP=2", "FAKE_MARK=" + mark}
	var runs []*child
	for i := 0; i < 4; i++ {
		runs = append(runs, start(t, bin, env, "-r", "s-1", "-d", work, "resume"))
	}
	ok, busy := 0, 0
	for _, r := range runs {
		switch r.wait(t) {
		case 0:
			ok++
		case 2:
			if strings.Contains(r.out.String()+r.err.String(), "already has an active run") {
				busy++
			}
		}
	}
	b, _ := os.ReadFile(mark)
	if ok != 1 || busy != 3 || len(strings.Fields(string(b)))/2 != 1 {
		t.Fatalf("ok=%d busy=%d starts=%q", ok, busy, b)
	}
}

// A worker's child (a build or test) is stopped on SIGTERM, partial progress and the resume
// hint survive, and an unrelated process of the same executable name is left alone.
func TestSIGTERMStopsWorkerChildrenAndKeepsProgress(t *testing.T) {
	needUnix(t)
	work, _ := setup(t, okReply)
	bin := offloadBin(t)
	childPID := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("FAKE_PREFIX", `{"type":"system","subtype":"init","session_id":"s-term"}`)
	bystander := exec.Command("sleep", "300")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bystander.Process.Kill(); bystander.Wait() })

	r := start(t, bin, []string{"FAKE_SLEEP=60", "FAKE_CHILD=" + childPID, "FAKE_PREFIX=" + os.Getenv("FAKE_PREFIX")},
		"-json", "-no-salvage", "-d", work, "task")
	waitFor(t, "worker child", 10*time.Second, func() bool { return pidFrom(childPID) > 0 })
	cpid := pidFrom(childPID)
	waitFor(t, "session published", 10*time.Second, func() bool {
		for _, s := range func() []live.State { l, _ := live.List(); return l }() {
			if s.Session == "s-term" {
				return true
			}
		}
		return false
	})
	r.cmd.Process.Signal(syscall.SIGTERM)
	code := r.wait(t)
	rec := decodeRun(t, r.out.String())
	if code != 1 || rec.Session != "s-term" || !strings.Contains(rec.Error, "offload -r s-term") || rec.Cleanup != "" {
		t.Fatalf("code=%d rec=%+v", code, rec)
	}
	if syscall.Kill(cpid, 0) == nil {
		var zombie bool
		if out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(cpid)).Output(); strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			zombie = true
		}
		if !zombie {
			t.Fatalf("worker child %d survived SIGTERM", cpid)
		}
	}
	if bystander.Process.Signal(syscall.Signal(0)) != nil {
		t.Fatal("an unrelated process was killed")
	}
}

// Timeout and idle limit stop the worker's children too, and still report progress.
func TestTimeoutAndIdleStopWorkerChildren(t *testing.T) {
	needUnix(t)
	for _, tc := range []string{"timeout", "idle"} {
		t.Run(tc, func(t *testing.T) {
			work, _ := setup(t, okReply)
			childPID := filepath.Join(t.TempDir(), "child.pid")
			t.Setenv("FAKE_SLEEP", "60")
			t.Setenv("FAKE_CHILD", childPID)
			t.Setenv("FAKE_PREFIX", `{"type":"system","subtype":"init","session_id":"s-x"}`)
			timeout := "1500ms"
			if tc == "idle" {
				orig := worker.IdleLimit
				worker.IdleLimit = time.Second
				t.Cleanup(func() { worker.IdleLimit = orig })
				timeout = "30s"
			}
			code, out, _ := runCLI("-json", "-no-salvage", "-d", work, "-t", timeout, "task")
			rec := decodeRun(t, out)
			if code != 124 || rec.Status != "timeout" || rec.Session != "s-x" || rec.Cleanup != "" || !strings.Contains(rec.Error, "offload -r s-x") {
				t.Fatalf("code=%d rec=%+v", code, rec)
			}
			cpid := pidFrom(childPID)
			if cpid == 0 {
				t.Fatal("fixture child never started")
			}
			waitFor(t, "child gone", 5*time.Second, func() bool {
				if syscall.Kill(cpid, 0) != nil {
					return true
				}
				o, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(cpid)).Output()
				return strings.HasPrefix(strings.TrimSpace(string(o)), "Z") || len(strings.TrimSpace(string(o))) == 0
			})
		})
	}
}

// A normal exit that leaves a background child behind still has it stopped, and the run is not
// reported clean until it is gone.
func TestChildLeftBehindByNormalCompletionIsStopped(t *testing.T) {
	needUnix(t)
	work, _ := setup(t, okReply)
	childPID := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("FAKE_CHILD", childPID)
	code, out, _ := runCLI("-json", "-d", work, "task")
	rec := decodeRun(t, out)
	cpid := pidFrom(childPID)
	if code != 0 || rec.Cleanup != "" || cpid == 0 {
		t.Fatalf("code=%d cpid=%d rec=%+v", code, cpid, rec)
	}
	waitFor(t, "leftover child stopped", 5*time.Second, func() bool {
		o, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(cpid)).Output()
		s := strings.TrimSpace(string(o))
		return s == "" || strings.HasPrefix(s, "Z")
	})
}

// A descendant that escapes the group with setsid cannot be reached by group signals. That is a
// documented limit (README, Known limits): offload neither sees nor kills it, and never reaches for
// processes outside the group it created.
func TestDetachedDescendantIsOutsideTheOwnedGroup(t *testing.T) {
	needUnix(t)
	if _, err := exec.LookPath("perl"); err != nil {
		t.Skip("needs perl for setsid")
	}
	work, _ := setup(t, okReply)
	childPID := filepath.Join(t.TempDir(), "detached.pid")
	fake := os.Getenv("OFFLOAD_CLAUDE")
	b, _ := os.ReadFile(fake)
	script := strings.Replace(string(b), "cat <<'EOF'",
		"perl -MPOSIX -e 'POSIX::setsid(); exec \"sleep\",\"300\"' >/dev/null 2>&1 &\necho $! > '"+childPID+"'\nsleep 0.5\ncat <<'EOF'", 1)
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI("-json", "-d", work, "task")
	cpid := pidFrom(childPID)
	if cpid > 0 {
		t.Cleanup(func() { syscall.Kill(cpid, syscall.SIGKILL) })
	}
	if cpid == 0 {
		t.Fatal("detached fixture never started")
	}
	rec := decodeRun(t, out)
	if code != 0 || rec.Status != "ok" {
		t.Fatalf("code=%d rec=%+v", code, rec)
	}
	if syscall.Kill(cpid, 0) != nil {
		t.Fatalf("detached process %d was signalled although it is outside the owned group", cpid)
	}
}

// If offload itself is killed, the lock frees but the worker's processes may live on. The state
// must say LOST with processes still running (not "finished"), and the slot must stay unavailable
// until they are gone.
func TestOwnerCrashKeepsSlotUnavailableWhileWorkerProcessesLive(t *testing.T) {
	needUnix(t)
	work, _ := setup(t, okReply)
	bin := offloadBin(t)
	origStale := live.StaleAfter
	live.StaleAfter = 500 * time.Millisecond
	t.Cleanup(func() { live.StaleAfter = origStale })
	env := []string{"OFFLOAD_MAX_WORKERS=1"}

	crashed := start(t, bin, append(env, "FAKE_SLEEP=60"), "-d", work, "doomed")
	var st live.State
	waitFor(t, "worker group published", 10*time.Second, func() bool {
		l, _ := live.List()
		for _, s := range l {
			if s.PID == crashed.cmd.Process.Pid && s.PGID > 0 && s.Phase == live.Running {
				st = s
				return true
			}
		}
		return false
	})
	g, _ := st.Group()
	t.Cleanup(g.Kill)
	crashed.cmd.Process.Signal(syscall.SIGKILL)
	crashed.cmd.Wait()
	if !g.Alive() {
		t.Fatal("fixture worker should outlive its killed owner")
	}

	waitFor(t, "state reported LOST with processes alive", 10*time.Second, func() bool {
		l, _ := live.List()
		for _, s := range l {
			if s.PID == crashed.cmd.Process.Pid {
				return s.Phase == live.Lost && s.Unresolved
			}
		}
		return false
	})
	_, out, _ := runCLI("status")
	if !strings.Contains(out, "worker processes alive") {
		t.Errorf("status hides the surviving processes:\n%s", out)
	}

	next := start(t, bin, append(env, "FAKE_SLEEP=0"), "-json", "-d", work, "next")
	waitFor(t, "next run queued behind the unresolved slot", 10*time.Second, func() bool {
		l, _ := live.List()
		for _, s := range l {
			if s.PID == next.cmd.Process.Pid {
				return s.Phase == live.Queued && strings.Contains(s.WaitReason, "unresolved")
			}
		}
		return false
	})
	time.Sleep(600 * time.Millisecond)
	if phases(t)[live.Running] != 0 {
		t.Fatal("capacity was reused while the crashed owner's worker was still alive")
	}
	g.Kill()
	if code := next.wait(t); code != 0 {
		t.Fatalf("next run did not proceed once the processes were gone: %d\n%s", code, next.err.String())
	}
}

func setConfigLimits(t *testing.T, limits string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "offload", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := strings.Replace(string(b), `{"default"`, `{"limits":`+limits+`,"default"`, 1)
	if err := os.WriteFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "offload", "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Build defaults reach the Claude worker, and an explicit caller setting is preserved.
func TestBuildDefaultsReachClaudeWorkerAndKeepExplicitSettings(t *testing.T) {
	work, record := setup(t, okReply)
	t.Setenv("GOMAXPROCS", "")
	os.Unsetenv("GOMAXPROCS")
	t.Setenv("GOFLAGS", "-mod=vendor")
	if code, _, e := runCLI("-d", work, "task"); code != 0 {
		t.Fatal(e)
	}
	got, _ := os.ReadFile(record)
	for _, want := range []string{"gomaxprocs=2", "goflags=-mod=vendor -p=1"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}

	t.Setenv("GOMAXPROCS", "7")
	t.Setenv("GOFLAGS", "-p=3 -race")
	if code, _, e := runCLI("-d", work, "task"); code != 0 {
		t.Fatal(e)
	}
	got, _ = os.ReadFile(record)
	for _, want := range []string{"gomaxprocs=7", "goflags=-p=3 -race"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("explicit setting overridden; missing %q in %s", want, got)
		}
	}
}

func TestConfiguredLimitsAndCodexEnvironment(t *testing.T) {
	work, record := setupCodex(t, codexReply)
	setConfigLimits(t, `{"go_max_procs":3,"go_parallelism":2}`)
	os.Unsetenv("GOMAXPROCS")
	os.Unsetenv("GOFLAGS")
	fake := os.Getenv("OFFLOAD_CODEX")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + record + "'\necho \"gomaxprocs=$GOMAXPROCS goflags=$GOFLAGS\" > '" + record + ".env'\ncat > /dev/null\ncat <<'EOF'\n" + codexReply + "\nEOF\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, e := runCLI("-p", "codex", "-d", work, "task"); code != 0 {
		t.Fatal(e)
	}
	got, _ := os.ReadFile(record + ".env")
	if !strings.Contains(string(got), "gomaxprocs=3 goflags=-p=2") {
		t.Errorf("configured limits did not reach the Codex worker: %s", got)
	}
}

func TestInvalidWorkerLimitIsAUsageError(t *testing.T) {
	work, record := setup(t, okReply)
	t.Setenv("OFFLOAD_MAX_WORKERS", "0")
	os.Remove(record)
	code, out, errOut := runCLI("-d", work, "task")
	if code != 2 || !strings.Contains(out+errOut, "OFFLOAD_MAX_WORKERS") {
		t.Fatalf("code=%d %q %q", code, out, errOut)
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("worker started despite an invalid limit")
	}
}
