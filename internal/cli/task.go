package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/live"
	"github.com/zhoushoujianwork/offload/internal/proc"
	"github.com/zhoushoujianwork/offload/internal/runcontrol"
	"github.com/zhoushoujianwork/offload/internal/transcript"
	"github.com/zhoushoujianwork/offload/pkg/execution"
)

type taskDocument struct {
	ResumePending bool   `json:"resume_pending"`
	SchemaVersion int    `json:"schema_version"`
	RunID         string `json:"run_id"`
	Caller        string `json:"caller"`
	CallerRunID   string `json:"caller_run_id,omitempty"`
	Profile       string `json:"profile,omitempty"`
	Backend       string `json:"backend,omitempty"`
	Model         string `json:"model,omitempty"`
	Directory     string `json:"directory,omitempty"`
	Task          string `json:"task,omitempty"`
	Result        string `json:"result,omitempty"`
	Phase         string `json:"phase"`
	Status        string `json:"status,omitempty"`
	Session       string `json:"session,omitempty"`
	Active        bool   `json:"active"`
	CanStop       bool   `json:"can_stop"`
	CanResume     bool   `json:"can_resume"`
	ResumeReason  string `json:"resume_reason"`
	ResumedFrom   string `json:"resumed_from,omitempty"`
	ResumedBy     string `json:"resumed_by,omitempty"`
}

func taskSnapshot(id string) (taskDocument, error) {
	d := taskDocument{SchemaVersion: 1, RunID: id, Caller: "unspecified", Phase: "unavailable", ResumeReason: "This run has no control registration; it may predate task controls."}
	if !transcript.ValidRunID(id) {
		return d, errors.New("invalid Offload run ID")
	}
	m, err := runcontrol.Read(id)
	registered := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return d, err
	}
	if registered {
		d.Caller, d.CallerRunID, d.Profile, d.Backend, d.Model, d.Directory = m.Caller, m.CallerRunID, m.Profile, m.Backend, m.Model, m.Directory
		d.Phase, d.Status, d.Session, d.ResumedFrom, d.ResumedBy = m.Phase, m.Status, m.Session, m.ResumedFrom, m.ResumedBy
		d.ResumePending = m.ResumePending()
		d.Active = m.Active()
		d.CanStop = d.Active && m.Phase != "stopping"
	}
	states, _ := live.List()
	for _, s := range states {
		if s.RunID == id {
			d.Task = boundTaskText(s.Task, 16<<10)
			d.Caller = s.Caller
			d.Model = s.Model
			d.Directory = s.Dir
			if d.Active && m.Phase != "stopping" {
				d.Phase = s.Phase
			}
			break
		}
	}
	if d.Active {
		d.ResumeReason = "Wait until the execution and cleanup have ended."
		return d, nil
	}
	r, found, err := history.FindRun(id)
	if err != nil {
		return d, err
	}
	if !found {
		d.ResumeReason = "The terminal history record is not available yet."
		return d, nil
	}
	d.Task, d.Result, d.Session, d.Status = boundTaskText(r.Task, 16<<10), boundTaskText(r.Result, 128<<10), r.Session, r.Status
	d.Profile, d.Backend, d.Model, d.Directory, d.Caller = r.Profile, r.Backend, r.Model, r.Dir, history.CallerOrUnspecified(r.Caller)
	d.CallerRunID, d.ResumedFrom = r.CallerRunID, r.ResumedFrom
	d.Phase = "done"
	if r.ErrorKind == "canceled" || r.StopReason == "canceled" {
		d.Status = "canceled"
	}
	switch {
	case !registered:
	case m.Phase != "done":
		d.ResumeReason = "Execution control did not confirm cleanup. Resume is unavailable."
	case d.ResumePending:
		d.ResumeReason = "A resume process is starting; wait for its registration."
	case m.ResumedBy != "":
		d.ResumeReason = "This run has already been resumed. Open the linked execution."
	case r.Status == "ok":
		d.ResumeReason = "This task completed successfully."
	case r.Session == "":
		d.ResumeReason = "No native session was established before execution stopped."
	case r.Cleanup != "":
		d.ResumeReason = "Native cleanup remains unresolved."
	case len(m.ResumeCommand) == 0 && (!m.CLIResume || !r.HasResumeSettings()):
		d.ResumeReason = "The caller did not provide a supported resume entry point."
	default:
		d.CanResume = true
		d.ResumeReason = "Resume creates a new execution in the existing compatible native session."
	}
	return d, nil
}

func cmdTask(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return fail(stderr, errors.New("usage: offload task show|stop|resume --id RUN_ID --json"))
	}
	fs := flag.NewFlagSet("task "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	id := fs.String("id", "", "Offload run ID")
	asJSON := fs.Bool("json", false, "")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if !*asJSON || fs.NArg() != 0 || !transcript.ValidRunID(*id) {
		return fail(stderr, errors.New("task controls require --id RUN_ID --json"))
	}
	switch args[0] {
	case "show":
		d, err := taskSnapshot(*id)
		if err != nil {
			return fail(stderr, err)
		}
		return writeTaskJSON(stdout, stderr, d)
	case "stop":
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if err := runcontrol.Stop(ctx, *id); err != nil {
			return fail(stderr, err)
		}
		d, err := taskSnapshot(*id)
		if err != nil {
			return fail(stderr, err)
		}
		return writeTaskJSON(stdout, stderr, d)
	case "resume":
		return resumeTask(*id, stdout, stderr)
	default:
		return fail(stderr, errors.New("unknown task control action"))
	}
}

func writeTaskJSON(stdout, stderr io.Writer, value any) int {
	if err := json.NewEncoder(stdout).Encode(value); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func resumeTask(id string, stdout, stderr io.Writer) int {
	release, err := runcontrol.Lease(id)
	if err != nil {
		return fail(stderr, errors.New("another resume request owns this run"))
	}
	defer release()
	d, err := taskSnapshot(id)
	if err != nil {
		return fail(stderr, err)
	}
	if !d.CanResume {
		return fail(stderr, errors.New(d.ResumeReason))
	}
	m, err := runcontrol.Read(id)
	if err != nil {
		return fail(stderr, err)
	}
	commandArgs := append([]string(nil), m.ResumeCommand...)
	directory := m.ResumeDirectory
	if len(commandArgs) == 0 {
		cfg, err := execution.LoadConfig(config.Path(), m.Profile)
		if err != nil {
			return fail(stderr, err)
		}
		if execution.ProfileFingerprint(cfg) != m.ProfileFingerprint {
			return fail(stderr, errors.New("original profile configuration changed; restore it before resuming"))
		}
		binary, err := os.Executable()
		if err != nil {
			return fail(stderr, err)
		}
		commandArgs = []string{binary, "resume", "--id", id, "--json", "--caller", "monitor"}
		directory = m.Directory
	} else if err := runcontrol.Validate(m.Info); err != nil {
		return fail(stderr, err)
	}
	command := exec.Command(commandArgs[0], commandArgs[1:]...)
	command.Dir = directory
	env := []string{}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "OFFLOAD_MONITOR_RESUMED_FROM=") && !strings.HasPrefix(kv, "OFFLOAD_MONITOR_RESUME_PARENT_PID=") {
			env = append(env, kv)
		}
	}
	command.Env = append(env, "OFFLOAD_MONITOR_RESUMED_FROM="+id, "OFFLOAD_MONITOR_RESUME_PARENT_PID="+strconv.Itoa(os.Getpid()))
	sink, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fail(stderr, err)
	}
	defer sink.Close()
	command.Stdout, command.Stderr = sink, sink
	proc.Prepare(command)
	if err := command.Start(); err != nil {
		return fail(stderr, errors.New("could not launch the saved resume entry point"))
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	if err := runcontrol.MarkResuming(id, command.Process.Pid); err != nil {
		release()
		return fail(stderr, err)
	}
	release()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		source, err := runcontrol.Read(id)
		if err != nil {
			return fail(stderr, err)
		}
		if source.ResumedBy != "" {
			next, err := taskSnapshot(source.ResumedBy)
			if err != nil {
				return fail(stderr, err)
			}
			return writeTaskJSON(stdout, stderr, next)
		}
		select {
		case <-exited:
			source, _ = runcontrol.Read(id)
			if source.ResumedBy == "" {
				return fail(stderr, errors.New("resume process ended before registering an execution; inspect caller configuration and state"))
			}
		case <-deadline.C:
			pending, err := taskSnapshot(id)
			if err != nil {
				return fail(stderr, err)
			}
			return writeTaskJSON(stdout, stderr, pending)
		case <-tick.C:
		}
	}
}

func boundTaskText(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return strings.ToValidUTF8(value[:max], "") + "\n[truncated; read the transcript for bounded pages]"
}
