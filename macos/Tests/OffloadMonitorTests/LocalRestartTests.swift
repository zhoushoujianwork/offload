import Foundation
import AppKit
import SwiftUI
import XCTest
@testable import OffloadMonitor

final class LocalInstallObservationTests: XCTestCase {
    func testCLIReplacementRefreshesWithoutAppRestart() {
        var state = LocalInstallObservation()
        XCTAssertFalse(state.ingest(app: "app", cli: "cli1"))
        XCTAssertTrue(state.ingest(app: "app", cli: "cli2"))
        XCTAssertFalse(state.appChanged)
        XCTAssertFalse(state.stable)
        XCTAssertFalse(state.ingest(app: "app", cli: "cli2"))
    }

    func testSameVersionReplacementNeedsStableBytesAndRestorationCancelsIt() {
        var state = LocalInstallObservation()
        _ = state.ingest(app: "launched", cli: "cli")
        _ = state.ingest(app: "new-build", cli: "cli")
        XCTAssertTrue(state.appChanged)
        XCTAssertFalse(state.stable)
        _ = state.ingest(app: "new-build", cli: "cli")
        XCTAssertTrue(state.stable)
        _ = state.ingest(app: "launched", cli: "cli")
        XCTAssertFalse(state.appChanged)
        XCTAssertFalse(state.stable)
    }

    func testMissingOrChangingFilesNeverCountAsStable() {
        var state = LocalInstallObservation()
        _ = state.ingest(app: "old", cli: "cli")
        _ = state.ingest(app: "new", cli: "cli")
        _ = state.ingest(app: nil, cli: nil)
        XCTAssertFalse(state.stable)
        _ = state.ingest(app: "new", cli: "cli")
        XCTAssertFalse(state.stable)
        _ = state.ingest(app: "newer", cli: "cli")
        XCTAssertFalse(state.stable)
        _ = state.ingest(app: "newer", cli: "cli")
        XCTAssertTrue(state.stable)
    }

    func testTemporaryStartupReadFailureDoesNotDisableFutureDetection() {
        var state = LocalInstallObservation()
        _ = state.ingest(app: nil, cli: nil)
        _ = state.ingest(app: "first-readable", cli: "cli")
        _ = state.ingest(app: "updated", cli: "cli")
        _ = state.ingest(app: "updated", cli: "cli")
        XCTAssertTrue(state.appChanged)
        XCTAssertTrue(state.stable)
    }
}

final class BinaryFingerprintTests: XCTestCase {
    func testInPlaceSameSizeSubsecondReplacementIsDetected() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        let file = dir.appendingPathComponent("with spaces")
        try Data("AAAA".utf8).write(to: file)
        let time = Date(timeIntervalSince1970: 1_800_000_000.1)
        try FileManager.default.setAttributes([.modificationDate: time], ofItemAtPath: file.path)
        let first = try BinaryFingerprint.read(file)
        try Data("BBBB".utf8).write(to: file)
        try FileManager.default.setAttributes([.modificationDate: time.addingTimeInterval(0.01)], ofItemAtPath: file.path)
        let second = try BinaryFingerprint.read(file, cached: first)
        XCTAssertNotEqual(first.digest, second.digest)
        XCTAssertEqual(second, try BinaryFingerprint.read(file, cached: second))
    }

    func testCopyingIdenticalBytesDoesNotMeanDifferentBuild() throws {
        let file = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: file) }
        try Data("same build".utf8).write(to: file)
        let first = try BinaryFingerprint.read(file)
        try Data("same build".utf8).write(to: file, options: .atomic)
        let second = try BinaryFingerprint.read(file, cached: first)
        XCTAssertEqual(first.digest, second.digest)
    }
}

@MainActor
final class LocalRestartTests: XCTestCase {
    var root: URL!
    var app: URL { root.appendingPathComponent("Test.app") }
    var executable: URL { app.appendingPathComponent("Contents/MacOS/OffloadMonitor") }
    var cli: URL { root.appendingPathComponent("offload") }

    override func setUp() async throws {
        root = FileManager.default.temporaryDirectory.appendingPathComponent("local-restart-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: executable.deletingLastPathComponent(), withIntermediateDirectories: true)
        try Data("launched".utf8).write(to: executable)
        try Data("version 1".utf8).write(to: app.appendingPathComponent("Contents/Info.plist"))
        try Data("cli1".utf8).write(to: cli)
    }

    override func tearDown() async throws { try? FileManager.default.removeItem(at: root) }

    func monitor(validate: @escaping (URL) async throws -> Void = { _ in }) -> LocalInstallMonitor {
        LocalInstallMonitor(appURL: app, executableURL: executable, cliURL: cli, validate: validate)
    }

    func prefs() -> Preferences {
        let defaults = UserDefaults(suiteName: "local-restart-\(UUID().uuidString)")!
        let p = Preferences(defaults: defaults)
        p.autoCheck = false
        return p
    }

    func replaceApp(_ monitor: LocalInstallMonitor) async throws {
        try Data("replacement build".utf8).write(to: executable, options: .atomic)
        await monitor.poll()
        XCTAssertFalse(monitor.readyToRestart)
        await monitor.poll()
    }

    func testSelectedCLIChangeRefreshesOnceWithoutTermination() async throws {
        let m = monitor()
        let u = UpdateManager(prefs: prefs(), consumeReport: false, localChanges: m)
        var refreshed = 0, terminated = 0
        u.configureLifecycle(blocker: { nil }, refreshCLI: { refreshed += 1 }, requestTermination: { terminated += 1 })
        await m.poll()
        try Data("cli2".utf8).write(to: cli, options: .atomic)
        await m.poll()
        await m.poll()
        XCTAssertEqual(refreshed, 1)
        XCTAssertEqual(terminated, 0)
        XCTAssertNotNil(m.cliChangedAt)
        XCTAssertNil(u.localNotice)
    }

    func testAppReplacementWaitsForChatsAndOnlyLaunchesAtApprovedQuitGate() async throws {
        let m = monitor()
        let u = UpdateManager(prefs: prefs(), consumeReport: false, localChanges: m)
        var blocked: String? = "running chat", requests = 0, launches = 0
        u.configureLifecycle(blocker: { blocked }, refreshCLI: {}, requestTermination: { requests += 1 })
        u.launchLocalRestart = { [self] url, digest in
            XCTAssertEqual(url, app)
            XCTAssertEqual(digest, try InstalledAppFingerprint.read(url).digest)
            launches += 1
        }
        await m.poll()
        try await replaceApp(m)
        XCTAssertTrue(m.readyToRestart)
        XCTAssertEqual(requests, 0)
        XCTAssertEqual(u.restartWaitReason, "running chat")
        blocked = nil
        u.considerRestart()
        XCTAssertEqual(requests, 1)
        XCTAssertEqual(launches, 0)
        XCTAssertEqual(u.approvePendingRestart(), true)
        XCTAssertEqual(launches, 1)
        XCTAssertEqual(u.approvePendingRestart(), true)
        XCTAssertEqual(launches, 1, "duplicate quit callbacks must not launch multiple helpers")
    }

    func testLateDraftBlocksTheQuitGateAndNoHelperStarts() async throws {
        let m = monitor()
        let u = UpdateManager(prefs: prefs(), consumeReport: false, localChanges: m)
        var blocked: String?
        var launches = 0
        u.configureLifecycle(blocker: { blocked }, refreshCLI: {}, requestTermination: {})
        u.launchLocalRestart = { _, _ in launches += 1 }
        await m.poll()
        try await replaceApp(m)
        blocked = "unsent draft"
        XCTAssertEqual(u.approvePendingRestart(), false)
        XCTAssertEqual(launches, 0)
        XCTAssertEqual(u.restartWaitReason, "unsent draft")
    }

    func testVersionMetadataChangeRequiresRestartEvenWithSameSwiftExecutable() async throws {
        let m = monitor()
        await m.poll()
        let before = try BinaryFingerprint.read(executable).digest
        try Data("version 2".utf8).write(to: app.appendingPathComponent("Contents/Info.plist"), options: .atomic)
        await m.poll()
        await m.poll()
        XCTAssertEqual(before, try BinaryFingerprint.read(executable).digest)
        XCTAssertTrue(m.readyToRestart)
    }

    func testRestartNoticeAndSettingsFitWithoutAddingPanelScrolling() async throws {
        let m = monitor()
        let p = prefs()
        p.restartOnAppChange = false
        let u = UpdateManager(prefs: p, consumeReport: false, localChanges: m)
        u.configureLifecycle(blocker: { "unsaved profile edits" }, refreshCLI: {}, requestTermination: { XCTFail() })
        await m.poll()
        try await replaceApp(m)
        let model = MonitorModel(testingSnapshot: try StatsDecoder.decode(Data(StatsDecodeTests.full.utf8)))
        let profiles = ProfilesModel(testingSnapshot: nil)
        let motion = MotionController(monitor: model)
        let panel = NSHostingView(rootView: PopoverView(model: model, updates: u, profiles: profiles, motion: motion, measuring: true))
        XCTAssertLessThanOrEqual(panel.fittingSize.height, Layout.height)
        let settings = NSHostingView(rootView: AppSettingsView(updates: u, motion: motion).frame(width: SettingsLayout.width))
        XCTAssertLessThanOrEqual(settings.fittingSize.height, SettingsLayout.height)
    }

    func testDisablingAutomaticRestartCancelsAnIdleWait() async throws {
        let m = monitor()
        let u = UpdateManager(prefs: prefs(), consumeReport: false, localChanges: m)
        var blocked: String? = "unsaved edits", requests = 0
        u.configureLifecycle(blocker: { blocked }, refreshCLI: {}, requestTermination: { requests += 1 })
        await m.poll()
        try await replaceApp(m)
        u.restartOnAppChange = false
        blocked = nil
        u.considerRestart()
        XCTAssertEqual(requests, 0)
        u.restartInstalledApp()
        XCTAssertEqual(requests, 1, "an explicit restart still works with automatic restarts off")
    }

    func testSignatureFailureKeepsRunningAppAndCanBeRetried() async throws {
        var fail = true, checks = 0
        let m = monitor { _ in
            checks += 1
            if fail { throw CocoaError(.fileReadCorruptFile) }
        }
        await m.poll()
        try await replaceApp(m)
        XCTAssertFalse(m.readyToRestart)
        XCTAssertNotNil(m.problem)
        await m.poll()
        XCTAssertEqual(checks, 1)
        fail = false
        m.retryValidation()
        await waitUntil { m.readyToRestart }
        XCTAssertTrue(m.readyToRestart)
        XCTAssertEqual(checks, 2)
    }

    func testTemporaryMissingAppMustBeValidatedAgain() async throws {
        var checks = 0
        let m = monitor { _ in checks += 1 }
        await m.poll()
        try await replaceApp(m)
        XCTAssertTrue(m.readyToRestart)
        try FileManager.default.removeItem(at: executable)
        await m.poll()
        XCTAssertFalse(m.readyToRestart)
        try Data("replacement build".utf8).write(to: executable)
        await m.poll()
        await m.poll()
        XCTAssertTrue(m.readyToRestart)
        XCTAssertEqual(checks, 2)
    }

    func testHelperFailureDoesNotAutomaticallyLoop() async throws {
        let m = monitor()
        let u = UpdateManager(prefs: prefs(), consumeReport: false, localChanges: m)
        var requests = 0
        u.configureLifecycle(blocker: { nil }, refreshCLI: {}, requestTermination: { requests += 1 })
        u.launchLocalRestart = { _, _ in throw CocoaError(.fileWriteNoPermission) }
        await m.poll()
        try await replaceApp(m)
        XCTAssertEqual(u.approvePendingRestart(), false)
        u.considerRestart()
        await m.poll()
        XCTAssertEqual(requests, 1)
        XCTAssertTrue(u.restartWaitReason?.contains("Could not restart") == true)
    }

    func testWholeReleaseUpdateWaitsAndDoesNotLaunchHelperBeforeQuitApproval() {
        let u = UpdateManager(prefs: prefs(), consumeReport: false, localChanges: monitor())
        let c = UpdateCandidate(version: SemanticVersion("9.9.9")!, tag: "v9.9.9", assetName: "app.zip",
                                downloadURL: URL(string: "https://github.com/example/app.zip")!, sha256: "")
        u.setPhaseForTesting(.ready(c), staged: UpdateService.Staged(directory: root, bundle: app))
        var blocked: String? = "busy", requests = 0, launches = 0
        u.configureLifecycle(blocker: { blocked }, refreshCLI: {}, requestTermination: { requests += 1 })
        u.launchReleaseRestart = { _, _, _ in launches += 1 }
        u.installAndRestart()
        XCTAssertEqual(requests, 0)
        XCTAssertEqual(launches, 0)
        XCTAssertNil(u.approvePendingRestart(), "a queued update must not hijack an ordinary user Quit")
        blocked = nil
        u.considerRestart()
        XCTAssertEqual(requests, 1)
        XCTAssertEqual(launches, 0)
        XCTAssertEqual(u.approvePendingRestart(), true)
        XCTAssertEqual(launches, 1)
        XCTAssertEqual(u.phase, .installing(c))
    }

    func testTurningOffAutoInstallCancelsAutomaticQueuedRelease() {
        let p = prefs()
        p.autoInstall = true
        let u = UpdateManager(prefs: p, consumeReport: false, localChanges: monitor())
        let c = UpdateCandidate(version: SemanticVersion("9.9.9")!, tag: "v9.9.9", assetName: "app.zip",
                                downloadURL: URL(string: "https://github.com/example/app.zip")!, sha256: "")
        u.setPhaseForTesting(.ready(c), staged: UpdateService.Staged(directory: root, bundle: app))
        var blocked: String? = "busy", requests = 0
        u.configureLifecycle(blocker: { blocked }, refreshCLI: {}, requestTermination: { requests += 1 })
        u.installAndRestart(manual: false)
        u.autoInstall = false
        blocked = nil
        u.considerRestart()
        XCTAssertEqual(requests, 0)
        XCTAssertFalse(u.pendingReleaseRestart)
    }
}

final class RestartSafetyTests: XCTestCase {
    func testAllUnsavedAndRunningStatesBlockAutomaticRestart() {
        for active in 0..<5 {
            let flags = (0..<5).map { $0 == active }
            XCTAssertNotNil(RestartSafety.reason(running: flags[0], drafts: flags[1], unsavedReplies: flags[2], profileEdits: flags[3], settingsBusy: flags[4]))
        }
        XCTAssertNil(RestartSafety.reason(running: false, drafts: false, unsavedReplies: false, profileEdits: false, settingsBusy: false))
    }

    func testRelaunchPlanWaitsAndNeverOpensIfQuitWasCancelled() throws {
        let plan = LocalRelaunchPlan(pid: 42, app: URL(fileURLWithPath: "/tmp/Test.app"))
        var opened = 0
        XCTAssertFalse(try plan.run(wait: { _ in false }, open: { _ in opened += 1 }))
        XCTAssertEqual(opened, 0)
        XCTAssertTrue(try plan.run(wait: { XCTAssertEqual($0, 42); return true }, open: { _ in opened += 1 }))
        XCTAssertEqual(opened, 1)
        XCTAssertFalse(try LocalRelaunchPlan(pid: 0, app: plan.app).run(wait: { _ in XCTFail(); return true }, open: { _ in XCTFail() }))
    }
}
