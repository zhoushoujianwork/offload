# Isolated execution environment contract

See [usage and scope](isolated-execution-environment.md).

`execution.NewEnvironment` creates private directories for an empty `workspace`,
`home`, `codex`, `claude`, `config`, `cache` and `data`, plus version metadata.
`Request.EnvironmentDirectory` must identify such an environment and `Directory`
must be its workspace. Existing task repositories are explicit additional roots.
Symlinked roots, permissive directories, foreign native profiles and mismatched
profile/account bindings are rejected. Results and history expose the environment
directory independently of the native account/session home.

Child environment variables start from an allowlist: native HOME/XDG paths,
PATH, TMPDIR, locale and TLS certificate locations. Arbitrary parent variables,
shell startup variables and provider credentials are not inherited. Offload's
selected profile continues to resolve its own authentication normally.

Claude receives a separate CLAUDE_CONFIG_DIR and bare execution. Codex receives a
separate CODEX_HOME, ignores existing user config and rules, disables bundled
Skills and login shells, and uses only explicitly selected MCP configuration.
The owned workspace avoids automatic discovery from an existing task repository.
Explicit additional task roots remain accessible under the native permission
policy; this is not OS containment.

Codex currently requires private file-backed authentication from the selected
account. Only auth.json is seeded, once. Source configuration, sessions, history,
Skills and caches are not copied. Native credential refreshes remain in this
environment; Offload does not overwrite the original login. Create a new
environment to adopt a changed source authentication snapshot. Keep and protect
the environment's credentials as an independent native login snapshot.

Codex continuation requires a transcript already recorded in this environment.
The caller must serialize runs within one environment, including across processes.
Environment retention and explicit disposal belong to the embedding application;
normal completion does not remove task output or native history.

Acceptance covers controlled child-environment inspection, rejecting foreign
workspace/profile/symlink inputs, preserving independent credential refresh,
Claude native-directory separation, and an opt-in real Codex two-turn continuation
using OFFLOAD_ISOLATED_LIVE=1 with TestEnvironmentLiveCodexResume.
