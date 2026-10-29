package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/template"
	"unicode"

	"github.com/chzyer/readline"
	"github.com/manifoldco/promptui"
	"github.com/manifoldco/promptui/screenbuf"
	"github.com/zhoushoujianwork/offload/internal/agentinstall"
	"github.com/zhoushoujianwork/offload/internal/config"
)

type settingsPrompt interface {
	Choose(label string, choices []string, cursor int) (int, error)
	Slider(cursor int) (int, error)
	Ask(label, initial string) (string, error)
	Secret(label string) (string, error)
}

type terminalSettingsPrompt struct {
	in  io.ReadCloser
	out io.WriteCloser
}

// Prompts own their wrappers, never the streams supplied by the calling CLI.
type settingsWriter struct{ io.Writer }

func (settingsWriter) Close() error { return nil }

func (p terminalSettingsPrompt) Choose(label string, choices []string, cursor int) (int, error) {
	// promptui counts logical lines, so wrapped items leave stale rows on redraw.
	fit := func(text string) string {
		width := readline.GetScreenWidth()
		if width <= 0 {
			width = 80
		}
		return settingsLine(text, max(1, width-5))
	}
	funcs := template.FuncMap{}
	for name, fn := range promptui.FuncMap {
		funcs[name] = fn
	}
	funcs["fit"] = fit
	s := promptui.Select{Label: label, Items: choices, CursorPos: cursor, Size: 7,
		HideSelected: true, HideHelp: true, Stdin: p.in, Stdout: p.out,
		Templates: &promptui.SelectTemplates{
			Label: "{{ . | fit }}:", Active: "> {{ . | fit }}", Inactive: "  {{ . | fit }}",
			FuncMap: funcs,
		}}
	index, _, err := s.Run()
	return index, err
}

// Keep a rendered choice on one terminal row, including wide Unicode text.
func settingsLine(text string, width int) string {
	if width < 1 {
		width = 1
	}
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text)
	runes := readline.Runes{}
	if runes.WidthAll([]rune(text)) <= width {
		return text
	}
	suffix := "..."
	if width < len(suffix) {
		return suffix[:width]
	}
	var result strings.Builder
	used := 0
	for _, r := range text {
		used += runes.Width(r)
		if used > width-len(suffix) {
			break
		}
		result.WriteRune(r)
	}
	return result.String() + suffix
}

func offloadLevelLabel(mode string) string {
	if mode == config.ModeBasic {
		return "Less offload"
	}
	return "More offload (less caller-model work)"
}

func (p terminalSettingsPrompt) Ask(label, initial string) (string, error) {
	s := promptui.Prompt{Label: label, Default: initial, AllowEdit: true, HideEntered: true,
		Stdin: p.in, Stdout: p.out}
	return s.Run()
}

// Secret reads a masked value with readline directly. promptui.Prompt reads its cursor
// from the caller goroutine while readline's listener goroutine may still update it,
// which is a data race for secret input.
func (p terminalSettingsPrompt) Secret(label string) (string, error) {
	c := &readline.Config{Prompt: label + ": ", Stdin: p.in, Stdout: p.out, Stderr: p.out,
		EnableMask: true, MaskRune: '*', HistoryLimit: -1, UniqueEditLine: true,
		InterruptPrompt: "\n", EOFPrompt: "\n"}
	if err := c.Init(); err != nil {
		return "", err
	}
	c.Stdin = readline.NewCancelableStdin(c.Stdin)
	rl, err := readline.NewEx(c)
	if err != nil {
		return "", err
	}
	defer rl.Close()
	// Mirror promptui's masked echo, but keep the input in the readline goroutine's
	// own arguments so nothing is shared with the caller.
	screen := screenbuf.New(rl)
	c.SetListener(func(input []rune, _ int, key rune) ([]rune, int, bool) {
		switch key {
		case readline.CharEnter, readline.CharCtrlJ, readline.CharInterrupt, readline.CharDelete:
			// Readline may already have unblocked the caller; don't write during cleanup.
			return nil, 0, false
		}
		screen.Reset()
		screen.WriteString(label + ": " + strings.Repeat("*", len(input)))
		screen.Flush()
		return nil, 0, false // leave readline's buffer untouched
	})
	value, err := rl.Readline()
	if err == readline.ErrInterrupt {
		return "", promptui.ErrInterrupt
	}
	if err == io.EOF {
		return "", promptui.ErrEOF
	}
	return value, err
}

func terminalStream(stream any) bool {
	f, ok := stream.(*os.File)
	return ok && readline.IsTerminal(int(f.Fd()))
}

func cmdSettings(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		return fail(stderr, errors.New("usage: offload config"))
	}
	if !terminalStream(stdin) || !terminalStream(stdout) {
		return fail(stderr, errors.New("interactive settings require a terminal for stdin and stdout; run offload config in a terminal, or use offload mode / offload default"))
	}
	p := terminalSettingsPrompt{in: io.NopCloser(stdin), out: settingsWriter{stdout}}
	return runSettings(p, stdout, stderr)
}

func settingsCancelled(err error) bool {
	return errors.Is(err, promptui.ErrInterrupt) || errors.Is(err, readline.ErrInterrupt) || errors.Is(err, promptui.ErrEOF) || errors.Is(err, io.EOF)
}

func settingsChoice(p settingsPrompt, label string, choices []string, cursor int) (int, error) {
	index, err := p.Choose(label, choices, cursor)
	if err == nil && (index < 0 || index >= len(choices)) {
		err = errors.New("invalid settings selection")
	}
	return index, err
}

func settingsConfig() (*config.Config, string, error) {
	mode, err := config.LoadMode(config.Path())
	if err != nil {
		return nil, "", err
	}
	if _, err := os.Stat(config.Path()); errors.Is(err, os.ErrNotExist) {
		return &config.Config{}, mode, nil
	}
	cfg, err := config.Load(config.Path())
	return cfg, mode, err
}

func runSettings(p settingsPrompt, stdout, stderr io.Writer) int {
	cursor := 0
	fmt.Fprintf(stdout, "\nOffload settings\nConfiguration: %s\n", config.Path())
	fmt.Fprintln(stdout, "Up/Down: select   Enter: apply   Ctrl-C: back/exit")
	if override := os.Getenv("OFFLOAD_PROFILE"); override != "" {
		fmt.Fprintf(stdout, "Shell override: OFFLOAD_PROFILE=%s (takes priority over the saved default).\n", override)
	}
	for {
		cfg, mode, err := settingsConfig()
		if err != nil {
			return fail(stderr, err)
		}
		disabled, err := config.LoadDisabled(config.Path())
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "Offload globally disabled: %t (offload disable / enable)\n", disabled)
		choices := []string{"Profile: " + firstNonEmpty(cfg.Default, "(not configured)"), "Offload level: " + offloadLevelLabel(mode), "Exit"}
		selected, err := settingsChoice(p, "Settings", choices, cursor)
		if settingsCancelled(err) {
			return 0
		}
		if err != nil {
			return fail(stderr, err)
		}
		cursor = selected
		switch selected {
		case 0:
			err = settingsProfile(p, cfg)
		case 1:
			err = settingsMode(p, mode, stderr)
		case 2:
			return 0
		}
		if err != nil && !settingsCancelled(err) {
			fmt.Fprintln(stderr, "offload:", err)
		}
	}
}

func settingsMode(p settingsPrompt, current string, stderr io.Writer) error {
	modes := []string{config.ModeBasic, config.ModeProactive}
	cursor := 0
	if current == config.ModeProactive {
		cursor = 1
	}
	index, err := p.Slider(cursor)
	if err != nil {
		return err
	}
	if index < 0 || index >= len(modes) {
		return errors.New("invalid offload level")
	}
	agentinstall.RunMode([]string{modes[index]}, io.Discard, stderr)
	return nil // RunMode reports any install/persistence failure; keep the menu available.
}

func settingsProfile(p settingsPrompt, cfg *config.Config) error {
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return settingsWizard(p, cfg, "", true)
	}
	labels := make([]string, 0, len(names)+3)
	cursor := 0
	for i, name := range names {
		profile := cfg.Profiles[name]
		label := fmt.Sprintf("%s | %s | %s", name, profile.BackendName(), firstNonEmpty(profile.Model, "(native default)"))
		if profile.ReasoningEffort != "" {
			label += " | reasoning: " + profile.ReasoningEffort
		}
		if profile.ServiceTier != "" {
			speed := "Standard"
			if profile.ServiceTier == "priority" {
				speed = "Fast"
			}
			label += " | " + speed
		}
		labels = append(labels, label)
		if name == cfg.Default {
			cursor = i
		}
	}
	labels = append(labels, "Add profile")
	_, _, currentErr := cfg.Resolve(cfg.Default)
	canEdit := cfg.Default != "" && currentErr == nil
	if canEdit {
		labels = append(labels, "Edit current profile")
	}
	labels = append(labels, "Back")
	index, err := settingsChoice(p, "Profile", labels, cursor)
	if err != nil {
		return err
	}
	if index < len(names) {
		return config.SetDefault(config.Path(), names[index])
	}
	if index == len(names) {
		return settingsWizard(p, cfg, "", true)
	}
	if canEdit && index == len(names)+1 {
		return settingsWizard(p, cfg, cfg.Default, false)
	}
	return nil
}

func settingsWizard(p settingsPrompt, cfg *config.Config, name string, add bool) error {
	profile := cfg.Profiles[name]
	var err error
	if add {
		profile = config.Profile{}
		name, err = p.Ask("Profile name", "")
		if err != nil {
			return err
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return errors.New("profile name must not be empty")
		}
		if _, exists := cfg.Profiles[name]; exists {
			return errors.New("profile already exists")
		}
		index, err := settingsChoice(p, "Backend", []string{"Codex", "Claude gateway"}, 0)
		if err != nil {
			return err
		}
		profile.Backend = []string{"codex", "claude"}[index]
	}
	if profile.BackendName() == "codex" {
		profile.Model, err = p.Ask("Model (empty inherits Codex default)", profile.Model)
		if err != nil {
			return err
		}
		efforts := []string{"", "low", "medium", "high", "xhigh"}
		cursor := 0
		for i, effort := range efforts {
			if effort == profile.ReasoningEffort {
				cursor = i
			}
		}
		labels := []string{"Inherit", "Low", "Medium", "High", "Extra high"}
		if profile.ReasoningEffort == "max" || profile.ReasoningEffort == "ultra" {
			efforts = append(efforts, profile.ReasoningEffort)
			labels = append(labels, "Current: "+profile.ReasoningEffort)
			cursor = len(efforts) - 1
		}

		index, err := settingsChoice(p, "Reasoning", labels, cursor)
		if err != nil {
			return err
		}
		profile.ReasoningEffort = efforts[index]
		tiers := []string{"", "default", "priority"}
		cursor = 0
		for i, tier := range tiers {
			if tier == profile.ServiceTier {
				cursor = i
			}
		}
		index, err = settingsChoice(p, "Speed", []string{"Inherit", "Standard", "Fast"}, cursor)
		if err != nil {
			return err
		}
		profile.ServiceTier = tiers[index]
	} else {
		profile.BaseURL, err = p.Ask("Base URL", profile.BaseURL)
		if err != nil {
			return err
		}
		profile.BaseURL = strings.TrimSpace(profile.BaseURL)
		profile.Model, err = p.Ask("Model (required)", profile.Model)
		if err != nil {
			return err
		}
		labels := []string{"Token command", "API token"}
		keep := profile.TokenCmd != "" || profile.Token != ""
		if keep {
			labels = append([]string{"Keep current authentication"}, labels...)
		}
		index, err := settingsChoice(p, "Authentication", labels, 0)
		if err != nil {
			return err
		}
		if !keep || index != 0 {
			if keep {
				index--
			}
			var value string
			if index == 0 {
				value, err = p.Ask("Token command", "")
			} else {
				value, err = p.Secret("API token")
			}
			if err != nil {
				return err
			}
			value = strings.TrimSpace(value)
			if value == "" {
				return errors.New("authentication is required")
			}
			profile.Token, profile.TokenCmd = "", ""
			if index == 0 {
				profile.TokenCmd = value
			} else {
				profile.Token = value
			}
		}
	}
	profile.Model = strings.TrimSpace(profile.Model)
	return config.SaveProfile(config.Path(), name, profile, add)
}
