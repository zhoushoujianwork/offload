package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/chzyer/readline"
	"github.com/manifoldco/promptui"
	"github.com/zhoushoujianwork/offload/internal/agentinstall"
	"github.com/zhoushoujianwork/offload/internal/config"
)

type settingsPrompt interface {
	Choose(label string, choices []string, cursor int) (int, error)
	Ask(label, initial string) (string, error)
}

type terminalSettingsPrompt struct {
	in  io.ReadCloser
	out io.WriteCloser
}

// Prompts own their wrappers, never the streams supplied by the calling CLI.
type settingsWriter struct{ io.Writer }

func (settingsWriter) Close() error { return nil }

func (p terminalSettingsPrompt) Choose(label string, choices []string, cursor int) (int, error) {
	s := promptui.Select{Label: label, Items: choices, CursorPos: cursor, Size: 7,
		HideSelected: true, Stdin: p.in, Stdout: p.out}
	index, _, err := s.Run()
	return index, err
}

func (p terminalSettingsPrompt) Ask(label, initial string) (string, error) {
	s := promptui.Prompt{Label: label, Default: initial, AllowEdit: true, HideEntered: true,
		Stdin: p.in, Stdout: p.out}
	return s.Run()
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
	return errors.Is(err, promptui.ErrInterrupt) || errors.Is(err, promptui.ErrEOF) || errors.Is(err, io.EOF)
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
	for {
		cfg, mode, err := settingsConfig()
		if err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintf(stdout, "\nOffload settings\nConfiguration: %s\n", config.Path())
		fmt.Fprintln(stdout, "Up/Down: select   Enter: apply immediately   Ctrl-C: back/exit")
		if override := os.Getenv("OFFLOAD_PROFILE"); override != "" {
			fmt.Fprintf(stdout, "Shell override: OFFLOAD_PROFILE=%s (takes priority over the saved default).\n", override)
		}
		choices := []string{"Delegation mode: " + mode, "Default profile: " + firstNonEmpty(cfg.Default, "(not configured)"),
			"Profile model", "Exit"}
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
			err = settingsMode(p, mode, stdout, stderr)
		case 1:
			err = settingsProfile(p, cfg, false, stdout)
		case 2:
			err = settingsProfile(p, cfg, true, stdout)
		case 3:
			return 0
		}
		if err != nil && !settingsCancelled(err) {
			fmt.Fprintln(stderr, "offload:", err)
		}
	}
}

func settingsMode(p settingsPrompt, current string, stdout, stderr io.Writer) error {
	modes := []string{config.ModeBasic, config.ModeProactive}
	cursor := 0
	if current == config.ModeProactive {
		cursor = 1
	}
	index, err := settingsChoice(p, "Delegation mode", []string{
		"basic - research, localization and mechanical edits",
		"proactive - also implement, test and document after the main agent designs the change",
	}, cursor)
	if err != nil {
		return err
	}
	agentinstall.RunMode([]string{modes[index]}, stdout, stderr)
	return nil // RunMode reports any install/persistence failure; keep the menu available.
}

func settingsProfile(p settingsPrompt, cfg *config.Config, editModel bool, stdout io.Writer) error {
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Errorf("no profiles configured; add profiles to %s", config.Path())
	}
	labels := make([]string, len(names))
	cursor := 0
	for i, name := range names {
		profile := cfg.Profiles[name]
		model := profile.Model
		if model == "" {
			model = "(not set)"
			if profile.BackendName() == "codex" {
				model = "(Codex default)"
			}
		}
		labels[i] = fmt.Sprintf("%s | %s | %s", name, profile.BackendName(), model)
		if name == cfg.Default {
			cursor = i
		}
	}
	label := "Default profile"
	if editModel {
		label = "Profile to edit"
	}
	index, err := settingsChoice(p, label, labels, cursor)
	if err != nil {
		return err
	}
	name := names[index]
	if !editModel {
		if err := config.SetDefault(config.Path(), name); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "Default profile saved:", name)
		return nil
	}
	profile := cfg.Profiles[name]
	label = "Model for " + name + " (required)"
	if profile.BackendName() == "codex" {
		label = "Model for " + name + " (empty uses Codex default)"
	}
	model, err := p.Ask(label, profile.Model)
	if err != nil {
		return err
	}
	if err := config.SetModel(config.Path(), name, strings.TrimSpace(model)); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Model saved for profile %s.\n", name)
	return nil
}
