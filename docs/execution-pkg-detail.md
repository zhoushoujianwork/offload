# Execution package implementation

[Main goals and scope](execution-pkg.md).

## Public contract

`pkg/execution` exposes `Config`, `Profile`, `Limits`, `Request`, `Result`,
`RawEvent`, `Capture`, `RunError`, `LoadConfig`, `New` and `Client.Run`.
No exported signature requires importing an `internal` package. `New` validates
and snapshots the explicit profile without starting a native process.
`LoadConfig` reads the given absolute config file and resolves the named profile;
it never saves config or changes global defaults. Requests use an existing
absolute directory; native continuation is explicit, with no automatic lookup
or retry on a missing native session.

`Run` calls the existing internal worker in process. It inherits native binary
selection, account configuration, global enablement, shared admission slots and
the standard Offload state homes. These are process settings: callers must not
change environment or cwd concurrently with execution. The package itself never
sets them. Calls may run concurrently for distinct native conversations. Host
must serialize each continued session and check its profile/account/workspace
binding. Client cancellation affects only that call's owned native group.

`Timeout` is optional: nil selects the CLI's 30-minute native execution limit;
an explicit zero/negative duration expires immediately. Queue/setup and native
execution keep their existing timing semantics. Use the caller context to bound
the entire invocation, including admission. Supplied run IDs must be 1-128 safe
correlation characters and cannot overlap another active call in this process.
Cancellation can await the existing bounded preflight and process cleanup; it
is not a promise that every process has exited exactly at the deadline.

Result status preserves Offload's `ok`, `error` and `timeout` vocabulary.
Non-success returns `RunError`; context and callback error causes remain
inspectable. Report, native session, usage/cost coverage, measured duration and
raw record remain execution evidence. `RawRecord` preserves the existing JSON
record, including additive fields; it may contain task text and must stay private.
The CLI decodes this compatibility record back into its private history type,
then adds its existing thread, resume and transcript fields. This small in-process
conversion preserves the wire/history format without exposing internal types.

Native-log recovery retains incomplete observations separately: `Result.Usage`
stays nil for them, while `Result.PartialUsage` carries the observed lower bound.
`RawRecord.usage_scope` and `accounting_evidence` retain its scope and provenance.

`OnEvent` receives copied backend output lines in order, at most 256 KiB each.
It receives native content, including arguments/results when emitted, not a
sanitized metadata stream. It must return promptly; an error stops the owned
execution and is reported. Missing/oversized/unterminated lines are not presented
as complete transcript content. Transcript capture retains its
separate completeness/redaction contract through the public `Capture` interface;
the capture owner still creates/finalizes it. `RawEvent` is not a token delta or
a unified tool schema. No credentials are deliberately added to event metadata.

`OnRunEvent` receives sanitized `RunEvent` values, the same type and JSON shape
(including `schema_version`) as the CLI `-events-file` NDJSON, built by the same
code. Events are `start` (run ID, backend, model, and the requested native
session), one `progress` per native output line (allowlisted native type and tool
category only), and `final` (status, exit code, error kind, stop reason). The
native session ID appears on the first progress event after the backend reports
it, and on `final`. They never contain prompts, model text, tool arguments or
credentials, so they are safe to persist. Differences from `OnEvent`:

- `OnEvent` gives raw backend lines (format differs per backend, sensitive);
  `OnRunEvent` gives one normalized, sanitized format.
- The two callbacks and `Capture` are independent and may be combined.
- Both are called synchronously and in order, so a slow callback blocks the run.
  An error from `OnRunEvent` stops the owned execution with error kind
  `events_io`, and is kept in `RunError.Cause`. An error from the `start` event
  prevents the native process from launching. As with `-events-file`, progress
  events stop after 10001 events; `final` is still delivered.

## Compatibility and lifecycle

The CLI continues owning parsing, profile/account selection, session/thread
locks, signal handling, capture files, saved resume settings and history writes.
Both its normal invocation and its existing explicit thread fallback invoke the
public client. Library requests default to no history writes. Setting
`Request.RecordHistory` appends exactly one terminal record through the same
`history.Append` implementation as the CLI, to the standard private state home.
It includes the native outcome, caller/run correlation and private raw result,
including admitted setup failures and cancellation. Invalid/duplicate active run
IDs are rejected without appending another record. The CLI leaves this flag off
and retains its one final write after adding CLI-only resume/thread metadata.
Package records do not fabricate CLI resume settings or task acceptance.

`Result.HistoryRecorded` confirms the append succeeded. `HistoryError` reports
decode/write failure independently of the execution error, status and raw record.
Consumers must expose it; never rerun native work to repair a missing history
entry. This is append completion, not a durable fsync guarantee. Offload Monitor
and `offload ls/history/stats` use the ordinary record without a new schema or
reader migration. Existing consumers can opt out by leaving the flag false.
No historical calls are backfilled or replayed.

The shared worker still publishes advisory live/processing state;
live filenames include PID and execution identity to avoid same-process clashes.
Legacy PID-only live files remain readable and expire under the existing rules.

No `Close` is necessary: each `Run` waits for cleanup and releases its worker
slot before returning, or reports unresolved cleanup in the raw record. Native
processes may have made changes before any failure; cancellation is not rollback.
Library salvage defaults off; the CLI passes its existing setting explicitly.
An unresolved cleanup also returns a `RunError` with kind `cleanup`, preserving
the native outcome and original raw record rather than relabelling execution.

## Validation and rollout

An external consumer module must compile using only the public import and run
fake Claude/Codex fixture processes. Checks cover fresh execution, continuation,
instructions/events, unsupported controls, original error causes, timeout and
cancellation, concurrent isolation, opt-in terminal history, independent write
failure and unchanged CLI JSON/history/transcripts.
Run formatting, `go vet ./...`, `go test -race ./...` and existing macOS checks.
No real account/model invocation or desktop input is needed for these checks.
Release delivery follows the repository's normal PR, CI and verified dual-arch
App/CLI release process. Agent-Host consumes these controls behind its own execution adapter.

## Host native controls

`NativeControls` is optional. The existing Claude path snapshots
executable/profile paths, fresh/fork session IDs, permission mode, tool rules,
MCP server commands, skill directory, JSON schema and at most four 5 MiB images.
Unsupported combinations return `unsupported_controls` before native dispatch.
Restricted reads only mount Read/Glob/Grep and an explicit approval MCP server;
extraction requires a fresh tool-free run with safe mode and no session persistence.
The caller must also validate observed native tool inventories and completion.

`CallerContext` disables Offload's automatic project-file and delegation-prompt
assembly. Caller instructions use a private temporary file, the task uses stdin,
and images use native stream-JSON blocks. Normal native project discovery remains
owned by Claude; extraction and restricted reads exclude ambient instructions.
Credentials always come from the selected Offload profile, never an ambient
native profile's settings or caller ANTHROPIC variables. The optional stream
environment accepts only the validated streaming-recovery switch and positive
output-token limit; it is applied consistently to child environment and settings.

Codex also has an explicit `CodexAppServer` mode. It requires caller context,
owner `bypassPermissions`, an isolated environment and the Offload-selected
binary/account/model; opaque executable and profile overrides remain rejected.
Each clean absolute directory in `SkillRoots` selects exactly its own regular
`SKILL.md`; descendants are not implicitly selected. Offload mounts those
caller-approved external directories as native extra roots. Before a business
thread, Offload canonicalizes that exact manifest, initializes the stdio protocol,
sets the declared roots (or an empty array for an empty manifest), reloads
discovery, explicitly enables every selected file and disables every other
discovered file (including nested, bundled, system
and plugin declarations), then reloads again. The enabled set must equal the
declared canonical paths exactly, with no duplicate paths, plugin entries or
discovery errors. The validated canonical enabled and disabled states are carried
explicitly in `config.skills.config` for both thread start and resume; arbitrary
native config-file contents are never forwarded. Unsupported writes, malformed
responses and state mismatches fail closed without starting or resuming a thread.
Ambient apps, cloud and bundled Skills, plugins, project instructions and MCP
configuration are replaced by the explicit request configuration. Images,
forks, tool-free execution,
extraction, restricted reads and tool allowlists remain unsupported on this path.

App-server raw callbacks carry `NativeProtocol="codex-app-server"` and preserve
the original JSON-RPC method and params. Matching thread/turn completion is the
only success terminal. Model evidence comes from thread start/resume and native
reroute notifications; missing model, usage and cost remain unavailable. On
cancellation Offload first requests `turn/interrupt`, then closes and reaps the
same owned process group under the normal worker bounds.
The initialize request opts into the experimental API required by explicit Skill
roots. Native app-server responses may omit the optional `jsonrpc` member; an
explicit version other than `2.0` is rejected.
Completed agent-message items are authoritative final text when deltas or terminal
turn items are absent. Malformed response envelopes fail before dispatch, and a
failed turn retains only a bounded single-line provider diagnostic.

For Native calls, callbacks retain complete events up to 32 MiB (bounded image
envelopes included), including a final unterminated line. Larger events fail
explicitly. The default 256 KiB callback contract is unchanged for other callers.
After process exit, idle pipe reads are bounded independently of callback time.
Native caller-context cancellation freezes and stops the owned descendant tree,
including native Bash children in separate sessions; it never selects by command
name. NativeExitCode is optional actual process exit evidence, distinct from the
wrapper ExitCode. NativeStderr and original process errors remain private evidence;
`errors.As` can inspect the original `exec.ExitError`. None proves task acceptance.
