import Foundation

/// The single, pure description of what Offload is doing, shared by the menu-bar label and the panel header.
///
/// Rules that the reducer enforces:
/// - No snapshot means the counts are unknown (`counts == nil`), never zero.
/// - A queued task is waiting for a worker slot. It is never "stalled" and never counted as running.
/// - Only an explicit signal needs attention: a lost or unresolved task, or a running/salvaging task that
///   the CLI marked stalled. A past failed run (history) is not a current problem.
/// - A failed refresh keeps the old snapshot. It is labelled stale once it is older than `staleAfter`,
///   and stale data is never presented as idle or as a live count.
/// - There is no "disabled" state: the stats output has no data source for it.
struct OffloadStatus: Equatable {
    enum Kind: Equatable {
        /// No snapshot yet and no error: the first fetch is pending.
        case unknown
        /// No snapshot and the fetch failed.
        case unavailable
        case idle
        case queued
        case working
        case attention
        /// A snapshot exists, but refreshing has been failing for longer than `staleAfter`.
        case stale
    }

    enum Freshness: Equatable {
        case fresh
        /// The latest refresh failed, but the data is still recent.
        case refreshFailed
        case stale
        case unknown
    }

    struct Counts: Equatable {
        /// Workers in the "running" phase.
        var running = 0
        var salvaging = 0
        var cleaning = 0
        var queued = 0
        var lost = 0
        var unresolved = 0
        /// Running or salvaging workers that the CLI marked stalled. These are also counted in `running`/`salvaging`.
        var stalled = 0

        /// Workers doing or finishing work: running, salvaging and cleaning.
        var working: Int { running + salvaging + cleaning }
        /// Tasks that need a person to look. Disjoint from each other, so the sum counts each task once.
        var attention: Int { lost + unresolved + stalled }
    }

    var kind: Kind
    var freshness: Freshness
    /// nil when there is no snapshot.
    var counts: Counts?
    /// Seconds since the last successful refresh, when known.
    var dataAge: TimeInterval?

    static let staleDefault: TimeInterval = 20

    static func reduce(snapshot: StatsSnapshot?, lastError: String?, lastSuccess: Date?, now: Date,
                       staleAfter: TimeInterval) -> OffloadStatus {
        let age = lastSuccess.map { max(0, now.timeIntervalSince($0)) }
        guard let snapshot else {
            return OffloadStatus(kind: lastError == nil ? .unknown : .unavailable,
                                 freshness: .unknown, counts: nil, dataAge: nil)
        }
        var c = Counts()
        for t in snapshot.active {
            switch t.phase {
            case "running": c.running += 1; if t.stalled { c.stalled += 1 }
            case "salvaging": c.salvaging += 1; if t.stalled { c.stalled += 1 }
            case "cleaning": c.cleaning += 1
            case "queued": c.queued += 1
            case "lost": c.lost += 1
            case "unresolved": c.unresolved += 1
            default: break
            }
        }
        let freshness: Freshness
        if lastError == nil {
            freshness = .fresh
        } else if let age, age > staleAfter {
            freshness = .stale
        } else {
            freshness = .refreshFailed
        }
        let kind: Kind
        if freshness == .stale { kind = .stale }
        else if c.attention > 0 { kind = .attention }
        else if c.working > 0 { kind = .working }
        else if c.queued > 0 { kind = .queued }
        else { kind = .idle }
        return OffloadStatus(kind: kind, freshness: freshness, counts: c, dataAge: age)
    }
}

// MARK: - Presentation text (pure, so it can be tested without a view)

extension OffloadStatus {
    /// The widest text the menu-bar count slot has to hold.
    static let badgeCap = 99

    /// "0"..."99", then "99+". Negative values do not occur; they render as unknown.
    static func badge(_ n: Int) -> String {
        if n < 0 { return "–" }
        return n > badgeCap ? "\(badgeCap)+" : String(n)
    }

    /// The number shown next to the mark: workers when any exist, otherwise queued tasks. "–" means unknown
    /// (no data, or stale data whose count would be misleading); it is never a zero.
    var menuBarCountText: String {
        guard let c = counts, kind != .stale else { return "–" }
        return Self.badge(c.working > 0 ? c.working : c.queued)
    }

    /// SF Symbol for the state, or nil for idle (the slot stays reserved and empty). The shape differs per
    /// state, so color is never needed to tell them apart.
    var symbol: String? {
        switch kind {
        case .unknown: return "ellipsis"
        case .unavailable: return "exclamationmark.octagon"
        case .idle: return nil
        case .queued: return "hourglass"
        case .working: return "arrow.triangle.2.circlepath"
        case .attention: return "exclamationmark.triangle.fill"
        case .stale: return "clock.badge.exclamationmark"
        }
    }

    var title: String {
        switch kind {
        case .unknown: return "Loading"
        case .unavailable: return "Unavailable"
        case .idle: return "Idle"
        case .queued: return "Queued"
        case .working: return "Working"
        case .attention: return "Needs attention"
        case .stale: return "Stale"
        }
    }

    /// Count phrases in a fixed order: work first, then waiting, then problems.
    func countPhrases() -> [String] {
        guard let c = counts else { return [] }
        var p: [String] = []
        let running = c.running + c.salvaging
        if running > 0 { p.append("\(running) running") }
        if c.cleaning > 0 { p.append("\(c.cleaning) cleaning up") }
        if c.queued > 0 { p.append("\(c.queued) queued") }
        if c.stalled > 0 { p.append("\(c.stalled) stalled") }
        if c.lost > 0 { p.append("\(c.lost) lost") }
        if c.unresolved > 0 { p.append("\(c.unresolved) unresolved") }
        return p
    }

    static func ageText(_ seconds: TimeInterval) -> String {
        let s = Int(seconds.rounded())
        func unit(_ n: Int, _ w: String) -> String { "\(n) \(w)\(n == 1 ? "" : "s")" }
        if s < 60 { return unit(s, "second") }
        if s < 3600 { return unit(s / 60, "minute") }
        return unit(s / 3600, "hour")
    }

    /// The secondary line of the header.
    var detail: String {
        let phrases = countPhrases()
        let list = phrases.joined(separator: " · ")
        switch kind {
        case .unknown: return "Reading status…"
        case .unavailable: return "Cannot read stats"
        case .idle: return refreshFailedSuffix("No active tasks")
        case .queued, .working, .attention: return refreshFailedSuffix(list)
        case .stale:
            let known = phrases.isEmpty ? "no active tasks" : list
            let since = dataAge.map { " as of \(Self.ageText($0)) ago" } ?? ""
            return "Last known: \(known)\(since)"
        }
    }

    private func refreshFailedSuffix(_ s: String) -> String {
        freshness == .refreshFailed ? s + " · refresh failed" : s
    }

    /// Spoken value for VoiceOver and the hover tooltip. Carries every count and the data freshness in words.
    var accessibilityValue: String {
        var parts: [String] = []
        switch kind {
        case .unknown: return "Loading status. Task counts are not known yet."
        case .unavailable: return "Status unavailable. Offload could not read stats, so task counts are not known."
        case .idle: parts.append("Idle. No active tasks")
        case .queued: parts.append("Queued. " + countPhrases().joined(separator: ", "))
        case .working: parts.append("Working. " + countPhrases().joined(separator: ", "))
        case .attention:
            let c = counts ?? Counts()
            parts.append("Needs attention, \(c.attention) \(c.attention == 1 ? "task" : "tasks"). " + countPhrases().joined(separator: ", "))
        case .stale:
            let l = countPhrases().joined(separator: ", ")
            parts.append("Stale. Last known: \(l.isEmpty ? "no active tasks" : l)")
        }
        switch freshness {
        case .fresh: parts.append("Data is up to date")
        case .refreshFailed: parts.append("The latest refresh failed; showing recent data")
        case .stale: parts.append("The data is stale" + (dataAge.map { ", last updated \(Self.ageText($0)) ago" } ?? ""))
        case .unknown: break
        }
        return parts.joined(separator: ". ") + "."
    }
}

extension MonitorModel {
    func status(at now: Date) -> OffloadStatus {
        OffloadStatus.reduce(snapshot: snapshot, lastError: lastError, lastSuccess: lastSuccess, now: now, staleAfter: Self.staleAfter)
    }
}
