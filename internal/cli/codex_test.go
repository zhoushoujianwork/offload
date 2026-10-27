package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/worker"
)

const codexReply = `{"type":"thread.started","thread_id":"codex-session"}
{"type":"item.started","item":{"id":"cmd1","type":"command_execution","command":"rg retry"}}
{"type":"item.completed","item":{"id":"cmd1","type":"command_execution","command":"rg retry"}}
{"type":"item.completed","item":{"id":"msg1","type":"agent_message","text":"CODEX REPORT"}}
{"type":"turn.completed","usage":{"input_tokens":100,"output_tokens":10}}`

func setupCodex(t *testing.T, reply string) (string, string) {
	t.Helper()
	work, record := setup(t, okReply)
	cfg, err := config.Load(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Profiles["codex"] = config.Profile{Backend: "codex", Model: "test-model"}
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(config.Path(), b, 0600); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(t.TempDir(), "codex")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + record + "'\ncat > '" + record + ".stdin'\n" +
		"[ -n \"$FAKE_SLEEP\" ] && sleep \"$FAKE_SLEEP\"\ncat <<'EOF'\n" + reply + "\nEOF\nexit \"${FAKE_EXIT:-0}\"\n"
	if err := os.WriteFile(fake, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_CODEX", fake)
	worker.Preflight = func(string) error {
		t.Fatal("Codex must use native configuration without gateway preflight")
		return nil
	}
	return work, record
}

func TestCodexRunAndResumeAfterDefaultChange(t *testing.T) {
	work, record := setupCodex(t, codexReply)
	if code, _, err := runCLI("default", "codex"); code != 0 {
		t.Fatal(err)
	}
	if code, out, err := runCLI("-json", "-d", work, "inspect"); code != 0 {
		t.Fatalf("%d %s %s", code, out, err)
	}
	rs, err := history.Read(history.Path())
	if err != nil || len(rs) != 1 {
		t.Fatalf("%+v %v", rs, err)
	}
	r := rs[0]
	if r.Backend != "codex" || r.Result != "CODEX REPORT" || r.Session != "codex-session" || r.Turns != 1 || r.CostUSD != nil || r.ElapsedMs == nil {
		t.Fatalf("%+v", r)
	}
	args, _ := os.ReadFile(record)
	for _, want := range []string{"exec\n", `sandbox_mode="read-only"`, `approval_policy="never"`, "--json", "test-model"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("missing %q: %s", want, args)
		}
	}
	prompt, _ := os.ReadFile(record + ".stdin")
	if !strings.Contains(string(prompt), "Read-only task") || !strings.Contains(string(prompt), "Task:\ninspect") || strings.Contains(string(args), "inspect") {
		t.Fatalf("argv=%s stdin=%s", args, prompt)
	}
	runCLI("default", "cheap")
	t.Setenv("OFFLOAD_PROFILE", "cheap")
	if code, _, err := runCLI("-w", "-r", "codex-session", "follow up"); code != 0 {
		t.Fatal(err)
	}
	args, _ = os.ReadFile(record)
	if !strings.Contains(string(args), "resume\n") || !strings.Contains(string(args), "codex-session\n-\n") || !strings.Contains(string(args), `sandbox_mode="workspace-write"`) {
		t.Fatalf("%s", args)
	}
}

func TestDefaultValidationAndPreservation(t *testing.T) {
	setupCodex(t, codexReply)
	b, _ := os.ReadFile(config.Path())
	var fields map[string]json.RawMessage
	json.Unmarshal(b, &fields)
	fields["future"] = json.RawMessage(`{"keep":true}`)
	b, _ = json.Marshal(fields)
	os.WriteFile(config.Path(), b, 0600)
	if code, _, _ := runCLI("default", "missing"); code != 2 {
		t.Fatal(code)
	}
	after, _ := os.ReadFile(config.Path())
	if string(after) != string(b) {
		t.Fatal("invalid selection modified config")
	}
	if code, _, err := runCLI("default", "codex"); code != 0 {
		t.Fatal(err)
	}
	t.Setenv("OFFLOAD_PROFILE", "cheap")
	if _, out, _ := runCLI("default"); strings.TrimSpace(out) != "codex" {
		t.Fatal(out)
	}
	after, _ = os.ReadFile(config.Path())
	if !strings.Contains(string(after), `"keep": true`) {
		t.Fatal(string(after))
	}
	if code, _, _ := runCLI("default", "codex", "cheap"); code != 2 {
		t.Fatal(code)
	}
}

func TestCodexFailureAndTimeout(t *testing.T) {
	for _, tc := range []struct {
		name, reply, sleep, exit string
		code                     int
	}{
		{"turn failure", `{"type":"turn.failed","error":{"message":"authentication failed"}}`, "", "", 1},
		{"process failure", codexReply, "", "7", 1},
		{"invalid output", "not JSON", "", "", 1},
		{"interrupted", `{"type":"thread.started","thread_id":"codex-session"}`, "", "", 1},
		{"timeout", codexReply, "5", "", 124},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work, _ := setupCodex(t, tc.reply)
			t.Setenv("FAKE_SLEEP", tc.sleep)
			t.Setenv("FAKE_EXIT", tc.exit)
			timeout := "5s"
			if tc.name == "timeout" {
				timeout = "200ms"
			}
			code, out, err := runCLI("-json", "-p", "codex", "-d", work, "-t", timeout, "inspect")
			if code != tc.code {
				t.Fatalf("%d %s %s", code, out, err)
			}
			r := decodeRun(t, out)
			if r.CostUSD != nil || r.ElapsedMs == nil || r.Salvaged {
				t.Fatalf("%+v", r)
			}
			if tc.name == "turn failure" && !strings.Contains(r.Error, "authentication failed") {
				t.Fatal(r)
			}
		})
	}
}

func TestCodexBundleOverridesReachWorker(t *testing.T) {
	work, record := setupCodex(t, codexReply)
	cfg, err := config.Load(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Profiles["codex"]
	p.ReasoningEffort = "high"
	p.ServiceTier = "default"
	if err := config.SaveProfile(config.Path(), "codex", p, false); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := runCLI("-p", "codex", "-m", "override", "-d", work, "inspect"); code != 0 {
		t.Fatal(errOut)
	}
	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"-c\nmodel_reasoning_effort=\"high\"\n", "-c\nservice_tier=\"default\"\n", "-m\noverride\n"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("missing %q in %s", want, b)
		}
	}
}

func TestCodexResolvedModelReporting(t *testing.T) {
	for name, content := range map[string]string{
		"plain":                   "model = 'native-model'\n",
		"multiline model":         "model = \"\"\"native-model\"\"\"\n",
		"multiline literal model": "model = '''\nnative-model'''\n",
		"inline comment":          "model = 'native-model' # documentation: \"\"\"\n",
		"multiline instructions":  "model = 'native-model'\ndeveloper_instructions = \"\"\"\nBe concise.\n\"\"\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			work, _ := setupCodex(t, codexReply)
			t.Setenv("CODEX_HOME", t.TempDir())
			if err := os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "config.toml"), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if err := config.SaveProfile(config.Path(), "codex", config.Profile{Backend: "codex"}, false); err != nil {
				t.Fatal(err)
			}
			code, out, stderr := runCLI("-json", "-p", "codex", "-d", work, "inspect")
			if code != 0 {
				t.Fatalf("%d %s %s", code, out, stderr)
			}
			var rec history.Record
			if err := json.Unmarshal([]byte(out), &rec); err != nil {
				t.Fatal(err)
			}
			if rec.Model != "native-model" || rec.ModelSource != "config" {
				t.Fatalf("%+v", rec)
			}
			rs, err := history.Read(history.Path())
			if err != nil || len(rs) != 1 || rs[0].Model != rec.Model || rs[0].ModelSource != rec.ModelSource {
				t.Fatalf("%+v %v", rs, err)
			}
			for _, args := range [][]string{{"ls"}, {"show", rec.Session}} {
				code, out, stderr = runCLI(args...)
				if code != 0 || !strings.Contains(out, "native-model") || !strings.Contains(out, "config") {
					t.Fatalf("%v: %d %s %s", args, code, out, stderr)
				}
			}
		})
	}
}
