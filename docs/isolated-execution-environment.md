# Start with a clean native environment

Embedding applications can ask the Go package for a fresh execution environment.
It has its own native homes and an initially empty workspace, so existing user
configuration, history, Skills and plugins are not imported automatically.

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

This isolates execution configuration, not filesystem or network permissions.
Native permission policies still apply, and unsupported Host controls remain
unsupported. See [the detailed contract](isolated-execution-environment-detail.md).
