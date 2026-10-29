package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/chzyer/readline"
	"github.com/zhoushoujianwork/offload/internal/config"
)

func TestTerminalSettingsPromptRendersWithoutDuplicateHelp(t *testing.T) {
	var out bytes.Buffer
	p := terminalSettingsPrompt{in: io.NopCloser(strings.NewReader("\r")), out: settingsWriter{&out}}
	index, err := p.Choose("Assistance scope", []string{"Research & small edits", "Research + implementation"}, 0)
	if err != nil || index != 0 {
		t.Fatalf("selection = %d, %v; output: %q", index, err, out.String())
	}
	if strings.Contains(out.String(), "Use the arrow keys to navigate") {
		t.Fatalf("duplicate navigation help: %q", out.String())
	}
}

func TestSettingsLineFitsTerminalRow(t *testing.T) {
	for _, tc := range []struct {
		text  string
		width int
		want  string
	}{
		{"short", 10, "short"},
		{"long profile name", 10, "long pr..."},
		{"模型配置很长", 8, "模型..."},
		{"long", 2, ".."},
		{"long", 0, "."},
		{"a\nb\rc\td", 20, "a b c d"},
	} {
		got := settingsLine(tc.text, tc.width)
		if got != tc.want || !utf8.ValidString(got) {
			t.Errorf("settingsLine(%q, %d) = %q, want %q", tc.text, tc.width, got, tc.want)
		}
		if (readline.Runes{}).WidthAll([]rune(got)) > max(1, tc.width) {
			t.Errorf("rendered choice exceeds width: %q", got)
		}
	}
}

// step queues one scripted answer for either Choose or Ask. Whichever method
// is actually invoked next consumes it; err, when set, is returned instead of
// a value (io.EOF/ErrInterrupt simulate cancellation).
type step struct {
	index int
	text  string
	err   error
}

func choose(index int) step   { return step{index: index} }
func ask(text string) step    { return step{text: text} }
func failStep(err error) step { return step{err: err} }

type recordedCall struct {
	kind    string // "choose" or "ask"
	label   string
	choices []string
	cursor  int
	initial string
}

// fakePrompt is the injected settingsPrompt used by tests to drive runSettings
// without a real terminal. Steps are consumed in call order; once exhausted it
// returns io.EOF, mimicking Ctrl-D.
type fakePrompt struct {
	steps []step
	pos   int
	calls []recordedCall
}

func (f *fakePrompt) Choose(label string, choices []string, cursor int) (int, error) {
	f.calls = append(f.calls, recordedCall{kind: "choose", label: label, choices: append([]string{}, choices...), cursor: cursor})
	if f.pos >= len(f.steps) {
		return 0, io.EOF
	}
	s := f.steps[f.pos]
	f.pos++
	if s.err != nil {
		return 0, s.err
	}
	return s.index, nil
}

func (f *fakePrompt) Slider(cursor int) (int, error) {
	return f.Choose("Offload level", []string{"Less offload", "More offload"}, cursor)
}

func (f *fakePrompt) Ask(label, initial string) (string, error) {
	f.calls = append(f.calls, recordedCall{kind: "ask", label: label, initial: initial})
	if f.pos >= len(f.steps) {
		return "", io.EOF
	}
	s := f.steps[f.pos]
	f.pos++
	if s.err != nil {
		return "", s.err
	}
	return s.text, nil
}

func (f *fakePrompt) Secret(label string) (string, error) {
	value, err := f.Ask(label, "")
	f.calls[len(f.calls)-1].kind = "secret"
	return value, err
}

// settingsEnv isolates HOME/CODEX_HOME/CLAUDE_CONFIG_DIR/XDG dirs so install-agent
// writes and config reads/writes never touch the real machine.
func settingsEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
	t.Setenv("OFFLOAD_PROFILE", "")
	return home
}

func writeSettingsConfig(t *testing.T, contents string) {
	t.Helper()
	if err := os.MkdirAll(config.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.Path(), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runSettingsTest(f *fakePrompt) (code int, out, errOut string) {
	var o, e bytes.Buffer
	code = runSettings(f, &o, &e)
	return code, o.String(), e.String()
}

func assertNoANSI(t *testing.T, s string) {
	t.Helper()
	if strings.Contains(s, "\x1b") {
		t.Fatalf("unexpected ANSI escape sequence in output: %q", s)
	}
}

func TestSettingsModeChangePersistsAndInstallsRules(t *testing.T) {
	home := settingsEnv(t)
	f := &fakePrompt{steps: []step{choose(1), choose(0), ask(""), choose(2)}}
	code, out, errOut := runSettingsTest(f)
	if code != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if strings.Contains(out, "updated ") || strings.Contains(out, "Delegation mode:") || strings.Count(out, "Offload settings") != 1 {
		t.Fatalf("unexpected change logs or repeated header: %q", out)
	}
	mode, err := config.LoadMode(config.Path())
	if err != nil || mode != "basic" {
		t.Fatalf("mode=%q err=%v, want basic", mode, err)
	}
	for _, path := range []string{
		filepath.Join(home, ".codex", "AGENTS.md"),
		filepath.Join(home, ".claude", "CLAUDE.md"),
		filepath.Join(home, ".agents", "skills", "offload", "SKILL.md"),
		filepath.Join(home, ".claude", "skills", "offload", "SKILL.md"),
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(b), "Delegation mode: basic") {
			t.Fatalf("%s missing basic delegation marker: %q", path, b)
		}
	}
	if f.calls[0].label != "Settings" {
		t.Fatalf("top menu label = %q", f.calls[0].label)
	}
	if f.calls[1].label != "Offload level" || f.calls[1].cursor != 1 {
		t.Fatalf("mode submenu call = %+v, want cursor on proactive (current default)", f.calls[1])
	}
	if !slices.Equal(f.calls[1].choices, []string{"Less offload", "More offload"}) {
		t.Fatalf("unclear assistance choices: %v", f.calls[1].choices)
	}
	if f.calls[2].choices[1] != "Offload level: Less offload" {
		t.Fatalf("scope not reflected in menu: %v", f.calls[2].choices)
	}
}

func TestSettingsDefaultProfileSortedHighlightedIgnoresEnvOverride(t *testing.T) {
	settingsEnv(t)
	writeSettingsConfig(t, `{"default":"b","profiles":{
		"b":{"base_url":"https://b","model":"mb","token":"tok-b"},
		"a":{"base_url":"https://a","model":"ma","token":"tok-a"},
		"c":{"base_url":"https://c","model":"mc","token":"tok-c"}}}`)
	t.Setenv("OFFLOAD_PROFILE", "c")

	f := &fakePrompt{steps: []step{choose(0), choose(0), choose(2)}}
	code, out, errOut := runSettingsTest(f)
	if code != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if !strings.Contains(out, "Shell override: OFFLOAD_PROFILE=c") {
		t.Fatalf("missing override note: %q", out)
	}
	if strings.Contains(out, "saved:") {
		t.Fatalf("unexpected save confirmation: %q", out)
	}
	sub := f.calls[1]
	if sub.label != "Profile" {
		t.Fatalf("submenu label = %q", sub.label)
	}
	wantChoices := []string{"a | claude | ma", "b | claude | mb", "c | claude | mc", "Add profile", "Edit current profile", "Back"}
	if !slices.Equal(sub.choices, wantChoices) {
		t.Fatalf("choices = %v, want %v", sub.choices, wantChoices)
	}
	if sub.cursor != 1 {
		t.Fatalf("cursor = %d, want 1 (highlight the saved default 'b', not the OFFLOAD_PROFILE override)", sub.cursor)
	}
	cfg, err := config.Load(config.Path())
	if err != nil || cfg.Default != "a" {
		t.Fatalf("default = %q err=%v, want a", cfg.Default, err)
	}
	if cfg.Profiles["a"].Token != "tok-a" || cfg.Profiles["b"].Token != "tok-b" || cfg.Profiles["c"].Token != "tok-c" {
		t.Fatalf("secrets not preserved: %+v", cfg.Profiles)
	}
}

func TestSettingsAddBundlesFromMissingConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		steps   []step
		backend string
	}{
		{"codex", []step{ask(" quick "), choose(0), ask("gpt-test"), choose(1), choose(2), ask("")}, "codex"},
		{"command", []step{ask("quick"), choose(1), ask("https://gateway.example"), ask("m"), choose(0), choose(0), ask("never-execute-this-command"), ask("")}, "claude"},
		{"token", []step{ask("quick"), choose(1), ask("http://localhost:8080"), ask("m"), choose(2), choose(1), ask("secret-value"), ask("")}, "claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settingsEnv(t)
			steps := append([]step{choose(0)}, tc.steps...)
			steps = append(steps, choose(2))
			f := &fakePrompt{steps: steps}
			code, out, errOut := runSettingsTest(f)
			if code != 0 || errOut != "" {
				t.Fatalf("%d %s %s", code, out, errOut)
			}
			cfg, err := config.Load(config.Path())
			if err != nil {
				t.Fatal(err)
			}
			p := cfg.Profiles["quick"]
			if cfg.Default != "quick" || p.Backend != tc.backend {
				t.Fatalf("%+v", cfg)
			}
			if tc.name == "codex" && (p.ReasoningEffort != "low" || p.ServiceTier != "priority") {
				t.Fatal(p)
			}
			if tc.name == "command" && p.TokenCmd != "never-execute-this-command" {
				t.Fatal(p)
			}
			if tc.name == "token" && (p.Token != "secret-value" || f.calls[7].kind != "secret") {
				t.Fatal(f.calls)
			}
			if tc.name == "token" && (p.Endpoint != "llm" || f.calls[5].label != "Endpoint" ||
				!slices.Equal(f.calls[5].choices, []string{"Unset", "cc", "llm"})) {
				t.Fatal(p, f.calls[5])
			}
			if tc.name != "token" && p.Endpoint != "" {
				t.Fatal(p)
			}
			for _, c := range f.calls {
				if strings.Contains(c.label, "secret-value") || c.initial == "secret-value" {
					t.Fatal(c)
				}
			}
			if strings.Contains(out+errOut, "secret-value") {
				t.Fatal("secret echoed")
			}
			if len(f.calls[0].choices) != 3 || !slices.Equal(f.calls[0].choices, []string{"Profile: (not configured)", "Offload level: More offload (less caller-model work)", "Exit"}) {
				t.Fatal(f.calls[0])
			}
			st, _ := os.Stat(config.Path())
			if st.Mode().Perm() != 0600 {
				t.Fatal(st.Mode())
			}
		})
	}
}

func TestSettingsWizardCancellationEveryStage(t *testing.T) {
	for _, answers := range [][]step{
		{ask("new"), choose(0), ask("m"), choose(2), choose(1), ask("")},
		{ask("new"), choose(1), ask("https://x"), ask("m"), choose(0), choose(0), ask("command"), ask("")},
		{ask("new"), choose(1), ask("https://x"), ask("m"), choose(0), choose(1), ask("secret"), ask("")},
	} {
		for stage := range answers {
			t.Run(fmt.Sprintf("%d-%d", len(answers), stage), func(t *testing.T) {
				settingsEnv(t)
				f := &fakePrompt{steps: append(append([]step{choose(0)}, answers[:stage]...), failStep(io.EOF), choose(2))}
				code, _, errOut := runSettingsTest(f)
				if code != 0 || errOut != "" {
					t.Fatalf("%d %s", code, errOut)
				}
				if _, err := os.Stat(config.Path()); !os.IsNotExist(err) {
					t.Fatalf("created config: %v", err)
				}
			})
		}
	}
}

func TestSettingsEditAuthenticationAndUnknownFields(t *testing.T) {
	for _, auth := range []struct {
		choice                int
		value, token, command string
	}{
		{0, "", "old-token", "old-command"}, {1, "new-command", "", "new-command"}, {2, "new-token", "new-token", ""},
	} {
		t.Run(fmt.Sprint(auth.choice), func(t *testing.T) {
			settingsEnv(t)
			writeSettingsConfig(t, `{"default":"a","future":true,"profiles":{"a":{"base_url":"https://x","model":"m","token":"old-token","token_cmd":"old-command","future":{"keep":1}}}}`)
			steps := []step{choose(0), choose(2), ask("https://y"), ask("new-model"), choose(0), choose(auth.choice)}
			if auth.choice != 0 {
				steps = append(steps, ask(auth.value))
			}
			steps = append(steps, ask(""), choose(2))
			f := &fakePrompt{steps: steps}
			code, out, errOut := runSettingsTest(f)
			if code != 0 || errOut != "" {
				t.Fatalf("%d %s %s", code, out, errOut)
			}
			cfg, _ := config.Load(config.Path())
			p := cfg.Profiles["a"]
			if cfg.Default != "a" || p.Token != auth.token || p.TokenCmd != auth.command || p.Model != "new-model" || p.BaseURL != "https://y" || p.Backend != "" {
				t.Fatal(cfg)
			}
			b, _ := os.ReadFile(config.Path())
			var fields map[string]any
			json.Unmarshal(b, &fields)
			if fields["future"] != true || fields["profiles"].(map[string]any)["a"].(map[string]any)["future"] == nil {
				t.Fatal(string(b))
			}
			for _, call := range f.calls {
				if call.initial == "old-token" || call.initial == "old-command" || strings.Contains(call.label, "old-token") {
					t.Fatal(call)
				}
			}
		})
	}
}

func TestSettingsEditSavesEndpointAndPreselectsCurrent(t *testing.T) {
	settingsEnv(t)
	writeSettingsConfig(t, `{"default":"a","profiles":{"a":{"base_url":"https://x","model":"m","token":"tok","endpoint":"llm"}}}`)
	// Edit current profile, keep model/url, set endpoint to cc, keep authentication.
	f := &fakePrompt{steps: []step{choose(0), choose(2), ask("https://x"), ask("m"), choose(1), choose(0), ask(""), choose(2)}}
	code, out, errOut := runSettingsTest(f)
	if code != 0 || errOut != "" {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	var endpointCall *recordedCall
	for i := range f.calls {
		if f.calls[i].label == "Endpoint" {
			endpointCall = &f.calls[i]
		}
	}
	if endpointCall == nil || endpointCall.cursor != 2 || !slices.Equal(endpointCall.choices, []string{"Unset", "cc", "llm"}) {
		t.Fatalf("endpoint prompt = %+v, want cursor on current llm", endpointCall)
	}
	cfg, err := config.Load(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	if p := cfg.Profiles["a"]; p.Endpoint != "cc" || p.Token != "tok" {
		t.Fatalf("profile = %+v", p)
	}
	// Selecting Unset clears the field from the saved JSON.
	f = &fakePrompt{steps: []step{choose(0), choose(2), ask("https://x"), ask("m"), choose(0), choose(0), ask(""), choose(2)}}
	if code, out, errOut := runSettingsTest(f); code != 0 || errOut != "" {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	b, _ := os.ReadFile(config.Path())
	if strings.Contains(string(b), "endpoint") {
		t.Fatalf("endpoint not cleared: %s", b)
	}
}

func TestSettingsProfileLabelShowsEndpoint(t *testing.T) {
	settingsEnv(t)
	writeSettingsConfig(t, `{"default":"a","profiles":{"a":{"base_url":"https://a","model":"ma","endpoint":"cc"},"b":{"base_url":"https://b","model":"mb"}}}`)
	f := &fakePrompt{steps: []step{choose(0), failStep(io.EOF), choose(2)}}
	if code, out, errOut := runSettingsTest(f); code != 0 {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	want := []string{"a | claude | ma | endpoint: cc", "b | claude | mb"}
	if got := f.calls[1].choices[:2]; !slices.Equal(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
}

func TestSettingsCancellationReturnsToParentWithoutWrites(t *testing.T) {
	t.Run("submenu cancel", func(t *testing.T) {
		settingsEnv(t)
		writeSettingsConfig(t, `{"default":"a","profiles":{"a":{"base_url":"https://a","model":"ma"},"b":{"base_url":"https://b","model":"mb"}}}`)
		f := &fakePrompt{steps: []step{choose(0), failStep(io.EOF), choose(2)}}
		code, _, errOut := runSettingsTest(f)
		if code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		cfg, err := config.Load(config.Path())
		if err != nil || cfg.Default != "a" {
			t.Fatalf("default changed after cancellation: %q err=%v", cfg.Default, err)
		}
		if f.pos != len(f.steps) {
			t.Fatalf("menu did not return to parent and keep running: pos=%d of %d", f.pos, len(f.steps))
		}
	})
	t.Run("text prompt cancel", func(t *testing.T) {
		settingsEnv(t)
		original := `{"default":"a","profiles":{"a":{"base_url":"https://a","model":"ma"}}}`
		writeSettingsConfig(t, original)
		f := &fakePrompt{steps: []step{choose(0), choose(2), failStep(io.EOF), choose(2)}}
		code, _, errOut := runSettingsTest(f)
		if code != 0 {
			t.Fatalf("code=%d err=%q", code, errOut)
		}
		b, err := os.ReadFile(config.Path())
		if err != nil || string(b) != original {
			t.Fatalf("config written after cancelled text prompt: %s err=%v", b, err)
		}
	})
}

func TestSettingsMainMenuEOFExitsCleanlyNoMutations(t *testing.T) {
	settingsEnv(t)
	f := &fakePrompt{}
	code, out, errOut := runSettingsTest(f)
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d out=%q err=%q, want clean exit", code, out, errOut)
	}
	if _, err := os.Stat(config.Path()); !os.IsNotExist(err) {
		t.Fatalf("config created on EOF: %v", err)
	}
}

func TestSettingsMalformedConfigErrorsWithoutMutationOrANSI(t *testing.T) {
	settingsEnv(t)
	malformed := `{"default": "a", "profiles": {`
	writeSettingsConfig(t, malformed)
	f := &fakePrompt{}
	code, out, errOut := runSettingsTest(f)
	if code != 2 {
		t.Fatalf("code=%d out=%q err=%q, want 2", code, out, errOut)
	}
	if !strings.Contains(errOut, "offload:") || !strings.Contains(errOut, "parse") {
		t.Fatalf("missing clear parse error: %q", errOut)
	}
	if len(f.calls) != 0 {
		t.Fatalf("prompt invoked despite malformed config: %+v", f.calls)
	}
	b, err := os.ReadFile(config.Path())
	if err != nil || string(b) != malformed {
		t.Fatalf("malformed config mutated: %s err=%v", b, err)
	}
	assertNoANSI(t, out)
	assertNoANSI(t, errOut)
}

func TestCmdSettingsNonTTYErrorsWithoutMutationOrANSI(t *testing.T) {
	settingsEnv(t)
	var out, errOut bytes.Buffer
	code := Run("test", []string{"config"}, strings.NewReader(""), &out, &errOut)
	if code != 2 {
		t.Fatalf("code=%d out=%q err=%q, want 2", code, out.String(), errOut.String())
	}
	msg := errOut.String()
	if !strings.Contains(msg, "terminal") || !strings.Contains(msg, "offload mode") || !strings.Contains(msg, "offload default") {
		t.Fatalf("missing actionable nonTTY error: %q", msg)
	}
	if _, err := os.Stat(config.Path()); !os.IsNotExist(err) {
		t.Fatalf("config created on nonTTY config invocation: %v", err)
	}
	assertNoANSI(t, out.String())
	assertNoANSI(t, msg)
}

func TestCmdSettingsExtraArgsFailsUsage(t *testing.T) {
	settingsEnv(t)
	var out, errOut bytes.Buffer
	code := Run("test", []string{"config", "extra"}, strings.NewReader(""), &out, &errOut)
	if code != 2 {
		t.Fatalf("code=%d out=%q err=%q, want 2", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "usage: offload config") {
		t.Fatalf("missing usage error: %q", errOut.String())
	}
	if _, err := os.Stat(config.Path()); !os.IsNotExist(err) {
		t.Fatalf("config created on invalid config invocation: %v", err)
	}
}

func TestNoArgPipedTaskKeepsRunningTaskNotSettings(t *testing.T) {
	work, record := setup(t, okReply)
	var out, errOut bytes.Buffer
	t.Chdir(work)
	code := Run("test", nil, strings.NewReader("piped task"), &out, &errOut)
	if code != 0 || strings.TrimSpace(out.String()) != "REPORT" {
		t.Fatalf("code=%d out=%q err=%q", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "session=s-1") {
		t.Fatalf("missing normal run metadata: %q", errOut.String())
	}
	rec, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("worker never ran (settings menu entered instead?): %v", err)
	}
	if !strings.Contains(string(rec), "piped task") {
		t.Fatalf("piped task text missing from worker invocation: %q", rec)
	}
}

func TestTerminalSettingsSecretIsMasked(t *testing.T) {
	var out bytes.Buffer
	p := terminalSettingsPrompt{in: io.NopCloser(strings.NewReader("private-token\r")), out: settingsWriter{&out}}
	value, err := p.Secret("API token")
	if err != nil || value != "private-token" {
		t.Fatalf("secret prompt error: %v", err)
	}
	if strings.Contains(out.String(), "private-token") || !strings.Contains(out.String(), "*") {
		t.Fatal("secret was not masked")
	}
}

func TestSettingsRejectedWizardKeepsMenuUsable(t *testing.T) {
	for _, answers := range [][]step{
		{ask(" ")}, {ask(" a ")},
		{ask("new"), choose(1), ask("file:///tmp"), ask("m"), choose(1), ask("secret"), ask("")},
		{ask("new"), choose(1), ask("https://x"), ask(" "), choose(1), ask("secret"), ask("")},
	} {
		t.Run(fmt.Sprint(len(answers), answers[0].text), func(t *testing.T) {
			settingsEnv(t)
			original := `{"default":"a","profiles":{"a":{"backend":"codex"}}}`
			writeSettingsConfig(t, original)
			steps := append(append([]step{choose(0), choose(1)}, answers...), choose(2))
			f := &fakePrompt{steps: steps}
			code, _, errOut := runSettingsTest(f)
			if code != 0 || errOut == "" || f.pos != len(steps) {
				t.Fatalf("%d %q %d", code, errOut, f.pos)
			}
			b, _ := os.ReadFile(config.Path())
			if string(b) != original {
				t.Fatal("rejected wizard wrote config")
			}
		})
	}
}

func TestSettingsEditCancellationEveryStage(t *testing.T) {
	for _, answers := range [][]step{
		{ask("model"), choose(2), choose(1), ask("")},
		{ask("https://y"), ask("model"), choose(1), ask("new-command"), ask("")},
		{ask("https://y"), ask("model"), choose(2), ask("new-token"), ask("")},
	} {
		for stage := range answers {
			t.Run(fmt.Sprint(len(answers), stage, answers[len(answers)-1]), func(t *testing.T) {
				settingsEnv(t)
				original := `{"default":"a","profiles":{"a":{"backend":"claude","base_url":"https://x","model":"m","token":"old"}},"future":true}`
				if answers[0].text == "model" {
					original = `{"default":"a","profiles":{"a":{"backend":"codex","model":"m","reasoning_effort":"ultra","service_tier":"priority"}},"future":true}`
				}
				writeSettingsConfig(t, original)
				steps := append(append([]step{choose(0), choose(2)}, answers[:stage]...), failStep(io.EOF), choose(2))
				f := &fakePrompt{steps: steps}
				code, _, errOut := runSettingsTest(f)
				if code != 0 || errOut != "" {
					t.Fatalf("%d %q", code, errOut)
				}
				b, _ := os.ReadFile(config.Path())
				if string(b) != original {
					t.Fatal("cancelled edit wrote config")
				}
			})
		}
	}
}

func TestSettingsSwitchBundlesPreservesProfiles(t *testing.T) {
	settingsEnv(t)
	original := `{"default":"quick","future":true,"profiles":{"quick":{"backend":"codex","model":"same","reasoning_effort":"low","service_tier":"default","extra":1},"deep":{"backend":"codex","model":"same","reasoning_effort":"xhigh","service_tier":"priority","extra":2}}}`
	writeSettingsConfig(t, original)
	f := &fakePrompt{steps: []step{choose(0), choose(0), choose(2)}}
	code, _, errOut := runSettingsTest(f)
	if code != 0 || errOut != "" {
		t.Fatalf("%d %s", code, errOut)
	}
	want := []string{"deep | codex | same | reasoning: xhigh | Fast", "quick | codex | same | reasoning: low | Standard", "Add profile", "Edit current profile", "Back"}
	if !slices.Equal(f.calls[1].choices, want) || f.calls[1].cursor != 1 {
		t.Fatal(f.calls[1])
	}
	b, _ := os.ReadFile(config.Path())
	var before, after map[string]any
	json.Unmarshal([]byte(original), &before)
	json.Unmarshal(b, &after)
	before["default"] = "deep"
	a, _ := json.Marshal(before)
	z, _ := json.Marshal(after)
	if !bytes.Equal(a, z) {
		t.Fatal("bundle switching changed other fields")
	}
}

func TestSettingsEditPreservesAdvancedReasoning(t *testing.T) {
	for _, effort := range []string{"max", "ultra"} {
		t.Run(effort, func(t *testing.T) {
			settingsEnv(t)
			writeSettingsConfig(t, `{"default":"a","profiles":{"a":{"backend":"codex","model":"m","reasoning_effort":"`+effort+`","service_tier":"priority"}}}`)
			f := &fakePrompt{steps: []step{choose(0), choose(2), ask("m"), choose(5), choose(2), ask(""), choose(2)}}
			code, _, errOut := runSettingsTest(f)
			if code != 0 || errOut != "" {
				t.Fatalf("%d %s", code, errOut)
			}
			if f.calls[3].cursor != 5 {
				t.Fatal("existing reasoning is not selected")
			}
			cfg, err := config.Load(config.Path())
			if err != nil || cfg.Profiles["a"].ReasoningEffort != effort {
				t.Fatalf("reasoning changed: %v", err)
			}
		})
	}
}

func TestSettingsAliasesAddRemoveAndReject(t *testing.T) {
	for _, aliases := range []string{"cc, fast", "", "taken", "a", "b", "cc, cc", "UPPER"} {
		t.Run(aliases, func(t *testing.T) {
			settingsEnv(t)
			original := `{"default":"a","profiles":{"a":{"backend":"codex","aliases":["old"]},"b":{"backend":"codex","aliases":["taken"]}}}`
			writeSettingsConfig(t, original)
			f := &fakePrompt{steps: []step{choose(0), choose(3), ask(""), choose(0), choose(0), ask(aliases), choose(2)}}
			code, _, errOut := runSettingsTest(f)
			valid := aliases == "cc, fast" || aliases == ""
			if code != 0 || (errOut == "") != valid || f.pos != len(f.steps) {
				t.Fatalf("code=%d err=%q pos=%d", code, errOut, f.pos)
			}
			if f.calls[5].initial != "old" {
				t.Fatalf("alias initial value: %+v", f.calls[5])
			}
			if valid {
				cfg, err := config.Load(config.Path())
				if err != nil {
					t.Fatal(err)
				}
				want := []string(nil)
				if aliases != "" {
					want = []string{"cc", "fast"}
				}
				if !slices.Equal(cfg.Profiles["a"].Aliases, want) {
					t.Fatal(cfg.Profiles["a"].Aliases)
				}
			} else {
				got, _ := os.ReadFile(config.Path())
				if string(got) != original {
					t.Fatal("rejected aliases wrote configuration")
				}
			}
		})
	}
}
