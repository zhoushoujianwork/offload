# Newly observed task feedback (+1 / +N)

The macOS app shows a short `+N` next to the running count in the menu bar and in the panel header
when it **observes** new tasks (新观察到的任务). It does not claim to count every run.

## Phase 1 (implemented, no backend change)

Source: `macos/Sources/OffloadMonitor/NewTaskFeedback.swift`, wired in `MonitorModel.ingest`.

- **Identity:** `ActiveTask.id` = `pid|startedAt`. A PID reused with another start time is a new
  identity. A session id is never an identity: a resumed session starts a different invocation.
- **Baseline:** the first successful snapshot only records identities. Tasks that existed before
  the app started are never announced.
- **Counted phases:** a first-seen identity in `queued`, `running`, `salvaging` or `cleaning`.
  Later phase changes, disappearing and reappearing, reordering and re-decoding emit nothing.
  Swapping task A for task B at an equal total still emits `+1`.
- **`lost`:** a first-seen `lost` identity is advisory. It is tallied in the event but never shown
  as `+N`; the existing warning indicator covers it.
- **Only `active` counts:** `totals.runs`, `ok`, tokens, cost and `recent` never produce a hint.
  A rising success count is not a new task.
- **Failures and period switches:** a failed refresh does not touch the tracker. Switching
  Today/Week/Month/All keeps the tracker, because `active` does not depend on the period.
- **Bursts:** one snapshot with several new identities gives one `+N`. Events arriving while a pulse
  is visible merge into it. The pulse lasts 0.8 s (target 600-900 ms), and merging cannot extend it
  beyond 1.8 s from the first event. Nothing is queued. Text is capped at `+9+` and sits in a slot
  sized for the widest text, so neighbours do not move.
- **Retention:** the seen set holds at most 256 identities, evicting the oldest that are not in the
  current snapshot. The latest evicted `startedAt` is kept as a watermark; an unknown identity that
  started at or before it is treated as old, so evicted tasks cannot replay.
- **Reduce Motion:** the upward offset and the number roll are switched off; the badge fades only.
  The VoiceOver announcement ("N newly observed tasks") is posted either way and carries counts only,
  never task text, paths, sessions or models.
- **API level:** minimum macOS 13 (`Package.swift`). `ContentTransition.numericText()` is available
  from macOS 13; the value-based variant needs macOS 14 and is not used.

### Boundaries

- **Short tasks can be missed.** The app polls about every 5 s. A task that starts and ends between
  two polls is in no snapshot and gets no hint. Phase 1 promises "newly observed", not "every run".
- **Sleep or a long outage:** if two successful snapshots are more than 120 s apart, the second only
  re-baselines. Tasks that started during the gap are not announced late.
- **Hidden window:** polling and tracking continue while the panel is closed. Only the menu bar
  item can show the hint then. A hint that expired while hidden is not replayed.
- **Offline / CLI missing:** no snapshots, no events. After recovery, the tracker continues with its
  old seen set, subject to the 120 s rule.
- **Restart:** nothing is persisted. A restart baselines again and does not replay history.

## Phase 2 (proposal only, not implemented)

Needed only if "never miss a run, even a short one" becomes a requirement. Phase 1 must not be
described as meeting it.

1. **Run identity:** generate a `run_id` (ULID/UUID) once per invocation in the CLI, before
   admission. It is not the session id (reused by resume) or the PID (recycled). Write it into the
   live state file and into each history record.
2. **Event log:** append-only `events.jsonl` with a monotonically increasing `seq` per file and
   events `queued`, `started`, `finished`, `error`, `cancelled`. Each carries `run_id`, `seq`, a
   timestamp and a status. No task text, directory, or output is included.
3. **Incremental read:** `offload events --json --after <seq>` returns events with `seq` greater
   than the cursor, plus the current `last_seq` and an `oldest_seq`. The app stores the cursor in
   memory only (or in UserDefaults if replay-free restarts are wanted). If `after < oldest_seq`
   (log trimmed) the reply says `gap: true`; the app then reports an honest gap and re-baselines.
4. **Idempotence:** the consumer keeps a set of `run_id`s per `queued` event, so replays, reconnects
   and duplicated records are no-ops. Reading and rendering stay separate: the reader outputs
   `NewTaskEvent` counts, and the UI only consumes those.
5. **Compatibility:** add the new fields without bumping `schema_version` for `stats --json`
   (the decoder already ignores unknown keys) and ship `events` as a new command, so old apps keep
   working. An app that finds no `events` command falls back to Phase 1. Old history records have
   no `run_id`; they are never turned into events and are only used for totals.
6. **Retention and migration:** trim the log by size/age and always keep `oldest_seq`. Duplicate
   records are dropped by `run_id` on read. Migration needs no rewrite of existing history.
7. **Polling:** the cursor makes a gap detectable, so the poll interval is not shortened to find
   short runs. Accuracy claims need tests for gaps, reconnects and duplicates first.

`recent` (latest 20 rows, no run identity) and `totals.runs` differences cannot support an
"every run" guarantee, so they are not used.

## Verification status

Headless unit tests cover the tracker, the pulse policy, the model wiring and slot width
(`macos/Tests/OffloadMonitorTests/NewTaskFeedbackTests.swift`). Not verified here: the real animation
in the system menu bar and the panel (clipping, click-through, timing calibration), real VoiceOver
output and Reduce Motion. Those need a native session with the app running.
