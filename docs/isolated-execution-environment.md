# Start with a clean native environment

Embedding applications can ask the Go package for a fresh execution environment.
It has its own native homes and an initially empty workspace, so existing user
configuration, history, Skills and plugins are not imported automatically.

Import `github.com/zhoushoujianwork/offload/pkg/execution` in a Go 1.24+ module.
See the [complete compiling example](../pkg/execution/environment_example_test.go)
for configuration, explicit presets, observed model information and two-turn
continuation. The embedding application links the Go package; Offload does not
launch its own CLI, but the selected native Codex/Claude executable must be installed.

```go
environment, err := execution.NewEnvironment()
// Handle err and retain environment.Root privately for subsequent turns.
request.Directory = environment.Workspace
request.EnvironmentDirectory = environment.Root
result, err := client.Run(ctx, request)
```

Mount task directories explicitly with `AdditionalDirectories`. The selected
Offload profile still determines the backend, model and authentication. Omitting
`EnvironmentDirectory` preserves existing execution behavior.

Reuse the same environment and native session ID for continuation. A different
profile/account requires another environment. The caller owns retention and must
serialize access; the environment contains private authentication and task data.
Creation uses a private temporary directory, so long-term retention also depends
on the host's temporary-directory policy.

Callers can explicitly provide reviewed `AGENTS.md`, `MEMORY.md`, `USERS.md` and
`SECURITY.md` snapshots through `Request.PresetDocuments`. With native
`CallerContext` enabled, these join `Instructions` in the native instruction
channel. No user-home file discovery occurs. Omitting them leaves the new
environment free of those ambient preferences. See the detailed contract for
roles, limits and acceptance.

`LoadConfig` requires an explicit profile name or alias and an absolute path.
Applications that follow a mutable default must resolve that default themselves
and create a new client when it changes. Omit `Request.Model` to use the selected
profile's model; use `ObservedModel` as execution evidence, leaving it unknown
when native events provide none. Bind each environment to its selected profile
and account, and start another environment when that binding changes.

This isolates execution configuration, not filesystem or network permissions.
Native permission policies still apply, and unsupported Host controls remain
unsupported. See [the detailed contract](isolated-execution-environment-detail.md).
