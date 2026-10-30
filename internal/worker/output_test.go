package worker

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

func TestOutputRetentionIsBoundedAndKeepsFinalResult(t *testing.T) {
	w := newActivityWriter(&claudeParser{})
	w.Write([]byte(`{"type":"system","subtype":"init","session_id":"s-big"}` + "\n"))
	chunk := []byte(strings.Repeat("x", 1<<20) + "\n") // 1 MiB of non-event text per line
	for i := 0; i < 64; i++ {
		w.Write(chunk)
	}
	// One line far beyond the per-line cap must be dropped, not buffered.
	huge := []byte(strings.Repeat("y", 1<<20))
	for i := 0; i < 20; i++ {
		w.Write(huge)
	}
	w.Write([]byte("\n"))
	w.Write([]byte(`{"type":"result","result":"FINAL","is_error":false,"session_id":"s-big"}`)) // no trailing newline
	w.finish()

	if len(w.raw) > maxRawKept || cap(w.pending) > maxLineKept {
		t.Fatalf("retained raw=%d pending cap=%d", len(w.raw), cap(w.pending))
	}
	s := w.summary()
	if s.Session != "s-big" {
		t.Errorf("session lost: %+v", s)
	}
}

func TestFinalResultSurvivesLargeValidStream(t *testing.T) {
	w := newActivityWriter(&claudeParser{})
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/a"}}]}}` + "\n"
	for i := 0; i < 200000; i++ {
		w.Write([]byte(line))
	}
	w.Write([]byte(`{"type":"result","result":"FINAL REPORT","is_error":false,"num_turns":200000,"session_id":"s-1"}` + "\n"))
	s := w.summary()
	if s.Result == nil || s.Result.Result != "FINAL REPORT" || s.ToolCalls != 200000 || len(s.LastTools) != lastToolsKept {
		t.Fatalf("%+v", s)
	}
	if len(w.raw) > maxRawKept {
		t.Fatalf("raw output unbounded: %d", len(w.raw))
	}
	if !strings.HasSuffix(w.String(), "[output truncated]") {
		t.Error("truncation not marked")
	}
}

func TestIncrementalParsingMatchesOneShot(t *testing.T) {
	stream := `{"type":"system","subtype":"init","session_id":"s-1"}
{"type":"assistant","message":{"content":[{"type":"text","text":"hi"},{"type":"tool_use","name":"Grep","input":{"p":"x"}}]}}
{"type":"system","subtype":"permission_denied"}
{"type":"result","result":"R","is_error":false,"session_id":"s-1","num_turns":1}
`
	w := newActivityWriter(&claudeParser{})
	for i := 0; i < len(stream); i += 7 { // arbitrary chunk boundaries split lines
		w.Write([]byte(stream[i:min(i+7, len(stream))]))
	}
	got, want := w.summary(), parseStream([]byte(stream))
	if got.Events != want.Events || got.ToolCalls != want.ToolCalls || got.Denials != want.Denials || got.Session != want.Session ||
		got.Result == nil || got.Result.Result != "R" || got.LastText != "hi" {
		t.Fatalf("got %+v want %+v", got, want)
	}
}

type recordingTap struct {
	lines   []string
	skipped []string
}

func (r *recordingTap) Line(b []byte) { r.lines = append(r.lines, string(b)) }
func (r *recordingTap) Skipped(reason string, n int) {
	r.skipped = append(r.skipped, reason+":"+strconv.Itoa(n))
}

func TestActivityWriterTapSeesLinesAndReportsDroppedOnes(t *testing.T) {
	tap := &recordingTap{}
	w := newActivityWriter(nil)
	w.tap = tap
	w.Write([]byte("{\"a\":1}\nsplit-"))
	w.Write([]byte("line\n"))
	w.Write(bytes.Repeat([]byte("x"), maxLineKept+10))
	w.Write([]byte("\n{\"b\":2}\n{\"tail\":true}"))
	w.finish()
	if strings.Join(tap.lines, "|") != "{\"a\":1}|split-line|{\"b\":2}|{\"tail\":true}" {
		t.Fatalf("%q", tap.lines)
	}
	if len(tap.skipped) != 1 || !strings.HasPrefix(tap.skipped[0], "line_too_large:") {
		t.Fatalf("%q", tap.skipped)
	}
	// An unterminated final line that is not valid JSON is reported, not silently lost.
	tap2 := &recordingTap{}
	w2 := newActivityWriter(nil)
	w2.tap = tap2
	w2.Write([]byte("{\"cut\":"))
	w2.finish()
	if len(tap2.lines) != 0 || len(tap2.skipped) != 1 || tap2.skipped[0] != "unterminated_final_line:7" {
		t.Fatalf("%q %q", tap2.lines, tap2.skipped)
	}
}
