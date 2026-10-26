// Package cli implements the offload command line: running a task, and inspecting past runs.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zhoushoujianwork/offload/internal/agentinstall"
	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/live"
	"github.com/zhoushoujianwork/offload/internal/slots"
	"github.com/zhoushoujianwork/offload/internal/text"
	"github.com/zhoushoujianwork/offload/internal/worker"
)

const usage = `offload — hand a task to Claude Code or Codex; print only its final report.

usage:
  offload [flags] "task"        run a task (task may also come on stdin)
  offload config               open interactive settings (also: offload in a terminal)
  offload ls [n]                list the last n runs (default 10)
  offload show <session>        print every report of a session
  offload status                what each worker is doing right now
  offload watch [-t dur]        one line per event (start, progress, STALLED, finish) until all workers end;
                                with -t, stop after dur and exit 124 if workers are still active
  offload profiles              list profiles (* = the one a run would use)
  offload default [profile]     show or set the saved default profile
  offload mode [basic|proactive] show or switch delegation mode and agent rules
  offload install-agent [flags] install delegation instructions for a host agent
  offload version

flags:
`

// Run dispatches one offload invocation and returns the process exit code.
func Run(version string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 && terminalStream(stdin) && terminalStream(stdout) {
		return cmdSettings(nil, stdin, stdout, stderr)
	}
	if len(args) > 0 {
		switch args[0] {
		case "config":
			return cmdSettings(args[1:], stdin, stdout, stderr)
		case "ls":
			return cmdList(args[1:], stdout, stderr)
		case "show":
			return cmdShow(args[1:], stdout, stderr)
		case "status":
			return cmdStatus(stdout, stderr)
		case "watch":
			fs := flag.NewFlagSet("offload watch", flag.ContinueOnError)
			fs.SetOutput(stderr)
			limit := fs.Duration("t", 0, "stop watching after this long (exit 124 if workers are still active); default: until all finish")
			if err := fs.Parse(args[1:]); err != nil {
				return 2
			}
			if fs.NArg() != 0 || *limit < 0 {
				return fail(stderr, errors.New("usage: offload watch [-t duration]"))
			}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()
			if *limit > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, *limit)
				defer cancel()
			}
			return cmdWatch(ctx, stdout, stderr)
		case "profiles":
			return cmdProfiles(stdout, stderr)
		case "default":
			return cmdDefault(args[1:], stdout, stderr)
		case "mode":
			return agentinstall.RunMode(args[1:], stdout, stderr)
		case "install-agent":
			return agentinstall.Run(args[1:], stdout, stderr)
		case "version":
			fmt.Fprintln(stdout, version)
			return 0
		}
	}
	return cmdRun(args, stdin, stdout, stderr)
}

func cmdRun(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	started := time.Now()
	fs := flag.NewFlagSet("offload", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage); fs.PrintDefaults() }
	profile := fs.String("p", "", "profile name (default: $OFFLOAD_PROFILE, then config default)")
	model := fs.String("m", "", "override the profile's model")
	write := fs.Bool("w", false, "write mode: edit files in -d + build/test commands (default read-only)")
	resume := fs.String("r", "", "resume a session id; reuses its original dir unless -d is given")
	dir := fs.String("d", "", "working directory (default: cwd)")
	timeout := fs.Duration("t", 30*time.Minute, "timeout")
	noSalvage := fs.Bool("no-salvage", false, "skip automatic report salvage for stopped Claude workers")
	jsonOutput := fs.Bool("json", false, "print one structured run result instead of plain report")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	task := strings.Join(fs.Args(), " ")
	if task == "" && !isTerminal(stdin) {
		b, _ := io.ReadAll(stdin)
		task = strings.TrimSpace(string(b))
	}
	if task == "" {
		fs.Usage()
		return 2
	}

	logPath := history.Path()
	rec := history.Record{TS: started.Format("2006-01-02 15:04:05"), Status: "error", Profile: *profile,
		Model: *model, Dir: *dir, Task: task, CostIsEstimate: true}
	failRun := func(err error) int {
		code := 2
		rec.ExitCode, rec.Error, rec.Result = &code, err.Error(), err.Error()
		elapsed := time.Since(started).Milliseconds()
		rec.ElapsedMs = &elapsed
		return printRun(rec, logPath, *jsonOutput, stdout, stderr)
	}
	if *resume != "" {
		// The lock closes the race the live-state check below only narrows: of two simultaneous
		// resumes, one fails to take it. It is held until this process exits.
		lock, _, lockErr := slots.LockSession(slots.Dir(), *resume)
		switch {
		case errors.Is(lockErr, slots.ErrSessionBusy):
			pid := slots.HolderPID(slots.Dir(), *resume)
			return failRun(fmt.Errorf("session %s already has an active run (pid %d); do not send overlapping follow-ups to one session. "+
				"Wait for it (offload watch -t 10m), inspect the result or partial edits with offload show %s and git diff, then resume once",
				*resume, pid, *resume))
		case lockErr != nil:
			return failRun(fmt.Errorf("lock session %s: %w", *resume, lockErr))
		}
		defer lock.Release()
		if s, busy := activeSessionRun(*resume); busy {
			return failRun(fmt.Errorf("session %s already has an active run (pid %d); do not send overlapping follow-ups to one session. "+
				"Wait for it (offload watch -t 10m), inspect the result or partial edits with offload show %s and git diff, then resume once",
				*resume, s.PID, *resume))
		}
	}
	cfg, err := config.Load(config.Path())
	if err != nil {
		return failRun(err)
	}
	selected := *profile
	resumedProfile := ""
	// Resumes retain their original profile even after the default changes.
	if selected == "" && *resume != "" {
		rs, readErr := history.Read(logPath)
		if readErr != nil {
			return failRun(readErr)
		}
		if hit := history.BySession(rs, *resume); len(hit) > 0 {
			selected = hit[len(hit)-1].Profile
			resumedProfile = selected
		}
	}
	name, prof, err := cfg.Resolve(selected)
	rec.Profile, rec.Model = name, firstNonEmpty(*model, prof.Model)
	rec.Backend = prof.BackendName()
	if err != nil {
		if resumedProfile != "" {
			// Never fall back to the default profile: that would silently change the provider.
			err = fmt.Errorf("cannot resume session %s: its profile %q is unavailable (%w). Restore that profile in %s to continue the session, "+
				"or start a new session with -p <profile> and restate the context; offload show %s has the previous reports",
				*resume, resumedProfile, err, config.Path(), *resume)
		}
		return failRun(err)
	}
	wd, err := resolveDir(*dir, *resume, logPath)
	if err != nil {
		if *resume != "" && *dir == "" {
			err = fmt.Errorf("cannot resume session %s: %w. Restore that directory to continue the session, or pass -d to run elsewhere "+
				"(the session's files may not be visible there); offload show %s has the previous reports", *resume, err, *resume)
		}
		return failRun(err)
	}
	maxWorkers, err := cfg.Limits.EffectiveMaxWorkers()
	if err != nil {
		return failRun(err)
	}
	o := worker.Opts{ProfileName: name, Profile: prof, Model: firstNonEmpty(*model, prof.Model), Write: *write,
		Resume: *resume, Dir: wd, Task: task, Timeout: *timeout, StartedAt: started, NoSalvage: *noSalvage,
		MaxWorkers: maxWorkers, Limits: cfg.Limits}

	// SIGTERM is what a supervisor's own cancellation or a timeout wrapper sends; handle it like Ctrl-C
	// so the worker's process group is stopped instead of orphaned.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	rec, _ = worker.Run(ctx, o)
	return printRun(rec, logPath, *jsonOutput, stdout, stderr)
}

// activeSessionRun finds another offload process still working in the session. The live state is
// advisory, so this narrows the window for overlapping follow-ups rather than closing it.
func activeSessionRun(session string) (live.State, bool) {
	states, err := live.List()
	if err != nil {
		return live.State{}, false
	}
	for _, s := range states {
		if s.Active() && s.Session == session && s.PID != os.Getpid() {
			return s, true
		}
	}
	return live.State{}, false
}

func printRun(rec history.Record, logPath string, jsonOutput bool, stdout, stderr io.Writer) int {
	warning := ""
	if err := history.Append(logPath, rec); err != nil {
		warning = " warning=" + text.OneLine("run log not written: "+err.Error())
	}
	if jsonOutput {
		if err := json.NewEncoder(stdout).Encode(rec); err != nil {
			fmt.Fprintln(stderr, "offload: write JSON result:", err)
			return 2
		}
	} else {
		fmt.Fprintln(stdout, rec.Result)
	}
	errText := ""
	if rec.Error != "" {
		errText = " error=" + text.OneLine(rec.Error)
	}
	if rec.Salvaged {
		errText = " salvaged=true" + errText
	}
	if rec.Denials > 0 {
		errText = fmt.Sprintf(" permission_denials=%d", rec.Denials) + errText
	}
	if rec.Cleanup != "" {
		errText += " cleanup=" + text.OneLine(rec.Cleanup)
	}
	fmt.Fprintf(stderr, "[offload] %s exit_code=%s %s/%s turns=%d estimated_cost=%s elapsed=%s%s session=%s%s%s\n",
		rec.Status, exitText(rec), rec.Profile, rec.Model, rec.Turns, costText(rec), elapsedText(rec), waitText(rec),
		firstNonEmpty(rec.Session, "-"), errText, warning)
	if rec.ExitCode != nil {
		return *rec.ExitCode
	}
	return 1
}

func costText(rec history.Record) string {
	if rec.CostUSD == nil {
		return "unknown"
	}
	return fmt.Sprintf("$%.4f", *rec.CostUSD)
}

func elapsedText(rec history.Record) string {
	if rec.ElapsedMs == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.3fs", float64(*rec.ElapsedMs)/1000)
}

// waitText splits elapsed into queue wait and execution when the run was admitted. The -t timeout
// covers only the execution part.
func waitText(rec history.Record) string {
	if rec.WaitMs == nil || rec.ExecMs == nil {
		return ""
	}
	return fmt.Sprintf(" wait=%.3fs exec=%.3fs", float64(*rec.WaitMs)/1000, float64(*rec.ExecMs)/1000)
}

func exitText(rec history.Record) string {
	if rec.ExitCode == nil {
		return "unknown"
	}
	return strconv.Itoa(*rec.ExitCode)
}

// resolveDir picks the worker's cwd. Claude stores sessions per directory, so a resume
// must run where the session started.
func resolveDir(dir, resume, logPath string) (string, error) {
	if dir == "" && resume != "" {
		rs, err := history.Read(logPath)
		if err != nil {
			return "", err
		}
		if hit := history.BySession(rs, resume); len(hit) > 0 {
			dir = hit[0].Dir
		}
	}
	if dir == "" {
		return os.Getwd()
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return "", fmt.Errorf("not a directory: %s", abs)
	}
	return abs, nil
}

func cmdShow(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		return fail(stderr, errors.New("usage: offload show <session>"))
	}
	rs, err := history.Read(history.Path())
	if err != nil {
		return fail(stderr, err)
	}
	hit := history.BySession(rs, args[0])
	if len(hit) == 0 {
		return fail(stderr, fmt.Errorf("no run with session %s", args[0]))
	}
	for _, r := range hit {
		fmt.Fprintf(stdout, "## %s %s %s/%s %s\nexit_code=%s turns=%d estimated_cost=%s elapsed=%s session=%s\n> %s\n\n%s\n\n",
			r.TS, r.Status, r.Profile, r.Model, r.Dir, exitText(r), r.Turns, costText(r), elapsedText(r), r.Session, r.Task, r.Result)
	}
	return 0
}

// cmdProfiles never prints tokens.
func cmdProfiles(stdout, stderr io.Writer) int {
	cfg, err := config.Load(config.Path())
	if err != nil {
		return fail(stderr, err)
	}
	active, _, _ := cfg.Resolve("")
	names := make([]string, 0, len(cfg.Profiles))
	for n := range cfg.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		mark := " "
		if n == active {
			mark = "*"
		}
		p := cfg.Profiles[n]
		fmt.Fprintf(stdout, "%s %-12s %-8s %-24s %s\n", mark, n, p.BackendName(), p.Model, p.BaseURL)
	}
	return 0
}

func cmdDefault(args []string, stdout, stderr io.Writer) int {
	if len(args) > 1 {
		return fail(stderr, errors.New("usage: offload default [profile]"))
	}
	if len(args) == 1 {
		if err := config.SetDefault(config.Path(), args[0]); err != nil {
			return fail(stderr, err)
		}
	}
	cfg, err := config.Load(config.Path())
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintln(stdout, cfg.Default)
	return 0
}

func fail(w io.Writer, err error) int {
	fmt.Fprintln(w, "offload:", err)
	return 2
}

func isTerminal(r io.Reader) bool {
	return terminalStream(r)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
