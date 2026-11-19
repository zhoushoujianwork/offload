package worker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type imageRecordingCapture struct{ lines []string }

func (*imageRecordingCapture) AddSecrets(...string)        {}
func (*imageRecordingCapture) BeginAttempt(string, string) {}
func (c *imageRecordingCapture) Line(line []byte) {
	c.lines = append(c.lines, string(line))
}
func (*imageRecordingCapture) Skipped(string, int) {}

func TestCodexAppServerTurnCarriesOrderedImagesOnFreshAndResume(t *testing.T) {
	images := []NativeImage{
		{Name: "first \"label\".png", MediaType: "image/png", Data: []byte{0, 1, 2, 3}},
		{MediaType: "image/webp", Data: []byte("exact second image")},
	}
	for _, resume := range []bool{false, true} {
		name := "fresh"
		if resume {
			name = "resume"
		}
		t.Run(name, func(t *testing.T) {
			bin, log := appServerFixture(t, "", completedAppServerTail)
			o := appServerOpts(t, t.TempDir())
			o.Native.SkillRoots, o.Native.JSONSchema, o.Native.Images = nil, "", images
			if resume {
				o.Resume = "existing-thread"
			}
			a := runAttempt(context.Background(), bin, o, "", "", o.Timeout, nil, hooks{})
			if a.runErr != nil || a.s.Result == nil {
				t.Fatalf("%+v", a)
			}
			input := capturedTurnInput(t, log)
			if len(input) != 4 || input[0].Type != "text" || input[0].Text != o.Task || input[1].Type != "text" || input[1].Text != `Image name: "first \"label\".png"` {
				t.Fatalf("unexpected ordered input: %+v", input)
			}
			for index, position := range []int{2, 3} {
				got := input[position]
				want := images[index]
				prefix := "data:" + want.MediaType + ";base64,"
				if got.Type != "image" || !strings.HasPrefix(got.URL, prefix) || strings.Contains(got.URL, "http://") || strings.Contains(got.URL, "https://") {
					t.Fatalf("image %d was not inline: %+v", index, got)
				}
				decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got.URL, prefix))
				if err != nil || string(decoded) != string(want.Data) {
					t.Fatalf("image %d bytes changed: %x %v", index, decoded, err)
				}
			}
		})
	}
}

type capturedUserInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
	URL  string `json:"url"`
}

func capturedTurnInput(t *testing.T, path string) []capturedUserInput {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		var request struct {
			Method string `json:"method"`
			Params struct {
				Input []capturedUserInput `json:"input"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &request) == nil && request.Method == "turn/start" {
			return request.Params.Input
		}
	}
	t.Fatalf("turn/start missing from %s", raw)
	return nil
}

func TestCodexImageValidationPreservesBoundsAndLegacyRejection(t *testing.T) {
	valid := NativeImage{Name: "image.png", MediaType: "image/png", Data: []byte("x")}
	base := NativeControls{CallerContext: true, CodexAppServer: true, PermissionMode: "bypassPermissions"}
	for _, tc := range []struct {
		name   string
		change func(*NativeControls)
	}{
		{name: "too many", change: func(n *NativeControls) { n.Images = []NativeImage{valid, valid, valid, valid, valid} }},
		{name: "too large", change: func(n *NativeControls) {
			n.Images = []NativeImage{{MediaType: "image/png", Data: make([]byte, (5<<20)+1)}}
		}},
		{name: "unsupported MIME", change: func(n *NativeControls) { n.Images = []NativeImage{{MediaType: "image/svg+xml", Data: []byte("x")}} }},
		{name: "invalid name", change: func(n *NativeControls) {
			n.Images = []NativeImage{{Name: string([]byte{0xff}), MediaType: "image/png", Data: []byte("x")}}
		}},
		{name: "legacy", change: func(n *NativeControls) { n.CodexAppServer = false; n.Images = []NativeImage{valid} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := base
			tc.change(&n)
			if err := n.Validate("codex", "", false); err == nil {
				t.Fatal("invalid image controls accepted")
			}
		})
	}
}

func TestCodexAppServerMaximalValidImagesFitOutboundFrame(t *testing.T) {
	data := bytes.Repeat([]byte{0xa5}, 5<<20)
	images := make([]NativeImage, 4)
	for i := range images {
		images[i] = NativeImage{MediaType: "image/png", Data: data}
	}
	o := Opts{Task: "maximal images", Dir: "/workspace", Native: &NativeControls{Images: images}}
	params := appServerTurnParams(o, "thread-max", []string{"/workspace"})
	var output bytes.Buffer
	parser := newAppServerParser(discardWriteCloser{&output}, o)
	if err := parser.write(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "turn/start", "params": params}); err != nil {
		t.Fatal(err)
	}
	if output.Len() <= maxLineKept || output.Len() > codexAppServerMaxFrame+1 {
		t.Fatalf("serialized frame size=%d, want (%d,%d]", output.Len(), maxLineKept, codexAppServerMaxFrame+1)
	}
	var request struct {
		Method string `json:"method"`
		Params struct {
			Input []capturedUserInput `json:"input"`
		} `json:"params"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &request); err != nil || request.Method != "turn/start" || len(request.Params.Input) != 5 {
		t.Fatalf("invalid maximal turn request: method=%q input=%d err=%v", request.Method, len(request.Params.Input), err)
	}
	for index, item := range request.Params.Input[1:] {
		payload := strings.TrimPrefix(item.URL, "data:image/png;base64,")
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if item.Type != "image" || err != nil || !bytes.Equal(decoded, data) {
			t.Fatalf("image %d did not round trip: type=%q bytes=%d err=%v", index, item.Type, len(decoded), err)
		}
	}
}

func TestCodexAppServerRedactsImageFromRecordObserverAndTranscript(t *testing.T) {
	root := t.TempDir()
	skill := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(skill, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	secret := []byte("history-private-image-bytes")
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(secret)
	nested, err := json.Marshal(map[string]any{"type": "image", "url": dataURL})
	if err != nil {
		t.Fatal(err)
	}
	turnStarted := mustTestJSON(t, map[string]any{
		"method": "turn/started",
		"params": map[string]any{"threadId": "thread-native", "turn": map[string]any{"id": "turn-native"}},
	})
	started := mustTestJSON(t, map[string]any{
		"method": "item/started",
		"params": map[string]any{
			"threadId": "thread-native", "turnId": "turn-native",
			"item": map[string]any{"id": "user", "type": "userMessage", "content": []any{map[string]any{"type": "image", "url": dataURL}}},
		},
	})
	completed := mustTestJSON(t, map[string]any{
		"method": "item/completed",
		"params": map[string]any{
			"threadId": "thread-native", "turnId": "turn-native",
			"item": map[string]any{"id": "answer", "type": "agentMessage", "text": string(nested)},
		},
	})
	turnCompleted := mustTestJSON(t, map[string]any{
		"method": "turn/completed",
		"params": map[string]any{
			"threadId": "thread-native",
			"turn":     map[string]any{"id": "turn-native", "status": "completed", "items": []any{}, "error": nil},
		},
	})
	tail := fmt.Sprintf("printf '%%s\\n' '%s'\nprintf '%%s\\n' '%s'\nprintf '%%s\\n' '%s'\nprintf '%%s\\n' '%s'", turnStarted, started, completed, turnCompleted)
	bin, _ := appServerFixture(t, skill, tail)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(`{"token":"fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", source)
	t.Setenv("OFFLOAD_CODEX", bin)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	o := appServerOpts(t, root)
	o.CodexHome = ""
	o.Native.JSONSchema = ""
	o.MaxWorkers = 1
	transcript := &imageRecordingCapture{}
	o.Transcript = transcript
	var observed []string
	o.OnLine = func(line []byte) error {
		observed = append(observed, string(line))
		return nil
	}
	record, err := Run(context.Background(), o)
	if err != nil || record.Status != "ok" || record.Session != "thread-native" {
		t.Fatalf("record=%+v err=%v observed=%q transcript=%q", record, err, observed, transcript.lines)
	}
	recordJSON, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	encodedSecret := base64.StdEncoding.EncodeToString(secret)
	for name, output := range map[string]string{
		"history record": string(recordJSON),
		"observer":       strings.Join(observed, "\n"),
		"transcript":     strings.Join(transcript.lines, "\n"),
	} {
		if strings.Contains(output, encodedSecret) || strings.Contains(output, string(secret)) {
			t.Fatalf("%s leaked image payload: %s", name, output)
		}
		if !strings.Contains(output, "sha256=") || !strings.Contains(output, "bytes=") {
			t.Fatalf("%s lost redaction evidence: %s", name, output)
		}
	}
}
