package worker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// This opt-in probe exercises only app-server Skill configuration. It never
// starts or resumes a thread and therefore never authenticates or calls a model.
func TestInstalledCodexAppServerExactSkillsModelFree(t *testing.T) {
	if os.Getenv("OFFLOAD_APP_SERVER_NATIVE") != "1" {
		t.Skip("explicit installed-native protocol probe only")
	}
	bin, err := codexBin()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		selectRoot string
	}{
		{name: "external root excludes nested", selectRoot: "root"},
		{name: "nested child explicitly selected", selectRoot: "nested"},
		{name: "empty allowlist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			environment, err := NewEnvironment()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(environment) })

			externalParent := t.TempDir()
			externalRoot := filepath.Join(externalParent, "external-selected")
			nestedRoot := filepath.Join(externalRoot, "nested")
			ambientRoot := filepath.Join(environment, "home", ".agents", "skills", "ambient")
			for path, name := range map[string]string{externalRoot: "external", nestedRoot: "nested", ambientRoot: "ambient"} {
				if err := writeNativeProbeSkill(path, name); err != nil {
					t.Fatal(err)
				}
			}
			canonicalEnvironment, _ := filepath.EvalSymlinks(environment)
			canonicalExternal, _ := filepath.EvalSymlinks(externalRoot)
			if canonicalExternal == canonicalEnvironment || strings.HasPrefix(canonicalExternal, canonicalEnvironment+string(filepath.Separator)) {
				t.Fatal("external fixture unexpectedly resides inside the isolated environment")
			}

			var roots []string
			switch tc.selectRoot {
			case "root":
				roots = []string{externalRoot}
			case "nested":
				roots = []string{nestedRoot}
			}
			o := Opts{Dir: filepath.Join(environment, "workspace"), CodexHome: filepath.Join(environment, "codex"), Native: &NativeControls{SkillRoots: roots}}
			inventory, stderr, err := runInstalledSkillProbe(t, bin, o)
			if err != nil {
				t.Fatalf("%v; stderr=%s", err, stderr)
			}

			expected := map[string]bool{}
			canonicalAmbient, _ := filepath.EvalSymlinks(filepath.Join(ambientRoot, "SKILL.md"))
			expected[canonicalAmbient] = false
			switch tc.selectRoot {
			case "root":
				canonicalSelected, _ := filepath.EvalSymlinks(filepath.Join(externalRoot, "SKILL.md"))
				canonicalNested, _ := filepath.EvalSymlinks(filepath.Join(nestedRoot, "SKILL.md"))
				expected[canonicalSelected] = true
				expected[canonicalNested] = false
			case "nested":
				canonicalNested, _ := filepath.EvalSymlinks(filepath.Join(nestedRoot, "SKILL.md"))
				expected[canonicalNested] = true
			}
			got := make(map[string]bool, len(inventory))
			for _, skill := range inventory {
				got[skill.Path] = skill.Enabled
				if skill.Enabled && !expected[skill.Path] {
					t.Fatalf("unexpected Skill remained enabled: %s", skill.Path)
				}
			}
			for path, enabled := range expected {
				if actual, ok := got[path]; !ok || actual != enabled {
					t.Fatalf("Skill state mismatch path=%s got=%t present=%t want=%t inventory=%+v", path, actual, ok, enabled, inventory)
				}
			}
		})
	}
}

func writeNativeProbeSkill(root, name string) error {
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	content := fmt.Sprintf("---\nname: %s\ndescription: model-free exact Skill fixture\n---\n", name)
	return os.WriteFile(filepath.Join(root, "SKILL.md"), []byte(content), 0600)
}

func runInstalledSkillProbe(t *testing.T, bin string, o Opts) ([]discoveredSkill, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, buildCodexAppServerArgs(o)...)
	cmd.Dir = o.Dir
	root := filepath.Dir(o.CodexHome)
	cmd.Env = []string{
		"HOME=" + filepath.Join(root, "home"), "CODEX_HOME=" + o.CodexHome,
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"), "XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"XDG_DATA_HOME=" + filepath.Join(root, "data"), "XDG_STATE_HOME=" + filepath.Join(root, "data"),
		"PATH=" + os.Getenv("PATH"),
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "", err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	parser := newAppServerParser(stdin, o)
	if err := cmd.Start(); err != nil {
		return nil, stderr.String(), err
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	scanned := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64<<10), maxLineKept)
		for scanner.Scan() {
			parser.feed(scanner.Bytes())
		}
		parser.fail(io.EOF)
		scanned <- scanner.Err()
	}()

	finish := func(kill bool) error {
		_ = stdin.Close()
		if kill && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		var waitErr error
		select {
		case waitErr = <-waited:
		case <-time.After(2 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			select {
			case waitErr = <-waited:
			case <-time.After(2 * time.Second):
				return errors.New("installed app-server did not exit after kill")
			}
		}
		select {
		case scanErr := <-scanned:
			return errors.Join(waitErr, scanErr)
		case <-time.After(2 * time.Second):
			return errors.Join(waitErr, errors.New("installed app-server stdout did not close"))
		}
	}

	initialized := make(chan error, 1)
	go func() { initialized <- parser.initializeExactSkills() }()
	select {
	case err = <-initialized:
	case <-ctx.Done():
		parser.fail(ctx.Err())
		err = ctx.Err()
	}
	if err != nil {
		return nil, stderr.String(), errors.Join(err, finish(true))
	}

	type inventoryResult struct {
		raw json.RawMessage
		err error
	}
	listed := make(chan inventoryResult, 1)
	go func() {
		raw, requestErr := parser.request("skills/list", map[string]any{"cwds": []string{o.Dir}, "forceReload": true})
		listed <- inventoryResult{raw: raw, err: requestErr}
	}()
	var raw json.RawMessage
	select {
	case result := <-listed:
		raw, err = result.raw, result.err
	case <-ctx.Done():
		parser.fail(ctx.Err())
		err = ctx.Err()
	}
	if err != nil {
		return nil, stderr.String(), errors.Join(err, finish(true))
	}
	inventory, err := parseAppServerSkills(raw)
	finishErr := finish(false)
	if err != nil || finishErr != nil {
		return nil, stderr.String(), errors.Join(err, finishErr)
	}
	if err := os.Remove(filepath.Join(o.CodexHome, "config.toml")); err != nil && !os.IsNotExist(err) {
		return nil, stderr.String(), err
	}
	return inventory, stderr.String(), nil
}
