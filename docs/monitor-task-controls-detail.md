# Monitor task controls — implementation

[User-facing scope](monitor-task-controls.md).

## Execution ownership

Each monitored execution registers a private loopback endpoint under its Offload
run ID. A random capability in an owner-private record authorizes Stop. The
server cancels only that run's context, including when independent calls share
the same Go process. No control operation signals a shared Host PID.

Control files reject unsafe owners, permissions, symlinks and malformed IDs.
Requests reject browser origins and redirects. PID/start identity prevents
dispatch to abandoned registrations. Cleanup and terminal history precede
terminal control state; failure remains explicit. Records expose no capability
or command arguments in the App-facing protocol.

`offload task show|stop|resume --id RUN_ID --json` supplies bounded structured
responses. Native output is read separately through the existing transcript
cursor API. The App polls only while a detail is open and bounds retained text;
gaps, redactions and unavailable capture remain distinguishable from completion.
New CLI tasks capture bounded private transcripts by default; explicit
`-capture-transcript=false` opts out. Package consumers opt in through
`Request.Monitor`, without changing the defaults of existing SDK callers.

## Explicit resume

A private resume lease and persisted source/child link prevent duplicate
continuations. Resume requires terminal cleanup, a native session and either
CLI saved settings or an explicit caller command. The CLI validates its original
profile fingerprint. Embedding Hosts validate their own runtime, session and
authorization before dispatch. Credentials are resolved anew, never stored in
resume commands. Commands are exact argv and absolute working directories,
executed without a shell.

Resume launches a new owning process and returns its registration (or an explicit
starting state). The old record stays canceled/failed, with `resumed_by` pointing
to the new run and `resumed_from` on the new execution. Startup failures do not
silently retry. Missing legacy metadata does not grant resume or stop authority.

Agent-Host direct CLI supplies a resume entry point containing the exact owned
session and canceled Host run ID. Its explicit canceled-run gate requires the
latest owned canceled run and matching Offload session/cleanup evidence. The
normal runtime fingerprint and session lease still apply. Room/channel and
auxiliary calls without a reconstructable caller entry point show Resume as
unavailable rather than invoking a native continuation outside Host.

## Acceptance

Quiet tests must prove isolation of two SDK runs in one process, authenticated
Stop, stale/unsafe record rejection, streamed capture before completion, capture
gaps/unavailability, canceled terminal state, native session retention, explicit
resume creating a new linked ID, duplicate-click prevention, unchanged old
records and configuration/cleanup refusal. Test a real fixture CLI lifecycle and
an independent public consumer, then verify App model polling and offscreen
details in running, stopping, canceled, resumed and unavailable states.

Real platform messages and desktop input require their own acceptance scope.
Release acceptance follows the repository's PR/CI and verified dual-architecture
App/CLI delivery process. Existing production services are not switched by tests.
