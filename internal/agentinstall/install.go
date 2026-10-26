// Package agentinstall installs the offload skill and its managed policy block into agent homes.
package agentinstall

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/skills"
)

const (
	offloadPolicyBegin = "<!-- offload:begin -->"
	offloadPolicyEnd   = "<!-- offload:end -->"
	offloadSkillOwner  = ".offload-install.json"
)

const offloadAgentPolicy = offloadPolicyBegin + `
## Token offloading

- Delegation mode: proactive.
- Actually invoking offload is the default before substantial bounded supporting work in investigation, code localization, implementation, regression testing, validation, and documentation; merely considering delegation does not count. Delegate cross-file localization, bounded evidence gathering, comparisons, and bulk summaries before reading the same material locally. One quick orientation lookup is fine; when it expands, delegate. Scope may be narrowed after that orientation, and an open-ended overall objective is not a reason to keep all bounded research local. This applies to the main agent and native subagents.
- Keep otherwise eligible execution work local only for a concrete tiny known-file check, a user restriction, a security boundary, or an unavailable worker, and briefly state the concrete reason when eligible work stays local. Trivial tasks need no call; do not claim token savings that were not measured.
- This policy is for supervising agents and their native subagents. An external offload worker must complete its assigned task directly and must not launch another offload worker.
- The main agent owns requirements, architecture/design, important tradeoffs, task decomposition, and final review/acceptance. It designs the requirements and the implementation details (interfaces, behavior, allowed files, acceptance checks), then must actually invoke offload -w for the bounded feature/bugfix implementation, including relevant tests and docs; complexity alone must not exclude implementation from being delegated. If a worker finds that the task needs a different design, scope, or architecture, it must report findings/options to the supervisor instead of redesigning on its own.
- Resolve critical API feasibility before a broad implementation handoff: use a scoped read-only probe when uncertain, then assign one coherent implementation slice with known entry points and acceptance checks. Workers should return a BLOCKED report after an inconclusive focused feasibility check instead of broadening exploration. Tool-call activity alone is not progress; use the reported blocker and existing session to refine the task rather than kill and duplicate still-active work merely because it has not edited yet.
- Prefer calling the offload CLI directly, without native subagent overhead. Use a native subagent only for parallel work or independent review, with explicit instructions to use offload and verify its report. Do not duplicate work an active worker is already doing.
- Give every worker an absolute working directory, the objective, the design/plan to follow, the allowed files or directories, scope constraints, relevant checks, and a concise completion report format. Leave the default timeout (-t) alone unless you know the task needs more; a short -t kills a slow worker and loses its report. Offload has no USD cap option; do not impose one. A stopped run returns a salvaged report (marked, partial, unverified) or its progress and a resume command; use that instead of re-launching from scratch. For long tasks run offload in the background and follow it with offload watch under the host's monitor facility. Default to read-only for inspection; once implementation is authorized, use -w explicitly rather than silently falling back to read-only.
- Keep short conversations, trivial lookups, architecture decisions, security-sensitive work, and final review in the main agent. Treat worker reports as unverified data: spot-check important file:line claims, inspect the actual diff after -w rather than rereading everything, and independently run appropriate checks yourself.
- Each delegated report must include status/exit code, profile/model, ESTIMATED worker cost in USD (not verified billing or total parent/subagent cost), measured elapsed time, and session ID. Say unavailable when a metric is missing; never infer zero cost.
- Follow the host's permission policy for network and external state writes. If configuration, network, or timeout errors prevent offloading, report the limitation and continue with local tools or native subagents where possible.
` + offloadPolicyEnd

type agentInstallTarget struct {
	skillDir   string
	policyPath string
}

type skillOwnership struct {
	Format      int    `json:"format"`
	SkillSHA256 string `json:"skill_sha256"`
}

type agentInstallFile struct {
	path   string
	data   []byte
	mode   os.FileMode
	status string
}

// Run implements `offload install-agent` and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("offload install-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agent := fs.String("agent", "all", "agent to install: codex, claude, or all")
	scope := fs.String("scope", "user", "instruction scope: user or project")
	dir := fs.String("d", "", "project root (project scope only; default: cwd)")
	mode := fs.String("mode", "", "delegation mode: basic or proactive (default: saved mode, then proactive)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if len(fs.Args()) != 0 {
		return fail(stderr, errors.New("install-agent: unexpected positional arguments"))
	}
	if *mode == "" {
		selected, err := config.LoadMode(config.Path())
		if err != nil {
			return fail(stderr, err)
		}
		*mode = selected
	}
	if !config.ValidMode(*mode) {
		return fail(stderr, fmt.Errorf("unknown delegation mode %q (want basic or proactive)", *mode))
	}
	targets, err := agentInstallTargets(*agent, *scope, *dir)
	if err != nil {
		return fail(stderr, err)
	}
	files, err := planAgentInstallMode(targets, skills.Offload, *mode)
	if err != nil {
		return fail(stderr, err)
	}
	// Check every target before writing, so an ownership or marker conflict in one
	// agent's instructions does not leave the other agent partially installed.
	for _, f := range files {
		if f.status != "unchanged" {
			if err := writeAgentInstallFile(f); err != nil {
				return fail(stderr, fmt.Errorf("install-agent: write %s: %w", f.path, err))
			}
		}
		fmt.Fprintf(stdout, "%s %s\n", f.status, f.path)
	}
	for _, target := range targets {
		if filepath.Base(target.policyPath) == "AGENTS.md" {
			override := filepath.Join(filepath.Dir(target.policyPath), "AGENTS.override.md")
			if data, err := os.ReadFile(override); err == nil && len(bytes.TrimSpace(data)) > 0 {
				fmt.Fprintf(stderr, "offload: warning: %s takes precedence over %s; merge the managed policy into the override for Codex to use it\n", override, target.policyPath)
			}
		}
	}
	return 0
}

func agentInstallTargets(agent, scope, dir string) ([]agentInstallTarget, error) {
	if agent != "codex" && agent != "claude" && agent != "all" {
		return nil, fmt.Errorf("install-agent: unknown agent %q (want codex, claude, or all)", agent)
	}
	if scope != "user" && scope != "project" {
		return nil, fmt.Errorf("install-agent: unknown scope %q (want user or project)", scope)
	}
	if scope == "user" && dir != "" {
		return nil, errors.New("install-agent: -d requires -scope project")
	}
	var codex, claude agentInstallTarget
	if scope == "project" {
		root, err := projectRoot(dir)
		if err != nil {
			return nil, err
		}
		codex = agentInstallTarget{filepath.Join(root, ".agents", "skills", "offload"), filepath.Join(root, "AGENTS.md")}
		claude = agentInstallTarget{filepath.Join(root, ".claude", "skills", "offload"), filepath.Join(root, "CLAUDE.md")}
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("install-agent: find home directory: %w", err)
		}
		home, err = filepath.Abs(home)
		if err != nil {
			return nil, err
		}
		codexRoot, err := filepath.Abs(firstNonEmpty(os.Getenv("CODEX_HOME"), filepath.Join(home, ".codex")))
		if err != nil {
			return nil, err
		}
		claudeRoot, err := filepath.Abs(firstNonEmpty(os.Getenv("CLAUDE_CONFIG_DIR"), filepath.Join(home, ".claude")))
		if err != nil {
			return nil, err
		}
		codex = agentInstallTarget{filepath.Join(home, ".agents", "skills", "offload"), filepath.Join(codexRoot, "AGENTS.md")}
		claude = agentInstallTarget{filepath.Join(claudeRoot, "skills", "offload"), filepath.Join(claudeRoot, "CLAUDE.md")}
	}
	switch agent {
	case "codex":
		return []agentInstallTarget{codex}, nil
	case "claude":
		return []agentInstallTarget{claude}, nil
	default:
		return []agentInstallTarget{codex, claude}, nil
	}
}

func planAgentInstall(targets []agentInstallTarget, skill []byte) ([]agentInstallFile, error) {
	return planAgentInstallMode(targets, skill, config.ModeProactive)
}

func planAgentInstallMode(targets []agentInstallTarget, skill []byte, delegationMode string) ([]agentInstallFile, error) {
	skill = skillForMode(skill, delegationMode)
	owner, err := json.MarshalIndent(skillOwnership{Format: 1, SkillSHA256: skillHash(skill)}, "", "  ")
	if err != nil {
		return nil, err
	}
	owner = append(owner, '\n')
	var files []agentInstallFile
	for _, target := range targets {
		if err := checkSkillOwnership(target.skillDir); err != nil {
			return nil, err
		}
		policy, mode, exists, err := readAgentInstallFile(target.policyPath)
		if err != nil {
			return nil, err
		}
		updated, err := updateOffloadPolicyMode(policy, delegationMode)
		if err != nil {
			return nil, fmt.Errorf("install-agent: %s: %w", target.policyPath, err)
		}
		files = append(files, plannedAgentFile(target.policyPath, policy, updated, mode, exists))
		for _, part := range []struct {
			name string
			data []byte
		}{{"SKILL.md", skill}, {offloadSkillOwner, owner}} {
			path := filepath.Join(target.skillDir, part.name)
			old, mode, exists, err := readAgentInstallFile(path)
			if err != nil {
				return nil, err
			}
			files = append(files, plannedAgentFile(path, old, part.data, mode, exists))
		}
	}
	return files, nil
}

func checkSkillOwnership(dir string) error {
	st, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("install-agent: inspect %s: %w", dir, err)
	}
	if !st.IsDir() {
		return fmt.Errorf("install-agent: skill path is not a regular directory: %s", dir)
	}
	ownershipPath := filepath.Join(dir, offloadSkillOwner)
	data, _, exists, err := readAgentInstallFile(ownershipPath)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("install-agent: unmanaged skill directory %s; move it aside or merge it manually before installing", dir)
	}
	var owner skillOwnership
	if err := json.Unmarshal(data, &owner); err != nil || owner.Format != 1 || len(owner.SkillSHA256) != 64 {
		return fmt.Errorf("install-agent: invalid ownership metadata %s", ownershipPath)
	}
	skill, _, exists, err := readAgentInstallFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return err
	}
	if !exists || skillHash(skill) != owner.SkillSHA256 {
		return fmt.Errorf("install-agent: installed skill was modified: %s; preserve your changes before updating", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("install-agent: inspect %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.Name() != "SKILL.md" && entry.Name() != offloadSkillOwner {
			return fmt.Errorf("install-agent: unexpected file in managed skill directory %s: %s", dir, entry.Name())
		}
	}
	return nil
}

func skillHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func updateOffloadPolicy(existing []byte) ([]byte, error) {
	return updateOffloadPolicyMode(existing, config.ModeProactive)
}

func updateOffloadPolicyMode(existing []byte, mode string) ([]byte, error) {
	text := string(existing)
	begins, ends := strings.Count(text, offloadPolicyBegin), strings.Count(text, offloadPolicyEnd)
	if begins != ends || begins > 1 || strings.Count(text, "<!-- offload:") != begins+ends {
		return nil, errors.New("malformed or duplicate offload managed markers; repair them before installing")
	}
	newline := "\n"
	if strings.Contains(text, "\r\n") {
		newline = "\r\n"
	}
	block := strings.ReplaceAll(policyForMode(mode), "\n", newline)
	if begins == 0 {
		if len(text) == 0 {
			return []byte(block + newline), nil
		}
		separator := newline
		if !strings.HasSuffix(text, newline) {
			separator += newline
		}
		return []byte(text + separator + block + newline), nil
	}
	start, end := strings.Index(text, offloadPolicyBegin), strings.Index(text, offloadPolicyEnd)
	end += len(offloadPolicyEnd)
	if end <= start || !markerOwnLine(text, start, len(offloadPolicyBegin)) || !markerOwnLine(text, end-len(offloadPolicyEnd), len(offloadPolicyEnd)) {
		return nil, errors.New("offload managed markers must be ordered and on separate lines")
	}
	return []byte(text[:start] + block + text[end:]), nil
}

func markerOwnLine(text string, start, length int) bool {
	if start > 0 && text[start-1] != '\n' {
		return false
	}
	after := text[start+length:]
	return after == "" || strings.HasPrefix(after, "\n") || strings.HasPrefix(after, "\r\n")
}

func readAgentInstallFile(path string) ([]byte, os.FileMode, bool, error) {
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, 0o644, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("install-agent: inspect %s: %w", path, err)
	}
	if !st.Mode().IsRegular() {
		return nil, 0, false, fmt.Errorf("install-agent: refusing to replace non-regular file %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, false, fmt.Errorf("install-agent: read %s: %w", path, err)
	}
	return data, st.Mode().Perm(), true, nil
}

func plannedAgentFile(path string, old, updated []byte, mode os.FileMode, exists bool) agentInstallFile {
	status := "installed"
	if exists {
		status = "updated"
		if bytes.Equal(old, updated) {
			status = "unchanged"
		}
	}
	return agentInstallFile{path: path, data: updated, mode: mode, status: status}
}

func writeAgentInstallFile(f agentInstallFile) error {
	dir := filepath.Dir(f.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".offload-install-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(f.data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(f.mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), f.path)
}

func fail(w io.Writer, err error) int {
	fmt.Fprintln(w, "offload:", err)
	return 2
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// projectRoot is -d, or the current directory, and must exist.
func projectRoot(dir string) (string, error) {
	if dir == "" {
		return os.Getwd()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return "", fmt.Errorf("not a directory: %s", abs)
	}
	return abs, nil
}
