package worker

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
)

func nativeOpts(model string) Opts {
	return Opts{ProfileName: "claude-sonnet", Model: model, Task: "do it",
		Profile: config.Profile{AuthMode: config.AuthModeNative, Model: model}}
}

func TestNativeClaudeArgsOmitBareAndPassModel(t *testing.T) {
	for _, resume := range []string{"", "sess"} {
		o := nativeOpts("sonnet")
		o.Resume, o.AddDirs = resume, []string{"/a"}
		args := buildArgs(o, "/p")
		if slices.Contains(args, "--bare") {
			t.Fatalf("--bare never reads the official login: %q", args)
		}
		want := []string{"-p", "--setting-sources", "", "--output-format", "stream-json", "--verbose", "--model", "sonnet"}
		if resume != "" {
			want = append(want, "--resume", resume)
		}
		want = append(want, "--add-dir", "/a", "--dangerously-skip-permissions", "--append-system-prompt-file", "/p", "do it")
		if !slices.Equal(args, want) {
			t.Fatalf("%q != %q", args, want)
		}
	}
}

func TestNativeClaudeKeepsPermissionAndToolBehavior(t *testing.T) {
	for name, tc := range map[string]struct {
		mut  func(*Opts)
		want []string
		not  []string
	}{
		"allowed tools":    {func(o *Opts) { o.AllowedTools = "Read,Grep" }, []string{"--permission-mode", "default", "--allowedTools", "Read Grep"}, []string{"--dangerously-skip-permissions"}},
		"disallowed tools": {func(o *Opts) { o.DisallowedTools = "Bash" }, []string{"--disallowedTools", "Bash"}, []string{"--dangerously-skip-permissions"}},
		"tools disabled":   {func(o *Opts) { o.ToolsDisabled = true }, []string{"--tools", ""}, []string{"--dangerously-skip-permissions"}},
		"managed":          {func(o *Opts) { o.ExecutionPolicy = "managed-development" }, []string{"--permission-mode", "acceptEdits"}, []string{"--dangerously-skip-permissions", "do it"}},
		"salvage":          {func(o *Opts) { o.Salvage = true }, []string{"--tools", "", "--dangerously-skip-permissions"}, nil},
		"default":          {func(*Opts) {}, []string{"--dangerously-skip-permissions"}, []string{"--permission-mode"}},
	} {
		t.Run(name, func(t *testing.T) {
			o := nativeOpts("sonnet")
			tc.mut(&o)
			args := buildArgs(o, "/p")
			gateway := o
			gateway.Profile = config.Profile{BaseURL: "https://gw", Model: "sonnet", Token: "t"}
			// Everything after the fixed prefix must match the gateway worker exactly.
			if !slices.Equal(args[8:], buildArgs(gateway, "/p")[7:]) {
				t.Fatalf("native permission/tool args differ from the gateway worker:\nnative  %q\ngateway %q", args, buildArgs(gateway, "/p"))
			}
			j := strings.Join(args, "\x00")
			for _, w := range tc.want {
				if !strings.Contains(j, strings.Join(strings.Split(w, " "), "\x00")) && !slices.Contains(args, w) {
					t.Errorf("missing %q in %q", w, args)
				}
			}
			for _, n := range tc.not {
				if slices.Contains(args, n) {
					t.Errorf("unexpected %q in %q", n, args)
				}
			}
			if slices.Contains(args, "--bare") {
				t.Errorf("--bare present: %q", args)
			}
		})
	}
}

func TestGatewayClaudeArgsKeepBareAndNoModelFlag(t *testing.T) {
	args := buildArgs(Opts{Model: "m", Profile: config.Profile{BaseURL: "https://gw", Model: "m", Token: "t"}, Task: "do it"}, "/p")
	if !slices.Contains(args, "--bare") || slices.Contains(args, "--model") {
		t.Fatalf("gateway worker changed: %q", args)
	}
	// An unknown auth_mode value or a Codex profile never selects the native path.
	for _, p := range []config.Profile{{AuthMode: "other", BaseURL: "https://gw"}, {Backend: "codex", AuthMode: "native"}} {
		if p.UsesNativeAuth() {
			t.Fatalf("%+v selected native auth", p)
		}
	}
}

func envValue(env []string, name string) (string, bool) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			return v, true
		}
	}
	return "", false
}

func TestNativeChildEnvStripsAmbientAnthropicAndSetsNone(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/h", "ANTHROPIC_API_KEY=ambient-key", "ANTHROPIC_AUTH_TOKEN=ambient-token",
		"ANTHROPIC_BASE_URL=https://other", "ANTHROPIC_MODEL=other", "ANTHROPIC_CUSTOM_HEADERS=X: 1", "ANTHROPIC_DEFAULT_SONNET_MODEL=x"}
	env := childEnv(base, nativeOpts("sonnet"), "must-not-be-used")
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_") {
			t.Errorf("native worker kept %q", kv)
		}
		if strings.Contains(kv, "must-not-be-used") || strings.Contains(kv, "ambient") {
			t.Errorf("credential reached the native worker environment: %q", kv)
		}
	}
	for _, keep := range []string{"PATH", "HOME"} {
		if _, ok := envValue(env, keep); !ok {
			t.Errorf("%s was dropped", keep)
		}
	}
	if v, _ := envValue(env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY"); v != "1" {
		t.Errorf("auto-memory must stay disabled without --bare: %q", env)
	}
	// The gateway path is unchanged.
	g := nativeOpts("m")
	g.Profile = config.Profile{BaseURL: "https://gw", Model: "m", Token: "t"}
	genv := childEnv(base, g, "tok")
	for k, want := range map[string]string{"ANTHROPIC_BASE_URL": "https://gw", "ANTHROPIC_AUTH_TOKEN": "tok", "ANTHROPIC_MODEL": "m"} {
		if v, _ := envValue(genv, k); v != want {
			t.Errorf("gateway %s=%q want %q", k, v, want)
		}
	}
	if _, ok := envValue(genv, "ANTHROPIC_API_KEY"); ok {
		t.Error("gateway worker kept ambient ANTHROPIC_API_KEY")
	}
}

// fakeNativeClaude records argv and the credential-bearing environment, then reports a result.
func fakeNativeClaude(t *testing.T) (bin, record string) {
	t.Helper()
	dir := t.TempDir()
	record = filepath.Join(dir, "record")
	bin = filepath.Join(dir, "claude")
	script := "#!/bin/sh\n{ printf 'ARG %s\\n' \"$@\"; env | grep -E '^(ANTHROPIC_|CLAUDE_CODE_DISABLE_AUTO_MEMORY)' | sort; echo END; } > '" + record + "'\n" +
		`printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"NATIVE REPORT","session_id":"s-native","num_turns":1}'` + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, record
}

func TestNativeRunSkipsTokenAndPreflightAndGatewayEnv(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	t.Setenv("OFFLOAD_MAX_WORKERS", "-1")
	t.Setenv("ANTHROPIC_API_KEY", "ambient-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "ambient-token")
	t.Setenv("ANTHROPIC_BASE_URL", "https://ambient.example")
	bin, record := fakeNativeClaude(t)
	t.Setenv("OFFLOAD_CLAUDE", bin)
	orig := Preflight
	Preflight = func(string) error { t.Fatal("native profile must not preflight a gateway"); return nil }
	t.Cleanup(func() { Preflight = orig })

	o := nativeOpts("sonnet")
	o.Dir, o.Timeout = t.TempDir(), 30*time.Second
	// A token command would fail the run if it were resolved.
	o.Profile.TokenCmd = ""
	rec, err := Run(context.Background(), o)
	if err != nil || rec.Status != "ok" || rec.Result != "NATIVE REPORT" || rec.Backend != "claude" {
		t.Fatalf("rec=%+v err=%v", rec, err)
	}
	b, _ := os.ReadFile(record)
	got := string(b)
	for _, want := range []string{"ARG --model\nARG sonnet\n", "ARG --setting-sources\nARG \n", "ARG --dangerously-skip-permissions\n", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "ARG --bare") || strings.Contains(got, "ANTHROPIC_") || strings.Contains(got, "ambient") {
		t.Errorf("native worker saw a gateway variable or --bare:\n%s", got)
	}
}

func TestNativeAuthRejectsIsolatedEnvironment(t *testing.T) {
	o := nativeOpts("sonnet")
	o.EnvironmentDirectory = t.TempDir()
	if _, err := o.prepareEnvironment(""); err == nil || !strings.Contains(err.Error(), "native-auth") {
		t.Fatalf("an isolated environment would hide the official login: %v", err)
	}
}
