import Foundation

// MARK: - Result of one `offload ... -json` run

/// The structured run record printed by the CLI. Decoded from stdout first, even when the exit code is not zero.
struct RunResult: Decodable, Equatable {
    var ts: String?
    var status: String
    var exitCode: Int?
    var profile: String?
    var model: String?
    var dir: String?
    var session: String?
    var turns: Int?
    var costUsd: Double?
    var usage: TokenUsage?
    var elapsedMs: Int64?
    var result: String?
    var error: String?
    var salvaged: Bool?

    static func decode(_ data: Data) -> RunResult? {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        // The CLI prints one JSON object; tolerate leading log lines by taking the last line that parses.
        if let r = try? d.decode(RunResult.self, from: data) { return r }
        for line in String(decoding: data, as: UTF8.self).split(separator: "\n").reversed() {
            if let r = try? d.decode(RunResult.self, from: Data(line.utf8)) { return r }
        }
        return nil
    }
}

/// How a send ended, for the UI.
struct ChatOutcome: Equatable {
    enum Kind: Equatable { case finished, failed, timedOut, stopped, couldNotRun }
    var kind: Kind
    var result: RunResult?
    /// Plain-language problem, when there is one.
    var message: String?
    /// The CLI could not write its run log, so the response only exists in memory.
    var notPersisted: Bool
    var session: String? { result?.session.flatMap { $0.isEmpty ? nil : $0 } }
}

// MARK: - Command construction

struct ChatTarget: Equatable {
    /// Provider session to resume; nil for a new conversation.
    var session: String?
    /// Profile for a new conversation. Resumed conversations keep their original profile.
    var profile: String?
    var dir: String
    var allowWrites: Bool
}

enum ChatCommand {
    static let timeoutFlag = "30m"
    /// CLI timeout (30m) plus salvage time.
    static let outerTimeout: TimeInterval = 35 * 60
    static let maxOutput = 16 << 20

    /// The task is never part of this list: it goes through stdin.
    static func arguments(_ t: ChatTarget) -> [String] {
        var a: [String] = []
        if let s = t.session, !s.isEmpty {
            a += ["-r", s]
        } else if let p = t.profile, !p.isEmpty {
            a += ["-p", p]
        }
        a += ["-d", t.dir, "-json", "-t", timeoutFlag]
        if t.allowWrites { a.append("-w") }
        return a
    }

    /// Existing PATH first, then the usual tool locations. No shell startup files are read.
    static func environment(base: [String: String] = ProcessInfo.processInfo.environment, home: String = NSHomeDirectory()) -> [String: String] {
        var env = base
        let extra = ["\(home)/.local/bin", "\(home)/go/bin", "\(home)/.claude/local", "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin"]
        var seen = Set<String>()
        var parts: [String] = []
        for p in (base["PATH"] ?? "").split(separator: ":").map(String.init) + extra where !p.isEmpty && seen.insert(p).inserted { parts.append(p) }
        env["PATH"] = parts.joined(separator: ":")
        return env
    }

    /// `warning=run log not written:` in the CLI's summary line.
    static func notPersisted(stderr: Data) -> Bool {
        String(decoding: stderr.suffix(8192), as: UTF8.self).contains("warning=run log not written:")
    }

    /// Classifies a finished process. JSON on stdout wins over the exit code.
    static func outcome(_ out: ProcessOutput) -> ChatOutcome {
        let np = notPersisted(stderr: out.stderr)
        let parsed = RunResult.decode(out.stdout)
        if out.truncated {
            return ChatOutcome(kind: .failed, result: parsed, message: "The response was larger than the allowed size and was cut off.", notPersisted: np)
        }
        if out.timedOut {
            // The run hit the outer time limit and was interrupted; whatever it printed is kept.
            return ChatOutcome(kind: .timedOut, result: parsed, message: parsed?.error ?? "The run did not finish in time and was stopped.", notPersisted: np)
        }
        if let r = parsed, r.status == "ok", out.status == 0 {
            // The run really finished, even if Stop was clicked just before it ended.
            return ChatOutcome(kind: .finished, result: r, message: nil, notPersisted: np)
        }
        if out.cancelled {
            // Stopped by the user: report what the CLI printed, but never as a normal success.
            return ChatOutcome(kind: .stopped, result: parsed, message: parsed == nil ? "Stopped before a result was produced." : nil, notPersisted: np)
        }
        if let r = parsed {
            switch r.status {
            case "ok" where out.status == 0: return ChatOutcome(kind: .finished, result: r, message: nil, notPersisted: np)
            case "timeout": return ChatOutcome(kind: .timedOut, result: r, message: r.error ?? "The run timed out.", notPersisted: np)
            default: return ChatOutcome(kind: .failed, result: r, message: r.error ?? "The run failed (exit \(out.status)).", notPersisted: np)
            }
        }
        let tail = OffloadCLI.sanitize(stderr: out.stderr, secrets: [], status: out.status)
        return ChatOutcome(kind: .couldNotRun, result: nil, message: tail, notPersisted: np)
    }
}

// MARK: - Execution

/// Runs one chat turn through the bundled CLI. Does not touch the main thread.
struct ChatRunner {
    var executable: URL
    var environment: [String: String] = ChatCommand.environment()
    var timeout: TimeInterval = ChatCommand.outerTimeout
    var maxOutput: Int = ChatCommand.maxOutput

    func send(task: String, target: ChatTarget, control: ProcessControl, onStart: @escaping @Sendable (Int32) -> Void) async -> ChatOutcome {
        if control.isCancelled { return ChatOutcome(kind: .stopped, result: nil, message: "Stopped before it started.", notPersisted: false) }
        let out: ProcessOutput
        do {
            out = try await ProcessRunner.run(executable: executable, arguments: ChatCommand.arguments(target), environment: environment,
                                              timeout: timeout, maxOutput: maxOutput, stdin: Data(task.utf8),
                                              control: control, onStart: onStart)
        } catch is CancellationError {
            return ChatOutcome(kind: .stopped, result: nil, message: "Stopped before it started.", notPersisted: false)
        } catch {
            return ChatOutcome(kind: .couldNotRun, result: nil, message: "Could not start the offload command.", notPersisted: false)
        }
        return ChatCommand.outcome(out)
    }
}

// MARK: - Workspace

enum Workspaces {
    static var defaultRoot: URL {
        FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
            .appendingPathComponent("OffloadMonitor/Workspaces", isDirectory: true)
    }

    /// The general-purpose workspace path. Nothing is created here.
    static func general(root: URL = defaultRoot) -> URL { root.appendingPathComponent("General", isDirectory: true) }

    /// Creates the folder (owner-only) at send time. Existing folders are used as they are.
    @discardableResult
    static func ensure(_ url: URL) throws -> URL {
        try FileManager.default.createDirectory(at: url, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        return url
    }

    static func exists(_ path: String) -> Bool {
        var isDir: ObjCBool = false
        return FileManager.default.fileExists(atPath: path, isDirectory: &isDir) && isDir.boolValue
    }
}
