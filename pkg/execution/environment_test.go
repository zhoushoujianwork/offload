package execution

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnvironmentUsesSeparateClaudeConfiguration(t *testing.T) {
	env, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(env.Root) })
	source := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", source)
	t.Setenv("OFFLOAD_MAX_WORKERS", "-1")
	if err := os.WriteFile(filepath.Join(source, "settings.json"), []byte(`{"ambient":"must-not-copy"}`), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(source, "claude")
	marker := filepath.Join(source, "observed")
	script := `#!/bin/sh
printf '%s' "$CLAUDE_CONFIG_DIR" > '` + marker + `'
while [ "$#" -gt 0 ]; do
  if [ "$1" = '--append-system-prompt-file' ]; then cat "$2" > '` + marker + `.instructions'; break; fi
  shift
done
cat >/dev/null
printf '%s\n' '{"type":"system","subtype":"init","session_id":"claude-isolated","model":"fixture"}' '{"type":"result","subtype":"success","is_error":false,"session_id":"claude-isolated","result":"CLAUDE_ISOLATED_OK"}'
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CLAUDE", binary)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	client, err := New(Config{ProfileName: "claude-fixture", Profile: Profile{Backend: "claude", Model: "fixture", BaseURL: server.URL, Token: "fixture-token"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Run(context.Background(), Request{EnvironmentDirectory: env.Root, Directory: env.Workspace, Task: "isolated task", PresetDocuments: []PresetDocument{{Name: "AGENTS.md", Content: []byte("CLAUDE_PRESET_4317")}}, Native: &NativeControls{CallerContext: true, PermissionMode: "bypassPermissions"}})
	if err != nil || result.Report != "CLAUDE_ISOLATED_OK" {
		t.Fatalf("status=%s err=%v", result.Status, err)
	}
	observed, _ := os.ReadFile(marker)
	if string(observed) != filepath.Join(env.Root, "claude") {
		t.Fatal("Claude reused ambient configuration")
	}
	instructions, _ := os.ReadFile(marker + ".instructions")
	if !strings.Contains(string(instructions), "CLAUDE_PRESET_4317") {
		t.Fatal("Claude native system instructions missing preset")
	}
	if _, err := os.Stat(filepath.Join(env.Root, "claude/settings.json")); !os.IsNotExist(err) {
		t.Fatal("ambient settings copied")
	}
}

func TestEnvironmentUsesPrivateNativeHomeWithoutAmbientVariables(t *testing.T) {
	env, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(env.Root) })
	source := t.TempDir()
	t.Setenv("CODEX_HOME", source)
	t.Setenv("OFFLOAD_MAX_WORKERS", "-1")
	t.Setenv("BASH_ENV", "ambient-startup-script")
	t.Setenv("UNRELATED_SECRET", "must-not-inherit")
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(`{"auth":"fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(source, "observed")
	binary := filepath.Join(source, "codex")
	script := `#!/bin/sh
printf '%s\n' "$HOME" "$CODEX_HOME" "$XDG_CONFIG_HOME" "${BASH_ENV-unset}" "${UNRELATED_SECRET-unset}" > '` + marker + `'
while [ "$#" -gt 0 ]; do
  if [ "$1" = '--profile' ]; then cat "$CODEX_HOME/$2.config.toml" > '` + marker + `.instructions'; break; fi
  shift
done
cat >/dev/null
printf '%s\n' '{"type":"thread.started","thread_id":"isolated-session"}' '{"type":"item.completed","item":{"id":"m","type":"agent_message","text":"ISOLATED_OK"}}' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CODEX", binary)
	client, err := New(Config{ProfileName: "fixture", Profile: Profile{Backend: "codex", Model: "fixture"}})
	if err != nil {
		t.Fatal(err)
	}
	timeout := time.Minute
	result, err := client.Run(context.Background(), Request{EnvironmentDirectory: env.Root, Directory: env.Workspace, Task: "isolated task", Timeout: &timeout, PresetDocuments: []PresetDocument{{Name: "SECURITY.md", Content: []byte("CODEX_PRESET_2763")}}, Native: &NativeControls{CallerContext: true, PermissionMode: "bypassPermissions"}})
	if err != nil || result.Report != "ISOLATED_OK" || result.EnvironmentDirectory != env.Root {
		t.Fatalf("status=%s report=%s err=%v", result.Status, result.Report, err)
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	expected := strings.Join([]string{filepath.Join(env.Root, "home"), filepath.Join(env.Root, "codex"), filepath.Join(env.Root, "config"), "unset", "unset", ""}, "\n")
	if string(raw) != expected {
		t.Fatalf("unexpected environment: %q", raw)
	}
	instructions, _ := os.ReadFile(marker + ".instructions")
	if !strings.Contains(string(instructions), "developer_instructions") || !strings.Contains(string(instructions), "CODEX_PRESET_2763") {
		t.Fatal("Codex native developer instructions missing preset")
	}
	var record struct {
		CodexHome   string `json:"codex_home"`
		Environment string `json:"environment_directory"`
	}
	if json.Unmarshal(result.RawRecord, &record) != nil || record.CodexHome != filepath.Join(env.Root, "codex") || record.Environment != env.Root {
		t.Fatal("isolated runtime provenance missing")
	}
	if err := os.WriteFile(filepath.Join(env.Root, "codex", "config.toml"), []byte("developer_instructions = 'foreign policy'"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	_, err = client.Run(context.Background(), Request{EnvironmentDirectory: env.Root, Directory: env.Workspace, Task: "must not dispatch", Instructions: []byte("host instructions"), Native: &NativeControls{CallerContext: true, PermissionMode: "bypassPermissions"}})
	if err == nil {
		t.Fatalf("foreign base configuration accepted: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("native executable started despite foreign base configuration")
	}
}

func TestEnvironmentLiveCodexResume(t *testing.T) {
	if os.Getenv("OFFLOAD_ISOLATED_LIVE") != "1" {
		t.Skip("explicit native acceptance only")
	}
	env, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(env.Root) })
	home, _ := os.UserHomeDir()
	cfg, err := LoadConfig(filepath.Join(home, ".config/offload/config.json"), "codex-cqmuhqa4xreuz")
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	timeout := 2 * time.Minute
	req := Request{EnvironmentDirectory: env.Root, Directory: env.Workspace, Task: "Validation only. Do not use tools or change files. Remember marker EMPTY_ENV_MAPLE_6382 and reply exactly EMPTY_ENV_MAPLE_6382.", Timeout: &timeout}
	first, err := client.Run(context.Background(), req)
	if err != nil || first.ObservedModel != "gpt-5.6-sol" || !strings.Contains(first.Report, "EMPTY_ENV_MAPLE_6382") {
		t.Fatalf("status=%s model=%s report=%s err=%v", first.Status, first.ObservedModel, first.Report, err)
	}
	req.NativeSessionID = first.NativeSessionID
	req.Task = "Without using tools, repeat the marker from the previous turn."
	next, err := client.Run(context.Background(), req)
	if err != nil || next.NativeSessionID != first.NativeSessionID || !strings.Contains(next.Report, "EMPTY_ENV_MAPLE_6382") {
		t.Fatalf("resume status=%s report=%s err=%v", next.Status, next.Report, err)
	}
	t.Logf("model=%s isolated native session resumed=%s", next.ObservedModel, next.NativeSessionID)
}
