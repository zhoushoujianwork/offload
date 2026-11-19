package worker

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

type discardWriteCloser struct{ io.Writer }

func (discardWriteCloser) Close() error { return nil }

func TestCodexAppServerImageLinesAreSanitizedEverywhere(t *testing.T) {
	secret := bytes.Repeat([]byte("private-image-byte-"), 1<<15)
	encoded := base64.StdEncoding.EncodeToString(secret)
	dataURL := "data:image/png;base64," + encoded
	nested, _ := json.Marshal(map[string]any{"type": "image", "url": dataURL})
	frames := [][]byte{
		mustTestJSON(t, map[string]any{
			"method": "item/started",
			"params": map[string]any{
				"threadId": "thread", "turnId": "turn",
				"item": map[string]any{"id": "user", "type": "userMessage", "content": []any{map[string]any{"type": "image", "url": dataURL}}},
			},
		}),
		mustTestJSON(t, map[string]any{
			"method": "item/completed",
			"params": map[string]any{
				"threadId": "thread", "turnId": "turn",
				"item": map[string]any{"id": "user", "type": "userMessage", "content": []any{map[string]any{"type": "text", "text": string(nested)}}},
			},
		}),
	}
	parser := newAppServerParser(discardWriteCloser{io.Discard}, Opts{Native: &NativeControls{}})
	w := newActivityWriter(parser)
	w.callbackTail = true
	tap := &recordingTap{}
	w.tap = tap
	var observed []string
	w.onLine = func(line []byte) error { observed = append(observed, string(line)); return nil }
	stream := append(append(append([]byte{}, frames[0]...), '\n'), frames[1]...)
	for offset := 0; offset < len(stream); offset += 137 {
		w.Write(stream[offset:min(offset+137, len(stream))])
	}
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(secret)
	for name, output := range map[string]string{
		"observer": strings.Join(observed, "\n"),
		"capture":  strings.Join(tap.lines, "\n"),
		"raw":      w.String(),
	} {
		if strings.Contains(output, encoded) || strings.Contains(output, string(secret)) {
			t.Fatalf("%s leaked image data", name)
		}
		if !strings.Contains(output, hex.EncodeToString(digest[:])) || !strings.Contains(output, `bytes=`) {
			t.Fatalf("%s lost omission evidence: %s", name, output)
		}
	}
	if got := w.summary().Events; got != 2 {
		t.Fatalf("sanitized frames changed parser summary: events=%d", got)
	}
}

func TestCodexAppServerImageSanitizerFailsClosedAndKeepsTextExact(t *testing.T) {
	parser := newAppServerParser(discardWriteCloser{io.Discard}, Opts{Native: &NativeControls{}})
	w := newActivityWriter(parser)
	w.callbackTail = true
	tap := &recordingTap{}
	w.tap = tap
	var observed []string
	w.onLine = func(line []byte) error { observed = append(observed, string(line)); return nil }
	plain := []byte(`{"method":"notice","params":{"text":"ordinary text"}}`)
	w.Write(append(append([]byte{}, plain...), '\n'))
	secret := base64.StdEncoding.EncodeToString([]byte("escaped-malformed-private-image"))
	malformed := []byte(`{"method":"item/completed","params":{"url":"\u0064ata:image\/png\u003bbase64\u002c` + secret)
	w.Write(malformed)
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 2 || observed[0] != string(plain) || tap.lines[0] != string(plain) {
		t.Fatalf("normal text changed: observer=%q capture=%q", observed, tap.lines)
	}
	for _, output := range append(append([]string{}, observed...), tap.lines...) {
		if strings.Contains(output, secret) || strings.Contains(output, "data:image") {
			t.Fatalf("malformed image frame leaked: %s", output)
		}
	}
	if !strings.Contains(w.String(), "malformed_image_frame") || strings.Contains(w.String(), secret) {
		t.Fatalf("diagnostic did not fail closed: %s", w.String())
	}
	if parser.err == nil || parser.err.Error() != "Codex app-server emitted a malformed JSON-RPC frame" {
		t.Fatalf("malformed frame did not fail parser eagerly: %v", parser.err)
	}
	if got := string(sanitizeCodexAppServerLine([]byte("not json"))); got != malformedImageFrame {
		t.Fatalf("marker-free malformed JSON was not omitted: %q", got)
	}
}

func TestCodexAppServerImageSanitizerHandlesJSONEscapedSlash(t *testing.T) {
	secret := []byte("escaped-slash-private-image")
	encoded := base64.StdEncoding.EncodeToString(secret)
	line := []byte(`{"method":"item/started","params":{"url":"\u0064ata:image\/png;base64,` + encoded + `"}}`)
	sanitized := sanitizeCodexAppServerLine(line)
	if bytes.Contains(sanitized, []byte(encoded)) || !bytes.Contains(sanitized, []byte("sha256=")) || !json.Valid(sanitized) {
		t.Fatalf("escaped image URL was not safely redacted: %s", sanitized)
	}
}

func TestCodexAppServerNestedEscapedImageURLIsSanitized(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString([]byte("nested-escaped-private-image"))
	nested := `{"url":"\u0064ata:image\/png\u003bbase64\u002c` + secret + `"}`
	line := mustTestJSON(t, map[string]any{
		"method": "item/completed", "params": map[string]any{"id": "kept", "text": nested},
	})
	sanitized := sanitizeCodexAppServerLine(line)
	if bytes.Contains(sanitized, []byte(secret)) || !bytes.Contains(sanitized, []byte("sha256=")) || !bytes.Contains(sanitized, []byte(`"id":"kept"`)) || !json.Valid(sanitized) {
		t.Fatalf("nested escaped image URL leaked or lost protocol evidence: %s", sanitized)
	}
}

func TestCodexAppServerEmbeddedImageURLPreservesTerminalReport(t *testing.T) {
	secret := []byte("embedded-private-image")
	encoded := base64.StdEncoding.EncodeToString(secret)
	report := "before data:image/jpeg;base64," + encoded + " after"
	frame := mustTestJSON(t, map[string]any{
		"method": "turn/completed",
		"params": map[string]any{
			"threadId": "thread", "turn": map[string]any{
				"id": "turn", "status": "completed", "error": nil,
				"items": []any{map[string]any{"type": "agentMessage", "text": report}},
			},
		},
	})
	parser := newAppServerParser(discardWriteCloser{io.Discard}, Opts{Native: &NativeControls{}})
	parser.threadID, parser.turnID, parser.s.Session = "thread", "turn", "thread"
	sanitized := sanitizeCodexAppServerLine(frame)
	parser.feed(sanitized)
	summary := parser.summary()
	if parser.err != nil || summary.Session != "thread" || summary.Result == nil || summary.Result.IsError {
		t.Fatalf("terminal control fields changed: summary=%+v err=%v", summary, parser.err)
	}
	got := summary.Result.Result
	if !strings.HasPrefix(got, "before ") || !strings.HasSuffix(got, " after") || !strings.Contains(got, "sha256=") || !strings.Contains(got, "bytes=22") || strings.Contains(got, encoded) {
		t.Fatalf("embedded image was not span-redacted: %q", got)
	}
}

func TestCodexAppServerMalformedAndUnsupportedEmbeddedImagesKeepFrame(t *testing.T) {
	for _, value := range []string{
		"before data:image/png;base64,PRIVATE after",
		"before data:image/svg+xml;base64,U0VDUkVU after",
	} {
		line := mustTestJSON(t, map[string]any{"method": "notice", "params": map[string]any{"text": value, "control": "kept"}})
		sanitized := sanitizeCodexAppServerLine(line)
		var frame struct {
			Method string `json:"method"`
			Params struct {
				Text, Control string
			} `json:"params"`
		}
		if err := json.Unmarshal(sanitized, &frame); err != nil || frame.Method != "notice" || frame.Params.Control != "kept" || !strings.HasPrefix(frame.Params.Text, "before ") || !strings.HasSuffix(frame.Params.Text, " after") || !strings.Contains(frame.Params.Text, "[image-data-omitted]") {
			t.Fatalf("embedded malformed URL dropped frame: %s err=%v", sanitized, err)
		}
	}
}

func TestCodexAppServerLargeAndOversizedImageLinesUseSafeBoundedOutput(t *testing.T) {
	secret := bytes.Repeat([]byte("large-private-image"), 1<<16)
	line := mustTestJSON(t, map[string]any{
		"method": "item/started",
		"params": map[string]any{
			"item": map[string]any{"type": "userMessage", "content": []any{map[string]any{"type": "image", "url": "data:image/webp;base64," + base64.StdEncoding.EncodeToString(secret)}}},
		},
	})
	parser := newAppServerParser(discardWriteCloser{io.Discard}, Opts{Native: &NativeControls{}})
	w := newActivityWriter(parser)
	tap := &recordingTap{}
	w.tap = tap
	var observed []string
	w.onLine = func(line []byte) error { observed = append(observed, string(line)); return nil }
	for offset := 0; offset < len(line); offset += 4093 {
		w.Write(line[offset:min(offset+4093, len(line))])
	}
	w.Write([]byte("\n"))
	if len(observed) != 1 || len(observed[0]) >= maxCallbackLine || strings.Contains(observed[0], base64.StdEncoding.EncodeToString(secret)) || len(w.raw) >= maxRawKept {
		t.Fatalf("large image line was not reduced before retention: observed=%d raw=%d", len(observed), len(w.raw))
	}

	oversized := newActivityWriter(newAppServerParser(discardWriteCloser{io.Discard}, Opts{Native: &NativeControls{}}))
	oversized.lineLimit = 64
	oversized.callbackTail = true
	overTap := &recordingTap{}
	oversized.tap = overTap
	var overObserved []string
	oversized.onLine = func(line []byte) error { overObserved = append(overObserved, string(line)); return nil }
	oversized.Write(line)
	if err := oversized.finish(); err != nil {
		t.Fatal(err)
	}
	if len(overObserved) != 1 || !strings.Contains(overObserved[0], "line_too_large") || strings.Contains(oversized.String(), "data:image") || len(overTap.skipped) != 1 {
		t.Fatalf("oversized line did not become safe omission: observer=%q raw=%q skipped=%q", overObserved, oversized.String(), overTap.skipped)
	}
}

func mustTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
