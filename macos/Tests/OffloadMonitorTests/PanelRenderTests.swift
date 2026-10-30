import AppKit
import SwiftUI
import XCTest
@testable import OffloadMonitor

/// Layout invariants for the monitor panel and the menu-bar label. The checks work on measured sizes, so
/// they can be exercised on a deliberately broken layout (see `PanelRenderInvariantTests`).
enum PanelInvariants {
    /// The panel must be exactly the configured width, and its natural (unclipped) height must fit the fixed height.
    static func panelViolations(_ state: String, natural: CGSize) -> [String] {
        var out: [String] = []
        if abs(natural.width - Layout.width) > 0.5 {
            out.append("\(state): panel width \(natural.width) is not the configured \(Layout.width)")
        }
        if natural.height > Layout.height + 0.5 {
            out.append("\(state): content needs \(natural.height) pt and is clipped by the fixed height \(Layout.height)")
        }
        return out
    }

    /// The menu-bar label must keep its fixed width.
    static func labelViolations(_ state: String, width: CGFloat) -> [String] {
        abs(width - MenuBarLabel.Metrics.totalWidth) > 0.5
            ? ["\(state): menu-bar label width \(width) is not the fixed \(MenuBarLabel.Metrics.totalWidth)"] : []
    }
}

/// Synthetic data only: every path, name, task and number below is made up for these tests.
@MainActor
final class PanelRenderTests: XCTestCase {
    struct State {
        let name: String
        let model: MonitorModel
        var update: UpdateManager.Phase = .idle
        var profile = "work"
    }

    private static let template: ActiveTask = {
        // swiftlint:disable:next force_try
        try! StatsDecoder.decode(Data(StatsDecodeTests.full.utf8)).active[0]
    }()

    private func task(_ phase: String, pid: Int, stalled: Bool = false, text: String = "Synthetic fixture task") -> ActiveTask {
        var t = Self.template
        t.phase = phase; t.pid = pid; t.stalled = stalled; t.task = text
        return t
    }

    private func snapshot(active: [ActiveTask]) throws -> StatsSnapshot {
        var s = try StatsDecoder.decode(Data(StatsDecodeTests.full.utf8))
        s.active = active
        let w = try StatsDecoder.decode(WorkloadDecodeTests.doc(
            workload: WorkloadDecodeTests.workload(processing: "\(active.filter(\.isRunning).count)", queued: "\(active.filter { $0.phase == "queued" }.count)", stalled: "0"),
            daily: WorkloadDecodeTests.daily(todayCalls: "19", peak: "7", avg: "82.0")))
        s.workload = w.workload
        s.daily = w.daily
        return s
    }

    private func states() throws -> [State] {
        let candidate = UpdateCandidate(version: SemanticVersion("9.9.9")!, tag: "v9.9.9", assetName: "synthetic.zip",
                                        downloadURL: URL(string: "https://example.invalid/synthetic")!, sha256: "")
        return [
            State(name: "idle", model: MonitorModel(testingSnapshot: try snapshot(active: []))),
            State(name: "working", model: MonitorModel(testingSnapshot: try snapshot(active: (0..<3).map { task("running", pid: 100 + $0) }))),
            State(name: "queued", model: MonitorModel(testingSnapshot: try snapshot(active: (0..<2).map { task("queued", pid: 200 + $0) }))),
            State(name: "needs-attention", model: MonitorModel(testingSnapshot: try snapshot(active: [
                task("running", pid: 300, stalled: true), task("lost", pid: 301), task("unresolved", pid: 302)]))),
            State(name: "stale", model: MonitorModel(testingSnapshot: try snapshot(active: [task("running", pid: 400)]),
                                                     error: "offload stats failed (exit 2): synthetic failure",
                                                     lastSuccess: Date().addingTimeInterval(-3600))),
            State(name: "offline", model: MonitorModel(testingSnapshot: nil, error: "offload stats failed (exit 2): synthetic failure " + String(repeating: "x", count: 200))),
            State(name: "unknown", model: MonitorModel(testingSnapshot: nil, loading: true)),
            State(name: "long-profile-name", model: MonitorModel(testingSnapshot: try snapshot(active: [task("running", pid: 500)])),
                  profile: "synthetic-profile-" + String(repeating: "very-long-name-", count: 8)),
            State(name: "update-notice", model: MonitorModel(testingSnapshot: try snapshot(active: [task("running", pid: 600)])),
                  update: .available(candidate)),
            State(name: "populated-task-list", model: MonitorModel(testingSnapshot: try PanelFitTests().snapshot(active: 7, recent: 20, groups: 15, warnings: 4))),
        ]
    }

    private func panel(_ s: State, measuring: Bool) throws -> NSHostingView<some View> {
        let prefs = Preferences(defaults: UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!)
        prefs.autoCheck = false
        let updates = UpdateManager(prefs: prefs, consumeReport: false)
        updates.setPhaseForTesting(s.update)
        let profiles = ProfilesModel(testingSnapshot: try ProfilesSnapshot.decode(Data(Fixtures.profiles(
            [Fixtures.profile(name: s.profile)], active: s.profile).utf8)))
        let motion = MotionController(monitor: s.model, prefs: prefs, scheduler: ManualScheduler(), observeSystem: false)
        let view = PopoverView(model: s.model, updates: updates, profiles: profiles, motion: motion, isDashboard: false, measuring: measuring)
            .background(Color(nsColor: .windowBackgroundColor))
        let host = NSHostingView(rootView: view)
        host.appearance = NSAppearance(named: .aqua)
        return host
    }

    private func label(_ s: State) -> NSHostingView<MenuBarLabel> {
        let prefs = Preferences(defaults: UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!)
        let updates = UpdateManager(prefs: prefs, consumeReport: false)
        updates.setPhaseForTesting(s.update)
        let motion = MotionController(monitor: s.model, prefs: prefs, scheduler: ManualScheduler(), observeSystem: false)
        return NSHostingView(rootView: MenuBarLabel(model: s.model, updates: updates, motion: motion.menuBar))
    }

    private func png(_ host: NSView) throws -> Data {
        host.layoutSubtreeIfNeeded()
        let bitmap = try XCTUnwrap(host.bitmapImageRepForCachingDisplay(in: host.bounds))
        host.cacheDisplay(in: host.bounds, to: bitmap)
        return try XCTUnwrap(bitmap.representation(using: .png, properties: [:]))
    }

    /// Renders every state offscreen (nothing is shown on screen), writes one PNG per state to
    /// `OFFLOAD_PANEL_RENDER_DIR` when set, and reports every violated invariant. Images are written
    /// before any assertion, so a failing run still leaves them for upload.
    func testEveryStateRendersAndKeepsLayoutInvariants() throws {
        var output: URL?
        if let dir = ProcessInfo.processInfo.environment["OFFLOAD_PANEL_RENDER_DIR"], !dir.isEmpty {
            output = URL(fileURLWithPath: dir)
            try FileManager.default.createDirectory(at: output!, withIntermediateDirectories: true)
        }
        let all = try states()
        XCTAssertEqual(all.count, 10)
        var violations: [String] = []
        for s in all {
            // Natural size: the content is not forced into the fixed height, so overflow shows up as a larger height.
            let natural = try panel(s, measuring: true).fittingSize
            violations += PanelInvariants.panelViolations(s.name, natural: natural)

            // Image: the real fixed-size panel, as the menu shows it.
            let host = try panel(s, measuring: false)
            host.frame = NSRect(x: 0, y: 0, width: Layout.width, height: Layout.height)
            let data = try png(host)
            XCTAssertGreaterThan(data.count, 1000, "\(s.name): rendered image looks empty")
            let fixed = host.fittingSize
            violations += PanelInvariants.panelViolations(s.name + " (fixed)", natural: CGSize(width: fixed.width, height: min(fixed.height, Layout.height)))

            let labelHost = label(s)
            let labelWidth = labelHost.fittingSize.width
            labelHost.frame = NSRect(x: 0, y: 0, width: labelWidth, height: max(labelHost.fittingSize.height, 22))
            violations += PanelInvariants.labelViolations(s.name, width: labelWidth)

            if let output {
                try data.write(to: output.appendingPathComponent("panel-\(s.name).png"))
                try png(labelHost).write(to: output.appendingPathComponent("menubar-\(s.name).png"))
            }
        }
        XCTAssertTrue(violations.isEmpty, "layout invariants broken:\n" + violations.joined(separator: "\n"))
    }

    /// The menu-bar label keeps its width for every status and count, including the cap (99+).
    func testMenuBarLabelWidthIsFixedForEveryStatusAndCount() throws {
        var violations: [String] = []
        for phase in ["running", "queued", "lost"] {
            for n in [0, 1, 9, 10, 99, 100, 250] {
                let m = MonitorModel(testingSnapshot: try snapshot(active: (0..<n).map { task(phase, pid: 1000 + $0) }))
                violations += PanelInvariants.labelViolations("\(phase) x\(n)", width: label(State(name: "", model: m)).fittingSize.width)
            }
        }
        XCTAssertTrue(violations.isEmpty, violations.joined(separator: "\n"))
    }
}

/// The invariant checks must fail on a broken layout; otherwise a passing run proves nothing.
@MainActor
final class PanelRenderInvariantTests: XCTestCase {
    func testOverflowingHeightIsReported() {
        let tall = NSHostingView(rootView: Color.red.frame(width: Layout.width, height: Layout.height + 40)).fittingSize
        let v = PanelInvariants.panelViolations("tall", natural: tall)
        XCTAssertEqual(v.count, 1)
        XCTAssertTrue(v[0].contains("clipped"))
    }

    func testWrongWidthIsReported() {
        let narrow = NSHostingView(rootView: Color.red.frame(width: Layout.width - 20, height: 100)).fittingSize
        XCTAssertEqual(PanelInvariants.panelViolations("narrow", natural: narrow).count, 1)
    }

    func testCorrectSizeIsAccepted() {
        XCTAssertTrue(PanelInvariants.panelViolations("ok", natural: CGSize(width: Layout.width, height: Layout.height)).isEmpty)
        XCTAssertTrue(PanelInvariants.labelViolations("ok", width: MenuBarLabel.Metrics.totalWidth).isEmpty)
    }

    func testWrongLabelWidthIsReported() {
        XCTAssertEqual(PanelInvariants.labelViolations("wide", width: MenuBarLabel.Metrics.totalWidth + 6).count, 1)
    }
}
