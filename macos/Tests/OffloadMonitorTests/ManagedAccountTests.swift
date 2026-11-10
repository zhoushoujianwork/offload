import XCTest
import AppKit
import SwiftUI
@testable import OffloadMonitor

final class ManagedAccountTests: XCTestCase {
    func testManagedAccountDraftPatchAndClear() throws {
        var draft = ProfileDraft.new()
        draft.name = "codex-work"
        draft.backend = "codex"
        draft.codexAccount = "work"
        let request = try draft.request(existingNames: [])
        let json = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(request)) as? [String: Any])
        XCTAssertEqual(json["codex_account"] as? String, "work")
        XCTAssertNil(json["secret"])
        let entry = ProfileEntry(codexAccount: "work", name: "codex-work", backend: "codex", baseUrl: "", baseUrlRedacted: false, model: "", endpoint: "", reasoningEffort: "", serviceTier: "", authMode: "none")
        draft = ProfileDraft(editing: entry)
        XCTAssertFalse(draft.isDirty)
        draft.codexAccount = ""
        XCTAssertTrue(draft.isDirty)
        XCTAssertEqual(try draft.request(existingNames: []).codexAccount, "")
    }

    func testAccountCatalogDecode() throws {
        let data = Data(#"{"schema_version":1,"config_exists":true,"configured_default":"","active":"","active_source":"none","delegation_mode":"basic","profiles":[],"warnings":[],"managed_accounts":["work","personal"]}"#.utf8)
        XCTAssertEqual(try ProfilesSnapshot.decode(data).managedAccounts, ["work", "personal"])
    }

    @MainActor func testLinkPassesOnlyPathAndRefreshesWithoutProfileWrites() async throws {
        var object = try XCTUnwrap(JSONSerialization.jsonObject(with: Data(Fixtures.profiles([
            Fixtures.profile(name: "codex-work", backend: "codex", model: "saved-model", auth: "none")], active: "codex-work").utf8)) as? [String: Any])
        object["managed_accounts"] = ["work"]
        let snapshotJSON = String(decoding: try JSONSerialization.data(withJSONObject: object), as: UTF8.self)
        let fake = try FakeCLI(profilesJSON: snapshotJSON)
        var script = try String(contentsOf: fake.exe)
        let path = fake.dir.appendingPathComponent("external account/auth.json")
        script = script.replacingOccurrences(of: "case \"$1 $2\" in", with: """
        case "$1 $2" in
          "accounts link") test "$#" = 4 && test "$3" = work && test "$4" = '\(path.path)' || exit 65; echo '{}' ;;
        """)
        try script.write(to: fake.exe, atomically: false, encoding: .utf8)
        let model = ProfilesModel(cli: fake.cli, autoLoad: false)
        let loaded = await model.reload()
        XCTAssertTrue(loaded)
        let before = model.snapshot
        let linked = await model.linkAccount(name: "work", authURL: path)
        XCTAssertTrue(linked)
        XCTAssertEqual(fake.argv, ["profiles --json", "accounts link work \(path.path)", "profiles --json"])
        XCTAssertEqual(model.snapshot, before)
        XCTAssertEqual(model.snapshot?.managedAccounts, ["work"])
        XCTAssertEqual(fake.stdin, "")
        XCTAssertFalse(FileManager.default.fileExists(atPath: path.path), "the App must pass the pathname without reading or creating the source")
    }

    @MainActor func testLinkFailureKeepsSnapshotAndShowsError() async throws {
        let fake = try FakeCLI()
        let model = ProfilesModel(cli: fake.cli, autoLoad: false)
        let loaded = await model.reload()
        XCTAssertTrue(loaded)
        let before = model.snapshot
        let linked = await model.linkAccount(name: "work", authURL: URL(fileURLWithPath: "/fake/auth.json"))
        XCTAssertFalse(linked)
        XCTAssertNotNil(model.actionError)
        XCTAssertEqual(model.snapshot, before)
        XCTAssertFalse(model.busy)
    }

    @MainActor func testCodexLinkControlsFitOffscreen() throws {
        let json = Fixtures.profiles([
            Fixtures.profile(name: "codex-work", backend: "codex", auth: "none")], active: "codex-work")
            .replacingOccurrences(of: #""auth_mode":"none""#, with: #""auth_mode":"none","codex_account":"work""#)
        let snapshot = try ProfilesSnapshot.decode(Data(json.utf8))
        let model = ProfilesModel(testingSnapshot: snapshot)
        let draft = ProfileDraft(editing: snapshot.profiles[0])
        for scheme in [ColorScheme.light, .dark] {
            let host = NSHostingView(rootView: ProfilesSettingsView(profiles: model, initialDraft: draft)
                .font(.system(size: 12)).environment(\.colorScheme, scheme)
                .background(Color(nsColor: .windowBackgroundColor))
                // Match SettingsView's fixed outer proposal, rather than asking
                // the child for an unconstrained natural width.
                .frame(width: SettingsLayout.width, height: SettingsLayout.height - 56))
            host.appearance = NSAppearance(named: scheme == .light ? .aqua : .darkAqua)
            host.frame = NSRect(x: 0, y: 0, width: SettingsLayout.width, height: SettingsLayout.height - 56)
            host.layoutSubtreeIfNeeded()
            XCTAssertLessThanOrEqual(host.fittingSize.width, SettingsLayout.width + 1)
            XCTAssertLessThanOrEqual(host.fittingSize.height, SettingsLayout.height - 55)
            host.layoutSubtreeIfNeeded()
            host.displayIfNeeded()
            let bitmap = try XCTUnwrap(host.bitmapImageRepForCachingDisplay(in: host.bounds))
            host.cacheDisplay(in: host.bounds, to: bitmap)
            let data = try XCTUnwrap(bitmap.representation(using: .png, properties: [:]))
            XCTAssertGreaterThan(data.count, 1000)
            if let directory = ProcessInfo.processInfo.environment["OFFLOAD_SETTINGS_RENDER_DIR"] {
                let output = URL(fileURLWithPath: directory)
                try FileManager.default.createDirectory(at: output, withIntermediateDirectories: true)
                try data.write(to: output.appendingPathComponent("codex-linked-\(scheme == .light ? "light" : "dark").png"))
            }
        }
    }
}
