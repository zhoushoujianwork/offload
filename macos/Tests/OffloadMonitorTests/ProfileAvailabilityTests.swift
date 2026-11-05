import AppKit
import SwiftUI
import XCTest
@testable import OffloadMonitor

@MainActor
final class ProfileAvailabilityTests: XCTestCase {
    private func entry(_ name: String, unavailable: Bool) -> String {
        let extra = unavailable
            ? #","available":false,"unavailable_reason":"authentication failed: invalid key","unavailable_at":"2026-01-02T03:04:05Z""#
            : #","available":true,"unavailable_reason":"","unavailable_at":"""#
        var s = Fixtures.profile(name: name)
        s.removeLast()
        return s + extra + "}"
    }

    private func snapshot() throws -> ProfilesSnapshot {
        try ProfilesSnapshot.decode(Data(Fixtures.profiles([entry("bad", unavailable: true), entry("good", unavailable: false)], active: "good", configured: "good").utf8))
    }

    func testDecodesUnavailableStateAndHoverDetail() throws {
        let s = try snapshot()
        let bad = try XCTUnwrap(s.entry("bad"))
        XCTAssertTrue(bad.isUnavailable)
        XCTAssertTrue(bad.unavailableDetail.contains("authentication failed"))
        XCTAssertTrue(bad.unavailableDetail.contains("2026-01-02T03:04:05Z"))
        XCTAssertFalse(try XCTUnwrap(s.entry("good")).isUnavailable)
    }

    func testOlderCLIOutputWithoutStateDecodesAsAvailable() throws {
        let s = try ProfilesSnapshot.decode(Data(Fixtures.profiles([Fixtures.profile(name: "w")], active: "w").utf8))
        XCTAssertFalse(try XCTUnwrap(s.entry("w")).isUnavailable)
    }

    func testPresentationShowsStateAndRetestEntry() throws {
        let s = try snapshot()
        XCTAssertTrue(ProfilePresentation.pickerTitle(try XCTUnwrap(s.entry("bad"))).hasSuffix("(unavailable)"))
        XCTAssertFalse(ProfilePresentation.pickerTitle(try XCTUnwrap(s.entry("good"))).contains("unavailable"))
        XCTAssertEqual(ProfilePresentation.retestTitle(testing: false), "Re-test")
        XCTAssertEqual(ProfilePresentation.retestTitle(testing: true), "Testing…")
        XCTAssertNotNil(ProfilePresentation.testing(true))
        XCTAssertNil(ProfilePresentation.testing(false))
    }

    func testRunningTestBlocksSecondTestOfSameProfile() async throws {
        let model = ProfilesModel(testingSnapshot: try snapshot())
        model.setTestingForTesting(["bad"])
        let started = await model.test("bad")
        XCTAssertFalse(started)
        XCTAssertEqual(model.testing, ["bad"], "the running test must stay recorded")
    }

    func testSettingsRendersUnavailableProfileWithinWindow() throws {
        let profiles = ProfilesModel(testingSnapshot: try snapshot())
        let skills = AgentSkillsModel(testingSnapshot: try SkillsSnapshot.decode(Data(Fixtures.skills([
            Fixtures.agent("codex", status: "enabled", enabled: true)]).utf8)))
        let d = UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!
        let updates = UpdateManager(prefs: Preferences(defaults: d), consumeReport: false)
        for testing in [false, true] {
            profiles.setTestingForTesting(testing ? ["bad"] : [])
            let host = NSHostingView(rootView: SettingsView(profiles: profiles, skills: skills, updates: updates, motion: MotionController(monitor: MonitorModel(testingSnapshot: nil), prefs: Preferences(defaults: d), scheduler: ManualScheduler(), observeSystem: false), tab: .profiles))
            host.frame = NSRect(x: 0, y: 0, width: SettingsLayout.width, height: SettingsLayout.height)
            host.layoutSubtreeIfNeeded()
            XCTAssertLessThanOrEqual(host.fittingSize.height, SettingsLayout.height + 1)
            let rep = try XCTUnwrap(host.bitmapImageRepForCachingDisplay(in: host.bounds))
            host.cacheDisplay(in: host.bounds, to: rep)
            XCTAssertGreaterThan(rep.pixelsWide, 0)
        }
    }
}
