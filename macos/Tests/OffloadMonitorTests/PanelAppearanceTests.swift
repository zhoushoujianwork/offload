import AppKit
import SwiftUI
import XCTest
@testable import OffloadMonitor

/// Offscreen rendering only: never orders a window front or touches the running App.
@MainActor
final class PanelAppearanceTests: XCTestCase {
    func testPanelFixtures() throws {
        for active in [0, 7] {
            for scheme in [ColorScheme.light, .dark] {
                let fixture = PanelFitTests()
                var snapshot = try fixture.snapshot(active: active, recent: 8, groups: 5, warnings: 3)
                let workload = try StatsDecoder.decode(WorkloadDecodeTests.doc(
                    workload: WorkloadDecodeTests.workload(processing: "\(active)", queued: "0", stalled: "0"),
                    daily: WorkloadDecodeTests.daily(todayCalls: "19", peak: "7", avg: "82.0")))
                snapshot.workload = workload.workload
                snapshot.daily = workload.daily
                let model = MonitorModel(testingSnapshot: snapshot)
                let defaults = UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!
                let prefs = Preferences(defaults: defaults)
                prefs.autoCheck = false
                let updates = UpdateManager(prefs: prefs, consumeReport: false)
                updates.setPhaseForTesting(.available(UpdateCandidate(version: SemanticVersion("0.4.2")!, tag: "v0.4.2",
                    assetName: "test.zip", downloadURL: URL(string: "https://github.com/test")!, sha256: "")))
                let profiles = ProfilesModel(testingSnapshot: try ProfilesSnapshot.decode(Data(Fixtures.profiles(
                    [Fixtures.profile(name: "codex-gpt-6-with-a-long-profile-name")],
                    active: "codex-gpt-6-with-a-long-profile-name").utf8)))
                let motion = MotionController(monitor: model, prefs: prefs, scheduler: ManualScheduler(), observeSystem: false)
                let panel = PopoverView(model: model, updates: updates, profiles: profiles, motion: motion)
                    .environment(\.colorScheme, scheme)
                    .background(Color(nsColor: scheme == .light ? .windowBackgroundColor : NSColor(calibratedWhite: 0.12, alpha: 1)))
                let host = NSHostingView(rootView: panel)
                host.appearance = NSAppearance(named: scheme == .light ? .aqua : .darkAqua)
                host.frame = NSRect(x: 0, y: 0, width: Layout.width, height: Layout.height)
                host.layoutSubtreeIfNeeded()
                XCTAssertEqual(host.fittingSize.width, Layout.width, accuracy: 0.5)
                XCTAssertEqual(host.fittingSize.height, Layout.height, accuracy: 0.5)
                if let directory = ProcessInfo.processInfo.environment["OFFLOAD_PANEL_RENDER_DIR"] {
                    let output = URL(fileURLWithPath: directory)
                    try FileManager.default.createDirectory(at: output, withIntermediateDirectories: true)
                    let bitmap = try XCTUnwrap(host.bitmapImageRepForCachingDisplay(in: host.bounds))
                    host.cacheDisplay(in: host.bounds, to: bitmap)
                    let data = try XCTUnwrap(bitmap.representation(using: .png, properties: [:]))
                    XCTAssertGreaterThan(data.count, 1000)
                    try data.write(to: output.appendingPathComponent("panel-\(active == 0 ? "idle" : "busy")-\(scheme == .light ? "light" : "dark").png"))
                }
            }
        }
    }
}
