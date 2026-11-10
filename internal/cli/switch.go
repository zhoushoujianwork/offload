package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"time"

	"github.com/zhoushoujianwork/offload/internal/agentinstall"
	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/live"
)

const switchUsage = "usage: offload status [--json] | offload enable [--json] | offload disable [--json]"

// New-execution states in the switch document. They are part of the JSON contract.
const (
	newExecutionsAllowed = "allowed"
	newExecutionsBlocked = "blocked"
	// runningNotStopped is the handling of work that was already running when the switch was turned off.
	runningNotStopped = "not_stopped"
)

// switchRunning counts the invocations the local state directory shows. Counts are null when the live
// state could not be read; null is not zero. Active is processing + queued + cleaning + unresolved.
type switchRunning struct {
	Status     string `json:"status"` // ok | partial (lower bounds) | unavailable
	Processing *int   `json:"processing"`
	Queued     *int   `json:"queued"`
	Cleaning   *int   `json:"cleaning"`
	Unresolved *int   `json:"unresolved"`
	Lost       *int   `json:"lost"`
	Active     *int   `json:"active"`
	// Handling is set only while the switch is off: running work is not stopped, and queued work is
	// refused before it starts.
	Handling string `json:"handling,omitempty"`
}

type switchInstructions struct {
	Updated bool `json:"updated"`
	// NewSessionRequired is always true: an agent session that is already open keeps the instructions it
	// loaded. The CLI refuses executions on its own, so an old session is still stopped.
	NewSessionRequired bool   `json:"new_session_required"`
	Error              string `json:"error,omitempty"`
}

// switchDocument is the JSON document of `offload status|enable|disable --json`.
type switchDocument struct {
	SchemaVersion int  `json:"schema_version"`
	Enabled       bool `json:"enabled"`
	// NewExecutions is allowed or blocked. Blocked covers new tasks, resumes, queued tasks, scheduled
	// tasks and profile checks, everything that starts a worker or makes a model call.
	NewExecutions string              `json:"new_executions"`
	ErrorKind     string              `json:"error_kind,omitempty"`
	Reason        string              `json:"reason,omitempty"`
	ConfigPath    string              `json:"config_path"`
	Running       switchRunning       `json:"running"`
	Instructions  *switchInstructions `json:"instructions,omitempty"`
}

func buildSwitchDocument(now time.Time) (switchDocument, error) {
	disabled, err := config.LoadDisabled(config.Path())
	if err != nil {
		return switchDocument{}, err
	}
	d := switchDocument{SchemaVersion: 1, Enabled: !disabled, NewExecutions: newExecutionsAllowed, ConfigPath: config.Path()}
	if disabled {
		d.NewExecutions, d.ErrorKind, d.Reason = newExecutionsBlocked, config.DisabledErrorKind, config.ErrDisabled.Error()
	}
	states, unreadable, listErr := live.ListChecked()
	w := classifyWorkload(states, unreadable, listErr, now)
	d.Running = switchRunning{Status: w.Status, Processing: w.Processing, Queued: w.Queued, Cleaning: w.Cleaning,
		Unresolved: w.Unresolved, Lost: w.Lost}
	if w.Status != workloadUnavailable {
		active := *w.Processing + *w.Queued + *w.Cleaning + *w.Unresolved
		d.Running.Active = &active
	}
	if disabled {
		d.Running.Handling = runningNotStopped
	}
	return d, nil
}

func writeSwitchJSON(stdout, stderr io.Writer, d switchDocument) int {
	if err := json.NewEncoder(stdout).Encode(d); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// parseJSONFlag accepts only an optional --json.
func parseJSONFlag(name string, args []string, stderr io.Writer) (asJSON, ok bool) {
	fs := flag.NewFlagSet("offload "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	j := fs.Bool("json", false, "print the machine-readable switch state")
	if err := fs.Parse(args); err != nil {
		return false, false
	}
	if fs.NArg() != 0 {
		fail(stderr, errors.New(switchUsage))
		return false, false
	}
	return *j, true
}

// cmdSwitch implements `offload disable|enable [--json]`. The text form is unchanged. The JSON form keeps
// stdout to one document: the instruction update report is folded into it. The exit code is 2 when the switch
// could not be saved (nothing changed) and also when it was saved but the agent instructions could not be fully
// updated (document printed, instructions.error set).
func cmdSwitch(disabled bool, args []string, stdout, stderr io.Writer) int {
	name := "enable"
	if disabled {
		name = "disable"
	}
	asJSON, ok := parseJSONFlag(name, args, stderr)
	if !ok {
		return 2
	}
	if !asJSON {
		return agentinstall.RunSwitch([]string{name}, stdout, stderr)
	}
	var report bytes.Buffer
	saved, code := agentinstall.ApplySwitch(disabled, &report, stderr, nil)
	if !saved {
		return code
	}
	d, err := buildSwitchDocument(time.Now())
	if err != nil {
		return fail(stderr, err)
	}
	d.Instructions = &switchInstructions{Updated: code == 0, NewSessionRequired: true}
	if code != 0 {
		d.Instructions.Error = "agent instructions could not be fully updated; fix the reported conflict and retry install-agent"
	}
	if rc := writeSwitchJSON(stdout, stderr, d); rc != 0 {
		return rc
	}
	return code
}

// cmdStatusArgs implements `offload status [--json]`.
func cmdStatusArgs(args []string, stdout, stderr io.Writer) int {
	asJSON, ok := parseJSONFlag("status", args, stderr)
	if !ok {
		return 2
	}
	if !asJSON {
		return cmdStatus(stdout, stderr)
	}
	d, err := buildSwitchDocument(time.Now())
	if err != nil {
		return fail(stderr, err)
	}
	return writeSwitchJSON(stdout, stderr, d)
}
