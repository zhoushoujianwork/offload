package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/live"
)

func TestCallerFlagRejectsInvalidAndDuplicate(t *testing.T) {
	work, record := setup(t, okReply)
	for _, args := range [][]string{
		{"-caller", ""},
		{"-caller", "has space"},
		{"-caller", " agh"},
		{"-caller", "ägh"},
		{"-caller", "-bad"},
		{"-caller", strings.Repeat("a", 65)},
		{"-caller", "agh", "-caller", "agh"},
		{"-caller", "agh", "--caller=clawflow"},
	} {
		code, out, stderr := runCLI(append(append([]string{"-json", "-d", work}, args...), "task")...)
		if code == 0 {
			t.Errorf("%v accepted: %s", args, out)
			continue
		}
		rec := decodeRun(t, out)
		if rec.Status != "error" || rec.Caller != "unspecified" || !strings.Contains(stderr, "caller") {
			t.Errorf("%v: %+v stderr=%s", args, rec, stderr)
		}
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("worker launched for a rejected caller")
	}
}

func TestCallerNormalizedAndPropagated(t *testing.T) {
	work, _ := setup(t, okReply)
	events := filepath.Join(t.TempDir(), "events.ndjson")
	code, out, stderr := runCLI("-json", "-d", work, "-caller", "ClawFlow", "-caller-run-id", "job:1", "-events-file", events, "task")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	rec := decodeRun(t, out)
	if rec.Caller != "clawflow" || rec.CallerRunID != "job:1" {
		t.Fatalf("%+v", rec)
	}
	rs, _ := history.Read(history.Path())
	if len(rs) != 1 || rs[0].Caller != "clawflow" || rs[0].CallerRunID != "job:1" {
		t.Fatalf("%+v", rs)
	}
	b, _ := os.ReadFile(events)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	for _, l := range lines {
		if !strings.Contains(l, `"caller":"clawflow"`) || !strings.Contains(l, `"caller_run_id":"job:1"`) {
			t.Fatalf("event without caller: %s", l)
		}
	}
	raw, _ := os.ReadFile(history.Path())
	if !strings.Contains(string(raw), `"caller":"clawflow"`) {
		t.Fatalf("%s", raw)
	}
}

func TestCallerOmittedIsUnspecifiedInResultHistoryAndEvents(t *testing.T) {
	work, _ := setup(t, okReply)
	events := filepath.Join(t.TempDir(), "events.ndjson")
	code, out, stderr := runCLI("-json", "-d", work, "-events-file", events, "task")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	if rec := decodeRun(t, out); rec.Caller != "unspecified" {
		t.Fatalf("%+v", rec)
	}
	if !strings.Contains(out, `"caller":"unspecified"`) {
		t.Fatal(out)
	}
	b, _ := os.ReadFile(events)
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if !strings.Contains(l, `"caller":"unspecified"`) {
			t.Fatalf("%s", l)
		}
	}
}

// Each invocation, resumes included, carries only its own caller; older records stay byte-identical.
func TestResumeCallersAreIndependent(t *testing.T) {
	work, _ := setup(t, okReply)
	steps := []struct {
		args []string
		want string
	}{
		{[]string{"-caller", "agh"}, "agh"},
		{nil, "unspecified"},
		{[]string{"-caller", "Custom.Tool-1"}, "custom.tool-1"},
		{nil, "unspecified"},
	}
	var prev []byte
	for i, s := range steps {
		args := []string{"-json", "-d", work}
		if i > 0 {
			args = append(args, "-r", "s-1")
		}
		code, out, stderr := runCLI(append(append(args, s.args...), "task")...)
		if code != 0 {
			t.Fatalf("step %d: %d %s %s", i, code, out, stderr)
		}
		if rec := decodeRun(t, out); rec.Caller != s.want {
			t.Fatalf("step %d: caller %q, want %q", i, rec.Caller, s.want)
		}
		raw, _ := os.ReadFile(history.Path())
		if !strings.HasPrefix(string(raw), string(prev)) {
			t.Fatalf("step %d rewrote earlier records", i)
		}
		prev = raw
	}
	rs, _ := history.Read(history.Path())
	if len(rs) != len(steps) {
		t.Fatalf("%+v", rs)
	}
	for i, s := range steps {
		if rs[i].Caller != s.want {
			t.Errorf("record %d caller %q, want %q", i, rs[i].Caller, s.want)
		}
	}
}

func TestLegacyHistoryNotRewrittenByResume(t *testing.T) {
	work, _ := setup(t, okReply)
	path := history.Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{"ts":"2026-01-01 00:00:00","status":"ok","exit_code":0,"profile":"cheap","model":"m-1","dir":"` + work + `","task":"old","session":"s-1","cost_usd":null,"result":"old"}` + "\n"
	os.WriteFile(path, []byte(legacy), 0o600)
	if code, out, e := runCLI("-json", "-d", work, "-r", "s-1", "-caller", "agh", "next"); code != 0 {
		t.Fatalf("%s %s", out, e)
	}
	raw, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(raw), legacy) {
		t.Fatalf("legacy line changed: %s", raw)
	}
	rs, _ := history.Read(path)
	if len(rs) != 2 || rs[0].Caller != "unspecified" || rs[1].Caller != "agh" {
		t.Fatalf("%+v", rs)
	}
}

func TestLiveStateAndStatusShowCaller(t *testing.T) {
	work, _ := setup(t, okReply)
	if code, out, e := runCLI("-json", "-d", work, "-caller", "AGH", "task"); code != 0 {
		t.Fatalf("%s %s", out, e)
	}
	states, err := live.List()
	if err != nil || len(states) != 1 || states[0].Caller != "agh" {
		t.Fatalf("%+v %v", states, err)
	}
	var out, errb bytes.Buffer
	if code := cmdStatus(&out, &errb); code != 0 || !strings.Contains(out.String(), "caller=agh") {
		t.Fatalf("%d %s %s", code, out.String(), errb.String())
	}
	if got := describe(live.State{PID: 1, Phase: live.Running}); !strings.Contains(got, "caller=unspecified") {
		t.Fatal(got)
	}
}

func TestHelpExposesCallerFlag(t *testing.T) {
	_, _, stderr := runCLI("-h")
	if !strings.Contains(stderr, "-caller ") {
		t.Fatalf("help does not list -caller:\n%s", stderr)
	}
}

func TestCallerInCompletionAndHistorySummaries(t *testing.T) {
	for _, caller := range []string{"agh", ""} {
		w, out := newTestWatcher()
		s := live.State{PID: 9, Phase: live.Running, Caller: caller, LastEventAt: time.Now()}
		w.step([]live.State{s})
		out.Reset()
		s.Phase, s.Status = live.Done, "ok"
		w.step([]live.State{s})
		w.recentlyFinished([]live.State{s})
		want := "caller=" + history.CallerOrUnspecified(caller)
		for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			if !strings.Contains(line, "finished status=ok exit_code=0") || !strings.HasSuffix(line, want) {
				t.Fatalf("completion omitted caller or changed status prefix: %s", line)
			}
		}
	}
	setup(t, okReply)
	if err := history.Append(history.Path(), history.Record{Session: "caller-summary", Caller: "clawflow"}); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runCLI("show", "caller-summary")
	if code != 0 || !strings.Contains(out, "caller=clawflow") {
		t.Fatalf("show: %d %s %s", code, out, stderr)
	}
}
