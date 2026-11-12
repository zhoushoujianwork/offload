package cli

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
)

func saveEffortProfile(t *testing.T, name, effort, tier string) {
	t.Helper()
	cfg, err := config.Load(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Profiles[name]
	p.ReasoningEffort, p.ServiceTier = effort, tier
	if err := config.SaveProfile(config.Path(), name, p, false); err != nil {
		t.Fatal(err)
	}
}

func TestCodexPerRunOverridesAndResume(t *testing.T) {
	work, record := setupCodex(t, codexReply)
	saveEffortProfile(t, "codex", "medium", "priority")
	before, _ := os.ReadFile(config.Path())
	for i, tc := range []struct {
		flags        []string
		effort, tier string
	}{
		{nil, "medium", "priority"},
		{[]string{"-effort", "ultra", "-fast=false", "-m", "override-model"}, "ultra", "default"},
		{[]string{"-effort", "low", "-fast"}, "low", "priority"},
		{[]string{"-effort="}, "", "priority"},
	} {
		args := []string{"-p", "codex", "-d", work}
		if i > 0 {
			args = append(args, "-r", "codex-session")
		}
		args = append(args, tc.flags...)
		args = append(args, "task")
		if code, out, stderr := runCLI(args...); code != 0 {
			t.Fatalf("%d %s %s", code, out, stderr)
		}
		got := argv(t, record)
		if !slices.Contains(got, `service_tier="`+tc.tier+`"`) {
			t.Fatalf("%q", got)
		}
		if tc.effort != "" && !slices.Contains(got, `model_reasoning_effort="`+tc.effort+`"`) {
			t.Fatalf("%q", got)
		}
		if tc.effort == "" && strings.Contains(strings.Join(got, " "), "model_reasoning_effort") {
			t.Fatalf("%q", got)
		}
		r := lastRecord(t)
		if r.ReasoningEffort != tc.effort || r.ServiceTier != tc.tier {
			t.Fatalf("%+v", r)
		}
		if i == 1 && r.Model != "override-model" {
			t.Fatalf("%+v", r)
		}
	}
	after, _ := os.ReadFile(config.Path())
	if string(before) != string(after) {
		t.Fatal("overrides modified saved profile")
	}
}

func TestCodexFreshRunOverrides(t *testing.T) {
	work, record := setupCodex(t, codexReply)
	for _, fast := range []string{"-fast", "-fast=false"} {
		if code, _, stderr := runCLI("-p", "codex", "-d", work, "-effort", "xhigh", fast, "task"); code != 0 {
			t.Fatal(code, stderr)
		}
		tier := "priority"
		if fast == "-fast=false" {
			tier = "default"
		}
		got := argv(t, record)
		if slices.Contains(got, "resume") || !slices.Contains(got, `model_reasoning_effort="xhigh"`) || !slices.Contains(got, `service_tier="`+tier+`"`) {
			t.Fatal(got)
		}
	}
}

func TestClaudePerRunEffortAndResume(t *testing.T) {
	work, record := setup(t, okReply)
	saveEffortProfile(t, "cheap", "medium", "provider-tier")
	before, _ := os.ReadFile(config.Path())
	for i, effort := range []string{"medium", "max"} {
		args := []string{"-d", work}
		if i > 0 {
			args = append(args, "-r", "s-1", "-effort", effort)
		}
		if code, _, stderr := runCLI(append(args, "task")...); code != 0 {
			t.Fatal(code, stderr)
		}
		got := argv(t, record)
		at := slices.Index(got, "--effort")
		if at < 0 || got[at+1] != effort {
			t.Fatalf("%q", got)
		}
		r := lastRecord(t)
		if r.ReasoningEffort != effort || r.ServiceTier != "" {
			t.Fatalf("%+v", r)
		}
	}
	after, _ := os.ReadFile(config.Path())
	if string(before) != string(after) {
		t.Fatal("override modified profile")
	}
}

func TestInvalidRunOverridesDoNotStartWorkers(t *testing.T) {
	for _, tc := range []struct {
		backend string
		flags   []string
		message string
	}{
		{"codex", []string{"-effort", "invalid"}, "invalid Codex reasoning effort"},
		{"claude", []string{"-effort", "ultra"}, "invalid Claude reasoning effort"},
		{"claude", []string{"-effort", "invalid"}, "invalid Claude reasoning effort"},
		{"claude", []string{"-fast"}, "fast mode is only supported by Codex profiles"},
		{"claude", []string{"-fast=false"}, "fast mode is only supported by Codex profiles"},
	} {
		t.Run(tc.backend+strings.Join(tc.flags, ""), func(t *testing.T) {
			var work, record, profile string
			if tc.backend == "codex" {
				work, record = setupCodex(t, codexReply)
				profile = "codex"
			} else {
				work, record = setup(t, okReply)
				profile = "cheap"
			}
			args := append([]string{"-p", profile, "-d", work}, tc.flags...)
			code, _, stderr := runCLI(append(args, "task")...)
			if code != 2 || !strings.Contains(stderr, tc.message) {
				t.Fatal(code, stderr)
			}
			if _, err := os.Stat(record); !os.IsNotExist(err) {
				t.Fatalf("worker started: %v", err)
			}
		})
	}
}

func TestHelpShowsRunOverrides(t *testing.T) {
	code, _, stderr := runCLI("--help")
	if code != 2 || !strings.Contains(stderr, "-effort") || !strings.Contains(stderr, "-fast") {
		t.Fatal(code, stderr)
	}
}
