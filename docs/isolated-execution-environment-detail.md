# Isolated execution environment contract

See [usage and scope](isolated-execution-environment.md).

`execution.NewEnvironment` creates private directories for an empty `workspace`,
`home`, `codex`, `claude`, `config`, `cache` and `data`, plus version metadata.
`Request.EnvironmentDirectory` must identify such an environment and `Directory`
must be its workspace. Existing task repositories are explicit additional roots.
Symlinked roots, permissive directories, foreign native profiles and mismatched
profile/account bindings are rejected. Results and history expose the environment
directory independently of the native account/session home.
`execution.NewEnvironmentAt(root)` creates the identical structure at a clean
absolute caller-selected root using exclusive creation. It never adopts or
overwrites an existing root; rollback removes only the root created by that call.

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

## Explicit preset documents

`Request.PresetDocuments` accepts up to four `PresetDocument{Name, Content}`
snapshots. Names are case-insensitive conventional basenames: `AGENTS.md`,
`MEMORY.md`, `USERS.md`, `SECURITY.md`. Paths, duplicates, NUL bytes, invalid UTF-8
and combined instructions exceeding 256KiB are rejected before dispatch.
Callers read and review the content; Offload never searches the local home,
workspace or additional roots to assemble it.

Preset documents require `Native.CallerContext=true`, cannot accompany
`InstructionsFile`, and cannot override extraction's fixed system prompt.
Compilation preserves the caller's `Instructions` and appends labelled sections
in stable order: SECURITY, AGENTS, USERS, MEMORY. These sections occupy one native
instruction message; filenames do not create independent authority levels or a
conflict-resolution policy. The embedding application must resolve conflicting
policies before submission. Model/platform rules and actual native permission
grants remain authoritative. Memory and user facts are contextual data; raw
retrieved material must be reviewed before promotion into this instruction channel.

Claude uses its append-system-prompt-file entry; Codex uses its private native
developer-instructions profile. Codex native caller instructions require an
isolated environment: `--ignore-user-config` suppresses explicit profiles too.
The generated profile is loaded only in that owned home, and a pre-existing
base `config.toml` there causes an explicit error before dispatch. Offload does
not claim to replace a provider's
root system prompt. Snapshots are supplied on each turn, do not mutate the
environment's configuration files, and do not erase prior conversation state.
Use a fresh environment/session when testing removal of a previous preference.

```go
request.Native = &execution.NativeControls{
    CallerContext: true, PermissionMode: "bypassPermissions",
}
request.Instructions = []byte("Host policy chosen by the embedding application.")
request.PresetDocuments = []execution.PresetDocument{
    {Name: "AGENTS.md", Content: []byte("Reply in English and Chinese.")},
    {Name: "USERS.md", Content: []byte("The user's name is Alice.")},
}
```

`TestEnvironmentLiveLanguageAndPresetIsolation` constructs a disposable ambient
home with bilingual rules and a unique contamination marker, without editing the
user's files. Fresh isolated sessions compare an unspecified-language greeting
against explicit bilingual presets plus user/memory/security markers. Language
alone is behavioral evidence, not proof of containment; controlled process tests
also verify native paths and environment filtering.
