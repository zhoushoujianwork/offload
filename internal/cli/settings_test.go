package cli

import (
	"bytes"
	"encoding/json"
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
	f := &fakePrompt{steps: []step{choose(0), choose(0), choose(3)}}
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
	if f.calls[2].choices[0] != "Offload level: Less offload" {
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

	f := &fakePrompt{steps: []step{choose(1), choose(0), choose(3)}}
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
	if sub.label != "Default profile" {
		t.Fatalf("submenu label = %q", sub.label)
	}
	wantChoices := []string{"a | claude | ma", "b | claude | mb", "c | claude | mc"}
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

func TestSettingsModelEditPreservesSecretsAndSupportsCodexBlank(t *testing.T) {
	settingsEnv(t)
	writeSettingsConfig(t, `{"default":"claude1","profiles":{
		"codex1":{"backend":"codex","model":"o3","token":"tok-codex"},
		"claude1":{"backend":"claude","base_url":"https://x","model":"m1","token_cmd":"echo tok"}}}`)
	// Sorted order: claude1 (index 0), codex1 (index 1).
	f := &fakePrompt{steps: []step{
		choose(2), choose(1), ask(""), // codex1 -> blank model allowed
		choose(2), choose(0), ask("m2"), // claude1 -> nonblank model required
		choose(3),
	}}
	code, out, errOut := runSettingsTest(f)
	if code != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if strings.Contains(out, "Model saved") {
		t.Fatalf("unexpected save confirmations: %q", out)
	}
	cfg, err := config.Load(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	codex1 := cfg.Profiles["codex1"]
	if codex1.Model != "" || codex1.Token != "tok-codex" || codex1.BackendName() != "codex" {
		t.Fatalf("codex1 = %+v, want blank model with secrets/backend preserved", codex1)
	}
	claude1 := cfg.Profiles["claude1"]
	if claude1.Model != "m2" || claude1.BaseURL != "https://x" || claude1.TokenCmd != "echo tok" {
		t.Fatalf("claude1 = %+v, want model=m2 with base_url/token_cmd preserved", claude1)
	}
	// The Ask prompt must never surface secrets.
	for _, c := range f.calls {
		if strings.Contains(c.label, "tok-codex") || strings.Contains(c.label, "echo tok") {
			t.Fatalf("secret leaked into prompt label: %q", c.label)
		}
	}
}

func TestSettingsInvalidModelReportsErrorAndMenuStaysUsable(t *testing.T) {
	settingsEnv(t)
	original := `{"default":"claude1","profiles":{"claude1":{"backend":"claude","base_url":"https://x","model":"m1","token_cmd":"echo tok"}}}`
	writeSettingsConfig(t, original)
	f := &fakePrompt{steps: []step{choose(2), choose(0), ask(""), choose(3)}}
	code, out, errOut := runSettingsTest(f)
	if code != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if !strings.Contains(errOut, "offload:") || !strings.Contains(errOut, "needs base_url and model") {
		t.Fatalf("missing actionable error: %q", errOut)
	}
	b, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(original), &want); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Fatalf("config mutated after failed save: %s", b)
	}
	// Menu kept running after the error: Exit still ran as the final queued step.
	if f.pos != len(f.steps) {
		t.Fatalf("not all steps consumed: pos=%d of %d", f.pos, len(f.steps))
	}
}

func TestSettingsCancellationReturnsToParentWithoutWrites(t *testing.T) {
	t.Run("submenu cancel", func(t *testing.T) {
		settingsEnv(t)
		writeSettingsConfig(t, `{"default":"a","profiles":{"a":{"base_url":"https://a","model":"ma"},"b":{"base_url":"https://b","model":"mb"}}}`)
		f := &fakePrompt{steps: []step{choose(1), failStep(io.EOF), choose(3)}}
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
		f := &fakePrompt{steps: []step{choose(2), choose(0), failStep(io.EOF), choose(3)}}
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

func TestSettingsEmptyConfigReportsNoProfilesWithoutCreatingOne(t *testing.T) {
	settingsEnv(t)
	f := &fakePrompt{steps: []step{choose(2), choose(3)}}
	code, out, errOut := runSettingsTest(f)
	if code != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if !strings.Contains(errOut, "no profiles configured") || !strings.Contains(errOut, config.Path()) {
		t.Fatalf("missing actionable no-profiles error: %q", errOut)
	}
	if !slices.Contains(f.calls[0].choices, "Default profile: (not configured)") {
		t.Fatalf("menu missing default-profile placeholder: %q", out)
	}
	if _, err := os.Stat(config.Path()); !os.IsNotExist(err) {
		t.Fatalf("config created while listing profiles: %v", err)
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
