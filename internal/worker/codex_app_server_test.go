package worker

import (
	"context"
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
	dir := t.TempDir()
	log := filepath.Join(dir, "requests")
	bin := filepath.Join(dir, "codex-fixture")
	skills := `[]`
	if skillPath != "" {
		skills = fmt.Sprintf(`[{"name":"ops","description":"fixture","enabled":true,"path":%q,"scope":"user","pluginId":null}]`, skillPath)
	}
	body := fmt.Sprintf(`#!/bin/sh
log=%q
read_frame() { IFS= read -r frame || exit 0; printf '%%s\n' "$frame" >> "$log"; }
read_frame
printf '%%s\n' '{"id":1,"result":{"userAgent":"fixture"}}'
read_frame
read_frame
printf '%%s\n' '{"id":2,"result":{}}'
read_frame
printf '%%s\n' '{"id":3,"result":{"data":[{"cwd":"/workspace","errors":[],"skills":%s}]}}'
read_frame
printf '%%s\n' '{"id":4,"result":{"model":"gpt-start","thread":{"id":"thread-native","model":"gpt-start"}}}'
read_frame
printf '%%s\n' '{"id":5,"result":{"turn":{"id":"turn-native","status":"inProgress","items":[]}}}'
%s
`, log, skills, tail)
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
	o := appServerOpts(t, skillRoot)
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
	for _, want := range []string{`"method":"initialize"`, `"experimentalApi":true`, `"method":"skills/extraRoots/set"`, `"forceReload":true`, `"method":"thread/start"`, `"developerInstructions":"developer"`, `"mcp_servers"`, `"method":"turn/start"`, `"outputSchema"`} {
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
	if a.runErr == nil || !strings.Contains(a.runErr.Error(), "outside approved roots") {
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
printf '%s\n' '{"id":6,"result":{}}'
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
