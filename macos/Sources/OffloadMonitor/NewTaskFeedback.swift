import Foundation
import SwiftUI

// Phase 1 of "newly observed task" feedback (the +1 / +N hint). Everything in this file is pure
// logic with no UI and no clock of its own, so it is tested headlessly.
//
// Wording: the hint means "新观察到的任务" (a task this app instance saw for the first time). It does
// NOT mean "a new run was started". A task that starts and finishes between two polls (about 5 s)
// is never present in any snapshot and is never reported. See docs/new-task-feedback.md.

/// What one snapshot added, as counts only. It carries no task text, directory, session or model,
/// so nothing sensitive can reach an animation or an accessibility announcement.
struct NewTaskEvent: Equatable {
    /// Identities seen for the first time in queued, running, salvaging or cleaning.
    var newCount: Int
    /// Identities seen for the first time already in "lost". Advisory only: never shown as +N.
    var lostCount: Int
}

/// Decides which active tasks are new, by identity (`ActiveTask.id` = pid + startedAt).
///
/// Scope of the de-duplication: one app process. The first successful snapshot, and the first one
/// after a long gap, only establish a baseline. Nothing is persisted, so a restart baselines again
/// and never replays history.
struct ObservedTaskTracker {
    static let defaultCapacity = 256
    /// A gap between two successful observations longer than this (sleep, long outage) re-baselines.
    /// Tasks that appeared during the gap are absorbed silently instead of announced late.
    static let maxGap: TimeInterval = 120

    private static let alertPhases: Set<String> = ["queued", "running", "salvaging", "cleaning"]

    private(set) var hasBaseline = false
    private var seen = Set<String>()
    private var order: [String] = []
    private var lastObservedAt: Date?
    /// Latest startedAt among identities dropped by the retention bound. An identity that is not in
    /// `seen` but started at or before this is treated as old, so a recycled entry cannot replay.
    private var watermark: Date?
    private let capacity: Int

    init(capacity: Int = ObservedTaskTracker.defaultCapacity) { self.capacity = max(1, capacity) }

    var seenCount: Int { seen.count }

    /// Feed one successful snapshot's `active` list. Failed refreshes must not call this: the
    /// baseline and the seen set simply stay as they are.
    mutating func observe(_ active: [ActiveTask], at now: Date) -> NewTaskEvent? {
        defer { lastObservedAt = now; trim(keeping: Set(active.map(\.id))) }

        let gapExceeded = lastObservedAt.map { now.timeIntervalSince($0) > Self.maxGap } ?? false
        let silent = !hasBaseline || gapExceeded
        hasBaseline = true

        var newCount = 0, lostCount = 0
        for task in active {
            guard remember(task.id) else { continue }
            if silent { continue }
            if let w = watermark, let started = Format.parseTimestamp(task.startedAt), started <= w { continue }
            if task.isLost {
                lostCount += 1
            } else if Self.alertPhases.contains(task.phase) {
                newCount += 1
            }
        }
        return newCount + lostCount > 0 ? NewTaskEvent(newCount: newCount, lostCount: lostCount) : nil
    }

    /// Returns true when the identity was not known before.
    private mutating func remember(_ id: String) -> Bool {
        guard seen.insert(id).inserted else { return false }
        order.append(id)
        return true
    }

    /// Drops the oldest identities beyond the capacity, never one that is in the current snapshot.
    private mutating func trim(keeping current: Set<String>) {
        guard seen.count > capacity else { return }
        var kept: [String] = []
        var excess = seen.count - capacity
        for id in order {
            if excess > 0, !current.contains(id) {
                seen.remove(id)
                excess -= 1
                if let started = Self.startedAt(of: id) { watermark = max(watermark ?? started, started) }
            } else {
                kept.append(id)
            }
        }
        order = kept
    }

    /// `ActiveTask.id` is "pid|startedAt".
    private static func startedAt(of id: String) -> Date? {
        guard let bar = id.firstIndex(of: "|") else { return nil }
        return Format.parseTimestamp(String(id[id.index(after: bar)...]))
    }
}

/// The short-lived "+N" shown in the menu bar and in the panel header.
struct NewTaskPulse: Equatable, Identifiable {
    var id: Int
    var count: Int
    var firstShownAt: Date
    var expiresAt: Date

    /// Fixed-width text so the reserved slot never changes size: "+1" ... "+9", then "+9+".
    var text: String { count > 9 ? "+9+" : "+\(count)" }
    var accessibilityText: String { "\(count) newly observed task\(count == 1 ? "" : "s")" }
    func isExpired(at now: Date) -> Bool { expiresAt <= now }
}

/// Burst handling: events that arrive while a pulse is visible merge into it. Showing time is
/// extended by one `duration`, but never beyond `maxTotal` after the first event, so a burst cannot
/// build a queue of animations.
enum NewTaskPulsePolicy {
    static let duration: TimeInterval = 0.8     // within the 600-900 ms target; calibrate in a native preview
    static let maxTotal: TimeInterval = 1.8

    static func merge(_ current: NewTaskPulse?, newCount: Int, nextID: Int, at now: Date) -> NewTaskPulse {
        if let c = current, !c.isExpired(at: now) {
            let cap = c.firstShownAt.addingTimeInterval(maxTotal)
            return NewTaskPulse(id: c.id, count: c.count + newCount, firstShownAt: c.firstShownAt,
                                expiresAt: min(now.addingTimeInterval(duration), cap))
        }
        return NewTaskPulse(id: nextID, count: newCount, firstShownAt: now, expiresAt: now.addingTimeInterval(duration))
    }
}

// MARK: - Views

/// A fixed-width slot that shows "+N" for the pulse and nothing otherwise. The slot is always laid
/// out at the same width, so neither the menu bar item nor the panel header changes size when the
/// hint appears. Reduce Motion drops the upward movement and keeps a short fade.
struct NewTaskPulseBadge: View {
    var pulse: NewTaskPulse?
    var fontSize: CGFloat = 10
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        ZStack {
            Text("+9+").hidden()   // sizes the slot to the widest text
            if let p = pulse {
                Text(p.text)
                    .foregroundStyle(Color.omTeal)
                    .transition(reduceMotion ? .opacity : .opacity.combined(with: .offset(y: -3)))
                    .id(p.id)
                    .accessibilityLabel(p.accessibilityText)
                    .help("Newly observed task (新观察到的任务)")
            }
        }
        .font(.system(size: fontSize, weight: .semibold).monospacedDigit())
        .lineLimit(1)
        .fixedSize()
        .clipped()
        .animation(reduceMotion ? nil : .easeOut(duration: 0.22), value: pulse?.id)
        .animation(reduceMotion ? nil : .easeOut(duration: 0.22), value: pulse?.count)
        .accessibilityElement(children: .contain)
    }
}
