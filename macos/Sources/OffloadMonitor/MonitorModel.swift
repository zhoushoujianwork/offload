import AppKit
import Foundation
import SwiftUI

@MainActor
final class MonitorModel: ObservableObject {
    static let pollInterval: TimeInterval = 5
    /// Data older than this, with the latest refresh failing, is labeled stale.
    static let staleAfter: TimeInterval = 20

    @Published private(set) var snapshot: StatsSnapshot?
    @Published private(set) var lastSuccess: Date?
    @Published private(set) var lastError: String?
    @Published private(set) var isLoading = false
    /// The current "+N" hint for newly observed tasks, nil when none is showing.
    @Published private(set) var pulse: NewTaskPulse?
    @Published var period: StatsPeriod {
        didSet {
            guard period != oldValue else { return }
            prefs.period = period
            snapshot = nil
            lastSuccess = nil
            lastError = nil
            refreshNow()
        }
    }

    private let prefs: Preferences
    private let client: StatsClient?
    private var loop: Task<Void, Never>?
    private var sleeper: Task<Void, Never>?
    private var inFlight = false
    private var wakeRequested = false
    /// Lives as long as the model, so period switches and failed refreshes keep the baseline.
    private var tracker = ObservedTaskTracker()
    private var pulseCounter = 0
    private var pulseExpiry: Task<Void, Never>?
    /// Spoken change notice for VoiceOver. The default posts an announcement only when an app exists.
    var announce: (String) -> Void = { text in
        guard NSApp != nil else { return }
        NSAccessibility.post(element: NSApp as Any, notification: .announcementRequested,
                             userInfo: [.announcement: text, .priority: NSAccessibilityPriorityLevel.medium.rawValue])
    }

    init(prefs: Preferences = Preferences(), client: StatsClient? = nil) {
        self.prefs = prefs
        self.period = prefs.period
        self.client = client ?? StatsClient.locateCLI().map { StatsClient(executable: $0) }
        if self.client == nil { lastError = StatsError.cliNotFound.errorDescription }
        start()
    }

    /// Headless layout tests: shows the given state and never starts polling.
    init(testingSnapshot: StatsSnapshot?, error: String? = nil, loading: Bool = false) {
        self.prefs = Preferences(defaults: UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!)
        self.client = nil
        self.period = .today
        self.snapshot = testingSnapshot
        self.lastSuccess = testingSnapshot == nil ? nil : Date()
        self.lastError = error
        self.isLoading = loading
    }

    /// Headless tests only.
    func setSnapshotForTesting(_ s: StatsSnapshot?) { snapshot = s }

    var hasCLI: Bool { client != nil }

    func start() {
        guard loop == nil, client != nil else { return }
        loop = Task { [weak self] in
            while !Task.isCancelled {
                guard let self else { return }
                await self.fetchOnce()
                if self.wakeRequested { self.wakeRequested = false; continue }
                let s = Task<Void, Never> { try? await Task.sleep(nanoseconds: UInt64(Self.pollInterval * 1_000_000_000)) }
                self.sleeper = s
                await s.value
            }
        }
    }

    /// Triggers an immediate refresh. Never starts a second process while one is running.
    func refreshNow() {
        if inFlight { wakeRequested = true } else { sleeper?.cancel() }
    }

    /// Applies one successful snapshot. Only `active` can produce a "new task" hint; totals, recent
    /// runs, tokens and cost never do.
    func ingest(_ result: StatsSnapshot, at now: Date) {
        snapshot = result
        lastSuccess = now
        lastError = nil
        if let p = pulse, p.isExpired(at: now) { pulse = nil }   // e.g. after sleep
        if let event = tracker.observe(result.active, at: now), event.newCount > 0 {
            showPulse(newCount: event.newCount, at: now)
        }
    }

    private func showPulse(newCount: Int, at now: Date) {
        if pulse == nil || pulse!.isExpired(at: now) { pulseCounter += 1 }
        let merged = NewTaskPulsePolicy.merge(pulse, newCount: newCount, nextID: pulseCounter, at: now)
        pulse = merged
        announce(merged.accessibilityText)
        pulseExpiry?.cancel()
        let wait = max(0, merged.expiresAt.timeIntervalSince(now))
        pulseExpiry = Task { [weak self] in
            try? await Task.sleep(nanoseconds: UInt64(wait * 1_000_000_000))
            guard !Task.isCancelled else { return }
            self?.pulse = nil
        }
    }

    private func fetchOnce() async {
        guard let client else { return }
        inFlight = true
        defer { inFlight = false }
        let asked = period
        if snapshot == nil { isLoading = true }
        do {
            let result = try await client.fetch(period: asked)
            guard asked == period else { wakeRequested = true; return } // period changed meanwhile
            ingest(result, at: Date())
        } catch {
            guard asked == period else { wakeRequested = true; return }
            lastError = (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
        }
        isLoading = false
    }

    // MARK: Derived state for the menu bar

    func isStale(at now: Date) -> Bool {
        guard lastError != nil, let t = lastSuccess else { return false }
        return now.timeIntervalSince(t) > Self.staleAfter
    }

    var hasProblem: Bool { lastError != nil }
    var needsAttention: Bool {
        guard let s = snapshot else { return false }
        return s.stalledCount > 0 || s.lostCount > 0
    }
    var runningCount: Int? { snapshot?.runningCount }
}
