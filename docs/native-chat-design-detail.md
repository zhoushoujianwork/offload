# Native chat: history API and boundaries

Part of [native-chat-design.md](native-chat-design.md). This file covers the history API and execution boundaries.

## History API

Both commands print one JSON document (`schema_version` 1) on stdout. Failures exit with code 2 and print one line on stderr. That line never contains task text, report text or search text.

### `offload history list --json [--limit N] [--offset N] [--search-stdin]`

- `--limit` defaults to 50 and must be 1 to 200. `--offset` defaults to 0.
- With `--search-stdin`, the search text is read from stdin (at most 4096 bytes, valid UTF-8). It is trimmed and matched case-insensitively against task, result, profile, model, dir and session of every run. A conversation matches if any of its runs matches. Search text never goes in argv.
- A missing run log gives an empty list.
- Output: `schema_version`, `total`, `offset`, `limit`, `has_more`, `conversations`. Each conversation summary has `id`, `session`, `title`, `dir`, `profile`, `backend`, `model`, optional `add_dirs` (the latest run's extra directories), `started_at`, `updated_at`, `run_count`, `status`, `active`, `usage`, `usage_runs`, `cost_usd`, `cost_runs`. The list carries no reports.

### `offload history show --json --id <id>`

- `id` is matched exactly.
- Output: `schema_version`, `conversation` (the same summary), `runs` and `active`. Each run has `id`, `ts`, `status`, `exit_code`, `task`, `result`, `error`, `dir`, optional `add_dirs`, `profile`, `backend`, `model`, `session`, `usage`, `cost_usd`, `salvaged`, `elapsed_ms`, `turns`. Task and result are complete, not truncated.
- `active` is the live worker state for the session, or null. It is advisory.

## Conversations

- All runs with the same non-empty session form one conversation. Its `id` is the session id.
- A record with no session (an early failure) is its own conversation with an opaque id `run:<hash>`, and `session` is empty. The app cannot continue such a conversation.
- Conversations are sorted by latest run time, newest first. Runs are in chronological order. A run with an unreadable timestamp keeps its place by log order and is not dropped.
- The summary shows the latest recorded profile, backend, model and dir. `started_at` is the earliest run. The title is the first line of the first task, at most 100 runes.
- `active` is true when a worker with that session is running or salvaging. A lost worker is not treated as busy.

## Usage and cost

Usage and cost follow the same rules as `offload stats`: salvaged runs, legacy Claude costs and cumulative figures are excluded, and cumulative figures are never summed. `usage_runs` and `cost_runs` say how many of `run_count` runs the totals cover. Null means no trusted figure.

## Boundaries

- The history API only reads. It starts no worker and does not cancel one.
- Cancelling a run and starting or resuming one belong to the existing run command, not to this API.
- The app reads history only through these commands, not from the log file.

## Native execution

- New sends capture a configured profile and pass `-p`, an explicit `-d`, `-json`, and `-t 30m`. Follow-ups use `-r` and omit `-p` so the CLI retains the recorded profile. Task text is sent through stdin, never argv. `-w` is added only when Allow file changes is enabled.
- The General workspace is under `~/Library/Application Support/OffloadMonitor/Workspaces/General`. It is created with owner-only permissions on the first send. Choosing a folder is an explicit workspace override.
- Each conversation permits one app-owned send at a time. The CLI also rejects overlapping resumes. The app retains its conversations while their windows are closed.
- The run command's final JSON is decoded even on a nonzero exit. Session IDs and partial reports survive failures and cancellation. A failed log append is shown as a persistence warning; the app retains that response in memory.
- The app allows 35 minutes for a 30-minute CLI run plus finalization. History and run output have a 16 MiB bound; oversized output is reported explicitly.
- Progress uses published worker activity, not streamed response tokens. Markdown renders locally with selectable text and code blocks. Remote images are not fetched; only explicit HTTP(S) links can be opened.

## Cancellation

- Stop targets only a process started by this app: SIGINT, then a bounded SIGTERM/SIGKILL fallback. The CLI handles SIGINT and SIGTERM and writes its final record.
- Unix provider processes and credential commands start in a new process group. Context cancellation kills that owned group, including descendants that remain in it. Normal completion does not kill intentional background services. Processes that detach into another group/session are outside this mechanism. Non-Unix builds retain direct-child cancellation.
- Credential resolution accepts the run context, so a hanging `token_cmd` can be cancelled. The compatibility `ResolveToken` method remains available.
- Quitting with app-owned tasks prompts to keep the app open or stop its tasks first. External workers are never cancelled by this flow.
