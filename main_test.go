package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// setup isolates config/state dirs and installs a fake claude that records argv/env and
// answers with canned JSON (or sleeps when FAKE_SLEEP is set).
func setup(t *testing.T, reply string) (work, record string) {
	t.Helper()
	orig := preflight
	preflight = func(string) error { return nil }
	t.Cleanup(func() { preflight = orig })
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
		"{ printf '%s\\n' \"$@\"; echo \"ENV token=$ANTHROPIC_AUTH_TOKEN base=$ANTHROPIC_BASE_URL model=$ANTHROPIC_MODEL key=$ANTHROPIC_API_KEY hdr=$ANTHROPIC_CUSTOM_HEADERS pwd=$(pwd -P)\"; } > " + record + "\n" +
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
		"[ -n \"$FAKE_PREFIX\" ] && printf '%s\\n' \"$FAKE_PREFIX\"\n" +
		"[ -n \"$FAKE_SLEEP\" ] && sleep \"$FAKE_SLEEP\"\n" +
		"[ -n \"$FAKE_STDERR\" ] && printf '%s\\n' \"$FAKE_STDERR\" >&2\n" +
		"cat <<'EOF'\n" + reply + "\nEOF\n" +
		"exit \"${FAKE_EXIT:-0}\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CLAUDE", fake)
	cfg := `{"default":"cheap","profiles":{"cheap":{"base_url":"https://gw.example/anthropic","model":"m-1","token_cmd":"echo tok-123"}}}`
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath(), []byte(cfg), 0o600); err != nil {
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
	code := realMain(args, strings.NewReader(""), &out, &errb)
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
	rs, _ := readRecords(historyPath())
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
	if err := checkReachable("http://" + addr); err != nil {
		t.Fatalf("open port reported unreachable: %v", err)
	}
	ln.Close()
	err = checkReachable("http://" + addr)
	if err == nil || !strings.Contains(err.Error(), "sandbox") {
		t.Fatalf("closed port: err=%v, want sandbox hint", err)
	}
	t.Setenv("HTTPS_PROXY", "http://proxy:1")
	if err := checkReachable("http://" + addr); err != nil {
		t.Errorf("proxy set should skip direct dial: %v", err)
	}
}

func TestProfilesMarksActiveAndHidesToken(t *testing.T) {
	setup(t, okReply)
	cfg := `{"default":"a","profiles":{"a":{"base_url":"https://a","model":"ma","token":"secret-a"},"b":{"base_url":"https://b","model":"mb","token":"secret-b"}}}`
	os.WriteFile(configPath(), []byte(cfg), 0o600)
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
	if _, err := (Profile{TokenCmd: "true"}).token(); err == nil {
		t.Error("empty token_cmd output should error")
	}
}
