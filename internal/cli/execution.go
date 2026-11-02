package cli

import (
	"context"
	"encoding/json"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/worker"
	"github.com/zhoushoujianwork/offload/pkg/execution"
)

// runExecution preserves CLI record ownership while using the same public
// execution entry point as Go consumers. Only the compatibility record is
// decoded; no native work or model/tool loop is repeated.
func runExecution(ctx context.Context, o worker.Opts) history.Record {
	p := o.Profile
	limit := o.MaxWorkers
	if limit <= 0 {
		limit = -1
	}
	client, err := execution.New(execution.Config{ProfileName: o.ProfileName,
		Profile: execution.Profile{Backend: p.Backend, BaseURL: p.BaseURL, Model: p.Model,
			ReasoningEffort: p.ReasoningEffort, ServiceTier: p.ServiceTier, CodexAccount: p.CodexAccount,
			IdleTimeout: p.IdleTimeout, Endpoint: p.Endpoint, TokenCmd: p.TokenCmd, Token: p.Token},
		Limits: execution.Limits{MaxWorkers: limit, GoMaxProcs: o.Limits.GoMaxProcs, GoParallelism: o.Limits.GoParallelism}})
	var result execution.Result
	if err == nil {
		req := execution.Request{RunID: o.RunID, CallerRunID: o.CallerRunID, Caller: o.Caller,
			Task: o.Task, Directory: o.Dir, Model: o.Model, NativeSessionID: o.Resume, CodexHome: o.CodexHome,
			AdditionalDirectories: o.AddDirs, Instructions: o.Instructions, InstructionsFile: o.InstructionsFile,
			Write: o.Write, ExecutionPolicy: o.ExecutionPolicy, CodexSandbox: o.CodexSandbox,
			ToolsDisabled: o.ToolsDisabled, AllowedTools: o.AllowedTools, DisallowedTools: o.DisallowedTools,
			OnUnsupportedTools: o.OnUnsupportedTools, Timeout: &o.Timeout, IdleTimeout: o.IdleTimeout,
			StartedAt: o.StartedAt, AllowSalvage: !o.NoSalvage, Capture: o.Transcript}
		if o.OnLine != nil {
			req.OnEvent = func(e execution.RawEvent) error { return o.OnLine(e.Data) }
		}
		result, err = client.Run(ctx, req)
		if len(result.RawRecord) > 0 {
			var rec history.Record
			if decodeErr := json.Unmarshal(result.RawRecord, &rec); decodeErr == nil {
				return rec
			} else {
				err = decodeErr
			}
		}
	}
	code, elapsed := 2, time.Since(o.StartedAt).Milliseconds()
	kind := result.ErrorKind
	if kind == "" {
		kind = "configuration"
	}
	return history.Record{RunID: o.RunID, CallerRunID: o.CallerRunID, Caller: history.CallerOrUnspecified(o.Caller),
		TS: o.StartedAt.Format("2006-01-02 15:04:05"), Status: "error", ExitCode: &code,
		Profile: o.ProfileName, Model: o.Model, Backend: p.BackendName(), Task: o.Task, Dir: o.Dir,
		ExecutionPolicy: o.ExecutionPolicy, ErrorKind: kind, Error: err.Error(), Result: err.Error(), ElapsedMs: &elapsed, CostIsEstimate: true}
}
