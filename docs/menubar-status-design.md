# Menu-bar and panel header: status hierarchy

Both places use the same four layers, in this order of emphasis: **brand mark, primary status, task count, actions**.
The state comes from one pure reducer, `OffloadStatus.reduce` (`macos/Sources/OffloadMonitor/StatusModel.swift`).

## States

| Kind | When | Symbol (shape, not color) | Count in menu bar |
|---|---|---|---|
| unknown | no snapshot yet, no error | `ellipsis` | `–` |
| unavailable | no snapshot, fetch failed | `exclamationmark.octagon` | `–` |
| idle | snapshot, nothing active | none (slot kept) | `0` |
| queued | only queued tasks | `hourglass` | queue length |
| working | running / salvaging / cleaning | `arrow.triangle.2.circlepath` | workers |
| attention | lost, unresolved, or running/salvaging marked stalled | `exclamationmark.triangle.fill` | workers (may be `0`) |
| stale | refresh failing and data older than 20 s | `clock.badge.exclamationmark` | `–` |

Rules: unknown is never shown as 0 or Idle; a queued task is never "stalled"; past failed runs are not a current
problem; there is no disabled state because `offload stats` has no source for it.

## Menu bar (template rendering: tinted by the system for light, dark and highlighted)

```
[mark 16] [symbol slot 13] [count slot 28]      fixed total width 16+3+13+3+28 = 63 pt
   mark      cycle arrows     12                working, 12 workers
   mark      warning          2                 attention, 2 running
   mark      (empty)          0                 idle, symbol slot empty, width unchanged
   mark      ellipsis         –                 unknown
   mark      clock            –                 stale
   mark      hourglass        99+               queued, 100+ tasks
```

The update hint is a 5 pt dot on the mark, not an extra item, so it cannot move neighbours.
Everything is monochrome template content, so the highlighted state needs no separate asset.
Tooltip and VoiceOver value carry the full sentence (counts and data freshness).

## Panel header (400 pt wide)

```
[mark 17]  <symbol> Working              [Profile v] [New Chat] [Refresh] [Settings]
           2 running · 3 queued
[ Today | Week | Month | All ]
```

1. Brand mark, 17 pt, `.primary`.
2. Status: 13 pt semibold title with state symbol, 10 pt secondary detail line (counts, "refresh failed", or
   "Last known ... as of N ago"). Fixed 150 pt column.
3. Actions: profile menu, New Chat, Refresh, Settings. Borderless, each with a tooltip and an accessibility label.

Light and dark use only system colors (`.primary`, `.secondary`) plus orange/teal/blue tints on the symbol. The
symbol shape and the title text carry the state, so the tint is redundant.

## Not verified here

Real menu-bar behavior (click, popover anchor, multiple displays and scaling, a crowded menu bar, VoiceOver
reading) needs a person on a real desktop. The headless tests only check layout widths and text.
