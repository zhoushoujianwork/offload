import AppKit
import SwiftUI
import XCTest
@testable import OffloadMonitor

final class StatusReducerTests: XCTestCase {
    let now = Date(timeIntervalSince1970: 1_000_000)

    func task(_ phase: String, stalled: Bool = false, pid: Int = 1) -> ActiveTask {
        ActiveTask(pid: pid, phase: phase, status: nil, exitCode: nil, dir: "/w/a", task: "t", profile: "p", model: "m", session: nil,
                   startedAt: "2026-10-29T18:00:00Z", updatedAt: "2026-10-29T18:00:00Z", lastEventAt: "2026-10-29T18:00:00Z",
                   toolCalls: 0, permissionDenials: 0, lastTool: nil, stalled: stalled)
    }

    func snapshot(_ active: [ActiveTask], failedRuns: Bool = false) throws -> StatsSnapshot {
        var s = try StatsDecoder.decode(Data(StatsDecodeTests.empty.utf8))
        s.active = active.enumerated().map { var t = $1; t.pid = 100 + $0; return t }
        if failedRuns {
            s.totals.runs = 5; s.totals.error = 5
            s.recent = [RecentRun(ts: "2026-10-29 18:19:51", status: "error", dir: "/w/a", task: "x", session: "s", model: "m",
                                  costUsd: nil, usage: nil, result: "failed", salvaged: false)]
        }
        return s
    }

    func reduce(_ s: StatsSnapshot?, error: String? = nil, age: TimeInterval = 1) -> OffloadStatus {
        OffloadStatus.reduce(snapshot: s, lastError: error, lastSuccess: s == nil ? nil : now.addingTimeInterval(-age), now: now, staleAfter: 20)
    }

    func testInitialLoadIsUnknownNotZero() {
        let st = reduce(nil)
        XCTAssertEqual(st.kind, .unknown)
        XCTAssertNil(st.counts, "unknown counts must not be a fake zero")
        XCTAssertEqual(st.menuBarCountText, "–")
        XCTAssertNotEqual(st.title, "Idle")
        XCTAssertNil(st.dataAge)
    }

    func testFetchFailureWithoutSnapshotIsUnavailableNotIdle() {
        let st = reduce(nil, error: "boom")
        XCTAssertEqual(st.kind, .unavailable)
        XCTAssertNil(st.counts)
        XCTAssertEqual(st.menuBarCountText, "–")
        XCTAssertTrue(st.accessibilityValue.contains("not known"))
    }

    func testEmptySnapshotIsIdleWithRealZero() throws {
        let st = reduce(try snapshot([]))
        XCTAssertEqual(st.kind, .idle)
        XCTAssertEqual(st.counts, OffloadStatus.Counts())
        XCTAssertEqual(st.menuBarCountText, "0")
        XCTAssertNil(st.symbol)
        XCTAssertEqual(st.freshness, .fresh)
    }

    func testMixedQueuedAndRunningShowsBothAccurately() throws {
        let st = reduce(try snapshot([task("running"), task("running"), task("queued"), task("queued"), task("queued")]))
        XCTAssertEqual(st.kind, .working)
        XCTAssertEqual(st.counts?.running, 2)
        XCTAssertEqual(st.counts?.queued, 3)
        XCTAssertEqual(st.menuBarCountText, "2")
        XCTAssertEqual(st.detail, "2 running · 3 queued")
        XCTAssertTrue(st.accessibilityValue.contains("2 running, 3 queued"))
    }

    func testOnlyQueuedIsQueuedAndShowsQueueLength() throws {
        let st = reduce(try snapshot([task("queued"), task("queued")]))
        XCTAssertEqual(st.kind, .queued)
        XCTAssertEqual(st.menuBarCountText, "2")
        XCTAssertEqual(st.detail, "2 queued")
    }

    func testQueuedIsNeverStalledEvenIfTheCLIMarksItSilent() throws {
        // The CLI sets stalled for any active worker that has been silent; a queued run has no backend yet.
        let st = reduce(try snapshot([task("queued", stalled: true)]))
        XCTAssertEqual(st.kind, .queued)
        XCTAssertEqual(st.counts?.stalled, 0)
        XCTAssertEqual(st.counts?.attention, 0)
        XCTAssertFalse(st.accessibilityValue.contains("stalled"))
    }

    func testCleaningIsWorkingAndKeepsItsDetail() throws {
        let st = reduce(try snapshot([task("cleaning", stalled: true)]))
        XCTAssertEqual(st.kind, .working)
        XCTAssertEqual(st.counts?.cleaning, 1)
        XCTAssertEqual(st.counts?.running, 0)
        XCTAssertEqual(st.counts?.stalled, 0, "cleaning is not a model stall")
        XCTAssertEqual(st.menuBarCountText, "1")
        XCTAssertEqual(st.detail, "1 cleaning up")
    }

    func testSalvagingCountsAsRunning() throws {
        let st = reduce(try snapshot([task("salvaging")]))
        XCTAssertEqual(st.kind, .working)
        XCTAssertEqual(st.detail, "1 running")
    }

    func testLostNeedsAttentionAndIsNotWorking() throws {
        let st = reduce(try snapshot([task("lost")]))
        XCTAssertEqual(st.kind, .attention)
        XCTAssertEqual(st.counts?.lost, 1)
        XCTAssertEqual(st.counts?.working, 0)
        XCTAssertEqual(st.menuBarCountText, "0", "a lost task is not running work")
        XCTAssertEqual(st.detail, "1 lost")
        XCTAssertTrue(st.accessibilityValue.hasPrefix("Needs attention, 1 task."))
    }

    func testStalledRunningNeedsAttentionAndStaysCountedAsRunning() throws {
        let st = reduce(try snapshot([task("running", stalled: true), task("running"), task("queued")]))
        XCTAssertEqual(st.kind, .attention)
        XCTAssertEqual(st.counts?.running, 2)
        XCTAssertEqual(st.counts?.stalled, 1)
        XCTAssertEqual(st.detail, "2 running · 1 queued · 1 stalled")
    }

    func testUnresolvedNeedsAttention() throws {
        let st = reduce(try snapshot([task("unresolved")]))
        XCTAssertEqual(st.kind, .attention)
        XCTAssertEqual(st.counts?.unresolved, 1)
    }

    func testHistoricalFailuresAreNotACurrentProblem() throws {
        let st = reduce(try snapshot([], failedRuns: true))
        XCTAssertEqual(st.kind, .idle)
        XCTAssertEqual(st.counts?.attention, 0)
    }

    func testFailedRefreshWithRecentSnapshotKeepsCountsAndSaysSo() throws {
        let st = reduce(try snapshot([task("running")]), error: "boom", age: 5)
        XCTAssertEqual(st.kind, .working)
        XCTAssertEqual(st.freshness, .refreshFailed)
        XCTAssertEqual(st.menuBarCountText, "1")
        XCTAssertEqual(st.detail, "1 running · refresh failed")
        XCTAssertTrue(st.accessibilityValue.contains("latest refresh failed"))
    }

    func testOldSnapshotAfterFailureIsStaleAndHidesTheLiveCount() throws {
        let st = reduce(try snapshot([task("running"), task("queued")]), error: "boom", age: 45)
        XCTAssertEqual(st.kind, .stale)
        XCTAssertEqual(st.freshness, .stale)
        XCTAssertEqual(st.menuBarCountText, "–", "stale numbers must not look live")
        XCTAssertEqual(st.detail, "Last known: 1 running · 1 queued as of 45 seconds ago")
        XCTAssertTrue(st.accessibilityValue.contains("stale"))
        XCTAssertTrue(st.accessibilityValue.contains("1 running, 1 queued"))
    }

    func testStaleEmptySnapshotIsNotPresentedAsIdle() throws {
        let st = reduce(try snapshot([]), error: "boom", age: 3600)
        XCTAssertEqual(st.kind, .stale)
        XCTAssertNotEqual(st.title, "Idle")
        XCTAssertEqual(st.detail, "Last known: no active tasks as of 1 hour ago")
    }

    func testStaleBoundary() throws {
        XCTAssertEqual(reduce(try snapshot([]), error: "e", age: 20).kind, .idle, "exactly at the limit is not yet stale")
        XCTAssertEqual(reduce(try snapshot([]), error: "e", age: 21).kind, .stale)
        XCTAssertEqual(reduce(try snapshot([]), error: nil, age: 500).kind, .idle, "no error means fresh however old the timestamp")
    }

    func testSuccessAfterFailureRecoversFully() throws {
        let st = reduce(try snapshot([task("running")]), error: nil, age: 0)
        XCTAssertEqual(st.freshness, .fresh)
        XCTAssertEqual(st.kind, .working)
    }

    func testUnknownPhaseIsIgnoredRatherThanCounted() throws {
        let st = reduce(try snapshot([task("done")]))
        XCTAssertEqual(st.kind, .idle)
    }

    func testStateShapesDifferSoColorIsNotRequired() {
        let kinds: [OffloadStatus.Kind] = [.unknown, .unavailable, .queued, .working, .attention, .stale]
        let symbols = kinds.map { OffloadStatus(kind: $0, freshness: .fresh, counts: nil, dataAge: nil).symbol }
        XCTAssertEqual(Set(symbols.compactMap { $0 }).count, kinds.count)
        XCTAssertEqual(Set(kinds.map { OffloadStatus(kind: $0, freshness: .fresh, counts: nil, dataAge: nil).title }).count, kinds.count)
    }

    func testBadgeText() {
        XCTAssertEqual([0, 1, 9, 10, 99, 100, 1234].map(OffloadStatus.badge), ["0", "1", "9", "10", "99", "99+", "99+"])
    }

    func testAgeText() {
        XCTAssertEqual(OffloadStatus.ageText(1), "1 second")
        XCTAssertEqual(OffloadStatus.ageText(59), "59 seconds")
        XCTAssertEqual(OffloadStatus.ageText(60), "1 minute")
        XCTAssertEqual(OffloadStatus.ageText(7200), "2 hours")
    }

    func testModelExposesTheReducer() throws {
        let m = MainActor.assumeIsolated { MonitorModel(testingSnapshot: nil, loading: true) }
        MainActor.assumeIsolated { XCTAssertEqual(m.status(at: Date()).kind, .unknown) }
    }
}

/// Headless size checks only. They do not replace verification in the real system menu bar.
@MainActor
final class StatusLayoutTests: XCTestCase {
    func status(count n: Int) -> OffloadStatus {
        var c = OffloadStatus.Counts(); c.running = n
        return OffloadStatus(kind: .working, freshness: .fresh, counts: c, dataAge: 0)
    }

    func labelWidth(_ model: MonitorModel) -> CGFloat {
        let u = UpdateManager(prefs: Preferences(defaults: UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!), consumeReport: false)
        return NSHostingView(rootView: MenuBarLabel(model: model, updates: u, motion: MotionController(monitor: model, prefs: Preferences(defaults: UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!), scheduler: ManualScheduler(), observeSystem: false).menuBar)).fittingSize.width
    }

    func testMenuBarLabelWidthIsIdenticalForEveryStateAndCount() throws {
        var widths = Set<CGFloat>()
        var base = try StatsDecoder.decode(Data(StatsDecodeTests.empty.utf8))
        let template = try StatsDecoder.decode(Data(StatsDecodeTests.full.utf8)).active[0]
        for n in [0, 1, 9, 10, 99, 100, 150] {
            base.active = (0..<n).map { var t = template; t.pid = 500 + $0; t.stalled = false; return t }
            widths.insert(labelWidth(MonitorModel(testingSnapshot: base)))
        }
        widths.insert(labelWidth(MonitorModel(testingSnapshot: nil, loading: true)))
        widths.insert(labelWidth(MonitorModel(testingSnapshot: nil, error: "boom")))
        widths.insert(labelWidth(MonitorModel(testingSnapshot: try StatsDecoder.decode(Data(StatsDecodeTests.full.utf8)), error: "boom")))
        XCTAssertEqual(widths.count, 1, "label width changed between states: \(widths.sorted())")
        XCTAssertEqual(widths.first ?? 0, MenuBarLabel.Metrics.totalWidth, accuracy: 0.5)
    }

    func testCountSlotFitsTheWidestText() {
        let font = NSFont.menuBarFont(ofSize: 0)
        let mono = NSFont.monospacedDigitSystemFont(ofSize: font.pointSize, weight: .regular)
        for t in ["0", "9", "10", "99", "99+", "–"] {
            let w = (t as NSString).size(withAttributes: [.font: mono]).width
            XCTAssertLessThanOrEqual(w, MenuBarLabel.Metrics.countSlot, "\(t) is clipped by the count slot")
        }
    }

    func testHeaderFitsEveryStateOnOneLinePerElement() throws {
        let full = try StatsDecoder.decode(Data(StatsDecodeTests.full.utf8))
        let models = [MonitorModel(testingSnapshot: nil, loading: true), MonitorModel(testingSnapshot: nil, error: "x"),
                      MonitorModel(testingSnapshot: full), MonitorModel(testingSnapshot: full, error: "x")]
        for m in models {
            let host = NSHostingView(rootView: StatusSummary(model: m, motion: MotionController(monitor: m, prefs: Preferences(defaults: UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!), scheduler: ManualScheduler(), observeSystem: false).header))
            XCTAssertEqual(host.fittingSize.width, StatusSummary.textWidth, accuracy: 0.5)
            XCTAssertLessThanOrEqual(host.fittingSize.height, 40)
        }
    }

    func testLongestDetailFitsTheTextColumn() {
        var c = OffloadStatus.Counts(); c.running = 99; c.queued = 99; c.stalled = 99; c.lost = 99
        let st = OffloadStatus(kind: .attention, freshness: .refreshFailed, counts: c, dataAge: 1)
        let w = (st.detail as NSString).size(withAttributes: [.font: NSFont.systemFont(ofSize: 10)]).width
        // minimumScaleFactor 0.8 is the only slack; beyond it the text truncates, which tooltip and VoiceOver cover.
        XCTAssertGreaterThan(w, 0)
        XCTAssertTrue(st.accessibilityValue.contains("99 lost"))
    }
}
