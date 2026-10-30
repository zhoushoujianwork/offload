package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/transcript"
)

func readTranscript(t *testing.T, args ...string) transcript.Response {
	t.Helper()
	code, out, stderr := runCLI(append([]string{"transcript", "read", "--json"}, args...)...)
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	var r transcript.Response
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return r
}

const transcriptCodexReply = `{"type":"thread.started","thread_id":"codex-session"}
{"type":"item.started","item":{"id":"cmd1","type":"command_execution","command":"echo SECRET_FROM_ENV","status":"in_progress"}}
{"type":"item.completed","item":{"id":"cmd1","type":"command_execution","command":"echo SECRET_FROM_ENV","aggregated_output":"SECRET_FROM_ENV\n","exit_code":0,"status":"completed"}}
{"type":"item.completed","item":{"id":"msg1","type":"agent_message","text":"CODEX REPORT"}}
{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":10}}`

func TestCaptureTranscriptReplayAfterRunAndMetadataUnchanged(t *testing.T) {
	work, _ := setupCodex(t, transcriptCodexReply)
	t.Setenv("MY_SERVICE_TOKEN", "SECRET_FROM_ENV")
	events := filepath.Join(t.TempDir(), "events.ndjson")
	code, out, stderr := runCLI("-json", "-p", "codex", "-d", work, "-caller-run-id", "job:77", "-events-file", events, "-capture-transcript", "inspect the repo")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, stderr)
	}
	rec := decodeRun(t, out)
	if rec.Result != "CODEX REPORT" || rec.RunID == "" {
		t.Fatalf("%+v", rec)
	}
	// Metadata events keep their old shape and carry no content.
	meta, _ := os.ReadFile(events)
	for _, leak := range []string{"CODEX REPORT", "echo", "inspect the repo", "SECRET_FROM_ENV"} {
		if bytes.Contains(meta, []byte(leak)) {
			t.Fatalf("events-file leaked %q", leak)
		}
	}
	// The run log says capture happened, and nothing else about its content.
	rs, _ := history.Read(history.Path())
	if len(rs) != 1 || rs[0].Transcript != "captured" {
		t.Fatalf("%+v", rs)
	}
	for _, args := range [][]string{{"--run-id", rec.RunID}, {"--caller-run-id", "job:77"}} {
		resp := readTranscript(t, args...)
		if resp.Availability != transcript.Available || resp.State != transcript.StateEnded || resp.Completeness != transcript.Complete || !resp.EndOfStream {
			t.Fatalf("%+v", resp)
		}
		if resp.Run.RunID != rec.RunID || resp.Run.CallerRunID != "job:77" || resp.Run.Backend != "codex" || resp.Run.Session != "codex-session" {
			t.Fatalf("%+v", resp.Run)
		}
		var kinds []string
		var all strings.Builder
		for _, raw := range resp.Events {
			var e transcript.Event
			json.Unmarshal(raw, &e)
			kinds = append(kinds, e.Type)
			all.Write(raw)
		}
		if strings.Join(kinds, ",") != "capture_start,attempt_start,message,tool_call,tool_result,message,execution_end" {
			t.Fatal(kinds)
		}
		if strings.Contains(all.String(), "SECRET_FROM_ENV") || !strings.Contains(all.String(), "CODEX REPORT") || !strings.Contains(all.String(), "inspect the repo") {
			t.Fatalf("redaction or content wrong: %s", all.String())
		}
	}
	// The transcript file is private.
	st, err := os.Stat(transcript.Path(rec.RunID))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
	// list finds it by caller run id and exposes no content.
	_, listOut, _ := runCLI("transcript", "list", "--json", "--caller-run-id", "job:77")
	if !strings.Contains(listOut, rec.RunID) || strings.Contains(listOut, "CODEX REPORT") {
		t.Fatal(listOut)
	}
}

func TestWithoutCaptureFlagNothingIsRecorded(t *testing.T) {
	work, _ := setupCodex(t, transcriptCodexReply)
	code, out, _ := runCLI("-json", "-p", "codex", "-d", work, "inspect")
	if code != 0 {
		t.Fatal(out)
	}
	rec := decodeRun(t, out)
	if _, err := os.Stat(transcript.Dir()); !os.IsNotExist(err) {
		t.Fatalf("transcript store created without opt-in: %v", err)
	}
	if resp := readTranscript(t, "--run-id", rec.RunID); resp.Availability != transcript.CaptureDisabled {
		t.Fatalf("%+v", resp)
	}
	rs, _ := history.Read(history.Path())
	if rs[0].Transcript != "" {
		t.Fatal(rs[0].Transcript)
	}
}

func TestTailRunningInvocationThenReplay(t *testing.T) {
	work, record := setupCodex(t, transcriptCodexReply)
	// A worker that prints its first events, waits for a release file, then finishes.
	release := filepath.Join(t.TempDir(), "release")
	fake := filepath.Join(t.TempDir(), "codex")
	script := "#!/bin/sh\ncat >/dev/null\n" +
		"printf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"codex-session\"}'\n" +
		"printf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"id\":\"m0\",\"type\":\"agent_message\",\"text\":\"FIRST\"}}'\n" +
		"while [ ! -e '" + release + "' ]; do sleep 0.05; done\n" +
		"printf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"id\":\"m1\",\"type\":\"agent_message\",\"text\":\"SECOND\"}}'\n" +
		"printf '%s\\n' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	os.WriteFile(fake, []byte(script), 0o755)
	t.Setenv("OFFLOAD_CODEX", fake)
	_ = record

	var wg sync.WaitGroup
	var runOut string
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, runOut, _ = runCLI("-json", "-p", "codex", "-d", work, "-caller-run-id", "job:live", "-capture-transcript", "task")
	}()
	// Bind to the running invocation by caller-run-id alone, as a supervisor would.
	var first transcript.Response
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		first = readTranscript(t, "--caller-run-id", "job:live")
		if first.Availability == transcript.Available && strings.Contains(rawJoin(first), "FIRST") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if first.State != transcript.StateRunning || first.Completeness != transcript.InProgress || first.EndOfStream || !strings.Contains(rawJoin(first), "FIRST") {
		t.Fatalf("%+v", first)
	}
	if strings.Contains(rawJoin(first), "SECOND") {
		t.Fatal("future content visible")
	}
	// Reading did not occupy a slot or start another worker.
	if n := countRunning(t); n != 1 {
		t.Fatalf("%d running states", n)
	}
	os.WriteFile(release, nil, 0o600)
	wg.Wait()
	rec := decodeRun(t, runOut)
	if rec.Status != "ok" {
		t.Fatalf("%+v", rec)
	}
	rest := readTranscript(t, "--run-id", first.Run.RunID, "--cursor", first.NextCursor)
	if rest.State != transcript.StateEnded || !rest.EndOfStream || rest.Completeness != transcript.Complete || !strings.Contains(rawJoin(rest), "SECOND") || strings.Contains(rawJoin(rest), "FIRST") {
		t.Fatalf("%+v", rest)
	}
	var lastSeq int64
	for _, resp := range []transcript.Response{first, rest} {
		for _, raw := range resp.Events {
			var e transcript.Event
			json.Unmarshal(raw, &e)
			if e.Seq != lastSeq+1 {
				t.Fatalf("seq %d after %d", e.Seq, lastSeq)
			}
			lastSeq = e.Seq
		}
	}
}

func rawJoin(r transcript.Response) string {
	var b strings.Builder
	for _, raw := range r.Events {
		b.Write(raw)
	}
	return b.String()
}

func countRunning(t *testing.T) int {
	t.Helper()
	_, out, _ := runCLI("status")
	return strings.Count(out, "running")
}

func TestCancelledQueuedOrFailedSetupStillExplainsItself(t *testing.T) {
	work, _ := setupCodex(t, transcriptCodexReply)
	// A rejected invocation (bad caller) never created a transcript; reading must say so.
	code, out, _ := runCLI("-json", "-p", "codex", "-d", work, "-capture-transcript", "-caller-run-id", "job:bad", "-caller", "BAD CALLER", "x")
	if code == 0 {
		t.Fatal(out)
	}
	rec := decodeRun(t, out)
	resp := readTranscript(t, "--run-id", rec.RunID)
	if resp.Availability != transcript.NoCapturedContent || len(resp.Events) != 0 {
		t.Fatalf("%+v", resp)
	}
}

func TestFailedBackendRunEndsTranscriptWithStatus(t *testing.T) {
	work, _ := setupCodex(t, `{"type":"thread.started","thread_id":"codex-session"}
{"type":"turn.failed","error":{"message":"model exploded"}}`)
	code, out, _ := runCLI("-json", "-p", "codex", "-d", work, "-capture-transcript", "x")
	if code == 0 {
		t.Fatal(out)
	}
	rec := decodeRun(t, out)
	resp := readTranscript(t, "--run-id", rec.RunID)
	var end transcript.Event
	for _, raw := range resp.Events {
		json.Unmarshal(raw, &end)
	}
	if end.Type != transcript.TypeExecutionEnd || end.Status != "error" || end.ExitCode == nil || *end.ExitCode != code || !strings.Contains(rawJoin(resp), "model exploded") {
		t.Fatalf("%+v", end)
	}
}

func TestUnsafeStoreFailsRunBeforeLaunch(t *testing.T) {
	work, argv := setupCodex(t, transcriptCodexReply)
	dir := transcript.Dir()
	os.MkdirAll(filepath.Dir(dir), 0o700)
	os.Symlink(t.TempDir(), dir)
	code, out, _ := runCLI("-json", "-p", "codex", "-d", work, "-capture-transcript", "x")
	rec := decodeRun(t, out)
	if code != 2 || rec.ErrorKind != "configuration" || rec.Transcript != "failed" || !strings.Contains(rec.Error, "capture-transcript") {
		t.Fatal(code, rec)
	}
	if _, err := os.Stat(argv); !os.IsNotExist(err) {
		t.Fatal("worker launched although capture was requested and impossible")
	}
}

func TestTranscriptCommandValidationAndCapabilities(t *testing.T) {
	setupCodex(t, transcriptCodexReply)
	for _, args := range [][]string{
		{"transcript"},
		{"transcript", "bogus", "--json"},
		{"transcript", "read"},
		{"transcript", "read", "--json"},
		{"transcript", "read", "--json", "--run-id", "../../etc/passwd"},
		{"transcript", "read", "--json", "--run-id", "short"},
		{"transcript", "read", "--json", "--caller-run-id", "has space"},
		{"transcript", "read", "--json", "--run-id", strings.Repeat("a", 32), "--limit", "-1"},
		{"transcript", "read", "--json", "--run-id", strings.Repeat("a", 32), "extra"},
	} {
		if code, out, _ := runCLI(args...); code != 2 || out != "" {
			t.Errorf("%v: %d %q", args, code, out)
		}
	}
	code, out, _ := runCLI("transcript", "capabilities", "--json")
	var c capabilitiesOutput
	if code != 0 || json.Unmarshal([]byte(out), &c) != nil {
		t.Fatal(code, out)
	}
	if c.SchemaVersion != 1 || c.Interface != "offload.transcript" || c.Capture.Default != "off" || len(c.Backends) != 2 || c.Reading.StartsBackend || c.Reading.TakesSlot {
		t.Fatalf("%+v", c)
	}
	for _, b := range c.Backends {
		if b.TextDeltas || strings.Contains(strings.ToLower(b.Source), "acp") && !strings.Contains(b.Source, "not ACP") {
			t.Fatalf("%+v", b)
		}
	}
}

func TestReadNeverLaunchesABackend(t *testing.T) {
	_, argv := setupCodex(t, transcriptCodexReply)
	for _, args := range [][]string{
		{"transcript", "read", "--json", "--run-id", strings.Repeat("a", 32)},
		{"transcript", "list", "--json"},
		{"transcript", "capabilities", "--json"},
	} {
		runCLI(args...)
	}
	if _, err := os.Stat(argv); !os.IsNotExist(err) {
		t.Fatal("a backend was launched by a read")
	}
}

func TestClaudeSalvageIsASeparateAttempt(t *testing.T) {
	work, _ := setup(t, `{"type":"system","session_id":"s-killed"}
{"type":"assistant","session_id":"s-killed","message":{"id":"m1","content":[{"type":"text","text":"reading"},{"type":"tool_use","id":"tu9","name":"Read","input":{"file_path":"/a"}}]}}`)
	t.Setenv("FAKE_SALVAGE", "SALVAGED REPORT")
	t.Setenv("FAKE_EXIT", "1")
	_, out, _ := runCLI("-json", "-d", work, "-capture-transcript", "-caller-run-id", "job:salv", "investigate")
	rec := decodeRun(t, out)
	if !rec.Salvaged {
		t.Skipf("fixture did not salvage: %+v", rec)
	}
	resp := readTranscript(t, "--run-id", rec.RunID)
	attempts := map[int]string{}
	for _, raw := range resp.Events {
		var e transcript.Event
		json.Unmarshal(raw, &e)
		if e.Type == transcript.TypeAttemptStart {
			attempts[e.Attempt] = e.AttemptKind
		}
		if e.Type == transcript.TypeMessage && e.Role == "user" && e.Attempt == 2 && e.Origin != "offload_salvage" {
			t.Fatalf("%+v", e)
		}
	}
	if len(attempts) != 2 || attempts[1] != "run" || attempts[2] != "salvage" {
		t.Fatalf("%v", attempts)
	}
	// The interrupted first attempt left a tool call without a result: not presented as complete.
	if resp.Completeness != transcript.Incomplete || resp.Run.Session != "s-killed" {
		t.Fatalf("%s %v %+v", resp.Completeness, resp.CompletenessReasons, resp.Run)
	}
}
