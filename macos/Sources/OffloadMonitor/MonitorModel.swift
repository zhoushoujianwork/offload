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

    private func fetchOnce() async {
        guard let client else { return }
        inFlight = true
        defer { inFlight = false }
        let asked = period
        if snapshot == nil { isLoading = true }
        do {
            let result = try await client.fetch(period: asked)
            guard asked == period else { wakeRequested = true; return } // period changed meanwhile
            snapshot = result
            lastSuccess = Date()
            lastError = nil
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
    var runningCount: Int? { snapshot?.processingCount }
}
