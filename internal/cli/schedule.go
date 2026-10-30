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
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/schedule"
	"github.com/zhoushoujianwork/offload/internal/text"
)

const scheduleSchemaVersion = 1

const scheduleUsage = `usage:
  offload schedule add (--cron "M H DOM MON DOW" | --every DUR | --at TIME) [--tz ZONE] [flags] "task"
  offload schedule list [--json]
  offload schedule show <task> [--json]
  offload schedule update <task> [schedule or run flags] ["new task"]
  offload schedule pause|resume <task> [--json]
  offload schedule delete <task> [--json]
  offload schedule results <task> [--run ID | --latest] [--limit N] [--json]
  offload schedule start [--interval DUR] [--once]     run the scheduler in the foreground
  offload schedule status [--json]                     is a scheduler running?

<task> is a task id, an exact --name, or an unambiguous id prefix.
Run "offload schedule add -h" for every flag.`

// taskView is a task as printed with --json: the stored fields plus derived ones.
type taskView struct {
	schedule.Task
	State        string `json:"state"`
	NextRunLocal string `json:"next_run_local,omitempty"` // the next run in the task's own timezone
	Description  string `json:"schedule_description"`
}

func viewOf(t *schedule.Task) taskView {
	v := taskView{Task: *t, State: t.State(), Description: t.Schedule.Describe()}
	if t.NextRun != nil {
		if loc, err := schedule.LoadZone(t.Schedule.Timezone); err == nil {
			v.NextRunLocal = t.NextRun.In(loc).Format(time.RFC3339)
		}
	}
	return v
}

type schedulerState struct {
	Running bool `json:"running"`
	PID     int  `json:"pid,omitempty"`
}

func currentScheduler(st *schedule.Store) schedulerState {
	running, pid := st.SchedulerStatus()
	return schedulerState{Running: running, PID: pid}
}

func cmdSchedule(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return fail(stderr, errors.New(scheduleUsage))
	}
	st := schedule.NewStore("")
	switch args[0] {
	case "add":
		return scheduleAdd(st, args[1:], stdin, stdout, stderr)
	case "update":
		return scheduleUpdate(st, args[1:], stdout, stderr)
	case "list", "ls":
		return scheduleList(st, args[1:], stdout, stderr)
	case "show", "inspect":
		return scheduleShow(st, args[1:], stdout, stderr)
	case "pause", "resume", "delete":
		return scheduleLifecycle(st, args[0], args[1:], stdout, stderr)
	case "results":
		return scheduleResults(st, args[1:], stdout, stderr)
	case "start":
		return scheduleStart(st, args[1:], stdout, stderr)
	case "status":
		return scheduleStatus(st, args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprintln(stdout, scheduleUsage)
		return 0
	}
	return fail(stderr, errors.New(scheduleUsage))
}

// parseMixed parses flags that may appear before or after positional arguments. Everything after
// a bare "--" is positional.
func parseMixed(fs *flag.FlagSet, args []string) ([]string, error) {
	var rest []string
	if i := slices.Index(args, "--"); i >= 0 {
		args, rest = args[:i], args[i+1:]
	}
	var pos []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = nil
		if fs.NArg() > 0 {
			pos = append(pos, fs.Arg(0))
			args = fs.Args()[1:]
		}
	}
	return append(pos, rest...), nil
}

// taskFlags are the options shared by add and update.
type taskFlags struct {
	name, cronExpr, every, at, tz, profile, model, dir, policy, timeout, idle, sandbox, misfire string
	allowed, disallowed                                                                         string
	write, noSalvage, toolsDisabled, jsonOut                                                    bool
	keep                                                                                        int
	addDirs                                                                                     dirList
}

func newTaskFlagSet(name string, stderr io.Writer, verb string) (*flag.FlagSet, *taskFlags) {
	f := &taskFlags{}
	fs := flag.NewFlagSet("offload schedule "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&f.name, "name", "", "short unique name, usable instead of the id")
	fs.StringVar(&f.cronExpr, "cron", "", `recurring: five-field cron "minute hour day-of-month month day-of-week" in --tz`)
	fs.StringVar(&f.every, "every", "", "recurring: fixed interval such as 90m or 6h (minimum 1m), counted from now")
	fs.StringVar(&f.at, "at", "", "one time: RFC3339 with offset, or YYYY-MM-DDTHH:MM[:SS] in --tz")
	fs.StringVar(&f.tz, "tz", "", "IANA timezone for cron fields and zone-less --at times (default: the machine's zone)")
	fs.StringVar(&f.profile, "p", "", "profile name or alias (default: the profile default when each run starts)")
	fs.StringVar(&f.model, "m", "", "override the profile's model")
	fs.BoolVar(&f.write, "w", false, "the task may modify files (see offload -h)")
	fs.StringVar(&f.dir, "d", "", "working directory (default: current directory)")
	fs.Var(&f.addDirs, "add-dir", "additional directory for the worker (repeatable)")
	fs.StringVar(&f.policy, "execution-policy", "", "default or managed-development (requires -w)")
	fs.StringVar(&f.timeout, "t", "", "worker timeout per run (default 30m)")
	fs.StringVar(&f.idle, "idle", "", "idle timeout per run")
	fs.StringVar(&f.sandbox, "codex-sandbox", "", "managed Codex sandbox: workspace-write or danger-full-access")
	fs.BoolVar(&f.noSalvage, "no-salvage", false, "skip report salvage for stopped Claude workers")
	fs.BoolVar(&f.toolsDisabled, "tools-disabled", false, "disable all worker tools (Claude only)")
	fs.StringVar(&f.allowed, "allowed-tools", "", "comma-separated Claude tool allowlist")
	fs.StringVar(&f.disallowed, "disallowed-tools", "", "comma-separated Claude tool denylist")
	fs.StringVar(&f.misfire, "misfire", "", `overdue occurrence: "skip" (default; record it as missed) or "run-once" (run once on catch-up)`)
	fs.IntVar(&f.keep, "keep", 0, fmt.Sprintf("results to retain (default %d)", schedule.DefaultKeep))
	fs.BoolVar(&f.jsonOut, "json", false, "print JSON")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: offload schedule %s [flags] %s\n\nflags:\n", name, verb)
		fs.PrintDefaults()
		fmt.Fprint(stderr, scheduleSyntax)
	}
	return fs, f
}

const scheduleSyntax = `
schedule syntax (exactly one of --cron, --every, --at):
  --cron "30 9 * * 1-5"   09:30 on weekdays, evaluated in --tz. Fields accept *, N, A-B, A,B and */N or A-B/N;
                          months and weekdays also accept names (jan, mon). If both day-of-month and
                          day-of-week are restricted, either may match (as in classic cron).
  --every 6h              every 6 hours counted from the moment the schedule was saved
  --at 2026-11-01T09:00   once, in --tz; or --at 2026-11-01T09:00:00+08:00 (the offset wins)
`

func (f *taskFlags) schedulePicked(fs *flag.FlagSet) int {
	n := 0
	fs.Visit(func(x *flag.Flag) {
		switch x.Name {
		case "cron", "every", "at":
			n++
		}
	})
	return n
}

func (f *taskFlags) buildSpec(now time.Time, current *schedule.Spec) (schedule.Spec, error) {
	kind, expr := schedule.Kind(""), ""
	switch {
	case f.cronExpr != "":
		kind, expr = schedule.KindCron, f.cronExpr
	case f.every != "":
		kind, expr = schedule.KindEvery, f.every
	case f.at != "":
		kind, expr = schedule.KindAt, f.at
	default:
		return schedule.Spec{}, errors.New("choose a schedule: --cron, --every or --at")
	}
	tz := f.tz
	if tz == "" && current != nil {
		tz = current.Timezone
	}
	if tz == "" {
		var err error
		if tz, err = schedule.DetectTimezone(); err != nil {
			return schedule.Spec{}, err
		}
	}
	return schedule.NewSpec(kind, expr, tz, now)
}

// validateRun checks the run options without touching a worker, so a bad task fails at creation
// and not at its first scheduled occurrence.
func validateRun(t *schedule.Task) error {
	if strings.TrimSpace(t.Prompt) == "" {
		return errors.New("task prompt required")
	}
	if t.Policy != "default" && t.Policy != "managed-development" {
		return errors.New("invalid execution-policy (want default or managed-development)")
	}
	if t.Policy == "managed-development" && !t.Write {
		return errors.New("managed-development requires -w")
	}
	if t.Misfire != schedule.MisfireSkip && t.Misfire != schedule.MisfireRunOnce {
		return fmt.Errorf("invalid --misfire %q (want skip or run-once)", t.Misfire)
	}
	for _, d := range []string{t.Timeout, t.Idle} {
		if d == "" {
			continue
		}
		if v, err := time.ParseDuration(d); err != nil || v < 0 {
			return fmt.Errorf("invalid duration %q", d)
		}
	}
	if t.Timeout != "" {
		if v, _ := time.ParseDuration(t.Timeout); v <= 0 {
			return errors.New("timeout must be positive")
		}
	}
	if t.Sandbox != "" && (t.Policy != "managed-development" || !t.Write || (t.Sandbox != "workspace-write" && t.Sandbox != "danger-full-access")) {
		return errors.New("codex-sandbox requires managed-development and -w; expected workspace-write or danger-full-access")
	}
	if t.ToolsDisabled && t.AllowedTools != "" {
		return errors.New("tools-disabled conflicts with allowed-tools")
	}
	if t.Keep < 0 {
		return errors.New("keep must not be negative")
	}
	if st, err := os.Stat(t.Dir); err != nil || !st.IsDir() {
		return fmt.Errorf("not a directory: %s", t.Dir)
	}
	if t.Profile != "" {
		cfg, err := config.Load(config.Path())
		if err != nil {
			return err
		}
		name, p, err := cfg.Resolve(t.Profile)
		if err != nil {
			return err
		}
		if t.Sandbox != "" && p.BackendName() != "codex" {
			return fmt.Errorf("codex-sandbox requires a Codex profile; %q is %s", name, p.BackendName())
		}
	}
	return nil
}

func absDir(p string) (string, error) {
	if p == "" {
		return os.Getwd()
	}
	return filepath.Abs(p)
}

func scheduleAdd(st *schedule.Store, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs, f := newTaskFlagSet("add", stderr, `"task"`)
	pos, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	prompt := strings.Join(pos, " ")
	if prompt == "" && !isTerminal(stdin) {
		b, _ := io.ReadAll(io.LimitReader(stdin, maxInstructionBytes))
		prompt = strings.TrimSpace(string(b))
	}
	now := time.Now().UTC().Truncate(time.Second)
	if n := f.schedulePicked(fs); n != 1 {
		return fail(stderr, errors.New("choose exactly one of --cron, --every, --at"))
	}
	spec, err := f.buildSpec(now, nil)
	if err != nil {
		return fail(stderr, err)
	}
	dir, err := absDir(f.dir)
	if err != nil {
		return fail(stderr, err)
	}
	t := &schedule.Task{ID: schedule.NewID(), Name: f.name, Prompt: prompt, Dir: dir, Profile: f.profile, Model: f.model, Write: f.write,
		Policy: firstNonEmpty(f.policy, "default"), Timeout: f.timeout, Idle: f.idle, Sandbox: f.sandbox, NoSalvage: f.noSalvage,
		ToolsDisabled: f.toolsDisabled, AllowedTools: f.allowed, DisallowedTools: f.disallowed, Keep: f.keep,
		Schedule: spec, Misfire: firstNonEmpty(f.misfire, schedule.MisfireSkip), CreatedAt: now, UpdatedAt: now}
	if dirs, err := normalizeAddDirs(f.addDirs, false); err != nil {
		return fail(stderr, err)
	} else {
		t.AddDirs = uniqueDirs(dirs, dir)
	}
	if err := validateRun(t); err != nil {
		return fail(stderr, err)
	}
	if err := validateName(t.Name); err != nil {
		return fail(stderr, err)
	}
	next, _, err := spec.Next(now)
	if err != nil {
		return fail(stderr, err)
	}
	t.NextRun = &next
	unlock, err := st.Lock()
	if err != nil {
		return fail(stderr, err)
	}
	defer unlock()
	if err := ensureNameFree(st, t.Name, ""); err != nil {
		return fail(stderr, err)
	}
	if err := st.Save(t); err != nil {
		return fail(stderr, err)
	}
	return printTask(t, st, f.jsonOut, "created", stdout, stderr)
}

const maxInstructionBytes = 256 << 10

func validateName(name string) error {
	if name == "" {
		return nil
	}
	if strings.HasPrefix(name, "t-") || strings.ContainsAny(name, "/\\ \t\n") || strings.HasPrefix(name, ".") || len(name) > 64 {
		return errors.New("name must be at most 64 characters, without spaces or slashes, and must not start with \"t-\" or \".\"")
	}
	return nil
}

func ensureNameFree(st *schedule.Store, name, selfID string) error {
	if name == "" {
		return nil
	}
	all, _, err := st.List()
	if err != nil {
		return err
	}
	for _, o := range all {
		if o.Name == name && o.ID != selfID {
			return fmt.Errorf("name %q is already used by task %s", name, o.ID)
		}
	}
	return nil
}

func scheduleUpdate(st *schedule.Store, args []string, stdout, stderr io.Writer) int {
	fs, f := newTaskFlagSet("update", stderr, `<task> ["new task"]`)
	pos, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) == 0 {
		return fail(stderr, errors.New("usage: offload schedule update <task> [flags] [\"new task\"]"))
	}
	unlock, err := st.Lock()
	if err != nil {
		return fail(stderr, err)
	}
	defer unlock()
	t, err := st.Find(pos[0])
	if err != nil {
		return fail(stderr, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	set := map[string]bool{}
	fs.Visit(func(x *flag.Flag) { set[x.Name] = true })
	if len(pos) > 1 {
		t.Prompt = strings.Join(pos[1:], " ")
	}
	if set["name"] {
		t.Name = f.name
	}
	if set["p"] {
		t.Profile = f.profile
	}
	if set["m"] {
		t.Model = f.model
	}
	if set["w"] {
		t.Write = f.write
	}
	if set["d"] {
		if t.Dir, err = absDir(f.dir); err != nil {
			return fail(stderr, err)
		}
	}
	if set["add-dir"] {
		dirs, err := normalizeAddDirs(f.addDirs, false)
		if err != nil {
			return fail(stderr, err)
		}
		t.AddDirs = uniqueDirs(dirs, t.Dir)
	}
	if set["execution-policy"] {
		t.Policy = f.policy
	}
	if set["t"] {
		t.Timeout = f.timeout
	}
	if set["idle"] {
		t.Idle = f.idle
	}
	if set["codex-sandbox"] {
		t.Sandbox = f.sandbox
	}
	if set["no-salvage"] {
		t.NoSalvage = f.noSalvage
	}
	if set["tools-disabled"] {
		t.ToolsDisabled = f.toolsDisabled
	}
	if set["allowed-tools"] {
		t.AllowedTools = f.allowed
	}
	if set["disallowed-tools"] {
		t.DisallowedTools = f.disallowed
	}
	if set["misfire"] {
		t.Misfire = f.misfire
	}
	if set["keep"] {
		t.Keep = f.keep
	}
	rescheduled := false
	switch n := f.schedulePicked(fs); {
	case n > 1:
		return fail(stderr, errors.New("choose at most one of --cron, --every, --at"))
	case n == 1:
		spec, err := f.buildSpec(now, &t.Schedule)
		if err != nil {
			return fail(stderr, err)
		}
		t.Schedule, rescheduled = spec, true
	case set["tz"]:
		if t.Schedule.Kind == schedule.KindEvery {
			return fail(stderr, errors.New("--tz does not apply to an --every schedule"))
		}
		if _, err := schedule.LoadZone(f.tz); err != nil {
			return fail(stderr, err)
		}
		// Only the zone changes. A cron expression is re-read in it; a one-time instant stays fixed.
		spec := t.Schedule
		spec.Timezone = f.tz
		t.Schedule, rescheduled = spec, true
	}
	if err := validateRun(t); err != nil {
		return fail(stderr, err)
	}
	if err := validateName(t.Name); err != nil {
		return fail(stderr, err)
	}
	if err := ensureNameFree(st, t.Name, t.ID); err != nil {
		return fail(stderr, err)
	}
	if rescheduled && !t.Paused {
		if err := setNext(t, now); err != nil {
			return fail(stderr, err)
		}
	}
	t.UpdatedAt = now
	if err := st.Save(t); err != nil {
		return fail(stderr, err)
	}
	return printTask(t, st, f.jsonOut, "updated", stdout, stderr)
}

// setNext puts the next occurrence strictly after now into the task.
func setNext(t *schedule.Task, now time.Time) error {
	next, ok, err := t.Schedule.Next(now)
	if err != nil {
		return err
	}
	if ok {
		t.NextRun = &next
	} else {
		t.NextRun = nil
	}
	return nil
}

func printTask(t *schedule.Task, st *schedule.Store, jsonOut bool, verb string, stdout, stderr io.Writer) int {
	if jsonOut {
		return encodeJSON(viewOf(t), stdout, stderr)
	}
	fmt.Fprintf(stdout, "%s task %s\n", verb, t.ID)
	writeTaskDetail(stdout, t)
	if verb != "deleted" {
		if s := currentScheduler(st); !s.Running {
			fmt.Fprintln(stdout, "note: no scheduler is running; start one with: offload schedule start")
		}
	}
	return 0
}

func encodeJSON(v any, stdout, stderr io.Writer) int {
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(stderr, "offload: write JSON:", err)
		return 2
	}
	return 0
}

func nextText(t *schedule.Task) string {
	if t.NextRun == nil {
		switch {
		case t.Paused:
			return "- (paused)"
		case t.Active != nil:
			return "- (running; none remaining)"
		}
		return "- (none remaining)"
	}
	loc, err := schedule.LoadZone(t.Schedule.Timezone)
	if err != nil {
		loc = time.UTC
	}
	return fmt.Sprintf("%s (%s UTC)", t.NextRun.In(loc).Format("2006-01-02 15:04:05 MST"), t.NextRun.UTC().Format("2006-01-02 15:04:05"))
}

func writeTaskDetail(w io.Writer, t *schedule.Task) {
	fmt.Fprintf(w, "id:        %s\n", t.ID)
	if t.Name != "" {
		fmt.Fprintf(w, "name:      %s\n", t.Name)
	}
	fmt.Fprintf(w, "state:     %s\n", t.State())
	fmt.Fprintf(w, "schedule:  %s\n", t.Schedule.Describe())
	fmt.Fprintf(w, "next run:  %s\n", nextText(t))
	fmt.Fprintf(w, "misfire:   %s\n", t.Misfire)
	fmt.Fprintf(w, "dir:       %s\n", t.Dir)
	fmt.Fprintf(w, "profile:   %s\n", firstNonEmpty(t.Profile, "(default when the run starts)"))
	opts := []string{"policy=" + t.Policy}
	if t.Write {
		opts = append(opts, "write")
	}
	for k, v := range map[string]string{"model": t.Model, "timeout": t.Timeout, "idle": t.Idle, "codex-sandbox": t.Sandbox,
		"allowed-tools": t.AllowedTools, "disallowed-tools": t.DisallowedTools} {
		if v != "" {
			opts = append(opts, k+"="+v)
		}
	}
	slices.Sort(opts)
	if t.NoSalvage {
		opts = append(opts, "no-salvage")
	}
	if t.ToolsDisabled {
		opts = append(opts, "tools-disabled")
	}
	if len(t.AddDirs) > 0 {
		opts = append(opts, "add-dirs="+strings.Join(t.AddDirs, ","))
	}
	fmt.Fprintf(w, "options:   %s\n", strings.Join(opts, " "))
	fmt.Fprintf(w, "task:      %s\n", text.OneLine(t.Prompt))
	if t.Active != nil {
		fmt.Fprintf(w, "running:   run %s since %s (scheduled for %s)\n", t.Active.RunID, t.Active.StartedAt.UTC().Format(time.RFC3339), t.Active.ScheduledFor.UTC().Format(time.RFC3339))
	}
	if t.Last != nil {
		fmt.Fprintf(w, "last run:  %s at %s (run %s)", t.Last.Status, t.Last.FinishedAt.UTC().Format(time.RFC3339), t.Last.RunID)
		if t.Last.Reason != "" {
			fmt.Fprintf(w, " - %s", t.Last.Reason)
		}
		if t.Last.Error != "" {
			fmt.Fprintf(w, " - error: %s", text.OneLine(t.Last.Error))
		}
		fmt.Fprintln(w)
	} else if t.Active == nil {
		fmt.Fprintln(w, "last run:  never")
	}
	if t.ConsecutiveFailures > 0 {
		fmt.Fprintf(w, "failures:  %d in a row\n", t.ConsecutiveFailures)
	}
}

type listOutput struct {
	SchemaVersion int            `json:"schema_version"`
	Scheduler     schedulerState `json:"scheduler"`
	Tasks         []taskView     `json:"tasks"`
	Warnings      []string       `json:"warnings"`
}

func scheduleList(st *schedule.Store, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("offload schedule list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	if _, err := parseMixed(fs, args); err != nil {
		return 2
	}
	tasks, warnings, err := st.List()
	if err != nil {
		return fail(stderr, err)
	}
	if *jsonOut {
		out := listOutput{SchemaVersion: scheduleSchemaVersion, Scheduler: currentScheduler(st), Tasks: []taskView{}, Warnings: append([]string{}, warnings...)}
		for _, t := range tasks {
			out.Tasks = append(out.Tasks, viewOf(t))
		}
		return encodeJSON(out, stdout, stderr)
	}
	if len(tasks) == 0 {
		fmt.Fprintln(stdout, "no scheduled tasks")
	}
	for _, t := range tasks {
		last := "never"
		if t.Last != nil {
			last = t.Last.Status
		}
		name := t.ID
		if t.Name != "" {
			name += " (" + t.Name + ")"
		}
		fmt.Fprintf(stdout, "%s | %s | %s | next: %s | last: %s\n  %s\n", name, t.State(), t.Schedule.Describe(), nextText(t), last, text.Truncate(text.OneLine(t.Prompt), 100))
	}
	for _, w := range warnings {
		fmt.Fprintln(stderr, "warning:", w)
	}
	if s := currentScheduler(st); !s.Running && len(tasks) > 0 {
		fmt.Fprintln(stdout, "note: no scheduler is running; due tasks start only while `offload schedule start` runs")
	}
	return 0
}

func scheduleShow(st *schedule.Store, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("offload schedule show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	pos, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		return fail(stderr, errors.New("usage: offload schedule show <task> [--json]"))
	}
	t, err := st.Find(pos[0])
	if err != nil {
		return fail(stderr, err)
	}
	if *jsonOut {
		return encodeJSON(viewOf(t), stdout, stderr)
	}
	writeTaskDetail(stdout, t)
	fmt.Fprintf(stdout, "prompt:\n%s\n", t.Prompt)
	return 0
}

func scheduleLifecycle(st *schedule.Store, verb string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("offload schedule "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	pos, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 {
		return fail(stderr, fmt.Errorf("usage: offload schedule %s <task> [--json]", verb))
	}
	unlock, err := st.Lock()
	if err != nil {
		return fail(stderr, err)
	}
	defer unlock()
	t, err := st.Find(pos[0])
	if err != nil {
		return fail(stderr, err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	switch verb {
	case "pause":
		t.Paused, t.NextRun = true, nil
	case "resume":
		t.Paused = false
		// Time spent paused is never caught up: the next occurrence is the first one after now.
		if err := setNext(t, now); err != nil {
			return fail(stderr, err)
		}
	case "delete":
		if t.Active != nil {
			return fail(stderr, fmt.Errorf("task %s has a run in progress (%s); pause it and delete after the run finishes", t.ID, t.Active.RunID))
		}
		if err := st.Delete(t.ID); err != nil {
			return fail(stderr, err)
		}
		if *jsonOut {
			return encodeJSON(map[string]any{"schema_version": scheduleSchemaVersion, "deleted": t.ID}, stdout, stderr)
		}
		fmt.Fprintf(stdout, "deleted task %s and its retained results\n", t.ID)
		return 0
	}
	t.UpdatedAt = now
	if err := st.Save(t); err != nil {
		return fail(stderr, err)
	}
	return printTask(t, st, *jsonOut, map[string]string{"pause": "paused", "resume": "resumed"}[verb], stdout, stderr)
}

type resultsOutput struct {
	SchemaVersion int                `json:"schema_version"`
	TaskID        string             `json:"task_id"`
	Results       []*schedule.Result `json:"results"`
}

func scheduleResults(st *schedule.Store, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("offload schedule results", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	run := fs.String("run", "", "show one retained result, including the worker's report")
	latest := fs.Bool("latest", false, "show the most recent result with its report")
	limit := fs.Int("limit", 10, "number of results to list")
	pos, err := parseMixed(fs, args)
	if err != nil {
		return 2
	}
	if len(pos) != 1 || *limit < 0 || (*run != "" && *latest) {
		return fail(stderr, errors.New("usage: offload schedule results <task> [--run ID | --latest] [--limit N] [--json]"))
	}
	t, err := st.Find(pos[0])
	if err != nil {
		return fail(stderr, err)
	}
	var rs []*schedule.Result
	switch {
	case *run != "":
		r, err := st.Result(t.ID, *run)
		if err != nil {
			return fail(stderr, err)
		}
		rs = []*schedule.Result{r}
	case *latest:
		if rs, err = st.Results(t.ID, 1); err != nil {
			return fail(stderr, err)
		}
		if len(rs) == 0 {
			return fail(stderr, fmt.Errorf("task %s has no retained results yet", t.ID))
		}
	default:
		if rs, err = st.Results(t.ID, *limit); err != nil {
			return fail(stderr, err)
		}
	}
	if *jsonOut {
		if rs == nil {
			rs = []*schedule.Result{}
		}
		return encodeJSON(resultsOutput{SchemaVersion: scheduleSchemaVersion, TaskID: t.ID, Results: rs}, stdout, stderr)
	}
	detail := *run != "" || *latest
	if len(rs) == 0 {
		fmt.Fprintln(stdout, "no retained results")
	}
	for _, r := range rs {
		exit := "-"
		if r.ExitCode != nil {
			exit = fmt.Sprint(*r.ExitCode)
		}
		fmt.Fprintf(stdout, "%s | %s | scheduled %s | finished %s | exit %s", r.RunID, r.Status, r.ScheduledFor.UTC().Format(time.RFC3339), r.FinishedAt.UTC().Format(time.RFC3339), exit)
		if r.Session != "" {
			fmt.Fprintf(stdout, " | session %s", r.Session)
		}
		fmt.Fprintln(stdout)
		if r.Reason != "" {
			fmt.Fprintf(stdout, "  note: %s\n", r.Reason)
		}
		if r.Error != "" {
			fmt.Fprintf(stdout, "  error: %s\n", text.OneLine(r.Error))
		}
		if detail && r.Report != "" {
			fmt.Fprintf(stdout, "\n%s\n", r.Report)
		}
	}
	return 0
}

func scheduleStatus(st *schedule.Store, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("offload schedule status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	if _, err := parseMixed(fs, args); err != nil {
		return 2
	}
	s := currentScheduler(st)
	if *jsonOut {
		return encodeJSON(map[string]any{"schema_version": scheduleSchemaVersion, "scheduler": s}, stdout, stderr)
	}
	if s.Running {
		fmt.Fprintf(stdout, "scheduler running (pid %d)\n", s.PID)
	} else {
		fmt.Fprintln(stdout, "scheduler not running; tasks are stored but nothing starts until: offload schedule start")
	}
	return 0
}

func scheduleStart(st *schedule.Store, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("offload schedule start", flag.ContinueOnError)
	fs.SetOutput(stderr)
	interval := fs.Duration("interval", 15*time.Second, "how often to check for due tasks (minimum 1s)")
	once := fs.Bool("once", false, "check once, wait for the runs it started, and exit (for an external timer)")
	grace := fs.Duration("misfire-grace", schedule.MisfireGrace, "how late an occurrence may start and still count as on time")
	if pos, err := parseMixed(fs, args); err != nil {
		return 2
	} else if len(pos) != 0 || *interval < time.Second || *grace < 0 {
		return fail(stderr, errors.New("usage: offload schedule start [--interval DUR] [--once] [--misfire-grace DUR]"))
	}
	release, pid, busy, err := st.LockScheduler()
	if err != nil {
		return fail(stderr, err)
	}
	if busy {
		return fail(stderr, fmt.Errorf("a scheduler is already running (pid %d)", pid))
	}
	defer release()
	exe, err := os.Executable()
	if err != nil {
		return fail(stderr, err)
	}
	s := &schedule.Scheduler{Store: st, Exe: exe, Enabled: config.CheckEnabled, Keep: schedule.DefaultKeep, Grace: *grace, Log: stderr}
	if *once {
		if err := s.Tick(); err != nil {
			return fail(stderr, err)
		}
		s.Wait()
		return 0
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(stdout, "offload scheduler started (pid %d); checking every %s; Ctrl-C to stop (runs already started keep going)\n", os.Getpid(), *interval)
	s.Run(ctx, *interval)
	return 0
}
