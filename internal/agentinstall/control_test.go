package agentinstall

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/skills"
)

func runControl(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := RunControl(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func controlState(t *testing.T, args ...string) ControlStatus {
	t.Helper()
	code, out, errOut := runControl(append(args, "--json")...)
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	var st ControlStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("bad json %q: %v", out, err)
	}
	return st
}

func controlTargets(t *testing.T) []agentInstallTarget {
	t.Helper()
	targets, err := agentInstallTargets("all", "user", "")
	if err != nil {
		t.Fatal(err)
	}
	return targets
}

type treeSnap map[string]string

// snapTree records every file, dir and symlink under roots with content and mode.
func snapTree(t *testing.T, roots ...string) treeSnap {
	t.Helper()
	snap := treeSnap{}
	for _, root := range roots {
		filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() {
				return nil // empty shared parents (~/.agents/skills) may remain after rollback
			}
			entry := info.Mode().String()
			if info.Mode().IsRegular() {
				data, _ := os.ReadFile(p)
				entry += " " + string(data)
			} else if info.Mode()&os.ModeSymlink != 0 {
				target, _ := os.Readlink(p)
				entry += " -> " + target
			}
			snap[p] = entry
			return nil
		})
	}
	return snap
}

func assertSameTree(t *testing.T, before, after treeSnap) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("file set changed: before=%d after=%d", len(before), len(after))
	}
	for p, v := range before {
		if after[p] != v {
			t.Fatalf("%s changed", p)
		}
	}
}

func homeRoots(home string) []string {
	return []string{filepath.Join(home, ".codex"), filepath.Join(home, ".claude"), filepath.Join(home, ".agents")}
}

func TestControlStatusEmptyIsDisabledProactiveAndNonNil(t *testing.T) {
	home := modeHome(t)
	code, out, errOut := runControl("status", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	if !strings.Contains(out, `"warnings":[]`) || !strings.Contains(out, `"agents":[{`) {
		t.Fatalf("arrays must be non-nil: %s", out)
	}
	var st ControlStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatal(err)
	}
	if st.SchemaVersion != 1 || st.Scope != "user" || st.Mode != config.ModeProactive || len(st.Agents) != 2 {
		t.Fatalf("%+v", st)
	}
	for i, name := range []string{"codex", "claude"} {
		a := st.Agents[i]
		if a.Agent != name || a.Status != "disabled" || a.Enabled || a.Installed || !a.CanEnable || a.CanDisable || !a.RestartRequired || a.SkillState != "absent" || a.PolicyState != "absent" {
			t.Fatalf("%+v", a)
		}
	}
	for _, root := range homeRoots(home) {
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("status created %s", root)
		}
	}
	if code, out, _ = runControl(); code != 0 || !strings.Contains(out, "codex: disabled") {
		t.Fatalf("default is status: %d %q", code, out)
	}
}

func TestControlEnableDisableRoundTripPreservesOutsideBytes(t *testing.T) {
	home := modeHome(t)
	targets := controlTargets(t)
	originals := [][]byte{[]byte("# Codex rules\r\n\r\nKeep.\r\n"), []byte("# Claude rules\n\nKeep.\n\n")}
	for i, target := range targets {
		installTestWrite(t, target.policyPath, originals[i], 0o600)
	}
	for round := 0; round < 2; round++ {
		st := controlState(t, "enable", "--agent", "all")
		for _, a := range st.Agents {
			if a.Status != "enabled" || !a.Enabled || !a.SkillCurrent || !a.PolicyCurrent || a.Mode != "proactive" || a.CanEnable || !a.CanDisable || !a.RestartRequired {
				t.Fatalf("round %d: %+v", round, a)
			}
		}
		for i, target := range targets {
			got := installTestRead(t, target.policyPath)
			if !bytes.HasPrefix(got, originals[i]) || !bytes.Contains(got, []byte(offloadPolicyBegin)) {
				t.Fatalf("enabled policy %d: %q", i, got)
			}
		}
		st = controlState(t, "disable", "--agent", "all")
		for _, a := range st.Agents {
			if a.Status != "disabled" || a.Installed || a.CanDisable || !a.CanEnable {
				t.Fatalf("round %d: %+v", round, a)
			}
		}
		for i, target := range targets {
			// User text is never removed; only the installer's blank separator may remain.
			got := installTestRead(t, target.policyPath)
			if !bytes.HasPrefix(got, originals[i]) || len(bytes.TrimSpace(got[len(originals[i]):])) != 0 {
				t.Fatalf("round %d policy %d not preserved: %q", round, i, got)
			}
			originals[i] = got // later rounds must be stable from here
			if st, err := os.Stat(target.policyPath); err != nil || st.Mode().Perm() != 0o600 {
				t.Fatalf("mode: %v %v", st, err)
			}
			if _, err := os.Lstat(target.skillDir); !os.IsNotExist(err) {
				t.Fatalf("skill dir remains: %v", err)
			}
		}
	}
	// Parents installed by the first enable are shared locations and stay.
	if _, err := os.Stat(filepath.Join(home, ".agents", "skills")); err != nil {
		t.Fatal(err)
	}
}

func TestControlDisableLeavesEmptyPolicyFileAndUnrelatedBlocks(t *testing.T) {
	modeHome(t)
	targets := controlTargets(t)
	if st := controlState(t, "enable", "--agent", "codex"); st.Agents[0].Status != "enabled" || st.Agents[1].Status != "disabled" {
		t.Fatalf("%+v", st.Agents)
	}
	policy := targets[0].policyPath
	if got := installTestRead(t, policy); !strings.HasPrefix(string(got), offloadPolicyBegin) {
		t.Fatalf("expected block-only file: %q", got)
	}
	controlState(t, "disable", "--agent", "codex")
	if got := installTestRead(t, policy); len(got) != 0 {
		t.Fatalf("expected empty file: %q", got)
	}
	// Foreign blocks before and after the managed block stay byte-identical.
	foreign := "<!-- other:begin -->\nmine\n<!-- other:end -->\n"
	installTestWrite(t, policy, []byte(foreign), 0o644)
	controlState(t, "enable", "--agent", "codex")
	raw := string(installTestRead(t, policy))
	installTestWrite(t, policy, []byte(raw+"tail after block\n"), 0o644)
	controlState(t, "disable", "--agent", "codex")
	if got := string(installTestRead(t, policy)); got != foreign+"\ntail after block\n" {
		t.Fatalf("got %q", got)
	}
}

func TestControlStateMatrix(t *testing.T) {
	home := modeHome(t)
	targets := controlTargets(t)
	codex := targets[0]

	// Outdated skill and policy: managed by an older source stays enabled with update available.
	old := []byte("# old skill\n")
	installTestWrite(t, filepath.Join(codex.skillDir, "SKILL.md"), old, 0o644)
	owner, _ := json.Marshal(skillOwnership{Format: 1, SkillSHA256: skillHash(old)})
	installTestWrite(t, filepath.Join(codex.skillDir, offloadSkillOwner), owner, 0o644)
	installTestWrite(t, codex.policyPath, []byte("before\n"+offloadPolicyBegin+"\nold policy\n"+offloadPolicyEnd+"\nafter\n"), 0o644)
	a := controlState(t, "status").Agents[0]
	if a.Status != "enabled" || !a.Enabled || a.SkillCurrent || a.PolicyCurrent || !a.CanEnable || !a.CanDisable || !strings.Contains(a.Message, "update available") {
		t.Fatalf("outdated: %+v", a)
	}
	controlState(t, "enable", "--agent", "codex")
	a = controlState(t, "status").Agents[0]
	if a.Status != "enabled" || !a.SkillCurrent || !a.PolicyCurrent || a.CanEnable {
		t.Fatalf("updated: %+v", a)
	}
	if got := string(installTestRead(t, codex.policyPath)); !strings.HasPrefix(got, "before\n") || !strings.HasSuffix(got, "\nafter\n") {
		t.Fatalf("outside bytes lost: %q", got)
	}

	// Mode switch makes the installed files outdated relative to the saved mode.
	if err := config.SetMode(config.Path(), config.ModeBasic); err != nil {
		t.Fatal(err)
	}
	st := controlState(t, "status")
	if st.Mode != "basic" || st.Agents[0].Mode != "proactive" || st.Agents[0].SkillCurrent || st.Agents[0].PolicyCurrent {
		t.Fatalf("%+v", st)
	}
	controlState(t, "enable", "--agent", "codex")
	if a = controlState(t, "status").Agents[0]; a.Mode != "basic" || !a.SkillCurrent || !a.PolicyCurrent {
		t.Fatalf("%+v", a)
	}

	// Override shadows, is never modified.
	override := filepath.Join(filepath.Dir(codex.policyPath), "AGENTS.override.md")
	installTestWrite(t, override, []byte("# preferred\n"), 0o600)
	st = controlState(t, "status")
	if a = st.Agents[0]; a.Status != "shadowed" || a.Enabled || !a.OverrideShadow || !a.CanDisable || len(st.Warnings) != 1 {
		t.Fatalf("%+v %v", a, st.Warnings)
	}
	controlState(t, "disable", "--agent", "codex")
	if got := installTestRead(t, override); string(got) != "# preferred\n" {
		t.Fatal("override modified")
	}
	installTestWrite(t, override, []byte(" \n"), 0o600)
	if a = controlState(t, "status").Agents[0]; a.OverrideShadow {
		t.Fatalf("blank override must not shadow: %+v", a)
	}

	// Partial: skill only, then policy only. Enable repairs; disable removes the owned part.
	claude := targets[1]
	skill := skillForMode(skills.Offload, config.ModeBasic)
	installTestWrite(t, filepath.Join(claude.skillDir, "SKILL.md"), skill, 0o644)
	owner, _ = json.Marshal(skillOwnership{Format: 1, SkillSHA256: skillHash(skill)})
	installTestWrite(t, filepath.Join(claude.skillDir, offloadSkillOwner), owner, 0o644)
	if a = controlState(t, "status").Agents[1]; a.Status != "partial" || a.Enabled || !a.Installed || !a.CanEnable || !a.CanDisable {
		t.Fatalf("partial skill: %+v", a)
	}
	controlState(t, "disable", "--agent", "claude")
	if _, err := os.Lstat(claude.skillDir); !os.IsNotExist(err) {
		t.Fatal("partial skill not removed")
	}
	installTestWrite(t, claude.policyPath, []byte("keep\n"+policyForMode(config.ModeBasic)+"\n"), 0o644)
	if a = controlState(t, "status").Agents[1]; a.Status != "partial" || !a.PolicyCurrent {
		t.Fatalf("partial policy: %+v", a)
	}
	if a = controlState(t, "enable", "--agent", "claude").Agents[1]; a.Status != "enabled" {
		t.Fatalf("repair: %+v", a)
	}
	_ = home
}

func TestControlBlockedStatesAreRefusedWithoutEffects(t *testing.T) {
	cases := map[string]func(t *testing.T, tg agentInstallTarget){
		"modified": func(t *testing.T, tg agentInstallTarget) {
			controlState(t, "enable", "--agent", "claude")
			installTestWrite(t, filepath.Join(tg.skillDir, "SKILL.md"), []byte("edited"), 0o644)
		},
		"unmanaged": func(t *testing.T, tg agentInstallTarget) {
			installTestWrite(t, filepath.Join(tg.skillDir, "SKILL.md"), []byte("mine"), 0o644)
		},
		"extra_files": func(t *testing.T, tg agentInstallTarget) {
			controlState(t, "enable", "--agent", "claude")
			installTestWrite(t, filepath.Join(tg.skillDir, "notes.txt"), []byte("mine"), 0o644)
		},
		"symlink": func(t *testing.T, tg agentInstallTarget) {
			ext := t.TempDir()
			installTestWrite(t, filepath.Join(ext, "SKILL.md"), []byte("shared"), 0o644)
			if err := os.MkdirAll(filepath.Dir(tg.skillDir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(ext, tg.skillDir); err != nil {
				t.Fatal(err)
			}
		},
		"malformed": func(t *testing.T, tg agentInstallTarget) {
			installTestWrite(t, tg.policyPath, []byte("x\n"+offloadPolicyBegin+"\nno end\n"), 0o644)
		},
		"policy_symlink": func(t *testing.T, tg agentInstallTarget) {
			ext := filepath.Join(t.TempDir(), "shared.md")
			installTestWrite(t, ext, []byte("shared\n"), 0o644)
			if err := os.MkdirAll(filepath.Dir(tg.policyPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(ext, tg.policyPath); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		for _, action := range []string{"enable", "disable"} {
			t.Run(name+"/"+action, func(t *testing.T) {
				home := modeHome(t)
				targets := controlTargets(t)
				setup(t, targets[1])
				// Codex is healthy and enabled/disabled normally; "all" must not touch it when claude is blocked.
				if action == "disable" {
					controlState(t, "enable", "--agent", "codex")
				}
				before := snapTree(t, homeRoots(home)...)
				a := controlState(t, "status").Agents[1]
				if a.Status != "blocked" || a.CanEnable || a.CanDisable {
					t.Fatalf("%+v", a)
				}
				for _, agent := range []string{"claude", "all"} {
					code, out, errOut := runControl(action, "--agent", agent, "--json")
					if code != 2 || out != "" || !strings.Contains(errOut, "no changes made") || strings.Contains(errOut, "Token offloading") {
						t.Fatalf("%s: code=%d out=%q err=%q", agent, code, out, errOut)
					}
					assertSameTree(t, before, snapTree(t, homeRoots(home)...))
				}
			})
		}
	}
}

func TestControlRejectsBadArguments(t *testing.T) {
	home := modeHome(t)
	for _, args := range [][]string{{"enable"}, {"disable"}, {"enable", "--agent", "gemini"}, {"bogus"}, {"status", "extra"}, {"status", "--agent", "all"}, {"enable", "--agent", "all", "extra"}} {
		if code, out, _ := runControl(args...); code != 2 || out != "" {
			t.Fatalf("%v: code=%d out=%q", args, code, out)
		}
	}
	for _, root := range homeRoots(home) {
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("created %s", root)
		}
	}
}

func TestControlHonorsConfigRootsAndNeverReadsDefaults(t *testing.T) {
	home := modeHome(t)
	codex, claude := filepath.Join(t.TempDir(), "codex"), filepath.Join(t.TempDir(), "claude")
	t.Setenv("CODEX_HOME", codex)
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	st := controlState(t, "enable", "--agent", "all")
	if st.Agents[0].PolicyPath != filepath.Join(codex, "AGENTS.md") || st.Agents[0].SkillPath != filepath.Join(home, ".agents", "skills", "offload") || st.Agents[1].PolicyPath != filepath.Join(claude, "CLAUDE.md") || st.Agents[1].SkillPath != filepath.Join(claude, "skills", "offload") {
		t.Fatalf("%+v", st.Agents)
	}
	controlState(t, "disable", "--agent", "all")
	for _, fallback := range []string{filepath.Join(home, ".codex"), filepath.Join(home, ".claude")} {
		if _, err := os.Stat(fallback); !os.IsNotExist(err) {
			t.Fatalf("fallback touched: %s", fallback)
		}
	}
	// A Codex override in the configured root shadows; one in the default root is irrelevant.
	controlState(t, "enable", "--agent", "codex")
	installTestWrite(t, filepath.Join(home, ".codex", "AGENTS.override.md"), []byte("x"), 0o644)
	if a := controlState(t, "status").Agents[0]; a.Status != "enabled" {
		t.Fatalf("%+v", a)
	}
}

func TestControlStatusIgnoresConfigSecretsAndRejectsBadMode(t *testing.T) {
	modeHome(t)
	marker := filepath.Join(t.TempDir(), "ran")
	cfg := `{"delegation_mode":"basic","profiles":{"p":{"token_cmd":"touch ` + marker + `","api_key":"SECRET-VALUE"}}}`
	installTestWrite(t, config.Path(), []byte(cfg), 0o600)
	code, out, errOut := runControl("status", "--json")
	if code != 0 || strings.Contains(out+errOut, "SECRET-VALUE") || strings.Contains(out, "token_cmd") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("token_cmd executed")
	}
	installTestWrite(t, config.Path(), []byte(`{"delegation_mode":"weird"}`), 0o600)
	if code, out, _ = runControl("status", "--json"); code != 2 || out != "" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestControlEnableDoesNotPrintPolicyBody(t *testing.T) {
	modeHome(t)
	code, out, errOut := runControl("enable", "--agent", "all")
	if code != 0 || errOut != "" || strings.Contains(out, "Token offloading") || strings.Contains(out, "offload:begin") || !strings.Contains(out, "Start a new agent session") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
}

func TestControlDisableRefusesWhenTargetChangesAfterPreflight(t *testing.T) {
	home := modeHome(t)
	targets := controlTargets(t)
	controlState(t, "enable", "--agent", "all")
	before := snapTree(t, homeRoots(home)...)
	controlAfterPreflight = func() {
		data := installTestRead(t, targets[1].policyPath)
		installTestWrite(t, targets[1].policyPath, append(data, []byte("concurrent edit\n")...), 0o644)
	}
	defer func() { controlAfterPreflight = func() {} }()
	code, _, errOut := runControl("disable", "--agent", "all")
	if code != 2 || !strings.Contains(errOut, "changed while preparing") || !strings.Contains(errOut, "no changes made") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	after := snapTree(t, homeRoots(home)...)
	delete(after, "")
	if !strings.Contains(after[targets[1].policyPath], "concurrent edit") {
		t.Fatal("concurrent edit lost")
	}
	for p, v := range before {
		if p != targets[1].policyPath && after[p] != v {
			t.Fatalf("%s changed", p)
		}
	}
}

func TestControlDisableRollsBackOnIOFailure(t *testing.T) {
	home := modeHome(t)
	controlState(t, "enable", "--agent", "all")
	before := snapTree(t, homeRoots(home)...)
	failing := filepath.Join(controlTargets(t)[1].skillDir, offloadSkillOwner)
	controlRemove = func(p string) error {
		if p == failing {
			return errors.New("injected remove failure")
		}
		return os.Remove(p)
	}
	defer func() { controlRemove = os.Remove }()
	code, out, errOut := runControl("disable", "--agent", "all", "--json")
	if code != 2 || out != "" || !strings.Contains(errOut, "injected remove failure") || !strings.Contains(errOut, "rolled back") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	assertSameTree(t, before, snapTree(t, homeRoots(home)...))
	controlRemove = os.Remove
	for _, a := range controlState(t, "status").Agents {
		if a.Status != "enabled" {
			t.Fatalf("%+v", a)
		}
	}
}

func TestControlDisableReportsFailedRollbackHonestly(t *testing.T) {
	home := modeHome(t)
	controlState(t, "enable", "--agent", "all")
	targets := controlTargets(t)
	failing := filepath.Join(targets[1].skillDir, offloadSkillOwner)
	controlRemove = func(p string) error {
		if p == failing {
			return errors.New("injected remove failure")
		}
		return os.Remove(p)
	}
	writes := 0
	controlWrite = func(f agentInstallFile) error {
		writes++
		if writes > 2 { // two policy writes during disable succeed; every restore fails
			return errors.New("injected write failure")
		}
		return writeAgentInstallFile(f)
	}
	defer func() { controlRemove, controlWrite = os.Remove, writeAgentInstallFile }()
	code, out, errOut := runControl("disable", "--agent", "all", "--json")
	if code != 2 || out != "" || !strings.Contains(errOut, "rollback incomplete") || !strings.Contains(errOut, "partial") || strings.Contains(errOut, "all changes rolled back") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	controlRemove, controlWrite = os.Remove, writeAgentInstallFile
	// Status is still available for a UI to refetch and must not claim both are disabled.
	st := controlState(t, "status")
	if st.Agents[0].Status == "disabled" && st.Agents[1].Status == "disabled" {
		t.Fatalf("claimed disabled: %+v", st.Agents)
	}
	_ = home
}

func TestControlEnableRollsBackOnInstallFailure(t *testing.T) {
	home := modeHome(t)
	claude := filepath.Join(t.TempDir(), "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	codexPolicy := filepath.Join(home, ".codex", "AGENTS.md")
	original := []byte("# Codex\n")
	installTestWrite(t, codexPolicy, original, 0o600)
	// Codex installs first; the read-only Claude root then makes the installer fail midway.
	if err := os.MkdirAll(claude, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(claude, 0o755)
	if f, err := os.CreateTemp(claude, "probe"); err == nil {
		f.Close()
		t.Skip("directory permissions are not enforced")
	}
	before := snapTree(t, homeRoots(home)...)
	code, out, errOut := runControl("enable", "--agent", "all", "--json")
	if code != 2 || out != "" || !strings.Contains(errOut, "rolled back") || strings.Contains(errOut, "Token offloading") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	assertSameTree(t, before, snapTree(t, homeRoots(home)...))
	if got := installTestRead(t, codexPolicy); !bytes.Equal(got, original) {
		t.Fatalf("codex policy not restored: %q", got)
	}
}

func TestControlDisableKeepsLiteralPrefixBlankLinesBeforeEOFBlock(t *testing.T) {
	for _, nl := range []string{"\n", "\r\n"} {
		modeHome(t)
		policy := controlTargets(t)[0].policyPath
		prefix := "# mine" + nl + nl + nl + nl
		installTestWrite(t, policy, []byte(prefix+strings.ReplaceAll(policyForMode(config.ModeProactive), "\n", nl)+nl), 0o640)
		a := controlState(t, "status").Agents[0]
		if a.PolicyState != "managed" || !a.PolicyCurrent {
			t.Fatalf("%+v", a)
		}
		controlState(t, "disable", "--agent", "codex")
		if got := installTestRead(t, policy); string(got) != prefix {
			t.Fatalf("nl=%q got %q", nl, got)
		}
		if st, _ := os.Stat(policy); st.Mode().Perm() != 0o640 {
			t.Fatalf("mode %v", st.Mode())
		}
		// Block without trailing line break at EOF.
		installTestWrite(t, policy, []byte(prefix+strings.ReplaceAll(policyForMode(config.ModeProactive), "\n", nl)), 0o640)
		controlState(t, "disable", "--agent", "codex")
		if got := installTestRead(t, policy); string(got) != prefix {
			t.Fatalf("no-eol nl=%q got %q", nl, got)
		}
	}
}

func TestControlEnableConcurrentEditBeforeWriteIsNeverOverwritten(t *testing.T) {
	home := modeHome(t)
	targets := controlTargets(t)
	installTestWrite(t, targets[0].policyPath, []byte("# codex\n"), 0o644)
	before := snapTree(t, homeRoots(home)...)
	edit := "# codex\nconcurrent edit\n"
	controlAfterPreflight = func() { installTestWrite(t, targets[0].policyPath, []byte(edit), 0o644) }
	defer func() { controlAfterPreflight = func() {} }()
	code, out, errOut := runControl("enable", "--agent", "all", "--json")
	if code != 2 || out != "" || !strings.Contains(errOut, "changed while preparing") || !strings.Contains(errOut, "no changes made") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if got := string(installTestRead(t, targets[0].policyPath)); got != edit {
		t.Fatalf("concurrent edit lost: %q", got)
	}
	after := snapTree(t, homeRoots(home)...)
	delete(before, targets[0].policyPath)
	delete(after, targets[0].policyPath)
	assertSameTree(t, before, after)

	// A target that becomes unmanaged concurrently is refused before any write, and the
	// newly created foreign file is not deleted.
	controlAfterPreflight = func() { installTestWrite(t, filepath.Join(targets[1].skillDir, "SKILL.md"), []byte("theirs"), 0o644) }
	code, _, errOut = runControl("enable", "--agent", "all")
	if code != 2 || !strings.Contains(errOut, "no changes made") {
		t.Fatalf("code=%d err=%q", code, errOut)
	}
	if got := string(installTestRead(t, filepath.Join(targets[1].skillDir, "SKILL.md"))); got != "theirs" {
		t.Fatalf("foreign file changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(targets[0].skillDir, "SKILL.md")); !os.IsNotExist(err) {
		t.Fatal("codex was written despite refusal")
	}
}

func TestControlRollbackLeavesConcurrentlyChangedPathsUntouched(t *testing.T) {
	home := modeHome(t)
	targets := controlTargets(t)
	controlState(t, "enable", "--agent", "all")
	// Disable writes codex policy, then claude policy; fail removing claude's owner file.
	// After our own writes, another process edits codex's policy and recreates claude's SKILL.md.
	failing := filepath.Join(targets[1].skillDir, offloadSkillOwner)
	codexEdited := "# codex rewritten by someone else\n"
	claudeSkillTheirs := "someone else's skill"
	controlRemove = func(p string) error {
		if p == failing {
			installTestWrite(t, targets[0].policyPath, []byte(codexEdited), 0o644)
			// SKILL.md was already removed by us; another process recreates it.
			installTestWrite(t, filepath.Join(targets[1].skillDir, "SKILL.md"), []byte(claudeSkillTheirs), 0o644)
			return errors.New("injected remove failure")
		}
		return os.Remove(p)
	}
	defer func() { controlRemove = os.Remove }()
	code, out, errOut := runControl("disable", "--agent", "all", "--json")
	if code != 2 || out != "" || !strings.Contains(errOut, "injected remove failure") || !strings.Contains(errOut, "rollback incomplete") || !strings.Contains(errOut, "partial") || strings.Contains(errOut, "all changes rolled back") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if got := string(installTestRead(t, targets[0].policyPath)); got != codexEdited {
		t.Fatalf("codex concurrent edit overwritten: %q", got)
	}
	if got := string(installTestRead(t, filepath.Join(targets[1].skillDir, "SKILL.md"))); got != claudeSkillTheirs {
		t.Fatalf("claude skill overwritten: %q", got)
	}
	for _, p := range []string{targets[0].policyPath, filepath.Join(targets[1].skillDir, "SKILL.md")} {
		if !strings.Contains(errOut, p) {
			t.Fatalf("error does not name conflicting path %s: %q", p, errOut)
		}
	}
	// Untouched-by-others paths were still restored: claude's policy block is back.
	if got := string(installTestRead(t, targets[1].policyPath)); !strings.Contains(got, offloadPolicyBegin) {
		t.Fatalf("claude policy not restored: %q", got)
	}
	_ = home
}

func TestControlEnableRollbackKeepsConcurrentEditAfterOwnWrite(t *testing.T) {
	home := modeHome(t)
	targets := controlTargets(t)
	claude := filepath.Join(t.TempDir(), "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	targets = controlTargets(t)
	installTestWrite(t, targets[0].policyPath, []byte("# codex\n"), 0o644)
	edited := "# codex\nsomeone else\n"
	writes := 0
	controlWrite = func(f agentInstallFile) error {
		writes++
		if f.path == targets[1].policyPath || writes > 3 {
			return errors.New("injected write failure")
		}
		if err := writeAgentInstallFile(f); err != nil {
			return err
		}
		if f.path == targets[0].policyPath {
			// Another process appends to the file right after our write.
			installTestWrite(t, f.path, append(append([]byte{}, f.data...), []byte("someone else\n")...), 0o644)
		}
		return nil
	}
	defer func() { controlWrite = writeAgentInstallFile }()
	code, out, errOut := runControl("enable", "--agent", "all", "--json")
	if code != 2 || out != "" || !strings.Contains(errOut, "injected write failure") || !strings.Contains(errOut, "rollback incomplete") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if got := string(installTestRead(t, targets[0].policyPath)); !strings.HasSuffix(got, "someone else\n") || !strings.Contains(got, "# codex") {
		t.Fatalf("concurrent edit lost: %q", got)
	}
	_ = edited
	_ = home
}
