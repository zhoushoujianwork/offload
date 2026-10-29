import Foundation

enum CLIError: LocalizedError, Equatable {
    case unavailable
    case launch
    case timedOut
    case tooLarge
    /// A failure message that is already safe to show.
    case failed(String)
    case badOutput(String)
    case invalidArgument(String)

    var errorDescription: String? {
        switch self {
        case .unavailable: return StatsError.cliNotFound.errorDescription
        case .launch: return "Could not start the offload command."
        case .timedOut: return "The offload command did not answer in time."
        case .tooLarge: return "The offload command produced more output than expected."
        case .failed(let m): return m
        case .badOutput(let m): return "The offload command returned unreadable data (\(m)). The app and the bundled command may be out of sync."
        case .invalidArgument(let m): return m
        }
    }
}

/// Runs the bundled `offload` for profile and agent-skill management. Request bodies (which may hold a
/// secret) go through stdin only, never argv, and nothing from stdin or stdout is logged.
struct OffloadCLI {
    var executable: URL
    var timeout: TimeInterval = 15

    func run(_ args: [String], stdin: Data? = nil, secrets: [String] = [], maxOutput: Int = 2_000_000) async throws -> Data {
        let out: ProcessOutput
        do {
            out = try await ProcessRunner.run(executable: executable, arguments: args, timeout: timeout, maxOutput: maxOutput, stdin: stdin)
        } catch { throw CLIError.launch }
        if out.timedOut { throw CLIError.timedOut }
        if out.truncated { throw CLIError.tooLarge }
        guard out.status == 0 else {
            throw CLIError.failed(Self.sanitize(stderr: out.stderr, secrets: secrets, status: out.status))
        }
        return out.stdout
    }

    /// First line of stderr, trimmed and bounded, with any secret removed. Falls back to a generic message.
    static func sanitize(stderr: Data, secrets: [String], status: Int32) -> String {
        var line = String(decoding: stderr.prefix(2000), as: UTF8.self)
            .split(whereSeparator: \.isNewline).first.map(String.init)?
            .trimmingCharacters(in: .whitespaces) ?? ""
        if line.hasPrefix("offload:") { line = String(line.dropFirst("offload:".count)).trimmingCharacters(in: .whitespaces) }
        for s in secrets where s.count >= 3 && line.contains(s) { line = "" }
        if line.isEmpty || line.unicodeScalars.contains(where: { $0.value < 0x20 }) {
            return "The offload command failed (exit \(status))."
        }
        return line.count > 200 ? String(line.prefix(200)) + "…" : line
    }

    // MARK: Profiles

    static func validArgumentName(_ name: String) -> Bool {
        !name.isEmpty && name.count <= 128 && !name.hasPrefix("-") && !name.unicodeScalars.contains { $0.value < 0x20 }
    }

    func profiles() async throws -> ProfilesSnapshot {
        try ProfilesSnapshot.decode(try await run(["profiles", "--json"]))
    }

    /// Existing text-mode command: `offload default <name>`.
    func useProfile(_ name: String) async throws {
        guard Self.validArgumentName(name) else { throw CLIError.invalidArgument("That profile name cannot be selected from here.") }
        _ = try await run(["default", name])
    }

    func saveProfile(_ request: ProfileSaveRequest) async throws -> ProfilesSnapshot {
        let body: Data
        do { body = try JSONEncoder().encode(request) } catch { throw CLIError.invalidArgument("The profile could not be encoded.") }
        let secrets = request.secret.map { [$0] } ?? []
        return try ProfilesSnapshot.decode(try await run(["profile", "save", "--json"], stdin: body, secrets: secrets))
    }

    // MARK: Agent skills

    func agentStatus() async throws -> SkillsSnapshot {
        try SkillsSnapshot.decode(try await run(["agent", "status", "--json"]))
    }

    func setAgent(_ agent: String, enabled: Bool) async throws {
        guard AgentSkill.knownAgents.contains(agent) else { throw CLIError.invalidArgument("Unknown agent.") }
        _ = try await run(["agent", enabled ? "enable" : "disable", "--agent", agent, "--json"])
    }
}

// MARK: - History (read-only)

extension OffloadCLI {
    static let maxSearchBytes = 4096

    /// A page of conversations, latest first. The query goes through stdin as raw UTF-8 (never argv), capped at 4096 bytes.
    func historyList(search: String, offset: Int, limit: Int = 50) async throws -> HistoryList {
        var q = Data(search.utf8)
        if q.count > Self.maxSearchBytes {
            // Cut on a character boundary so the query stays valid UTF-8.
            var cut = Self.maxSearchBytes
            while cut > 0, (q[cut] & 0xC0) == 0x80 { cut -= 1 }
            q = q.prefix(cut)
        }
        let args = ["history", "list", "--json", "--limit", String(max(1, min(limit, 200))), "--offset", String(max(0, offset)), "--search-stdin"]
        return try HistoryList.decode(try await run(args, stdin: q, maxOutput: 16 << 20))
    }

    func historyShow(id: String) async throws -> HistoryDetail {
        guard Self.validArgumentName(id) else { throw CLIError.invalidArgument("That conversation cannot be opened.") }
        return try HistoryDetail.decode(try await run(["history", "show", "--json", "--id", id], maxOutput: 16 << 20))
    }
}
