package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
)

func appServerFixture(t *testing.T, skillPath, tail string) (string, string) {
	t.Helper()
	skills := `[]`
	if skillPath != "" {
		skills = fmt.Sprintf(`[{"name":"ops","description":"fixture","enabled":true,"path":%q,"scope":"user","pluginId":null}]`, skillPath)
	}
	return appServerInventoryFixture(t, skills, skills, "", tail)
}

func appServerInventoryFixture(t *testing.T, initialSkills, finalSkills, writeResult, tail string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "requests")
	bin := filepath.Join(dir, "codex-fixture")
	body := fmt.Sprintf(`#!/bin/sh
log=%q
initial_skills=%q
final_skills=%q
write_result=%q
list_count=0
read_frame() { IFS= read -r frame || exit 0; printf '%%s\n' "$frame" >> "$log"; }
while true; do
  read_frame
  id=$(printf '%%s' "$frame" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
  case "$frame" in
    *'"method":"initialize"'*) printf '%%s\n' "{\"id\":$id,\"result\":{\"userAgent\":\"fixture\"}}" ;;
    *'"method":"initialized"'*) ;;
    *'"method":"skills/extraRoots/set"'*) printf '%%s\n' "{\"id\":$id,\"result\":{}}" ;;
    *'"method":"skills/list"'*)
      list_count=$((list_count + 1))
      skills=$initial_skills
      if [ "$list_count" -gt 1 ]; then skills=$final_skills; fi
      printf '{"id":%%s,"result":{"data":[{"cwd":"/workspace","errors":[],"skills":%%s}]}}\n' "$id" "$skills"
      ;;
    *'"method":"skills/config/write"'*)
      : > "$CODEX_HOME/config.toml"
      if [ "$write_result" = error ]; then
        printf '{"id":%%s,"error":{"code":-32601,"message":"unsupported"}}\n' "$id"
        continue
      fi
      if [ -n "$write_result" ]; then
        printf '{"id":%%s,"result":%%s}\n' "$id" "$write_result"
        continue
      fi
      case "$frame" in
        *'"enabled":true'*) effective=true ;;
        *) effective=false ;;
      esac
      printf '%%s\n' "{\"id\":$id,\"result\":{\"effectiveEnabled\":$effective}}"
      ;;
    *'"method":"thread/start"'*|*'"method":"thread/resume"'*) printf '%%s\n' "{\"id\":$id,\"result\":{\"model\":\"gpt-start\",\"thread\":{\"id\":\"thread-native\",\"model\":\"gpt-start\"}}}" ;;
    *'"method":"turn/start"'*)
      printf '%%s\n' "{\"id\":$id,\"result\":{\"turn\":{\"id\":\"turn-native\",\"status\":\"inProgress\",\"items\":[]}}}"
      break
      ;;
  esac
done
%s
`, log, initialSkills, finalSkills, writeResult, tail)
	if err := os.WriteFile(bin, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func TestCodexAppServerEmptySkillRootsSerializeAsArray(t *testing.T) {
	for _, tc := range []struct {
		name  string
		roots []string
	}{
		{name: "nil"},
		{name: "empty", roots: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tail := `printf '%s\n' '{"method":"turn/started","params":{"threadId":"thread-native","turn":{"id":"turn-native"}}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-native","turn":{"id":"turn-native","status":"completed","items":[{"id":"item","type":"agentMessage","text":"done"}],"error":null}}}'`
			bin, log := appServerFixture(t, "", tail)
			o := appServerOpts(t, t.TempDir())
			o.Native.SkillRoots = tc.roots
			o.Native.JSONSchema = ""
			a := runAttempt(context.Background(), bin, o, "", "", o.Timeout, nil, hooks{})
			if a.runErr != nil || a.s.Result == nil || a.s.Result.Result != "done" {
				t.Fatalf("%+v", a)
			}
			requests, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, line := range strings.Split(string(requests), "\n") {
				if strings.Contains(line, `"method":"skills/extraRoots/set"`) {
					found = true
					if !strings.Contains(line, `"extraRoots":[]`) || strings.Contains(line, `"extraRoots":null`) {
						t.Fatalf("invalid roots request: %s", line)
					}
				}
			}
			if !found {
				t.Fatal("skills/extraRoots/set request missing")
			}
		})
	}
}

func appServerOpts(t *testing.T, skillRoot string) Opts {
	t.Helper()
	root, err := NewEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return Opts{ProfileName: "fixture", Profile: config.Profile{Backend: "codex"}, Model: "gpt-requested", Dir: filepath.Join(root, "workspace"), EnvironmentDirectory: root, CodexHome: filepath.Join(root, "codex"), Task: "do work", Instructions: []byte("developer"), Timeout: 5 * time.Second, Native: &NativeControls{CallerContext: true, CodexAppServer: true, PermissionMode: "bypassPermissions", SkillRoots: []string{skillRoot}, JSONSchema: `{"type":"object"}`, MCPServers: []NativeMCPServer{{Name: "ops", Command: "/bin/echo", Args: []string{"ok"}}}}}
}

func skillDeclaration(path string, enabled bool, pluginID string) string {
	plugin := "null"
	if pluginID != "" {
		plugin = fmt.Sprintf(`%q`, pluginID)
	}
	return fmt.Sprintf(`{"name":"fixture","description":"fixture","enabled":%t,"path":%q,"scope":"user","pluginId":%s}`, enabled, path, plugin)
}

const completedAppServerTail = `printf '%s\n' '{"method":"turn/started","params":{"threadId":"thread-native","turn":{"id":"turn-native"}}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-native","turn":{"id":"turn-native","status":"completed","items":[{"id":"item","type":"agentMessage","text":"done"}],"error":null}}}'`

func TestCodexAppServerEnforcesExactSkillSet(t *testing.T) {
	parent := t.TempDir()
	parentSkill := filepath.Join(parent, "SKILL.md")
	child := filepath.Join(parent, "child")
	childSkill := filepath.Join(child, "SKILL.md")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{parentSkill, childSkill} {
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name     string
		selected string
		final    string
		disabled string
	}{
		{name: "parent does not imply child", selected: parent, final: "[" + skillDeclaration(parentSkill, true, "") + "," + skillDeclaration(childSkill, false, "") + "]", disabled: childSkill},
		{name: "child explicitly selected", selected: child, final: "[" + skillDeclaration(parentSkill, false, "") + "," + skillDeclaration(childSkill, true, "") + "]", disabled: parentSkill},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initial := "[" + skillDeclaration(parentSkill, true, "") + "," + skillDeclaration(childSkill, true, "") + "]"
			bin, log := appServerInventoryFixture(t, initial, tc.final, "", completedAppServerTail)
			o := appServerOpts(t, tc.selected)
			o.Native.JSONSchema = ""
			a := runAttempt(context.Background(), bin, o, "", "", o.Timeout, nil, hooks{})
			if a.runErr != nil || a.s.Result == nil {
				t.Fatalf("%+v", a)
			}
			requests, _ := os.ReadFile(log)
			disabled, _ := filepath.EvalSymlinks(tc.disabled)
			if !strings.Contains(string(requests), fmt.Sprintf(`"enabled":false,"path":%q`, disabled)) {
				t.Fatalf("unapproved Skill was not disabled: %s", requests)
			}
		})
	}
}

func TestCodexAppServerCarriesValidatedSkillStatesIntoThreadConfig(t *testing.T) {
	selected := t.TempDir()
	selectedPath := filepath.Join(selected, "SKILL.md")
	nested := filepath.Join(selected, "nested")
	nestedPath := filepath.Join(nested, "SKILL.md")
	ambient := t.TempDir()
	ambientPath := filepath.Join(ambient, "SKILL.md")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{selectedPath, nestedPath, ambientPath} {
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	initial := "[" + skillDeclaration(selectedPath, true, "") + "," + skillDeclaration(nestedPath, true, "") + "," + skillDeclaration(ambientPath, true, "") + "]"
	final := "[" + skillDeclaration(selectedPath, true, "") + "," + skillDeclaration(nestedPath, false, "") + "," + skillDeclaration(ambientPath, false, "") + "]"
	canonicalSelected, _ := filepath.EvalSymlinks(selectedPath)
	canonicalNested, _ := filepath.EvalSymlinks(nestedPath)
	canonicalAmbient, _ := filepath.EvalSymlinks(ambientPath)
	want := map[string]bool{canonicalSelected: true, canonicalNested: false, canonicalAmbient: false}
	for _, resume := range []bool{false, true} {
		name := "start"
		method := "thread/start"
		if resume {
			name, method = "resume", "thread/resume"
		}
		t.Run(name, func(t *testing.T) {
			bin, log := appServerInventoryFixture(t, initial, final, "", completedAppServerTail)
			o := appServerOpts(t, selected)
			o.Native.JSONSchema = ""
			if resume {
				o.Resume = "original-thread-id"
			}
			a := runAttempt(context.Background(), bin, o, "", "", o.Timeout, nil, hooks{})
			if a.runErr != nil || a.s.Result == nil {
				t.Fatalf("%+v", a)
			}
			config := capturedThreadSkillConfig(t, log, method)
			if config == nil || len(*config) != len(want) {
				t.Fatalf("thread Skill config is not the validated inventory: %+v", config)
			}
			for _, state := range *config {
				if enabled, ok := want[state.Path]; !ok || enabled != state.Enabled {
					t.Fatalf("unexpected thread Skill state: %+v want=%+v", state, want)
				}
			}
		})
	}
}

func TestCodexAppServerCarriesNonNilEmptySkillConfig(t *testing.T) {
	for _, resume := range []bool{false, true} {
		name := "start"
		method := "thread/start"
		if resume {
			name, method = "resume", "thread/resume"
		}
		t.Run(name, func(t *testing.T) {
			bin, log := appServerInventoryFixture(t, `[]`, `[]`, "", completedAppServerTail)
			o := appServerOpts(t, t.TempDir())
			o.Native.SkillRoots = nil
			o.Native.JSONSchema = ""
			if resume {
				o.Resume = "original-thread-id"
			}
			a := runAttempt(context.Background(), bin, o, "", "", o.Timeout, nil, hooks{})
			if a.runErr != nil || a.s.Result == nil {
				t.Fatalf("%+v", a)
			}
			config := capturedThreadSkillConfig(t, log, method)
			if config == nil || len(*config) != 0 {
				t.Fatalf("empty validated Skill config was not serialized as []: %+v", config)
			}
		})
	}
}

func capturedThreadSkillConfig(t *testing.T, log, method string) *[]appServerSkillConfig {
	t.Helper()
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		var request struct {
			Method string `json:"method"`
			Params struct {
				Config struct {
					Skills struct {
						Bundled struct {
							Enabled *bool `json:"enabled"`
						} `json:"bundled"`
						Config *[]appServerSkillConfig `json:"config"`
					} `json:"skills"`
					Cloud struct {
						Skills struct {
							Enabled *bool `json:"enabled"`
						} `json:"skills"`
					} `json:"cloud"`
				} `json:"config"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(line), &request) == nil && request.Method == method {
			bundled := request.Params.Config.Skills.Bundled.Enabled
			cloud := request.Params.Config.Cloud.Skills.Enabled
			if bundled == nil || *bundled || cloud == nil || *cloud {
				t.Fatalf("%s did not explicitly disable bundled/cloud Skills: %s", method, line)
			}
			return request.Params.Config.Skills.Config
		}
	}
	t.Fatalf("%s request missing from %s", method, raw)
	return nil
}

func TestCodexAppServerDisablesBuiltinAndPluginSkills(t *testing.T) {
	approved := t.TempDir()
	approvedPath := filepath.Join(approved, "SKILL.md")
	builtin := filepath.Join(t.TempDir(), "SKILL.md")
	plugin := filepath.Join(t.TempDir(), "SKILL.md")
	for _, path := range []string{approvedPath, builtin, plugin} {
		if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	initial := "[" + skillDeclaration(approvedPath, false, "") + "," + skillDeclaration(builtin, true, "") + "," + skillDeclaration(plugin, false, "plugin.fixture") + "]"
	final := "[" + skillDeclaration(approvedPath, true, "") + "," + skillDeclaration(builtin, false, "") + "," + skillDeclaration(plugin, false, "plugin.fixture") + "]"
	bin, log := appServerInventoryFixture(t, initial, final, "", completedAppServerTail)
	o := appServerOpts(t, approved)
	o.Native.JSONSchema = ""
	a := runAttempt(context.Background(), bin, o, "", "", o.Timeout, nil, hooks{})
	if a.runErr != nil || a.s.Result == nil {
		t.Fatalf("%+v", a)
	}
	requests, _ := os.ReadFile(log)
	for _, path := range []string{builtin, plugin} {
		canonical, _ := filepath.EvalSymlinks(path)
		if !strings.Contains(string(requests), fmt.Sprintf(`"enabled":false,"path":%q`, canonical)) {
			t.Fatalf("unapproved declaration was not explicitly disabled: %s", requests)
		}
	}
}

func TestCodexAppServerSkillConfigurationFailuresStopBeforeThread(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	declaration := "[" + skillDeclaration(path, false, "") + "]"
	for _, tc := range []struct {
		name, initial, final, writeResult, want string
	}{
		{name: "write unsupported", initial: declaration, final: declaration, writeResult: "error", want: "skills/config/write failed"},
		{name: "invalid write response", initial: declaration, final: declaration, writeResult: `{}`, want: "invalid Skill configuration result"},
		{name: "effective state mismatch", initial: declaration, final: declaration, writeResult: `{"effectiveEnabled":false}`, want: "invalid Skill configuration result"},
		{name: "missing approved", initial: `[]`, final: `[]`, want: "approved Skill was not enabled"},
		{name: "duplicate canonical", initial: "[" + skillDeclaration(path, false, "") + "," + skillDeclaration(path, false, "") + "]", final: declaration, want: "duplicate canonical Skill path"},
		{name: "malformed discovered path", initial: `[{"name":"bad","enabled":true,"path":"relative/SKILL.md","pluginId":null}]`, final: declaration, want: "malformed Skill declaration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, log := appServerInventoryFixture(t, tc.initial, tc.final, tc.writeResult, `exit 0`)
			o := appServerOpts(t, root)
			a := runAttempt(context.Background(), bin, o, "", "", o.Timeout, nil, hooks{})
			if a.runErr == nil || !strings.Contains(a.runErr.Error(), tc.want) {
				t.Fatalf("%+v", a)
			}
			requests, _ := os.ReadFile(log)
			if strings.Contains(string(requests), `"method":"thread/start"`) || strings.Contains(string(requests), `"method":"thread/resume"`) {
				t.Fatalf("thread started after Skill policy failure: %s", requests)
			}
			if tc.name == "malformed discovered path" && strings.Contains(string(requests), `"method":"skills/config/write"`) {
				t.Fatalf("malformed discovered path reached configuration write: %s", requests)
			}
		})
	}
}

func TestAppServerSkillInventoriesAreStructurallyBounded(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want string
	}{
		{name: "missing data", raw: `{}`, want: "invalid Skills list"},
		{name: "empty data", raw: `{"data":[]}`, want: "invalid Skills list"},
		{name: "missing errors", raw: `{"data":[{"skills":[]}]}`, want: "malformed Skills inventory"},
		{name: "missing skills", raw: `{"data":[{"errors":[]}]}`, want: "malformed Skills inventory"},
		{name: "discovery error", raw: `{"data":[{"errors":[{"message":"bad"}],"skills":[]}]}`, want: "discovery error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseAppServerSkills([]byte(tc.raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("malformed inventory accepted: %v", err)
			}
		})
	}
}

func TestExactSkillManifestRejectsMissingAndDuplicateCanonicalFiles(t *testing.T) {
	missing := t.TempDir()
	if _, err := exactSkillManifest([]string{missing}); err == nil || !strings.Contains(err.Error(), "own regular SKILL.md") {
		t.Fatalf("missing exact Skill file accepted: %v", err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(root, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := exactSkillManifest([]string{root, alias}); err == nil || !strings.Contains(err.Error(), "canonically unique") {
		t.Fatalf("duplicate canonical manifest accepted: %v", err)
	}
}

func TestCodexAppServerConfigCleanupAndResidueGuard(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	declaration := "[" + skillDeclaration(path, true, "") + "]"
	bin, _ := appServerInventoryFixture(t, declaration, declaration, "", completedAppServerTail)
	o := appServerOpts(t, root)
	o.Native.JSONSchema = ""
	a := runAttempt(context.Background(), bin, o, "", "", o.Timeout, nil, hooks{})
	configPath := filepath.Join(o.CodexHome, "config.toml")
	if a.runErr != nil || a.s.Result == nil {
		t.Fatalf("%+v", a)
	}
	if _, err := os.Lstat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("attempt-owned config was not removed after reap: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("foreign = true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a = runAttempt(context.Background(), bin, o, "", "", o.Timeout, nil, hooks{})
	if a.runErr == nil || !strings.Contains(a.runErr.Error(), "requires an absent base config.toml") {
		t.Fatalf("ambient config residue was accepted: %+v", a)
	}
}

func TestCodexAppServerProtocolAndStructuredResult(t *testing.T) {
	skillRoot := t.TempDir()
	skill := filepath.Join(skillRoot, "ops", "SKILL.md")
	if err := os.Mkdir(filepath.Dir(skill), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	tail := `printf '%s\n' '{"method":"turn/started","params":{"threadId":"thread-native","turn":{"id":"turn-native"}}}'
printf '%s\n' '{"method":"item/agentMessage/delta","params":{"threadId":"thread-native","turnId":"turn-native","itemId":"item","delta":"{\"ok\":true}"}}'
printf '%s\n' '{"method":"model/rerouted","params":{"threadId":"thread-native","turnId":"turn-native","fromModel":"gpt-start","toModel":"gpt-rerouted","reason":"highRiskCyberActivity"}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-native","turn":{"id":"turn-native","status":"completed","items":[{"id":"item","type":"agentMessage","text":"{\"ok\":true}"}],"error":null}}}'`
	bin, log := appServerFixture(t, skill, tail)
	o := appServerOpts(t, filepath.Dir(skill))
	var events []string
	o.OnLine = func(b []byte) error { events = append(events, string(b)); return nil }
	a := runAttempt(context.Background(), bin, o, "", "", o.Timeout, nil, hooks{})
	if a.runErr != nil || a.cause != nil || a.leftover != nil || a.s.Result == nil {
		t.Fatalf("%+v", a)
	}
	if a.s.Session != "thread-native" || a.s.ObservedModel != "gpt-rerouted" || string(a.s.Result.StructuredOutput) != `{"ok":true}` {
		t.Fatalf("%+v", a.s)
	}
	requests, _ := os.ReadFile(log)
	for _, want := range []string{`"method":"initialize"`, `"experimentalApi":true`, `"method":"skills/extraRoots/set"`, fmt.Sprintf(`"extraRoots":[%q]`, filepath.Dir(skill)), `"forceReload":true`, `"method":"thread/start"`, `"developerInstructions":"developer"`, `"mcp_servers"`, `"method":"turn/start"`, `"outputSchema"`} {
		if !strings.Contains(string(requests), want) {
			t.Fatalf("missing %s in %s", want, requests)
		}
	}
	if len(events) == 0 || !strings.Contains(events[len(events)-1], `"method":"turn/completed"`) {
		t.Fatal("raw RPC notifications were not preserved")
	}
}

func TestCodexAppServerRejectsMalformedResponses(t *testing.T) {
	for _, tc := range []struct{ name, frame, want string }{
		{"wrong version", `{"jsonrpc":"1.0","id":1,"result":{}}`, "malformed JSON-RPC"},
		{"missing result and error", `{"id":1}`, "exactly one result or error"},
		{"both result and error", `{"id":1,"result":{},"error":{"code":-1,"message":"bad"}}`, "exactly one result or error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, writer := io.Pipe()
			t.Cleanup(func() { _ = reader.Close() })
			p := newAppServerParser(writer, Opts{})
			p.feed([]byte(tc.frame))
			p.mu.Lock()
			err := p.err
			p.mu.Unlock()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("invalid response accepted: %v", err)
			}
		})
	}
}

func TestCodexAppServerItemCompletedSuppliesCanonicalReport(t *testing.T) {
	root := t.TempDir()
	skill := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(skill, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	tail := `printf '%s\n' '{"method":"turn/started","params":{"threadId":"thread-native","turn":{"id":"turn-native"}}}'
printf '%s\n' '{"method":"item/completed","params":{"threadId":"thread-native","turnId":"turn-native","completedAtMs":1,"item":{"id":"item","type":"agentMessage","text":"item final"}}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-native","turn":{"id":"turn-native","status":"completed","items":[],"error":null}}}'`
	bin, _ := appServerFixture(t, skill, tail)
	o := appServerOpts(t, root)
	o.Native.JSONSchema = ""
	a := runAttempt(context.Background(), bin, o, "", "", o.Timeout, nil, hooks{})
	if a.runErr != nil || a.s.Result == nil || a.s.Result.Result != "item final" {
		t.Fatalf("%+v", a)
	}
}

func TestCodexAppServerTurnFailureKeepsBoundedProviderReason(t *testing.T) {
	root := t.TempDir()
	skill := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(skill, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	reason := strings.Repeat("provider unavailable ", 40)
	tail := fmt.Sprintf(`printf '%%s\n' '{"method":"turn/started","params":{"threadId":"thread-native","turn":{"id":"turn-native"}}}'
printf '%%s\n' '{"method":"turn/completed","params":{"threadId":"thread-native","turn":{"id":"turn-native","status":"failed","items":[],"error":{"message":%q,"additionalDetails":"retry elsewhere"}}}}'`, reason)
	bin, _ := appServerFixture(t, skill, tail)
	o := appServerOpts(t, root)
	o.Native.JSONSchema = ""
	a := runAttempt(context.Background(), bin, o, "", "", o.Timeout, nil, hooks{})
	if a.s.Result == nil || !a.s.Result.IsError || len(a.s.Result.Errors) < 2 || !strings.Contains(a.s.Result.Errors[len(a.s.Result.Errors)-1], "provider unavailable") || len(a.s.Result.Errors[len(a.s.Result.Errors)-1]) > 540 {
		t.Fatalf("provider diagnostic missing or unbounded: %+v", a.s.Result)
	}
}

func TestCodexAppServerWorkerRecordUsesNativeEvidence(t *testing.T) {
	skillRoot := t.TempDir()
	skill := filepath.Join(skillRoot, "SKILL.md")
	if err := os.WriteFile(skill, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	tail := `printf '%s\n' '{"method":"turn/started","params":{"threadId":"thread-native","turn":{"id":"turn-native"}}}'
printf '%s\n' '{"method":"model/rerouted","params":{"threadId":"thread-native","turnId":"turn-native","fromModel":"gpt-start","toModel":"gpt-native","reason":"highRiskCyberActivity"}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-native","turn":{"id":"turn-native","status":"completed","items":[{"id":"item","type":"agentMessage","text":"done"}],"error":null}}}'`
	bin, _ := appServerFixture(t, skill, tail)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "auth.json"), []byte(`{"token":"fixture"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", source)
	t.Setenv("OFFLOAD_CODEX", bin)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	o := appServerOpts(t, skillRoot)
	o.CodexHome = ""
	o.Native.JSONSchema = ""
	o.MaxWorkers = 1
	rec, err := Run(context.Background(), o)
	if err != nil || rec.Status != "ok" || rec.NativeProtocol != codexAppServerProtocol || rec.Session != "thread-native" || rec.ObservedModel != "gpt-native" || rec.ModelSource != "native_app_server" {
		t.Fatalf("%+v err=%v", rec, err)
	}
	if rec.Usage != nil || rec.CostUSD != nil || !strings.Contains(rec.UsageUnavailable, "no trusted") {
		t.Fatalf("native accounting was invented: %+v", rec)
	}
}

func TestCodexAppServerResumeAndSkillBoundary(t *testing.T) {
	approved := t.TempDir()
	inside := filepath.Join(approved, "SKILL.md")
	if err := os.WriteFile(inside, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	completed := `printf '%s\n' '{"method":"turn/started","params":{"threadId":"thread-native","turn":{"id":"turn-native"}}}'
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-native","turn":{"id":"turn-native","status":"completed","items":[{"id":"item","type":"agentMessage","text":"{\"ok\":true}"}],"error":null}}}'`
	bin, log := appServerFixture(t, inside, completed)
	o := appServerOpts(t, approved)
	o.Resume = "original-thread-id"
	a := runAttempt(context.Background(), bin, o, "", "", o.Timeout, nil, hooks{})
	if a.runErr != nil || a.s.Result == nil {
		t.Fatalf("%+v", a)
	}
	requests, _ := os.ReadFile(log)
	if !strings.Contains(string(requests), `"method":"thread/resume"`) || !strings.Contains(string(requests), `"threadId":"original-thread-id"`) {
		t.Fatalf("original resume identity missing: %s", requests)
	}

	outside := filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.WriteFile(outside, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	bin, log = appServerFixture(t, outside, `exit 0`)
	o = appServerOpts(t, approved)
	o.Resume = "original-thread-id"
	a = runAttempt(context.Background(), bin, o, "", "", o.Timeout, nil, hooks{})
	if a.runErr == nil || !strings.Contains(a.runErr.Error(), "unexpected Skill remained enabled") {
		t.Fatalf("%+v", a)
	}
	requests, _ = os.ReadFile(log)
	if strings.Contains(string(requests), `"method":"thread/resume"`) || strings.Contains(string(requests), `"method":"turn/start"`) {
		t.Fatal("business thread started before Skill validation")
	}
}

func TestCodexAppServerUnexpectedEOFIsFailure(t *testing.T) {
	root := t.TempDir()
	skill := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(skill, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	bin, _ := appServerFixture(t, skill, `exit 0`)
	o := appServerOpts(t, root)
	a := runAttempt(context.Background(), bin, o, "", "", o.Timeout, nil, hooks{})
	if a.runErr == nil || !strings.Contains(a.runErr.Error(), "unexpected EOF") || a.s.Result != nil {
		t.Fatalf("%+v", a)
	}
}

func TestCodexAppServerRejectsInteractiveServerRequest(t *testing.T) {
	root := t.TempDir()
	skill := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(skill, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	tail := `printf '%s\n' '{"method":"tool/requestUserInput","id":99,"params":{"questions":[]}}'
read_frame`
	bin, log := appServerFixture(t, skill, tail)
	o := appServerOpts(t, root)
	a := runAttempt(context.Background(), bin, o, "", "", o.Timeout, nil, hooks{})
	if a.runErr == nil || !strings.Contains(a.runErr.Error(), "unsupported interaction") {
		t.Fatalf("%+v", a)
	}
	requests, _ := os.ReadFile(log)
	if !strings.Contains(string(requests), `"code":-32601`) || strings.Contains(string(requests), `"result"`) {
		t.Fatalf("interactive request was approved: %s", requests)
	}
}

func TestCodexAppServerCancelInterruptsAndReaps(t *testing.T) {
	root := t.TempDir()
	skill := filepath.Join(root, "SKILL.md")
	if err := os.WriteFile(skill, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	tail := `printf '%s\n' '{"method":"turn/started","params":{"threadId":"thread-native","turn":{"id":"turn-native"}}}'
read_frame
id=$(printf '%s' "$frame" | sed -n 's/.*"id":\([0-9][0-9]*\).*/\1/p')
printf '%s\n' "{\"id\":$id,\"result\":{}}"
printf '%s\n' '{"method":"turn/completed","params":{"threadId":"thread-native","turn":{"id":"turn-native","status":"interrupted","items":[],"error":{"message":"interrupted"}}}}'`
	bin, log := appServerFixture(t, skill, tail)
	o := appServerOpts(t, root)
	ctx, cancel := context.WithCancel(context.Background())
	o.OnLine = func(b []byte) error {
		if strings.Contains(string(b), `"method":"turn/started"`) {
			cancel()
		}
		return nil
	}
	a := runAttempt(ctx, bin, o, "", "", o.Timeout, nil, hooks{})
	if a.cause == nil || a.leftover != nil {
		t.Fatalf("%+v", a)
	}
	requests, _ := os.ReadFile(log)
	if !strings.Contains(string(requests), `"method":"turn/interrupt"`) {
		t.Fatalf("interrupt missing: %s", requests)
	}
}
