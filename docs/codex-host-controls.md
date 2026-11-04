# Embedded Codex owner execution

The package accepts Host native controls for ordinary owner Codex execution with
`PermissionMode=bypassPermissions` and explicit stdio MCP servers. Host instructions
are passed as developer instructions through a private temporary native profile,
removed after the owned attempt. Task text remains on stdin. Ambient user config,
exec rules and project instruction discovery are excluded from this invocation.
Caller instructions require an [isolated execution environment](isolated-execution-environment.md).
The profile must be loaded without `--ignore-user-config`, which also suppresses
explicit profiles; a foreign base config in the owned home is rejected. Only
authentication is seeded from the Offload-selected account into that home.

The embedding Host must adopt the native thread ID from the first initialization
event; the Claude fresh-session identifier cannot select a Codex thread ID.

Unsupported controls fail before dispatch: tool-free execution, extraction,
read-only member policy, native tool allowlists, Skills, images, JSON schema and
forks. These guards intentionally preserve the existing policy contract rather
than silently ignoring restrictions. This is an initial supported subset, not
full Claude capability parity.
