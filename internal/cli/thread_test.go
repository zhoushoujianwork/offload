package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/thread"
)

func resumeArg(args []string) string {
	for i, a := range args {
		if a == "--resume" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func TestThreadFirstRunStartsFreshThenResumesSameSession(t *testing.T) {
	work, record := setup(t, okReply)
	if code, _, e := runCLI("-thread", "issue-1", "-d", work, "first"); code != 0 {
		t.Fatal(e)
	}
	if got := resumeArg(argv(t, record)); got != "" {
		t.Fatalf("first run must not resume, got --resume %q", got)
	}
	first := lastRecord(t)
	if first.Thread != "issue-1" || first.ThreadStart != "fresh" || first.Session != "s-1" {
		t.Fatalf("first record %+v", first)
	}
	code, _, e := runCLI("-thread", "issue-1", "-d", work, "second")
	if code != 0 {
		t.Fatal(e)
	}
	args := argv(t, record)
	if got := resumeArg(args); got != "s-1" {
		t.Fatalf("second run must resume s-1, got %q in %q", got, args)
	}
	if args[len(args)-1] != "second" {
		t.Fatalf("task must be sent unchanged and last: %q", args)
	}
	if r := lastRecord(t); r.ThreadStart != "resumed" || r.Thread != "issue-1" {
		t.Fatalf("second record %+v", r)
	}
}

func TestThreadKeysAreIndependentAndScopedByDirectory(t *testing.T) {
	work, record := setup(t, okReply)
	other := mkdirs(t, "other")[0]
	if code, _, e := runCLI("-thread", "a", "-d", work, "x"); code != 0 {
		t.Fatal(e)
	}
	for name, args := range map[string][]string{
		"different key":       {"-thread", "b", "-d", work, "x"},
		"different directory": {"-thread", "a", "-d", other, "x"},
	} {
		if code, _, e := runCLI(args...); code != 0 {
			t.Fatalf("%s: %s", name, e)
		}
		if got := resumeArg(argv(t, record)); got != "" {
			t.Fatalf("%s must start fresh, resumed %q", name, got)
		}
		if r := lastRecord(t); r.ThreadStart != "fresh" {
			t.Fatalf("%s: %+v", name, r)
		}
	}
}

func TestThreadResetStartsNewSessionAndRebinds(t *testing.T) {
	work, record := setup(t, okReply)
	runCLI("-thread", "k", "-d", work, "x")
	if code, _, e := runCLI("-thread", "k", "-thread-reset", "-d", work, "again"); code != 0 {
		t.Fatal(e)
	}
	if got := resumeArg(argv(t, record)); got != "" {
		t.Fatalf("reset must not resume, got %q", got)
	}
	if r := lastRecord(t); r.ThreadStart != "reset" {
		t.Fatalf("%+v", r)
	}
	if e, err := thread.Load("k", work); err != nil || e == nil || e.Session != "s-1" {
		t.Fatalf("binding %+v %v", e, err)
	}
}

func TestThreadRejectsInvalidUse(t *testing.T) {
	work, _ := setup(t, okReply)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"with -r":         {[]string{"-thread", "k", "-r", "s-9", "-d", work, "x"}, "cannot be combined with -r"},
		"reset alone":     {[]string{"-thread-reset", "-d", work, "x"}, "needs -thread"},
		"bad key":         {[]string{"-thread", "../x", "-d", work, "x"}, "invalid thread key"},
		"empty-ish key":   {[]string{"-thread", "-bad", "-d", work, "x"}, "invalid thread key"},
		"overlong key":    {[]string{"-thread", strings.Repeat("a", 129), "-d", work, "x"}, "invalid thread key"},
		"space in key":    {[]string{"-thread", "a b", "-d", work, "x"}, "invalid thread key"},
		"slash in key":    {[]string{"-thread", "a/b", "-d", work, "x"}, "invalid thread key"},
		"nul-ish control": {[]string{"-thread", "a\x01b", "-d", work, "x"}, "invalid thread key"},
	} {
		code, _, e := runCLI(tc.args...)
		if code != 2 || !strings.Contains(e, tc.want) {
			t.Errorf("%s: code=%d err=%q", name, code, e)
		}
	}
	rs, _ := history.Read(history.Path())
	for _, r := range rs {
		if r.ThreadStart != "" {
			t.Errorf("a rejected invocation must not record a thread start: %+v", r)
		}
	}
}

func TestThreadSavedSessionMissingFallsBackToFreshAndRebinds(t *testing.T) {
	work, record := setup(t, okReply)
	if err := thread.Save("k", work, "claude", "cheap", "gone-1"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_STDERR", "")
	// First launch (resuming gone-1) is refused by the backend; the retry has no --resume.
	script := filepath.Join(t.TempDir(), "claude")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> '" + record + ".all'\necho --- >> '" + record + ".all'\ncat > /dev/null\n" +
		"if printf '%s\\n' \"$@\" | grep -qx -- '--resume'; then\n" +
		"  echo '{\"type\":\"result\",\"subtype\":\"error_during_execution\",\"is_error\":true,\"errors\":[\"No conversation found with session ID: gone-1\"],\"session_id\":\"gone-1\"}'\n  exit 1\nfi\n" +
		"echo '" + strings.ReplaceAll(okReply, "s-1", "s-new") + "'\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CLAUDE", script)
	code, out, e := runCLI("-thread", "k", "-d", work, "go on")
	if code != 0 || strings.TrimSpace(out) != "REPORT" {
		t.Fatalf("code=%d out=%q err=%q", code, out, e)
	}
	if !strings.Contains(e, "no longer exists") {
		t.Errorf("fallback must be announced: %q", e)
	}
	all, _ := os.ReadFile(record + ".all")
	if n := strings.Count(string(all), "---"); n != 2 {
		t.Fatalf("expected exactly one retry (2 launches), got %d:\n%s", n, all)
	}
	r := lastRecord(t)
	if r.ThreadStart != "fallback" || r.Session != "s-new" {
		t.Fatalf("%+v", r)
	}
	if b, _ := thread.Load("k", work); b == nil || b.Session != "s-new" {
		t.Fatalf("binding must move to the new session: %+v", b)
	}
}

func TestThreadOtherFailureDoesNotFallBack(t *testing.T) {
	work, record := setup(t, `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"build failed: syntax error","session_id":"s-1"}`)
	if err := thread.Save("k", work, "claude", "cheap", "s-1"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_EXIT", "1")
	code, _, e := runCLI("-thread", "k", "-d", work, "go on")
	if code == 0 {
		t.Fatalf("a failed run must fail: %s", e)
	}
	if strings.Contains(e, "no longer exists") {
		t.Errorf("an unrelated failure must not start a fresh session: %q", e)
	}
	if got := resumeArg(argv(t, record)); got != "s-1" {
		t.Fatalf("must have resumed once, got %q", got)
	}
	if b, _ := thread.Load("k", work); b == nil || b.Session != "s-1" {
		t.Fatalf("binding must keep the existing session: %+v", b)
	}
	if r := lastRecord(t); r.ThreadStart != "resumed" {
		t.Fatalf("%+v", r)
	}
}

func TestThreadRefusesBackendOrProfileMismatch(t *testing.T) {
	work, record := setup(t, okReply)
	if err := thread.Save("k", work, "codex", "other-profile", "s-1"); err != nil {
		t.Fatal(err)
	}
	code, _, e := runCLI("-thread", "k", "-d", work, "x")
	if code != 2 || !strings.Contains(e, "different backend or profile") || !strings.Contains(e, "-thread-reset") {
		t.Fatalf("code=%d err=%q", code, e)
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("a refused run must not launch a worker")
	}
	if code, _, e := runCLI("-thread", "k", "-thread-reset", "-d", work, "x"); code != 0 {
		t.Fatalf("reset must clear the mismatch: %s", e)
	}
	if b, _ := thread.Load("k", work); b == nil || b.Backend != "claude" || b.Profile != "cheap" {
		t.Fatalf("%+v", b)
	}
}

func TestThreadDamagedBindingFailsSafely(t *testing.T) {
	work, _ := setup(t, okReply)
	runCLI("-thread", "good", "-d", work, "x")
	if err := thread.Save("bad", work, "claude", "cheap", "s-1"); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(thread.Dir(), "")
	entries, _ := os.ReadDir(bad)
	var damaged string
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), ".json") {
			b, _ := os.ReadFile(filepath.Join(bad, en.Name()))
			if strings.Contains(string(b), `"key":"bad"`) {
				damaged = filepath.Join(bad, en.Name())
			}
		}
	}
	if damaged == "" {
		t.Fatal("binding file for bad not found")
	}
	if err := os.WriteFile(damaged, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, e := runCLI("-thread", "bad", "-d", work, "x")
	if code != 2 || !strings.Contains(e, "unreadable") {
		t.Fatalf("code=%d err=%q", code, e)
	}
	if b, _ := os.ReadFile(damaged); string(b) != "{not json" {
		t.Fatal("a damaged binding must not be overwritten")
	}
	if code, _, e := runCLI("-thread", "good", "-d", work, "y"); code != 0 {
		t.Fatalf("other threads must be unaffected: %s", e)
	}
}

func TestThreadSimultaneousCallsOnOneKeyCannotBothLaunch(t *testing.T) {
	needUnix(t)
	work, _ := setup(t, okReply)
	bin := offloadBin(t)
	mark := filepath.Join(t.TempDir(), "starts")
	env := []string{"OFFLOAD_MAX_WORKERS=-1", "FAKE_SLEEP=2", "FAKE_MARK=" + mark}
	var runs []*child
	for i := 0; i < 4; i++ {
		runs = append(runs, start(t, bin, env, "-thread", "k", "-d", work, "go"))
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

func TestThreadReportedInStatsHistoryAndList(t *testing.T) {
	work, _ := setup(t, okReply)
	runCLI("-thread", "k", "-d", work, "one")
	runCLI("-thread", "k", "-d", work, "two")
	runCLI("-d", work, "plain")
	code, out, e := runCLI("stats", "--json", "--period", "all")
	if code != 0 {
		t.Fatal(e)
	}
	var st struct {
		Totals struct {
			Runs           int `json:"runs"`
			ThreadFresh    int `json:"thread_fresh"`
			ThreadResumed  int `json:"thread_resumed"`
			ThreadFallback int `json:"thread_fallback"`
		} `json:"totals"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if st.Totals.Runs != 3 || st.Totals.ThreadFresh != 1 || st.Totals.ThreadResumed != 1 || st.Totals.ThreadFallback != 0 {
		t.Fatalf("%+v", st.Totals)
	}
	if _, text, _ := runCLI("stats", "--period", "all"); !strings.Contains(text, "thread runs: 2 (resumed 1, fresh 1, fell back to fresh 0)") {
		t.Errorf("stats text: %s", text)
	}
	if _, show, _ := runCLI("show", "s-1"); !strings.Contains(show, "thread=k") || !strings.Contains(show, "thread_start=resumed") {
		t.Errorf("show must name the thread: %s", show)
	}
	if _, ls, _ := runCLI("ls"); !strings.Contains(ls, "thread k (resumed)") {
		t.Errorf("ls must name the thread: %s", ls)
	}
	_, hist, _ := runCLI("history", "show", "--json", "--id", "s-1")
	if !strings.Contains(hist, `"thread":"k"`) || !strings.Contains(hist, `"thread_start":"resumed"`) {
		t.Errorf("history json must carry thread fields: %s", hist)
	}
}
