// Package execution runs Claude Code or Codex directly through Offload's shared
// worker. The caller owns task policy, session serialization, delivery and memory.
// Run installs no signal handlers and never changes the process environment/cwd.
package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/worker"
)

// Profile explicitly selects the backend and its native configuration. Token
// fields are credentials; do not log or serialize the configuration.
type Profile struct {
	Backend, BaseURL, Model, ReasoningEffort, ServiceTier string
	CodexAccount, IdleTimeout, Endpoint                   string
	TokenCmd                                              string `json:"-"`
	Token                                                 string `json:"-"`
}

// Limits sets shared admission and child build defaults, not OS containment.
// Zero uses Offload defaults; -1 disables the corresponding limit. Existing
// OFFLOAD_MAX_WORKERS, GOMAXPROCS and GOFLAGS retain their usual precedence.
type Limits struct{ MaxWorkers, GoMaxProcs, GoParallelism int }

type Config struct {
	ProfileName string
	Profile     Profile
	Limits      Limits
}

// Capture is an optional caller-owned transcript sink. Methods must be safe for
// the same usage as Offload's recorder and return promptly. The caller owns its
// creation and finalization. Raw output is sensitive, not sanitized telemetry.
type Capture interface {
	AddSecrets(...string)
	BeginAttempt(kind, task string)
	Line([]byte)
	Skipped(reason string, bytes int)
}

// RawEvent contains one bounded complete native output line. Backend formats
// differ; no token deltas or normalized tool schema are implied. Data is copied
// and may be retained by the observer. Do not forward it to public logs.
type RawEvent struct {
	RunID, CallerRunID, Backend string
	Data                        []byte
}

type Request struct {
	RunID, CallerRunID, Caller string
	Task, Directory, Model     string
	NativeSessionID            string
	// CodexHome pins the account home recorded by the prior native invocation.
	CodexHome             string
	AdditionalDirectories []string
	Instructions          []byte
	InstructionsFile      string
	Write                 bool // Intent, never permission or filesystem containment.
	ExecutionPolicy       string
	CodexSandbox          string
	ToolsDisabled         bool
	AllowedTools          string
	DisallowedTools       string
	OnUnsupportedTools    string
	Timeout               *time.Duration // Nil uses 30m; zero/negative expires immediately, like CLI -t.
	IdleTimeout           *time.Duration
	StartedAt             time.Time
	AllowSalvage          bool // Explicit CLI compatibility; defaults to no additional model invocation.
	OnEvent               func(RawEvent) error
	Capture               Capture
}

// Result is execution evidence, never task acceptance. Missing usage/cost stays
// nil. RawRecord preserves the complete private CLI/history-compatible record.
type Result struct {
	RunID, CallerRunID, Profile, Backend, Model string
	NativeSessionID, Status, ErrorKind          string
	Report, Error, Cleanup                      string
	ExitCode                                    int
	Usage                                       json.RawMessage
	CostUSD                                     *float64
	ElapsedMS                                   *int64
	RawRecord                                   json.RawMessage
}

// RunError distinguishes execution failure from a successful report. Cause
// retains context cancellation/deadline and observer errors when available.
type RunError struct {
	Kind, Status string
	ExitCode     int
	Cause        error
}

func (e *RunError) Error() string {
	return fmt.Sprintf("offload execution: %s (status=%s, exit=%d)", e.Kind, e.Status, e.ExitCode)
}
func (e *RunError) Unwrap() error { return e.Cause }

// Client snapshots a validated profile and is safe for concurrent independent
// runs. Callers must serialize turns of the same native session. Standard
// Offload account/state homes remain process-wide; the client does not own them.
type Client struct {
	cfg     Config
	profile config.Profile
	limits  config.Limits
}

var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
var activeRuns sync.Map

func internalProfile(p Profile) config.Profile {
	return config.Profile{Backend: p.Backend, BaseURL: p.BaseURL, Model: p.Model,
		ReasoningEffort: p.ReasoningEffort, ServiceTier: p.ServiceTier, CodexAccount: p.CodexAccount,
		IdleTimeout: p.IdleTimeout, Endpoint: p.Endpoint, TokenCmd: p.TokenCmd, Token: p.Token}
}

// New validates explicit configuration; it starts no process and loads no
// credentials. Use LoadConfig to opt into reading a named installed profile.
func New(c Config) (*Client, error) {
	if strings.TrimSpace(c.ProfileName) == "" {
		return nil, errors.New("execution requires an explicit profile name")
	}
	profiles := config.Config{Profiles: map[string]config.Profile{c.ProfileName: internalProfile(c.Profile)}}
	_, p, err := profiles.Resolve(c.ProfileName)
	if err != nil {
		return nil, err
	}
	limits := config.Limits{MaxWorkers: c.Limits.MaxWorkers, GoMaxProcs: c.Limits.GoMaxProcs, GoParallelism: c.Limits.GoParallelism}
	return &Client{cfg: c, profile: p, limits: limits}, nil
}

// LoadConfig reads exactly the specified absolute config path and resolves an
// explicit profile/alias. It neither writes defaults nor uses OFFLOAD_PROFILE.
func LoadConfig(path, name string) (Config, error) {
	if !filepath.IsAbs(path) || strings.TrimSpace(name) == "" {
		return Config{}, errors.New("LoadConfig requires an absolute path and explicit profile name")
	}
	c, err := config.Load(path)
	if err != nil {
		return Config{}, err
	}
	canonical, p, err := c.Resolve(name)
	if err != nil {
		return Config{}, err
	}
	return Config{ProfileName: canonical, Profile: Profile{Backend: p.Backend, BaseURL: p.BaseURL,
		Model: p.Model, ReasoningEffort: p.ReasoningEffort, ServiceTier: p.ServiceTier,
		CodexAccount: p.CodexAccount, IdleTimeout: p.IdleTimeout, Endpoint: p.Endpoint, TokenCmd: p.TokenCmd, Token: p.Token},
		Limits: Limits{MaxWorkers: c.Limits.MaxWorkers, GoMaxProcs: c.Limits.GoMaxProcs, GoParallelism: c.Limits.GoParallelism}}, nil
}

// Run owns one native invocation until cleanup completes. No Offload CLI is
// launched and no CLI history record is appended. Raw native events are ordered;
// observer errors stop this invocation. There are no retries/profile fallbacks.
func (c *Client) Run(ctx context.Context, req Request) (Result, error) {
	started := req.StartedAt
	if started.IsZero() {
		started = time.Now()
	}
	id := req.RunID
	if id == "" {
		id = worker.NewRunID()
	}
	setupError := func(kind string, cause error) (Result, error) {
		elapsed := time.Since(started).Milliseconds()
		r := Result{RunID: id, CallerRunID: req.CallerRunID, Profile: c.cfg.ProfileName,
			Backend: c.profile.BackendName(), Model: c.profile.Model, Status: "error",
			ErrorKind: kind, ExitCode: 2, Error: cause.Error(), ElapsedMS: &elapsed}
		return r, &RunError{Kind: kind, Status: r.Status, ExitCode: r.ExitCode, Cause: cause}
	}
	if !runIDPattern.MatchString(id) {
		return setupError("configuration", errors.New("run ID must contain 1-128 safe correlation characters"))
	}
	if _, busy := activeRuns.LoadOrStore(id, struct{}{}); busy {
		return setupError("configuration", errors.New("run ID is already active in this process"))
	}
	defer activeRuns.Delete(id)
	if err := ctx.Err(); err != nil {
		kind := "canceled"
		if errors.Is(err, context.DeadlineExceeded) {
			kind = "timeout"
		}
		return setupError(kind, errors.Join(ctx.Err(), context.Cause(ctx)))
	}
	if strings.TrimSpace(req.Task) == "" || !filepath.IsAbs(req.Directory) {
		return setupError("configuration", errors.New("execution requires a task and absolute working directory"))
	}
	if info, err := os.Stat(req.Directory); err != nil || !info.IsDir() {
		return setupError("configuration", errors.New("working directory must exist"))
	}
	if len(req.Instructions) > 256<<10 || len(req.Instructions) > 0 && req.InstructionsFile != "" {
		return setupError("configuration", errors.New("instructions must fit 256KiB and cannot combine bytes with a file"))
	}
	timeout := 30 * time.Minute
	if req.Timeout != nil {
		timeout = *req.Timeout
	}
	maxWorkers, err := c.limits.EffectiveMaxWorkers()
	if err != nil {
		return setupError("configuration", err)
	}
	o := worker.Opts{ProfileName: c.cfg.ProfileName, Profile: c.profile, Model: req.Model,
		CodexHome: req.CodexHome, RunID: id, CallerRunID: req.CallerRunID, Caller: req.Caller,
		Task: req.Task, Dir: req.Directory, AddDirs: slices.Clone(req.AdditionalDirectories), Resume: req.NativeSessionID,
		Instructions: bytes.Clone(req.Instructions), InstructionsFile: req.InstructionsFile,
		Write: req.Write, ExecutionPolicy: req.ExecutionPolicy, CodexSandbox: req.CodexSandbox,
		ToolsDisabled: req.ToolsDisabled, AllowedTools: req.AllowedTools, DisallowedTools: req.DisallowedTools,
		OnUnsupportedTools: req.OnUnsupportedTools, Timeout: timeout, StartedAt: started,
		NoSalvage: !req.AllowSalvage, MaxWorkers: maxWorkers, Limits: c.limits, Transcript: req.Capture}
	if o.Model == "" {
		o.Model = c.profile.Model
	}
	if req.IdleTimeout != nil {
		idle := *req.IdleTimeout
		o.IdleTimeout = &idle
	}
	for _, dir := range o.AddDirs {
		if !filepath.IsAbs(dir) {
			return setupError("configuration", errors.New("additional directories must be absolute"))
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return setupError("configuration", errors.New("additional directory must exist"))
		}
	}
	var observerErr error
	if req.OnEvent != nil {
		o.OnLine = func(line []byte) error {
			err := req.OnEvent(RawEvent{RunID: id, CallerRunID: req.CallerRunID, Backend: c.profile.BackendName(), Data: bytes.Clone(line)})
			observerErr = errors.Join(observerErr, err)
			return err
		}
	}
	record, runErr := worker.Run(ctx, o)
	raw, encodeErr := json.Marshal(record)
	usage, _ := json.Marshal(record.Usage)
	if record.Usage == nil {
		usage = nil
	}
	exit := -1
	if record.ExitCode != nil {
		exit = *record.ExitCode
	}
	result := Result{RunID: record.RunID, CallerRunID: record.CallerRunID, Profile: record.Profile,
		Backend: record.Backend, Model: record.Model, NativeSessionID: record.Session,
		Status: record.Status, ErrorKind: record.ErrorKind, ExitCode: exit, Report: record.Result,
		Error: record.Error, Cleanup: record.Cleanup, Usage: usage, CostUSD: record.CostUSD,
		ElapsedMS: record.ElapsedMs, RawRecord: raw}
	if record.Status == "ok" && runErr == nil && encodeErr == nil && observerErr == nil && record.Cleanup == "" {
		return result, nil
	}
	if record.Status != "ok" && ctx.Err() != nil {
		runErr = errors.Join(runErr, ctx.Err(), context.Cause(ctx))
	}
	if record.ErrorKind == "timeout" {
		runErr = errors.Join(runErr, context.DeadlineExceeded)
	}
	if record.Cleanup != "" {
		result.ErrorKind = "cleanup"
		runErr = errors.Join(runErr, errors.New("native process cleanup remains unresolved"))
	}
	cause := errors.Join(runErr, encodeErr, observerErr)
	if cause == nil {
		cause = errors.New("native execution did not complete successfully")
	}
	return result, &RunError{Kind: result.ErrorKind, Status: result.Status, ExitCode: exit, Cause: cause}
}
