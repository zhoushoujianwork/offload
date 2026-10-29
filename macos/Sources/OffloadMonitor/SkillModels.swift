import Foundation

// MARK: - `offload agent status --json`

struct AgentSkill: Codable, Equatable, Identifiable {
    static let knownAgents = ["codex", "claude"]

    var agent: String
    var status: String
    var enabled: Bool
    var installed: Bool
    var skillState: String
    var policyState: String
    var skillPath: String
    var policyPath: String
    var skillCurrent: Bool
    var policyCurrent: Bool
    var mode: String
    var overrideShadowed: Bool
    var canEnable: Bool
    var canDisable: Bool
    var message: String
    var restartRequired: Bool
    var id: String { agent }

    var title: String { agent == "codex" ? "Codex" : (agent == "claude" ? "Claude Code" : agent.capitalized) }

    /// Enabled on disk but the installed files are older than this app's: enabling again refreshes them.
    var updateAvailable: Bool { enabled && !(skillCurrent && policyCurrent) }

    enum Action: Equatable { case enable, update, disable }

    /// The one-click actions that the CLI says are allowed. No force option exists.
    var actions: [Action] {
        var a: [Action] = []
        if updateAvailable { if canEnable { a.append(.update) } }
        else if !enabled && canEnable { a.append(.enable) }
        if canDisable { a.append(.disable) }
        return a
    }

    var statusLabel: String {
        switch status {
        case "enabled": return updateAvailable ? "Enabled · update available" : "Enabled"
        case "disabled": return "Disabled"
        case "partial": return "Partly installed"
        case "blocked": return "Blocked"
        case "shadowed": return "Shadowed"
        default: return status.capitalized
        }
    }
}

struct SkillsSnapshot: Codable, Equatable {
    var schemaVersion: Int
    var scope: String
    var mode: String
    var agents: [AgentSkill]
    var warnings: [String]

    static func decode(_ data: Data) throws -> SkillsSnapshot {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        do {
            struct Probe: Decodable { var schemaVersion: Int }
            guard try d.decode(Probe.self, from: data).schemaVersion == 1 else { throw CLIError.badOutput("unsupported schema") }
            return try d.decode(SkillsSnapshot.self, from: data)
        } catch let e as CLIError { throw e } catch { throw CLIError.badOutput("agent status") }
    }
}
