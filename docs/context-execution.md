# AGH context execution

Use [the execution SDK](execution-pkg.md) and set `Request.Context` to
`ContextExecution` to dispatch an AGH context envelope. Existing callers can
keep using their current `Client.Run` requests. The managed path owns the local
execution journal. AGH owns authorization, knowledge assembly, checkpoint
acceptance, credential policy and the broker.

## Dispatch and receipt

1. Open a private, canonical local journal directory (mode `0700`) with
   `OpenContextStore`. Use the same directory for all processes handling the
   task. Keep the journal's dispatch keys for the whole supported retry window.
2. Resolve the authorized project, resource roots, native executable and Skill
   sources to absolute paths. Populate the envelope with the current AGH scope,
   versions and capability requirements. Hash task and instruction bytes with
   `ContextContentDigest`. Preset descriptors use the uppercase preset name,
   source ID, classification, original byte count and digest. Pass the actual
   bytes in the SDK request. Ordinary MCP server descriptors pin the digest of
   their resolved `MCPServer` configuration.
3. Seal with `envelope.Digest()` and place the result in `EnvelopeDigest`.
   Seal each binding with `binding.Digest()` before sealing the envelope. The
   version is `1.0`. Top-level optional extensions survive JSON forwarding
   within major version 1. Digests use compact Go JSON with ordered object
   keys and preserved number tokens. Credential values travel outside the
   envelope.
4. Supply an `Authorize` callback that checks current grants. Offload invokes
   it before claiming the dispatch and again immediately before opening the
   native process. When authorizing a handoff, check each checkpoint reference
   against the receiving Agent, scope, audience and current policy. Use the
   run's context to cancel a dispatched turn if AGH revokes its access.
5. Read `Result.ContextReceipt` on success and error. The injection evidence is
   produced after native launch for Claude and after an acknowledged Codex
   turn start. It includes the actual cwd, configured roots, pinned Skill
   snapshots, tool versions, compiled instruction digest and byte count, and
   per-document digests and byte counts. Resource-root digests are authorized
   metadata for caller-managed directories. AGH validates any corresponding
   snapshot or revision. The receipt contains no instruction or document text,
   broker configuration or credential values.

A terminal receipt distinguishes `completed` execution, pre-dispatch
`rejected` execution and an `unknown` outcome after launch. Completion is
execution evidence. AGH separately confirms the task and its side effects.
The journal persists only receipts and reference metadata. Private native
session files live beside the journal and have the provider's retention and
format. AGH owns journal and runtime retention.

A replay of the same still-authorized envelope returns
`dispatch_already_recorded` with the prior receipt and executes no native turn.
Key reuse with changed input yields `idempotency_conflict`. A claimed dispatch
without a terminal receipt yields `dispatch_unknown`, including an interrupted
process or an in-flight concurrent request. Reconcile side effects before
assigning a new dispatch key. Separate machines must share the same dispatch
owner or use AGH's own durable claim system.

## Runtime capabilities

| Capability | Claude stream-json | Codex app-server |
| --- | --- | --- |
| Clean home, config, cwd and explicit instructions | Yes | Yes |
| Explicit ambient runtime | Yes, fresh invocation | Pre-dispatch rejection |
| Additional workspace roots | Native `--add-dir` | Thread and turn workspace roots |
| Exact Skill snapshots | Pre-dispatch rejection | Verified staged copies |
| Tool-scoped credential broker | MCP | MCP |
| Managed native resume | Same clean runtime and compatible fingerprint | Same clean runtime and compatible fingerprint |
| Explicit task checkpoint handoff | Yes | Yes |

`RequiredCapabilities` accepts `clean`, `ambient`, `preset_documents`,
`resource_roots`, `native_resume`, `exact_skills`, `mcp`, `credential_broker`
and `checkpoint`. Unknown or unavailable requirements fail before launch.

Clean is the default. Offload creates a private native home and empty cwd with
the primary project as the first workspace root. Only selected roots go into
the native protocol. The boot environment keeps PATH and locale and sets fresh
home/config/cache/data locations. Claude clean mode uses an explicit gateway
token in the selected profile. Codex clean mode uses a caller-supplied
`ContextExecution.CodexAPIKey` and an empty native authentication directory at
first launch. The selected profile and executable are explicit in both cases.
Set `Runtime.AuthVersion` to the provider credential version and change it on
rotation.

Claude ambient mode additionally requires explicit authorization in the
envelope and in `Authorize`. Its cwd is the primary project. The receipt
identifies the inherited HOME, Claude configuration, Skill/plugin roots and
environment classes. A clean setup failure ends the dispatch.

Workspace roots constrain native context selection. Filesystem access still
depends on the caller's OS permissions. Requests that require an enforced
per-root mode, such as read-only, fail before dispatch.
The declared tools select MCP servers. Native built-in tool behavior follows
the adapter's permission policy. Claude supports explicit tool-free execution
through `Runtime.ToolsDisabled`.

Resume requires the `NativeSessionRef` from the latest successful receipt and
the same subject, task, scope, authorization, runtime and injected context
fingerprint. The next turn may supply a new task message. One successor can
claim a receipt. Use reference-based handoff after uncertain native execution,
when native state is absent, or when the receiving configuration changes.
Credential-bound turns start fresh and have no reusable managed session.

## Checkpoints and tool leases

Call `ContextStore.SaveCheckpoint` with the source run and the explicitly
confirmed steps, independent evidence references, pending/unknown step IDs and
artifact references. Repeating an identical save returns the same reference.
The receiving Agent supplies that reference in a new envelope. AGH selects and
assembles the authorized facts or artifacts into that request. It can allow a
different audience, scope or backend after independently authorizing the
references. Offload never reads the producer's native transcript to assemble
the receiver's context.

Version 1 supports `broker` and `mcp` credential delivery. Offload validates an
opaque, time-limited binding against its Run and declared MCP tool. The broker
installs a dedicated endpoint with non-secret configuration. It authenticates
the tool process, enforces allowed operations, redemption counts, expiry and
revocation, and scrubs results before returning them to the native harness.
Secret values are never passed to Offload by this interface.

Offload bounds the run by the earliest envelope, checkpoint or lease expiry.
It calls broker release on success, error and cancellation, including partial
activation. A release failure is visible in the receipt. The broker owns expiry
and revocation after process death. Unsupported delivery methods such as direct
environment values or temporary secret files fail before native launch.
