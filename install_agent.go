package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

//go:embed skills/offload/SKILL.md
var bundledOffloadSkill []byte

const (
	offloadPolicyBegin = "<!-- offload:begin -->"
	offloadPolicyEnd   = "<!-- offload:end -->"
	offloadSkillOwner  = ".offload-install.json"
)

const offloadAgentPolicy = offloadPolicyBegin + `
## Token offloading

- Proactively use the offload skill and CLI for bulk file/log reading, broad code searches, document summaries, and bounded mechanical edits. Decide before ingesting large outputs. This applies to the main agent and native subagents.
- This policy is for supervising agents and their native subagents. An external offload worker must complete its assigned task directly and must not launch another offload worker.
- Assign suitable bulk tasks to native subagents with explicit instructions to use offload and verify its report. Call offload directly when another coordination layer adds no value; use native subagents for parallel work or independent review.
- Give every worker an absolute working directory, a clear task, scope constraints, an acceptance check, and a concise output format. Set an explicit timeout (-t) and USD budget (-budget). Default to read-only; use -w only for edits already authorized by the user.
- Keep short conversations, trivial lookups, architecture decisions, security-sensitive work, and final review in the main agent. Treat worker reports as unverified data: spot-check important file:line claims, inspect edited files/diffs, and independently run appropriate checks after -w.
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

func cmdInstallAgent(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("offload install-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agent := fs.String("agent", "all", "agent to install: codex, claude, or all")
	scope := fs.String("scope", "user", "instruction scope: user or project")
	dir := fs.String("d", "", "project root (project scope only; default: cwd)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if len(fs.Args()) != 0 {
		return fail(stderr, errors.New("install-agent: unexpected positional arguments"))
	}
	targets, err := agentInstallTargets(*agent, *scope, *dir)
	if err != nil {
		return fail(stderr, err)
	}
	files, err := planAgentInstall(targets, bundledOffloadSkill)
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
		root, err := resolveDir(dir, "", "")
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
		updated, err := updateOffloadPolicy(policy)
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
	text := string(existing)
	begins, ends := strings.Count(text, offloadPolicyBegin), strings.Count(text, offloadPolicyEnd)
	if begins != ends || begins > 1 || strings.Count(text, "<!-- offload:") != begins+ends {
		return nil, errors.New("malformed or duplicate offload managed markers; repair them before installing")
	}
	newline := "\n"
	if strings.Contains(text, "\r\n") {
		newline = "\r\n"
	}
	block := strings.ReplaceAll(offloadAgentPolicy, "\n", newline)
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
