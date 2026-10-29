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

func modeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

func runMode(args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := RunMode(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func modeInstalledPaths(t *testing.T) []string {
	t.Helper()
	targets, err := agentInstallTargets("all", "user", "")
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{config.Path()}
	for _, target := range targets {
		paths = append(paths, target.policyPath, filepath.Join(target.skillDir, "SKILL.md"), filepath.Join(target.skillDir, offloadSkillOwner))
	}
	return paths
}

func assertInstalledMode(t *testing.T, mode string) {
	t.Helper()
	targets, _ := agentInstallTargets("all", "user", "")
	for _, target := range targets {
		policy := installTestRead(t, target.policyPath)
		if !strings.Contains(string(policy), policyForMode(mode)) {
			t.Fatalf("wrong policy in %s", target.policyPath)
		}
		got := installTestRead(t, filepath.Join(target.skillDir, "SKILL.md"))
		if !bytes.Equal(got, skillForMode(skills.Offload, mode)) {
			t.Fatalf("wrong skill in %s", target.skillDir)
		}
		if !strings.Contains(string(got), "Delegation mode: "+mode) {
			t.Fatalf("missing active mode in %s", target.skillDir)
		}
		if err := checkSkillOwnership(target.skillDir); err != nil {
			t.Fatal(err)
		}
	}
}

func TestModeDefaultsWithoutCreatingFiles(t *testing.T) {
	home := modeHome(t)
	code, out, errOut := runMode()
	if code != 0 || out != "proactive\n" || errOut != "" {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Fatal("reading mode created files")
	}
}

func TestModeSwitchPreservesConfigAndUpdatesBothAgents(t *testing.T) {
	modeHome(t)
	original := `{"default":"model-profile","profiles":{"model-profile":{"backend":"codex","model":"example","token":"test-token"}},"future":{"keep":true}}`
	installTestWrite(t, config.Path(), []byte(original), 0600)
	for _, mode := range []string{"basic", "proactive", "basic"} {
		code, out, errOut := runMode(mode)
		if code != 0 || !strings.Contains(out, "new agent session") {
			t.Fatalf("%d %s %s", code, out, errOut)
		}
		assertInstalledMode(t, mode)
		_, out, _ = runMode()
		if out != mode+"\n" {
			t.Fatal(out)
		}
		var fields map[string]json.RawMessage
		json.Unmarshal(installTestRead(t, config.Path()), &fields)
		var originalFields map[string]json.RawMessage
		json.Unmarshal([]byte(original), &originalFields)
		for key, expected := range originalFields {
			var compactGot, compactWant bytes.Buffer
			json.Compact(&compactGot, fields[key])
			json.Compact(&compactWant, expected)
			if !bytes.Equal(compactGot.Bytes(), compactWant.Bytes()) {
				t.Fatalf("changed %s", key)
			}
		}
		paths := modeInstalledPaths(t)
		before := make(map[string]os.FileInfo)
		for _, path := range paths {
			st, _ := os.Stat(path)
			before[path] = st
		}
		if code, _, errOut = runMode(mode); code != 0 {
			t.Fatal(errOut)
		}
		for _, path := range paths {
			st, _ := os.Stat(path)
			if !st.ModTime().Equal(before[path].ModTime()) {
				t.Fatalf("repeat changed %s", path)
			}
		}
	}
}

func TestModeCanCreatePrivateConfigWithoutProfiles(t *testing.T) {
	modeHome(t)
	if code, _, err := runMode("basic"); code != 0 {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	json.Unmarshal(installTestRead(t, config.Path()), &fields)
	if len(fields) != 1 || string(fields["delegation_mode"]) != `"basic"` {
		t.Fatal(fields)
	}
	for _, path := range []string{config.Path(), config.Dir()} {
		st, _ := os.Stat(path)
		if st.Mode().Perm()&0077 != 0 {
			t.Fatalf("not private: %s", path)
		}
	}
}

func TestModeInvalidInputDoesNotModifyAnything(t *testing.T) {
	for _, args := range [][]string{{"unknown"}, {"basic", "extra"}, {""}} {
		t.Run(strings.Join(args, "/"), func(t *testing.T) {
			home := modeHome(t)
			if code, _, _ := runMode(args...); code != 2 {
				t.Fatal(code)
			}
			entries, _ := os.ReadDir(home)
			if len(entries) != 0 {
				t.Fatal("invalid mode wrote files")
			}
		})
	}
}

func TestModeMalformedConfigDoesNotChangeInstructions(t *testing.T) {
	for _, invalid := range []string{`{`, `null`, `[]`, `{"delegation_mode":"invalid"}`, `{"delegation_mode":2}`} {
		t.Run(invalid, func(t *testing.T) {
			modeHome(t)
			if code, _, err := runMode("proactive"); code != 0 {
				t.Fatal(err)
			}
			installTestWrite(t, config.Path(), []byte(invalid), 0600)
			snapshot := make(map[string][]byte)
			for _, path := range modeInstalledPaths(t) {
				snapshot[path] = installTestRead(t, path)
			}
			if code, _, _ := runMode("basic"); code != 2 {
				t.Fatal(code)
			}
			for path, expected := range snapshot {
				if !bytes.Equal(installTestRead(t, path), expected) {
					t.Fatalf("changed %s", path)
				}
			}
		})
	}
}

func TestModeOwnershipConflictDoesNotPersistOrPartiallyInstall(t *testing.T) {
	home := modeHome(t)
	if code, _, err := runMode("proactive"); code != 0 {
		t.Fatal(err)
	}
	conflict := filepath.Join(home, ".claude", "skills", "offload", "SKILL.md")
	installTestWrite(t, conflict, []byte("custom skill"), 0600)
	snapshot := make(map[string][]byte)
	for _, path := range modeInstalledPaths(t) {
		snapshot[path] = installTestRead(t, path)
	}
	if code, _, err := runMode("basic"); code != 2 || !strings.Contains(err, "modified") {
		t.Fatalf("%d %s", code, err)
	}
	for path, expected := range snapshot {
		if !bytes.Equal(installTestRead(t, path), expected) {
			t.Fatalf("changed %s", path)
		}
	}
}

func TestInstallerInheritsModeAndOverrideDoesNotSave(t *testing.T) {
	modeHome(t)
	if code, _, err := runMode("basic"); code != 0 {
		t.Fatal(err)
	}
	if code, _, err := installAgentCLI(); code != 0 {
		t.Fatal(err)
	}
	assertInstalledMode(t, "basic")
	if code, _, err := installAgentCLI("-mode", "proactive"); code != 0 {
		t.Fatal(err)
	}
	assertInstalledMode(t, "proactive")
	if mode, _ := config.LoadMode(config.Path()); mode != "basic" {
		t.Fatal(mode)
	}
	if code, _, err := installAgentCLI(); code != 0 {
		t.Fatal(err)
	}
	assertInstalledMode(t, "basic")
	if code, _, _ := installAgentCLI("-mode", "unknown"); code != 2 {
		t.Fatal(code)
	}
}

func TestModeReportsPersistenceFailureAfterUpdatingRules(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	modeHome(t)
	if code, _, err := runMode("proactive"); code != 0 {
		t.Fatal(err)
	}
	before := installTestRead(t, config.Path())
	if err := os.Chmod(config.Dir(), 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(config.Dir(), 0700) })
	code, _, errOut := runMode("basic")
	if code != 2 || !strings.Contains(errOut, "rules updated to basic, but mode was not saved") {
		t.Fatalf("%d %s", code, errOut)
	}
	if !bytes.Equal(before, installTestRead(t, config.Path())) {
		t.Fatal("saved mode changed despite failure")
	}
	assertInstalledMode(t, "basic")
}

func TestProjectModeOverridePreservesUserSettings(t *testing.T) {
	modeHome(t)
	if code, _, err := runMode("proactive"); code != 0 {
		t.Fatal(err)
	}
	snapshot := make(map[string][]byte)
	for _, path := range modeInstalledPaths(t) {
		snapshot[path] = installTestRead(t, path)
	}
	project := t.TempDir()
	if code, _, err := installAgentCLI("-scope", "project", "-d", project, "-mode", "basic"); code != 0 {
		t.Fatal(err)
	}
	targets, _ := agentInstallTargets("all", "project", project)
	for _, target := range targets {
		if !strings.Contains(string(installTestRead(t, target.policyPath)), policyForMode("basic")) {
			t.Fatal("project did not use basic mode")
		}
		if !bytes.Equal(installTestRead(t, filepath.Join(target.skillDir, "SKILL.md")), skillForMode(skills.Offload, "basic")) {
			t.Fatal("project skill mode differs")
		}
	}
	for path, expected := range snapshot {
		if !bytes.Equal(installTestRead(t, path), expected) {
			t.Fatalf("changed user file %s", path)
		}
	}
}

func TestSwitchPreservesCustomInstructionsAndSettings(t *testing.T) {
	home := modeHome(t)
	path := filepath.Join(home, ".codex", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("Custom instructions.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	original := `{"delegation_mode":"basic","default":"a","profiles":{"a":{"token":"secret"}},"future":{"x":1}}`
	if err := os.MkdirAll(config.Dir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path(), []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"disable", "enable"} {
		var out, errOut bytes.Buffer
		if code := RunSwitch([]string{command}, &out, &errOut); code != 0 {
			t.Fatal(errOut.String())
		}
		b := installTestRead(t, path)
		if !strings.HasPrefix(string(b), "Custom instructions.\n") {
			t.Fatal(string(b))
		}
		var before, after map[string]json.RawMessage
		json.Unmarshal([]byte(original), &before)
		json.Unmarshal(installTestRead(t, config.Path()), &after)
		for k, want := range before {
			var a, b bytes.Buffer
			json.Compact(&a, want)
			json.Compact(&b, after[k])
			if a.String() != b.String() {
				t.Fatalf("%s changed", k)
			}
		}
	}
	assertInstalledMode(t, "basic")
}

func TestDisableRemainsSavedWhenInstructionsConflict(t *testing.T) {
	home := modeHome(t)
	path := filepath.Join(home, ".agents", "skills", "offload", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("user-owned skill"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := RunSwitch([]string{"disable"}, &out, &errOut); code != 2 {
		t.Fatal(code)
	}
	if config.CheckEnabled() != config.ErrDisabled {
		t.Fatal("disable was undone")
	}
	if string(installTestRead(t, path)) != "user-owned skill" {
		t.Fatal("custom skill overwritten")
	}
	if !strings.Contains(errOut.String(), "Global switch saved") {
		t.Fatal(errOut.String())
	}
}
