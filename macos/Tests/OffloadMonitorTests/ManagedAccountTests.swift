import XCTest
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
}
