import XCTest
@testable import OffloadMonitor

final class ProfileLifecycleTests: XCTestCase {
    func testDisabledPatchEncoding() throws {
        for value in [false, true] {
            let request = ProfileSaveRequest(name: "work", create: false, disabled: value)
            let object = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(request)) as? [String: Any])
            XCTAssertEqual(object["name"] as? String, "work")
            XCTAssertEqual(object["disabled"] as? Bool, value)
            XCTAssertNil(object["codex_account"])
            XCTAssertNil(object["model"])
            XCTAssertFalse(request.isEmptyPatch)
        }
        let request = ProfileSaveRequest(name: "work", create: false)
        let object = try XCTUnwrap(JSONSerialization.jsonObject(with: JSONEncoder().encode(request)) as? [String: Any])
        XCTAssertNil(object["disabled"])
        XCTAssertTrue(request.isEmptyPatch)
    }

    func testDeleteArguments() {
        XCTAssertEqual(ProfileDeleteRequest(name: "work").arguments, ["profile", "delete", "work", "--json"])
        XCTAssertEqual(ProfileDeleteRequest(name: "work", account: true).arguments,
                       ["profile", "delete", "work", "--account", "--json"])
    }

    func testDisabledDecodeAndPresentation() throws {
        let original = Fixtures.profiles([Fixtures.profile(name: "work", backend: "codex", model: "", auth: "none")], active: "work")
        let older = try ProfilesSnapshot.decode(Data(original.utf8))
        XCTAssertNil(older.profiles[0].disabled)
        XCTAssertFalse(older.profiles[0].isDisabled)
        var object = try XCTUnwrap(JSONSerialization.jsonObject(with: Data(original.utf8)) as? [String: Any])
        var entries = try XCTUnwrap(object["profiles"] as? [[String: Any]])
        entries[0]["disabled"] = true
        entries[0]["available"] = false
        object["profiles"] = entries
        let entry = try ProfilesSnapshot.decode(JSONSerialization.data(withJSONObject: object)).profiles[0]
        XCTAssertTrue(entry.isDisabled)
        XCTAssertFalse(entry.isUnavailable)
        XCTAssertEqual(ProfilePresentation.pickerTitle(entry), "work (disabled)")
    }

    @MainActor func testDeleteUsesCLIAndRefreshesSnapshot() async throws {
        let json = Fixtures.profiles([Fixtures.profile(name: "work", backend: "codex", model: "", auth: "none")], active: "work")
        let fake = try FakeCLI(profilesJSON: json)
        var script = try String(contentsOf: fake.exe)
        script = script.replacingOccurrences(of: "case \"$1 $2\" in", with: """
        case "$1 $2" in
          "profile delete") test "$#" = 5 && test "$3" = work && test "$4" = --account && test "$5" = --json || exit 65; if [ -f "$D/delete.fail" ]; then echo "offload: profile is in use" >&2; exit 2; fi; cat "$D/profiles.json";;
        """)
        try script.write(to: fake.exe, atomically: false, encoding: .utf8)
        let model = ProfilesModel(cli: fake.cli, autoLoad: false)
        let success = await model.delete(ProfileDeleteRequest(name: "work", account: true))
        XCTAssertTrue(success)
        XCTAssertEqual(model.snapshot?.profiles.first?.name, "work")
        XCTAssertNil(model.actionError)
        XCTAssertFalse(model.busy)
        let before = model.snapshot
        FileManager.default.createFile(atPath: fake.dir.appendingPathComponent("delete.fail").path, contents: Data())
        let refused = await model.delete(ProfileDeleteRequest(name: "work", account: true))
        XCTAssertFalse(refused)
        XCTAssertEqual(model.snapshot, before)
        XCTAssertTrue(model.actionError?.contains("profile is in use") == true)
        XCTAssertFalse(model.busy)
    }
}
