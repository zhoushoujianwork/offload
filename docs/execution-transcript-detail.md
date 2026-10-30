# Execution transcript: implementation detail

Companion to [execution-transcript.md](execution-transcript.md).

## Code map

- `internal/transcript/schema.go`: event, response and bound definitions.
- `internal/transcript/recorder.go`: writer. Numbers events, redacts, then bounds, then writes one line.
- `internal/transcript/backend.go`: maps Claude `stream-json` and Codex `exec --json` lines to events.
- `internal/transcript/read.go`: availability, cursor, completeness, `List`.
- `internal/transcript/store.go`: path safety, retention.
- `internal/worker/output.go`: `lineTap` receives every complete line the parser sees, and a note for
  each line it had to drop (over 16 MiB, or an unterminated final line that is not valid JSON).
- `internal/cli/transcript.go`: the `transcript` command and `capabilities`.

## Writer

The writer holds one `O_APPEND` descriptor, one event per `write`, always newline terminated. Redaction
runs before truncation so a secret cut in half is not left behind. If escaping makes a line exceed
128 KiB the largest field is halved until it fits and marked in `truncated`. The last 16 KiB of the file
cap is reserved so `execution_end` can always be written. Past the cap, content events are dropped,
counted, and one `gap` is written. A write error stops capture and is reported in `execution_end` if
that can still be written. Capture never changes the run's result or exit code. The only exception is
setup: when `-capture-transcript` was asked for and the store cannot be created safely, the run fails
with `configuration` before any worker starts, because the supervisor asked for something that cannot
be delivered.

`capture_start` records the writer's PID and process start time. A reader treats the run as `running`
only while that process is alive and no `execution_end` exists; otherwise it is `abandoned`.

## Cursor

`t1.<capture_id>.<byte offset>.<next seq>`. A reader rejects a cursor whose capture ID differs from the
file's, whose offset is past the end, or whose offset does not follow a newline. Because the writer only
appends, a valid cursor stays valid until the file is removed. Sequence numbers let a client drop
duplicates after a reconnect.

## Backend notes

- Claude: assistant events carry `text`, `thinking` and `tool_use` blocks; user events carry
  `tool_result` blocks. Messages arrive whole.
- Codex: `item.started` and `item.completed` repeat the same item. The mapper writes one `tool_call`
  and one `tool_result` per item ID. Output and exit code come from `item.completed`.
- Claude `result` events that report an error become `backend_error`. The success result text is not
  copied; the final answer stays in the normal result.

## Tests

`internal/transcript/transcript_test.go` (unit), `internal/cli/transcript_test.go` (end to end with
fake backends, including tailing a running invocation), `internal/worker/output_test.go` (tap).
