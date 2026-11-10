import Foundation

// MARK: - `offload status --json` / `offload enable|disable --json`

/// Work that was already running or queued when the status was read. Counts are nil when the CLI could not
/// read the live state: unknown is not zero.
struct SwitchRunning: Codable, Equatable {
    var status: String
    var processing: Int?
    var queued: Int?
    var cleaning: Int?
    var unresolved: Int?
    var lost: Int?
    var active: Int?
    var handling: String?
}

struct SwitchInstructions: Codable, Equatable {
    var updated: Bool
    var newSessionRequired: Bool
    var error: String?
}

struct OffloadSwitch: Codable, Equatable {
    var schemaVersion: Int
    var enabled: Bool
    var newExecutions: String
    var errorKind: String?
    var reason: String?
    var configPath: String
    var running: SwitchRunning
    var instructions: SwitchInstructions?

    /// The stable refusal kind that every blocked execution reports.
    static let disabledKind = "globally_disabled"

    static func decode(_ data: Data) throws -> OffloadSwitch {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        do {
            struct Probe: Decodable { var schemaVersion: Int }
            guard try d.decode(Probe.self, from: data).schemaVersion == 1 else { throw CLIError.badOutput("unsupported schema") }
            return try d.decode(OffloadSwitch.self, from: data)
        } catch let e as CLIError { throw e } catch { throw CLIError.badOutput("status") }
    }

    var isEnabled: Bool { enabled }
    var blocksNewExecutions: Bool { newExecutions == "blocked" }
}

/// What the switch UI says. Pure functions, so every state can be tested without a window.
enum SwitchPresentation {
    /// "Offload is on" / "Offload is off". The state is the persisted one, never a guess.
    static func title(_ s: OffloadSwitch?) -> String {
        guard let s else { return "Offload status unknown" }
        return s.enabled ? "Offload is on" : "Offload is off"
    }

    /// The two things the issue asks to keep apart: new executions, and work that is still running.
    static func newExecutionsLine(_ s: OffloadSwitch) -> String {
        s.blocksNewExecutions
            ? "New executions are blocked: new tasks, resumes, queued and scheduled tasks, and profile checks do not start."
            : "New executions are allowed."
    }

    static func runningLine(_ s: OffloadSwitch) -> String {
        guard let active = s.running.active else { return "Running tasks: unknown, the live state could not be read." }
        let bound = s.running.status == "partial" ? "at least " : ""
        if active == 0 { return s.blocksNewExecutions ? "No tasks are running." : "No tasks are running right now." }
        var parts: [String] = []
        if let n = s.running.processing, n > 0 { parts.append("\(n) running") }
        if let n = s.running.queued, n > 0 { parts.append("\(n) queued") }
        if let n = s.running.cleaning, n > 0 { parts.append("\(n) cleaning up") }
        if let n = s.running.unresolved, n > 0 { parts.append("\(n) unresolved") }
        let list = parts.isEmpty ? "\(active)" : parts.joined(separator: ", ")
        if s.blocksNewExecutions {
            return "\(bound)\(list). Running tasks keep going until they finish; queued ones will not start."
        }
        return "\(bound)\(list)."
    }

    /// The disabled instructions only reach agent sessions opened after the change.
    static let newSessionNote = "Open a new Codex or Claude Code session to load the updated instructions. An open session may still try to delegate, and the CLI will refuse it."

    /// Short banner text for the main panel and chat.
    static let chatBlockReason = "Offload is off. Turn it on in Settings > Offload to send messages."

    static func banner(_ s: OffloadSwitch) -> (title: String, message: String)? {
        guard s.blocksNewExecutions else { return nil }
        let active = s.running.active ?? 0
        let tail = active > 0 ? " \(active) task\(active == 1 ? "" : "s") still running." : " No tasks running."
        return ("Offload is off", "New executions are blocked." + tail)
    }
}
