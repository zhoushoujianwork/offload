package cli

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/live"
	"github.com/zhoushoujianwork/offload/internal/worker"
)

// setup isolates config/state dirs and installs a fake claude that records argv/env and
// answers with canned JSON (or sleeps when FAKE_SLEEP is set).
func setup(t *testing.T, reply string) (work, record string) {
	t.Helper()
	orig := worker.Preflight
	worker.Preflight = func(string) error { return nil }
	t.Cleanup(func() { worker.Preflight = orig })
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "caller-secret")
	t.Setenv("ANTHROPIC_API_KEY", "caller-key")
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "X-Caller: 1")
	t.Setenv("OFFLOAD_PROFILE", "")
	record = filepath.Join(tmp, "record")
	fake := filepath.Join(tmp, "claude")
	script := "#!/bin/sh\n" +
		"{ printf '%s\\n' \"$@\"; echo \"ENV token=$ANTHROPIC_AUTH_TOKEN base=$ANTHROPIC_BASE_URL model=$ANTHROPIC_MODEL key=$ANTHROPIC_API_KEY hdr=$ANTHROPIC_CUSTOM_HEADERS pwd=$(pwd -P) gomaxprocs=$GOMAXPROCS goflags=$GOFLAGS\"; } > " + record + "\n" +
		"cat > '" + record + ".stdin'\n" +
		"[ -n \"$FAKE_CHILD\" ] && { sleep 300 >/dev/null 2>&1 & echo $! > \"$FAKE_CHILD\"; }\n" +
		"[ -n \"$FAKE_MARK\" ] && echo \"$$ $(date +%s)\" >> \"$FAKE_MARK\"\n" +
		"prompt_file=''; inline_prompt=0; previous=''\n" +
		"for arg in \"$@\"; do\n" +
		"  [ \"$previous\" = '--append-system-prompt-file' ] && prompt_file=$arg\n" +
		"  [ \"$arg\" = '--append-system-prompt' ] && inline_prompt=1\n" +
		"  previous=$arg\n" +
		"done\n" +
		"if [ -n \"$prompt_file\" ] && [ \"$inline_prompt\" = 1 ]; then echo 'Cannot use both append prompt flags' >&2; exit 9; fi\n" +
		"if [ -n \"$prompt_file\" ]; then\n" +
		"  printf '%s\\n' \"$prompt_file\" > '" + record + ".prompt-path'\n" +
		"  cat \"$prompt_file\" > '" + record + ".prompt'\n" +
		"  ls -l \"$prompt_file\" > '" + record + ".prompt-mode'\n" +
		"fi\n" +
		"if [ -n \"$FAKE_SALVAGE\" ] && printf '%s\\n' \"$@\" | grep -qx -- '--tools'; then\n" +
		"  [ -n \"$FAKE_SALVAGE_SLEEP\" ] && sleep \"$FAKE_SALVAGE_SLEEP\"\n" +
		"  printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"'\"$FAKE_SALVAGE\"'\",\"session_id\":\"s-killed\",\"total_cost_usd\":0.02}'\n" +
		"  echo \"$@\" > '" + record + ".salvage-args'\n" +
		"  exit 0\n" +
		"fi\n" +
		"[ -n \"$FAKE_PREFIX\" ] && printf '%s\\n' \"$FAKE_PREFIX\"\n" +
		"[ -n \"$FAKE_TICKS\" ] && for i in $(seq 1 \"$FAKE_TICKS\"); do printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"thinking_tokens\"}'; sleep 0.4; done\n" +
		"[ -n \"$FAKE_SLEEP\" ] && sleep \"$FAKE_SLEEP\"\n" +
		"[ -n \"$FAKE_STDERR\" ] && printf '%s\\n' \"$FAKE_STDERR\" >&2\n" +
		"cat <<'EOF'\n" + reply + "\nEOF\n" +
		"exit \"${FAKE_EXIT:-0}\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CLAUDE", fake)
	cfg := `{"default":"cheap","profiles":{"cheap":{"base_url":"https://gw.example/anthropic","model":"m-1","token_cmd":"echo tok-123"}}}`
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	work = filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	work, _ = filepath.EvalSymlinks(work)
	return work, record
}

func runCLI(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Run("test", args, strings.NewReader(""), &out, &errb)
	return code, out.String(), errb.String()
}

const okReply = `{"type":"result","result":"REPORT","is_error":false,"num_turns":3,"total_cost_usd":0.01,"duration_ms":1200,"session_id":"s-1"}`

func TestRunReadOnlyPrintsReportAndIsolatesCredentials(t *testing.T) {
	work, record := setup(t, okReply)
	code, out, errOut := runCLI("-d", work, "find the bug")
	if code != 0 || strings.TrimSpace(out) != "REPORT" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if !strings.Contains(errOut, "session=s-1") {
		t.Errorf("meta line missing session: %q", errOut)
	}
	rec, _ := os.ReadFile(record)
	got := string(rec)
	for _, want := range []string{"--bare", "--setting-sources\n\n--output-format", "find the bug",
		"token=tok-123 base=https://gw.example/anthropic model=m-1 key= hdr= pwd=" + work} {
		if !strings.Contains(got, want) {
			t.Errorf("argv/env missing %q in:\n%s", want, got)
		}
	}
	prompt, err := os.ReadFile(record + ".prompt")
	if err != nil || !strings.Contains(string(prompt), "Read-only task") {
		t.Fatalf("read-only instruction missing from merged prompt: %q err=%v", prompt, err)
	}
	if strings.Contains(got, "tok-123\n") || strings.Contains(got, "caller-") {
		t.Errorf("token leaked into argv or caller credential reached worker:\n%s", got)
	}
	if strings.Contains(got, "acceptEdits") {
		t.Error("read-only run must not get acceptEdits")
	}
}

// rtk saves tokens, so its read-only forms must be allowed; its command runners must not be,
// since they would bypass the tool lists.
func TestRtkReadFormsAllowedRunnersNot(t *testing.T) {
	for _, write := range []bool{false, true} {
		work, record := setup(t, okReply)
		args := []string{"-d", work, "x"}
		if write {
			args = []string{"-w", "-d", work, "x"}
		}
		runCLI(args...)
		rec, _ := os.ReadFile(record)
		for _, want := range []string{"Bash(rtk ls:*)", "Bash(rtk read:*)", "Bash(rtk grep:*)"} {
			if !strings.Contains(string(rec), want) {
				t.Errorf("write=%v: missing %q", write, want)
			}
		}
		for _, bad := range []string{"Bash(rtk run", "Bash(rtk proxy", "Bash(rtk err", "Bash(rtk summary", "Bash(rtk pipe", "Bash(rtk test", "Bash(rtk find"} {
			if strings.Contains(string(rec), bad) {
				t.Errorf("write=%v: %q would let the worker run arbitrary commands", write, bad)
			}
		}
	}
}

func TestWriteModeAndProjectInstructions(t *testing.T) {
	work, record := setup(t, okReply)
	os.WriteFile(filepath.Join(work, "AGENTS.md"), []byte("rules"), 0o644)
	if code, _, e := runCLI("-w", "-d", work, "edit"); code != 0 {
		t.Fatal(e)
	}
	rec, _ := os.ReadFile(record)
	for _, want := range []string{"acceptEdits", "Bash(go test:*)"} {
		if !strings.Contains(string(rec), want) {
			t.Errorf("missing %q", want)
		}
	}
	prompt, err := os.ReadFile(record + ".prompt")
	if err != nil || !strings.Contains(string(prompt), filepath.Join(work, "AGENTS.md")) || !strings.Contains(string(prompt), "rules") || strings.Contains(string(prompt), "Read-only task") {
		t.Fatalf("write-mode project instructions: %q err=%v", prompt, err)
	}
	if strings.Contains(string(rec), "Bash(git push") || strings.Contains(string(rec), "Bash(rm") {
		t.Error("write mode must not allow push or rm")
	}
}

func TestHistoryListShowAndResumeDir(t *testing.T) {
	work, record := setup(t, okReply)
	runCLI("-d", work, "first task")

	_, out, _ := runCLI("ls")
	if !strings.Contains(out, "s-1") || !strings.Contains(out, "first task") {
		t.Fatalf("ls: %q", out)
	}
	_, out, _ = runCLI("show", "s-1")
	if !strings.Contains(out, "REPORT") || !strings.Contains(out, "> first task") {
		t.Fatalf("show: %q", out)
	}
	if code, _, _ := runCLI("show", "nope"); code == 0 {
		t.Error("show of unknown session should fail")
	}

	// Resume from another cwd must run in the session's original dir.
	t.Chdir(t.TempDir())
	if code, _, e := runCLI("-r", "s-1", "follow up"); code != 0 {
		t.Fatal(e)
	}
	rec, _ := os.ReadFile(record)
	lines := strings.Split(string(rec), "\n")
	if !slices.Contains(lines, "--resume") || !strings.Contains(string(rec), "pwd="+work) {
		t.Errorf("resume not wired to original dir:\n%s", rec)
	}
}

func TestErrorAndTimeoutStatus(t *testing.T) {
	work, _ := setup(t, `{"type":"result","result":"API Error: 401","is_error":true,"session_id":"s-e"}`)
	code, out, _ := runCLI("-d", work, "x")
	if code != 1 || !strings.Contains(out, "401") {
		t.Fatalf("error run: code=%d out=%q", code, out)
	}
	t.Setenv("FAKE_SLEEP", "5")
	code, _, _ = runCLI("-d", work, "-t", "200ms", "slow")
	if code != 124 {
		t.Fatalf("timeout exit = %d, want 124", code)
	}
	rs, _ := history.Read(history.Path())
	if len(rs) != 2 || rs[0].Status != "error" || rs[1].Status != "timeout" {
		t.Fatalf("records: %+v", rs)
	}
}

func TestPreflightFailsFastOnUnreachableGateway(t *testing.T) {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"} {
		t.Setenv(k, "")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := worker.CheckReachable("http://" + addr); err != nil {
		t.Fatalf("open port reported unreachable: %v", err)
	}
	ln.Close()
	err = worker.CheckReachable("http://" + addr)
	if err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("closed port: err=%v, want sandbox hint", err)
	}
	t.Setenv("HTTPS_PROXY", "http://proxy:1")
	if err := worker.CheckReachable("http://" + addr); err != nil {
		t.Errorf("proxy set should skip direct dial: %v", err)
	}
}

func TestProfilesMarksActiveAndHidesToken(t *testing.T) {
	setup(t, okReply)
	cfg := `{"default":"a","profiles":{"a":{"base_url":"https://a","model":"ma","token":"secret-a"},"b":{"base_url":"https://b","model":"mb","token":"secret-b"}}}`
	os.WriteFile(config.Path(), []byte(cfg), 0o600)
	_, out, _ := runCLI("profiles")
	if !strings.Contains(out, "* a ") || strings.Contains(out, "secret") {
		t.Fatalf("profiles: %q", out)
	}
	t.Setenv("OFFLOAD_PROFILE", "b")
	if _, out, _ = runCLI("profiles"); !strings.Contains(out, "* b ") {
		t.Fatalf("OFFLOAD_PROFILE not honored: %q", out)
	}
}

func TestConfigErrors(t *testing.T) {
	work, _ := setup(t, okReply)
	if code, _, e := runCLI("-p", "missing", "-d", work, "x"); code != 2 || !strings.Contains(e, "unknown profile") {
		t.Errorf("code=%d err=%q", code, e)
	}
	if _, err := (config.Profile{TokenCmd: "true"}).ResolveToken(); err == nil {
		t.Error("empty token_cmd output should error")
	}
}

// A resume must keep the session's profile; if it is gone, fail with guidance instead of
// silently running on the default provider.
func TestResumeWithDeletedProfileFailsWithRecovery(t *testing.T) {
	work, record := setup(t, okReply)
	if code, _, e := runCLI("-d", work, "first"); code != 0 {
		t.Fatal(e)
	}
	cfg := `{"default":"other","profiles":{"other":{"base_url":"https://other.example/anthropic","model":"m-2","token_cmd":"echo tok-999"}}}`
	if err := os.WriteFile(config.Path(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(record)
	code, out, errOut := runCLI("-r", "s-1", "follow up")
	if code != 2 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	for _, want := range []string{`profile "cheap"`, "Restore that profile", "-p <profile>", "offload show s-1"} {
		if !strings.Contains(out+errOut, want) {
			t.Errorf("recovery guidance missing %q in %q", want, out+errOut)
		}
	}
	if _, err := os.Stat(record); err == nil {
		t.Error("worker must not run on a different provider")
	}
}

// Overlapping follow-ups to one session are refused; other sessions are unaffected.
func TestResumeRefusesSessionWithActiveRun(t *testing.T) {
	work, record := setup(t, okReply)
	if code, _, e := runCLI("-d", work, "first"); code != 0 {
		t.Fatal(e)
	}
	now := time.Now()
	if err := live.Publish(live.State{PID: os.Getpid() + 100000, Phase: live.Running, Session: "s-1", Dir: work,
		StartedAt: now, LastEventAt: now}); err != nil {
		t.Fatal(err)
	}
	os.Remove(record)
	code, out, errOut := runCLI("-r", "s-1", "second")
	if code != 2 || !strings.Contains(out+errOut, "already has an active run") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if _, err := os.Stat(record); err == nil {
		t.Error("worker must not start while the session is busy")
	}
	// A finished run no longer blocks the session.
	done := live.State{PID: os.Getpid() + 100000, Phase: live.Done, Status: "ok", Session: "s-1", Dir: work,
		StartedAt: now, LastEventAt: now}
	if err := live.Publish(done); err != nil {
		t.Fatal(err)
	}
	if code, _, e := runCLI("-r", "s-1", "second"); code != 0 {
		t.Fatalf("resume after completion: %d %s", code, e)
	}
}

// A resume keeps the session's profile even when the default changes, and its directory even
// when run from elsewhere or with a different Claude profile available.
func TestResumeKeepsProfileAndDirectory(t *testing.T) {
	work, record := setup(t, okReply)
	if code, _, e := runCLI("-d", work, "first"); code != 0 {
		t.Fatal(e)
	}
	cfg := `{"default":"other","profiles":{` +
		`"cheap":{"base_url":"https://gw.example/anthropic","model":"m-1","token_cmd":"echo tok-123"},` +
		`"other":{"base_url":"https://other.example/anthropic","model":"m-2","token_cmd":"echo tok-999"}}}`
	if err := os.WriteFile(config.Path(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	if code, _, e := runCLI("-r", "s-1", "second"); code != 0 {
		t.Fatal(e)
	}
	rec, _ := os.ReadFile(record)
	for _, want := range []string{"base=https://gw.example/anthropic model=m-1", "pwd=" + work} {
		if !strings.Contains(string(rec), want) {
			t.Errorf("resume lost %q:\n%s", want, rec)
		}
	}
	rs, _ := history.Read(history.Path())
	if len(rs) != 2 || rs[1].Profile != "cheap" || rs[1].Dir != work {
		t.Fatalf("history of the resumed run: %+v", rs)
	}
}

// If the session's directory is gone, say how to recover instead of running somewhere else.
func TestResumeWithMissingDirectoryFailsWithRecovery(t *testing.T) {
	work, record := setup(t, okReply)
	if code, _, e := runCLI("-d", work, "first"); code != 0 {
		t.Fatal(e)
	}
	if err := os.RemoveAll(work); err != nil {
		t.Fatal(err)
	}
	os.Remove(record)
	code, out, errOut := runCLI("-r", "s-1", "second")
	if code != 2 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	for _, want := range []string{"cannot resume session s-1", "not a directory", "pass -d", "offload show s-1"} {
		if !strings.Contains(out+errOut, want) {
			t.Errorf("recovery guidance missing %q in %q", want, out+errOut)
		}
	}
	if _, err := os.Stat(record); err == nil {
		t.Error("worker must not run in a different directory")
	}
}

// A resumed run is registered under its session at once, before the worker has emitted its
// first event, so a second follow-up started right away can be refused.
func TestResumedRunHoldsItsSessionFromTheStart(t *testing.T) {
	work, _ := setup(t, okReply)
	if code, _, e := runCLI("-d", work, "first"); code != 0 {
		t.Fatal(e)
	}
	t.Setenv("FAKE_SLEEP", "3")
	done := make(chan struct{})
	go func() { defer close(done); runCLI("-r", "s-1", "second") }()
	defer func() { <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		states, _ := live.List()
		held := false
		for _, s := range states {
			held = held || (s.Active() && s.Session == "s-1" && s.ToolCalls == 0)
		}
		if held {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("resumed run never held its session")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The live state of a finished run must carry the real exit code, not the zero value.
func TestLiveStateRecordsFailureExitCode(t *testing.T) {
	work, _ := setup(t, `{"type":"result","result":"API Error: 401","is_error":true,"session_id":"s-e"}`)
	if code, _, _ := runCLI("-d", work, "x"); code != 1 {
		t.Fatalf("code=%d", code)
	}
	states, _ := live.List()
	if len(states) != 1 || states[0].Phase != live.Done || states[0].Status != "error" || states[0].ExitCode != 1 {
		t.Fatalf("%+v", states)
	}
}
