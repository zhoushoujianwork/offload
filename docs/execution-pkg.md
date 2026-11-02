# Public execution package

Goal: let Agent-Host and other Go applications use Offload as an execution
component without launching the Offload CLI. Import
`github.com/zhoushoujianwork/offload/pkg/execution` and call the same execution
entry point used by the CLI. Native Claude/Codex processes still own their model
and tool loops.

## Ownership

The caller owns tasks, authorization, Agent/project routing, context and Skills,
session compatibility, business acceptance, delivery and durable application
memory. Offload owns native backend invocation, supported tool restrictions,
events, cancellation, timeout and process cleanup. Execution completion never
implies business acceptance or memory publication.

Configuration is explicit: construct a client with a resolved profile, or load a
named profile from an explicit absolute configuration path. A request supplies
the task, working directory, internal instructions and optional native session.
The caller cancels through its context; the package installs no process-wide
signal handlers and does not change cwd or environment.

```go
cfg, err := execution.LoadConfig("/absolute/offload/config.json", "host-profile")
if err != nil { return err }
client, err := execution.New(cfg)
if err != nil { return err }
result, err := client.Run(ctx, execution.Request{
    Caller: "agh", CallerRunID: "agh:task-123",
    RecordHistory: true, // Optional: include this run in Offload Monitor/history.
    Directory: "/absolute/project", Task: "Inspect the retry behavior.",
    Instructions: []byte("Report evidence and any unresolved conditions."),
})
// Inspect err and result independently; persist private evidence under Host policy.
// Also inspect result.HistoryError; a log failure does not undo execution.
```

## First delivery

- Public configuration, request, result and raw native event types, with no
  `internal` types in the exported API.
- Fresh execution and explicit continuation on Claude/Codex, additional
  directories, existing supported tool controls and per-request deadlines.
- Private result evidence and ordered, bounded native output callbacks.
- Optional shared history writes let Monitor retain completed, failed and
  canceled package calls; recording is off by default for existing consumers.
- One execution implementation for the library and existing CLI; preserve CLI
  flags, JSON, history, transcript, scheduling and thread behavior.
- Concurrent independent calls in one process have distinct execution records.
- Independent consumer-module tests use fake native executables, not live models.

The library performs no automatic retry, profile fallback or salvage by default.
The CLI explicitly retains its existing opt-in/default report-salvage behavior.
Callers serialize turns that share a native session and retain the session's
compatible profile, account and workspace; a session ID alone grants no access.

## Boundaries and later work

This delivery makes Offload importable. It does not switch Agent-Host's current
CLI adapter, implement dynamic MCP injection, token deltas, interactive approvals
or automatic session migration. Existing Offload user config/account/state homes
remain process-wide; per-client homes and an application-specific prompt policy
require a separate contract. Native events are backend records, not a promise of
a normalized or complete UI transcript.

[API, compatibility and validation details](execution-pkg-detail.md).
