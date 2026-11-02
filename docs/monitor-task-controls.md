# Monitor task details and controls

Open a task in Monitor to read captured messages and tool activity while it is
running. Stop requests cancellation of that execution and shows “Stopping” until
native cleanup is confirmed. Existing edits and external effects are retained.

After a stopped or failed run has ended, Resume explicitly creates a new run in
the compatible native session. The old outcome remains unchanged and links to
the new execution. Repeated clicks do not start duplicate continuations.

Offload CLI runs use their saved execution settings. Go consumers opt into
monitoring and provide a caller-owned resume command when they can reconstruct
the authorized context. Agent-Host CLI resumes through Host, which rebuilds its
tools/context and records a new Host run. Offload does not adopt Host policy.

Missing capture, absent native sessions, unresolved cleanup, changed
configuration and missing caller resume support are visible unavailable states.
Older processes remain observation-only; updating the App cannot add controls or
recover output they never recorded. No task resumes automatically.

The first implementation covers task details, captured output, Stop and Resume.
It does not add interactive input takeover or guarantee instruction-level
checkpoint recovery. Output updates when native message/tool events arrive;
the backend determines their granularity.

[Lifecycle, protocol and validation details](monitor-task-controls-detail.md).
