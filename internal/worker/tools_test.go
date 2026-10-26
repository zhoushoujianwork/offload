package worker

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
)

// offload imposes no permission barrier. Every normal Claude run, read-only or write, fresh or
// resumed, skips permission prompts and gets neither a tool subset nor an allow or deny list.
// Only the report-only salvage step is tool-less.
func TestClaudeArgsImposeNoPermissionBarrier(t *testing.T) {
	banned := []string{"--allowedTools", "--allowed-tools", "--disallowedTools", "--disallowed-tools", "--permission-mode", "acceptEdits", "--tools"}
	for _, tc := range []struct {
		name string
		o    Opts
	}{
		{"read-only", Opts{}},
		{"write", Opts{Write: true}},
		{"read-only resume", Opts{Resume: "sess"}},
		{"write resume", Opts{Write: true, Resume: "sess"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := buildArgs(tc.o, "prompt.md")
			if n := strings.Count(strings.Join(args, "\n")+"\n", "--dangerously-skip-permissions\n"); n != 1 {
				t.Errorf("want exactly one --dangerously-skip-permissions: %q", args)
			}
			for _, b := range banned {
				if slices.Contains(args, b) {
					t.Errorf("%s must not appear: %q", b, args)
				}
			}
			for _, kept := range []string{"-p", "--bare", "--setting-sources", "--append-system-prompt-file"} {
				if !slices.Contains(args, kept) {
					t.Errorf("%s lost: %q", kept, args)
				}
			}
			if i := slices.Index(args, "--setting-sources"); i < 0 || args[i+1] != "" {
				t.Errorf("settings isolation lost: %q", args)
			}
			if got := slices.Contains(args, "--resume"); got != (tc.o.Resume != "") {
				t.Errorf("resume flag=%v for %+v: %q", got, tc.o, args)
			}
		})
	}
	// -w is task intent only: it must not change the arguments of a normal run.
	if r, w := buildArgs(Opts{Task: "t"}, "p"), buildArgs(Opts{Task: "t", Write: true}, "p"); !slices.Equal(r, w) {
		t.Errorf("Write changed Claude arguments:\nread  %q\nwrite %q", r, w)
	}
	// Salvage is tool-less by design, and also skips permissions.
	for _, write := range []bool{false, true} {
		s := buildArgs(Opts{Salvage: true, Write: write, Resume: "sess"}, "p")
		i := slices.Index(s, "--tools")
		if i < 0 || s[i+1] != "" || !slices.Contains(s, "--dangerously-skip-permissions") || !slices.Contains(s, "--resume") {
			t.Errorf("salvage args: %q", s)
		}
		for _, b := range []string{"--allowedTools", "--disallowedTools", "--permission-mode"} {
			if slices.Contains(s, b) {
				t.Errorf("salvage must not have %s: %q", b, s)
			}
		}
	}
}

// Codex explicitly bypasses approvals and sandboxing for both Write values, fresh and resumed,
// while profile settings and the model still reach the command line.
func TestCodexArgsImposeNoSandbox(t *testing.T) {
	for _, write := range []bool{false, true} {
		for _, resume := range []string{"", "sess"} {
			p := config.Profile{ReasoningEffort: "high", ServiceTier: "flex"}
			args := buildCodexArgs(Opts{Profile: p, Model: "mdl", Write: write, Resume: resume})
			joined := "\n" + strings.Join(args, "\n") + "\n"
			for _, want := range []string{"\n--dangerously-bypass-approvals-and-sandbox\n",
				"\nmodel_reasoning_effort=\"high\"\n", "\nservice_tier=\"flex\"\n", "\n-m\nmdl\n"} {
				if !strings.Contains(joined, want) {
					t.Errorf("write=%v resume=%q missing %q: %q", write, resume, want, args)
				}
			}
			for _, bad := range []string{"read-only", "workspace-write", "--sandbox", "--full-auto", "sandbox_mode=", "approval_policy="} {
				if strings.Contains(joined, bad) {
					t.Errorf("write=%v resume=%q has %q: %q", write, resume, bad, args)
				}
			}
			if got := slices.Contains(args, "resume"); got != (resume != "") {
				t.Errorf("resume=%q: %q", resume, args)
			}
		}
	}
}

func promptFor(t *testing.T, o Opts) string {
	t.Helper()
	o.Dir = t.TempDir()
	path, err := writeWorkerPrompt(o)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The prompt carries no shell, file or network restriction, keeps the missing-tool fallback and
// the no-delegation rule, and states read-only only as task intent.
func TestWorkerPromptsHaveNoImposedRestrictions(t *testing.T) {
	forbidden := []string{"Shell is limited", "pipes, &&", "python3", "python -c", "awk", "not approved", "refused and waste",
		"configured sandbox", "Do not commit", "allowed tool", "rtk run, proxy"}
	for _, backend := range []string{"claude", "codex"} {
		for _, write := range []bool{false, true} {
			prompt := promptFor(t, Opts{Write: write, Profile: config.Profile{Backend: backend}})
			for _, bad := range forbidden {
				if strings.Contains(prompt, bad) {
					t.Errorf("%s write=%v prompt still contains %q", backend, write, bad)
				}
			}
			for _, want := range []string{"imposes no command", "supervisor's authorization", "host operating system, network and credentials", "or delegate to other workers"} {
				if !strings.Contains(prompt, want) {
					t.Errorf("%s write=%v prompt missing %q", backend, write, want)
				}
			}
			if got := strings.Contains(prompt, "Read-only task: do not modify files."); got == write {
				t.Errorf("%s write=%v: read-only task intent present=%v", backend, write, got)
			}
		}
	}
	claude := promptFor(t, Opts{Profile: config.Profile{}})
	for _, want := range []string{"only when they are exposed", "rg or rtk grep", "ls or rtk ls", "do not repeat a call"} {
		if !strings.Contains(claude, want) {
			t.Errorf("missing-tool fallback lost: %q", want)
		}
	}
	if strings.Contains(claude, "Prefer Read, Grep and Glob") || strings.Contains(claude, "return a BLOCKED report instead of reading large raw") {
		t.Error("obsolete tool or aggregation instruction remains")
	}
}
