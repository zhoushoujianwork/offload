import Foundation

// Pure motion rules for the status shown in the menu bar and the panel header. Nothing here touches SwiftUI,
// timers or the CLI, so every rule can be tested with synthetic `OffloadStatus` values and explicit clocks.
//
// Principles:
// - Motion is decoration. The symbol, the title and every number are rendered the same with or without it.
// - Motion has an end. Working motion stops when the state or the data validity ends; every other motion is a
//   short, bounded one-shot.
// - Motion never reports progress. There is no percentage, no counting up, and nothing that looks like a task
//   is moving when it is only waiting.
// - No terminal (success/failure) motion exists. The stats data has no deduplicable terminal event that can be
//   tied to one run, and "the task disappeared" must not be read as success. See docs/menubar-motion-design.md.

/// What the decorated status symbol is doing right now.
enum MotionPhase: Equatable {
    /// Static symbol. The only phase that costs nothing.
    case still
    /// First data has not arrived: a short, low-contrast pulse on the "…" symbol. Bounded.
    case loading
    /// Running, salvaging or cleaning with fresh data: a slow breathing of the symbol. Lasts while the data is valid.
    case working
    /// A lost, unresolved or stalled task was just detected: one short attention pulse. Bounded, then static.
    case attention
}

/// Whether decorative motion may run at all.
struct MotionContext: Equatable {
    /// The macOS "Reduce Motion" accessibility setting.
    var systemReduceMotion: Bool
    /// The in-app switch. Default on.
    var decorativeEnabled: Bool

    var allowed: Bool { decorativeEnabled && !systemReduceMotion }
    static let enabled = MotionContext(systemReduceMotion: false, decorativeEnabled: true)
}

enum MotionSurface: Equatable {
    case menuBar
    case panelHeader
}

/// Strength of a motion on one surface. Both surfaces share the same states and phases; only the strength differs.
struct MotionStyle: Equatable {
    /// Lowest opacity of the symbol during working motion.
    var dimOpacity: Double
    /// Seconds from full to dim opacity. One full breath takes twice this.
    var halfPeriod: TimeInterval
}

enum MotionPolicy {
    /// The loading hint runs this long at most, then the symbol stays static until data or an error arrives.
    static let loadingDuration: TimeInterval = 3
    /// Length of the attention one-shot.
    static let attentionDuration: TimeInterval = 1.2
    /// A second attention pulse is not played sooner than this after the previous one, so a task that flips
    /// between attention and working does not raise the alarm repeatedly.
    static let attentionCooldown: TimeInterval = 30
    /// Working motion is kept only while data is this recent. Longer without a new observation, it stops.
    /// Equal to the stale threshold (`MonitorModel.staleAfter`, 20 s), and well above the 5 s poll interval.
    static let validityWindow: TimeInterval = OffloadStatus.staleDefault

    /// The phase a status asks for when it is newly observed. `.attention` is an event, not a state: the driver
    /// plays it once on entry and then returns to `.still`.
    static func target(for status: OffloadStatus, context: MotionContext) -> MotionPhase {
        guard context.allowed else { return .still }
        // Data that has not been confirmed within the validity window no longer describes the present.
        let current = (status.dataAge ?? 0) <= validityWindow
        switch status.kind {
        case .unknown:
            return .loading
        case .working:
            // A failed refresh means the data may no longer describe reality: do not keep animating "working".
            return status.freshness == .fresh && current ? .working : .still
        case .attention:
            return status.freshness == .fresh && current ? .attention : .still
        case .idle, .queued, .unavailable, .stale:
            // Queued is waiting, not executing: it never moves. Idle, unavailable and stale are static states.
            return .still
        }
    }

    /// Seconds between two steps of a phase's animation, or nil when the phase is static and needs no clock.
    static func tickInterval(for phase: MotionPhase, style: MotionStyle) -> TimeInterval? {
        switch phase {
        case .still: return nil
        case .working, .loading: return style.halfPeriod
        case .attention: return attentionDuration / 2
        }
    }

    /// Symbol opacity for a phase at one step. `beat` is true on the bright step. A static phase is always 1.
    static func opacity(phase: MotionPhase, beat: Bool, style: MotionStyle) -> Double {
        phase == .still || beat ? 1 : style.dimOpacity
    }

    static func style(for surface: MotionSurface) -> MotionStyle {
        switch surface {
        case .menuBar: return MotionStyle(dimOpacity: 0.6, halfPeriod: 1.4)
        case .panelHeader: return MotionStyle(dimOpacity: 0.35, halfPeriod: 1.1)
        }
    }

    /// Whether a changing count may animate its digits. Only known numbers of fresh data do; "–" never does.
    static func animatesCount(_ status: OffloadStatus, context: MotionContext) -> Bool {
        context.allowed && status.counts != nil && status.kind != .stale && status.freshness == .fresh
            && (status.dataAge ?? 0) <= validityWindow
    }
}

/// The state machine behind one surface's motion. It is a value type with an explicit clock, so tests can run
/// any sequence of observations without waiting.
///
/// `generation` increases whenever the phase changes. Anything scheduled for an older generation (a finish
/// timer, for instance) is stale and must be ignored, which is how a state change cancels the old animation.
struct MotionDriver {
    private(set) var phase: MotionPhase = .still
    private(set) var generation = 0
    /// When the current phase ends by itself, or nil. The owner schedules one sleep for it and calls `expire`.
    private(set) var deadline: Date?

    private var lastKind: OffloadStatus.Kind?
    private var lastAttentionAt: Date?
    private var suspended = false
    /// True from creation, and again after `resume()`, until the first fresh observation. That observation is a
    /// baseline: a continuous state (working) may start from it, but no one-shot is played for something that
    /// was already true at launch or while the Mac slept.
    private var baselining = true

    mutating func observe(_ status: OffloadStatus, context: MotionContext, now: Date) {
        let entering = lastKind != status.kind
        let isBaseline = baselining && status.freshness == .fresh
        defer {
            lastKind = status.kind
            if status.freshness == .fresh { baselining = false }
        }
        guard !suspended else { return }
        switch MotionPolicy.target(for: status, context: context) {
        case .still:
            set(.still, deadline: nil)
        case .working:
            // Valid until `validityWindow` after the data was actually read, not after this call: an unrelated
            // republish of an old snapshot must not extend the motion.
            let end = now.addingTimeInterval(max(0, MotionPolicy.validityWindow - (status.dataAge ?? 0)))
            if phase == .working { deadline = end } // same generation: only the end moves
            else { set(.working, deadline: end) }
        case .loading:
            if phase == .loading { break }
            if entering { set(.loading, deadline: now.addingTimeInterval(MotionPolicy.loadingDuration)) }
            else { set(.still, deadline: nil) } // the hint already ran its course; do not restart it on every poll
        case .attention:
            if phase == .attention { break }
            let cooledDown = lastAttentionAt.map { now.timeIntervalSince($0) >= MotionPolicy.attentionCooldown } ?? true
            if entering && !isBaseline && cooledDown {
                lastAttentionAt = now
                set(.attention, deadline: now.addingTimeInterval(MotionPolicy.attentionDuration))
            } else {
                set(.still, deadline: nil)
            }
        }
    }

    /// Ends a phase whose deadline has passed: a finished one-shot, or working motion whose data was not
    /// confirmed again in time.
    mutating func expire(now: Date) {
        guard let d = deadline, now >= d else { return }
        set(.still, deadline: nil)
    }

    /// Stops everything: the view is hidden, the system sleeps, or the display sleeps.
    mutating func suspend() {
        suspended = true
        set(.still, deadline: nil)
    }

    /// Allows motion again. Nothing that happened while suspended is replayed: the next observation is a baseline.
    mutating func resume() {
        suspended = false
        baselining = true
        lastKind = nil
    }

    private mutating func set(_ p: MotionPhase, deadline d: Date?) {
        if p != phase { phase = p; generation += 1 }
        deadline = d
    }
}

// MARK: - Scheduling

/// Schedules one future call and returns a closure that cancels it. Production sleeps on a task; tests drive a
/// manual clock, so no test waits for real time.
protocol MotionScheduling {
    @MainActor func schedule(after seconds: TimeInterval, _ fire: @escaping @MainActor () -> Void) -> () -> Void
}

struct TaskMotionScheduler: MotionScheduling {
    @MainActor func schedule(after seconds: TimeInterval, _ fire: @escaping @MainActor () -> Void) -> () -> Void {
        let task = Task { @MainActor in
            try? await Task.sleep(nanoseconds: UInt64(max(0.01, seconds) * 1_000_000_000))
            guard !Task.isCancelled else { return }
            fire()
        }
        return { task.cancel() }
    }
}
