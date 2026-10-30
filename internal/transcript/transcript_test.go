package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
)

const (
	runA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	runB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func state(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

func open(t *testing.T, run, caller, backend string, secrets ...string) *Recorder {
	t.Helper()
	r, err := Open(Identity{RunID: run, CallerRunID: caller, Caller: "clawflow", Backend: backend, Model: "m"}, NewRedactor(secrets...))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func end(r *Recorder) {
	zero := 0
	r.End(EndInfo{Status: "ok", ExitCode: &zero})
}

func read(t *testing.T, o ReadOptions) Response {
	t.Helper()
	return Read(o)
}

func types(t *testing.T, r Response) []string {
	t.Helper()
	var out []string
	for _, raw := range r.Events {
		var e Event
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e.Type)
	}
	return out
}

func decode(t *testing.T, r Response) []Event {
	t.Helper()
	var out []Event
	for _, raw := range r.Events {
		var e Event
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

const claudeAssistant = `{"type":"assistant","session_id":"sess-1","message":{"id":"msg_1","role":"assistant","content":[{"type":"thinking","thinking":"private"},{"type":"text","text":"looking"},{"type":"tool_use","id":"tu1","name":"Bash","input":{"command":"ls"}}]}}`
const claudeResult = `{"type":"user","session_id":"sess-1","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu1","content":"a.go\nb.go","is_error":false}]}}`

func TestClaudeMappingPairsToolCallAndResult(t *testing.T) {
	state(t)
	r := open(t, runA, "job:1", "claude")
	r.BeginAttempt(AttemptKindRun, "do it")
	r.Line([]byte(claudeAssistant))
	r.Line([]byte(claudeResult))
	r.Line([]byte(`{"type":"assistant","session_id":"sess-1","message":{"id":"msg_2","content":[{"type":"text","text":"done"}]}}`))
	end(r)
	resp := read(t, ReadOptions{RunID: runA})
	if resp.Availability != Available || resp.State != StateEnded || resp.Completeness != Complete || !resp.EndOfStream {
		t.Fatalf("%+v", resp)
	}
	got := strings.Join(types(t, resp), ",")
	if got != "capture_start,attempt_start,message,message,tool_call,tool_result,message,execution_end" {
		t.Fatal(got)
	}
	ev := decode(t, resp)
	call, result := ev[4], ev[5]
	if call.ToolCallID != "tu1" || result.ToolCallID != "tu1" || call.ToolCategory != "shell" || call.Arguments != `{"command":"ls"}` ||
		result.Output != "a.go\nb.go" || result.ToolName != "Bash" || result.IsError == nil || *result.IsError {
		t.Fatalf("%+v %+v", call, result)
	}
	if strings.Contains(strings.Join(rawStrings(resp), ""), "private") {
		t.Fatal("thinking content stored")
	}
	if resp.Summary == nil || resp.Summary.SkippedBlocks != 1 || resp.Run.Session != "sess-1" || resp.Run.CallerRunID != "job:1" {
		t.Fatalf("%+v %+v", resp.Summary, resp.Run)
	}
	for i, e := range ev {
		if e.Seq != int64(i+1) || e.RunID != runA || e.Backend != "claude" {
			t.Fatalf("event %d: %+v", i, e)
		}
	}
}

func rawStrings(r Response) []string {
	var out []string
	for _, raw := range r.Events {
		out = append(out, string(raw))
	}
	return out
}

func TestCodexMappingDeduplicatesItemStates(t *testing.T) {
	state(t)
	r := open(t, runA, "", "codex")
	r.BeginAttempt(AttemptKindRun, "task")
	for _, l := range []string{
		`{"type":"thread.started","thread_id":"th-1"}`,
		`{"type":"item.started","item":{"id":"c1","type":"command_execution","command":"false","status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"c1","type":"command_execution","command":"false","aggregated_output":"boom","exit_code":1,"status":"failed"}}`,
		`{"type":"item.completed","item":{"id":"c1","type":"command_execution","command":"false","aggregated_output":"boom","exit_code":1,"status":"failed"}}`,
		`{"type":"item.completed","item":{"id":"r1","type":"reasoning","text":"secret thoughts"}}`,
		`{"type":"item.completed","item":{"id":"m1","type":"agent_message","text":"FINAL"}}`,
		`{"type":"turn.completed","usage":{}}`,
	} {
		r.Line([]byte(l))
	}
	end(r)
	resp := read(t, ReadOptions{RunID: runA})
	if got := strings.Join(types(t, resp), ","); got != "capture_start,attempt_start,message,tool_call,tool_result,message,execution_end" {
		t.Fatal(got)
	}
	ev := decode(t, resp)
	res := ev[4]
	if res.ToolCallID != "c1" || res.Output != "boom" || res.ExitCode == nil || *res.ExitCode != 1 || res.IsError == nil || !*res.IsError {
		t.Fatalf("%+v", res)
	}
	if ev[3].Arguments != "false" || ev[3].ToolCategory != "shell" || ev[5].Text != "FINAL" || ev[5].Session != "th-1" {
		t.Fatalf("%+v %+v", ev[3], ev[5])
	}
	if strings.Contains(strings.Join(rawStrings(resp), ""), "secret thoughts") || resp.Run.Session != "th-1" {
		t.Fatal("reasoning stored or session missing")
	}
}

func TestAttemptsResumeAndRetryStayDistinct(t *testing.T) {
	state(t)
	r, err := Open(Identity{RunID: runA, CallerRunID: "job:2", Backend: "claude", Session: "old-session"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.BeginAttempt(AttemptKindRun, "first")
	r.Line([]byte(claudeAssistant))
	r.BeginAttempt(AttemptKindReport, "report now")
	r.Line([]byte(`{"type":"assistant","session_id":"sess-1","message":{"content":[{"type":"text","text":"report"}]}}`))
	end(r)
	resp := read(t, ReadOptions{RunID: runA})
	ev := decode(t, resp)
	if ev[0].ResumedFrom != "old-session" || ev[0].Session != "old-session" {
		t.Fatalf("%+v", ev[0])
	}
	attempts := map[int]string{}
	for _, e := range ev {
		if e.Type == TypeAttemptStart {
			attempts[e.Attempt] = e.AttemptKind
		}
		if e.Type == TypeMessage && e.Role == "user" && e.Attempt == 2 && e.Origin != "offload_salvage" {
			t.Fatalf("salvage prompt mislabelled: %+v", e)
		}
	}
	if len(attempts) != 2 || attempts[1] != AttemptKindRun || attempts[2] != AttemptKindReport {
		t.Fatal(attempts)
	}
	// The first attempt's tool call never got a result, so coverage must say so.
	if resp.Completeness != Incomplete || !contains(resp.CompletenessReasons, "tool_calls_without_result") {
		t.Fatalf("%s %v", resp.Completeness, resp.CompletenessReasons)
	}
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

func TestSimultaneousRunsAreIsolated(t *testing.T) {
	state(t)
	a, b := open(t, runA, "job:A", "claude"), open(t, runB, "job:B", "codex")
	a.BeginAttempt(AttemptKindRun, "task A")
	b.BeginAttempt(AttemptKindRun, "task B")
	a.Line([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"only-A"}]}}`))
	b.Line([]byte(`{"type":"item.completed","item":{"id":"m","type":"agent_message","text":"only-B"}}`))
	end(a)
	end(b)
	ra, rb := read(t, ReadOptions{RunID: runA}), read(t, ReadOptions{CallerRunID: "job:B"})
	sa, sb := strings.Join(rawStrings(ra), ""), strings.Join(rawStrings(rb), "")
	if !strings.Contains(sa, "only-A") || strings.Contains(sa, "only-B") || !strings.Contains(sb, "only-B") || strings.Contains(sb, "only-A") {
		t.Fatal("transcripts mixed")
	}
	if rb.Run.RunID != runB || rb.Run.Backend != "codex" {
		t.Fatalf("%+v", rb.Run)
	}
	if l := List("", 0); len(l.Transcripts) != 2 {
		t.Fatalf("%+v", l)
	}
	if l := List("job:A", 0); len(l.Transcripts) != 1 || l.Transcripts[0].RunID != runA {
		t.Fatalf("%+v", l)
	}
}

func TestIncrementalReadCursorAndRunningState(t *testing.T) {
	state(t)
	r := open(t, runA, "job:3", "claude")
	r.BeginAttempt(AttemptKindRun, "task")
	first := read(t, ReadOptions{RunID: runA, Limit: 1})
	if first.State != StateRunning || first.Completeness != InProgress || first.EndOfStream || !first.HasMore || len(first.Events) != 1 {
		t.Fatalf("%+v", first)
	}
	seen := decode(t, first)
	cursor := first.NextCursor
	drain := func() {
		for {
			p := read(t, ReadOptions{RunID: runA, Cursor: cursor, Limit: 1})
			if p.Availability != Available {
				t.Fatalf("%+v", p)
			}
			seen = append(seen, decode(t, p)...)
			cursor = p.NextCursor
			if !p.HasMore {
				return
			}
		}
	}
	drain()
	if len(seen) != 3 {
		t.Fatalf("%d", len(seen))
	}
	// Nothing new: an empty page, the same cursor, still running.
	idle := read(t, ReadOptions{RunID: runA, Cursor: cursor})
	if len(idle.Events) != 0 || idle.NextCursor != cursor || idle.State != StateRunning || idle.EndOfStream {
		t.Fatalf("%+v", idle)
	}
	r.Line([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"more"}]}}`))
	end(r)
	drain()
	last := read(t, ReadOptions{RunID: runA, Cursor: cursor})
	if !last.EndOfStream || last.State != StateEnded || last.Completeness != Complete {
		t.Fatalf("%+v", last)
	}
	for i, e := range seen {
		if e.Seq != int64(i+1) {
			t.Fatalf("duplicate or missing seq at %d: %d", i, e.Seq)
		}
	}
	// A replay from the start yields exactly the events the tail saw.
	all := read(t, ReadOptions{RunID: runA})
	if len(all.Events) != len(seen) {
		t.Fatalf("%d != %d", len(all.Events), len(seen))
	}
}

func TestPartialTailIsNotDeliveredUntilComplete(t *testing.T) {
	state(t)
	r := open(t, runA, "", "claude")
	r.BeginAttempt(AttemptKindRun, "task")
	path := Path(runA)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString(`{"schema_version":1,"seq":4,"run_id":"` + runA + `","type":"mess`)
	p := read(t, ReadOptions{RunID: runA})
	if len(p.Events) != 3 || p.PartialTailBytes == 0 || p.HasMore || p.EndOfStream {
		t.Fatalf("%d events, tail=%d more=%v eos=%v", len(p.Events), p.PartialTailBytes, p.HasMore, p.EndOfStream)
	}
	f.WriteString(`age","ts":"x"}` + "\n")
	next := read(t, ReadOptions{RunID: runA, Cursor: p.NextCursor})
	if len(next.Events) != 1 || next.PartialTailBytes != 0 {
		t.Fatalf("%+v", next)
	}
}

func TestStaleAndInvalidCursors(t *testing.T) {
	state(t)
	r := open(t, runA, "", "claude")
	r.BeginAttempt(AttemptKindRun, "task")
	end(r)
	good := read(t, ReadOptions{RunID: runA, Limit: 1}).NextCursor
	other := strings.Replace(good, ".", ".0", 2)
	for name, c := range map[string]string{
		"malformed":       "garbage",
		"other capture":   "t1.0000000000000000.10.2",
		"beyond end":      "t1." + captureOf(t, good) + ".99999999.2",
		"mid event":       "t1." + captureOf(t, good) + ".5.2",
		"negative":        "t1." + captureOf(t, good) + ".-1.1",
		"changed version": strings.Replace(good, "t1.", "t9.", 1),
		"junk":            other,
	} {
		resp := read(t, ReadOptions{RunID: runA, Cursor: c})
		if resp.Availability != CursorInvalid || len(resp.Events) != 0 || resp.NextCursor != "" {
			t.Errorf("%s: %+v", name, resp)
		}
	}
	// Recreated capture (retention then a new run with the same id cannot happen, but a replaced
	// file has a new capture id): the old cursor must not be accepted.
	os.Remove(Path(runA))
	r2 := open(t, runA, "", "claude")
	end(r2)
	if resp := read(t, ReadOptions{RunID: runA, Cursor: good}); resp.Availability != CursorInvalid {
		t.Fatalf("%+v", resp)
	}
}

func captureOf(t *testing.T, cursor string) string {
	t.Helper()
	id, _, _, ok := parseCursor(cursor)
	if !ok {
		t.Fatal(cursor)
	}
	return id
}

func TestFieldBoundsAndTruncationMarkers(t *testing.T) {
	state(t)
	r := open(t, runA, "", "codex")
	r.BeginAttempt(AttemptKindRun, strings.Repeat("p", MaxMessageBytes+10))
	huge := strings.Repeat("é", MaxToolOutputBytes) // 2 bytes per rune: cutting must stay valid UTF-8
	l, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"id": "c", "type": "command_execution", "command": "x", "aggregated_output": huge, "exit_code": 0}})
	r.Line(l)
	end(r)
	resp := read(t, ReadOptions{RunID: runA})
	var sawOut, sawPrompt bool
	for _, e := range decode(t, resp) {
		if e.Type == TypeToolResult {
			sawOut = true
			if e.Truncated["output"] != len(huge) || len(e.Output) > MaxToolOutputBytes || !json.Valid([]byte(`"`+e.Output+`"`)) {
				t.Fatalf("%v %d", e.Truncated, len(e.Output))
			}
			for _, c := range e.Output {
				if c == '�' {
					t.Fatal("cut inside a rune")
				}
			}
		}
		if e.Type == TypeMessage && e.Role == "user" {
			sawPrompt = true
			if e.Truncated["text"] != MaxMessageBytes+10 {
				t.Fatalf("%v", e.Truncated)
			}
		}
	}
	if !sawOut || !sawPrompt || resp.Completeness != Bounded || !contains(resp.CompletenessReasons, "truncated_fields") || resp.Summary.TruncatedEvents != 2 {
		t.Fatalf("%v %v %+v", sawOut, sawPrompt, resp)
	}
}

func TestEscapingHeavyEventStaysUnderEventBound(t *testing.T) {
	state(t)
	r := open(t, runA, "", "codex")
	r.BeginAttempt(AttemptKindRun, "t")
	nasty := strings.Repeat("\x01", MaxToolOutputBytes) // each byte becomes a 6-byte JSON escape
	l, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"id": "c", "type": "command_execution", "command": "x", "aggregated_output": nasty}})
	r.Line(l)
	end(r)
	b, _ := os.ReadFile(Path(runA))
	for _, line := range strings.SplitAfter(string(b), "\n") {
		if len(line) > MaxEventBytes {
			t.Fatalf("event of %d bytes", len(line))
		}
	}
}

func TestCaptureSizeLimitKeepsExecutionEndAndMarksGap(t *testing.T) {
	state(t)
	r := open(t, runA, "", "claude")
	r.BeginAttempt(AttemptKindRun, "t")
	chunk := strings.Repeat("x", MaxMessageBytes)
	for range MaxCaptureBytes/MaxMessageBytes + 8 {
		l, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": chunk}}}})
		r.Line(l)
	}
	end(r)
	st, _ := os.Stat(Path(runA))
	if st.Size() > MaxCaptureBytes {
		t.Fatalf("file is %d bytes", st.Size())
	}
	var all Response
	cursor := ""
	var evs []Event
	for {
		all = read(t, ReadOptions{RunID: runA, Cursor: cursor, Limit: MaxReadLimit})
		evs = append(evs, decode(t, all)...)
		cursor = all.NextCursor
		if !all.HasMore {
			break
		}
	}
	if all.State != StateEnded || all.Completeness != Incomplete || !contains(all.CompletenessReasons, "capture_size_limit") ||
		all.Summary == nil || !all.Summary.CaptureTruncated || all.Summary.DroppedEvents == 0 {
		t.Fatalf("%+v %+v", all.CompletenessReasons, all.Summary)
	}
	gap := 0
	for _, e := range evs {
		if e.Type == TypeGap && e.Reason == "capture_size_limit" {
			gap++
		}
	}
	if gap != 1 || evs[len(evs)-1].Type != TypeExecutionEnd {
		t.Fatalf("gap=%d last=%s", gap, evs[len(evs)-1].Type)
	}
}

func TestSkippedLineIsAGapNotSilence(t *testing.T) {
	state(t)
	r := open(t, runA, "", "claude")
	r.BeginAttempt(AttemptKindRun, "t")
	r.Line([]byte("not json at all"))
	r.Skipped("line_too_large", 123)
	r.Line([]byte(`{"type":"brand_new_event"}`))
	end(r)
	resp := read(t, ReadOptions{RunID: runA})
	if resp.Completeness != Incomplete || resp.Summary.Gaps != 2 || resp.Summary.Unrecognized != 1 {
		t.Fatalf("%+v %v", resp.Summary, resp.CompletenessReasons)
	}
}

func TestCrashedWriterIsAbandonedAndIncomplete(t *testing.T) {
	state(t)
	r := open(t, runA, "", "claude")
	r.BeginAttempt(AttemptKindRun, "t")
	// Simulate a killed offload: the file keeps its events, but the recorded process is not ours.
	b, _ := os.ReadFile(Path(runA))
	b = []byte(strings.Replace(string(b), `"pid_start":"`, `"pid_start":"x`, 1))
	if err := os.WriteFile(Path(runA), b, 0o600); err != nil {
		t.Fatal(err)
	}
	resp := read(t, ReadOptions{RunID: runA})
	if resp.State != StateAbandoned || resp.Completeness != Incomplete || !contains(resp.CompletenessReasons, "no_execution_end") || !resp.EndOfStream {
		t.Fatalf("%+v", resp)
	}
}

func TestCancelledRunRecordsEndFields(t *testing.T) {
	state(t)
	r := open(t, runA, "", "claude")
	code := 130
	r.End(EndInfo{Status: "error", ExitCode: &code, ErrorKind: "canceled", StopReason: "canceled", TerminationClass: "canceled"})
	r.End(EndInfo{Status: "ok"}) // a second End is ignored
	resp := read(t, ReadOptions{RunID: runA})
	ev := decode(t, resp)
	last := ev[len(ev)-1]
	if last.Type != TypeExecutionEnd || last.ErrorKind != "canceled" || last.TerminationClass != "canceled" || *last.ExitCode != 130 || last.Status != "error" {
		t.Fatalf("%+v", last)
	}
	if resp.Availability != NoCapturedContent || resp.State != StateEnded {
		t.Fatalf("%s %s", resp.Availability, resp.State)
	}
}

func TestAvailabilityStates(t *testing.T) {
	state(t)
	logRec := func(r history.Record) {
		if err := history.Append(history.Path(), r); err != nil {
			t.Fatal(err)
		}
	}
	logRec(history.Record{RunID: runA, CallerRunID: "plain", Backend: "claude", Status: "ok"})
	logRec(history.Record{RunID: runB, CallerRunID: "old", Backend: "claude", Status: "ok", Transcript: "captured"})
	other := "cccccccccccccccccccccccccccccccc"
	logRec(history.Record{RunID: other, CallerRunID: "bad", Backend: "claude", Status: "error", Transcript: "failed"})
	acp := "dddddddddddddddddddddddddddddddd"
	logRec(history.Record{RunID: acp, CallerRunID: "x", Backend: "gemini", Status: "ok"})
	for _, tc := range []struct {
		opt  ReadOptions
		want string
	}{
		{ReadOptions{RunID: runA}, CaptureDisabled},
		{ReadOptions{CallerRunID: "plain"}, CaptureDisabled},
		{ReadOptions{RunID: runB}, Expired},
		{ReadOptions{RunID: other}, Unreadable},
		{ReadOptions{RunID: acp}, UnsupportedBackend},
		{ReadOptions{RunID: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"}, NotFound},
		{ReadOptions{CallerRunID: "nobody"}, NotFound},
	} {
		if got := Read(tc.opt); got.Availability != tc.want || len(got.Events) != 0 {
			t.Errorf("%+v: %s (%s)", tc.opt, got.Availability, got.Reason)
		}
	}
}

func TestNotYetAvailableUnsupportedVersionAndMismatch(t *testing.T) {
	state(t)
	if err := prepareDir(Dir()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(runA), []byte(`{"schema_version":1,"seq":1`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := Read(ReadOptions{RunID: runA}); got.Availability != NotYetAvailable {
		t.Fatalf("%+v", got)
	}
	line := func(version int, run, backend string) []byte {
		b, _ := json.Marshal(Event{SchemaVersion: version, Seq: 1, RunID: run, Type: TypeCaptureStart, CaptureID: "0123456789abcdef", Backend: backend})
		return append(b, '\n')
	}
	os.WriteFile(Path(runA), line(2, runA, "claude"), 0o600)
	if got := Read(ReadOptions{RunID: runA}); got.Availability != UnsupportedVersion {
		t.Fatalf("%+v", got)
	}
	os.WriteFile(Path(runA), line(1, runA, "gemini"), 0o600)
	if got := Read(ReadOptions{RunID: runA}); got.Availability != UnsupportedBackend {
		t.Fatalf("%+v", got)
	}
	os.WriteFile(Path(runA), line(1, runB, "claude"), 0o600)
	if got := Read(ReadOptions{RunID: runA}); got.Availability != Unreadable {
		t.Fatalf("copied file accepted: %+v", got)
	}
	r := open(t, runB, "job:real", "claude")
	end(r)
	if got := Read(ReadOptions{RunID: runB, CallerRunID: "job:other"}); got.Availability != IdentityMismatch {
		t.Fatalf("%+v", got)
	}
}

func TestAmbiguousCallerRunIDListsCandidates(t *testing.T) {
	state(t)
	end(open(t, runA, "same", "claude"))
	end(open(t, runB, "same", "claude"))
	got := Read(ReadOptions{CallerRunID: "same"})
	if got.Availability != Ambiguous || len(got.Candidates) != 2 || len(got.Events) != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestUnsafePathsAndPermissionsAreRejected(t *testing.T) {
	state(t)
	end(open(t, runA, "", "claude"))
	// Group-readable file.
	os.Chmod(Path(runA), 0o644)
	if got := Read(ReadOptions{RunID: runA}); got.Availability != Unreadable {
		t.Fatalf("%+v", got)
	}
	os.Chmod(Path(runA), 0o600)
	// Symlinked transcript file.
	target := filepath.Join(t.TempDir(), "elsewhere")
	os.Rename(Path(runA), target)
	os.Symlink(target, Path(runA))
	if got := Read(ReadOptions{RunID: runA}); got.Availability != Unreadable {
		t.Fatalf("symlink followed: %+v", got)
	}
	if _, err := Open(Identity{RunID: runA, Backend: "claude"}, nil); err == nil {
		t.Fatal("capture opened over a symlink")
	}
	// Open directory.
	os.Chmod(Dir(), 0o755)
	if got := Read(ReadOptions{RunID: runB}); got.Availability != Unreadable {
		t.Fatalf("%+v", got)
	}
	if _, err := Open(Identity{RunID: runB, Backend: "claude"}, nil); err == nil {
		t.Fatal("capture opened in a shared directory")
	}
	os.Chmod(Dir(), 0o700)
	// Symlinked store directory.
	real := filepath.Join(t.TempDir(), "real")
	os.Mkdir(real, 0o700)
	os.RemoveAll(Dir())
	os.Symlink(real, Dir())
	if _, err := Open(Identity{RunID: runB, Backend: "claude"}, nil); err == nil {
		t.Fatal("capture opened through a symlinked directory")
	}
	// Run IDs are never joined into paths unless they have the run-id shape.
	if ValidRunID("../../etc/passwd") || ValidRunID(strings.ToUpper(runA)) {
		t.Fatal("unsafe run id accepted")
	}
	if _, err := Open(Identity{RunID: "../x", Backend: "claude"}, nil); err == nil {
		t.Fatal("unsafe run id opened")
	}
}

func TestExistingFileIsNeverOverwritten(t *testing.T) {
	state(t)
	end(open(t, runA, "", "claude"))
	before, _ := os.ReadFile(Path(runA))
	if _, err := Open(Identity{RunID: runA, Backend: "claude"}, nil); err == nil {
		t.Fatal("existing transcript reopened")
	}
	after, _ := os.ReadFile(Path(runA))
	if string(before) != string(after) {
		t.Fatal("transcript changed")
	}
	st, _ := os.Stat(Path(runA))
	if st.Mode().Perm() != 0o600 {
		t.Fatal(st.Mode())
	}
	if d, _ := os.Stat(Dir()); d.Mode().Perm() != 0o700 {
		t.Fatal(d.Mode())
	}
}

func TestRedaction(t *testing.T) {
	state(t)
	const token = "tok-super-secret-value-123"
	r := open(t, runA, "", "codex", token)
	r.AddSecrets("short") // below the minimum length: ignored, so ordinary words survive
	r.BeginAttempt(AttemptKindRun, "use "+token)
	out := "echo " + token + "\nAuthorization: Bearer abcdefghijklmnopqrstuvwx\nkey sk-abcdefghijklmnopqrstuv\nghp_abcdefghijklmnopqrstuvwxyz0123\n" +
		"API_KEY=zzzzzzzzzzzz\nAKIAABCDEFGHIJKLMNOP\n-----BEGIN RSA PRIVATE KEY-----\nMIIB\n-----END RSA PRIVATE KEY-----\nshort stays"
	l, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"id": "c", "type": "command_execution", "command": "cat env " + token, "aggregated_output": out, "exit_code": 0}})
	r.Line(l)
	end(r)
	raw, _ := os.ReadFile(Path(runA))
	for _, secret := range []string{token, "abcdefghijklmnopqrstuvwx", "sk-abcdefghijklmnopqrstuv", "ghp_abcdefghijkl", "zzzzzzzzzzzz", "AKIAABCDEFGHIJKLMNOP", "MIIB"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%q survived in\n%s", secret, raw)
		}
	}
	if !strings.Contains(string(raw), "short stays") {
		t.Fatal("ordinary text was blanked")
	}
	resp := read(t, ReadOptions{RunID: runA})
	if resp.Summary.Redactions != 3 {
		t.Fatalf("redacted events = %d", resp.Summary.Redactions)
	}
	for _, e := range decode(t, resp) {
		if (e.Type == TypeMessage || e.Type == TypeToolCall || e.Type == TypeToolResult) && !e.Redacted {
			t.Fatalf("event not flagged: %+v", e)
		}
	}
}

func TestRedactionRunsBeforeTruncation(t *testing.T) {
	red := NewRedactor("0123456789-secret")
	// The secret straddles the 8-byte cut; removing it first leaves nothing of it behind.
	s, n := red.Apply("aaaa0123456789-secretbbbb")
	if n != 1 || strings.Contains(s, "0123") {
		t.Fatal(s)
	}
	if got := EnvSecrets([]string{"HOME=/x", "MY_API_KEY=abc12345", "GH_TOKEN=def67890", "PATH=/bin"}); len(got) != 2 {
		t.Fatal(got)
	}
}

func TestPruneKeepsLiveAndCurrentWritersAndRemovesOld(t *testing.T) {
	state(t)
	old := open(t, runA, "", "claude")
	end(old)
	live := open(t, runB, "", "claude") // writer is this process: alive
	past := time.Now().Add(-RetainFor - time.Hour)
	for _, id := range []string{runA, runB} {
		os.Chtimes(Path(id), past, past)
	}
	if n := Prune(Dir(), "", time.Now()); n != 1 {
		t.Fatalf("removed %d", n)
	}
	if _, err := os.Stat(Path(runA)); !os.IsNotExist(err) {
		t.Fatal("expired transcript kept")
	}
	if _, err := os.Stat(Path(runB)); err != nil {
		t.Fatal("transcript of a live writer removed")
	}
	live.End(EndInfo{Status: "ok"})
	// File-count and size bounds remove the oldest first.
	oldFiles, oldBytes := RetainFiles, RetainBytes
	defer func() { RetainFiles, RetainBytes = oldFiles, oldBytes }()
	RetainFiles = 1
	end(open(t, "cccccccccccccccccccccccccccccccc", "", "claude"))
	if n := len(storeFiles(Dir())); n != 1 {
		t.Fatalf("%d files kept", n)
	}
	// After retention removed a run, reading says so instead of pretending it never existed.
	history.Append(history.Path(), history.Record{RunID: runB, Backend: "claude", Transcript: "captured"})
	if got := Read(ReadOptions{RunID: runB}); got.Availability != Expired {
		t.Fatalf("%+v", got)
	}
}

func TestWriteFailureMarksCaptureIncompleteWithoutPanicking(t *testing.T) {
	state(t)
	r, err := newRecorder(&failAfter{n: 2}, Identity{RunID: runA, Backend: "claude"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.BeginAttempt(AttemptKindRun, "t")
	r.Line([]byte(claudeAssistant))
	end(r) // must not panic or block
	if !r.failed {
		t.Fatal("failure not noticed")
	}
}

type failAfter struct{ n int }

func (f *failAfter) Write(b []byte) (int, error) {
	if f.n--; f.n < 0 {
		return 0, os.ErrClosed
	}
	return len(b), nil
}
func (f *failAfter) Close() error { return nil }

func TestNilRecorderIsNoOp(t *testing.T) {
	var r *Recorder
	r.AddSecrets("x")
	r.BeginAttempt(AttemptKindRun, "t")
	r.Line([]byte("{}"))
	r.Skipped("x", 1)
	r.End(EndInfo{})
}
