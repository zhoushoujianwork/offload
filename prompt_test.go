package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestMergedWorkerPromptIsPrivateAndCleanedUp(t *testing.T) {
	for _, tc := range []struct {
		name               string
		write              bool
		reply, exit, sleep string
		wantCode           int
	}{
		{"read-only", false, okReply, "", "", 0},
		{"write", true, okReply, "", "", 0},
		{"worker error", false, `{"type":"result","result":"failed","is_error":true}`, "", "", 1},
		{"process error", false, okReply, "7", "", 1},
		{"timeout", false, okReply, "", "5", 124},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work, record := setup(t, tc.reply)
			t.Setenv("TMPDIR", t.TempDir())
			t.Setenv("FAKE_EXIT", tc.exit)
			t.Setenv("FAKE_SLEEP", tc.sleep)
			for _, source := range []struct{ name, content string }{
				{"CLAUDE.md", "CLAUDE_PROJECT_MARKER"}, {"AGENTS.md", "AGENTS_PROJECT_MARKER"},
			} {
				if err := os.WriteFile(filepath.Join(work, source.name), []byte(source.content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			timeout := "3s"
			if tc.name == "timeout" {
				timeout = "2s"
			}
			args := []string{"-json", "-d", work, "-t", timeout}
			if tc.write {
				args = append(args, "-w")
			}
			args = append(args, "inspect instructions")
			code, out, stderr := runCLI(args...)
			if code != tc.wantCode {
				t.Fatalf("code=%d want=%d out=%q err=%q", code, tc.wantCode, out, stderr)
			}
			argv, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(string(argv), "\n")
			if slices.Contains(lines, "--append-system-prompt") || strings.Count(string(argv), "--append-system-prompt-file\n") != 1 {
				t.Fatalf("must pass exactly one file prompt flag: %q", argv)
			}
			for _, text := range []string{"CLAUDE_PROJECT_MARKER", "AGENTS_PROJECT_MARKER", workerPrompt} {
				if strings.Contains(string(argv), text) {
					t.Fatalf("prompt contents exposed in argv: %q", argv)
				}
			}
			prompt, err := os.ReadFile(record + ".prompt")
			if err != nil {
				t.Fatal(err)
			}
			merged := string(prompt)
			ci, ai, wi := strings.Index(merged, "CLAUDE_PROJECT_MARKER"), strings.Index(merged, "AGENTS_PROJECT_MARKER"), strings.Index(merged, workerPrompt)
			if ci < 0 || ai <= ci || wi <= ai || !strings.Contains(merged, filepath.Join(work, "CLAUDE.md")) || !strings.Contains(merged, filepath.Join(work, "AGENTS.md")) {
				t.Fatalf("merged prompt must preserve sources and append worker constraints last: %q", merged)
			}
			readOnly := "Read-only task: do not modify files."
			if tc.write && strings.Contains(merged, readOnly) || !tc.write && !strings.HasSuffix(strings.TrimSpace(merged), readOnly) {
				t.Fatalf("write/read-only constraints: %q", merged)
			}
			mode, err := os.ReadFile(record + ".prompt-mode")
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Fields(string(mode)); len(got) == 0 || strings.TrimRight(got[0], "@+") != "-rw-------" {
				t.Fatalf("temporary prompt is not private: %q", mode)
			}
			promptPath, err := os.ReadFile(record + ".prompt-path")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(strings.TrimSpace(string(promptPath))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("prompt not removed after code %d: err=%v path=%q", code, err, promptPath)
			}
			assertNoTemporaryPrompts(t)
		})
	}
}

func TestFakeClaudeRejectsIncompatiblePromptFlags(t *testing.T) {
	work, _ := setup(t, okReply)
	cmd := exec.Command(os.Getenv("OFFLOAD_CLAUDE"), "--append-system-prompt", "inline", "--append-system-prompt-file", filepath.Join(work, "AGENTS.md"))
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 9 || !strings.Contains(string(out), "Cannot use both") {
		t.Fatalf("fake must reproduce real CLI rejection: out=%q err=%v", out, err)
	}
}

func TestPromptSetupErrorsHaveMetricsAndLeaveNoTemporaryFiles(t *testing.T) {
	for _, tc := range []struct {
		name, wantError string
		modify          func(t *testing.T, work string)
	}{
		{"instruction read", "read project instructions", func(t *testing.T, work string) {
			if err := os.Mkdir(filepath.Join(work, "AGENTS.md"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"temporary file creation", "create worker prompt", func(t *testing.T, work string) {
			t.Setenv("TMPDIR", filepath.Join(os.TempDir(), "missing-directory"))
		}},
		{"credential resolution", "token_cmd failed", func(t *testing.T, work string) {
			cfg, err := loadConfig(configPath())
			if err != nil {
				t.Fatal(err)
			}
			prof := cfg.Profiles["cheap"]
			prof.TokenCmd = "exit 1"
			cfg.Profiles["cheap"] = prof
			contents, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath(), contents, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work, record := setup(t, okReply)
			t.Setenv("TMPDIR", t.TempDir())
			tc.modify(t, work)
			code, out, stderr := runCLI("-json", "-d", work, "inspect")
			rec := decodeRun(t, out)
			if code != 2 || rec.Status != "error" || rec.ExitCode == nil || *rec.ExitCode != 2 || rec.ElapsedMs == nil || rec.CostUSD != nil || !strings.Contains(rec.Error, tc.wantError) {
				t.Fatalf("prompt setup error metrics: code=%d rec=%+v stderr=%q", code, rec, stderr)
			}
			if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("worker should not start after setup error: %v", err)
			}
			assertNoTemporaryPrompts(t)
		})
	}
}

func assertNoTemporaryPrompts(t *testing.T) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(os.TempDir(), "offload-prompt-*.md"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("temporary prompts left behind: %v err=%v", paths, err)
	}
}
