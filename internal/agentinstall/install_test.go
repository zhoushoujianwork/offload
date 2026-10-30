package agentinstall

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/skills"
)

// Installer tests must not inherit a developer's saved delegation mode or config.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "offload-install-test-config-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func installAgentCLI(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func installTestRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func installTestWrite(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func TestInstallAgentUserDefaultsEmbedSkillWithoutCheckout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Chdir(t.TempDir())
	code, out, errOut := installAgentCLI()
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	for _, agent := range []struct{ policyRoot, skillRoot, policy string }{{".codex", ".agents", "AGENTS.md"}, {".claude", ".claude", "CLAUDE.md"}} {
		root := filepath.Join(home, agent.policyRoot)
		skill := filepath.Join(home, agent.skillRoot, "skills", "offload", "SKILL.md")
		if got := installTestRead(t, skill); !bytes.Equal(got, skills.Offload) {
			t.Fatalf("installed skill differs from bundled skill: %s", skill)
		}
		policy := string(installTestRead(t, filepath.Join(root, agent.policy)))
		for _, want := range []string{offloadPolicyBegin, offloadPolicyEnd, "main agent and native subagents", "must not launch another offload", "-t", "no USD cap", "read-only", "ESTIMATED", "measured elapsed time", "profile/model", "session ID", "status/exit code"} {
			if !strings.Contains(policy, want) {
				t.Errorf("policy missing %q: %s", want, policy)
			}
		}
		if err := checkSkillOwnership(filepath.Dir(skill)); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Count(out, "installed ") != 6 {
		t.Fatalf("expected six installed files: %q", out)
	}
}

func TestInstallAgentUserHonorsConfigRoots(t *testing.T) {
	tmp := t.TempDir()
	home, codex, claude := filepath.Join(tmp, "home"), filepath.Join(tmp, "codex"), filepath.Join(tmp, "claude")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codex)
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	if code, _, errOut := installAgentCLI(); code != 0 {
		t.Fatal(errOut)
	}
	installTestRead(t, filepath.Join(codex, "AGENTS.md"))
	installTestRead(t, filepath.Join(claude, "CLAUDE.md"))
	installTestRead(t, filepath.Join(home, ".agents", "skills", "offload", "SKILL.md"))
	for _, fallback := range []string{filepath.Join(home, ".codex"), filepath.Join(home, ".claude")} {
		if _, err := os.Stat(fallback); !os.IsNotExist(err) {
			t.Fatalf("fallback config directory unexpectedly touched: %s: %v", fallback, err)
		}
	}
}

func TestInstallAgentProjectPreservesInstructionsModeAndIdempotency(t *testing.T) {
	project := t.TempDir()
	policyPath := filepath.Join(project, "AGENTS.md")
	original := []byte("# Project instructions\n\nKeep existing rules.\n")
	installTestWrite(t, policyPath, original, 0o600)
	args := []string{"-agent", "codex", "-scope", "project", "-d", project}
	if code, _, errOut := installAgentCLI(args...); code != 0 {
		t.Fatal(errOut)
	}
	policy := installTestRead(t, policyPath)
	if !bytes.HasPrefix(policy, original) || strings.Count(string(policy), offloadPolicyBegin) != 1 {
		t.Fatalf("existing instructions not preserved: %s", policy)
	}
	st, err := os.Stat(policyPath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("policy mode not preserved: %v, %v", st, err)
	}
	skill := filepath.Join(project, ".agents", "skills", "offload", "SKILL.md")
	installTestRead(t, skill)
	if _, err := os.Stat(filepath.Join(project, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Fatalf("unselected Claude files touched: %v", err)
	}
	code, out, errOut := installAgentCLI(args...)
	if code != 0 || strings.Count(out, "unchanged ") != 3 {
		t.Fatalf("reinstall code=%d out=%q err=%q", code, out, errOut)
	}
	stAfter, _ := os.Stat(policyPath)
	if !st.ModTime().Equal(stAfter.ModTime()) || !bytes.Equal(installTestRead(t, policyPath), policy) {
		t.Fatal("idempotent install changed policy content or modification time")
	}
}

func TestInstallAgentProjectDefaultsToCurrentDirectory(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	if code, _, errOut := installAgentCLI("-agent", "claude", "-scope", "project"); code != 0 {
		t.Fatal(errOut)
	}
	installTestRead(t, filepath.Join(project, "CLAUDE.md"))
	installTestRead(t, filepath.Join(project, ".claude", "skills", "offload", "SKILL.md"))
	if _, err := os.Stat(filepath.Join(project, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("unselected Codex files touched: %v", err)
	}
}

func TestInstallAgentRejectsInvalidArguments(t *testing.T) {
	for _, args := range [][]string{{"-agent", "other"}, {"-scope", "other"}, {"-d", t.TempDir()}, {"extra"}, {"-scope", "project", "-d", filepath.Join(t.TempDir(), "missing")}} {
		if code, out, errOut := installAgentCLI(args...); code != 2 || out != "" || errOut == "" {
			t.Errorf("args=%q code=%d out=%q err=%q", args, code, out, errOut)
		}
	}
	if code, _, _ := installAgentCLI("-h"); code != 0 {
		t.Errorf("help exit code=%d", code)
	}
}

func TestInstallAgentRefusesMalformedManagedMarkersBeforeWrites(t *testing.T) {
	for name, content := range map[string]string{
		"missing end":    offloadPolicyBegin + "\nold policy\n",
		"missing begin":  offloadPolicyEnd,
		"duplicate":      offloadAgentPolicy + "\n" + offloadAgentPolicy,
		"reversed":       offloadPolicyEnd + "\n" + offloadPolicyBegin,
		"inline begin":   "prefix " + offloadPolicyBegin + "\n" + offloadPolicyEnd,
		"inline end":     offloadPolicyBegin + "\n" + offloadPolicyEnd + " suffix",
		"broken marker":  "<!-- offload:begin\n",
		"unknown marker": "<!-- offload:other -->\n",
	} {
		t.Run(name, func(t *testing.T) {
			project := t.TempDir()
			path := filepath.Join(project, "AGENTS.md")
			installTestWrite(t, path, []byte(content), 0o644)
			code, out, errOut := installAgentCLI("-agent", "codex", "-scope", "project", "-d", project)
			if code != 2 || out != "" || !strings.Contains(errOut, "markers") {
				t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
			}
			if got := string(installTestRead(t, path)); got != content {
				t.Fatal("conflicting policy changed")
			}
			if _, err := os.Stat(filepath.Join(project, ".agents")); !os.IsNotExist(err) {
				t.Fatalf("skill installed despite policy conflict: %v", err)
			}
		})
	}
}

func TestUpdateOffloadPolicyReplacesOnlyBlockAndPreservesCRLF(t *testing.T) {
	prefix, suffix := "# Rules\r\nKeep me.\r\n\r\n", "\r\n\r\nKeep this too.\r\n"
	old := prefix + offloadPolicyBegin + "\r\nOld instructions.\r\n" + offloadPolicyEnd + suffix
	updated, err := updateOffloadPolicy([]byte(old))
	if err != nil {
		t.Fatal(err)
	}
	want := prefix + strings.ReplaceAll(offloadAgentPolicy, "\n", "\r\n") + suffix
	if string(updated) != want {
		t.Fatalf("replacement changed surrounding content: %q", updated)
	}
	if again, err := updateOffloadPolicy(updated); err != nil || !bytes.Equal(again, updated) {
		t.Fatalf("replacement not idempotent: %v", err)
	}
}

func TestInstallAgentRefusesUnmanagedSkillAndPreflightsAllAgents(t *testing.T) {
	project := t.TempDir()
	skill := filepath.Join(project, ".claude", "skills", "offload", "SKILL.md")
	original := []byte("# My unrelated custom skill\n")
	installTestWrite(t, skill, original, 0o600)
	code, out, errOut := installAgentCLI("-scope", "project", "-d", project)
	if code != 2 || out != "" || !strings.Contains(errOut, "unmanaged skill") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if !bytes.Equal(installTestRead(t, skill), original) {
		t.Fatal("unmanaged skill overwritten")
	}
	for _, path := range []string{filepath.Join(project, "AGENTS.md"), filepath.Join(project, ".agents"), filepath.Join(project, "CLAUDE.md")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("preflight failure wrote %s: %v", path, err)
		}
	}
}

func TestInstallAgentRefusesSkillSymlinkWithoutTouchingTarget(t *testing.T) {
	project, external := t.TempDir(), t.TempDir()
	original := []byte("# Shared offload skill\n")
	installTestWrite(t, filepath.Join(external, "SKILL.md"), original, 0o644)
	skillDir := filepath.Join(project, ".agents", "skills", "offload")
	if err := os.MkdirAll(filepath.Dir(skillDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, skillDir); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := installAgentCLI("-agent", "codex", "-scope", "project", "-d", project)
	if code != 2 || out != "" || !strings.Contains(errOut, "not a regular directory") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if !bytes.Equal(installTestRead(t, filepath.Join(external, "SKILL.md")), original) {
		t.Fatal("symlink target overwritten")
	}
	if _, err := os.Stat(filepath.Join(project, "AGENTS.md")); !os.IsNotExist(err) {
		t.Fatalf("policy written despite skill symlink: %v", err)
	}
}

func TestInstallAgentRefusesModifiedManagedSkillAndUnexpectedFiles(t *testing.T) {
	for _, conflict := range []string{"modified", "extra", "metadata"} {
		t.Run(conflict, func(t *testing.T) {
			project := t.TempDir()
			args := []string{"-agent", "codex", "-scope", "project", "-d", project}
			if code, _, errOut := installAgentCLI(args...); code != 0 {
				t.Fatal(errOut)
			}
			skillDir := filepath.Join(project, ".agents", "skills", "offload")
			switch conflict {
			case "modified":
				installTestWrite(t, filepath.Join(skillDir, "SKILL.md"), []byte("my custom edits"), 0o644)
			case "extra":
				installTestWrite(t, filepath.Join(skillDir, "custom.txt"), []byte("custom"), 0o644)
			case "metadata":
				installTestWrite(t, filepath.Join(skillDir, offloadSkillOwner), []byte(`{"format":2}`), 0o644)
			}
			before := installTestRead(t, filepath.Join(project, "AGENTS.md"))
			if code, out, errOut := installAgentCLI(args...); code != 2 || out != "" || errOut == "" {
				t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
			}
			if !bytes.Equal(before, installTestRead(t, filepath.Join(project, "AGENTS.md"))) {
				t.Fatal("policy changed despite ownership conflict")
			}
		})
	}
}

func TestPlanAgentInstallUpdatesOwnedSkillPreservingMode(t *testing.T) {
	project := t.TempDir()
	if code, _, errOut := installAgentCLI("-agent", "codex", "-scope", "project", "-d", project); code != 0 {
		t.Fatal(errOut)
	}
	skillDir := filepath.Join(project, ".agents", "skills", "offload")
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.Chmod(skillPath, 0o600); err != nil {
		t.Fatal(err)
	}
	updatedSkill := append(bytes.Clone(skills.Offload), []byte("\nUpdated bundled guidance.\n")...)
	targets := []agentInstallTarget{{skillDir, filepath.Join(project, "AGENTS.md")}}
	files, err := planAgentInstall(targets, updatedSkill)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 || files[0].status != "unchanged" || files[1].status != "updated" || files[2].status != "updated" {
		t.Fatalf("unexpected update plan: %+v", files)
	}
	for _, file := range files {
		if file.status != "unchanged" {
			if err := writeAgentInstallFile(file); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !bytes.Equal(installTestRead(t, skillPath), updatedSkill) {
		t.Fatal("owned skill not updated")
	}
	st, _ := os.Stat(skillPath)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("skill mode changed to %v", st.Mode().Perm())
	}
	var owner skillOwnership
	if err := json.Unmarshal(installTestRead(t, filepath.Join(skillDir, offloadSkillOwner)), &owner); err != nil || owner.SkillSHA256 != skillHash(updatedSkill) {
		t.Fatalf("ownership metadata not updated: %+v %v", owner, err)
	}
	if err := checkSkillOwnership(skillDir); err != nil {
		t.Fatal(err)
	}
	files, err = planAgentInstall(targets, updatedSkill)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.status != "unchanged" {
			t.Fatalf("updated install not idempotent: %+v", file)
		}
	}
}

func TestInstallAgentRefusesPolicySymlink(t *testing.T) {
	project := t.TempDir()
	external := filepath.Join(t.TempDir(), "instructions.md")
	original := []byte("# Shared instructions\n")
	installTestWrite(t, external, original, 0o644)
	if err := os.Symlink(external, filepath.Join(project, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := installAgentCLI("-agent", "codex", "-scope", "project", "-d", project); code != 2 || !strings.Contains(errOut, "non-regular") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	if !bytes.Equal(installTestRead(t, external), original) {
		t.Fatal("symlink policy target changed")
	}
}

func TestInstallAgentWarnsWhenCodexPolicyIsOverridden(t *testing.T) {
	project := t.TempDir()
	path := filepath.Join(project, "AGENTS.override.md")
	original := []byte("# Preferred project instructions\n")
	installTestWrite(t, path, original, 0o600)
	code, _, errOut := installAgentCLI("-agent", "codex", "-scope", "project", "-d", project)
	if code != 0 || !strings.Contains(errOut, "AGENTS.override.md takes precedence") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	if !bytes.Equal(installTestRead(t, path), original) {
		t.Fatal("override instructions changed")
	}
}

func TestInstallAgentDoesNotWarnForEmptyCodexOverride(t *testing.T) {
	for _, original := range [][]byte{nil, []byte(" \t\r\n")} {
		project := t.TempDir()
		path := filepath.Join(project, "AGENTS.override.md")
		installTestWrite(t, path, original, 0o600)
		code, _, errOut := installAgentCLI("-agent", "codex", "-scope", "project", "-d", project)
		if code != 0 || errOut != "" {
			t.Fatalf("override=%q code=%d err=%q", original, code, errOut)
		}
		if !bytes.Equal(installTestRead(t, path), original) {
			t.Fatal("empty override changed")
		}
	}
}

// Policy, bundled skill, and the linked user guide describe the same workflow.
func TestSessionReuseWorkflowAgreesAcrossPolicySkillAndGuide(t *testing.T) {
	readme, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(readme), "docs/guide.md") {
		t.Fatal("README must link to the detailed user guide")
	}
	guide, err := os.ReadFile(filepath.Join("..", "..", "docs", "guide.md"))
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]string{"skill": string(skills.Offload), "guide": string(guide)}
	for _, mode := range []string{config.ModeBasic, config.ModeProactive} {
		docs["policy-"+mode] = policyForMode(mode)
	}
	for name, doc := range docs {
		for _, want := range []string{"offload -r", "watch -t", "compaction", "overlapping", "offload show", "STALLED"} {
			if !strings.Contains(doc, want) {
				t.Errorf("%s missing %q", name, want)
			}
		}
	}
	if !strings.Contains(policyForMode(config.ModeBasic), sessionReusePolicy) || !strings.Contains(policyForMode(config.ModeProactive), sessionReusePolicy) {
		t.Error("both modes must carry the shared session reuse policy")
	}
}
