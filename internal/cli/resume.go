package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/worker"
)

const resumeUsage = "usage: offload resume --id <run-id|session-id> [--json] [--caller name] [--caller-run-id id] [--events-file path]"

const continueTask = "Continue the original task in this same session. First verify existing edits, commits, checks and external effects; do not repeat completed actions. Stay within the original scope and permissions. Complete the remaining work and report any blocker.\n\nOriginal task:\n"

func savedResumeSettings(o worker.Opts) *history.ResumeSettings {
	a := []string{"-p", o.ProfileName, "-m", o.Model, "-d", o.Dir, "-t", o.Timeout.String(),
		"-execution-policy", o.ExecutionPolicy, "-on-unsupported-tools", o.OnUnsupportedTools}
	if o.Write {
		a = append(a, "-w")
	}
	if o.NoSalvage {
		a = append(a, "-no-salvage")
	}
	if o.ToolsDisabled {
		a = append(a, "-tools-disabled")
	}
	if o.IdleTimeout != nil {
		a = append(a, "-idle", o.IdleTimeout.String())
	}
	for _, p := range [][2]string{{"-instructions-file", o.InstructionsFile}, {"-codex-sandbox", o.CodexSandbox},
		{"-allowed-tools", o.AllowedTools}, {"-disallowed-tools", o.DisallowedTools}} {
		if p[1] != "" {
			a = append(a, p[0], p[1])
		}
	}
	// Explicitly replace inherited directories, including an intentionally empty set.
	if len(o.AddDirs) == 0 {
		a = append(a, "-clear-add-dirs")
	}
	for _, d := range o.AddDirs {
		a = append(a, "-add-dir", d)
	}
	return &history.ResumeSettings{Version: 1, Args: a, Task: o.Task}
}

func cmdResume(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("offload resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("id", "", "saved run ID or session ID")
	jsonOut := fs.Bool("json", false, "print the structured run result")
	caller := fs.String("caller", "", "caller of this invocation")
	correlation := fs.String("caller-run-id", "", "correlation for this invocation")
	events := fs.String("events-file", "", "new exclusive-create events file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *id == "" || fs.NArg() != 0 {
		return fail(stderr, errors.New(resumeUsage))
	}
	rs, err := history.Read(history.Path())
	if err != nil {
		return fail(stderr, err)
	}
	var previous *history.Record
	for i := len(rs) - 1; i >= 0; i-- {
		if rs[i].RunID == *id || "run:"+rs[i].RunID == *id || rs[i].Session == *id {
			previous = &rs[i]
			break
		}
	}
	if previous == nil {
		return fail(stderr, errors.New("no saved run or session with that id"))
	}
	if previous.Session == "" {
		return fail(stderr, errors.New("run has no provider session to resume"))
	}
	if previous.ResumeSettings == nil || previous.ResumeSettings.Version != 1 {
		return fail(stderr, fmt.Errorf("run has no supported saved execution settings; inspect offload show %s and use offload -r %s with explicit task and execution flags", previous.Session, previous.Session))
	}
	cfg, err := config.Load(config.Path())
	if err != nil {
		return fail(stderr, err)
	}
	p, ok := cfg.Profiles[previous.Profile]
	if !ok || p.BackendName() != previous.Backend {
		return fail(stderr, errors.New("original profile is missing or its backend changed; restore it before resuming"))
	}
	a := append([]string{}, previous.ResumeSettings.Args...)
	a = append(a, "-r", previous.Session)
	if *jsonOut {
		a = append(a, "-json")
	}
	for _, p := range [][2]string{{"-caller", *caller}, {"-caller-run-id", *correlation}, {"-events-file", *events}} {
		if p[1] != "" {
			a = append(a, p[0], p[1])
		}
	}
	a = append(a, "--", continueTask+previous.ResumeSettings.Task)
	return cmdRunSaved(a, strings.NewReader(""), stdout, stderr, previous)
}
