package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chzyer/readline"
	"github.com/zhoushoujianwork/offload/internal/config"
)

func TestSettingsSliderNavigation(t *testing.T) {
	for _, tc := range []struct {
		name, keys  string
		start, want int
		cancel      bool
	}{
		{"right", "\x1b[C\r", 0, 1, false},
		{"left", "\x1b[D\r", 1, 0, false},
		{"clamp right", "\x1b[C\x1b[C\r", 1, 1, false},
		{"clamp left", "\x1b[D\x1b[D\r", 0, 0, false},
		{"keep current", "\r", 1, 1, false},
		{"cancel", "\x1b[C\x03", 0, 0, true},
		{"eof", "", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			p := terminalSettingsPrompt{in: io.NopCloser(strings.NewReader(tc.keys)), out: settingsWriter{&out}}
			got, err := p.Slider(tc.start)
			if tc.cancel {
				if !settingsCancelled(err) {
					t.Fatalf("expected cancellation, got %v", err)
				}
			} else if err != nil || got != tc.want {
				t.Fatalf("got %d, %v; want %d", got, err, tc.want)
			}
			if !strings.HasSuffix(out.String(), "\x1b[?25h") {
				t.Fatalf("cursor not restored: %q", out.String())
			}
		})
	}
}

func TestSettingsSliderExplainsCallerWork(t *testing.T) {
	for _, cursor := range []int{0, 1} {
		text := strings.Join(sliderLines(cursor), "\n")
		if !strings.Contains(text, "More offload = less caller-model work.") || !strings.Contains(text, "Main agent keeps design and review.") {
			t.Fatalf("missing tradeoff: %s", text)
		}
		if strings.Contains(text, "implement") != (cursor == 1) {
			t.Fatalf("wrong scope description: %s", text)
		}
	}
}

func TestSettingsSliderCancelDoesNotSave(t *testing.T) {
	settingsEnv(t)
	writeSettingsConfig(t, `{"delegation_mode":"basic","profiles":{}}`)
	f := &fakePrompt{steps: []step{choose(0), failStep(readline.ErrInterrupt), choose(3)}}
	code, _, errOut := runSettingsTest(f)
	mode, err := config.LoadMode(config.Path())
	if code != 0 || errOut != "" || err != nil || mode != config.ModeBasic {
		t.Fatalf("cancel changed settings: code=%d stderr=%q mode=%q err=%v", code, errOut, mode, err)
	}
}

func TestSettingsModeInstallErrorRemainsVisible(t *testing.T) {
	home := settingsEnv(t)
	writeSettingsConfig(t, `{"delegation_mode":"basic","profiles":{}}`)
	skill := filepath.Join(home, ".agents", "skills", "offload")
	if err := os.MkdirAll(skill, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("user-owned skill"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakePrompt{steps: []step{choose(0), choose(1), choose(3)}}
	code, out, errOut := runSettingsTest(f)
	mode, err := config.LoadMode(config.Path())
	if code != 0 || !strings.Contains(errOut, "unmanaged skill") || err != nil || mode != config.ModeBasic {
		t.Fatalf("install failure hidden or mode changed: code=%d stderr=%q mode=%q err=%v", code, errOut, mode, err)
	}
	if strings.Contains(out, "Delegation mode:") || f.calls[2].choices[0] != "Offload level: Less offload" {
		t.Fatalf("failed change shown as applied: stdout=%q calls=%v", out, f.calls)
	}
}
