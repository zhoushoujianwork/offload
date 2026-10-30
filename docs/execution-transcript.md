# Execution transcript interface

Status: implemented, schema version 1. Companion detail: [execution-transcript-detail.md](execution-transcript-detail.md).

A supervisor can ask offload to record the conversation of one invocation and read it back while the
run is active or after it ends. The interface is additive and off by default.

## What exists today

| Source | Content kept | Usable for replay |
| --- | --- | --- |
| `-events-file` | Allowlisted metadata only: `native_type`, coarse `tool_category`, counters. Deliberately no text, arguments or output. | No. It stays a provenance channel. |
| `history list/show --json` | Task text and the final report of each run, plus live summary state. | No tool calls, no intermediate messages. |
| `live` state files | Phase, last tool name, counters. | No. |
| Worker stdout (`stream-json` for Claude, `exec --json` for Codex) | Full content, but offload folded it into a summary and discarded it. | This is the source the new interface taps. |

Neither backend uses ACP. Claude Code is run with `claude -p --output-format stream-json --verbose`
and Codex with `codex exec --json`. Their event shapes differ, and offload maps both to one schema.

## Decision

Capture is a separate opt-in channel: `-capture-transcript`. It writes one private NDJSON file per
run. A new read-only command, `offload transcript read --json`, serves bounded pages with a cursor.

Alternatives considered:

- Add content to `-events-file`: rejected. ClawFlow validates that file as metadata, and the issue says
  it must not become a content transport.
- Read the Claude or Codex private session files: rejected. Formats are unstable and files from
  unrelated sessions share the same directories.
- Run a server or socket: rejected. A file plus a CLI works over a LAN worker's own shell, needs no
  port and no new dependency, and survives offload exiting.

## Contract

```sh
offload -json -p codex -caller-run-id clawflow:run-123 -capture-transcript -d /repo "task"
offload transcript capabilities --json
offload transcript read --json --caller-run-id clawflow:run-123 [--cursor C] [--limit N] [--max-bytes N]
offload transcript read --json --run-id <32 hex> --cursor C
offload transcript list --json [--caller-run-id ID] [--limit N]
```

- Every response has `schema_version` (1), `interface` (`offload.transcript`) and `availability`.
- `read` exits 0 whenever it printed a document, including every non-`available` value. Exit 2 means a
  usage error.
- Events carry `run_id`, `caller_run_id`, `backend`, `model`, `session`, `attempt`, `ts` and a strictly
  increasing `seq` starting at 1. `capture_id` identifies the file and is inside every cursor.
- Event types: `capture_start`, `attempt_start`, `message`, `tool_call`, `tool_result`,
  `backend_error`, `gap`, `execution_end`.
- Binding a running task: pass the same `-caller-run-id` to `read`. The file exists before a worker slot
  is requested, so a queued run is already readable. If several runs share a caller ID the response is
  `ambiguous` with `candidates`; use `run_id` from the `-json` result or from `list`.
- Paging: `next_cursor` is always returned. Pass it back unchanged. `has_more` means another page is
  ready now. `end_of_stream` is true only when the invocation ended or was abandoned and everything was
  delivered. While `state` is `running`, an empty page is normal; poll again.
- A trailing line without a newline is never delivered. It is counted in `partial_tail_bytes`.

## Availability

`available`, `not_yet_available`, `no_captured_content`, `capture_disabled`, `expired`, `unreadable`,
`unsupported_backend`, `unsupported_version`, `not_found`, `ambiguous`, `cursor_invalid`,
`identity_mismatch`. After `cursor_invalid` the client discards what it showed and reads from the start.

## Completeness

`in_progress` while running. After the end: `complete`, `bounded` (all events present, some fields cut,
marked in `truncated`), or `incomplete` with `completeness_reasons`: `gaps`, `dropped_events`,
`capture_size_limit`, `write_failed`, `unrecognized_backend_events`, `tool_calls_without_result`,
`no_execution_end`, `corrupt_lines`, `sequence_gap`. A client must not label an `incomplete`
transcript as a full replay.

## Defaults and limits

Capture is off. Directory mode 0700, file mode 0600, exclusive create, symlinks and files owned by
others are rejected on write and on read. Message 64 KiB, tool arguments 16 KiB, tool output 32 KiB,
event 128 KiB, file 8 MiB, page 200 events or 256 KiB (maximums 1000 and 1 MiB). Retention: 14 days,
200 files, 512 MiB total, applied when a new capture starts. A transcript whose writer is alive is
never removed.

## Not provided

Text deltas (neither backend is asked for partial messages), reasoning content (counted in
`skipped_blocks`), non-text tool output such as images (named in `omitted`), transcripts for runs that
did not request capture, and any inference of a final answer or acceptance from transcript events. The
final result remains the stdout/`-json` record.

## Redaction limits

Best effort. Removed: the profile token, values of environment variables whose names contain KEY,
TOKEN, SECRET, PASSWORD or CREDENTIAL, Codex `auth.json` credentials, and common credential shapes.
Source code, prompts and command output are stored as produced. A secret with no known value and no
recognizable shape is stored. Transcripts must not be forwarded to public logs or analytics.

## Verification status

Fixture-verified (headless, fake backends): everything above. Not verified: a real Claude or Codex run
with `-capture-transcript`. The mapping follows the event shapes the existing parsers already consume
(`internal/worker/stream.go`, `codex.go`); Claude `tool_result` blocks and Codex `aggregated_output`
and `exit_code` fields are mapped from the public stream formats and have only fixture coverage. A
bounded, authorized smoke run should confirm them before ClawFlow relies on tool results.
