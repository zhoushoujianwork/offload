package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/live"
)

func runJSON(t *testing.T, v any, args ...string) {
	t.Helper()
	code, out, errOut := runCLI(args...)
	if code != 0 {
		t.Fatalf("offload %v: code=%d out=%q err=%q", args, code, out, errOut)
	}
	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("offload %v: not JSON: %v\n%s", args, err, out)
	}
}

type taskJSON struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	State       string  `json:"state"`
	Prompt      string  `json:"prompt"`
	Dir         string  `json:"dir"`
	Profile     string  `json:"profile"`
	Model       string  `json:"model"`
	Write       bool    `json:"write"`
	Policy      string  `json:"execution_policy"`
	Misfire     string  `json:"misfire"`
	NextRun     *string `json:"next_run"`
	NextLocal   string  `json:"next_run_local"`
	Description string  `json:"schedule_description"`
	Schedule    struct {
		Kind     string `json:"kind"`
		Expr     string `json:"expr"`
		Timezone string `json:"timezone"`
	} `json:"schedule"`
}

func TestScheduleLifecycleCRUD(t *testing.T) {
	work, _ := setup(t, okReply)
	var created taskJSON
	runJSON(t, &created, "schedule", "add", "--json", "--name", "nightly", "--cron", "30 9 * * *", "--tz", "Asia/Shanghai",
		"-d", work, "-p", "cheap", "-m", "m-2", "-w", "--misfire", "run-once", "review the logs")
	if created.ID == "" || created.State != "scheduled" || created.Prompt != "review the logs" || created.Dir != work || created.Profile != "cheap" ||
		created.Model != "m-2" || !created.Write || created.Policy != "default" || created.Misfire != "run-once" {
		t.Fatalf("created: %+v", created)
	}
	if created.Schedule.Kind != "cron" || created.Schedule.Timezone != "Asia/Shanghai" || created.NextRun == nil {
		t.Fatalf("schedule: %+v", created)
	}
	next, err := time.Parse(time.RFC3339, created.NextLocal)
	if err != nil || next.Format("15:04") != "09:30" {
		t.Fatalf("next run must be visible in the task timezone: local=%q err=%v", created.NextLocal, err)
	}
	if _, off := next.Zone(); off != 8*3600 {
		t.Errorf("next_run_local offset = %d, want +08:00", off)
	}

	// Text output shows the next run and a hint that nothing runs without a scheduler.
	code, out, _ := runCLI("schedule", "show", "nightly")
	if code != 0 || !strings.Contains(out, "next run:") || !strings.Contains(out, "CST") || !strings.Contains(out, "state:     scheduled") {
		t.Fatalf("show: %d\n%s", code, out)
	}
	if code, out, _ = runCLI("schedule", "list"); code != 0 || !strings.Contains(out, created.ID) || !strings.Contains(out, "no scheduler is running") {
		t.Fatalf("list: %d\n%s", code, out)
	}

	var list struct {
		SchemaVersion int        `json:"schema_version"`
		Tasks         []taskJSON `json:"tasks"`
		Scheduler     struct {
			Running bool `json:"running"`
		} `json:"scheduler"`
	}
	runJSON(t, &list, "schedule", "list", "--json")
	if list.SchemaVersion != 1 || len(list.Tasks) != 1 || list.Scheduler.Running {
		t.Fatalf("list json: %+v", list)
	}

	// Update: new prompt, new timezone-bound schedule, and a changed option; id by prefix.
	var updated taskJSON
	runJSON(t, &updated, "schedule", "update", created.ID[:5], "--json", "--cron", "0 8 * * 1-5", "--tz", "UTC", "-m", "m-3", "new prompt")
	if updated.Prompt != "new prompt" || updated.Model != "m-3" || updated.Schedule.Expr != "0 8 * * 1-5" || updated.Schedule.Timezone != "UTC" ||
		updated.Profile != "cheap" || !updated.Write || updated.Misfire != "run-once" {
		t.Fatalf("update must change named fields and keep the rest: %+v", updated)
	}
	if updated.NextRun == nil || *updated.NextRun == *created.NextRun {
		t.Errorf("next run not recomputed: %v -> %v", created.NextRun, updated.NextRun)
	}

	// Changing only the timezone re-reads the same cron fields in the new zone.
	var moved taskJSON
	runJSON(t, &moved, "schedule", "update", "nightly", "--json", "--tz", "Asia/Tokyo")
	if moved.Schedule.Expr != "0 8 * * 1-5" || moved.Schedule.Timezone != "Asia/Tokyo" || !strings.HasSuffix(moved.NextLocal, "+09:00") {
		t.Fatalf("tz-only update: %+v", moved)
	}
	if code, _, _ := runCLI("schedule", "update", "nightly", "--tz", "Nowhere/City"); code == 0 {
		t.Error("bad timezone accepted by update")
	}

	// Pause clears the next run; resume computes the next occurrence from now.
	var paused taskJSON
	runJSON(t, &paused, "schedule", "pause", "nightly", "--json")
	if paused.State != "paused" || paused.NextRun != nil {
		t.Fatalf("paused: %+v", paused)
	}
	if code, out, _ = runCLI("schedule", "show", "nightly"); !strings.Contains(out, "(paused)") {
		t.Errorf("paused show:\n%s", out)
	}
	var resumed taskJSON
	runJSON(t, &resumed, "schedule", "resume", "nightly", "--json")
	if resumed.State != "scheduled" || resumed.NextRun == nil {
		t.Fatalf("resumed: %+v", resumed)
	}

	// Delete removes it for good.
	if code, out, errOut := runCLI("schedule", "delete", "nightly"); code != 0 || !strings.Contains(out, "deleted") {
		t.Fatalf("delete: %d %s %s", code, out, errOut)
	}
	if code, _, errOut := runCLI("schedule", "show", "nightly"); code != 2 || !strings.Contains(errOut, "no such scheduled task") {
		t.Errorf("show after delete: %d %s", code, errOut)
	}
	runJSON(t, &list, "schedule", "list", "--json")
	if len(list.Tasks) != 0 {
		t.Errorf("tasks after delete: %+v", list.Tasks)
	}
}

func TestScheduleAddRejectsBadInput(t *testing.T) {
	work, _ := setup(t, okReply)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	cases := map[string][]string{
		"no schedule":            {"schedule", "add", "-d", work, "task"},
		"two schedules":          {"schedule", "add", "--every", "1h", "--cron", "* * * * *", "--tz", "UTC", "-d", work, "task"},
		"bad cron":               {"schedule", "add", "--cron", "61 * * * *", "--tz", "UTC", "-d", work, "task"},
		"unknown tz":             {"schedule", "add", "--cron", "* * * * *", "--tz", "Nowhere/City", "-d", work, "task"},
		"local tz":               {"schedule", "add", "--cron", "* * * * *", "--tz", "Local", "-d", work, "task"},
		"interval too short":     {"schedule", "add", "--every", "10s", "-d", work, "--tz", "UTC", "task"},
		"past time":              {"schedule", "add", "--at", "2020-01-01T00:00:00Z", "-d", work, "task"},
		"no prompt":              {"schedule", "add", "--at", future, "-d", work},
		"missing dir":            {"schedule", "add", "--at", future, "-d", filepath.Join(work, "nope"), "task"},
		"unknown profile":        {"schedule", "add", "--at", future, "-d", work, "-p", "ghost", "task"},
		"managed needs write":    {"schedule", "add", "--at", future, "-d", work, "--execution-policy", "managed-development", "task"},
		"bad misfire":            {"schedule", "add", "--at", future, "-d", work, "--misfire", "always", "task"},
		"bad policy":             {"schedule", "add", "--at", future, "-d", work, "--execution-policy", "yolo", "task"},
		"sandbox without policy": {"schedule", "add", "--at", future, "-d", work, "--codex-sandbox", "workspace-write", "task"},
		"missing add-dir":        {"schedule", "add", "--at", future, "-d", work, "--add-dir", filepath.Join(work, "nope"), "task"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if code, out, errOut := runCLI(args...); code == 0 {
				t.Fatalf("accepted: %s %s", out, errOut)
			}
		})
	}
	var list struct {
		Tasks []taskJSON `json:"tasks"`
	}
	runJSON(t, &list, "schedule", "list", "--json")
	if len(list.Tasks) != 0 {
		t.Errorf("a rejected add left %d task(s) behind", len(list.Tasks))
	}
}

func TestScheduleNamesAreUnique(t *testing.T) {
	work, _ := setup(t, okReply)
	args := []string{"schedule", "add", "--name", "daily", "--every", "1h", "--tz", "UTC", "-d", work, "task"}
	if code, _, errOut := runCLI(args...); code != 0 {
		t.Fatal(errOut)
	}
	if code, _, errOut := runCLI(args...); code == 0 || !strings.Contains(errOut, "already used") {
		t.Errorf("duplicate name accepted: %d %s", code, errOut)
	}
}

func TestSchedulePromptFromStdinAndStoredCredentialFree(t *testing.T) {
	work, _ := setup(t, okReply)
	var out, errb bytes.Buffer
	code := Run("test", []string{"schedule", "add", "--json", "--every", "1h", "--tz", "UTC", "-d", work, "-p", "cheap"}, strings.NewReader("prompt from stdin\n"), &out, &errb)
	if code != 0 {
		t.Fatalf("%d %s", code, errb.String())
	}
	var task taskJSON
	if err := json.Unmarshal(out.Bytes(), &task); err != nil || task.Prompt != "prompt from stdin" {
		t.Fatalf("%v %+v", err, task)
	}
	// The stored definition names the profile but never holds its token or token command.
	var stored string
	filepath.Walk(filepath.Join(filepath.Dir(config.Dir()), "..", "state", "offload", "schedules"), func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			stored += string(b)
		}
		return nil
	})
	if stored == "" {
		t.Fatal("no stored schedule files found")
	}
	if strings.Contains(stored, "tok-123") || strings.Contains(stored, "token_cmd") || strings.Contains(stored, "echo tok") {
		t.Errorf("credential material in stored schedule:\n%s", stored)
	}
	if !strings.Contains(stored, `"profile": "cheap"`) {
		t.Errorf("profile name not stored:\n%s", stored)
	}
}

// The scheduler launches a real offload process through the normal worker lifecycle: the fake
// Claude sees the saved options and the result is retained and readable as JSON.
func TestScheduledExecutionEndToEnd(t *testing.T) {
	needUnix(t)
	work, record := setup(t, okReply)
	bin := offloadBin(t)
	at := time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339)
	var task taskJSON
	runJSON(t, &task, "schedule", "add", "--json", "--name", "e2e", "--at", at, "-d", work, "-p", "cheap", "-w", "scheduled prompt text")

	// Before the time arrives, a scheduler pass starts nothing.
	early := start(t, bin, nil, "schedule", "start", "--once")
	if code := early.wait(t); code != 0 {
		t.Fatalf("early pass: %d %s", code, early.err.String())
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("task ran before its time")
	}

	time.Sleep(3500 * time.Millisecond)
	sched := start(t, bin, nil, "schedule", "start", "--once")
	if code := sched.wait(t); code != 0 {
		t.Fatalf("scheduler: %d\n%s", code, sched.err.String())
	}
	var res struct {
		TaskID  string `json:"task_id"`
		Results []struct {
			RunID  string `json:"run_id"`
			Status string `json:"status"`
			Report string `json:"report"`
			Record struct {
				Session         string `json:"session"`
				CallerRunID     string `json:"caller_run_id"`
				ExecutionPolicy string `json:"execution_policy"`
			} `json:"record"`
		} `json:"results"`
	}
	runJSON(t, &res, "schedule", "results", "e2e", "--json")
	if len(res.Results) != 1 || res.Results[0].Status != "ok" || res.Results[0].Report != "REPORT" || res.Results[0].Record.Session != "s-1" {
		t.Fatalf("results: %+v", res)
	}
	if got := res.Results[0].Record.CallerRunID; !strings.HasPrefix(got, "sched:"+task.ID+":") {
		t.Errorf("caller_run_id = %q", got)
	}
	argv, _ := os.ReadFile(record)
	if !strings.Contains(string(argv), "scheduled prompt text") {
		t.Errorf("worker did not receive the saved prompt:\n%s", argv)
	}
	if !strings.Contains(string(argv), "token=tok-123") {
		t.Errorf("worker did not get the profile's credentials through the normal mechanism:\n%s", argv)
	}

	// Human-readable retrieval and the task's last-run state.
	if code, out, _ := runCLI("schedule", "results", "e2e", "--latest"); code != 0 || !strings.Contains(out, "ok") || !strings.Contains(out, "REPORT") {
		t.Errorf("results --latest: %d\n%s", code, out)
	}
	var after taskJSON
	runJSON(t, &after, "schedule", "show", "e2e", "--json")
	if after.State != "completed" || after.NextRun != nil {
		t.Errorf("a one-time task is completed after its run: %+v", after)
	}
	if _, out, _ := runCLI("schedule", "show", "e2e"); !strings.Contains(out, "last run:  ok") {
		t.Errorf("show does not report the last run:\n%s", out)
	}
	// The scheduled run also appears in the ordinary run history.
	if code, out, _ := runCLI("ls"); code != 0 || !strings.Contains(out, "scheduled prompt text") {
		t.Errorf("run not in history:\n%s", out)
	}
}

func TestScheduledRunsRespectGlobalDisable(t *testing.T) {
	needUnix(t)
	work, record := setup(t, okReply)
	bin := offloadBin(t)
	at := time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339)
	var task taskJSON
	runJSON(t, &task, "schedule", "add", "--json", "--name", "off", "--at", at, "-d", work, "task")
	if code, _, errOut := runCLI("disable"); code != 0 {
		t.Fatal(errOut)
	}
	time.Sleep(2500 * time.Millisecond)
	if code := start(t, bin, nil, "schedule", "start", "--once").wait(t); code != 0 {
		t.Fatalf("scheduler code %d", code)
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("a disabled offload launched a scheduled worker")
	}
	var res struct {
		Results []struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"results"`
	}
	runJSON(t, &res, "schedule", "results", "off", "--json")
	if len(res.Results) != 1 || res.Results[0].Status != "skipped" || !strings.Contains(res.Results[0].Reason, "globally disabled") {
		t.Fatalf("%+v", res)
	}
}

func TestSchedulerRefusesSecondInstanceAndReportsStatus(t *testing.T) {
	needUnix(t)
	setup(t, okReply)
	bin := offloadBin(t)
	first := start(t, bin, nil, "schedule", "start", "--interval", "1s")
	waitFor(t, "scheduler to report running", 10*time.Second, func() bool {
		_, out, _ := runCLI("schedule", "status")
		return strings.Contains(out, "scheduler running")
	})
	var st struct {
		Scheduler struct {
			Running bool `json:"running"`
			PID     int  `json:"pid"`
		} `json:"scheduler"`
	}
	runJSON(t, &st, "schedule", "status", "--json")
	if !st.Scheduler.Running || st.Scheduler.PID != first.cmd.Process.Pid {
		t.Errorf("status: %+v (want pid %d)", st, first.cmd.Process.Pid)
	}
	second := start(t, bin, nil, "schedule", "start", "--once")
	if code := second.wait(t); code != 2 || !strings.Contains(second.err.String(), "already running") {
		t.Errorf("second scheduler: %d %s", code, second.err.String())
	}
}

func TestScheduleHelpDocumentsSyntax(t *testing.T) {
	setup(t, okReply)
	_, _, errOut := runCLI("schedule", "add", "-h")
	for _, want := range []string{"-cron", "-every", "-at", "-tz", "-misfire", "day-of-week", "IANA", "--cron"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("add -h missing %q:\n%s", want, errOut)
		}
	}
	if code, out, _ := runCLI("schedule", "help"); code != 0 || !strings.Contains(out, "results") || !strings.Contains(out, "pause|resume") {
		t.Errorf("schedule help: %d %s", code, out)
	}
}

// Scheduled runs are ordinary offload processes, so the machine-wide worker limit queues them.
func TestScheduledRunsShareTheWorkerLimit(t *testing.T) {
	needUnix(t)
	work, _ := setup(t, okReply)
	bin := offloadBin(t)
	mark := filepath.Join(t.TempDir(), "starts")
	at := time.Now().Add(2 * time.Second).UTC().Format(time.RFC3339)
	for _, name := range []string{"a", "b"} {
		runJSON(t, &taskJSON{}, "schedule", "add", "--json", "--name", name, "--at", at, "-d", work, "task "+name)
	}
	time.Sleep(2500 * time.Millisecond)
	sched := start(t, bin, []string{"OFFLOAD_MAX_WORKERS=1", "FAKE_SLEEP=1", "FAKE_MARK=" + mark}, "schedule", "start", "--once")
	waitFor(t, "one running and one queued", 20*time.Second, func() bool {
		p := phases(t)
		if p[live.Running] > 1 {
			t.Fatalf("limit exceeded: %v", p)
		}
		return p[live.Running] == 1 && p[live.Queued] == 1
	})
	if code := sched.wait(t); code != 0 {
		t.Fatalf("scheduler: %d %s", code, sched.err.String())
	}
	waited := 0
	for _, name := range []string{"a", "b"} {
		var res struct {
			Results []struct {
				Status string `json:"status"`
				Record struct {
					WaitMs *int64 `json:"wait_ms"`
				} `json:"record"`
			} `json:"results"`
		}
		runJSON(t, &res, "schedule", "results", name, "--json")
		if len(res.Results) != 1 || res.Results[0].Status != "ok" {
			t.Fatalf("%s: %+v", name, res)
		}
		if w := res.Results[0].Record.WaitMs; w != nil {
			t.Logf("%s wait_ms=%d", name, *w)
			if *w >= 500 {
				waited++
			}
		}
	}
	if waited != 1 {
		t.Errorf("exactly one scheduled run should have queued for a slot, got %d", waited)
	}
	if b, _ := os.ReadFile(mark); len(strings.Fields(string(b)))/2 != 2 {
		t.Errorf("backend starts: %q", b)
	}
}
