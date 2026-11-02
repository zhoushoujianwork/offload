package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/runcontrol"
	"github.com/zhoushoujianwork/offload/internal/worker"
)

func TestTaskResumeHelper(t *testing.T) {
	if os.Getenv("OFFLOAD_TASK_HELPER") != "1" {
		return
	}
	worker.Preflight = func(string) error { return nil }
	code := Run("test", []string{"resume", "--id", os.Getenv("OFFLOAD_MONITOR_RESUMED_FROM"), "--json"}, os.Stdin, os.Stdout, os.Stderr)
	os.Exit(code)
}

func TestTaskResumeCreatesOneNewRunAndRetainsSource(t *testing.T) {
	work, _ := setup(t, okReply)
	t.Setenv("FAKE_EXIT", "1")
	code, out, _ := runCLI("-json", "-d", work, "-idle", "0", "inspect")
	if code == 0 {
		t.Fatal("fixture must fail")
	}
	first := decodeRun(t, out)
	m, err := runcontrol.Read(first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	executable, _ := os.Executable()
	m.ResumeCommand = []string{executable, "-test.run=^TestTaskResumeHelper$"}
	m.ResumeDirectory = work
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(runcontrol.Path(first.RunID), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_TASK_HELPER", "1")
	t.Setenv("FAKE_EXIT", "0")
	code, out, e := runCLI("task", "resume", "--id", first.RunID, "--json")
	if code != 0 {
		t.Fatal(code, out, e)
	}
	var next taskDocument
	if json.Unmarshal([]byte(out), &next) != nil || next.RunID == first.RunID || next.ResumedFrom != first.RunID {
		t.Fatal(out)
	}
	if code, out, e := runCLI("task", "resume", "--id", first.RunID, "--json"); code == 0 {
		t.Fatal("duplicate resume accepted", out, e)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, found, err := history.FindRun(next.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			if r.Session != first.Session || r.Status != "ok" {
				t.Fatal(r)
			}
			old, _, _ := history.FindRun(first.RunID)
			if old.Status != first.Status || old.Result != first.Result {
				t.Fatal("source changed")
			}
			show, err := taskSnapshot(first.RunID)
			if err != nil || show.CanResume || show.ResumedBy != next.RunID {
				t.Fatal(show, err)
			}
			raw, _ := json.Marshal(show)
			for _, secret := range []string{m.Token, m.Address, "resume_command"} {
				if strings.Contains(string(raw), secret) {
					t.Fatal("control secret leaked")
				}
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("resumed run did not finish")
}
