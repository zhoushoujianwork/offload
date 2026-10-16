// offload runs a task on a cheaper model through `claude -p` against an Anthropic-compatible
// gateway and prints only the worker's final report, so a supervising agent spends few tokens.
package main

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
	"time"
)

var version = "dev"

const usage = `offload — hand a task to a cheaper model via claude -p; print only its final report.

usage:
  offload [flags] "task"        run a task (task may also come on stdin)
  offload ls [n]                list the last n runs (default 10)
  offload show <session>        print every report of a session
  offload profiles              list profiles (* = the one a run would use)
  offload install-agent [flags] install proactive instructions for a host agent
  offload version

flags:
`

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func realMain(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case "ls":
			return cmdList(args[1:], stdout, stderr)
		case "show":
			return cmdShow(args[1:], stdout, stderr)
		case "profiles":
			return cmdProfiles(stdout, stderr)
		case "install-agent":
			return cmdInstallAgent(args[1:], stdout, stderr)
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
	timeout := fs.Duration("t", 15*time.Minute, "timeout")
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

	logPath := historyPath()
	rec := Record{TS: started.Format("2006-01-02 15:04:05"), Status: "error", Profile: *profile,
		Model: *model, Dir: *dir, Task: task, CostIsEstimate: true}
	failRun := func(err error) int {
		code := 2
		rec.ExitCode, rec.Error, rec.Result = &code, err.Error(), err.Error()
		elapsed := time.Since(started).Milliseconds()
		rec.ElapsedMs = &elapsed
		return printRun(rec, logPath, *jsonOutput, stdout, stderr)
	}
	cfg, err := loadConfig(configPath())
	if err != nil {
		return failRun(err)
	}
	name, prof, err := cfg.profile(*profile)
	rec.Profile, rec.Model = name, firstNonEmpty(*model, prof.Model)
	if err != nil {
		return failRun(err)
	}
	wd, err := resolveDir(*dir, *resume, logPath)
	if err != nil {
		return failRun(err)
	}
	o := RunOpts{ProfileName: name, Profile: prof, Model: firstNonEmpty(*model, prof.Model), Write: *write,
		Resume: *resume, Dir: wd, Task: task, Timeout: *timeout, StartedAt: started}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	rec, _ = run(ctx, o)
	return printRun(rec, logPath, *jsonOutput, stdout, stderr)
}

func printRun(rec Record, logPath string, jsonOutput bool, stdout, stderr io.Writer) int {
	warning := ""
	if err := appendRecord(logPath, rec); err != nil {
		warning = " warning=" + oneLine("run log not written: "+err.Error())
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
		errText = " error=" + oneLine(rec.Error)
	}
	if rec.Denials > 0 {
		errText = fmt.Sprintf(" permission_denials=%d", rec.Denials) + errText
	}
	fmt.Fprintf(stderr, "[offload] %s exit_code=%s %s/%s turns=%d estimated_cost=%s elapsed=%s session=%s%s%s\n",
		rec.Status, exitText(rec), rec.Profile, rec.Model, rec.Turns, costText(rec), elapsedText(rec),
		firstNonEmpty(rec.Session, "-"), errText, warning)
	if rec.ExitCode != nil {
		return *rec.ExitCode
	}
	return 1
}

func costText(rec Record) string {
	if rec.CostUSD == nil {
		return "unknown"
	}
	return fmt.Sprintf("$%.4f", *rec.CostUSD)
}

func elapsedText(rec Record) string {
	if rec.ElapsedMs == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.3fs", float64(*rec.ElapsedMs)/1000)
}

func exitText(rec Record) string {
	if rec.ExitCode == nil {
		return "unknown"
	}
	return strconv.Itoa(*rec.ExitCode)
}

// resolveDir picks the worker's cwd. Claude stores sessions per directory, so a resume
// must run where the session started.
func resolveDir(dir, resume, logPath string) (string, error) {
	if dir == "" && resume != "" {
		rs, err := readRecords(logPath)
		if err != nil {
			return "", err
		}
		if hit := bySession(rs, resume); len(hit) > 0 {
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

func cmdList(args []string, stdout, stderr io.Writer) int {
	n := 10
	if len(args) > 0 {
		v, err := strconv.Atoi(args[0])
		if err != nil || v < 1 {
			return fail(stderr, fmt.Errorf("ls: bad count %q", args[0]))
		}
		n = v
	}
	rs, err := readRecords(historyPath())
	if err != nil {
		return fail(stderr, err)
	}
	if len(rs) > n {
		rs = rs[len(rs)-n:]
	}
	for _, r := range rs {
		fmt.Fprintf(stdout, "%s  %-7s exit_code=%s %s/%s turns=%d estimated_cost=%s elapsed=%s session=%s %s | %s\n",
			r.TS, r.Status, exitText(r), r.Profile, r.Model, r.Turns, costText(r), elapsedText(r),
			firstNonEmpty(r.Session, "-"), r.Dir, truncate(oneLine(r.Task), 60))
	}
	return 0
}

func cmdShow(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		return fail(stderr, errors.New("usage: offload show <session>"))
	}
	rs, err := readRecords(historyPath())
	if err != nil {
		return fail(stderr, err)
	}
	hit := bySession(rs, args[0])
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
	cfg, err := loadConfig(configPath())
	if err != nil {
		return fail(stderr, err)
	}
	active, _, _ := cfg.profile("")
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
		fmt.Fprintf(stdout, "%s %-12s %-24s %s\n", mark, n, p.Model, p.BaseURL)
	}
	return 0
}

func fail(w io.Writer, err error) int {
	fmt.Fprintln(w, "offload:", err)
	return 2
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
