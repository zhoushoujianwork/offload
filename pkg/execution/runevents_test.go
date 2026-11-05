package execution_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/cli"
	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/worker"
	"github.com/zhoushoujianwork/offload/pkg/execution"
)

const (
	eventSecret  = "injected-test-secret-7731"
	eventRawText = "RAW_MODEL_TEXT_9082"
)

type countingCapture struct{ lines int }

func (*countingCapture) AddSecrets(...string)     {}
func (*countingCapture) BeginAttempt(_, _ string) {}
func (c *countingCapture) Line([]byte)            { c.lines++ }
func (*countingCapture) Skipped(_ string, _ int)  {}

func runEventFixture(t *testing.T) (work, url string) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "cfg"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	t.Setenv("OFFLOAD_MAX_WORKERS", "-1")
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(server.Close)
	fake := filepath.Join(tmp, "claude")
	script := "#!/bin/sh\ncat >/dev/null\n" +
		"printf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"native-sess-1\",\"model\":\"fixture\"}'\n" +
		"printf '%s\\n' '{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"" + eventRawText + " " + eventSecret + "\"}]}}'\n" +
		"printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"session_id\":\"native-sess-1\",\"result\":\"" + eventRawText + "\"}'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CLAUDE", fake)
	work = filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	work, _ = filepath.EvalSymlinks(work)
	return work, server.URL
}

func normalized(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "timestamp")
	delete(m, "run_id")
	return m
}

func TestRunEventsMatchEventsFileAndAreSanitized(t *testing.T) {
	work, url := runEventFixture(t)
	client, err := execution.New(execution.Config{ProfileName: "fixture", Profile: execution.Profile{
		Backend: "claude", Model: "fixture", BaseURL: url, Token: eventSecret}})
	if err != nil {
		t.Fatal(err)
	}
	var got []execution.RunEvent
	var raw int
	capture := &countingCapture{}
	result, err := client.Run(context.Background(), execution.Request{Directory: work, Task: "do work " + eventSecret,
		OnRunEvent: func(e execution.RunEvent) error { got = append(got, e); return nil },
		OnEvent:    func(execution.RawEvent) error { raw++; return nil },
		Capture:    capture})
	if err != nil || result.Report != eventRawText {
		t.Fatalf("%v %+v", err, result)
	}
	if raw == 0 || capture.lines == 0 {
		t.Fatalf("OnEvent/Capture not independent: raw=%d capture=%d", raw, capture.lines)
	}
	if len(got) < 3 || got[0].Type != "start" || got[len(got)-1].Type != "final" {
		t.Fatalf("%+v", got)
	}
	for _, e := range got[1 : len(got)-1] {
		if e.Type != "progress" {
			t.Fatalf("%+v", e)
		}
	}
	if got[0].RunID != result.RunID || got[0].Backend != "claude" || got[0].SchemaVersion != 1 {
		t.Fatalf("start: %+v", got[0])
	}
	last := got[len(got)-1]
	if last.Session != "native-sess-1" || last.Status != "ok" || last.ExitCode == nil || *last.ExitCode != 0 {
		t.Fatalf("final: %+v", last)
	}
	encoded, _ := json.Marshal(got)
	for _, leaked := range []string{eventSecret, eventRawText, "do work"} {
		if bytes.Contains(encoded, []byte(leaked)) {
			t.Fatalf("event leaked %q: %s", leaked, encoded)
		}
	}

	// The same fixture through the CLI must write identical events.
	cfg := `{"default":"fixture","profiles":{"fixture":{"base_url":"` + url + `","model":"fixture","token_cmd":"echo cli-token"}}}`
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "events.ndjson")
	var out, errb bytes.Buffer
	orig := worker.Preflight
	worker.Preflight = func(string) error { return nil }
	defer func() { worker.Preflight = orig }()
	if code := cli.Run("test", []string{"-p", "fixture", "-d", work, "-events-file", file, "do work"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("%d %s %s", code, out.String(), errb.String())
	}
	data, _ := os.ReadFile(file)
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != len(got) {
		t.Fatalf("CLI wrote %d events, callback got %d", len(lines), len(got))
	}
	for i, line := range lines {
		want := normalized(t, line)
		have := normalized(t, mustJSON(t, got[i]))
		if !reflect.DeepEqual(want, have) {
			t.Fatalf("event %d differs:\ncli %v\ncb  %v", i, want, have)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRunEventCallbackErrorStopsRun(t *testing.T) {
	work, url := runEventFixture(t)
	client, err := execution.New(execution.Config{ProfileName: "fixture", Profile: execution.Profile{
		Backend: "claude", Model: "fixture", BaseURL: url, Token: eventSecret}})
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("callback refused")
	var calls int
	result, err := client.Run(context.Background(), execution.Request{Directory: work, Task: "x",
		OnRunEvent: func(e execution.RunEvent) error {
			calls++
			if e.Type == "progress" {
				return boom
			}
			return nil
		}})
	var runErr *execution.RunError
	if !errors.As(err, &runErr) || !errors.Is(err, boom) || result.ErrorKind != "events_io" {
		t.Fatalf("err=%v kind=%s", err, result.ErrorKind)
	}
	if calls < 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestRunEventStartErrorPreventsLaunch(t *testing.T) {
	work, url := runEventFixture(t)
	client, err := execution.New(execution.Config{ProfileName: "fixture", Profile: execution.Profile{
		Backend: "claude", Model: "fixture", BaseURL: url, Token: eventSecret}})
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("no start")
	marker := filepath.Join(work, "launched")
	launcher := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CLAUDE", launcher)
	_, err = client.Run(context.Background(), execution.Request{Directory: work, Task: "x",
		OnRunEvent: func(execution.RunEvent) error { return boom }})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("native process launched")
	}
}
