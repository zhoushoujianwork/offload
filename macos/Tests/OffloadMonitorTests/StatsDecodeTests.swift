import XCTest
@testable import OffloadMonitor

final class StatsDecodeTests: XCTestCase {
    static let full = """
    {"schema_version":1,"generated_at":"2026-10-29T18:23:21+08:00","period":"today",
     "totals":{"runs":3,"sessions":2,"ok":2,"error":1,"timeout":0,"salvaged":0,"elapsed_ms":9000,"elapsed_runs":3,
       "usage":{"input_tokens":100,"cached_input_tokens":60,"cache_write_input_tokens":5,"output_tokens":20},"usage_runs":1,
       "cost_usd":1.25,"cost_runs":2,"excluded_salvage_cost_runs":0},
     "projects":[{"key":"/w/a","name":"a","totals":{"runs":3,"sessions":2,"ok":2,"error":1,"timeout":0,"salvaged":0,"elapsed_ms":9000,"elapsed_runs":3,"usage":null,"usage_runs":0,"cost_usd":null,"cost_runs":0,"excluded_salvage_cost_runs":0}}],
     "models":[],
     "active":[{"pid":42,"phase":"running","dir":"/w/a","task":"t","profile":"p","model":"m","session":"s",
       "started_at":"2026-10-29T18:00:49.270694+08:00","updated_at":"2026-10-29T18:24:55.665871+08:00","last_event_at":"2026-10-29T18:24:51.864393+08:00",
       "tool_calls":3,"permission_denials":0,"last_tool":"Bash","stalled":true},
       {"pid":43,"phase":"lost","dir":"/w/b","task":"t","profile":"p","model":"m","started_at":"2026-10-29T18:00:49+08:00","updated_at":"2026-10-29T18:00:49+08:00","last_event_at":"2026-10-29T18:00:49+08:00","tool_calls":0,"permission_denials":0,"stalled":false}],
     "recent":[{"ts":"2026-10-29 18:19:51","status":"ok","dir":"/w/a","task":"x","session":"s","model":"m","cost_usd":null,"usage":null,"result":"done","salvaged":false}],
     "warnings":["w1"]}
    """

    static let empty = """
    {"schema_version":1,"generated_at":"2026-10-29T00:00:00Z","period":"all",
     "totals":{"runs":0,"sessions":0,"ok":0,"error":0,"timeout":0,"salvaged":0,"elapsed_ms":0,"elapsed_runs":0,"usage":null,"usage_runs":0,"cost_usd":null,"cost_runs":0,"excluded_salvage_cost_runs":0},
     "projects":[],"models":[],"active":[],"recent":[],"warnings":[]}
    """

    func testDecodesFullDocument() throws {
        let s = try StatsDecoder.decode(Data(Self.full.utf8))
        XCTAssertEqual(s.period, "today")
        XCTAssertEqual(s.totals.usage, TokenUsage(inputTokens: 100, cachedInputTokens: 60, cacheWriteInputTokens: 5, outputTokens: 20))
        XCTAssertEqual(s.totals.costUsd, 1.25)
        XCTAssertEqual(s.totals.usageRuns, 1)
        XCTAssertNil(s.projects[0].totals.usage)
        XCTAssertNil(s.projects[0].totals.costUsd)
        XCTAssertNil(s.recent[0].costUsd)
        XCTAssertEqual(s.runningCount, 1)
        XCTAssertEqual(s.stalledCount, 1)
        XCTAssertEqual(s.lostCount, 1)
        XCTAssertNil(s.active[1].lastTool)
        XCTAssertNotNil(Format.parseTimestamp(s.active[0].startedAt))
        XCTAssertNotNil(Format.parseTimestamp(s.generatedAt))
    }

    func testEmptyDocumentKeepsMissingUsageNil() throws {
        let s = try StatsDecoder.decode(Data(Self.empty.utf8))
        XCTAssertNil(s.totals.usage)
        XCTAssertNil(s.totals.costUsd)
        XCTAssertTrue(s.active.isEmpty && s.recent.isEmpty && s.warnings.isEmpty)
    }

    func testRejectsUnknownSchema() {
        let doc = Self.empty.replacingOccurrences(of: "\"schema_version\":1", with: "\"schema_version\":2")
        XCTAssertThrowsError(try StatsDecoder.decode(Data(doc.utf8))) {
            XCTAssertEqual($0 as? StatsError, .unsupportedSchema(2))
        }
    }

    func testRejectsMalformed() {
        for bad in ["", "not json", "{\"schema_version\":1}"] {
            XCTAssertThrowsError(try StatsDecoder.decode(Data(bad.utf8))) {
                guard case .invalidOutput = $0 as? StatsError else { return XCTFail("got \($0)") }
            }
        }
    }

    func testFormatting() {
        XCTAssertEqual(Format.count(999), "999")
        XCTAssertEqual(Format.count(1_500), "1.5K")
        XCTAssertEqual(Format.cost(0), "$0.00")
        XCTAssertEqual(Format.cost(0.004), "<$0.01")
        XCTAssertEqual(Format.duration(125), "2m 5s")
    }

    func testProcessRunnerTimesOut() async throws {
        let t0 = Date()
        let r = try await ProcessRunner.run(executable: URL(fileURLWithPath: "/bin/sleep"), arguments: ["30"], timeout: 0.5)
        XCTAssertTrue(r.timedOut)
        XCTAssertLessThan(Date().timeIntervalSince(t0), 5)
    }

    func testProcessRunnerCapsStreamingOutput() async throws {
        let r = try await ProcessRunner.run(executable: URL(fileURLWithPath: "/usr/bin/yes"), arguments: [], timeout: 5, maxOutput: 100_000)
        XCTAssertTrue(r.truncated)
        XCTAssertEqual(r.stdout.count, 100_000)
    }

    func testProcessRunnerFlagsOneByteOverCapWithoutMoreOutput() async throws {
        let cap = 1000
        let sh = URL(fileURLWithPath: "/bin/sh")
        let over = try await ProcessRunner.run(executable: sh, arguments: ["-c", "head -c \(cap + 1) /dev/zero"], timeout: 5, maxOutput: cap)
        XCTAssertTrue(over.truncated)
        XCTAssertEqual(over.stdout.count, cap)
        let exact = try await ProcessRunner.run(executable: sh, arguments: ["-c", "head -c \(cap) /dev/zero"], timeout: 5, maxOutput: cap)
        XCTAssertFalse(exact.truncated)
        XCTAssertEqual(exact.stdout.count, cap)
        let errOver = try await ProcessRunner.run(executable: sh, arguments: ["-c", "head -c \(cap + 1) /dev/zero >&2"], timeout: 5, maxOutput: cap)
        XCTAssertTrue(errOver.truncated)
        XCTAssertEqual(errOver.stderr.count, cap)
    }

    func testProcessRunnerEscalatesToKillWhenTermIgnored() async throws {
        let t0 = Date()
        let r = try await ProcessRunner.run(executable: URL(fileURLWithPath: "/bin/sh"),
                                            arguments: ["-c", "trap '' TERM; while :; do sleep 1; done"],
                                            timeout: 0.3, killGrace: 0.5)
        XCTAssertTrue(r.timedOut)
        XCTAssertLessThan(Date().timeIntervalSince(t0), 6)
    }

    func testProcessRunnerReturnsWhenDescendantHoldsPipe() async throws {
        let pidFile = FileManager.default.temporaryDirectory.appendingPathComponent("om-pid-\(UUID().uuidString)")
        defer {
            if let s = try? String(contentsOf: pidFile).trimmingCharacters(in: .whitespacesAndNewlines), let pid = Int32(s) { kill(pid, SIGKILL) }
            try? FileManager.default.removeItem(at: pidFile)
        }
        // The background sleep inherits stdout/stderr and outlives the shell.
        let t0 = Date()
        let r = try await ProcessRunner.run(executable: URL(fileURLWithPath: "/bin/sh"),
                                            arguments: ["-c", "echo partial; sleep 30 & echo $! > '\(pidFile.path)'"],
                                            timeout: 0.5, drainGrace: 0.3)
        XCTAssertLessThan(Date().timeIntervalSince(t0), 5)
        XCTAssertEqual(String(decoding: r.stdout, as: UTF8.self), "partial\n")
        XCTAssertEqual(r.status, 0)
        XCTAssertFalse(r.timedOut)
    }

    func testStatsClientReportsFailureAndMismatch() async throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("stats-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        func script(_ body: String) throws -> URL {
            let u = dir.appendingPathComponent(UUID().uuidString)
            try ("#!/bin/sh\n" + body + "\n").write(to: u, atomically: true, encoding: .utf8)
            try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: u.path)
            return u
        }
        let fail = StatsClient(executable: try script("echo boom >&2; exit 2"))
        do { _ = try await fail.fetch(period: .today); XCTFail() } catch {
            XCTAssertEqual(error as? StatsError, .failed(status: 2, message: "boom"))
        }
        let ok = StatsClient(executable: try script("cat <<'EOF'\n\(Self.empty)\nEOF"))
        let all = try await ok.fetch(period: .all)
        XCTAssertEqual(all.period, "all")
        do { _ = try await ok.fetch(period: .week); XCTFail() } catch {
            XCTAssertEqual(error as? StatsError, .periodMismatch(expected: "week", got: "all"))
        }
        var slow = StatsClient(executable: try script("sleep 30"))
        slow.timeout = 0.5
        do { _ = try await slow.fetch(period: .today); XCTFail() } catch {
            XCTAssertEqual(error as? StatsError, .timedOut(0))
        }
    }

    func testPreferencesDefaultsAndPersistence() {
        let suite = "test-\(UUID().uuidString)"
        let d = UserDefaults(suiteName: suite)!
        defer { d.removePersistentDomain(forName: suite) }
        let p = Preferences(defaults: d)
        XCTAssertEqual(p.period, .today)
        XCTAssertTrue(p.autoCheck)
        XCTAssertFalse(p.autoInstall)
        p.period = .month; p.autoCheck = false; p.autoInstall = true
        let q = Preferences(defaults: d)
        XCTAssertEqual(q.period, .month)
        XCTAssertFalse(q.autoCheck)
        XCTAssertTrue(q.autoInstall)
    }

    func testDashboardShownOnFirstLaunchOnly() {
        let suite = "test-\(UUID().uuidString)"
        let d = UserDefaults(suiteName: suite)!
        defer { d.removePersistentDomain(forName: suite) }
        let p = Preferences(defaults: d)
        XCTAssertFalse(p.dashboardShown)
        XCTAssertTrue(LaunchPolicy.shouldShowDashboard(arguments: ["app"], prefs: p))
        XCTAssertTrue(Preferences(defaults: d).dashboardShown)
        XCTAssertFalse(LaunchPolicy.shouldShowDashboard(arguments: ["app"], prefs: Preferences(defaults: d)))
        XCTAssertTrue(LaunchPolicy.shouldShowDashboard(arguments: ["app", "--dashboard"], prefs: Preferences(defaults: d)))
        XCTAssertFalse(LaunchPolicy.shouldShowDashboard(arguments: ["--dashboard"], prefs: Preferences(defaults: d)), "argv[0] is the program")
    }
}
