package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/threads"
	"github.com/zhoushoujianwork/offload/internal/worker"
)

func readRuns(t *testing.T) []history.Record {
	t.Helper()
	rs, err := history.Read(history.Path())
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

func TestThreadSecondRunResumesFirstSession(t *testing.T) {
	work, record := setup(t, okReply)
	code, out, e := runCLI("-json", "-d", work, "--thread", "issue-39", "first instruction")
	if code != 0 {
		t.Fatalf("first: %d %s %s", code, out, e)
	}
	first, _ := os.ReadFile(record)
	if strings.Contains(string(first), "--resume") {
		t.Fatalf("first run must be fresh: %s", first)
	}
	if !strings.Contains(e, "thread=issue-39 thread_start=fresh") {
		t.Fatalf("summary line lacks thread: %s", e)
	}
	code, out, e = runCLI("-json", "-d", work, "--thread", "issue-39", "only the new instruction")
	if code != 0 {
		t.Fatalf("second: %d %s %s", code, out, e)
	}
	args, _ := os.ReadFile(record)
	if !strings.Contains(string(args), "--resume\ns-1\n") {
		t.Fatalf("second run did not resume s-1: %s", args)
	}
	stdin, _ := os.ReadFile(record + ".stdin")
	prompt, _ := os.ReadFile(record + ".prompt")
	if strings.Contains(string(stdin)+string(prompt), "first instruction") {
		t.Fatalf("earlier turns were sent again: %s %s", stdin, prompt)
	}
	var rec history.Record
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Thread != "issue-39" || rec.ThreadStart != history.ThreadResumed || rec.Session != "s-1" || rec.Task != "only the new instruction" {
		t.Fatalf("%+v", rec)
	}
	rs := readRuns(t)
	if len(rs) != 2 || rs[0].ThreadStart != history.ThreadFresh || rs[1].ThreadStart != history.ThreadResumed {
		t.Fatalf("%+v", rs)
	}
}

func TestThreadResumeInheritsDirAndAddDirs(t *testing.T) {
	work, record := setup(t, okReply)
	extra := filepath.Join(filepath.Dir(work), "extra")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	extra, _ = filepath.EvalSymlinks(extra)
	if code, out, e := runCLI("-d", work, "--add-dir", extra, "--thread", "k", "one"); code != 0 {
		t.Fatalf("%d %s %s", code, out, e)
	}
	if code, out, e := runCLI("-d", work, "--thread", "k", "two"); code != 0 {
		t.Fatalf("%d %s %s", code, out, e)
	}
	b, _ := os.ReadFile(record)
	if !strings.Contains(string(b), "--add-dir\n"+extra+"\n") || !strings.Contains(string(b), "pwd="+work) {
		t.Fatalf("resume lost directories: %s", b)
	}
}

func TestThreadIsScopedByDirectory(t *testing.T) {
	work, record := setup(t, okReply)
	other := filepath.Join(filepath.Dir(work), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if code, _, e := runCLI("-d", work, "--thread", "k", "one"); code != 0 {
		t.Fatal(e)
	}
	if code, _, e := runCLI("-d", other, "--thread", "k", "one"); code != 0 {
		t.Fatal(e)
	}
	if b, _ := os.ReadFile(record); strings.Contains(string(b), "--resume") {
		t.Fatalf("a different directory resumed the thread: %s", b)
	}
}

func TestThreadCodexResumesBySessionID(t *testing.T) {
	work, record := setupCodex(t, codexReply)
	if code, _, e := runCLI("-d", work, "-p", "codex", "--thread", "c", "one"); code != 0 {
		t.Fatal(e)
	}
	if b, _ := os.ReadFile(record); strings.Contains(string(b), "resume") {
		t.Fatalf("first codex run must be fresh: %s", b)
	}
	if code, _, e := runCLI("-d", work, "--thread", "c", "two"); code != 0 {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(record)
	if !strings.Contains(string(b), "resume\n") || !strings.Contains(string(b), "codex-session\n") {
		t.Fatalf("codex did not resume: %s", b)
	}
	rs := readRuns(t)
	if rs[1].Backend != "codex" || rs[1].ThreadStart != history.ThreadResumed {
		t.Fatalf("%+v", rs[1])
	}
}

func TestThreadConcurrentRunIsRejectedAndFirstUnaffected(t *testing.T) {
	work, record := setup(t, okReply)
	t.Setenv("FAKE_SLEEP", "2")
	var wg sync.WaitGroup
	var code int
	var out, e string
	wg.Add(1)
	go func() {
		defer wg.Done()
		code, out, e = runCLI("-json", "-d", work, "--thread", "busy", "long")
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(record); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first run never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Setenv("FAKE_SLEEP", "")
	code2, out2, e2 := runCLI("-d", work, "--thread", "busy", "overlap")
	if code2 == 0 || !strings.Contains(e2, `thread "busy" already has an active run`) {
		t.Fatalf("overlap accepted: %d %s %s", code2, out2, e2)
	}
	wg.Wait()
	if code != 0 {
		t.Fatalf("first run affected: %d %s %s", code, out, e)
	}
	rs := readRuns(t)
	var ok, rejected int
	for _, r := range rs {
		if r.Status == "ok" {
			ok++
		} else if r.Session == "" && r.ThreadStart == "" {
			rejected++
		}
	}
	if ok != 1 || rejected != 1 {
		t.Fatalf("%+v", rs)
	}
	// The first run saved the mapping; the next call resumes.
	if code, _, e := runCLI("-d", work, "--thread", "busy", "again"); code != 0 {
		t.Fatal(e)
	}
}

// installRejectingClaude makes a resume fail like Claude does for a session it no longer has when
// the file gone exists; otherwise each fresh run reports a new session id.
func installRejectingClaude(t *testing.T, gone string) {
	t.Helper()
	counter := gone + ".n"
	script := "#!/bin/sh\ncat >/dev/null\n" +
		"if [ -e '" + gone + "' ] && printf '%s\\n' \"$@\" | grep -qx -- '--resume'; then\n" +
		"  echo '{\"type\":\"result\",\"subtype\":\"error_during_execution\",\"is_error\":true,\"num_turns\":0,\"session_id\":\"stale\",\"errors\":[\"No conversation found with session ID: stale\"]}'\n  exit 1\nfi\n" +
		"n=$(cat '" + counter + "' 2>/dev/null || echo 0); n=$((n+1)); echo $n > '" + counter + "'\n" +
		"printf '%s\\n' \"$@\" > '" + gone + ".args'\n" +
		"echo '{\"type\":\"result\",\"result\":\"REPORT\",\"is_error\":false,\"num_turns\":1,\"session_id\":\"fresh-'$n'\"}'\n"
	path := filepath.Join(t.TempDir(), "claude2")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CLAUDE", path)
}

func TestThreadRejectedSessionFallsBackToFreshAndUpdatesMapping(t *testing.T) {
	work, _ := setup(t, okReply)
	gone := filepath.Join(t.TempDir(), "gone")
	installRejectingClaude(t, gone)
	if code, _, e := runCLI("-d", work, "--thread", "k", "one"); code != 0 {
		t.Fatal(e)
	}
	if err := os.WriteFile(gone, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, e := runCLI("-json", "-d", work, "--thread", "k", "two")
	if code != 0 {
		t.Fatalf("fallback failed: %d %s %s", code, out, e)
	}
	if !strings.Contains(e, "thread_start=fallback") || !strings.Contains(e, "was rejected by the backend") || !strings.Contains(e, "fresh-1") {
		t.Fatalf("fallback is not visible: %s", e)
	}
	var rec history.Record
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Session != "fresh-2" || rec.ThreadStart != history.ThreadFallback || rec.ThreadNote == "" || rec.Status != "ok" {
		t.Fatalf("%+v", rec)
	}
	if args, _ := os.ReadFile(gone + ".args"); strings.Contains(string(args), "--resume") {
		t.Fatalf("fallback must be a fresh run: %s", args)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	// The mapping now points at the replacement session.
	if code, _, e := runCLI("-d", work, "--thread", "k", "three"); code != 0 {
		t.Fatal(e)
	}
	args, _ := os.ReadFile(gone + ".args")
	if !strings.Contains(string(args), "--resume\nfresh-2\n") {
		t.Fatalf("third run did not resume the replacement session: %s", args)
	}
	rs := readRuns(t)
	if last := rs[len(rs)-1]; last.ThreadStart != history.ThreadResumed || last.Session != "fresh-3" {
		t.Fatalf("%+v", last)
	}
}

func TestThreadNonSessionFailureDoesNotFallBack(t *testing.T) {
	work, record := setup(t, okReply)
	if code, _, e := runCLI("-d", work, "--thread", "k", "one"); code != 0 {
		t.Fatal(e)
	}
	t.Setenv("FAKE_EXIT", "1")
	runCLI("-d", work, "--thread", "k", "two") // the fake exits 1 after printing a result
	b, _ := os.ReadFile(record)
	if n := strings.Count(string(b), "--resume"); n != 1 {
		t.Fatalf("expected one resume attempt, got %d: %s", n, b)
	}
	rs := readRuns(t)
	if len(rs) != 2 || rs[1].ThreadStart != history.ThreadResumed {
		t.Fatalf("%+v", rs)
	}
}

func TestThreadBackendOrProfileMismatchIsRefusedAndReset(t *testing.T) {
	work, _ := setupCodex(t, codexReply)
	if code, _, e := runCLI("-d", work, "-p", "codex", "--thread", "k", "one"); code != 0 {
		t.Fatal(e)
	}
	worker.Preflight = func(string) error { return nil } // the Claude profile below needs the gateway check
	code, _, e := runCLI("-d", work, "-p", "cheap", "--thread", "k", "two")
	if code == 0 || !strings.Contains(e, "bound to profile") || !strings.Contains(e, "--thread-reset") {
		t.Fatalf("mismatch accepted: %d %s", code, e)
	}
	before := readRuns(t)
	if len(before) != 2 || before[1].Session != "" || before[1].ThreadStart != "" {
		t.Fatalf("refused run must not look started: %+v", before)
	}
	code, _, e = runCLI("-d", work, "-p", "cheap", "--thread", "k", "--thread-reset", "two")
	if code != 0 || !strings.Contains(e, "thread_start=fresh") {
		t.Fatalf("reset: %d %s", code, e)
	}
	// The mapping now belongs to the new profile.
	if code, _, e := runCLI("-d", work, "--thread", "k", "three"); code != 0 || !strings.Contains(e, "cheap/") {
		t.Fatalf("%d %s", code, e)
	}
}

func TestThreadKeepsItsProfileWhenDefaultChanges(t *testing.T) {
	work, record := setupCodex(t, codexReply)
	if code, _, e := runCLI("-d", work, "-p", "codex", "--thread", "k", "one"); code != 0 {
		t.Fatal(e)
	}
	if code, _, e := runCLI("-d", work, "--thread", "k", "two"); code != 0 || !strings.Contains(e, "codex/") {
		t.Fatalf("%d %s", code, e)
	}
	if b, _ := os.ReadFile(record); !strings.Contains(string(b), "resume\n") {
		t.Fatalf("%s", b)
	}
}

func TestThreadCannotCombineWithResume(t *testing.T) {
	work, _ := setup(t, okReply)
	code, _, e := runCLI("-d", work, "--thread", "k", "-r", "s-1", "x")
	if code == 0 || !strings.Contains(e, "cannot be combined with -r") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, e := runCLI("-d", work, "--thread-reset", "x"); code == 0 || !strings.Contains(e, "requires --thread") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, e := runCLI("-d", work, "--thread", "bad key", "x"); code == 0 || !strings.Contains(e, "whitespace") {
		t.Fatalf("%d %s", code, e)
	}
}

func TestThreadCorruptMappingFailsSafely(t *testing.T) {
	work, _ := setup(t, okReply)
	if code, _, e := runCLI("-d", work, "--thread", "good", "one"); code != 0 {
		t.Fatal(e)
	}
	h, err := threads.Acquire(threads.Dir(), "broken", work)
	if err != nil {
		t.Fatal(err)
	}
	bad := []byte("{truncated")
	if err := os.WriteFile(h.Path(), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	h.Release()
	code, _, e := runCLI("-d", work, "--thread", "broken", "x")
	if code == 0 || !strings.Contains(e, "unreadable") || !strings.Contains(e, "--thread-reset") {
		t.Fatalf("%d %s", code, e)
	}
	if b, _ := os.ReadFile(h.Path()); string(b) != string(bad) {
		t.Fatalf("corrupt mapping was modified: %s", b)
	}
	if code, _, e := runCLI("-d", work, "--thread", "good", "two"); code != 0 || !strings.Contains(e, "thread_start=resumed") {
		t.Fatalf("other thread affected: %d %s", code, e)
	}
	if code, _, e := runCLI("-d", work, "--thread", "broken", "--thread-reset", "x"); code != 0 {
		t.Fatalf("reset: %d %s", code, e)
	}
}

func TestThreadReportingInListShowHistoryAndStats(t *testing.T) {
	work, _ := setup(t, okReply)
	runCLI("-d", work, "--thread", "rep", "one")
	runCLI("-d", work, "--thread", "rep", "two")
	runCLI("-d", work, "plain")
	_, ls, _ := runCLI("ls")
	if !strings.Contains(ls, "Thread: rep (fresh)") || !strings.Contains(ls, "Thread: rep (resumed)") {
		t.Fatalf("ls: %s", ls)
	}
	_, show, _ := runCLI("show", "s-1")
	if !strings.Contains(show, "thread=rep thread_start=fresh") || !strings.Contains(show, "thread=rep thread_start=resumed") {
		t.Fatalf("show: %s", show)
	}
	_, hist, _ := runCLI("history", "show", "--json", "--id", "s-1")
	if !strings.Contains(hist, `"thread":"rep"`) || !strings.Contains(hist, `"thread_start":"resumed"`) {
		t.Fatalf("history: %s", hist)
	}
	_, st, _ := runCLI("stats", "--json", "--period", "all")
	var doc struct {
		Totals history.StatsTotals `json:"totals"`
	}
	if err := json.Unmarshal([]byte(st), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Totals.ThreadFreshRuns != 1 || doc.Totals.ThreadResumedRuns != 1 || doc.Totals.ThreadFallbackRuns != 0 {
		t.Fatalf("%+v", doc.Totals)
	}
	_, text, _ := runCLI("stats", "--period", "all")
	if !strings.Contains(text, "thread runs: 2 (resumed 1, fresh 1") {
		t.Fatalf("stats text: %s", text)
	}
}

func TestNoThreadRunsAreUnchanged(t *testing.T) {
	work, _ := setup(t, okReply)
	_, out, _ := runCLI("-json", "-d", work, "plain")
	if strings.Contains(out, `"thread`) {
		t.Fatalf("thread fields leaked into a plain run: %s", out)
	}
}
