import AppKit
import SwiftUI
import XCTest
@testable import OffloadMonitor

final class PaginationTests: XCTestCase {
    func testPageCount() {
        XCTAssertEqual(Pagination.pageCount(total: 0, size: 4), 1)
        XCTAssertEqual(Pagination.pageCount(total: 4, size: 4), 1)
        XCTAssertEqual(Pagination.pageCount(total: 5, size: 4), 2)
        XCTAssertEqual(Pagination.pageCount(total: 9, size: 3), 3)
        XCTAssertEqual(Pagination.pageCount(total: 5, size: 0), 1)
    }

    func testClamp() {
        XCTAssertEqual(Pagination.clamp(-3, total: 9, size: 4), 0)
        XCTAssertEqual(Pagination.clamp(1, total: 9, size: 4), 1)
        XCTAssertEqual(Pagination.clamp(7, total: 9, size: 4), 2)
        XCTAssertEqual(Pagination.clamp(7, total: 0, size: 4), 0, "data shrank to nothing")
        XCTAssertEqual(Pagination.clamp(2, total: 5, size: 4), 1, "data shrank")
    }

    func testSliceReachesEveryItemExactlyOnce() {
        let items = Array(1...10)
        var seen: [Int] = []
        for p in 0..<Pagination.pageCount(total: items.count, size: 4) { seen += Pagination.slice(items, page: p, size: 4) }
        XCTAssertEqual(seen, items)
        XCTAssertEqual(Pagination.slice(items, page: 99, size: 4), [9, 10], "out-of-range page shows the last")
        XCTAssertEqual(Pagination.slice([Int](), page: 0, size: 4), [])
    }

    func testRangeLabel() {
        XCTAssertEqual(Pagination.rangeLabel(page: 0, total: 0, size: 4), "0")
        XCTAssertEqual(Pagination.rangeLabel(page: 0, total: 9, size: 4), "1–4 of 9")
        XCTAssertEqual(Pagination.rangeLabel(page: 2, total: 9, size: 4), "9–9 of 9")
        XCTAssertEqual(Pagination.rangeLabel(page: 9, total: 9, size: 4), "9–9 of 9")
    }
}

/// The main panel must fit its fixed height in every state. These measure the natural (unclipped) height.
@MainActor
final class PanelFitTests: XCTestCase {
    func fitHeight(_ model: MonitorModel, _ updates: UpdateManager) -> CGFloat {
        let view = PopoverView(model: model, updates: updates, isDashboard: false, measuring: true)
        let host = NSHostingView(rootView: view)
        return host.fittingSize.height
    }

    func updates(_ phase: UpdateManager.Phase = .idle) -> UpdateManager {
        let d = UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!
        let u = UpdateManager(prefs: Preferences(defaults: d), consumeReport: false)
        u.setPhaseForTesting(phase)
        return u
    }

    func snapshot(active: Int, recent: Int, groups: Int, warnings: Int, trendPoints: Int = 24) throws -> StatsSnapshot {
        var s = try StatsDecoder.decode(Data(StatsDecodeTests.full.utf8))
        let a = s.active[0]
        s.active = (0..<active).map { var x = a; x.pid = 100 + $0; x.task = String(repeating: "long task text ", count: 10); return x }
        let r = RecentRun(ts: "2026-10-29 18:19:51", status: "ok", dir: "/w/a", task: String(repeating: "task ", count: 40), session: "s", model: "m",
                          costUsd: 0.01, usage: nil, result: String(repeating: "result ", count: 400), salvaged: false)
        s.recent = Array(repeating: r, count: recent)
        let g = s.projects[0]
        s.projects = (0..<groups).map { var x = g; x.key = "k\($0)"; x.name = "project-\($0)-with-a-fairly-long-name"; return x }
        s.models = s.projects
        s.warnings = Array(repeating: "A long coverage warning. " + String(repeating: "more ", count: 40), count: warnings)
        let t = TrendTests.totals(runs: 1, usage: TrendTests.usage, usageRuns: 1, cost: "1.5", costRuns: 1)
        s.trend = try StatsDecoder.decode(TrendTests.doc(trend: """
        {"interval":"hour","label":"Today by hour","points":[\((0..<trendPoints).map { #"{"start":"2026-10-29T\#(String(format: "%02d", $0)):00:00Z","label":"\#($0)","totals":\#(t)}"# }.joined(separator: ","))]}
        """)).trend
        return s
    }

    func testPopulatedWithNoticeFits() throws {
        let m = MonitorModel(testingSnapshot: try snapshot(active: 7, recent: 20, groups: 15, warnings: 4), error: "offload stats failed (exit 2): " + String(repeating: "x", count: 400))
        let h = fitHeight(m, updates(.failed(String(repeating: "update problem ", count: 40))))
        XCTAssertLessThanOrEqual(h, Layout.height, "populated + notices needs \(h)")
        XCTAssertGreaterThan(h, 500)
    }

    func testPopulatedWithoutNoticeFits() throws {
        let m = MonitorModel(testingSnapshot: try snapshot(active: 3, recent: 4, groups: 4, warnings: 0))
        XCTAssertLessThanOrEqual(fitHeight(m, updates()), Layout.height)
    }

    func testEveryTabKeepsTheSameHeight() throws {
        // The list region is fixed, so tab content cannot change the panel height.
        let m = MonitorModel(testingSnapshot: try snapshot(active: 0, recent: 0, groups: 0, warnings: 0))
        let empty = fitHeight(m, updates())
        let full = fitHeight(MonitorModel(testingSnapshot: try snapshot(active: 9, recent: 9, groups: 9, warnings: 1)), updates())
        XCTAssertEqual(empty, full - 0, accuracy: 20) // only the notes link and trend differ
        XCTAssertLessThanOrEqual(full, Layout.height)
    }

    func testEmptyTrendLoadingAndErrorStatesFit() throws {
        var s = try snapshot(active: 0, recent: 0, groups: 0, warnings: 0)
        s.trend = .empty
        XCTAssertLessThanOrEqual(fitHeight(MonitorModel(testingSnapshot: s), updates()), Layout.height)
        XCTAssertLessThanOrEqual(fitHeight(MonitorModel(testingSnapshot: nil, loading: true), updates()), Layout.height)
        XCTAssertLessThanOrEqual(fitHeight(MonitorModel(testingSnapshot: nil, error: "boom " + String(repeating: "x", count: 500)), updates(.available(UpdateCandidate(
            version: SemanticVersion("9.9.9")!, tag: "v9.9.9", assetName: "a", downloadURL: URL(string: "https://github.com/x")!, sha256: "")))), Layout.height)
    }

    func testNoticePriority() {
        let items = Notices.items(error: "e", hasSnapshot: true, isStale: true, phase: .idle, result: nil, currentVersion: "0.3.0")
        XCTAssertEqual(items.first?.title, "Showing stale data")
        XCTAssertTrue(Notices.items(error: nil, hasSnapshot: true, isStale: false, phase: .upToDate(checkedAt: Date()), result: nil, currentVersion: "0.3.0").isEmpty)
    }
}

/// A real report has a ~2000-character task and a ~4000-character result. The report window has a fixed
/// size, so no amount of text may enlarge it, and the result must stay reachable by scrolling.
@MainActor
final class ReportLayoutTests: XCTestCase {
    func run(task: String, result: String) -> RecentRun {
        RecentRun(ts: "2026-10-29 18:19:51", status: "ok", dir: "/w/project", task: task, session: "s", model: "m",
                  costUsd: 0.5, usage: nil, result: result, salvaged: false)
    }

    /// Hosts the report in a window of fixed size and returns it after layout.
    func host(_ r: RecentRun, size: NSSize = NSSize(width: 460, height: 420)) -> (NSWindow, NSHostingView<ReportView>) {
        let view = NSHostingView(rootView: ReportView(run: r))
        let w = NSWindow(contentRect: NSRect(origin: .zero, size: size), styleMask: [.titled], backing: .buffered, defer: false)
        w.contentView = view
        view.layoutSubtreeIfNeeded()
        return (w, view)
    }

    func scrollViews(in view: NSView) -> [NSScrollView] {
        var found: [NSScrollView] = []
        func walk(_ v: NSView) { if let s = v as? NSScrollView { found.append(s) }; v.subviews.forEach(walk) }
        walk(view)
        return found
    }

    func testHugeTaskDoesNotGrowFixedWindowAndResultStaysInScrollRegion() {
        let huge = run(task: String(repeating: "Implement the trend chart with exact buckets. ", count: 45), // ~2000 chars
                       result: String(repeating: "Result line with details. ", count: 150)) // ~4000 chars
        let small = run(task: "short", result: "short")
        let (_, hugeView) = host(huge)
        let (_, smallView) = host(small)

        // The content view keeps the window's size whatever the text length.
        XCTAssertEqual(hugeView.frame.size.height, 420, accuracy: 0.5)
        XCTAssertEqual(hugeView.frame.size, smallView.frame.size)

        // The scroll region fills the space under the header and is not squeezed away by the task text.
        let scrolls = scrollViews(in: hugeView)
        XCTAssertEqual(scrolls.count, 1, "task, metadata and result share one scroll view")
        let scroll = scrolls[0]
        XCTAssertGreaterThan(scroll.frame.height, 420 - 80, "scroll region fills the window below the sticky header")
        XCTAssertFalse(scroll.hasVerticalScroller && !scroll.autohidesScrollers && scroll.scrollerStyle == .legacy, "indicators hidden")

        // The document is taller than the window (content is reachable by scrolling), not clipped to it.
        let doc = scroll.documentView?.frame.height ?? 0
        XCTAssertGreaterThan(doc, scroll.frame.height * 1.5, "long text extends the scrollable document, not the window")
    }

    func testShortReportNeedsNoScrolling() {
        let (_, v) = host(run(task: "short", result: "short"))
        let scroll = scrollViews(in: v).first
        XCTAssertNotNil(scroll)
        XCTAssertLessThanOrEqual(scroll!.documentView?.frame.height ?? 0, scroll!.frame.height + 1)
    }
}
