# Menu-bar and panel header: state motion

Motion is decoration on top of the status from #35. It never carries information of its own: with motion off, the
symbol, the title and every number are identical. Code: `MotionPolicy.swift` (pure rules and state machine),
`MotionModel.swift` (timer owner and system hooks), `MotionViews.swift` (SwiftUI pieces).

## State to motion

| Status kind | Motion | Ends when |
|---|---|---|
| unknown (first fetch pending) | slow opacity pulse on the `ellipsis` symbol, the count stays `–` | 3 s, or data or an error arrives. Not restarted on later polls |
| idle | none | no timer exists |
| queued | none. A queue is waiting, not executing, so nothing rotates or breathes | no timer exists |
| working (running, salvaging, cleaning) | slow opacity breathing of the symbol, no percentage | state leaves working, refresh fails, or no fresh observation within 20 s, or motion is switched off |
| attention (lost, unresolved, stalled) | one 1.2 s pulse when the state is entered, then a static warning | after the pulse. Not repeated within 30 s, not played for what was already true at launch or wake |
| unavailable, stale | none | static warning symbol only |
| a task finished or failed | **none** | see "Terminal events" |

The two surfaces use the same phases. Only the strength differs: the system menu bar dims to 0.6 with a 1.4 s half
period, the panel header dims to 0.35 with 1.1 s. The count slot uses `numericText` when the number is known and fresh
(`numericText(value:)` behind `#available(macOS 14.0, *)`, `numericText(countsDown:)` on macOS 13).

## Lifecycle and cost

- One timer at most per surface, only while a phase needs steps. Idle and queued states schedule nothing. Each
  step is an ordinary state change; the timer is a cancellable `Task.sleep`, not a repeating animation.
- The poll loop (`MonitorModel`, 5 s, one CLI process at a time) is untouched. Motion only observes the results the
  model already publishes and never calls the client.
- Every phase change bumps a generation number. A timer from an older generation does nothing, so quick
  round-trips cannot leave a stale animation behind.
- Cancelled and not replayed: state change, Reduce Motion or in-app switch turned on, panel closed (the header only
  runs while a view is on screen), system or display sleep, CLI error, app restart (a new driver starts from a
  baseline). After wake the first fresh observation is a baseline: continuing work shows again, nothing is replayed.
- Motion is `allowsHitTesting(false)` opacity only. It takes no focus, opens no window, and does not change layout,
  so the menu-bar item and the panel controls respond at once.

## Accessibility

`NSWorkspace.accessibilityDisplayShouldReduceMotion` is read at start and on
`accessibilityDisplayOptionsDidChangeNotification`. Settings > App > "Animate status symbol" switches decorative
motion off (default on) and is disabled, with an explanation, while Reduce Motion is on. The static equivalent keeps the
symbol shape, title, count and the VoiceOver value.

## Terminal events (success, failure): deliberately not implemented

`offload stats` lists active tasks and a recent-run list, but a finished run cannot be reliably tied to a task that
was active before: the active entry is keyed by pid and start time, the run row by timestamp and session. A task
vanishing from `active` can mean success, failure, a lost worker, or a CLI that raced. History rows exist at launch
and must not replay. Guessing would produce false success or repeated failure feedback, so no terminal motion is
shipped. A follow-up needs an event contract: a stable run id on both the active entry and the terminal record, so
the app can show a one-shot exactly once per id.

## Platform notes

- `symbolEffect` is macOS 14+ and the brand mark is a template `NSImage`, not an SF Symbol. The implementation
  therefore does not use `symbolEffect`; it animates the opacity of an ordinary `Image(systemName:)`, which exists
  on macOS 13. Availability guards: `numericText(value:)` (14+) only. Nothing else here needs a guard.
- Whether the system menu-bar host interpolates the opacity step is not known without a real run. The design does not
  depend on it: if the host redraws without interpolation, the symbol steps between two opacities, which still
  signals "working" and stops on the same conditions.
- AppKit `NSStatusItem` + `NSStatusBarButton` fallback (not built): needs a status item owner, a hosting popover in
  place of `MenuBarExtra(.window)` (anchor, dismissal, accessibility and highlight handled by us), and moving
  `AppState.showDashboard` wiring. That is a replacement of the scene, not a patch, and it needs the #35 layout
  re-verified (no effort estimate was measured). It is justified only if the real menu bar shows the opacity steps clipped or
  dropped. `NSStatusItem.view` is deprecated and is not an option.

## Native acceptance: not done in this change

The following need a person on a real desktop and were not run (no authorization to operate desktop apps). Record
results in the PR before merge:

| Check | Result |
|---|---|
| Feasibility gate: a synthetic working state in the real `MenuBarExtra` label refreshes, steps, is not clipped, and tints correctly when highlighted | not run |
| macOS 13 (no `numericText(value:)`) and macOS 14+/26 behaviour | built and unit-tested on macOS 26.x only (Swift 6.3.3, arm64). 13 to 25 not covered |
| light / dark / highlighted, Reduce Motion on and off, in-app switch | not run |
| multiple displays and scale factors, sleep/wake | not run |
| open and close the menu or click controls during motion | not run |
| CPU / energy: Activity Monitor or `top -pid` sample for ~2 min in idle, queued, working with motion on, working with motion off | not run. No number is claimed |
