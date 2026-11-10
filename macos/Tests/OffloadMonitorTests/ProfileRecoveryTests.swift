import Foundation
import XCTest
@testable import OffloadMonitor

/// Every command targets an isolated shell fixture, never the installed CLI or real config.
@MainActor
final class ProfileRecoveryTests: XCTestCase {
    private let epoch = Date(timeIntervalSince1970: 1_800_000_000)

    private func snapshot(_ entries: [(String, Bool, Date?)]) throws -> ProfilesSnapshot {
        var result = try ProfilesSnapshot.decode(Data(Fixtures.profiles(entries.map { Fixtures.profile(name: $0.0) }).utf8))
        for (index, entry) in entries.enumerated() {
            result.profiles[index].available = entry.1
            result.profiles[index].unavailableAt = entry.2.map { ISO8601DateFormatter().string(from: $0) }
        }
        return result
    }

    private func fake(_ initial: ProfilesSnapshot, after: ProfilesSnapshot? = nil, delay: Double = 0) throws -> FakeCLI {
        let fake = try FakeCLI()
        let encoder = JSONEncoder()
        encoder.keyEncodingStrategy = .convertToSnakeCase
        try encoder.encode(initial).write(to: fake.dir.appendingPathComponent("profiles.json"))
        try encoder.encode(after ?? initial).write(to: fake.dir.appendingPathComponent("after.json"))
        let script = """
        #!/bin/sh
        D='\(fake.dir.path)'
        echo "$*" >> "$D/argv.log"
        case "$1 $2" in
          "profiles --json") cat "$D/profiles.json" ;;
          "profile test")
            if [ "$3" = hold ]; then exec /bin/sleep 30; fi
            /bin/sleep \(delay)
            case "$3" in
              crash) echo 'offload: unexpected probe failure' >&2; exit 7 ;;
              bad) cp "$D/after.json" "$D/profiles.json"; exit 1 ;;
              signal) kill -HUP $$ ;;
              *) cp "$D/after.json" "$D/profiles.json" ;;
            esac ;;
          *) exit 64 ;;
        esac
        """
        try script.write(to: fake.exe, atomically: true, encoding: .utf8)
        return fake
    }

    private func waitUntil(_ condition: () -> Bool) async throws {
        for _ in 0..<200 {
            if condition() { return }
            try await Task.sleep(nanoseconds: 5_000_000)
        }
        XCTFail("Fixture did not reach expected state")
    }

    func testAutomaticOnlyOverdueUnavailableAndLocalAttemptThrottle() async throws {
        var clock = epoch
        let old = epoch.addingTimeInterval(-1801)
        let initial = try snapshot([("recover", false, old), ("bad", false, old),
                                    ("fresh", false, epoch), ("healthy", true, nil)])
        var after = initial
        after.profiles[0].available = true
        let fake = try fake(initial, after: after)
        let model = ProfilesModel(cli: fake.cli, autoLoad: false, now: { clock })
        await model.pollRecovery()
        XCTAssertEqual(fake.argv.filter { $0.hasPrefix("profile test") }, ["profile test recover", "profile test bad"])
        XCTAssertEqual(model.snapshot?.entry("recover")?.isUnavailable, false)
        XCTAssertEqual(model.snapshot?.entry("bad")?.isUnavailable, true)
        await model.pollRecovery()
        XCTAssertEqual(fake.argv.filter { $0.hasPrefix("profile test") }.count, 2)
        clock = clock.addingTimeInterval(1800)
        await model.pollRecovery()
        XCTAssertEqual(fake.argv.filter { $0.hasPrefix("profile test") }.suffix(2), ["profile test bad", "profile test fresh"])
        XCTAssertEqual(model.batchCompleted, 2)
    }

    func testHealthyListMakesNoProbeAndManualBypassesInterval() async throws {
        let healthy = try snapshot([("healthy", true, nil)])
        let fake = try fake(healthy)
        let model = ProfilesModel(cli: fake.cli, autoLoad: false)
        await model.pollRecovery()
        XCTAssertEqual(fake.argv, ["profiles --json"])
        let first = await model.test("healthy")
        let second = await model.test("healthy")
        XCTAssertTrue(first && second)
        XCTAssertEqual(fake.argv.filter { $0 == "profile test healthy" }.count, 2)
    }

    func testBatchContinuesAfterUnexpectedFailureAndManualChecksFreshUnavailable() async throws {
        let initial = try snapshot([("crash", false, epoch), ("recover", false, epoch)])
        var after = initial
        after.profiles[1].available = true
        let fake = try fake(initial, after: after)
        let model = ProfilesModel(cli: fake.cli, autoLoad: false, now: { self.epoch })
        model.setSnapshotForTesting(initial)
        await model.testUnavailable()
        XCTAssertEqual(fake.argv, ["profile test crash", "profile test recover", "profiles --json"])
        XCTAssertEqual(model.actionError, "unexpected probe failure")
        XCTAssertEqual(model.snapshot?.entry("crash")?.isUnavailable, true)
        XCTAssertEqual(model.snapshot?.entry("recover")?.isUnavailable, false)
        XCTAssertEqual(model.batchCompleted, 2)
    }

    func testHealthyProfileWithFailedProbeCannotReportSuccess() async throws {
        let initial = try snapshot([("bad", true, nil)])
        var after = initial
        after.profiles[0].model = "refreshed-model"
        let fake = try fake(initial, after: after)
        let model = ProfilesModel(cli: fake.cli, autoLoad: false)
        model.setSnapshotForTesting(initial)
        let succeeded = await model.test("bad")
        XCTAssertFalse(succeeded)
        XCTAssertEqual(model.snapshot, after)
        XCTAssertEqual(model.snapshot?.entry("bad")?.isUnavailable, false)
        XCTAssertEqual(model.actionError, "The profile check failed (exit 1).")
        XCTAssertEqual(fake.argv, ["profile test bad", "profiles --json"])
    }

    func testFailedUnavailableProbeRefreshesReasonAndBatchContinues() async throws {
        let initial = try snapshot([("bad", false, epoch), ("recover", false, epoch)])
        var after = initial
        after.profiles[0].unavailableReason = "authentication failed: sign in required"
        after.profiles[1].available = true
        let fake = try fake(initial, after: after)
        let model = ProfilesModel(cli: fake.cli, autoLoad: false)
        model.setSnapshotForTesting(initial)
        let succeeded = await model.test("bad")
        XCTAssertFalse(succeeded)
        XCTAssertEqual(model.snapshot?.entry("bad")?.unavailableReason, after.profiles[0].unavailableReason)
        XCTAssertEqual(model.actionError, "The profile check failed (exit 1).")
        // Keep the second entry unavailable in the model to exercise continuing after exit 1.
        var batchSnapshot = after
        batchSnapshot.profiles[1].available = false
        model.setSnapshotForTesting(batchSnapshot)
        await model.testUnavailable()
        XCTAssertEqual(fake.argv.suffix(4), ["profile test bad", "profiles --json", "profile test recover", "profiles --json"])
        XCTAssertEqual(model.batchCompleted, 2)
        XCTAssertEqual(model.snapshot?.entry("recover")?.isUnavailable, false)
    }

    func testMissingAndInvalidUnavailableDatesAreInitiallyDueThenThrottled() async throws {
        var clock = epoch
        var initial = try snapshot([("missing", false, nil), ("invalid", false, nil), ("empty", false, nil)])
        initial.profiles[1].unavailableAt = "not-a-date"
        initial.profiles[2].unavailableAt = ""
        let fake = try fake(initial)
        let model = ProfilesModel(cli: fake.cli, autoLoad: false, now: { clock })
        await model.pollRecovery()
        XCTAssertEqual(fake.argv.filter { $0.hasPrefix("profile test") }, ["profile test missing", "profile test invalid", "profile test empty"])
        await model.pollRecovery()
        XCTAssertEqual(fake.argv.filter { $0.hasPrefix("profile test") }.count, 3)
        clock = clock.addingTimeInterval(1800)
        await model.pollRecovery()
        XCTAssertEqual(fake.argv.filter { $0.hasPrefix("profile test") }.count, 6)
    }

    func testSerialDedupAndNoReadOrWriteDuringProbe() async throws {
        let initial = try snapshot([("healthy", true, nil)])
        let fake = try fake(initial, delay: 0.2)
        let model = ProfilesModel(cli: fake.cli, autoLoad: false)
        model.setSnapshotForTesting(initial)
        let task = Task { await model.test("healthy") }
        try await waitUntil { model.testing.contains("healthy") }
        let duplicate = await model.test("healthy")
        let other = await model.test("other")
        let reload = await model.reload()
        let use = await model.use("healthy")
        let save = await model.save(ProfileSaveRequest(name: "healthy", create: false))
        await model.testUnavailable()
        XCTAssertFalse(duplicate || other || reload || use || save)
        let ok = await task.value
        XCTAssertTrue(ok)
        XCTAssertEqual(fake.argv, ["profile test healthy", "profiles --json"])
        XCTAssertTrue(model.testing.isEmpty)
        XCTAssertFalse(model.busy)
    }

    func testStopCancelsInFlightBatchWithoutRefreshingOrChangingAvailability() async throws {
        let initial = try snapshot([("hold", false, epoch), ("later", false, epoch)])
        let fake = try fake(initial)
        let model = ProfilesModel(cli: fake.cli, autoLoad: false)
        model.setSnapshotForTesting(initial)
        model.startUnavailableTests()
        model.startUnavailableTests()
        try await waitUntil { fake.argv.contains("profile test hold") }
        XCTAssertTrue(model.batchRunning)
        model.stopPolling()
        try await waitUntil { !model.busy }
        XCTAssertEqual(fake.argv, ["profile test hold"])
        XCTAssertEqual(model.snapshot, initial)
        XCTAssertNil(model.actionError)
        XCTAssertFalse(model.batchRunning)
        XCTAssertTrue(model.testing.isEmpty)
    }

    func testStartupPollingReloadsThenProbesWithoutASettingsWindow() async throws {
        let initial = try snapshot([("hold", false, epoch.addingTimeInterval(-1801))])
        let fake = try fake(initial)
        let model = ProfilesModel(cli: fake.cli, refreshInterval: 0.02, now: { self.epoch })
        try await waitUntil { fake.argv.contains("profile test hold") }
        XCTAssertEqual(fake.argv, ["profiles --json", "profile test hold"])
        model.stopPolling()
        try await waitUntil { !model.busy }
        XCTAssertEqual(model.snapshot, initial)
        XCTAssertNil(model.actionError)
    }

    func testCancelBeforeQueuedBatchStartsAllowsAnotherBatch() async throws {
        let initial = try snapshot([("bad", false, epoch)])
        let fake = try fake(initial)
        let model = ProfilesModel(cli: fake.cli, autoLoad: false)
        model.setSnapshotForTesting(initial)
        model.startUnavailableTests()
        model.cancelTests()
        // Let the cancelled task execute its cleanup before a fresh user action.
        try await Task.sleep(nanoseconds: 20_000_000)
        model.startUnavailableTests()
        try await waitUntil { fake.argv.contains("profile test bad") }
        try await waitUntil { !model.busy }
        XCTAssertEqual(fake.argv, ["profile test bad", "profiles --json"])
    }

    func testTaskCancellationAlsoStopsManualProbe() async throws {
        let initial = try snapshot([("hold", true, nil)])
        let fake = try fake(initial)
        let model = ProfilesModel(cli: fake.cli, autoLoad: false)
        model.setSnapshotForTesting(initial)
        let task = Task { await model.test("hold") }
        try await waitUntil { fake.argv.contains("profile test hold") }
        task.cancel()
        let ok = await task.value
        XCTAssertFalse(ok)
        XCTAssertEqual(model.snapshot, initial)
        XCTAssertNil(model.actionError)
        XCTAssertEqual(fake.argv, ["profile test hold"])
    }

    func testStopPollingAlsoCancelsInitialRead() async throws {
        let fake = try FakeCLI()
        try """
        #!/bin/sh
        echo "$*" >> '\(fake.argvLog.path)'
        exec /bin/sleep 30
        """.write(to: fake.exe, atomically: true, encoding: .utf8)
        let model = ProfilesModel(cli: fake.cli)
        try await waitUntil { fake.argv == ["profiles --json"] }
        model.stopPolling()
        try await waitUntil { !model.busy }
        XCTAssertNil(model.snapshot)
        XCTAssertNil(model.loadError)
    }

    func testSharedControlCancelsSequentialFollowupProcess() async throws {
        let control = ProcessControl(intGrace: 0.01, termGrace: 0.01)
        for _ in 0..<5 {
            _ = try await ProcessRunner.run(executable: URL(fileURLWithPath: "/usr/bin/true"), arguments: [], timeout: 2, control: control)
        }
        let task = Task {
            try await ProcessRunner.run(executable: URL(fileURLWithPath: "/bin/sleep"), arguments: ["30"], timeout: 2, control: control)
        }
        try await waitUntil { control.runningPID != nil }
        control.cancel()
        let output = try await task.value
        XCTAssertTrue(output.cancelled)
        XCTAssertFalse(output.timedOut)
        XCTAssertNil(control.runningPID)
    }

    func testProbeUsesLongTimeoutAndOtherFailuresRemainErrors() async throws {
        let initial = try snapshot([("healthy", true, nil), ("bad", false, epoch)])
        let fake = try fake(initial, delay: 0.15)
        var cli = fake.cli
        cli.timeout = 0.05
        XCTAssertGreaterThanOrEqual(OffloadCLI.profileTestTimeout, 130)
        _ = try await cli.testProfile("healthy") // exceeds the normal command timeout
        _ = try await cli.testProfile("bad") // exit 1 reads the updated failed state
        do {
            _ = try await cli.testProfile("crash")
            XCTFail("Unexpected exit must throw")
        } catch let error as CLIError { XCTAssertEqual(error, .failed("unexpected probe failure")) }
        do {
            _ = try await cli.testProfile("signal")
            XCTFail("Signal 1 must not be treated as normal exit 1")
        } catch let error as CLIError { XCTAssertEqual(error, .failed("The offload command was terminated by a signal.")) }
        do {
            _ = try await cli.run(["profile", "test", "hold"], control: ProcessControl(intGrace: 0.01, termGrace: 0.01), commandTimeout: 0.03)
            XCTFail("Timeout must throw")
        } catch let error as CLIError { XCTAssertEqual(error, .timedOut) }
        do {
            _ = try await OffloadCLI(executable: fake.dir.appendingPathComponent("missing")).testProfile("healthy")
            XCTFail("Launch failure must throw")
        } catch let error as CLIError { XCTAssertEqual(error, .launch) }
        XCTAssertEqual(fake.argv.filter { $0 == "profiles --json" }.count, 2)
    }
}
