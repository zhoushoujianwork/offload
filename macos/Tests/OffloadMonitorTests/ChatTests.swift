import AppKit
import SwiftUI
import XCTest
@testable import OffloadMonitor

/// A shell script standing in for `offload` that handles history queries and chat runs. It records argv, stdin and
/// the working directory in an isolated temp directory. Nothing here touches a real config, history or provider.
final class FakeChatCLI {
    let dir: URL
    var exe: URL { dir.appendingPathComponent("offload") }
    var cli: OffloadCLI { OffloadCLI(executable: exe, timeout: 10) }
    func file(_ n: String) -> URL { dir.appendingPathComponent(n) }
    func read(_ n: String) -> String { (try? String(contentsOf: file(n))) ?? "" }
    func write(_ n: String, _ s: String) { try? s.write(to: file(n), atomically: true, encoding: .utf8) }

    enum Mode: String { case ok, fail, warn, slow, stubbornInt = "stubborn_int", stubbornAll = "stubborn_all", hang }

    init(mode: Mode = .ok, runJSON: String = FakeChatCLI.runOK()) throws {
        dir = FileManager.default.temporaryDirectory.appendingPathComponent("fake-chat-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        write("mode", mode.rawValue)
        write("run.json", runJSON)
        write("list.json", FakeChatCLI.list([]))
        let script = """
        #!/bin/sh
        D='\(dir.path)'
        case "$1" in
          history)
            echo "$*" >> "$D/hist.argv"
            case "$2" in
              list)
                cat > "$D/list.stdin"
                off=0; prev=""
                for a in "$@"; do [ "$prev" = "--offset" ] && off=$a; prev=$a; done
                f="$D/list.$off.json"; [ -f "$f" ] || f="$D/list.json"
                cat "$f" ;;
              show)
                if [ -f "$D/show.delay" ]; then sleep "$(cat "$D/show.delay")"; fi
                if [ -f "$D/show.fail" ]; then echo "offload: history unavailable" >&2; exit 1; fi
                cat "$D/show.json" ;;
            esac ;;
          *)
            printf '%s\\n' "$@" > "$D/run.argv.$$"
            echo "$$" >> "$D/run.pids"
            printf '%s\\n' "--" >> "$D/runs.log"
            cat "$D/run.argv.$$" >> "$D/runs.log"
            cat > "$D/run.stdin"
            pwd > "$D/run.cwd"
            case "$(cat "$D/mode")" in
              ok) cat "$D/run.json" ;;
              fail) cat "$D/run.json"; echo "[offload] error exit_code=1" >&2; exit 1 ;;
              warn) cat "$D/run.json"; echo "[offload] ok exit_code=0 session=s warning=run log not written: disk full" >&2 ;;
              slow)
                sleep 60 & CHILD=$!
                echo "$CHILD" > "$D/child.pid"
                trap 'kill $CHILD 2>/dev/null; cat "$D/run.json"; exit 130' INT
                touch "$D/ready"
                wait ;;
              stubborn_int)
                trap '' INT
                trap 'cat "$D/run.json"; exit 143' TERM
                touch "$D/ready"
                while :; do sleep 0.2; done ;;
              stubborn_all)
                trap '' INT TERM
                touch "$D/ready"
                while :; do sleep 0.2; done ;;
              hang) touch "$D/ready"; while :; do sleep 0.2; done ;;
            esac ;;
        esac
        """
        write("offload", script)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: exe.path)
    }

    deinit {
        // Never leave a fake worker behind.
        for name in ["child.pid", "run.pids"] {
            for line in read(name).split(separator: "\n") { if let p = Int32(line) { kill(p, SIGKILL) } }
        }
        try? FileManager.default.removeItem(at: dir)
    }

    var runCount: Int { read("runs.log").split(separator: "\n").filter { $0 == "--" }.count }
    /// argv of the most recent run.
    var lastArgs: [String] {
        let all = read("runs.log").components(separatedBy: "--\n")
        return (all.last ?? "").split(separator: "\n", omittingEmptySubsequences: true).map(String.init)
    }
    var histArgv: [String] { read("hist.argv").split(separator: "\n").map(String.init) }

    func waitForFile(_ n: String, timeout: TimeInterval = 5) async -> Bool {
        let end = Date().addingTimeInterval(timeout)
        while !FileManager.default.fileExists(atPath: file(n).path) {
            if Date() > end { return false }
            try? await Task.sleep(nanoseconds: 20_000_000)
        }
        return true
    }

    static func runOK(session: String = "sess-new", result: String = "# Done\n\nAll good.", status: String = "ok", exit: Int = 0, error: String = "") -> String {
        """
        {"ts":"2026-10-29 20:00:00","status":"\(status)","exit_code":\(exit),"profile":"work","model":"m1","dir":"/w","task":"t","session":"\(session)",
         "turns":3,"cost_usd":0.12,"usage":{"input_tokens":100,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":20},
         "elapsed_ms":4200,"result":\(json(result)),"error":\(json(error))}
        """
    }

    static func json(_ s: String) -> String {
        String(decoding: (try? JSONSerialization.data(withJSONObject: [s], options: [])) ?? Data(), as: UTF8.self).dropFirst().dropLast().description
    }

    static func summary(id: String = "sess-1", session: String? = nil, title: String = "Fix the bug", dir: String = "/w", profile: String = "work", runs: Int = 1,
                        usage: Bool = true, cost: Bool = true) -> String {
        """
        {"id":"\(id)","session":"\(session ?? id)","title":\(json(title)),"dir":\(json(dir)),"profile":"\(profile)","backend":"claude","model":"m1",
         "started_at":"2026-10-29T10:00:00Z","updated_at":"2026-10-29T11:00:00Z","run_count":\(runs),"status":"ok","active":false,
         "usage":\(usage ? #"{"input_tokens":10,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":5}"# : "null"),"usage_runs":\(usage ? runs : 0),
         "cost_usd":\(cost ? "0.5" : "null"),"cost_runs":\(cost ? runs : 0)}
        """
    }

    static func list(_ items: [String], total: Int? = nil, offset: Int = 0, hasMore: Bool = false) -> String {
        #"{"schema_version":1,"total":\#(total ?? items.count),"offset":\#(offset),"limit":50,"has_more":\#(hasMore),"conversations":[\#(items.joined(separator: ","))]}"#
    }

    static func run(id: String, task: String, result: String, status: String = "ok", session: String = "sess-1") -> String {
        """
        {"id":"\(id)","ts":"2026-10-29T10:00:00Z","status":"\(status)","exit_code":\(status == "ok" ? "0" : "1"),"task":\(json(task)),"result":\(json(result)),"error":"",
         "dir":"/w","profile":"work","backend":"claude","model":"m1","session":"\(session)","usage":null,"cost_usd":null,"salvaged":false,"elapsed_ms":1500,"turns":2}
        """
    }

    static func show(summary: String, runs: [String], active: String = "null") -> String {
        #"{"schema_version":1,"conversation":\#(summary),"runs":[\#(runs.joined(separator: ","))],"active":\#(active)}"#
    }
}

@MainActor
func waitUntil(_ timeout: TimeInterval = 8, _ cond: @escaping @MainActor () -> Bool) async {
    let end = Date().addingTimeInterval(timeout)
    while true {
        if cond() || Date() >= end { return }
        try? await Task.sleep(nanoseconds: 30_000_000)
    }
}

// MARK: - Command construction and outcome classification

final class ChatCommandTests: XCTestCase {
    func testNewConversationArguments() {
        let a = ChatCommand.arguments(ChatTarget(session: nil, profile: "work", dir: "/w", allowWrites: false))
        XCTAssertEqual(a, ["-p", "work", "-d", "/w", "-json", "-t", "30m"])
        XCTAssertEqual(ChatCommand.arguments(ChatTarget(session: nil, profile: "work", dir: "/w", allowWrites: true)).last, "-w")
    }

    func testResumeKeepsOriginalProfileAndPassesDir() {
        let a = ChatCommand.arguments(ChatTarget(session: "abc", profile: "ignored", dir: "/orig", allowWrites: false))
        XCTAssertEqual(a, ["-r", "abc", "-d", "/orig", "-json", "-t", "30m"])
        XCTAssertFalse(a.contains("-p"), "a resumed chat lets the CLI use the original profile")
        XCTAssertFalse(a.contains("-w"), "no write instruction unless asked")
    }

    func testPathKeepsExistingAndAddsToolLocationsOnce() {
        let env = ChatCommand.environment(base: ["PATH": "/custom/bin:/usr/bin", "HOME": "/h", "TOKEN": "x"], home: "/h")
        let parts = env["PATH"]!.split(separator: ":").map(String.init)
        XCTAssertEqual(parts.first, "/custom/bin")
        XCTAssertEqual(parts.filter { $0 == "/usr/bin" }.count, 1)
        for p in ["/h/.local/bin", "/h/go/bin", "/h/.claude/local", "/opt/homebrew/bin", "/usr/local/bin", "/bin"] { XCTAssertTrue(parts.contains(p), p) }
        XCTAssertEqual(env["TOKEN"], "x", "other variables pass through untouched")
        XCTAssertFalse(ChatCommand.environment(base: [:], home: "/h")["PATH"]!.isEmpty)
    }

    func out(_ stdout: String, status: Int32 = 0, stderr: String = "", timedOut: Bool = false, truncated: Bool = false, cancelled: Bool = false) -> ProcessOutput {
        ProcessOutput(stdout: Data(stdout.utf8), stderr: Data(stderr.utf8), status: status, timedOut: timedOut, truncated: truncated, cancelled: cancelled)
    }

    func testNonzeroExitWithJSONKeepsTheResult() {
        let o = ChatCommand.outcome(out(FakeChatCLI.runOK(result: "partial answer", status: "error", exit: 1, error: "tool crashed"), status: 1))
        XCTAssertEqual(o.kind, .failed)
        XCTAssertEqual(o.result?.result, "partial answer")
        XCTAssertEqual(o.message, "tool crashed")
        XCTAssertEqual(o.session, "sess-new")
    }

    func testOKTimeoutAndNoJSON() {
        XCTAssertEqual(ChatCommand.outcome(out(FakeChatCLI.runOK())).kind, .finished)
        XCTAssertEqual(ChatCommand.outcome(out(FakeChatCLI.runOK(status: "timeout", exit: 124), status: 124)).kind, .timedOut)
        let none = ChatCommand.outcome(out("", status: 64, stderr: "offload: unknown profile\n"))
        XCTAssertEqual(none.kind, .couldNotRun)
        XCTAssertEqual(none.message, "unknown profile")
        XCTAssertEqual(ChatCommand.outcome(out("not json", status: 2)).kind, .couldNotRun)
    }

    func testOuterTimeoutIsTimeoutNotStopped() {
        let o = ChatCommand.outcome(out(FakeChatCLI.runOK(), status: 130, timedOut: true, cancelled: true))
        XCTAssertEqual(o.kind, .timedOut, "the runner marks a timeout as cancelled too; the user did not stop it")
        XCTAssertEqual(ChatCommand.outcome(out("", status: 137, timedOut: true, cancelled: true)).kind, .timedOut)
    }

    func testCancelledWithJSONRetainsSessionAndNeverClaimsDone() {
        let o = ChatCommand.outcome(out(FakeChatCLI.runOK(session: "kept-session", status: "error", exit: 130), status: 130, cancelled: true))
        XCTAssertEqual(o.kind, .stopped)
        XCTAssertEqual(o.session, "kept-session")
        XCTAssertEqual(ChatCommand.outcome(out("", status: 130, cancelled: true)).session, nil)
    }

    func testLateStopAfterSuccessfulFinishIsStillFinished() {
        let ok = out(FakeChatCLI.runOK(session: "s-late", result: "complete answer"), status: 0, cancelled: true)
        let o = ChatCommand.outcome(ok)
        XCTAssertEqual(o.kind, .finished, "status ok with exit 0 is a real success even though Stop was clicked")
        XCTAssertEqual(o.result?.result, "complete answer")
        XCTAssertEqual(o.session, "s-late")
        // A cancelled run that failed is stopped; an OK record with a nonzero exit is not a success.
        XCTAssertEqual(ChatCommand.outcome(out(FakeChatCLI.runOK(), status: 130, cancelled: true)).kind, .stopped)
        XCTAssertEqual(ChatCommand.outcome(out(FakeChatCLI.runOK(status: "error", exit: 130), status: 130, cancelled: true)).kind, .stopped)
        // A timeout still wins over a late success flag only when the record is not a success.
        XCTAssertEqual(ChatCommand.outcome(out(FakeChatCLI.runOK(status: "timeout", exit: 124), status: 124, timedOut: true, cancelled: true)).kind, .timedOut)
    }

    func testOutputOverCapIsASafeError() {
        let o = ChatCommand.outcome(out("", status: 143, truncated: true))
        XCTAssertEqual(o.kind, .failed)
        XCTAssertTrue(o.message!.contains("larger than the allowed size"))
    }

    func testNotPersistedWarning() {
        XCTAssertTrue(ChatCommand.outcome(out(FakeChatCLI.runOK(), stderr: "[offload] ok warning=run log not written: disk full\n")).notPersisted)
        XCTAssertFalse(ChatCommand.outcome(out(FakeChatCLI.runOK(), stderr: "[offload] ok\n")).notPersisted)
    }
}

// MARK: - Runner cancellation against a fake child

final class ChatRunnerCancelTests: XCTestCase {
    func run(_ fake: FakeChatCLI, control: ProcessControl, timeout: TimeInterval = 30) async throws -> ProcessOutput {
        try await ProcessRunner.run(executable: fake.exe, arguments: ["-p", "w", "-d", "/tmp", "-json"], timeout: timeout, stdin: Data("task".utf8), control: control)
    }

    func alive(_ pid: Int32) -> Bool { kill(pid, 0) == 0 }

    func testInterruptLetsTheChildPrintItsResultAndLeavesNoOrphan() async throws {
        let fake = try FakeChatCLI(mode: .slow, runJSON: FakeChatCLI.runOK(session: "s-int", status: "error", exit: 130))
        let control = ProcessControl()
        let t0 = Date()
        async let result = run(fake, control: control)
        let ready = await fake.waitForFile("ready")
        XCTAssertTrue(ready)
        control.cancel()
        let out = try await result
        XCTAssertTrue(out.cancelled)
        XCTAssertEqual(out.status, 130)
        XCTAssertEqual(RunResult.decode(out.stdout)?.session, "s-int", "the JSON printed after SIGINT is kept")
        XCTAssertLessThan(Date().timeIntervalSince(t0), 5, "a clean interrupt does not wait for the grace period")
        try await Task.sleep(nanoseconds: 200_000_000)
        if let c = Int32(fake.read("child.pid").trimmingCharacters(in: .whitespacesAndNewlines)) { XCTAssertFalse(alive(c), "fake grandchild is gone") }
    }

    func testEscalatesToTermWhenInterruptIsIgnored() async throws {
        let fake = try FakeChatCLI(mode: .stubbornInt, runJSON: FakeChatCLI.runOK(session: "s-term", status: "error", exit: 143))
        let control = ProcessControl(intGrace: 0.4, termGrace: 3)
        async let result = run(fake, control: control)
        _ = await fake.waitForFile("ready")
        let t0 = Date()
        control.cancel()
        let out = try await result
        XCTAssertEqual(out.status, 143)
        XCTAssertEqual(RunResult.decode(out.stdout)?.session, "s-term")
        XCTAssertLessThan(Date().timeIntervalSince(t0), 6)
    }

    func testEscalatesToKillWhenEverythingIsIgnored() async throws {
        let fake = try FakeChatCLI(mode: .stubbornAll)
        let control = ProcessControl(intGrace: 0.3, termGrace: 0.3)
        async let result = run(fake, control: control)
        _ = await fake.waitForFile("ready")
        let t0 = Date()
        control.cancel()
        let out = try await result
        XCTAssertEqual(out.status, 9, "killed by SIGKILL")
        XCTAssertLessThan(Date().timeIntervalSince(t0), 6)
        let pids = fake.read("run.pids").split(separator: "\n").compactMap { Int32($0) }
        try await Task.sleep(nanoseconds: 300_000_000)
        for p in pids { XCTAssertFalse(alive(p)) }
    }

    func testCancelBeforeStartNeverLaunches() async throws {
        let fake = try FakeChatCLI()
        let control = ProcessControl()
        control.cancel()
        do { _ = try await run(fake, control: control); XCTFail("should not run") } catch { XCTAssertTrue(error is CancellationError) }
        XCTAssertEqual(fake.runCount, 0)
        let o = await ChatRunner(executable: fake.exe).send(task: "x", target: ChatTarget(session: nil, profile: "w", dir: "/tmp", allowWrites: false), control: control) { _ in }
        XCTAssertEqual(o.kind, .stopped)
        XCTAssertEqual(fake.runCount, 0)
    }

    func testOuterTimeoutInterruptsAndReportsTimeout() async throws {
        let fake = try FakeChatCLI(mode: .slow, runJSON: FakeChatCLI.runOK(status: "timeout", exit: 124))
        let o = await ChatRunner(executable: fake.exe, timeout: 0.6).send(task: "x", target: ChatTarget(session: nil, profile: "w", dir: "/tmp", allowWrites: false), control: ProcessControl()) { _ in }
        XCTAssertEqual(o.kind, .timedOut)
    }

    func testStartCallbackReportsPID() async throws {
        let fake = try FakeChatCLI()
        let box = Locked<Int32?>(nil)
        let o = await ChatRunner(executable: fake.exe).send(task: "x", target: ChatTarget(session: nil, profile: "w", dir: "/tmp", allowWrites: false), control: ProcessControl()) { box.set($0) }
        XCTAssertEqual(o.kind, .finished)
        XCTAssertNotNil(box.value)
        XCTAssertEqual(String(box.value!), fake.read("run.pids").trimmingCharacters(in: .whitespacesAndNewlines))
    }
}

final class ProcessControlRaceTests: XCTestCase {
    func start(_ path: String, _ args: [String] = []) throws -> Process {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: path)
        p.arguments = args
        p.standardOutput = FileHandle.nullDevice
        p.standardError = FileHandle.nullDevice
        try p.run()
        return p
    }

    func testExitedBeforeBindIsRefusedAndNeverSignalled() throws {
        let c = ProcessControl(intGrace: 0.05, termGrace: 0.05)
        let p = try start("/usr/bin/true")
        p.waitUntilExit()
        c.processExited(p) // termination reported first
        XCTAssertFalse(c.bind(p), "an exited process cannot be reattached")
        c.cancel()
        XCTAssertEqual(c.signals, [])
        XCTAssertNil(c.runningPID)
    }

    func testBindOfAnAlreadyDeadProcessIsRefusedEvenWithoutExitReport() throws {
        let c = ProcessControl()
        let p = try start("/usr/bin/true")
        p.waitUntilExit()
        XCTAssertFalse(c.bind(p))
        c.cancel()
        XCTAssertEqual(c.signals, [])
    }

    func testPreCancelThenBindSendsInterruptOnce() throws {
        let c = ProcessControl(intGrace: 5, termGrace: 5)
        c.cancel() // before the process exists
        let p = try start("/bin/sleep", ["30"])
        XCTAssertTrue(c.bind(p))
        p.waitUntilExit()
        XCTAssertEqual(p.terminationReason, .uncaughtSignal)
        XCTAssertEqual(c.signals, [SIGINT])
    }

    func testNoDelayedSignalAfterExit() async throws {
        let c = ProcessControl(intGrace: 0.15, termGrace: 0.15)
        let p = try start("/bin/sleep", ["30"])
        XCTAssertTrue(c.bind(p))
        c.cancel()
        p.waitUntilExit()
        c.processExited(p)
        try await Task.sleep(nanoseconds: 700_000_000) // both grace periods pass
        XCTAssertEqual(c.signals, [SIGINT], "TERM and KILL never fire after the process exited")
    }

    func testFullEscalationOrderIsIntTermKill() async throws {
        let c = ProcessControl(intGrace: 0.2, termGrace: 0.2)
        let p = try start("/bin/sh", ["-c", "trap '' INT TERM; while :; do sleep 0.1; done"])
        try await Task.sleep(nanoseconds: 200_000_000) // let the shell install its traps
        XCTAssertTrue(c.bind(p))
        c.cancel()
        let end = Date().addingTimeInterval(5)
        while p.isRunning, Date() < end { try await Task.sleep(nanoseconds: 50_000_000) }
        XCTAssertFalse(p.isRunning)
        XCTAssertEqual(c.signals, [SIGINT, SIGTERM, SIGKILL])
    }

    func testRunnerFastExitHasNoStaleSignals() async throws {
        for _ in 0..<20 {
            let c = ProcessControl(intGrace: 0.05, termGrace: 0.05)
            async let out = ProcessRunner.run(executable: URL(fileURLWithPath: "/usr/bin/true"), arguments: [], timeout: 5, control: c)
            c.cancel() // races the launch and the exit
            let r = try? await out
            _ = r
            try await Task.sleep(nanoseconds: 150_000_000)
            XCTAssertLessThanOrEqual(c.signals.count, 1, "at most the one interrupt, never an escalation: \(c.signals)")
        }
    }
}

final class Locked<T>: @unchecked Sendable {
    private let lock = NSLock()
    private var v: T
    init(_ v: T) { self.v = v }
    func set(_ n: T) { lock.lock(); v = n; lock.unlock() }
    var value: T { lock.lock(); defer { lock.unlock() }; return v }
}

// MARK: - Chat model with a fake CLI

@MainActor
final class ChatModelTests: XCTestCase {
    var tmp: URL!

    override func setUp() async throws {
        tmp = FileManager.default.temporaryDirectory.appendingPathComponent("chat-ws-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tmp, withIntermediateDirectories: true)
    }
    override func tearDown() async throws { try? FileManager.default.removeItem(at: tmp) }

    func profiles(_ names: [String] = ["work"], active: String = "work") throws -> ProfilesModel {
        ProfilesModel(testingSnapshot: try ProfilesSnapshot.decode(Data(Fixtures.profiles(names.map { Fixtures.profile(name: $0) }, active: active, configured: active).utf8)))
    }

    func env(_ fake: FakeChatCLI, profiles p: ProfilesModel? = nil, monitor: MonitorModel? = nil) throws -> ChatEnv {
        let d = UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!
        return ChatEnv(cli: fake.cli, profiles: try p ?? profiles(), monitor: monitor ?? MonitorModel(testingSnapshot: nil),
                       history: HistoryModel(cli: fake.cli, debounce: 0), prefs: Preferences(defaults: d),
                       generalWorkspace: tmp.appendingPathComponent("General"))
    }

    func idle(_ m: ChatModel) async { await waitUntil { m.phase == .idle } }

    func testFirstSendUsesStdinProfileAndGeneralWorkspaceCreatedOnlyOnSend() async throws {
        let fake = try FakeChatCLI(runJSON: FakeChatCLI.runOK(session: "sess-new"))
        let secretish = "Please summarize SECRET-TASK-TEXT-123"
        fake.write("show.json", FakeChatCLI.show(summary: FakeChatCLI.summary(id: "sess-new", dir: tmp.appendingPathComponent("General").path),
                                                  runs: [FakeChatCLI.run(id: "r1", task: secretish, result: "# Done\n\nAll good.", session: "sess-new")]))
        let e = try env(fake)
        let m = ChatModel(env: e)
        let general = tmp.appendingPathComponent("General")
        XCTAssertFalse(FileManager.default.fileExists(atPath: general.path), "nothing is created before Send")
        XCTAssertEqual(m.profileName, "work")
        m.draft = secretish
        m.send()
        await idle(m)
        XCTAssertEqual(fake.lastArgs, ["-p", "work", "-d", general.path, "-json", "-t", "30m"])
        XCTAssertEqual(fake.read("run.stdin"), secretish)
        XCTAssertFalse(fake.lastArgs.joined(separator: " ").contains("SECRET-TASK-TEXT"), "the task never appears in argv")
        var isDir: ObjCBool = false
        XCTAssertTrue(FileManager.default.fileExists(atPath: general.path, isDirectory: &isDir) && isDir.boolValue)
        let perms = try FileManager.default.attributesOfItem(atPath: general.path)[.posixPermissions] as? Int
        XCTAssertEqual(perms.map { $0 & 0o777 }, 0o700)
        XCTAssertEqual(m.conversationID, "sess-new")
        XCTAssertEqual(m.entries.map(\.role), [.user, .assistant])
        XCTAssertEqual(m.entries.filter(\.local).count, 0, "canonical records replace the local copy, no duplicates")
        XCTAssertTrue(m.entries[1].text.contains("All good."))
    }

    func testExplicitFolderAndWriteFlagAndLastWorkspacePersisted() async throws {
        let fake = try FakeChatCLI()
        fake.write("show.json", FakeChatCLI.show(summary: FakeChatCLI.summary(id: "sess-new", dir: tmp.path), runs: [FakeChatCLI.run(id: "r1", task: "t", result: "r", session: "sess-new")]))
        let e = try env(fake)
        let m = ChatModel(env: e)
        m.workspace = tmp.path
        m.allowWrites = true
        m.draft = "go"
        m.send()
        await idle(m)
        XCTAssertEqual(fake.lastArgs, ["-p", "work", "-d", tmp.path, "-json", "-t", "30m", "-w"])
        XCTAssertFalse(FileManager.default.fileExists(atPath: tmp.appendingPathComponent("General").path), "General is not created when another folder is chosen")
    }

    func testResumeUsesSessionOriginalDirAndNoProfileFlag() async throws {
        let fake = try FakeChatCLI(runJSON: FakeChatCLI.runOK(session: "sess-1"))
        let s = try HistoryList.decode(Data(FakeChatCLI.list([FakeChatCLI.summary(id: "sess-1", dir: tmp.path)]).utf8)).conversations[0]
        fake.write("show.json", FakeChatCLI.show(summary: FakeChatCLI.summary(id: "sess-1", dir: tmp.path, runs: 2), runs: [
            FakeChatCLI.run(id: "r1", task: "one", result: "a"), FakeChatCLI.run(id: "r2", task: "two", result: "b")]))
        let m = ChatModel(env: try env(fake))
        m.openExisting(s)
        await waitUntil { m.entries.count == 4 }
        XCTAssertFalse(m.allowWrites, "reopened history defaults to read-only")
        m.draft = "continue please"
        XCTAssertTrue(m.canSend)
        m.send()
        await idle(m)
        XCTAssertEqual(fake.lastArgs, ["-r", "sess-1", "-d", tmp.path, "-json", "-t", "30m"])
        XCTAssertEqual(fake.read("run.stdin"), "continue please")
    }

    func testDeletedProfileBlocksResumeWithoutFallback() async throws {
        let fake = try FakeChatCLI()
        let s = try HistoryList.decode(Data(FakeChatCLI.list([FakeChatCLI.summary(id: "sess-1", dir: tmp.path, profile: "gone")]).utf8)).conversations[0]
        let m = ChatModel(env: try env(fake, profiles: try profiles(["other"], active: "other")))
        m.openExisting(s)
        m.draft = "hello"
        XCTAssertEqual(m.missingProfile, "gone")
        XCTAssertFalse(m.canSend)
        XCTAssertTrue(m.blockReason!.contains("Settings > Profiles"))
        m.send()
        XCTAssertEqual(fake.runCount, 0, "no silent fallback to another profile")
    }

    func testMissingFolderNeedsExplicitOverride() async throws {
        let fake = try FakeChatCLI(runJSON: FakeChatCLI.runOK(session: "sess-1"))
        let gone = tmp.appendingPathComponent("deleted").path
        let s = try HistoryList.decode(Data(FakeChatCLI.list([FakeChatCLI.summary(id: "sess-1", dir: gone)]).utf8)).conversations[0]
        fake.write("show.json", FakeChatCLI.show(summary: FakeChatCLI.summary(id: "sess-1", dir: gone), runs: [FakeChatCLI.run(id: "r1", task: "t", result: "r")]))
        let m = ChatModel(env: try env(fake))
        m.openExisting(s)
        m.draft = "hi"
        XCTAssertTrue(m.needsFolderChoice)
        XCTAssertFalse(m.canSend)
        m.dirOverride = tmp.path
        XCTAssertTrue(m.canSend)
        m.send()
        await idle(m)
        XCTAssertEqual(fake.lastArgs, ["-r", "sess-1", "-d", tmp.path, "-json", "-t", "30m"])
    }

    func testSessionlessRecordCannotBeContinued() async throws {
        let fake = try FakeChatCLI()
        let s = try HistoryList.decode(Data(FakeChatCLI.list([FakeChatCLI.summary(id: "run:abc", session: "")]).utf8)).conversations[0]
        let m = ChatModel(env: try env(fake))
        m.openExisting(s)
        m.draft = "x"
        XCTAssertFalse(s.canResume)
        XCTAssertNil(m.session)
        XCTAssertTrue(m.blockReason!.contains("no session"))
    }

    func testBusyBlocksDuplicateSendAndExternalActiveBlocksSend() async throws {
        let fake = try FakeChatCLI(mode: .slow, runJSON: FakeChatCLI.runOK(session: "sess-1", status: "error", exit: 130))
        let s = try HistoryList.decode(Data(FakeChatCLI.list([FakeChatCLI.summary(id: "sess-1", dir: tmp.path)]).utf8)).conversations[0]
        fake.write("show.json", FakeChatCLI.show(summary: FakeChatCLI.summary(id: "sess-1", dir: tmp.path), runs: [FakeChatCLI.run(id: "r1", task: "t", result: "r")]))
        let m = ChatModel(env: try env(fake))
        m.openExisting(s)
        await waitUntil { m.entries.count == 2 }
        m.draft = "first"
        m.send()
        XCTAssertEqual(m.phase, .sending)
        m.draft = "second"
        XCTAssertFalse(m.canSend)
        m.send()
        _ = await fake.waitForFile("ready")
        XCTAssertEqual(fake.runCount, 1, "one run only")
        m.stop()
        await idle(m)
        XCTAssertEqual(fake.runCount, 1)

        // An outside worker on the same session disables Send until it is gone.
        let other = try FakeChatCLI()
        let ext = try StatsDecoder.decode(Data(StatsDecodeTests.full.utf8)) // active pid 42, session "s", running
        let m2 = ChatModel(env: try env(other, monitor: MonitorModel(testingSnapshot: ext)))
        let ms = try HistoryList.decode(Data(FakeChatCLI.list([FakeChatCLI.summary(id: "s", dir: tmp.path)]).utf8)).conversations[0]
        m2.openExisting(ms)
        m2.draft = "hi"
        XCTAssertTrue(m2.externalBusy)
        XCTAssertFalse(m2.canSend)
        XCTAssertTrue(m2.blockReason!.contains("Another offload run"))
        m2.send()
        XCTAssertEqual(other.runCount, 0)
        m2.env.monitor.setSnapshotForTesting(try StatsDecoder.decode(Data(StatsDecodeTests.empty.utf8)))
        XCTAssertTrue(m2.canSend)
    }

    func testFailedRunKeepsResultAndNonzeroJSON() async throws {
        let fake = try FakeChatCLI(mode: .fail, runJSON: FakeChatCLI.runOK(session: "sess-f", result: "partial work", status: "error", exit: 1, error: "tool crashed"))
        fake.write("show.fail", "1")
        let m = ChatModel(env: try env(fake))
        m.draft = "do it"
        m.send()
        await idle(m)
        XCTAssertEqual(m.entries.count, 2)
        XCTAssertEqual(m.entries[1].text, "partial work")
        XCTAssertEqual(m.entries[1].meta?.status, "error")
        XCTAssertEqual(m.notice, "tool crashed")
        XCTAssertEqual(m.conversationID, "sess-f", "a failed run with a session can be continued")
        XCTAssertTrue(m.entries.allSatisfy(\.local), "history could not be read, so the local record is kept")
    }

    func testNotPersistedKeepsResponseAndWarns() async throws {
        let fake = try FakeChatCLI(mode: .warn, runJSON: FakeChatCLI.runOK(session: "sess-w", result: "only in memory"))
        fake.write("show.json", FakeChatCLI.show(summary: FakeChatCLI.summary(id: "sess-w", dir: tmp.path), runs: []))
        let m = ChatModel(env: try env(fake))
        m.draft = "x"
        m.send()
        await idle(m)
        XCTAssertTrue(m.notPersisted)
        XCTAssertTrue(m.notice!.contains("not saved to the history log"))
        XCTAssertTrue(m.entries.contains { $0.text == "only in memory" }, "the response is not lost")
    }

    func testCouldNotRunReturnsTheMessageToTheComposer() async throws {
        let fake = try FakeChatCLI(runJSON: "")
        fake.write("mode", "ok")
        let m = ChatModel(env: try env(fake))
        m.draft = "keep me"
        m.send()
        await idle(m)
        XCTAssertEqual(m.draft, "keep me")
        XCTAssertTrue(m.entries.isEmpty)
        XCTAssertNotNil(m.notice)
    }

    func testUnsavedSalvagedAccountingIsUnavailable() async throws {
        var data = try XCTUnwrap(FakeChatCLI.runOK(session: "sess-salvage").data(using: .utf8))
        var json = try XCTUnwrap(JSONSerialization.jsonObject(with: data) as? [String: Any])
        json["salvaged"] = true
        data = try JSONSerialization.data(withJSONObject: json)
        let fake = try FakeChatCLI(mode: .warn, runJSON: String(decoding: data, as: UTF8.self))
        fake.write("show.fail", "1")
        let m = ChatModel(env: try env(fake))
        m.draft = "salvaged task"
        m.send()
        await idle(m)
        XCTAssertTrue(m.entries.last?.meta?.salvaged == true)
        XCTAssertNil(m.entries.last?.meta?.usage)
        XCTAssertNil(m.entries.last?.meta?.costUsd)
    }

    func testStopKeepsSessionAndLabelsStopped() async throws {
        let fake = try FakeChatCLI(mode: .slow, runJSON: FakeChatCLI.runOK(session: "sess-stop", result: "so far", status: "error", exit: 130))
        fake.write("show.fail", "1")
        let m = ChatModel(env: try env(fake))
        m.draft = "long job"
        m.send()
        _ = await fake.waitForFile("ready")
        await waitUntil { m.ownPID != nil }
        XCTAssertNotNil(m.ownPID, "PID reported on start")
        m.stop()
        XCTAssertEqual(m.phase, .stopping, "not reported done before the process exits")
        await idle(m)
        XCTAssertEqual(m.entries.last?.meta?.status, "stopped")
        XCTAssertEqual(m.conversationID, "sess-stop")
        XCTAssertTrue(m.notice!.hasPrefix("Stopped."))
        XCTAssertNil(m.ownPID)
        let saved = FakeChatCLI.run(id: "stopped-record", task: "long job", result: "so far", status: "error", session: "sess-stop")
            .replacingOccurrences(of: "2026-10-29T10:00:00Z", with: "2026-10-29 20:00:00")
        try FileManager.default.removeItem(at: fake.file("show.fail"))
        fake.write("show.json", FakeChatCLI.show(summary: FakeChatCLI.summary(id: "sess-stop", dir: tmp.path), runs: [saved]))
        await m.loadDetail()
        XCTAssertEqual(m.entries.last?.meta?.status, "stopped", "canonical reload retains this app's known cancellation")
    }

    func testProgressComesFromSharedMonitorSnapshot() async throws {
        let fake = try FakeChatCLI(mode: .slow, runJSON: FakeChatCLI.runOK(session: "sess-p", status: "error", exit: 130))
        fake.write("show.fail", "1")
        let monitor = MonitorModel(testingSnapshot: nil)
        let m = ChatModel(env: try env(fake, monitor: monitor))
        m.draft = "x"
        m.send()
        _ = await fake.waitForFile("ready")
        await waitUntil { m.ownPID != nil }
        var snap = try StatsDecoder.decode(Data(StatsDecodeTests.full.utf8))
        snap.active[0].pid = Int(m.ownPID!)
        snap.active[0].toolCalls = 7
        snap.active[0].lastTool = "Edit file.swift"
        monitor.setSnapshotForTesting(snap)
        await waitUntil { m.live != nil }
        XCTAssertEqual(m.live?.toolCalls, 7)
        XCTAssertEqual(m.live?.lastTool, "Edit file.swift")
        m.stop()
        await idle(m)
    }

    func testStoreReusesModelsAndStopsOnlyOwnRuns() async throws {
        let fake = try FakeChatCLI(mode: .slow, runJSON: FakeChatCLI.runOK(session: "sess-1", status: "error", exit: 130))
        fake.write("show.fail", "1")
        let store = ChatStore(env: try env(fake))
        let a = store.newChat()
        XCTAssertTrue(store.newChat() === a, "an empty new chat is reused")
        let s = try HistoryList.decode(Data(FakeChatCLI.list([FakeChatCLI.summary(id: "sess-1", dir: tmp.path)]).utf8)).conversations[0]
        let b = store.open(s)
        XCTAssertTrue(store.open(s) === b, "the same conversation never gets a second model")
        XCTAssertTrue(store.open(id: "sess-1") === b)
        b.draft = "run"
        b.send()
        _ = await fake.waitForFile("ready")
        XCTAssertTrue(store.hasRunningOwn)
        XCTAssertTrue(store.runningOwn.first === b)
        await store.stopAllOwn(timeout: 10)
        XCTAssertFalse(store.hasRunningOwn)
        XCTAssertEqual(b.entries.last?.meta?.status, "stopped")
    }

    func testNewChatProfilePickerFollowsActiveUntilChosenAndLocksWhileRunning() async throws {
        let fake = try FakeChatCLI(mode: .slow, runJSON: FakeChatCLI.runOK(session: "sess-pp", status: "error", exit: 130))
        fake.write("show.fail", "1")
        let p = try profiles(["work", "alt"], active: "work")
        let m = ChatModel(env: try env(fake, profiles: p))
        XCTAssertEqual(m.profileName, "work", "initialised from the active profile")
        XCTAssertEqual(m.profileChoices.map(\.name), ["work", "alt"])
        p.setSnapshotForTesting(try ProfilesSnapshot.decode(Data(Fixtures.profiles([Fixtures.profile(name: "work"), Fixtures.profile(name: "alt")], active: "alt", configured: "alt").utf8)))
        XCTAssertEqual(m.profileName, "alt", "follows the active profile until the user chooses")
        m.chooseProfile("work")
        p.setSnapshotForTesting(try ProfilesSnapshot.decode(Data(Fixtures.profiles([Fixtures.profile(name: "work"), Fixtures.profile(name: "alt")], active: "alt", configured: "alt").utf8)))
        XCTAssertEqual(m.profileName, "work", "an explicit choice is kept")
        m.draft = "go"
        m.send()
        _ = await fake.waitForFile("ready")
        XCTAssertEqual(Array(fake.lastArgs.prefix(2)), ["-p", "work"])
        m.chooseProfile("alt")
        XCTAssertEqual(m.profileName, "work", "locked while running")
        m.stop()
        await idle(m)
        m.chooseProfile("alt")
        XCTAssertEqual(m.profileName, "work", "an existing session never changes profile")
    }

    func testPickedProfileGoesToTheCLIAndMissingOneBlocks() async throws {
        let fake = try FakeChatCLI(runJSON: FakeChatCLI.runOK(session: "sess-pick"))
        fake.write("show.fail", "1")
        let p = try profiles(["work", "alt"], active: "work")
        let m = ChatModel(env: try env(fake, profiles: p))
        m.chooseProfile("alt")
        m.draft = "hi"
        m.send()
        await idle(m)
        XCTAssertEqual(Array(fake.lastArgs.prefix(2)), ["-p", "alt"])
        let n = ChatModel(env: try env(fake, profiles: try profiles(["other"], active: "other")))
        n.chooseProfile("ghost")
        n.draft = "x"
        XCTAssertTrue(n.profileMissing)
        XCTAssertFalse(n.canSend)
    }

    func testLocalChatsSectionListsUnsentAndRunningButNotCompletedHistory() async throws {
        let fake = try FakeChatCLI(mode: .slow, runJSON: FakeChatCLI.runOK(session: "sess-loc", status: "error", exit: 130))
        fake.write("show.fail", "1")
        let store = ChatStore(env: try env(fake))
        let a = store.newChat()
        XCTAssertEqual(store.localChats(historyIDs: []).map(\.localID), [a.localID], "an unsent draft is listed")
        a.draft = "start something long"
        a.send()
        _ = await fake.waitForFile("ready")
        let b = store.newChat() // a different new chat is selected while a runs
        XCTAssertFalse(a === b)
        XCTAssertTrue(store.selected === b)
        let locals = store.localChats(historyIDs: [])
        XCTAssertEqual(Set(locals.map(\.localID)), [a.localID, b.localID], "the running chat stays reachable")
        XCTAssertEqual(a.statusText, "Running")
        XCTAssertEqual(a.sidebarTitle, "start something long")
        store.select(a)
        XCTAssertTrue(store.selected === a)
        XCTAssertEqual(fake.runCount, 1, "selecting never launches again")
        a.stop()
        await idle(a)
        // Once its session is in the canonical list it is no longer shown twice; an unsent draft still is.
        let after = store.localChats(historyIDs: ["sess-loc"])
        XCTAssertFalse(after.contains { $0 === a })
        XCTAssertTrue(after.contains { $0 === b })
        XCTAssertTrue(store.newChat() === b, "an empty new chat is still reused")
    }

    func testSendCannotRaceFinalization() async throws {
        let fake = try FakeChatCLI(runJSON: FakeChatCLI.runOK(session: "sess-race", result: "first answer"))
        let first = FakeChatCLI.run(id: "r1", task: "first", result: "first answer", session: "sess-race")
        fake.write("show.json", FakeChatCLI.show(summary: FakeChatCLI.summary(id: "sess-race", dir: tmp.path), runs: [first]))
        fake.write("show.delay", "1.2")
        let m = ChatModel(env: try env(fake))
        m.workspace = tmp.path
        m.draft = "first"
        m.send()
        await waitUntil { m.phase == .finalizing }
        XCTAssertEqual(m.phase, .finalizing, "history is being re-read; the process has already ended")
        m.draft = "second"
        XCTAssertFalse(m.canSend)
        XCTAssertTrue(m.isBusy)
        m.send()
        XCTAssertEqual(fake.runCount, 1, "no second launch while finalizing")
        XCTAssertEqual(m.draft, "second", "the typed message is not lost")
        m.stop()
        XCTAssertEqual(m.phase, .finalizing, "Stop is for running work only")
        await idle(m)
        XCTAssertTrue(m.canSend)
        XCTAssertEqual(m.entries.map(\.text), ["first", "first answer"], "finalization finished before anything else could change the entries")

        // The next turn starts from the settled state and keeps both turns.
        fake.write("show.delay", "0")
        fake.write("run.json", FakeChatCLI.runOK(session: "sess-race", result: "second answer"))
        fake.write("show.json", FakeChatCLI.show(summary: FakeChatCLI.summary(id: "sess-race", dir: tmp.path, runs: 2), runs: [
            first, FakeChatCLI.run(id: "r2", task: "second", result: "second answer", session: "sess-race")]))
        m.send()
        await idle(m)
        XCTAssertEqual(fake.runCount, 2)
        XCTAssertEqual(m.entries.map(\.text), ["first", "first answer", "second", "second answer"])
        XCTAssertEqual(Array(fake.lastArgs.prefix(2)), ["-r", "sess-race"])
    }

    func testUnpersistedResponseSurvivesLaterRefreshes() async throws {
        let fake = try FakeChatCLI(mode: .warn, runJSON: FakeChatCLI.runOK(session: "sess-np", result: "never logged"))
        fake.write("show.json", FakeChatCLI.show(summary: FakeChatCLI.summary(id: "sess-np", dir: tmp.path), runs: []))
        let m = ChatModel(env: try env(fake))
        m.draft = "x"
        m.send()
        await idle(m)
        await m.loadDetail()
        await m.loadDetail()
        XCTAssertEqual(m.entries.filter { $0.text == "never logged" }.count, 1, "kept once, not duplicated")
        XCTAssertEqual(m.entries.filter { $0.text == "x" }.count, 1)
    }

    func testFocusFromRunWithoutSessionSeedsSearch() throws {
        let fake = try FakeChatCLI()
        let store = ChatStore(env: try env(fake))
        let run = RecentRun(ts: "t", status: "error", dir: "/w", task: "First line of task\nsecond line", session: "", model: "m", costUsd: nil, usage: nil, result: "", salvaged: false)
        store.focus(run: run)
        XCTAssertEqual(store.env.history.query, "First line of task")
        XCTAssertNil(store.selected)
        store.focus(run: RecentRun(ts: "t", status: "ok", dir: "/w", task: "x", session: "abc", model: "m", costUsd: nil, usage: nil, result: "", salvaged: false))
        XCTAssertEqual(store.selected?.conversationID, "abc")
    }
}

// MARK: - History API

@MainActor
final class HistoryAPITests: XCTestCase {
    func testListDecodeAndMissingFields() throws {
        let json = FakeChatCLI.list([FakeChatCLI.summary(id: "a"), FakeChatCLI.summary(id: "run:abc", session: "", usage: false, cost: false)], total: 120, hasMore: true)
        let l = try HistoryList.decode(Data(json.utf8))
        XCTAssertEqual(l.total, 120)
        XCTAssertTrue(l.hasMore)
        XCTAssertEqual(l.conversations[0].usage?.inputTokens, 10)
        XCTAssertNil(l.conversations[1].usage)
        XCTAssertNil(l.conversations[1].costUsd)
        XCTAssertFalse(l.conversations[1].canResume)
        XCTAssertEqual(l.conversations[0].projectName, "w")
        XCTAssertThrowsError(try HistoryList.decode(Data(#"{"schema_version":2}"#.utf8)))
    }

    func testDetailKeepsFullTextOver4000AndActive() throws {
        let long = String(repeating: "x", count: 9000)
        let live = #"{"pid":9,"phase":"running","session":"sess-1","last_tool":"Bash","tool_calls":4}"#
        let d = try HistoryDetail.decode(Data(FakeChatCLI.show(summary: FakeChatCLI.summary(), runs: [FakeChatCLI.run(id: "r", task: long, result: long)], active: live).utf8))
        XCTAssertEqual(d.runs[0].result.count, 9000)
        XCTAssertEqual(d.runs[0].task.count, 9000)
        XCTAssertEqual(d.active?.isRunning, true)
        XCTAssertEqual(d.active?.toolCalls, 4)
        let none = try HistoryDetail.decode(Data(FakeChatCLI.show(summary: FakeChatCLI.summary(), runs: []).utf8))
        XCTAssertNil(none.active)
        XCTAssertTrue(none.runs.isEmpty)
    }

    func testSearchGoesThroughStdinAsUTF8NotArgv() async throws {
        let fake = try FakeChatCLI()
        let q = "日本語 café \"quoted\" -n --json"
        _ = try await fake.cli.historyList(search: q, offset: 100, limit: 50)
        XCTAssertEqual(fake.read("list.stdin"), q)
        let argv = fake.histArgv[0]
        XCTAssertEqual(argv, "history list --json --limit 50 --offset 100 --search-stdin")
        XCTAssertFalse(argv.contains("café"))
        _ = try await fake.cli.historyList(search: "", offset: 0)
        XCTAssertEqual(fake.read("list.stdin"), "")
    }

    func testOversizedQueryIsCutOnACharacterBoundary() async throws {
        let fake = try FakeChatCLI()
        _ = try await fake.cli.historyList(search: String(repeating: "日", count: 2000), offset: 0)
        let data = try Data(contentsOf: fake.file("list.stdin"))
        XCTAssertLessThanOrEqual(data.count, 4096)
        XCTAssertNotNil(String(data: data, encoding: .utf8), "still valid UTF-8")
        XCTAssertEqual(data.count, 4095)
    }

    func testShowRejectsOptionLikeIDs() async throws {
        let fake = try FakeChatCLI()
        do { _ = try await fake.cli.historyShow(id: "--id"); XCTFail() } catch {}
        XCTAssertTrue(fake.histArgv.isEmpty)
    }

    func testModelPagesAndSearches() async throws {
        let fake = try FakeChatCLI()
        fake.write("list.0.json", FakeChatCLI.list([FakeChatCLI.summary(id: "a"), FakeChatCLI.summary(id: "b")], total: 3, offset: 0, hasMore: true))
        fake.write("list.2.json", FakeChatCLI.list([FakeChatCLI.summary(id: "c"), FakeChatCLI.summary(id: "b")], total: 3, offset: 2, hasMore: false))
        let m = HistoryModel(cli: fake.cli, debounce: 0.01)
        await m.reload()
        XCTAssertEqual(m.items.map(\.id), ["a", "b"])
        XCTAssertTrue(m.hasMore)
        await m.loadMore()
        XCTAssertEqual(m.items.map(\.id), ["a", "b", "c"], "a repeated id is not shown twice")
        XCTAssertFalse(m.hasMore)
        XCTAssertEqual(m.total, 3)
        XCTAssertTrue(fake.histArgv.contains("history list --json --limit 50 --offset 2 --search-stdin"))
        fake.write("list.0.json", FakeChatCLI.list([FakeChatCLI.summary(id: "z", title: "found")], total: 1))
        m.query = "needle"
        await waitUntil { m.items.map(\.id) == ["z"] }
        XCTAssertEqual(m.items.map(\.id), ["z"])
        XCTAssertEqual(fake.read("list.stdin"), "needle")
    }

    func testOverlappingReloadsRunSequentiallyAndEndOnTheLatest() async throws {
        let fake = try FakeChatCLI()
        fake.write("list.0.json", FakeChatCLI.list([FakeChatCLI.summary(id: "a")]))
        let m = HistoryModel(cli: fake.cli, debounce: 0)
        async let one: Void = m.reload()
        async let two: Void = m.reload()
        _ = await (one, two)
        await waitUntil { !m.loading }
        XCTAssertEqual(m.items.map(\.id), ["a"])
        XCTAssertLessThanOrEqual(fake.histArgv.count, 3)
    }

    func testErrorIsReportedNotSwallowed() async throws {
        let fake = try FakeChatCLI()
        fake.write("list.0.json", "not json")
        let m = HistoryModel(cli: fake.cli, debounce: 0)
        await m.reload()
        XCTAssertNotNil(m.error)
        XCTAssertTrue(m.items.isEmpty)
    }

    func testEntriesBuiltFromRunsAreUserThenAssistantWithoutToolTranscript() throws {
        let d = try HistoryDetail.decode(Data(FakeChatCLI.show(summary: FakeChatCLI.summary(runs: 2), runs: [
            FakeChatCLI.run(id: "r1", task: "ask one", result: "answer one"), FakeChatCLI.run(id: "r2", task: "ask two", result: "", status: "error")]).utf8))
        let e = ChatEntry.entries(from: d.runs)
        XCTAssertEqual(e.map(\.role), [.user, .assistant, .user, .assistant])
        XCTAssertEqual(e.map(\.text), ["ask one", "answer one", "ask two", ""])
        XCTAssertEqual(e[3].meta?.status, "error")
        XCTAssertEqual(Set(e.map(\.id)).count, 4)
    }

    func testDateFormatting() {
        XCTAssertEqual(HistoryDates.short("not a date"), "not a date")
        XCTAssertNotNil(HistoryDates.parse("2026-10-29 18:19:51"))
        XCTAssertNotNil(HistoryDates.parse("2026-10-29T18:19:51+08:00"))
    }
}

// MARK: - Markdown

final class MarkdownTests: XCTestCase {
    func testHeadingsParagraphsAndRules() {
        let b = Markdown.parse("# Title\n\nSome *text*\nsecond line\n\n---\n\n### Sub ###")
        XCTAssertEqual(b, [.heading(level: 1, text: "Title"), .paragraph("Some *text*\nsecond line"), .rule, .heading(level: 3, text: "Sub")])
        XCTAssertEqual(Markdown.parse("#nospace"), [.paragraph("#nospace")], "not a heading, kept as text")
        XCTAssertEqual(Markdown.parse("####### seven"), [.paragraph("####### seven")])
    }

    func testHeadingKeepsHashThatIsPartOfTheText() {
        XCTAssertEqual(Markdown.parse("# C#"), [.heading(level: 1, text: "C#")])
        XCTAssertEqual(Markdown.parse("## F# and C##"), [.heading(level: 2, text: "F# and C##")])
        XCTAssertEqual(Markdown.parse("## Title ##"), [.heading(level: 2, text: "Title")])
        XCTAssertEqual(Markdown.parse("# Title   #"), [.heading(level: 1, text: "Title")])
        XCTAssertEqual(Markdown.parse("### Using C# ###"), [.heading(level: 3, text: "Using C#")])
        XCTAssertEqual(Markdown.parse("# #"), [.heading(level: 1, text: "")])
    }

    func testListsAndNumbering() {
        XCTAssertEqual(Markdown.parse("- a\n- b\n  more\n* c"), [.bullets(["a", "b more", "c"])])
        XCTAssertEqual(Markdown.parse("3. x\n4. y"), [.numbered(start: 3, items: ["x", "y"])])
        XCTAssertEqual(Markdown.parse("1) one\n2) two"), [.numbered(start: 1, items: ["one", "two"])])
        XCTAssertEqual(Markdown.parse("-not a list"), [.paragraph("-not a list")])
    }

    func testBlockquotesNest() {
        XCTAssertEqual(Markdown.parse("> quoted\n> text\n>\n> - item"), [.quote([.paragraph("quoted\ntext"), .bullets(["item"])])])
        XCTAssertEqual(Markdown.parse("> a\n>> b").count, 1)
    }

    func testFencedCodeKeepsLanguageAndContentVerbatim() {
        let src = "before\n```swift\nlet x = 1\n\n# not a heading\n```\nafter"
        XCTAssertEqual(Markdown.parse(src), [.paragraph("before"), .code(language: "swift", text: "let x = 1\n\n# not a heading", closed: true), .paragraph("after")])
        XCTAssertEqual(Markdown.parse("~~~\nx\n~~~"), [.code(language: "", text: "x", closed: true)])
        XCTAssertEqual(Markdown.parse("````\n```\ninner\n```\n````"), [.code(language: "", text: "```\ninner\n```", closed: true)], "longer fence contains shorter ones")
    }

    func testUnclosedFenceKeepsEverything() {
        let b = Markdown.parse("intro\n```python\nprint(1)\n- not a list\n# no heading")
        XCTAssertEqual(b, [.paragraph("intro"), .code(language: "python", text: "print(1)\n- not a list\n# no heading", closed: false)])
        XCTAssertTrue(Markdown.plain(b).contains("# no heading"))
    }

    func testTables() {
        let src = "| Name | Qty |\n|:-----|----:|\n| a | 1 |\n| b |\n| c | 3 | extra |"
        let parsed = Markdown.parse(src)
        guard case .table(let h, let al, let rows)? = parsed.first else { return XCTFail() }
        XCTAssertEqual(h, ["Name", "Qty"])
        XCTAssertEqual(al, [.left, .right])
        XCTAssertEqual(rows, [["a", "1"], ["b", ""]])
        XCTAssertEqual(parsed.dropFirst().first, .paragraph("| c | 3 | extra |"), "an overflowing row is kept as text, not trimmed")
        XCTAssertEqual(Markdown.parse("| a | b |\nplain"), [.paragraph("| a | b |\nplain")], "no separator row: plain text")
        XCTAssertEqual(Markdown.cells("a \\| b | c"), ["a \\| b", "c"])
    }

    func testMalformedInputNeverLosesText() {
        for src in ["**unclosed bold", "[link](", "`tick", "| | |", "> ", "```", "* ", "- ", "1. ", "![](x)", "\u{0}weird", "", "   "] {
            let b = Markdown.parse(src)
            let plain = Markdown.plain(b)
            for token in src.split(whereSeparator: { " \n".contains($0) }) where token.count > 2 && !"```".contains(token) {
                XCTAssertTrue(plain.contains(token.replacingOccurrences(of: "\u{0}", with: "")) || token == "![](x)", "lost \(token) from \(src.debugDescription)")
            }
        }
        let text = String(repeating: "word ", count: 20_000) + "\n```\nopen"
        XCTAssertEqual(Markdown.parse(text).count, 2)
    }

    func testCRLFAndDeepQuotesAreSafe() {
        XCTAssertEqual(Markdown.parse("# a\r\n\r\ntext\r\n"), [.heading(level: 1, text: "a"), .paragraph("text")])
        let deep = String(repeating: ">", count: 200) + " x"
        XCTAssertFalse(Markdown.parse(deep).isEmpty)
    }

    func testInlineStylesAndLinkSafety() {
        let a = Markdown.inline("a **bold** and `code` and [site](https://example.com) and [bad](javascript:alert(1)) and [f](file:///etc/passwd)")
        let links = a.runs.compactMap(\.link)
        XCTAssertEqual(links.map(\.absoluteString), ["https://example.com"], "only http(s) stays clickable")
        XCTAssertTrue(String(a.characters).contains("bad"), "the link text is kept")
        XCTAssertEqual(String(Markdown.inline("**unclosed").characters).contains("unclosed"), true)
        XCTAssertEqual(String(Markdown.inline("line1\nline2").characters), "line1\nline2", "line breaks preserved")
        XCTAssertFalse(Markdown.isSafeLink(URL(string: "mailto:a@b.c")!))
        XCTAssertTrue(Markdown.isSafeLink(URL(string: "http://localhost:8080/x")!))
    }

    func testImagesBecomeAltTextAndAreNeverLinks() {
        XCTAssertEqual(Markdown.removeImages("see ![diagram](https://evil/x.png) here"), "see diagram here")
        XCTAssertEqual(Markdown.removeImages("![](x)"), "[image]")
        let a = Markdown.inline("![alt](https://evil.example/pixel.gif)")
        XCTAssertTrue(a.runs.compactMap(\.link).isEmpty)
        XCTAssertEqual(String(a.characters), "alt")
    }
}

// MARK: - Layout

@MainActor
final class ChatLayoutTests: XCTestCase {
    func testMainPanelStaysWithin680WithNewChatButtonAndLongNames() throws {
        let fit = PanelFitTests()
        let long = String(repeating: "very-long-profile-name-", count: 6)
        let profiles = ProfilesModel(testingSnapshot: try ProfilesSnapshot.decode(Data(Fixtures.profiles(
            [Fixtures.profile(name: long, model: String(repeating: "model-", count: 20))], active: long, source: "environment", configured: long).utf8)))
        let snap = try fit.snapshot(active: 7, recent: 20, groups: 15, warnings: 4)
        let h = fit.fitHeight(MonitorModel(testingSnapshot: snap, error: String(repeating: "x", count: 400)),
                              fit.updates(.failed(String(repeating: "problem ", count: 40))), profiles: profiles)
        XCTAssertLessThanOrEqual(h, Layout.height, "needs \(h)")
        let plain = fit.fitHeight(MonitorModel(testingSnapshot: try fit.snapshot(active: 0, recent: 0, groups: 0, warnings: 0)), fit.updates())
        XCTAssertLessThanOrEqual(plain, 633 + 16, "the header row grew more than the two-line status allows")
    }

    func testLongWrappedMessagesCanAlwaysBeExpanded() {
        XCTAssertFalse(UserBubble.isLong("short"))
        XCTAssertTrue(UserBubble.isLong(String(repeating: "日", count: 100)), "wrapped CJK text with no newline can pass four lines")
        XCTAssertTrue(UserBubble.isLong(String(repeating: "word ", count: 20)))
        XCTAssertTrue(UserBubble.isLong("a\nb\nc\nd\ne"))
        XCTAssertFalse(UserBubble.isLong(String(repeating: "x", count: UserBubble.longThreshold)))
    }

    func testChatPaneKeepsItsSizeAtMinimumWidthWithLongNames() throws {
        let fake = try FakeChatCLI()
        let d = UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!
        let longName = String(repeating: "very-long-profile-", count: 8)
        let env = ChatEnv(cli: fake.cli, profiles: ProfilesModel(testingSnapshot: try ProfilesSnapshot.decode(Data(Fixtures.profiles([Fixtures.profile(name: longName, model: String(repeating: "model-", count: 20))], active: longName).utf8))),
                          monitor: MonitorModel(testingSnapshot: nil), history: HistoryModel(testingItems: []), prefs: Preferences(defaults: d))
        let chat = ChatModel(env: env)
        let s = try HistoryList.decode(Data(FakeChatCLI.list([FakeChatCLI.summary(id: "s", title: String(repeating: "Long title ", count: 20), dir: "/" + String(repeating: "deep/", count: 30) + "project", profile: longName)]).utf8)).conversations[0]
        chat.openExisting(s)
        for item in [chat, ChatModel(env: env)] {
            let size = NSSize(width: 760 - ChatLayout.sidebar, height: 520)
            let host = NSHostingView(rootView: ChatDetailView(chat: item, profiles: env.profiles, monitor: env.monitor))
            let window = NSWindow(contentRect: NSRect(origin: .zero, size: size), styleMask: [.titled], backing: .buffered, defer: false)
            window.contentView = host
            host.layoutSubtreeIfNeeded()
            // Long names and paths cannot resize the pane. (Whether each badge truncates cleanly at this width is a visual check.)
            XCTAssertEqual(host.frame.size, size)
        }
    }

    func scrollViews(in v: NSView) -> [NSScrollView] {
        var out: [NSScrollView] = []
        func walk(_ x: NSView) { if let s = x as? NSScrollView { out.append(s) }; x.subviews.forEach(walk) }
        walk(v)
        return out
    }

    func testChatDetailKeepsComposerAndTranscriptUsableWithHugeContent() throws {
        let fake = try FakeChatCLI()
        let d = UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!
        let env = ChatEnv(cli: fake.cli, profiles: ProfilesModel(testingSnapshot: try ProfilesSnapshot.decode(Data(Fixtures.profiles([Fixtures.profile(name: "w")], active: "w").utf8))),
                          monitor: MonitorModel(testingSnapshot: nil), history: HistoryModel(testingItems: []), prefs: Preferences(defaults: d))
        let chat = ChatModel(env: env)
        let hugeTask = String(repeating: "Implement the trend chart with exact buckets.\n", count: 60)
        let hugeResult = "# Report\n\n" + String(repeating: "A paragraph of **findings** with `code`.\n\n", count: 60) + "```\n" + String(repeating: "line of code\n", count: 50) + "```\n\n| a | b |\n|---|---|\n| 1 | 2 |"
        let run = ConversationRun(id: "r", ts: "2026-10-29T10:00:00Z", status: "ok", exitCode: 0, task: hugeTask, result: hugeResult, error: "", dir: "/w", profile: "w",
                                  backend: "claude", model: "m", session: "s", usage: nil, costUsd: nil, salvaged: false, elapsedMs: 1000, turns: 1)
        let summary = try HistoryList.decode(Data(FakeChatCLI.list([FakeChatCLI.summary(id: "s", title: String(repeating: "Long title ", count: 20))]).utf8)).conversations[0]
        chat.openExisting(summary)
        chat.draft = String(repeating: "typing a long draft message\n", count: 30)
        _ = run
        let size = NSSize(width: 700, height: 720)
        let host = NSHostingView(rootView: ChatDetailView(chat: chat, profiles: env.profiles, monitor: env.monitor))
        let window = NSWindow(contentRect: NSRect(origin: .zero, size: size), styleMask: [.titled], backing: .buffered, defer: false)
        window.contentView = host
        host.layoutSubtreeIfNeeded()
        XCTAssertEqual(host.frame.size, size, "content cannot resize the window")
        let scrolls = scrollViews(in: host)
        XCTAssertGreaterThanOrEqual(scrolls.count, 1)
        let bounds = host.bounds
        for s in scrolls { XCTAssertTrue(bounds.insetBy(dx: -1, dy: -1).contains(s.convert(s.bounds, to: host)), "a scroll area leaves the window") }
        let tallest = scrolls.map(\.frame.height).max() ?? 0
        XCTAssertGreaterThan(tallest, 250, "the transcript keeps most of the height; the composer is bounded")
    }

    func testChatWindowSidebarAndDetailFitDefaultSize() throws {
        let fake = try FakeChatCLI()
        let d = UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!
        let items = try HistoryList.decode(Data(FakeChatCLI.list((0..<60).map { FakeChatCLI.summary(id: "s\($0)", title: String(repeating: "Title \($0) ", count: 12)) }, total: 200, hasMore: true).utf8)).conversations
        let history = HistoryModel(testingItems: items, total: 200, hasMore: true)
        let env = ChatEnv(cli: fake.cli, profiles: ProfilesModel(testingSnapshot: nil), monitor: MonitorModel(testingSnapshot: nil), history: history, prefs: Preferences(defaults: d))
        let store = ChatStore(env: env)
        store.newChat()
        let host = NSHostingView(rootView: ChatWindowView(store: store, history: history, profiles: env.profiles))
        let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: ChatLayout.width, height: ChatLayout.height), styleMask: [.titled, .resizable], backing: .buffered, defer: false)
        window.contentView = host
        host.layoutSubtreeIfNeeded()
        XCTAssertEqual(host.frame.size, NSSize(width: ChatLayout.width, height: ChatLayout.height))
        XCTAssertGreaterThanOrEqual(scrollViews(in: host).count, 1)
    }
}
