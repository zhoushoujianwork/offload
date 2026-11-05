package execution

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/transcript"
)

const envDumpClaude = `#!/bin/sh
env > "$ENV_DUMP"
cat >/dev/null
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s","model":"fixture"}' '{"type":"result","subtype":"success","is_error":false,"session_id":"s","result":"OK"}'
`

const envDumpCodex = `#!/bin/sh
env > "$ENV_DUMP"
cat >/dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"t"}' '{"type":"item.completed","item":{"id":"m","type":"agent_message","text":"OK"}}' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`

func envFixture(t *testing.T, backend string) *Client {
	t.Helper()
	t.Setenv("OFFLOAD_MAX_WORKERS", "-1")
	bin := filepath.Join(t.TempDir(), "fake")
	script, name := envDumpClaude, "OFFLOAD_CLAUDE"
	profile := Profile{Backend: "claude", Model: "fixture"}
	if backend == "codex" {
		script, name = envDumpCodex, "OFFLOAD_CODEX"
		profile = Profile{Backend: "codex", Model: "fixture"}
	} else {
		server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		t.Cleanup(server.Close)
		profile.BaseURL = server.URL
		profile.Token = "profile-token-value"
	}
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(name, bin)
	c, err := New(Config{ProfileName: "fixture", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func runEnv(t *testing.T, c *Client, dump string, pe *ProcessEnvironment) (map[string]string, error) {
	t.Helper()
	timeout := time.Minute
	_, err := c.Run(context.Background(), Request{Directory: t.TempDir(), Task: "t", Timeout: &timeout, ProcessEnvironment: pe,
		Native: nil})
	if err != nil {
		return nil, err
	}
	raw, rerr := os.ReadFile(dump)
	if rerr != nil {
		return nil, rerr
	}
	out := map[string]string{}
	for _, l := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			out[k] = v
		}
	}
	return out, nil
}

func TestProcessEnvironmentSetOverrideUnsetBothBackends(t *testing.T) {
	for _, backend := range []string{"claude", "codex"} {
		t.Run(backend, func(t *testing.T) {
			c := envFixture(t, backend)
			dump := filepath.Join(t.TempDir(), "env")
			t.Setenv("ENV_DUMP", dump)
			t.Setenv("PE_OVERRIDE", "old")
			t.Setenv("PE_REMOVE", "gone")
			t.Setenv("PE_KEEP", "kept")
			got, err := runEnv(t, c, dump, &ProcessEnvironment{
				Set:   map[string]string{"PE_ADD": "added-value-1", "PE_OVERRIDE": "new"},
				Unset: []string{"PE_REMOVE"}})
			if err != nil {
				t.Fatal(err)
			}
			if got["PE_ADD"] != "added-value-1" || got["PE_OVERRIDE"] != "new" || got["PE_KEEP"] != "kept" {
				t.Fatalf("unexpected env: add=%q override=%q keep=%q", got["PE_ADD"], got["PE_OVERRIDE"], got["PE_KEEP"])
			}
			if _, ok := got["PE_REMOVE"]; ok {
				t.Fatal("unset variable reached the child")
			}
			// Nil leaves the environment unchanged.
			got, err = runEnv(t, c, dump, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got["PE_OVERRIDE"] != "old" || got["PE_REMOVE"] != "gone" || got["PE_KEEP"] != "kept" {
				t.Fatal("nil ProcessEnvironment changed the environment")
			}
			if _, ok := got["PE_ADD"]; ok {
				t.Fatal("previous run leaked into the next")
			}
			if backend == "claude" && got["ANTHROPIC_AUTH_TOKEN"] != "profile-token-value" {
				t.Fatal("Offload credential not injected")
			}
		})
	}
}

func TestProcessEnvironmentRejectsOffloadOwnedVariables(t *testing.T) {
	for _, backend := range []string{"claude", "codex"} {
		c := envFixture(t, backend)
		dump := filepath.Join(t.TempDir(), "env")
		t.Setenv("ENV_DUMP", dump)
		for _, name := range []string{"ANTHROPIC_AUTH_TOKEN", "anthropic_base_url", "CODEX_HOME", "OPENAI_API_KEY", "CLAUDE_CONFIG_DIR", "BAD=NAME", ""} {
			for _, pe := range []*ProcessEnvironment{{Set: map[string]string{name: "x"}}, {Unset: []string{name}}} {
				if _, err := runEnv(t, c, dump, pe); err == nil {
					t.Fatalf("%s accepted %q", backend, name)
				}
			}
		}
		if _, err := os.Stat(dump); err == nil {
			t.Fatal("rejected request launched a backend")
		}
	}
}

func TestProcessEnvironmentConcurrentRunsAreIndependent(t *testing.T) {
	for _, backend := range []string{"claude", "codex"} {
		t.Run(backend, func(t *testing.T) {
			t.Setenv("OFFLOAD_MAX_WORKERS", "-1")
			bin := filepath.Join(t.TempDir(), "fake")
			script, name := strings.Replace(envDumpClaude, `"$ENV_DUMP"`, `"$PE_DUMP"`, 1), "OFFLOAD_CLAUDE"
			if backend == "codex" {
				script, name = strings.Replace(envDumpCodex, `"$ENV_DUMP"`, `"$PE_DUMP"`, 1), "OFFLOAD_CODEX"
			}
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv(name, bin)
			profile := Profile{Backend: backend, Model: "fixture"}
			if backend == "claude" {
				server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				defer server.Close()
				profile.BaseURL, profile.Token = server.URL, "profile-token-value"
			}
			c, err := New(Config{ProfileName: "fixture", Profile: profile})
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			var wg sync.WaitGroup
			errs := make(chan error, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					dump := filepath.Join(dir, fmt.Sprintf("env-%d", i))
					timeout := time.Minute
					_, err := c.Run(context.Background(), Request{Directory: t.TempDir(), Task: "t", Timeout: &timeout,
						ProcessEnvironment: &ProcessEnvironment{Set: map[string]string{"PE_DUMP": dump, "PE_ID": fmt.Sprint("run-", i)}}})
					if err != nil {
						errs <- err
						return
					}
					raw, _ := os.ReadFile(dump)
					if !strings.Contains(string(raw), fmt.Sprintf("PE_ID=run-%d\n", i)) || strings.Count(string(raw), "PE_ID=") != 1 {
						errs <- fmt.Errorf("run %d saw another run's environment", i)
					}
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
			if os.Getenv("PE_ID") != "" {
				t.Fatal("process environment was modified")
			}
		})
	}
}

type recordingCapture struct {
	mu    sync.Mutex
	added []string
	red   *transcript.Redactor
	text  strings.Builder
}

func (r *recordingCapture) AddSecrets(s ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.added = append(r.added, s...)
	if r.red == nil {
		r.red = transcript.NewRedactor()
	}
	r.red.Add(s...)
}
func (r *recordingCapture) BeginAttempt(kind, task string) {}
func (r *recordingCapture) Line(b []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.red == nil {
		r.red = transcript.NewRedactor()
	}
	out, _ := r.red.Apply(string(b))
	r.text.WriteString(out)
}
func (r *recordingCapture) Skipped(string, int) {}

// The backend echoes the variable value into its output, so the assertions
// prove redaction rather than mere absence.
func TestProcessEnvironmentValuesAreRedactedFromRecords(t *testing.T) {
	t.Setenv("OFFLOAD_MAX_WORKERS", "-1")
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	const leak = "pe-leak-value-7f3a91"
	bin := filepath.Join(t.TempDir(), "fake")
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s","model":"fixture"}' "{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"session_id\":\"s\",\"result\":\"saw $PE_SECRET\"}"
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CLAUDE", bin)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	c, err := New(Config{ProfileName: "fixture", Profile: Profile{Backend: "claude", Model: "fixture", BaseURL: server.URL, Token: "fixture-token"}})
	if err != nil {
		t.Fatal(err)
	}
	capture := &recordingCapture{}
	var events []string
	timeout := time.Minute
	res, err := c.Run(context.Background(), Request{Directory: t.TempDir(), Task: "t", Timeout: &timeout, RecordHistory: true, Capture: capture,
		ProcessEnvironment: &ProcessEnvironment{Set: map[string]string{"PE_SECRET": leak}},
		OnEvent:            func(e RawEvent) error { events = append(events, string(e.Data)); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if !res.HistoryRecorded {
		t.Fatalf("history not recorded: %v", res.HistoryError)
	}
	hist, err := os.ReadFile(filepath.Join(state, "offload", "runs.jsonl"))
	if err != nil || len(hist) == 0 {
		t.Fatalf("history unreadable: %v", err)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.text.Len() == 0 {
		t.Fatal("capture received no transcript lines")
	}
	registered := false
	for _, s := range capture.added {
		registered = registered || s == leak
	}
	if !registered {
		t.Fatal("value was not registered for transcript redaction")
	}
	for name, body := range map[string]string{
		"events": strings.Join(events, "\n"), "result": string(res.RawRecord) + res.Report + res.NativeStderr,
		"history": string(hist), "capture": capture.text.String()} {
		if strings.Contains(body, leak) {
			t.Fatalf("process environment value leaked into %s", name)
		}
	}
}
