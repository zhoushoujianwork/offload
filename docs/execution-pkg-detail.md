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

`OnEvent` receives copied backend output lines in order, at most 256 KiB each.
It receives native content, including arguments/results when emitted, not a
sanitized metadata stream. It must return promptly; an error stops the owned
execution and is reported. Missing/oversized/unterminated lines are not presented
as complete transcript content. Existing opt-in transcript capture retains its
separate completeness/redaction contract through the public `Capture` interface;
the capture owner still creates/finalizes it. `RawEvent` is not a token delta or
a unified tool schema. No credentials are deliberately added to event metadata.

## Compatibility and lifecycle

The CLI continues owning parsing, profile/account selection, session/thread
locks, signal handling, capture files, saved resume settings and history writes.
Both its normal invocation and its existing explicit thread fallback invoke the
public client. The library does not append a CLI history record or invent task
acceptance. Its shared worker still publishes advisory live/processing state;
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
cancellation, concurrent isolation and unchanged CLI JSON/history/transcripts.
Run formatting, `go vet ./...`, `go test -race ./...` and existing macOS checks.
No real account/model invocation or desktop input is needed for these checks.
Release delivery follows the repository's normal PR, CI and verified dual-arch
App/CLI release process. Agent-Host integration remains a later consumer change.
