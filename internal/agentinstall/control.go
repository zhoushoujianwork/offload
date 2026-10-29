package agentinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/skills"
)

// ControlAgent is the machine-readable state of one agent's user-scope installation.
// Status is enabled, disabled, partial, blocked, or shadowed. "enabled" describes the
// files on disk only; a running agent session may not have loaded them (RestartRequired).
type ControlAgent struct {
	Agent           string `json:"agent"`
	Status          string `json:"status"`
	Enabled         bool   `json:"enabled"`
	Installed       bool   `json:"installed"`
	SkillState      string `json:"skill_state"`
	PolicyState     string `json:"policy_state"`
	SkillPath       string `json:"skill_path"`
	PolicyPath      string `json:"policy_path"`
	SkillCurrent    bool   `json:"skill_current"`
	PolicyCurrent   bool   `json:"policy_current"`
	Mode            string `json:"mode"`
	OverrideShadow  bool   `json:"override_shadowed"`
	CanEnable       bool   `json:"can_enable"`
	CanDisable      bool   `json:"can_disable"`
	Message         string `json:"message"`
	RestartRequired bool   `json:"restart_required"`
}

// ControlStatus is the JSON document printed by `offload agent status|enable|disable --json`.
type ControlStatus struct {
	SchemaVersion int            `json:"schema_version"`
	Scope         string         `json:"scope"`
	Mode          string         `json:"mode"`
	Agents        []ControlAgent `json:"agents"`
	Warnings      []string       `json:"warnings"`
}

var controlAgentNames = []string{"codex", "claude"}

// Test seams for fault injection.
var (
	controlWrite          = writeAgentInstallFile
	controlRemove         = os.Remove
	controlAfterPreflight = func() {}
)

// RunControl implements `offload agent [status|enable|disable]` (user scope only).
func RunControl(args []string, stdout, stderr io.Writer) int {
	action := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	if action != "status" && action != "enable" && action != "disable" {
		return fail(stderr, fmt.Errorf("usage: offload agent [status [--json] | enable|disable --agent codex|claude|all [--json]]"))
	}
	fs := flag.NewFlagSet("offload agent "+action, flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print machine-readable JSON")
	agent := new(string)
	if action != "status" {
		agent = fs.String("agent", "", "agent to change: codex, claude, or all (required)")
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if len(fs.Args()) != 0 {
		return fail(stderr, errors.New("agent: unexpected positional arguments"))
	}
	var selected []int
	if action != "status" {
		switch *agent {
		case "codex":
			selected = []int{0}
		case "claude":
			selected = []int{1}
		case "all":
			selected = []int{0, 1}
		case "":
			return fail(stderr, fmt.Errorf("agent %s: --agent codex|claude|all is required", action))
		default:
			return fail(stderr, fmt.Errorf("agent %s: unknown agent %q (want codex, claude, or all)", action, *agent))
		}
	}
	status, targets, err := controlSnapshot()
	if err != nil {
		return fail(stderr, err)
	}
	if action != "status" {
		for _, i := range selected {
			if status.Agents[i].Status == "blocked" {
				return fail(stderr, fmt.Errorf("agent %s: %s: %s; no changes made", action, controlAgentNames[i], status.Agents[i].Message))
			}
		}
		if action == "enable" {
			err = controlEnable(selected, targets, status.Mode)
		} else {
			err = controlDisable(selected, targets)
		}
		if err != nil {
			return fail(stderr, err)
		}
		if status, _, err = controlSnapshot(); err != nil {
			return fail(stderr, fmt.Errorf("agent %s: changes applied but status could not be read: %w", action, err))
		}
	}
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(status); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	writeControlText(stdout, status, action != "status")
	return 0
}

func writeControlText(w io.Writer, st ControlStatus, changed bool) {
	fmt.Fprintln(w, "Delegation mode:", st.Mode)
	for _, a := range st.Agents {
		fmt.Fprintf(w, "%s: %s (%s)\n", a.Agent, a.Status, a.Message)
	}
	for _, warning := range st.Warnings {
		fmt.Fprintln(w, "warning:", warning)
	}
	if changed {
		fmt.Fprintln(w, "Start a new agent session to load the updated instructions.")
	}
}

// ---- status ----

func controlSnapshot() (ControlStatus, []agentInstallTarget, error) {
	mode, err := config.LoadMode(config.Path())
	if err != nil {
		return ControlStatus{}, nil, err
	}
	targets, err := agentInstallTargets("all", "user", "")
	if err != nil {
		return ControlStatus{}, nil, err
	}
	st := ControlStatus{SchemaVersion: 1, Scope: "user", Mode: mode, Agents: []ControlAgent{}, Warnings: []string{}}
	for i, target := range targets {
		a, warnings := controlAgentStatus(controlAgentNames[i], target, mode)
		st.Agents = append(st.Agents, a)
		st.Warnings = append(st.Warnings, warnings...)
	}
	return st, targets, nil
}

type policyInfo struct {
	state      string // absent, no_block, managed, malformed, non_regular, unreadable
	data       []byte
	start, end int
}

func inspectPolicy(path string) policyInfo {
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return policyInfo{state: "absent"}
	}
	if err != nil {
		return policyInfo{state: "unreadable"}
	}
	if !st.Mode().IsRegular() {
		return policyInfo{state: "non_regular"}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return policyInfo{state: "unreadable"}
	}
	start, end, found, err := parseManagedBlock(string(data))
	switch {
	case err != nil:
		return policyInfo{state: "malformed", data: data}
	case !found:
		return policyInfo{state: "no_block", data: data}
	}
	return policyInfo{state: "managed", data: data, start: start, end: end}
}

// parseManagedBlock applies the same marker rules as updateOffloadPolicyMode.
func parseManagedBlock(text string) (start, end int, found bool, err error) {
	begins, ends := strings.Count(text, offloadPolicyBegin), strings.Count(text, offloadPolicyEnd)
	if begins != ends || begins > 1 || strings.Count(text, "<!-- offload:") != begins+ends {
		return 0, 0, false, errors.New("malformed or duplicate offload managed markers")
	}
	if begins == 0 {
		return 0, 0, false, nil
	}
	start, end = strings.Index(text, offloadPolicyBegin), strings.Index(text, offloadPolicyEnd)
	end += len(offloadPolicyEnd)
	if end <= start || !markerOwnLine(text, start, len(offloadPolicyBegin)) || !markerOwnLine(text, end-len(offloadPolicyEnd), len(offloadPolicyEnd)) {
		return 0, 0, false, errors.New("offload managed markers must be ordered and on separate lines")
	}
	return start, end, true, nil
}

// inspectSkill mirrors checkSkillOwnership but reports a state instead of an error.
// States: absent, managed, modified, unmanaged, symlink, not_directory, invalid, extra_files, unreadable.
func inspectSkill(dir string) (string, []byte) {
	st, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return "absent", nil
	}
	if err != nil {
		return "unreadable", nil
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return "symlink", nil
	}
	if !st.IsDir() {
		return "not_directory", nil
	}
	ownerPath := filepath.Join(dir, offloadSkillOwner)
	ost, err := os.Lstat(ownerPath)
	if os.IsNotExist(err) {
		return "unmanaged", nil
	}
	if err != nil {
		return "unreadable", nil
	}
	if !ost.Mode().IsRegular() {
		return "invalid", nil
	}
	ownerData, err := os.ReadFile(ownerPath)
	if err != nil {
		return "unreadable", nil
	}
	var owner skillOwnership
	if json.Unmarshal(ownerData, &owner) != nil || owner.Format != 1 || len(owner.SkillSHA256) != 64 {
		return "invalid", nil
	}
	skillPath := filepath.Join(dir, "SKILL.md")
	sst, err := os.Lstat(skillPath)
	if os.IsNotExist(err) || err == nil && !sst.Mode().IsRegular() {
		return "modified", nil
	}
	if err != nil {
		return "unreadable", nil
	}
	skill, err := os.ReadFile(skillPath)
	if err != nil {
		return "unreadable", nil
	}
	if skillHash(skill) != owner.SkillSHA256 {
		return "modified", nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "unreadable", nil
	}
	for _, e := range entries {
		if e.Name() != "SKILL.md" && e.Name() != offloadSkillOwner {
			return "extra_files", nil
		}
	}
	return "managed", skill
}

func modeOfBytes(data []byte, render func(string) []byte) string {
	for _, m := range []string{config.ModeBasic, config.ModeProactive} {
		if bytes.Equal(data, render(m)) {
			return m
		}
	}
	return ""
}

func controlAgentStatus(name string, target agentInstallTarget, mode string) (ControlAgent, []string) {
	var warnings []string
	skillState, skill := inspectSkill(target.skillDir)
	pol := inspectPolicy(target.policyPath)
	a := ControlAgent{
		Agent: name, SkillState: skillState, PolicyState: pol.state,
		SkillPath: target.skillDir, PolicyPath: target.policyPath, RestartRequired: true,
	}
	skillManaged, policyManaged := skillState == "managed", pol.state == "managed"
	a.Installed = skillManaged || policyManaged
	if skillManaged {
		a.SkillCurrent = bytes.Equal(skill, skillForMode(skills.Offload, mode))
		a.Mode = modeOfBytes(skill, func(m string) []byte { return skillForMode(skills.Offload, m) })
	}
	if policyManaged {
		block := strings.ReplaceAll(string(pol.data[pol.start:pol.end]), "\r\n", "\n")
		a.PolicyCurrent = block == policyForMode(mode)
		if a.Mode == "" {
			a.Mode = modeOfBytes([]byte(block), func(m string) []byte { return []byte(policyForMode(m)) })
		}
	}
	if name == "codex" {
		override := filepath.Join(filepath.Dir(target.policyPath), "AGENTS.override.md")
		if data, err := os.ReadFile(override); err == nil {
			a.OverrideShadow = len(bytes.TrimSpace(data)) > 0
		} else if !os.IsNotExist(err) {
			warnings = append(warnings, name+": AGENTS.override.md could not be inspected")
		}
	}
	blocked := (skillState != "absent" && !skillManaged) || (pol.state != "absent" && pol.state != "no_block" && !policyManaged)
	switch {
	case blocked:
		a.Status = "blocked"
		a.Message = fmt.Sprintf("blocked: skill %s, policy %s; resolve it manually, offload will not change it", skillState, pol.state)
	case skillManaged && policyManaged && a.OverrideShadow:
		a.Status = "shadowed"
		a.Message = "installed, but a non-empty AGENTS.override.md takes precedence over the managed policy"
	case skillManaged && policyManaged:
		a.Status, a.Enabled = "enabled", true
		a.Message = "installed and current"
		if !a.SkillCurrent || !a.PolicyCurrent {
			a.Message = "installed; update available"
		}
	case a.Installed:
		a.Status = "partial"
		a.Message = "partly installed; enable repairs it, disable removes the remaining offload parts"
	default:
		a.Status = "disabled"
		a.Message = "not installed"
	}
	if a.OverrideShadow {
		warnings = append(warnings, name+": a non-empty AGENTS.override.md takes precedence over AGENTS.md; the managed policy is not used until it is merged there")
	}
	a.CanEnable = !blocked && !(skillManaged && policyManaged && a.SkillCurrent && a.PolicyCurrent)
	a.CanDisable = !blocked && a.Installed
	return a, warnings
}

// ---- snapshots and transactions ----

type fileSnap struct {
	Path   string
	Exists bool
	Data   []byte
	Mode   os.FileMode
}

type agentSnap struct {
	Policy       fileSnap
	DirExists    bool
	DirMode      os.FileMode
	DirEntries   []string
	Skill, Owner fileSnap
}

func snapFile(path string) (fileSnap, error) {
	data, mode, exists, err := readAgentInstallFile(path)
	if err != nil {
		return fileSnap{}, err
	}
	return fileSnap{Path: path, Exists: exists, Data: data, Mode: mode}, nil
}

func sameFile(a, b fileSnap) bool {
	if a.Exists != b.Exists {
		return false
	}
	return !a.Exists || bytes.Equal(a.Data, b.Data) && a.Mode == b.Mode
}

func captureAgent(t agentInstallTarget) (agentSnap, error) {
	var s agentSnap
	var err error
	if s.Policy, err = snapFile(t.policyPath); err != nil {
		return s, err
	}
	st, err := os.Lstat(t.skillDir)
	switch {
	case os.IsNotExist(err):
	case err != nil:
		return s, err
	case !st.IsDir():
		return s, fmt.Errorf("skill path is not a regular directory: %s", t.skillDir)
	default:
		s.DirExists, s.DirMode = true, st.Mode().Perm()
		entries, err := os.ReadDir(t.skillDir)
		if err != nil {
			return s, err
		}
		for _, e := range entries {
			s.DirEntries = append(s.DirEntries, e.Name())
		}
		sort.Strings(s.DirEntries)
	}
	if s.Skill, err = snapFile(filepath.Join(t.skillDir, "SKILL.md")); err != nil {
		return s, err
	}
	s.Owner, err = snapFile(filepath.Join(t.skillDir, offloadSkillOwner))
	return s, err
}

func captureAll(sel []int, targets []agentInstallTarget) ([]agentSnap, error) {
	snaps := make([]agentSnap, len(sel))
	for k, i := range sel {
		s, err := captureAgent(targets[i])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", controlAgentNames[i], err)
		}
		snaps[k] = s
	}
	return snaps, nil
}

// verifyUnchanged refuses when an agent's files differ from the captured pre-image.
func verifyUnchanged(i int, target agentInstallTarget, pre agentSnap) error {
	cur, err := captureAgent(target)
	if err != nil || !reflect.DeepEqual(cur, pre) {
		return fmt.Errorf("%s changed while preparing", controlAgentNames[i])
	}
	return nil
}

// txnOp records one mutation with the image this process expects to have left behind.
// Rollback only reverts a path while it still matches that image.
type txnOp struct {
	dir       bool
	path      string
	pre, post fileSnap    // files
	preDir    bool        // directories: existed before
	dirMode   os.FileMode // directories: mode to recreate
	done      bool
}

type txn struct{ ops []*txnOp }

func (x *txn) writeFile(f agentInstallFile, pre fileSnap) error {
	op := &txnOp{path: f.path, pre: pre, post: fileSnap{Path: f.path, Exists: true, Data: f.data, Mode: f.mode}}
	x.ops = append(x.ops, op)
	if err := controlWrite(f); err != nil {
		return fmt.Errorf("write %s: %w", f.path, err)
	}
	op.done = true
	return nil
}

func (x *txn) removeFile(pre fileSnap) error {
	op := &txnOp{path: pre.Path, pre: pre, post: fileSnap{Path: pre.Path}}
	x.ops = append(x.ops, op)
	if err := controlRemove(pre.Path); err != nil {
		return fmt.Errorf("remove %s: %w", pre.Path, err)
	}
	op.done = true
	return nil
}

// noteCreatedDir records a directory that a later write will create.
func (x *txn) noteCreatedDir(path string) {
	x.ops = append(x.ops, &txnOp{dir: true, path: path})
}

func (x *txn) removeDir(path string, mode os.FileMode) error {
	op := &txnOp{dir: true, path: path, preDir: true, dirMode: mode}
	x.ops = append(x.ops, op)
	if err := controlRemove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	op.done = true
	return nil
}

type rollbackIssue struct {
	path, msg string
}

// undo reverts operations in reverse order. A path is restored only when it still holds
// this process's own post-image (or is already back at its pre-image); anything else was
// changed by someone else and is left untouched.
func (x *txn) undo() []rollbackIssue {
	var issues []rollbackIssue
	skipped := map[string]bool{} // directories whose children must not be touched
	conflict := func(path string) {
		issues = append(issues, rollbackIssue{path, "changed by another process, left untouched"})
	}
	for k := len(x.ops) - 1; k >= 0; k-- {
		op := x.ops[k]
		if skipped[filepath.Dir(op.path)] {
			issues = append(issues, rollbackIssue{op.path, "parent directory was changed by another process, left untouched"})
			continue
		}
		if op.dir {
			st, err := os.Lstat(op.path)
			exists := err == nil
			if err != nil && !os.IsNotExist(err) {
				issues = append(issues, rollbackIssue{op.path, err.Error()})
				skipped[op.path] = true
				continue
			}
			if exists && !st.IsDir() {
				conflict(op.path)
				skipped[op.path] = true
				continue
			}
			switch {
			case op.preDir && !exists:
				if err := os.Mkdir(op.path, op.dirMode); err != nil {
					issues = append(issues, rollbackIssue{op.path, "recreate directory: " + err.Error()})
					skipped[op.path] = true
				}
			case op.preDir && exists && op.done:
				conflict(op.path) // we removed it; someone recreated it
				skipped[op.path] = true
			case !op.preDir && exists:
				if err := controlRemove(op.path); err != nil && !os.IsNotExist(err) {
					issues = append(issues, rollbackIssue{op.path, "remove created directory: " + err.Error()})
				}
			}
			continue
		}
		cur, err := snapFile(op.path)
		switch {
		case err != nil:
			issues = append(issues, rollbackIssue{op.path, "inspect: " + err.Error()})
		case sameFile(cur, op.pre):
			// already back at the original state
		case !sameFile(cur, op.post):
			conflict(op.path)
		case op.pre.Exists:
			if err := controlWrite(agentInstallFile{path: op.path, data: op.pre.Data, mode: op.pre.Mode}); err != nil {
				issues = append(issues, rollbackIssue{op.path, "restore: " + err.Error()})
			}
		default:
			if err := controlRemove(op.path); err != nil && !os.IsNotExist(err) {
				issues = append(issues, rollbackIssue{op.path, "remove created file: " + err.Error()})
			}
		}
	}
	return issues
}

// fail rolls back and returns an error that says honestly whether the rollback finished.
func (x *txn) fail(action string, cause error) error {
	issues := x.undo()
	if len(issues) == 0 {
		return fmt.Errorf("agent %s: %w; all changes rolled back", action, cause)
	}
	var details []string
	for _, is := range issues {
		details = append(details, is.path+": "+is.msg)
	}
	return fmt.Errorf("agent %s: %w; rollback incomplete (%s); state may be partial and conflicting paths were not overwritten; run offload agent status", action, cause, strings.Join(details, "; "))
}

// ---- enable ----

func controlEnable(sel []int, targets []agentInstallTarget, mode string) error {
	pre, err := captureAll(sel, targets)
	if err != nil {
		return fmt.Errorf("agent enable: %w; no changes made", err)
	}
	chosen := make([]agentInstallTarget, len(sel))
	for k, i := range sel {
		chosen[k] = targets[i]
	}
	files, err := planAgentInstallMode(chosen, skills.Offload, mode)
	if err != nil {
		return fmt.Errorf("agent enable: %w; no changes made", err)
	}
	controlAfterPreflight()
	for k, i := range sel {
		if err := verifyUnchanged(i, targets[i], pre[k]); err != nil {
			return fmt.Errorf("agent enable: %w; no changes made", err)
		}
	}
	var x txn
	for k, i := range sel {
		if err := verifyUnchanged(i, targets[i], pre[k]); err != nil {
			return x.fail("enable", err)
		}
		// planAgentInstallMode yields policy, SKILL.md, owner for each target, in order.
		for n, f := range files[3*k : 3*k+3] {
			if f.status == "unchanged" {
				continue
			}
			before := []fileSnap{pre[k].Policy, pre[k].Skill, pre[k].Owner}[n]
			if n == 1 && !pre[k].DirExists {
				x.noteCreatedDir(targets[i].skillDir)
			}
			if err := x.writeFile(f, before); err != nil {
				return x.fail("enable", err)
			}
		}
	}
	return nil
}

// ---- disable ----

// removeManagedBlock deletes the marker block and the line break that ends it. Every
// other byte, including blank lines before the block, is kept: whether those lines came
// from the installer or the user cannot be known.
func removeManagedBlock(text string, start, end int) string {
	after := text[end:]
	switch {
	case strings.HasPrefix(after, "\r\n"):
		after = after[2:]
	case strings.HasPrefix(after, "\n"):
		after = after[1:]
	}
	return text[:start] + after
}

func controlDisable(sel []int, targets []agentInstallTarget) error {
	pre, err := captureAll(sel, targets)
	if err != nil {
		return fmt.Errorf("agent disable: %w; no changes made", err)
	}
	type step struct {
		policy    *agentInstallFile
		removeDir bool
	}
	steps := make([]step, len(sel))
	for k, i := range sel {
		s := pre[k]
		if info := inspectPolicy(targets[i].policyPath); info.state == "managed" {
			if !bytes.Equal(info.data, s.Policy.Data) {
				return fmt.Errorf("agent disable: %s policy changed while preparing; no changes made", controlAgentNames[i])
			}
			updated := removeManagedBlock(string(info.data), info.start, info.end)
			steps[k].policy = &agentInstallFile{path: s.Policy.Path, data: []byte(updated), mode: s.Policy.Mode, status: "updated"}
		}
		if state, _ := inspectSkill(targets[i].skillDir); state == "managed" {
			steps[k].removeDir = true
		}
	}
	controlAfterPreflight()
	for k, i := range sel {
		if err := verifyUnchanged(i, targets[i], pre[k]); err != nil {
			return fmt.Errorf("agent disable: %w; no changes made", err)
		}
	}
	var x txn
	for k, i := range sel {
		if err := verifyUnchanged(i, targets[i], pre[k]); err != nil {
			return x.fail("disable", err)
		}
		if p := steps[k].policy; p != nil {
			if err := x.writeFile(*p, pre[k].Policy); err != nil {
				return x.fail("disable", err)
			}
		}
		if steps[k].removeDir {
			for _, f := range []fileSnap{pre[k].Skill, pre[k].Owner} {
				if err := x.removeFile(f); err != nil {
					return x.fail("disable", err)
				}
			}
			if err := x.removeDir(targets[i].skillDir, pre[k].DirMode); err != nil {
				return x.fail("disable", err)
			}
		}
	}
	return nil
}
