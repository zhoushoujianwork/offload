import XCTest
import AppKit
import SwiftUI
@testable import OffloadMonitor

private enum CallerFixtures {
    static func disabled(_ profile: String) -> String {
        profile.replacingOccurrences(of: #""auth_mode":"token"}"#, with: #""auth_mode":"token","disabled":true}"#)
    }

    static func unavailable(_ profile: String) -> String {
        profile.replacingOccurrences(of: #""auth_mode":"token"}"#, with: #""auth_mode":"token","available":false,"unavailable_reason":"quota"}"#)
    }

    /// A snapshot with caller defaults. `callers` is inserted as the `caller_defaults` object.
    static func json(_ entries: [String], callers: String?, configured: String = "work", active: String = "work", source: String = "config") -> String {
        let base = Fixtures.profiles(entries, active: active, source: source, configured: configured)
        guard let callers else { return base }
        return base.replacingOccurrences(of: #""delegation_mode":"proactive","#, with: #""delegation_mode":"proactive","caller_defaults":\#(callers),"#)
    }

    static func snapshot(_ entries: [String], callers: String?, configured: String = "work") throws -> ProfilesSnapshot {
        try ProfilesSnapshot.decode(Data(json(entries, callers: callers, configured: configured).utf8))
    }
}

final class CallerDefaultDecodeTests: XCTestCase {
    func testMissingCallerDefaultsDecodesAsEmpty() throws {
        let s = try CallerFixtures.snapshot([Fixtures.profile(name: "work")], callers: nil)
        XCTAssertEqual(s.callerDefaults, [:])
        XCTAssertEqual(s.callerDefault(for: .codex), "")
        XCTAssertEqual(s.callerDefault(for: .claude), "")
    }

    func testEmptyAndPopulatedCallerDefaultsDecode() throws {
        XCTAssertEqual(try CallerFixtures.snapshot([Fixtures.profile(name: "work")], callers: "{}").callerDefaults, [:])
        let s = try CallerFixtures.snapshot([Fixtures.profile(name: "work"), Fixtures.profile(name: "cx", backend: "codex", auth: "none")],
                                            callers: #"{"codex":"cx","claude":"work","agh":"work"}"#)
        XCTAssertEqual(s.callerDefault(for: .codex), "cx")
        XCTAssertEqual(s.callerDefault(for: .claude), "work")
        XCTAssertEqual(s.callerDefaults["agh"], "work", "callers the App has no control for are still decoded")
        XCTAssertEqual(s.configuredDefault, "work", "caller defaults never change the global fields")
    }

    func testSnapshotRoundTripKeepsCallerDefaults() throws {
        let s = try CallerFixtures.snapshot([Fixtures.profile(name: "work")], callers: #"{"codex":"work"}"#)
        let again = try JSONDecoder().decode(ProfilesSnapshot.self, from: JSONEncoder().encode(s))
        XCTAssertEqual(again, s)
    }
}

final class CallerDefaultOptionTests: XCTestCase {
    func testFollowGlobalIsFirstAndNamesTheConfiguredDefault() throws {
        let s = try CallerFixtures.snapshot([Fixtures.profile(name: "work"), Fixtures.profile(name: "alt")], callers: nil)
        let options = s.callerDefaultOptions(for: .codex)
        XCTAssertEqual(options.map(\.value), ["", "work", "alt"], "Follow global default first, then profiles in snapshot order")
        XCTAssertEqual(options.first?.title, "Follow global default (work)")
        XCTAssertEqual(try CallerFixtures.snapshot([Fixtures.profile(name: "work")], callers: nil, configured: "").followGlobalTitle(),
                       "Follow global default (none set)")
    }

    func testOnlyEnabledProfilesAreOffered() throws {
        let s = try CallerFixtures.snapshot([Fixtures.profile(name: "work"), CallerFixtures.disabled(Fixtures.profile(name: "off")),
                                             CallerFixtures.unavailable(Fixtures.profile(name: "slow"))], callers: nil)
        let values = s.callerDefaultOptions(for: .claude).map(\.value)
        XCTAssertEqual(values, ["", "work", "slow"])
        XCTAssertEqual(s.callerDefaultOptions(for: .claude).last?.title, "slow — m1 (unavailable)")
    }

    func testDisabledOrMissingCurrentMappingStaysVisibleAndLabelled() throws {
        let entries = [Fixtures.profile(name: "work"), CallerFixtures.disabled(Fixtures.profile(name: "off"))]
        let s = try CallerFixtures.snapshot(entries, callers: #"{"codex":"off","claude":"gone"}"#)
        let codex = s.callerDefaultOptions(for: .codex)
        XCTAssertEqual(codex.map(\.value), ["", "work", "off"])
        XCTAssertEqual(codex.last?.title, "off — m1 (disabled)")
        XCTAssertEqual(s.callerDefault(for: .codex), "off", "the saved mapping is shown, not replaced by the global default")
        let claude = s.callerDefaultOptions(for: .claude)
        XCTAssertEqual(claude.map(\.value), ["", "work", "gone"])
        XCTAssertEqual(claude.last?.title, "gone (not found)")
        XCTAssertTrue(claude.contains { $0.value == s.callerDefault(for: .claude) }, "the selection always has a matching row")
    }

    func testPickersAreIndependentPerCaller() throws {
        let s = try CallerFixtures.snapshot([Fixtures.profile(name: "work"), Fixtures.profile(name: "alt")], callers: #"{"codex":"alt"}"#)
        XCTAssertEqual(s.callerDefault(for: .codex), "alt")
        XCTAssertEqual(s.callerDefault(for: .claude), "")
    }

    func testExplanationNamesTheRequirementAndOverrides() {
        let t = CallerDefaultPresentation.explanation
        XCTAssertTrue(t.contains("--caller"))
        XCTAssertTrue(t.contains("-p"))
        XCTAssertTrue(t.contains("OFFLOAD_PROFILE"))
        XCTAssertEqual(CallerDefaultPresentation.notice(.codex, profile: nil), "Codex callers now follow the global default.")
        XCTAssertEqual(CallerDefaultPresentation.notice(.claude, profile: "x"), "Claude callers now use \"x\".")
    }
}

final class CallerDefaultCommandTests: XCTestCase {
    func testArgumentsSetAndClear() {
        XCTAssertEqual(OffloadCLI.callerDefaultArguments(.codex, profile: "cx"), ["default", "--caller", "codex", "cx"])
        XCTAssertEqual(OffloadCLI.callerDefaultArguments(.claude, profile: "claude-sonnet"), ["default", "--caller", "claude", "claude-sonnet"])
        XCTAssertEqual(OffloadCLI.callerDefaultArguments(.claude, profile: nil), ["default", "--caller", "claude", "--clear"])
    }

    func testOptionLikeAndInvalidNamesNeverReachTheCommandLine() {
        for bad in ["--clear", "-x", "", "a\nb"] {
            XCTAssertNil(OffloadCLI.callerDefaultArguments(.codex, profile: bad), bad)
        }
    }

    func testCLISetAndClearRunTheDocumentedCommands() async throws {
        let fake = try FakeCLI()
        try await fake.cli.setCallerDefault(.codex, profile: "cx")
        try await fake.cli.setCallerDefault(.claude, profile: nil)
        XCTAssertEqual(fake.argv, ["default --caller codex cx", "default --caller claude --clear"])
    }

    func testCLIRejectsInvalidNameWithoutRunning() async throws {
        let fake = try FakeCLI()
        do {
            try await fake.cli.setCallerDefault(.codex, profile: "--agent")
            XCTFail("expected an invalid argument error")
        } catch let e as CLIError {
            guard case .invalidArgument = e else { return XCTFail("\(e)") }
        }
        XCTAssertTrue(fake.argv.isEmpty)
    }
}

@MainActor
final class CallerDefaultModelTests: XCTestCase {
    private func fake(_ callers: String? = nil, failDefault: Bool = false) throws -> FakeCLI {
        try FakeCLI(profilesJSON: CallerFixtures.json([Fixtures.profile(name: "work"), Fixtures.profile(name: "cx", backend: "codex", auth: "none")], callers: callers),
                    failDefault: failDefault)
    }

    func testSetRefreshesSnapshotAfterTheCommand() async throws {
        let f = try fake(#"{"codex":"cx"}"#)
        let m = ProfilesModel(cli: f.cli, autoLoad: false)
        let ok = await m.setCallerDefault(.codex, profile: "cx")
        XCTAssertTrue(ok)
        XCTAssertEqual(f.argv, ["default --caller codex cx", "profiles --json"])
        XCTAssertEqual(m.snapshot?.callerDefault(for: .codex), "cx")
        XCTAssertNil(m.actionError)
        XCTAssertFalse(m.busy)
    }

    func testClearRunsClearThenRefreshes() async throws {
        let f = try fake()
        let m = ProfilesModel(cli: f.cli, autoLoad: false)
        let ok = await m.setCallerDefault(.claude, profile: nil)
        XCTAssertTrue(ok)
        XCTAssertEqual(f.argv, ["default --caller claude --clear", "profiles --json"])
        XCTAssertEqual(m.snapshot?.callerDefault(for: .claude), "")
    }

    func testFailureShowsErrorKeepsSnapshotAndDoesNotRefresh() async throws {
        let f = try fake(#"{"claude":"work"}"#, failDefault: true)
        let m = ProfilesModel(cli: f.cli, autoLoad: false)
        let loaded = await m.reload()
        XCTAssertTrue(loaded)
        let before = m.snapshot
        let ok = await m.setCallerDefault(.claude, profile: "cx")
        XCTAssertFalse(ok)
        XCTAssertEqual(m.actionError, "unknown profile")
        XCTAssertEqual(m.snapshot, before, "a failed change leaves the previous choice on screen")
        XCTAssertEqual(f.argv, ["profiles --json", "default --caller claude cx"], "no refetch after a failed command")
        XCTAssertFalse(m.busy)
    }

    func testOptionLikeNameIsRefusedWithAMessage() async throws {
        let f = try fake()
        let m = ProfilesModel(cli: f.cli, autoLoad: false)
        let ok = await m.setCallerDefault(.codex, profile: "--clear")
        XCTAssertFalse(ok)
        XCTAssertTrue(f.argv.isEmpty)
        XCTAssertNotNil(m.actionError)
    }

    func testActionsAreSerialized() async throws {
        let f = try fake(#"{"codex":"cx"}"#)
        var script = try String(contentsOf: f.exe)
        script = script.replacingOccurrences(of: "echo \"$*\" >> \"$D/argv.log\"", with: "echo \"$*\" >> \"$D/argv.log\"\ncase \"$1\" in default) sleep 0.4 ;; esac")
        try script.write(to: f.exe, atomically: false, encoding: .utf8)
        let m = ProfilesModel(cli: f.cli, autoLoad: false)
        async let first = m.setCallerDefault(.codex, profile: "cx")
        try await Task.sleep(nanoseconds: 150_000_000)
        XCTAssertTrue(m.busy, "the model is busy while the command runs, which disables the pickers")
        let second = await m.setCallerDefault(.claude, profile: "work")
        XCTAssertFalse(second, "a second change is refused while one runs")
        let firstOK = await first
        XCTAssertTrue(firstOK)
        XCTAssertEqual(f.argv, ["default --caller codex cx", "profiles --json"], "the refused change never ran")
    }

    func testUnavailableCLIDoesNothing() async {
        let m = ProfilesModel(testingSnapshot: nil)
        let ok = await m.setCallerDefault(.codex, profile: "x")
        XCTAssertFalse(ok)
    }

    func testCallerDefaultChangeLeavesGlobalDefaultFieldsAlone() async throws {
        let f = try fake()
        let m = ProfilesModel(cli: f.cli, autoLoad: false)
        _ = await m.setCallerDefault(.codex, profile: "cx")
        XCTAssertFalse(f.argv.contains { $0 == "default cx" || $0.hasPrefix("default cx") }, "the global default command is never used")
        XCTAssertEqual(m.snapshot?.configuredDefault, "work")
    }
}

@MainActor
final class CallerDefaultLayoutTests: XCTestCase {
    func testSectionFitsSettingsWindowOffscreen() throws {
        let long = String(repeating: "very-long-profile-name-", count: 5)
        let entries = [Fixtures.profile(name: "work"), Fixtures.profile(name: "cx", backend: "codex", auth: "none"),
                       CallerFixtures.disabled(Fixtures.profile(name: "off")), Fixtures.profile(name: long, model: String(repeating: "model-", count: 12))]
        let snapshot = try CallerFixtures.snapshot(entries, callers: #"{"codex":"off","claude":"\#(long)"}"#)
        let model = ProfilesModel(testingSnapshot: snapshot)
        // The tallest forms: a gateway Claude profile and a Codex profile (account and Fast rows).
        let drafts = [("claude", ProfileDraft(editing: snapshot.profiles[0])), ("codex", ProfileDraft(editing: snapshot.profiles[1]))]
        for (kind, draft) in drafts {
        for scheme in [ColorScheme.light, .dark] {
            let size = NSSize(width: SettingsLayout.width, height: SettingsLayout.height - 56)
            let host = NSHostingView(rootView: ProfilesSettingsView(profiles: model, initialDraft: draft)
                .font(.system(size: 12)).environment(\.colorScheme, scheme)
                .background(Color(nsColor: .windowBackgroundColor))
                .frame(width: size.width, height: size.height))
            host.appearance = NSAppearance(named: scheme == .light ? .aqua : .darkAqua)
            host.frame = NSRect(origin: .zero, size: size)
            host.layoutSubtreeIfNeeded()
            XCTAssertLessThanOrEqual(host.fittingSize.width, SettingsLayout.width + 1)
            XCTAssertLessThanOrEqual(host.fittingSize.height, size.height + 1)
            host.displayIfNeeded()
            let bitmap = try XCTUnwrap(host.bitmapImageRepForCachingDisplay(in: host.bounds))
            host.cacheDisplay(in: host.bounds, to: bitmap)
            let data = try XCTUnwrap(bitmap.representation(using: .png, properties: [:]))
            XCTAssertGreaterThan(data.count, 1000)
            if let directory = ProcessInfo.processInfo.environment["OFFLOAD_SETTINGS_RENDER_DIR"] {
                let output = URL(fileURLWithPath: directory)
                try FileManager.default.createDirectory(at: output, withIntermediateDirectories: true)
                try data.write(to: output.appendingPathComponent("caller-defaults-\(kind)-\(scheme == .light ? "light" : "dark").png"))
            }
        }
        }
    }
}
