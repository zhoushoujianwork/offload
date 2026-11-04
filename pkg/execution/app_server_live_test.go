package execution

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppServerLiveIsolatedSkillAndResume(t *testing.T) {
	if os.Getenv("OFFLOAD_APP_SERVER_LIVE") != "1" {
		t.Skip("explicit native acceptance only")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".config/offload/config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var selected struct {
		Default string `json:"default"`
	}
	if err := json.Unmarshal(raw, &selected); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, selected.Default)
	if err != nil || cfg.Profile.Backend != "codex" {
		t.Fatalf("configured default is not available as Codex: %v", err)
	}
	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	environment, err := NewEnvironmentAt(filepath.Join(parent, "owned"))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "approved-skills")
	skill := filepath.Join(root, "acceptance-ops-marker")
	if err := os.MkdirAll(skill, 0700); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: acceptance-ops-marker\ndescription: Use when asked for the OPS_NATIVE_ACCEPTANCE validation marker.\n---\n\nThe validation marker is OPS_SKILL_PINE_4261. Reply with it when requested. Do not use network tools or modify files.\n"
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	timeout := 90 * time.Second
	request := Request{
		EnvironmentDirectory: environment.Root, Directory: environment.Workspace,
		Task:         "OPS_NATIVE_ACCEPTANCE: use the acceptance-ops-marker skill and reply with its validation marker. Do not use the network or change files.",
		Instructions: []byte("This is a bounded native Skills acceptance check. Use only the supplied validation skill; do not delegate or execute operational actions."),
		Native:       &NativeControls{CallerContext: true, CodexAppServer: true, PermissionMode: "bypassPermissions", SkillRoots: []string{skill}},
		Timeout:      &timeout,
	}
	first, err := client.Run(context.Background(), request)
	if err != nil || first.Status != "ok" || first.NativeSessionID == "" || first.NativeProtocol != "codex-app-server" || !strings.Contains(first.Report, "OPS_SKILL_PINE_4261") {
		t.Fatalf("first status=%s model=%s report=%q error=%v detail=%s stderr=%s", first.Status, first.ObservedModel, first.Report, err, first.Error, first.NativeStderr)
	}
	if first.ObservedModel == "" || cfg.Profile.Model != "" && first.ObservedModel != cfg.Profile.Model {
		t.Fatalf("unexpected observed model: requested=%s observed=%s", cfg.Profile.Model, first.ObservedModel)
	}
	request.NativeSessionID = first.NativeSessionID
	request.Task = "Repeat the validation marker from the previous turn and append CONTINUATION_OK. Do not use the network or change files."
	next, err := client.Run(context.Background(), request)
	if err != nil || next.NativeSessionID != first.NativeSessionID || !strings.Contains(next.Report, "OPS_SKILL_PINE_4261") || !strings.Contains(next.Report, "CONTINUATION_OK") {
		t.Fatalf("resume status=%s model=%s report=%q error=%v detail=%s stderr=%s", next.Status, next.ObservedModel, next.Report, err, next.Error, next.NativeStderr)
	}
	t.Logf("profile=%s model=%s protocol=%s resumed_session=%s", next.Profile, next.ObservedModel, next.NativeProtocol, next.NativeSessionID)
}
