import AppKit
import SwiftUI
import XCTest
@testable import OffloadMonitor

/// Every command targets an isolated shell fixture, never the installed CLI or the real configuration.
@MainActor
final class OffloadSwitchTests: XCTestCase {
    static func doc(enabled: Bool, processing: Int? = 0, queued: Int? = 0, status: String = "ok",
                    instructionsError: String? = nil, withInstructions: Bool = false) -> String {
        func n(_ v: Int?) -> String { v.map(String.init) ?? "null" }
        let active = (processing != nil && queued != nil) ? String(processing! + queued!) : "null"
        let ins = withInstructions
            ? #","instructions":{"updated":\#(instructionsError == nil),"new_session_required":true\#(instructionsError.map { #","error":"\#($0)""# } ?? "")}"#
            : ""
        return """
        {"schema_version":1,"enabled":\(enabled),"new_executions":"\(enabled ? "allowed" : "blocked")",
         \(enabled ? "" : #""error_kind":"globally_disabled","reason":"offload is globally disabled; complete work directly or run offload enable","#)
         "config_path":"/c/config.json",
         "running":{"status":"\(status)","processing":\(n(processing)),"queued":\(n(queued)),"cleaning":0,"unresolved":0,"lost":0,"active":\(active)\(enabled ? "" : #","handling":"not_stopped""#)}\(ins)}
        """
    }

    private func fake(status: String, toggle: String? = nil, exit2: Bool = false) throws -> FakeCLI {
        let f = try FakeCLI()
        try status.write(to: f.dir.appendingPathComponent("status.json"), atomically: true, encoding: .utf8)
        try (toggle ?? status).write(to: f.dir.appendingPathComponent("toggle.json"), atomically: true, encoding: .utf8)
        let script = """
        #!/bin/sh
        D='\(f.dir.path)'
        echo "$*" >> "$D/argv.log"
        case "$1" in
          status) cat "$D/status.json" ;;
          enable|disable) cp "$D/toggle.json" "$D/status.json"; cat "$D/toggle.json"; \(exit2 ? "exit 2" : "exit 0") ;;
          task) echo '{"schema_version":1,"run_id":"'"$4"'","caller":"unspecified","phase":"cleaning","active":false,"can_stop":false,"can_resume":false,"resume_reason":"x","resume_pending":false}' ;;
          profile) echo "unexpected probe" >> "$D/probe.log"; exit 0 ;;
          *) echo "unexpected: $*" >&2; exit 64 ;;
        esac
        """
        try script.write(to: f.exe, atomically: true, encoding: .utf8)
        return f
    }

    func testDecodeKeepsStableKindAndReason() throws {
        let s = try OffloadSwitch.decode(Data(Self.doc(enabled: false, processing: 2, queued: 1).utf8))
        XCTAssertFalse(s.enabled)
        XCTAssertTrue(s.blocksNewExecutions)
        XCTAssertEqual(s.errorKind, OffloadSwitch.disabledKind)
        XCTAssertEqual(s.running.active, 3)
        XCTAssertEqual(s.running.handling, "not_stopped")
        let on = try OffloadSwitch.decode(Data(Self.doc(enabled: true).utf8))
        XCTAssertNil(on.errorKind)
        XCTAssertFalse(on.blocksNewExecutions)
    }

    func testDecodeRejectsUnknownSchemaAndGarbage() {
        XCTAssertThrowsError(try OffloadSwitch.decode(Data(#"{"schema_version":2}"#.utf8)))
        XCTAssertThrowsError(try OffloadSwitch.decode(Data("not json".utf8)))
    }

    func testPresentationSeparatesBlockedFromStillRunning() throws {
        let off = try OffloadSwitch.decode(Data(Self.doc(enabled: false, processing: 2, queued: 1).utf8))
        XCTAssertEqual(SwitchPresentation.title(off), "Offload is off")
        XCTAssertTrue(SwitchPresentation.newExecutionsLine(off).contains("blocked"))
        let running = SwitchPresentation.runningLine(off)
        XCTAssertTrue(running.contains("2 running") && running.contains("1 queued"), running)
        XCTAssertTrue(running.contains("keep going") && running.contains("will not start"), running)
        XCTAssertEqual(SwitchPresentation.banner(off)?.message, "New executions are blocked. 3 tasks still running.")

        let idle = try OffloadSwitch.decode(Data(Self.doc(enabled: false).utf8))
        XCTAssertEqual(SwitchPresentation.runningLine(idle), "No tasks are running.")
        XCTAssertEqual(SwitchPresentation.banner(idle)?.message, "New executions are blocked. No tasks running.")

        let on = try OffloadSwitch.decode(Data(Self.doc(enabled: true, processing: 1).utf8))
        XCTAssertEqual(SwitchPresentation.title(on), "Offload is on")
        XCTAssertNil(SwitchPresentation.banner(on))
    }

    func testUnknownAndPartialCountsAreNeverShownAsZero() throws {
        let unknown = try OffloadSwitch.decode(Data(Self.doc(enabled: false, processing: nil, queued: nil, status: "unavailable").utf8))
        XCTAssertTrue(SwitchPresentation.runningLine(unknown).contains("unknown"))
        let partial = try OffloadSwitch.decode(Data(Self.doc(enabled: false, processing: 1, queued: 0, status: "partial").utf8))
        XCTAssertTrue(SwitchPresentation.runningLine(partial).hasPrefix("at least "))
        XCTAssertEqual(SwitchPresentation.title(nil), "Offload status unknown")
    }

    func testReloadFollowsChangesMadeElsewhere() async throws {
        let f = try fake(status: Self.doc(enabled: true))
        let model = OffloadSwitchModel(cli: f.cli, autoLoad: false)
        let reloaded = await model.reload()
        XCTAssertTrue(reloaded)
        XCTAssertEqual(model.current?.enabled, true)
        // Another front end (the CLI) turns Offload off.
        try Self.doc(enabled: false, processing: 1).write(to: f.dir.appendingPathComponent("status.json"), atomically: true, encoding: .utf8)
        var disabledFired = 0
        model.onDisabled = { disabledFired += 1 }
        _ = await model.reload()
        XCTAssertEqual(model.current?.enabled, false)
        XCTAssertTrue(model.isOff)
        XCTAssertEqual(disabledFired, 1)
        _ = await model.reload()
        XCTAssertEqual(disabledFired, 1, "onDisabled fires on the transition only")
    }

    func testSetEnabledUsesTheSharedCommandAndKeepsRunningTasks() async throws {
        let f = try fake(status: Self.doc(enabled: true, processing: 2),
                         toggle: Self.doc(enabled: false, processing: 2, withInstructions: true))
        let model = OffloadSwitchModel(cli: f.cli, autoLoad: false)
        _ = await model.reload()
        let ok = await model.setEnabled(false)
        XCTAssertTrue(ok)
        XCTAssertTrue(model.isOff)
        XCTAssertEqual(f.argv.filter { $0 == "disable --json" }.count, 1)
        XCTAssertFalse(f.argv.contains { $0.hasPrefix("task") }, "turning off must not stop any task")
        XCTAssertNil(model.instructionsNote)
    }

    func testSavedButInstructionsConflictIsReportedNotThrown() async throws {
        let f = try fake(status: Self.doc(enabled: true),
                         toggle: Self.doc(enabled: false, instructionsError: "fix the conflict", withInstructions: true), exit2: true)
        let model = OffloadSwitchModel(cli: f.cli, autoLoad: false)
        let ok = await model.setEnabled(false)
        XCTAssertTrue(ok, "the switch was saved")
        XCTAssertTrue(model.isOff)
        XCTAssertEqual(model.instructionsNote, "fix the conflict")
        XCTAssertNil(model.actionError)
    }

    func testStopRunningIsSeparateAndExplicit() async throws {
        let f = try fake(status: Self.doc(enabled: false, processing: 1))
        let model = OffloadSwitchModel(cli: f.cli, autoLoad: false)
        let id = String(repeating: "a", count: 32)
        await model.stopRunning(runIDs: [id, "not-a-run-id"])
        XCTAssertEqual(f.argv.filter { $0.hasPrefix("task stop") }, ["task stop --id \(id) --json"])
        XCTAssertEqual(model.stopResult, "Asked 1 to stop; 1 could not be stopped.")
    }

    func testProfileChecksStayOffWhileSwitchIsOff() async throws {
        let f = try fake(status: Self.doc(enabled: false))
        let snapshot = try ProfilesSnapshot.decode(Data(Fixtures.profiles([Fixtures.profile(name: "a")]).utf8))
        var withUnavailable = snapshot
        withUnavailable.profiles[0].available = false
        withUnavailable.profiles[0].unavailableAt = ISO8601DateFormatter().string(from: Date(timeIntervalSince1970: 1))
        let profiles = ProfilesModel(cli: f.cli, autoLoad: false)
        profiles.setSnapshotForTesting(withUnavailable)
        profiles.checksBlocked = { true }

        await profiles.testUnavailable(automatic: true)   // the background path
        XCTAssertNil(profiles.blockedNotice, "the background path stays quiet")
        profiles.startUnavailableTests()                  // Check all unavailable
        let single = await profiles.test("a")             // Check profile
        XCTAssertFalse(single)
        XCTAssertEqual(profiles.blockedNotice, ProfilesModel.offNotice)
        XCTAssertFalse(f.argv.contains { $0.hasPrefix("profile test") }, "no model call may start: \(f.argv)")

        profiles.clearActionError()
        XCTAssertNil(profiles.blockedNotice)
        profiles.checksBlocked = { false }
        _ = await profiles.test("a")
        XCTAssertTrue(f.argv.contains { $0.hasPrefix("profile test a") }, "checks run again once Offload is on")
    }

    func testChatSendIsBlockedWithAReasonWhileOff() async throws {
        let f = try fake(status: Self.doc(enabled: false))
        let off = try OffloadSwitch.decode(Data(Self.doc(enabled: false).utf8))
        let sw = OffloadSwitchModel(testing: off)
        let profiles = ProfilesModel(testingSnapshot: try ProfilesSnapshot.decode(Data(Fixtures.profiles([Fixtures.profile(name: "w")], active: "w").utf8)))
        let d = UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!
        let env = ChatEnv(cli: f.cli, profiles: profiles, monitor: MonitorModel(testingSnapshot: nil),
                          history: HistoryModel(cli: nil), offloadSwitch: sw, prefs: Preferences(defaults: d))
        let chat = ChatModel(env: env)
        chat.draft = "hello"
        XCTAssertEqual(chat.blockReason, SwitchPresentation.chatBlockReason)
        XCTAssertFalse(chat.canSend)
        chat.send()
        XCTAssertTrue(f.argv.isEmpty, "no command may be launched while Offload is off")
    }

    func testOffloadTabFitsAndAllStatesRender() throws {
        let d = UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!
        let motion = MotionController(monitor: MonitorModel(testingSnapshot: nil), prefs: Preferences(defaults: d), scheduler: ManualScheduler(), observeSystem: false)
        let updates = UpdateManager()
        let states: [(String, String?)] = [
            (Self.doc(enabled: true), nil),
            (Self.doc(enabled: false, processing: 2, queued: 1), nil),
            (Self.doc(enabled: false, processing: nil, queued: nil, status: "unavailable"), nil),
            (Self.doc(enabled: false, instructionsError: "A custom skill file blocks the update and was left untouched.", withInstructions: true), nil),
        ]
        for (json, _) in states {
            let model = OffloadSwitchModel(testing: try OffloadSwitch.decode(Data(json.utf8)))
            let host = NSHostingView(rootView: SettingsView(profiles: ProfilesModel(testingSnapshot: nil), skills: AgentSkillsModel(testingSnapshot: nil),
                                                            updates: updates, motion: motion, offloadSwitch: model,
                                                            monitor: MonitorModel(testingSnapshot: nil), tab: .offload))
            host.frame = NSRect(x: 0, y: 0, width: SettingsLayout.width, height: SettingsLayout.height)
            host.layoutSubtreeIfNeeded()
            XCTAssertLessThanOrEqual(host.fittingSize.height, SettingsLayout.height + 1, "offload tab overflows for \(json)")
        }
        let unknown = OffloadSwitchModel(testing: nil, error: "The offload command did not answer in time.")
        let host = NSHostingView(rootView: OffloadSettingsView(model: unknown, monitor: MonitorModel(testingSnapshot: nil)).frame(width: SettingsLayout.width))
        XCTAssertLessThanOrEqual(host.fittingSize.height, SettingsLayout.height)
    }
}
