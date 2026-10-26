package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
)

const (
	fakeSecretStderr = "STDERR-SECRET-xyz"
	fakeToken        = "tok-secret-123"
)

// fakeClaude writes an executable that stands in for the claude CLI. A run without --tools is the
// first attempt: it reports a session and a tool call, then dies without a result, which is
// deterministic where a wall-clock timeout is not. A run with --tools is the salvage step, whose
// behaviour FAKE_SALVAGE_MODE selects.
func fakeClaude(t *testing.T) string {
	t.Helper()
	script := `#!/bin/sh
case " $* " in *" --tools "*) salvage=1;; esac
if [ -z "$salvage" ]; then
  printf '%s\n' '{"type":"system","subtype":"init","session_id":"s-stop"}'
  printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"read the retry loop"},{"type":"tool_use","name":"Read","input":{"file_path":"/a/b.go"}}]}}'
  exit 1
fi
echo "` + fakeSecretStderr + ` $ANTHROPIC_AUTH_TOKEN" >&2
case "$FAKE_SALVAGE_MODE" in
  ok) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"FINDING at a.go:3","session_id":"s-stop","total_cost_usd":0.02}';;
  exit_with_report) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"APPARENT REPORT","session_id":"s-stop"}'; exit 3;;
  error_result) printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom","session_id":"s-stop"}';;
  empty) printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"  ","session_id":"s-stop"}';;
  noresult) printf '%s\n' '{"type":"system","subtype":"init","session_id":"s-stop"}';;
  hang) printf '%s\n' '{"type":"system","subtype":"init","session_id":"s-stop"}'; exec sleep 30;;
esac
`
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testOpts(t *testing.T) Opts {
	t.Helper()
	return Opts{ProfileName: "p", Model: "m", Dir: t.TempDir(), Task: "inspect", Timeout: 30 * time.Second, // the fake ends runs itself; a short limit only races process start-up
		Profile: config.Profile{BaseURL: "https://gw.example/anthropic", Model: "m", Token: fakeToken}}
}

func mockPreflight(t *testing.T) {
	t.Helper()
	orig := Preflight
	Preflight = func(string) error { return nil }
	t.Cleanup(func() { Preflight = orig })
}

func TestSalvageOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, mode, wantErr, wantReport string
		idle, timeout                   time.Duration
		cancelled                       bool
	}{
		{name: "success", mode: "ok", wantReport: "FINDING at a.go:3"},
		{name: "process failed with apparent report", mode: "exit_with_report", wantErr: "process failed"},
		{name: "structured error", mode: "error_result", wantErr: "returned an error (error_during_execution)"},
		{name: "empty report", mode: "empty", wantErr: "empty report"},
		{name: "no result", mode: "noresult", wantErr: "without a result"},
		{name: "timeout", mode: "hang", timeout: 400 * time.Millisecond, wantErr: "timed out after"},
		{name: "idle", mode: "hang", idle: 400 * time.Millisecond, wantErr: "no output for"},
		{name: "cancelled", mode: "ok", cancelled: true, wantErr: "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("FAKE_SALVAGE_MODE", tc.mode)
			origIdle, origTimeout := IdleLimit, SalvageTimeout
			t.Cleanup(func() { IdleLimit, SalvageTimeout = origIdle, origTimeout })
			if tc.idle > 0 {
				IdleLimit = tc.idle
			}
			if tc.timeout > 0 {
				SalvageTimeout = tc.timeout
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			report, cost, err := salvage(ctx, fakeClaude(t), testOpts(t), filepath.Join(t.TempDir(), "prompt"), fakeToken, "s-stop", nil)
			if tc.wantErr == "" {
				if err != nil || report != tc.wantReport || cost == nil || *cost != 0.02 {
					t.Fatalf("report=%q cost=%v err=%v", report, cost, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || report != "" || cost != nil {
				t.Fatalf("want error %q, got report=%q cost=%v err=%v", tc.wantErr, report, cost, err)
			}
			for _, secret := range []string{fakeSecretStderr, fakeToken, "APPARENT REPORT"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaks %q: %v", secret, err)
				}
			}
		})
	}
}

// Run keeps the original failure, session, exit code and progress when the report step fails, and
// says why. It must not look like a salvaged run.
func TestRunFailedSalvageKeepsOriginalFailureAndSaysWhy(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("OFFLOAD_CLAUDE", fakeClaude(t))
	t.Setenv("FAKE_SALVAGE_MODE", "exit_with_report")
	mockPreflight(t)

	rec, err := Run(context.Background(), testOpts(t))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != "error" || rec.ExitCode == nil || *rec.ExitCode != 1 || rec.Session != "s-stop" || rec.Turns != 1 || rec.Salvaged {
		t.Fatalf("original failure lost: %+v", rec)
	}
	for _, want := range []string{"worker ended without a result", "salvage failed: the report step process failed", "offload -r s-stop"} {
		if !strings.Contains(rec.Error, want) {
			t.Errorf("Error missing %q: %s", want, rec.Error)
		}
	}
	for _, want := range []string{"salvage failed", "offload -r s-stop", "read the retry loop", "Read "} {
		if !strings.Contains(rec.Result, want) {
			t.Errorf("Result missing %q:\n%s", want, rec.Result)
		}
	}
	if strings.Contains(rec.Result, "SALVAGED REPORT") || strings.Contains(rec.Result, "APPARENT REPORT") {
		t.Errorf("failed salvage presented as a report:\n%s", rec.Result)
	}
	if strings.Count(rec.Error, "offload -r s-stop") != 1 || !strings.HasSuffix(rec.Error, `"`) {
		t.Errorf("resume hint must appear once, at the end of Error: %s", rec.Error)
	}
	for _, secret := range []string{fakeSecretStderr, fakeToken} {
		if strings.Contains(rec.Result+rec.Error, secret) {
			t.Errorf("record leaks %q", secret)
		}
	}
}

func TestRunSuccessfulSalvageIsStillMarked(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("OFFLOAD_CLAUDE", fakeClaude(t))
	t.Setenv("FAKE_SALVAGE_MODE", "ok")
	mockPreflight(t)

	rec, err := Run(context.Background(), testOpts(t))
	if err != nil || !rec.Salvaged || rec.Status != "error" || !strings.Contains(rec.Result, "[SALVAGED REPORT") ||
		!strings.Contains(rec.Result, "FINDING at a.go:3") || strings.Contains(rec.Error, "salvage failed") || rec.CostUSD == nil {
		t.Fatalf("err=%v rec=%+v", err, rec)
	}
}

func TestRunNoSalvageAddsNoSalvageDiagnostic(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("OFFLOAD_CLAUDE", fakeClaude(t))
	t.Setenv("FAKE_SALVAGE_MODE", "ok")
	mockPreflight(t)

	o := testOpts(t)
	o.NoSalvage = true
	rec, _ := Run(context.Background(), o)
	if rec.Salvaged || strings.Contains(rec.Error, "salvage") || !strings.Contains(rec.Result, "offload -r s-stop") {
		t.Fatalf("%+v", rec)
	}
}
