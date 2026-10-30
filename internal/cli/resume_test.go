package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/history"
)

func TestResumeSavedRun(t *testing.T) {
	work, capture := setup(t, okReply)
	t.Setenv("FAKE_EXIT", "1")
	code, out, e := runCLI("-json", "-w", "-d", work, "-m", "override", "-idle", "0", "-t", "2m", "-allowed-tools", "Read,Bash", "-caller", "clawflow", "-caller-run-id", "old-attempt", "fix the bug")
	if code == 0 {
		t.Fatalf("expected failed run: %s %s", out, e)
	}
	var first history.Record
	if err := json.Unmarshal([]byte(out), &first); err != nil {
		t.Fatal(err)
	}
	if first.ResumeSettings == nil {
		t.Fatal("missing settings")
	}
	t.Setenv("FAKE_EXIT", "0")
	for _, id := range []string{first.RunID, first.Session} {
		code, out, e = runCLI("resume", "--id", id, "--json", "--caller", "manual", "--caller-run-id", "new-attempt")
		if code != 0 {
			t.Fatalf("resume: %d %s %s", code, out, e)
		}
		var next history.Record
		if err := json.Unmarshal([]byte(out), &next); err != nil {
			t.Fatal(err)
		}
		if next.RunID == first.RunID || next.Session != first.Session || next.ResumedFrom == "" || next.Caller != "manual" || next.CallerRunID != "new-attempt" || next.Model != "override" {
			t.Fatalf("identity/settings lost: %+v", next)
		}
		if next.ResumeSettings.Task != "fix the bug" || strings.Count(next.Task, "Original task:") != 1 {
			t.Fatalf("task grew across resumes: %+v", next)
		}
		if !reflect.DeepEqual(next.ResumeSettings.Args, first.ResumeSettings.Args) {
			t.Fatalf("execution settings changed: %v -> %v", first.ResumeSettings.Args, next.ResumeSettings.Args)
		}
		b, _ := os.ReadFile(capture)
		for _, want := range []string{"--resume\ns-1", "--allowedTools\nRead Bash", "pwd=" + work, "model=override"} {
			if !strings.Contains(string(b), want) {
				t.Errorf("missing %q: %s", want, b)
			}
		}
		if !strings.Contains(string(b), "do not repeat completed actions") || !strings.Contains(string(b), "fix the bug") {
			t.Fatalf("prompt: %s", b)
		}
	}
}

func TestResumeFailureBeforeInitKeepsSession(t *testing.T) {
	work, _ := setup(t, okReply)
	if code, _, e := runCLI("-d", work, "inspect"); code != 0 {
		t.Fatal(e)
	}
	fake := filepath.Join(t.TempDir(), "failed-provider")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CLAUDE", fake)
	code, out, e := runCLI("resume", "--id", "s-1", "--json")
	if code == 0 {
		t.Fatalf("expected failure: %s %s", out, e)
	}
	var r history.Record
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if r.Session != "s-1" || !r.HasResumeSettings() || r.ResumedFrom == "" {
		t.Fatalf("session lost: %+v", r)
	}
}

func TestResumeRejectsUnsafeOrUnknownSource(t *testing.T) {
	_, capture := setup(t, okReply)
	for _, r := range []history.Record{
		{RunID: "early"},
		{RunID: "legacy", Session: "legacy-session"},
		{RunID: "future", Session: "future-session", ResumeSettings: &history.ResumeSettings{Version: 2}},
		{RunID: "changed", Session: "changed-session", Profile: "cheap", Backend: "codex", ResumeSettings: &history.ResumeSettings{Version: 1}},
	} {
		if err := history.Append(history.Path(), r); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"unknown", "early", "legacy", "future", "changed"} {
		if code, _, e := runCLI("resume", "--id", id); code == 0 {
			t.Fatalf("accepted %s: %s", id, e)
		}
	}
	if _, err := os.Stat(capture); !os.IsNotExist(err) {
		t.Fatalf("provider launched: %v", err)
	}
}

func TestResumeDoesNotInheritCaller(t *testing.T) {
	work, _ := setup(t, okReply)
	if code, _, e := runCLI("-d", work, "-caller", "clawflow", "-caller-run-id", "old", "inspect"); code != 0 {
		t.Fatal(e)
	}
	code, out, e := runCLI("resume", "--id", "s-1", "--json")
	if code != 0 {
		t.Fatal(e)
	}
	var next history.Record
	if err := json.Unmarshal([]byte(out), &next); err != nil {
		t.Fatal(err)
	}
	if next.Caller != "unspecified" || next.CallerRunID != "" {
		t.Fatalf("inherited caller: %+v", next)
	}
}
