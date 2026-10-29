//go:build unix

package worker

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
)

func waitFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.HasSuffix(string(b), "\n") {
			return strings.TrimSpace(string(b))
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s not written", path)
	return ""
}

// A cancelled run still yields a final record with the resume hint, stops the worker and its
// descendant, and skips the report step as before.
func TestRunCancelStopsWorkerTreeAndKeepsRecord(t *testing.T) {
	stateEnv(t)
	dir := t.TempDir()
	pidFile, salvaged := filepath.Join(dir, "desc"), filepath.Join(dir, "salvage")
	script := `#!/bin/sh
case " $* " in *" --tools "*) echo x > '` + salvaged + `'; exit 0;; esac
printf '%s\n' '{"type":"system","subtype":"init","session_id":"s-cancel"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/a/b.go"}}]}}'
sleep 60 &
echo $! > '` + pidFile + `'
wait
`
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CLAUDE", bin)
	mockPreflight(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		waitFile(t, pidFile)
		cancel()
	}()
	start := time.Now()
	done := make(chan struct{})
	var out struct {
		status, errText, session, result string
		turns                            int
		err                              error
	}
	go func() {
		defer close(done)
		r, err := Run(ctx, testOpts(t))
		out.status, out.errText, out.session, out.result, out.turns, out.err = r.Status, r.Error, r.Session, r.Result, r.Turns, err
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	t.Logf("returned %s after start", time.Since(start))
	if out.err != nil || out.status != "error" || out.session != "s-cancel" || out.turns != 1 ||
		!strings.Contains(out.errText, "offload -r s-cancel") || !strings.Contains(out.result, "offload -r s-cancel") {
		t.Fatalf("%+v", out)
	}
	if _, err := os.Stat(salvaged); err == nil {
		t.Fatal("report step ran after user cancel")
	}
	pid, _ := strconv.Atoi(waitFile(t, pidFile))
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("worker descendant survived cancel")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Cancelling while token_cmd runs ends Run promptly and never starts the worker.
func TestRunCancelDuringTokenCmd(t *testing.T) {
	stateEnv(t)
	dir := t.TempDir()
	started := filepath.Join(dir, "worker")
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho x > '"+started+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CLAUDE", bin)
	mockPreflight(t)
	o := testOpts(t)
	o.Profile = config.Profile{BaseURL: "https://gw.example/anthropic", Model: "m", TokenCmd: "sleep 30; echo tok"}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	rec, err := Run(ctx, o)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Run took %s", d)
	}
	if err == nil || !strings.Contains(err.Error(), "token_cmd cancelled") || rec.Status != "error" || rec.ExitCode == nil || *rec.ExitCode != 2 {
		t.Fatalf("err=%v rec=%+v", err, rec)
	}
	if _, err := os.Stat(started); err == nil {
		t.Fatal("worker started after cancel")
	}
}
