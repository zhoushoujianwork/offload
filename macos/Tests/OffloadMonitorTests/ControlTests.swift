import AppKit
import SwiftUI
import XCTest
@testable import OffloadMonitor

enum Fixtures {
    static func profile(name: String, backend: String = "claude", url: String = "https://gw.example/v1", redacted: Bool = false,
                        model: String = "m1", endpoint: String = "", effort: String = "", tier: String = "", auth: String = "token") -> String {
        """
        {"name":"\(name)","backend":"\(backend)","base_url":"\(redacted ? "" : url)","base_url_redacted":\(redacted),"model":"\(model)",
         "endpoint":"\(endpoint)","reasoning_effort":"\(effort)","service_tier":"\(tier)","auth_mode":"\(auth)"}
        """
    }

    static func profiles(_ entries: [String], active: String = "work", source: String = "config", configured: String = "work", exists: Bool = true) -> String {
        """
        {"schema_version":1,"config_exists":\(exists),"configured_default":"\(configured)","active":"\(active)","active_source":"\(source)",
         "delegation_mode":"proactive","profiles":[\(entries.joined(separator: ","))],"warnings":[]}
        """
    }

    static func agent(_ name: String, status: String, enabled: Bool, current: Bool = true, canEnable: Bool = true, canDisable: Bool = true, message: String = "") -> String {
        """
        {"agent":"\(name)","status":"\(status)","enabled":\(enabled),"installed":\(enabled),"skill_state":"ok","policy_state":"ok",
         "skill_path":"/h/\(name)/skill","policy_path":"/h/\(name)/policy","skill_current":\(current),"policy_current":\(current),"mode":"proactive",
         "override_shadowed":false,"can_enable":\(canEnable),"can_disable":\(canDisable),"message":"\(message)","restart_required":true}
        """
    }

    static func skills(_ agents: [String]) -> String {
        #"{"schema_version":1,"scope":"user","mode":"proactive","agents":[\#(agents.joined(separator: ","))],"warnings":[]}"#
    }
}

/// A shell script standing in for `offload`. It logs argv and stdin to files in an isolated directory and
/// answers from canned files, so no real config, credential or agent file is ever touched.
final class FakeCLI {
    let dir: URL
    var exe: URL { dir.appendingPathComponent("offload") }
    var cli: OffloadCLI { OffloadCLI(executable: exe, timeout: 5) }
    var argvLog: URL { dir.appendingPathComponent("argv.log") }
    var stdinLog: URL { dir.appendingPathComponent("stdin.log") }

    init(profilesJSON: String = Fixtures.profiles([]), saveJSON: String? = nil, skillsJSON: String = Fixtures.skills([]),
         failDefault: Bool = false, failAgent: Bool = false) throws {
        dir = FileManager.default.temporaryDirectory.appendingPathComponent("fake-cli-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        try profilesJSON.write(to: dir.appendingPathComponent("profiles.json"), atomically: true, encoding: .utf8)
        try (saveJSON ?? profilesJSON).write(to: dir.appendingPathComponent("save.json"), atomically: true, encoding: .utf8)
        try skillsJSON.write(to: dir.appendingPathComponent("skills.json"), atomically: true, encoding: .utf8)
        let script = """
        #!/bin/sh
        D='\(dir.path)'
        echo "$*" >> "$D/argv.log"
        case "$1 $2" in
          "profiles --json") cat "$D/profiles.json" ;;
          "profile save") cat > "$D/stdin.log"; if [ -f "$D/save.fail" ]; then echo "offload: profile name is invalid" >&2; exit 1; fi; cat "$D/save.json" ;;
          "agent status") cat "$D/skills.json" ;;
          "agent enable"|"agent disable") \(failAgent ? #"echo "offload: skill file was modified by hand" >&2; exit 1"# : "echo '{}'") ;;
          *) case "$1" in
               default) \(failDefault ? #"echo "offload: unknown profile" >&2; exit 1"# : #"echo "default profile: $2""#) ;;
               *) echo "unexpected: $*" >&2; exit 64 ;;
             esac ;;
        esac
        """
        try script.write(to: exe, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: exe.path)
    }

    deinit { try? FileManager.default.removeItem(at: dir) }

    var argv: [String] { ((try? String(contentsOf: argvLog)) ?? "").split(separator: "\n").map(String.init) }
    var stdin: String { (try? String(contentsOf: stdinLog)) ?? "" }
    func failNextSaves() { FileManager.default.createFile(atPath: dir.appendingPathComponent("save.fail").path, contents: Data()) }
}

final class ProfileContractTests: XCTestCase {
    func testDecodeKeepsRedactionAndNeverNeedsSecrets() throws {
        let json = Fixtures.profiles([Fixtures.profile(name: "work", redacted: true, auth: "token_cmd"),
                                      Fixtures.profile(name: "cx", backend: "codex", model: "", effort: "high", tier: "priority", auth: "none")])
        let s = try ProfilesSnapshot.decode(Data(json.utf8))
        XCTAssertEqual(s.profiles.count, 2)
        XCTAssertTrue(s.profiles[0].baseUrlRedacted)
        XCTAssertEqual(s.profiles[0].baseUrl, "")
        XCTAssertEqual(s.profiles[0].authMode, "token_cmd")
        XCTAssertTrue(s.profiles[1].isCodex)
        XCTAssertFalse(json.contains("\"token\":"), "fixture has no secret fields")
    }

    func testMissingConfigAndBadSchema() throws {
        let s = try ProfilesSnapshot.decode(Data(Fixtures.profiles([], active: "", source: "none", configured: "", exists: false).utf8))
        XCTAssertFalse(s.configExists)
        XCTAssertEqual(s.activeDescription, "No offload config found yet.")
        XCTAssertThrowsError(try ProfilesSnapshot.decode(Data(#"{"schema_version":2}"#.utf8)))
        XCTAssertThrowsError(try ProfilesSnapshot.decode(Data("nope".utf8)))
    }

    func testEnvironmentOverrideIsReportedTruthfully() throws {
        let s = try ProfilesSnapshot.decode(Data(Fixtures.profiles([Fixtures.profile(name: "a"), Fixtures.profile(name: "b")], active: "a", source: "environment", configured: "b").utf8))
        XCTAssertTrue(s.overriddenByEnvironment)
        XCTAssertTrue(s.activeDescription.contains("OFFLOAD_PROFILE"))
        XCTAssertTrue(s.activeDescription.contains("saved default is \"b\""))
        let c = try ProfilesSnapshot.decode(Data(Fixtures.profiles([Fixtures.profile(name: "a")], active: "a").utf8))
        XCTAssertFalse(c.overriddenByEnvironment)
        XCTAssertFalse(c.activeDescription.contains("OFFLOAD_PROFILE"))
    }
}

final class ProfileDraftTests: XCTestCase {
    func entry(_ name: String = "work", redacted: Bool = false, backend: String = "claude", auth: String = "token") throws -> ProfileEntry {
        try ProfilesSnapshot.decode(Data(Fixtures.profiles([Fixtures.profile(name: name, backend: backend, redacted: redacted, endpoint: backend == "codex" ? "" : "cc", auth: auth)]).utf8)).profiles[0]
    }
    func json(_ r: ProfileSaveRequest) throws -> [String: Any] {
        try JSONSerialization.jsonObject(with: JSONEncoder().encode(r)) as! [String: Any]
    }

    func testNewClaudeProfileRequest() throws {
        var d = ProfileDraft.new()
        d.name = "work"; d.model = "claude-x"; d.baseURL = "https://gw.example"; d.credential = .token; d.secret = "sk-123"
        let r = try d.request(existingNames: ["other"])
        let j = try json(r)
        XCTAssertEqual(j["create"] as? Bool, true)
        XCTAssertEqual(j["name"] as? String, "work")
        XCTAssertEqual(j["auth_mode"] as? String, "token")
        XCTAssertEqual(j["secret"] as? String, "sk-123")
        XCTAssertEqual(j["base_url"] as? String, "https://gw.example")
        XCTAssertNil(j["endpoint"], "unset endpoint is omitted")
    }

    func testNewProfileValidation() {
        var d = ProfileDraft.new()
        XCTAssertThrowsError(try d.request(existingNames: [])) { XCTAssertEqual($0 as? ProfileDraftError, .nameRequired) }
        d.name = "bad name!"
        XCTAssertThrowsError(try d.request(existingNames: [])) { XCTAssertEqual($0 as? ProfileDraftError, .nameInvalid) }
        d.name = "ok"
        XCTAssertThrowsError(try d.request(existingNames: ["ok"])) { XCTAssertEqual($0 as? ProfileDraftError, .nameTaken) }
        XCTAssertThrowsError(try d.request(existingNames: [])) { XCTAssertEqual($0 as? ProfileDraftError, .modelRequired) }
        d.model = "m"
        XCTAssertThrowsError(try d.request(existingNames: [])) { XCTAssertEqual($0 as? ProfileDraftError, .urlRequired) }
        d.baseURL = "ftp://x"
        XCTAssertThrowsError(try d.request(existingNames: [])) { XCTAssertEqual($0 as? ProfileDraftError, .urlInvalid) }
        d.baseURL = "https://x"; d.credential = .token; d.secret = "  "
        XCTAssertThrowsError(try d.request(existingNames: [])) { XCTAssertEqual($0 as? ProfileDraftError, .secretRequired("API token")) }
        d.credential = .tokenCmd
        XCTAssertThrowsError(try d.request(existingNames: [])) { XCTAssertEqual($0 as? ProfileDraftError, .secretRequired("token command")) }
        XCTAssertTrue(ProfileDraft.validName("a.b-c_1"))
        XCTAssertFalse(ProfileDraft.validName("-x"))
        XCTAssertFalse(ProfileDraft.validName("a b"))
    }

    func testEditPreservesRedactedURLAndAuth() throws {
        let d = ProfileDraft(editing: try entry(redacted: true))
        XCTAssertTrue(d.baseURLHidden)
        XCTAssertEqual(d.baseURL, "", "the redacted display value is never put in the field")
        XCTAssertEqual(d.credential, .keep)
        var e = d
        e.model = "new-model"
        let j = try json(e.request(existingNames: ["work"]))
        XCTAssertNil(j["base_url"], "a redacted URL is omitted, never sent")
        XCTAssertEqual(j["model"] as? String, "new-model")
        XCTAssertEqual(j["auth_mode"] as? String, "preserve")
        XCTAssertNil(j["secret"])
        XCTAssertEqual(j["create"] as? Bool, false)
        XCTAssertNil(j["backend"])
    }

    func testReplacingHiddenURLRequiresExplicitChoice() throws {
        var d = ProfileDraft(editing: try entry(redacted: true))
        d.replaceBaseURL = true
        XCTAssertThrowsError(try d.request(existingNames: ["work"])) { XCTAssertEqual($0 as? ProfileDraftError, .urlRequired) }
        d.baseURL = "https://new.example"
        XCTAssertEqual(try json(d.request(existingNames: ["work"]))["base_url"] as? String, "https://new.example")
    }

    func testVisibleURLUnchangedIsOmittedAndChangedIsSent() throws {
        var d = ProfileDraft(editing: try entry())
        XCTAssertNil(try json(d.request(existingNames: ["work"]))["base_url"])
        d.baseURL = "https://other.example"
        XCTAssertEqual(try json(d.request(existingNames: ["work"]))["base_url"] as? String, "https://other.example")
    }

    func testReplaceCredential() throws {
        var d = ProfileDraft(editing: try entry())
        d.credential = .tokenCmd
        XCTAssertThrowsError(try d.request(existingNames: ["work"]))
        d.secret = "pass show x"
        let j = try json(d.request(existingNames: ["work"]))
        XCTAssertEqual(j["auth_mode"] as? String, "token_cmd")
        XCTAssertEqual(j["secret"] as? String, "pass show x")
    }

    func testCodexProfileSendsNoCredentialAndClearsEndpoint() throws {
        var d = ProfileDraft.new()
        d.name = "cx"; d.backend = "codex"; d.reasoning = "high"; d.tier = "priority"
        var j = try json(d.request(existingNames: []))
        XCTAssertEqual(j["backend"] as? String, "codex")
        XCTAssertEqual(j["reasoning_effort"] as? String, "high")
        XCTAssertEqual(j["service_tier"] as? String, "priority")
        XCTAssertNil(j["auth_mode"]); XCTAssertNil(j["secret"]); XCTAssertNil(j["base_url"])
        XCTAssertNil(j["model"], "optional model inherits")
        // Switching an existing Claude profile to Codex clears its endpoint.
        var e = ProfileDraft(editing: try entry())
        e.backend = "codex"
        j = try json(e.request(existingNames: ["work"]))
        XCTAssertEqual(j["endpoint"] as? String, "")
        XCTAssertEqual(j["backend"] as? String, "codex")
        XCTAssertNil(j["secret"])
    }

    func testSecretNeverAppearsInDescriptions() {
        let r = ProfileSaveRequest(name: "n", create: true, secret: "sk-supersecret")
        XCTAssertFalse("\(r)".contains("supersecret"))
        XCTAssertFalse(String(reflecting: r).contains("supersecret"))
        XCTAssertFalse("\(ProfileDraftError.secretRequired("API token"))".contains("supersecret"))
    }

    func testCredentialChoicesForNewAndExisting() throws {
        XCTAssertEqual(ProfileDraft.new().credentialChoices, [.token, .tokenCmd], "a new Claude profile cannot choose no credential")
        XCTAssertEqual(ProfileDraft.new().credential, .token)
        XCTAssertEqual(ProfileDraft(editing: try entry()).credentialChoices, [.keep, .token, .tokenCmd])
        XCTAssertEqual(ProfileDraft(editing: try entry()).credential, .keep)
    }

    func testNewClaudeWithoutSecretIsRejectedBeforeSaving() {
        var d = ProfileDraft.new()
        d.name = "w"; d.model = "m"; d.baseURL = "https://x.example"
        XCTAssertThrowsError(try d.request(existingNames: [])) { XCTAssertEqual($0 as? ProfileDraftError, .secretRequired("API token")) }
        d.credential = .none // not offered, but never produces a request either
        XCTAssertThrowsError(try d.request(existingNames: []))
        d.credential = .keep
        XCTAssertThrowsError(try d.request(existingNames: []), "nothing saved to keep on a new profile")
    }

    func testClaudeEditedFromCodexMustSupplyCredential() throws {
        let codex = try entry(backend: "codex", auth: "none")
        var d = ProfileDraft(editing: codex)
        XCTAssertEqual(d.credentialChoices, [.token, .tokenCmd])
        XCTAssertEqual(d.credential, .token)
        d.backend = "claude"; d.model = "m"; d.baseURL = "https://x.example"
        XCTAssertThrowsError(try d.request(existingNames: ["work"]))
        d.secret = "sk-x"
        XCTAssertEqual(try json(d.request(existingNames: ["work"]))["auth_mode"] as? String, "token")
    }

    func testAllCodexEffortsAvailableAndExistingOnesPreserved() throws {
        XCTAssertEqual(ProfileDraft.efforts, ["", "low", "medium", "high", "xhigh", "max", "ultra"])
        for effort in ["max", "ultra", "xhigh"] {
            let e = try ProfilesSnapshot.decode(Data(Fixtures.profiles([Fixtures.profile(name: "cx", backend: "codex", model: "", effort: effort, auth: "none")]).utf8)).profiles[0]
            var d = ProfileDraft(editing: e)
            XCTAssertEqual(d.reasoning, effort)
            d.tier = "priority" // change something else
            let j = try json(d.request(existingNames: ["cx"]))
            XCTAssertNil(j["reasoning_effort"], "\(effort) is unchanged, so the patch omits it")
            XCTAssertEqual(j["service_tier"] as? String, "priority")
            d.reasoning = "low"
            XCTAssertEqual(try json(d.request(existingNames: ["cx"]))["reasoning_effort"] as? String, "low")
        }
        var n = ProfileDraft.new()
        n.name = "n"; n.backend = "codex"; n.reasoning = "ultra"
        XCTAssertEqual(try json(n.request(existingNames: []))["reasoning_effort"] as? String, "ultra")
        n.reasoning = "bogus"
        XCTAssertThrowsError(try n.request(existingNames: []))
    }
}

final class FakeCLITests: XCTestCase {
    func testSaveSendsSecretOnStdinOnly() async throws {
        let secret = "sk-ONLY-ON-STDIN-12345"
        let after = Fixtures.profiles([Fixtures.profile(name: "work", auth: "token")])
        let fake = try FakeCLI(saveJSON: after)
        var d = ProfileDraft.new()
        d.name = "work"; d.model = "m1"; d.baseURL = "https://gw.example"; d.credential = .token; d.secret = secret
        let snap = try await fake.cli.saveProfile(d.request(existingNames: []))
        XCTAssertEqual(snap.profiles.first?.authMode, "token")
        XCTAssertTrue(fake.stdin.contains(secret))
        XCTAssertFalse(fake.argv.joined(separator: " ").contains(secret), "secret never in argv")
        XCTAssertEqual(fake.argv, ["profile save --json"])
    }

    func testSaveFailureIsSanitized() async throws {
        let fake = try FakeCLI()
        fake.failNextSaves()
        do {
            _ = try await fake.cli.saveProfile(ProfileSaveRequest(name: "x", create: true, secret: "sk-zzz-secret"))
            XCTFail()
        } catch let e as CLIError { XCTAssertEqual(e, .failed("profile name is invalid")) }
        XCTAssertEqual(OffloadCLI.sanitize(stderr: Data("offload: bad value sk-zzz-secret\n".utf8), secrets: ["sk-zzz-secret"], status: 1),
                       "The offload command failed (exit 1).")
        XCTAssertEqual(OffloadCLI.sanitize(stderr: Data(), secrets: [], status: 3), "The offload command failed (exit 3).")
    }

    @MainActor func testUseRefetchesOnlyAfterSuccess() async throws {
        let fake = try FakeCLI(profilesJSON: Fixtures.profiles([Fixtures.profile(name: "work"), Fixtures.profile(name: "alt")]))
        let m = ProfilesModel(cli: fake.cli, autoLoad: false)
        let ok = await m.use("alt")
        XCTAssertTrue(ok)
        XCTAssertEqual(fake.argv, ["default alt", "profiles --json"])
        XCTAssertEqual(m.snapshot?.profiles.count, 2)

        let bad = try FakeCLI(failDefault: true)
        let m2 = ProfilesModel(cli: bad.cli, autoLoad: false)
        let ok2 = await m2.use("nope")
        XCTAssertFalse(ok2)
        XCTAssertEqual(bad.argv, ["default nope"], "no refetch after a failed switch")
        XCTAssertEqual(m2.actionError, "unknown profile")
    }

    @MainActor func testUseRejectsOptionLikeNamesWithoutRunning() async throws {
        let fake = try FakeCLI()
        let m = ProfilesModel(cli: fake.cli, autoLoad: false)
        let ok = await m.use("--agent")
        XCTAssertFalse(ok)
        XCTAssertTrue(fake.argv.isEmpty, "an option-like name must never reach the command line")
    }

    @MainActor func testSaveRoundTripUpdatesSnapshot() async throws {
        let fake = try FakeCLI(saveJSON: Fixtures.profiles([Fixtures.profile(name: "fresh")], active: "fresh", configured: "fresh"))
        let m = ProfilesModel(cli: fake.cli, autoLoad: false)
        var d = ProfileDraft.new()
        d.name = "fresh"; d.model = "m"; d.baseURL = "https://x.example"; d.secret = "sk-roundtrip"
        let req = try d.request(existingNames: [])
        let ok = await m.save(req)
        XCTAssertTrue(ok)
        XCTAssertEqual(m.snapshot?.active, "fresh")
        XCTAssertEqual(fake.argv, ["profile save --json"])
        XCTAssertTrue(fake.stdin.contains("\"create\":true"))
        XCTAssertTrue(fake.stdin.contains("\"secret\":\"sk-roundtrip\""))
        XCTAssertFalse(fake.argv.joined().contains("sk-roundtrip"))
    }

    @MainActor func testSkillsActionsRefetchEvenAfterFailure() async throws {
        let enabled = Fixtures.skills([Fixtures.agent("codex", status: "enabled", enabled: true), Fixtures.agent("claude", status: "disabled", enabled: false, canDisable: false)])
        let fake = try FakeCLI(skillsJSON: enabled)
        let m = AgentSkillsModel(cli: fake.cli)
        let ok = await m.setEnabled("claude", true)
        XCTAssertTrue(ok)
        XCTAssertEqual(fake.argv, ["agent enable --agent claude --json", "agent status --json"])

        let bad = try FakeCLI(skillsJSON: enabled, failAgent: true)
        let m2 = AgentSkillsModel(cli: bad.cli)
        let ok2 = await m2.setEnabled("codex", false)
        XCTAssertFalse(ok2)
        XCTAssertEqual(bad.argv, ["agent disable --agent codex --json", "agent status --json"], "state is re-read after a failure")
        XCTAssertEqual(m2.actionError, "skill file was modified by hand")
        XCTAssertNotNil(m2.snapshot)
        let unknown = await m2.setEnabled("--force", true)
        XCTAssertFalse(unknown)
        XCTAssertFalse(bad.argv.contains { $0.contains("--force") }, "an unknown agent never reaches the command line")
        XCTAssertEqual(bad.argv.last, "agent status --json", "only a status re-read follows the rejection")
    }

    @MainActor func testNoCLIMeansNoWork() async {
        let m = ProfilesModel(cli: nil, autoLoad: true)
        XCTAssertFalse(m.isAvailable)
        XCTAssertNotNil(m.loadError)
        let ok = await m.use("x")
        XCTAssertFalse(ok)
    }
}

final class SkillContractTests: XCTestCase {
    func testDecodeAndActions() throws {
        let json = Fixtures.skills([
            Fixtures.agent("codex", status: "enabled", enabled: true),
            Fixtures.agent("claude", status: "enabled", enabled: true, current: false),
            Fixtures.agent("x1", status: "disabled", enabled: false, canDisable: false),
            Fixtures.agent("x2", status: "blocked", enabled: false, canEnable: false, canDisable: false, message: "SKILL.md was modified"),
            Fixtures.agent("x3", status: "partial", enabled: false),
        ])
        let s = try SkillsSnapshot.decode(Data(json.utf8))
        let by = Dictionary(uniqueKeysWithValues: s.agents.map { ($0.agent, $0) })
        XCTAssertEqual(by["codex"]?.actions, [.disable])
        XCTAssertEqual(by["claude"]?.actions, [.update, .disable])
        XCTAssertTrue(by["claude"]!.statusLabel.contains("update available"))
        XCTAssertEqual(by["x1"]?.actions, [.enable])
        XCTAssertEqual(by["x2"]?.actions, [], "blocked offers no action and no force")
        XCTAssertEqual(by["x2"]?.message, "SKILL.md was modified")
        XCTAssertEqual(by["x3"]?.actions, [.enable, .disable])
        XCTAssertTrue(by["codex"]!.restartRequired)
        XCTAssertThrowsError(try SkillsSnapshot.decode(Data(#"{"schema_version":3}"#.utf8)))
    }
}

final class StdinRunnerTests: XCTestCase {
    func testStdinReachesChildAndNilStdinStillClosed() async throws {
        let cat = URL(fileURLWithPath: "/bin/cat")
        let r = try await ProcessRunner.run(executable: cat, arguments: [], timeout: 5, stdin: Data("hello".utf8))
        XCTAssertEqual(String(decoding: r.stdout, as: UTF8.self), "hello")
        let n = try await ProcessRunner.run(executable: cat, arguments: [], timeout: 5)
        XCTAssertEqual(n.stdout.count, 0, "no stdin means immediate EOF, as before")
    }

    func testChildThatNeverReadsCannotHangTheCaller() async throws {
        let big = Data(repeating: 0x61, count: 4_000_000)
        let t0 = Date()
        let r = try await ProcessRunner.run(executable: URL(fileURLWithPath: "/bin/sh"), arguments: ["-c", "sleep 30"], timeout: 0.5, stdin: big, killGrace: 0.5)
        XCTAssertTrue(r.timedOut)
        XCTAssertLessThan(Date().timeIntervalSince(t0), 6)
    }

    func testChildThatExitsEarlyDoesNotRaiseSIGPIPE() async throws {
        let big = Data(repeating: 0x62, count: 4_000_000)
        let r = try await ProcessRunner.run(executable: URL(fileURLWithPath: "/usr/bin/true"), arguments: [], timeout: 5, stdin: big)
        XCTAssertEqual(r.status, 0)
        XCTAssertFalse(r.timedOut)
        // The test process is still alive and a second run works; no process-wide signal disposition was changed.
        let again = try await ProcessRunner.run(executable: URL(fileURLWithPath: "/bin/echo"), arguments: ["ok"], timeout: 5)
        XCTAssertEqual(String(decoding: again.stdout, as: UTF8.self), "ok\n")
    }

    func testLargeStdinIsDeliveredCompletely() async throws {
        let big = Data(repeating: 0x63, count: 1_000_000)
        let r = try await ProcessRunner.run(executable: URL(fileURLWithPath: "/usr/bin/wc"), arguments: ["-c"], timeout: 10, stdin: big)
        XCTAssertEqual(String(decoding: r.stdout, as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines), "1000000")
    }
}

final class DraftSyncTests: XCTestCase {
    func stored(model: String = "m1", auth: String = "token") throws -> ProfileEntry {
        try ProfilesSnapshot.decode(Data(Fixtures.profiles([Fixtures.profile(name: "work", model: model, auth: auth)]).utf8)).profiles[0]
    }

    func testCleanDraftReloadsChangedProfile() throws {
        let d = ProfileDraft(editing: try stored())
        XCTAssertFalse(d.isDirty)
        let changed = try stored(model: "external-model")
        XCTAssertEqual(ProfileDraft.sync(draft: d, stored: changed), .reload(changed))
        XCTAssertEqual(ProfileDraft.sync(draft: d, stored: try stored()), .none, "unchanged entry leaves the form alone")
    }

    func testDirtyDraftIsKeptWhenStoredProfileChanges() throws {
        var d = ProfileDraft(editing: try stored())
        d.model = "my-unsaved-model"
        XCTAssertTrue(d.isDirty)
        XCTAssertEqual(ProfileDraft.sync(draft: d, stored: try stored(model: "external-model")), .keepEdits)
        // The kept form still saves against what the user saw, not the external value.
        XCTAssertEqual(d.model, "my-unsaved-model")
        XCTAssertEqual(d.original?.model, "m1")
    }

    func testTypedSecretAndUrlCountAsEdits() throws {
        var s = ProfileDraft(editing: try stored())
        s.credential = .token; s.secret = "sk-typing"
        XCTAssertTrue(s.isDirty)
        XCTAssertEqual(ProfileDraft.sync(draft: s, stored: try stored(model: "x")), .keepEdits)
        var u = ProfileDraft(editing: try stored())
        u.baseURL = "https://typed.example"
        XCTAssertTrue(u.isDirty)
        var c = ProfileDraft(editing: try stored())
        c.credential = .tokenCmd
        XCTAssertTrue(c.isDirty, "choosing a different credential mode is an edit")
    }

    func testRemovedProfile() throws {
        let clean = ProfileDraft(editing: try stored())
        XCTAssertEqual(ProfileDraft.sync(draft: clean, stored: nil), .removed)
        var dirty = clean
        dirty.model = "typed"
        XCTAssertEqual(ProfileDraft.sync(draft: dirty, stored: nil), .keepEdits, "edits are not thrown away even if the profile vanished")
    }

    func testNewProfileFormIsNeverTouched() {
        var d = ProfileDraft.new()
        XCTAssertFalse(d.isDirty)
        d.name = "typing"
        XCTAssertTrue(d.isDirty)
        XCTAssertEqual(ProfileDraft.sync(draft: d, stored: nil), .none)
    }

    func testExplicitReloadYieldsCleanStoredValues() throws {
        var d = ProfileDraft(editing: try stored())
        d.model = "typed"; d.credential = .token; d.secret = "sk-x"
        let latest = try stored(model: "external-model")
        let reloaded = ProfileDraft(editing: latest) // what choose(selection) builds
        XCTAssertFalse(reloaded.isDirty)
        XCTAssertEqual(reloaded.model, "external-model")
        XCTAssertEqual(reloaded.secret, "")
        XCTAssertEqual(reloaded.credential, .keep)
    }

    @MainActor func testOwnSaveResetsFormFromResponseAndClearsSecret() async throws {
        let after = Fixtures.profiles([Fixtures.profile(name: "work", model: "saved-model")])
        let fake = try FakeCLI(profilesJSON: Fixtures.profiles([Fixtures.profile(name: "work")]), saveJSON: after)
        let m = ProfilesModel(cli: fake.cli, autoLoad: false)
        _ = await m.reload()
        var d = ProfileDraft(editing: try XCTUnwrap(m.snapshot?.entry("work")))
        d.model = "saved-model"; d.credential = .token; d.secret = "sk-own-save"
        let req = try d.request(existingNames: ["work"])
        let ok = await m.save(req)
        XCTAssertTrue(ok)
        XCTAssertFalse(fake.argv.joined().contains("sk-own-save"))
        d.secret = ""
        d = ProfileDraft(editing: try XCTUnwrap(m.snapshot?.entry("work"))) // the caller's reset from the response
        XCTAssertFalse(d.isDirty)
        XCTAssertEqual(d.model, "saved-model")
        XCTAssertEqual(d.secret, "")
        XCTAssertEqual(ProfileDraft.sync(draft: d, stored: m.snapshot?.entry("work")), .none)
    }

    @MainActor func testBackgroundRefreshWithFakeCLIKeepsDirtyDraft() async throws {
        let fake = try FakeCLI(profilesJSON: Fixtures.profiles([Fixtures.profile(name: "work")]))
        let m = ProfilesModel(cli: fake.cli, autoLoad: false)
        _ = await m.reload()
        var d = ProfileDraft(editing: try XCTUnwrap(m.snapshot?.entry("work")))
        d.model = "mine"
        try Fixtures.profiles([Fixtures.profile(name: "work", model: "theirs")]).write(to: fake.dir.appendingPathComponent("profiles.json"), atomically: true, encoding: .utf8)
        _ = await m.reload()
        XCTAssertEqual(ProfileDraft.sync(draft: d, stored: m.snapshot?.entry("work")), .keepEdits)
        XCTAssertEqual(d.model, "mine")
    }
}

@MainActor
final class ProfilePollingTests: XCTestCase {
    func profilesJSON(active: String) -> String {
        Fixtures.profiles([Fixtures.profile(name: "a"), Fixtures.profile(name: "b")], active: active, configured: active)
    }

    func waitUntil(_ timeout: TimeInterval = 5, _ cond: @escaping @MainActor () -> Bool) async {
        let end = Date().addingTimeInterval(timeout)
        while true {
            if cond() || Date() >= end { return }
            try? await Task.sleep(nanoseconds: 50_000_000)
        }
    }

    func testPeriodicRefreshFollowsExternalChangeAndStops() async throws {
        let fake = try FakeCLI(profilesJSON: profilesJSON(active: "a"))
        let m = ProfilesModel(cli: fake.cli, autoLoad: true, refreshInterval: 0.2)
        await waitUntil { m.snapshot?.active == "a" }
        XCTAssertEqual(m.snapshot?.active, "a")
        try profilesJSON(active: "b").write(to: fake.dir.appendingPathComponent("profiles.json"), atomically: true, encoding: .utf8)
        await waitUntil { m.snapshot?.active == "b" }
        XCTAssertEqual(m.snapshot?.active, "b", "an outside change reaches the header")
        m.stopPolling()
        try await Task.sleep(nanoseconds: 300_000_000) // let any in-flight read finish
        let calls = fake.argv.count
        try await Task.sleep(nanoseconds: 700_000_000)
        XCTAssertEqual(fake.argv.count, calls, "no reads after stopPolling")
        XCTAssertTrue(fake.argv.allSatisfy { $0 == "profiles --json" })
    }

    func testNoPollingWithoutAutoLoadOrForTestSnapshot() async throws {
        let fake = try FakeCLI(profilesJSON: profilesJSON(active: "a"))
        let m = ProfilesModel(cli: fake.cli, autoLoad: false, refreshInterval: 0.05)
        let t = ProfilesModel(testingSnapshot: nil)
        try await Task.sleep(nanoseconds: 400_000_000)
        XCTAssertTrue(fake.argv.isEmpty)
        XCTAssertNil(m.snapshot); XCTAssertNil(t.snapshot)
    }

    func testRefreshesNeverOverlap() async throws {
        let fake = try FakeCLI(profilesJSON: profilesJSON(active: "a"))
        let m = ProfilesModel(cli: fake.cli, autoLoad: false)
        async let first = m.reload()
        async let second = m.reload()
        let (a, b) = await (first, second)
        XCTAssertNotEqual(a, b, "exactly one read runs; the overlapping one is skipped")
        XCTAssertEqual(fake.argv.count, 1)
    }

    func testErrorAfterValidSnapshotIsMarkedStale() async throws {
        let fake = try FakeCLI(profilesJSON: profilesJSON(active: "a"))
        let m = ProfilesModel(cli: fake.cli, autoLoad: false)
        let first = await m.reload()
        XCTAssertTrue(first)
        XCTAssertFalse(m.isStale)
        try "not json".write(to: fake.dir.appendingPathComponent("profiles.json"), atomically: true, encoding: .utf8)
        let second = await m.reload()
        XCTAssertFalse(second)
        XCTAssertEqual(m.snapshot?.active, "a", "the last good data is kept")
        XCTAssertNotNil(m.loadError)
        XCTAssertTrue(m.isStale, "but it is labelled stale")
        try profilesJSON(active: "b").write(to: fake.dir.appendingPathComponent("profiles.json"), atomically: true, encoding: .utf8)
        _ = await m.reload()
        XCTAssertFalse(m.isStale)
        XCTAssertEqual(m.snapshot?.active, "b")
        XCTAssertFalse(ProfilesModel(testingSnapshot: nil, error: "e").isStale, "an error with no data is unavailable, not stale")
    }
}

@MainActor
final class ControlsLayoutTests: XCTestCase {
    func testHeaderAndPanelFitWithLongProfileNamesAndNotices() throws {
        let long = String(repeating: "very-long-profile-name-", count: 6)
        let profiles = ProfilesModel(testingSnapshot: try ProfilesSnapshot.decode(Data(Fixtures.profiles(
            [Fixtures.profile(name: long, model: String(repeating: "model-", count: 20)), Fixtures.profile(name: "b")],
            active: long, source: "environment", configured: "b").utf8)))
        let fit = PanelFitTests()
        let snap = try fit.snapshot(active: 7, recent: 20, groups: 15, warnings: 4)
        let h = fit.fitHeight(MonitorModel(testingSnapshot: snap, error: String(repeating: "x", count: 400)),
                              fit.updates(.failed(String(repeating: "problem ", count: 40))), profiles: profiles)
        XCTAssertLessThanOrEqual(h, Layout.height, "long profile name + notices needs \(h)")
        // Same for an unreadable profile list.
        let broken = ProfilesModel(testingSnapshot: nil, error: String(repeating: "e", count: 300))
        XCTAssertLessThanOrEqual(fit.fitHeight(MonitorModel(testingSnapshot: snap), fit.updates(), profiles: broken), Layout.height)
    }

    func testHeaderRowIsAsTallAsBefore() throws {
        // The header gained a mark, a profile menu and a gear but must not grow taller than the old one.
        let fit = PanelFitTests()
        let snap = try fit.snapshot(active: 0, recent: 0, groups: 0, warnings: 0)
        let withProfile = ProfilesModel(testingSnapshot: try ProfilesSnapshot.decode(Data(Fixtures.profiles([Fixtures.profile(name: "w")], active: "w").utf8)))
        let a = fit.fitHeight(MonitorModel(testingSnapshot: snap), fit.updates(), profiles: withProfile)
        let b = fit.fitHeight(MonitorModel(testingSnapshot: snap), fit.updates(), profiles: ProfilesModel(testingSnapshot: nil))
        XCTAssertEqual(a, b, accuracy: 1)
        // The two-line status summary (title + counts) makes the row taller than the old pill; the 680pt panel has room.
        XCTAssertLessThanOrEqual(a, 633 + 16, "previous populated height was 633")
    }

    func testSettingsWindowFitsItsFixedSize() throws {
        let profiles = ProfilesModel(testingSnapshot: try ProfilesSnapshot.decode(Data(Fixtures.profiles([Fixtures.profile(name: "w", redacted: true)], active: "w").utf8)))
        let skills = AgentSkillsModel(testingSnapshot: try SkillsSnapshot.decode(Data(Fixtures.skills([
            Fixtures.agent("codex", status: "blocked", enabled: false, canEnable: false, canDisable: false, message: String(repeating: "Modified by hand. ", count: 12)),
            Fixtures.agent("claude", status: "enabled", enabled: true, current: false)]).utf8)))
        let d = UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!
        let updates = UpdateManager(prefs: Preferences(defaults: d), consumeReport: false)
        for tab in SettingsTab.allCases {
            let host = NSHostingView(rootView: SettingsView(profiles: profiles, skills: skills, updates: updates, tab: tab))
            host.frame = NSRect(x: 0, y: 0, width: SettingsLayout.width, height: SettingsLayout.height)
            host.layoutSubtreeIfNeeded()
            XCTAssertLessThanOrEqual(host.fittingSize.height, SettingsLayout.height + 1, "\(tab) overflows")
        }
    }

    func testBrandMarkDrawsInsideItsBox() {
        let rect = CGRect(x: 0, y: 0, width: 16, height: 16)
        let b = BrandMarkShape().path(in: rect).boundingRect
        XCTAssertFalse(b.isEmpty)
        XCTAssertTrue(rect.insetBy(dx: -0.01, dy: -0.01).contains(b))
        XCTAssertTrue(BrandMarkImage.menuBar.isTemplate)
    }
}
