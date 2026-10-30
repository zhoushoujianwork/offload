package worker

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
)

func TestManagedPolicyArgsAndPrompt(t *testing.T) {
	for _, resume := range []string{"", "session"} {
		o := Opts{Write: true, ExecutionPolicy: "managed-development", Resume: resume, Dir: t.TempDir(), Instructions: []byte("OPERATOR_OUTCOME: KEEP_VERBATIM\n"), Profile: config.Profile{Backend: "codex"}}
		for _, sandbox := range []string{"", "workspace-write", "danger-full-access"} {
			o.CodexSandbox = sandbox
			if err := Validate(&o); err != nil {
				t.Fatal(err)
			}
			args := strings.Join(buildCodexArgs(o), " ")
			if !strings.Contains(args, `approval_policy="never"`) {
				t.Fatal(args)
			}
			if sandbox == "danger-full-access" {
				if !strings.Contains(args, `sandbox_mode="danger-full-access"`) || strings.Contains(args, "network_access") {
					t.Fatal(args)
				}
			} else if !strings.Contains(args, "sandbox_workspace_write.network_access=true") {
				t.Fatal(args)
			}
		}
		p, err := writeWorkerPrompt(o)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(p)
		os.Remove(p)
		for _, want := range []string{"OPERATOR_OUTCOME: KEEP_VERBATIM", "Task-authorized tests", "git pushes", "clawflow", "force pushes", "recursively delegate"} {
			if !bytes.Contains(b, []byte(want)) {
				t.Fatalf("missing %s", want)
			}
		}
		if bytes.Contains(b, []byte("Do not commit, push")) || bytes.Contains(b, []byte("Shell is limited")) {
			t.Fatal(string(b))
		}
		if bytes.Index(b, []byte("OPERATOR_OUTCOME")) > bytes.Index(b, []byte(taskDiscipline)) {
			t.Fatal("operator instructions must precede discipline")
		}
		o.Profile = config.Profile{}
		args := strings.Join(buildArgs(o, "private"), " ")
		if !strings.Contains(args, "--permission-mode acceptEdits --allowedTools Bash Read Grep Glob Edit Write") {
			t.Fatal(args)
		}
		o.AllowedTools = "Read,Grep"
		o.DisallowedTools = "Bash,Write"
		args = strings.Join(buildArgs(o, "private"), " ")
		if strings.Contains(args, "Bash Read") || !strings.Contains(args, "--allowedTools Read Grep") || !strings.Contains(args, "--disallowedTools Bash Write") {
			t.Fatal(args)
		}
		if strings.Contains(args, "acceptEdits") || !strings.Contains(args, "--permission-mode default") {
			t.Fatal(args)
		}
		o.AllowedTools = ""
		o.ToolsDisabled = true
		if a := strings.Join(buildArgs(o, "private"), "|"); !strings.Contains(a, "--tools||") || strings.Contains(a, "--allowedTools") {
			t.Fatal(a)
		}
	}
}

func TestSupervisorValidationAndInstructionFiles(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "instructions")
	os.WriteFile(good, []byte("VERBATIM\n"), 0600)
	symlink := filepath.Join(dir, "symlink")
	os.Symlink(good, symlink)
	big := filepath.Join(dir, "large")
	os.WriteFile(big, make([]byte, (256<<10)+1), 0600)
	fifo := filepath.Join(dir, "fifo")
	if err := exec.Command("mkfifo", fifo).Run(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"relative", dir, symlink, big, fifo, filepath.Join(dir, "missing")} {
		if _, err := readInstructions(path); err == nil {
			t.Errorf("accepted %s", path)
		}
	}
	if b, err := readInstructions(good); err != nil || string(b) != "VERBATIM\n" {
		t.Fatalf("%q %v", b, err)
	}
	for _, o := range []Opts{
		{ExecutionPolicy: "unknown"}, {ExecutionPolicy: "managed-development"}, {CallerRunID: "bad id"}, {CallerRunID: strings.Repeat("a", 129)},
		{CodexSandbox: "workspace-write"}, {Write: true, ExecutionPolicy: "managed-development", CodexSandbox: "invalid"}, {AllowedTools: "Read,,Grep"},
	} {
		if err := Validate(&o); err == nil {
			t.Fatalf("accepted %+v", o)
		}
	}
	for _, o := range []Opts{{ToolsDisabled: true}, {AllowedTools: "Read"}, {DisallowedTools: "Bash"}} {
		o.Profile = config.Profile{Backend: "codex"}
		if err := Validate(&o); err == nil || ErrorKind(err) != "unsupported_tools" {
			t.Fatalf("%+v %v", o, err)
		}
	}
	for _, o := range []Opts{{ToolsDisabled: true}, {AllowedTools: "Read"}, {DisallowedTools: "Bash"}} {
		o.Profile, o.ProfileName, o.OnUnsupportedTools = config.Profile{Backend: "codex"}, "cx", UnsupportedToolsIgnore
		if err := Validate(&o); err != nil || !o.toolsNotEnforced {
			t.Fatalf("ignore mode: %+v %v", o, err)
		}
	}
	for _, backend := range []string{"claude", ""} {
		o := Opts{AllowedTools: "Read", OnUnsupportedTools: UnsupportedToolsIgnore, Profile: config.Profile{Backend: backend}}
		if err := Validate(&o); err != nil || o.toolsNotEnforced {
			t.Fatalf("%q: restriction must stay enforced: %+v %v", backend, o, err)
		}
	}
	if err := Validate(&Opts{OnUnsupportedTools: "maybe"}); err == nil {
		t.Fatal("accepted invalid on-unsupported-tools")
	}
	o := Opts{CallerRunID: "A-z_0.1:job", InstructionsFile: good}
	if err := Validate(&o); err != nil || string(o.Instructions) != "VERBATIM\n" {
		t.Fatalf("%+v %v", o, err)
	}
}

func TestActivityCompleteLinesOutsideBufferLock(t *testing.T) {
	var lines []string
	w := &activityWriter{}
	w.onLine = func(b []byte) error { _ = w.Bytes(); lines = append(lines, string(b)); return nil }
	w.Write([]byte("first\nsec"))
	w.Write([]byte("ond\npartial"))
	if strings.Join(lines, "|") != "first|second" {
		t.Fatal(lines)
	}
	w.pending = nil
	w.Write(bytes.Repeat([]byte("x"), (256<<10)+1))
	w.Write([]byte("\nnext\n"))
	if lines[len(lines)-1] != "next" || len(lines) != 3 {
		t.Fatal(len(lines))
	}
	w.onLine = func([]byte) error { return errors.New("disk full") }
	if _, err := w.Write([]byte("fail\n")); err == nil {
		t.Fatal("callback error ignored")
	}
}

func TestManagedGitWritableRoots(t *testing.T) {
	dir := t.TempDir()
	if b, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("%s %v", b, err)
	}
	o := Opts{Dir: dir, Write: true, ExecutionPolicy: "managed-development", Profile: config.Profile{Backend: "codex"}}
	if err := Validate(&o); err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.EvalSymlinks(filepath.Join(dir, ".git"))
	if len(o.GitRoots) == 0 || o.GitRoots[0] != root {
		t.Fatal(o.GitRoots)
	}
	if !strings.Contains(strings.Join(buildCodexArgs(o), " "), "sandbox_workspace_write.writable_roots=[") {
		t.Fatal("missing git roots")
	}
}

func TestExplicitAllowlistUsesDefaultPermissions(t *testing.T) {
	for _, policy := range []string{"default", "managed-development"} {
		for _, resume := range []string{"", "session"} {
			args := strings.Join(buildArgs(Opts{Write: true, ExecutionPolicy: policy, Resume: resume, AllowedTools: "Read"}, "private"), "|")
			if strings.Contains(args, "acceptEdits") || !strings.Contains(args, "--permission-mode|default|--allowedTools|Read|") {
				t.Fatal(args)
			}
		}
	}
	for _, backend := range []string{"claude", "codex"} {
		o := Opts{Profile: config.Profile{Backend: backend}, ToolsDisabled: true, AllowedTools: "Read"}
		if err := Validate(&o); err == nil || !strings.Contains(err.Error(), "tools-disabled conflicts with allowed-tools") || ErrorKind(err) != "configuration" {
			t.Fatalf("%s %v", backend, err)
		}
	}
}
