import Foundation

enum StatsError: LocalizedError, Equatable {
    case cliNotFound
    case launchFailed(String)
    case timedOut(Int)
    case outputTooLarge
    case failed(status: Int32, message: String)
    case invalidOutput(String)
    case unsupportedSchema(Int)
    case periodMismatch(expected: String, got: String)

    var errorDescription: String? {
        switch self {
        case .cliNotFound: return "The bundled offload command was not found. Reinstall Offload Monitor."
        case .launchFailed(let m): return "Could not start the offload command: \(m)"
        case .timedOut(let s): return "The offload command did not answer within \(s) seconds."
        case .outputTooLarge: return "The offload command produced more output than expected."
        case .failed(let status, let m): return "offload stats failed (exit \(status))" + (m.isEmpty ? "." : ": \(m)")
        case .invalidOutput(let m): return "offload stats returned unreadable data (\(m)). The app and the bundled command may be out of sync."
        case .unsupportedSchema(let v): return "offload stats uses schema version \(v), which this app does not understand. Update Offload Monitor."
        case .periodMismatch(let e, let g): return "offload stats answered for period \(g) instead of \(e)."
        }
    }
}

struct StatsClient {
    var executable: URL
    var timeout: TimeInterval = 10

    /// Bundled CLI first; `OFFLOAD_MONITOR_CLI` or a PATH lookup serve `swift run` development.
    static func locateCLI(bundle: Bundle = .main, environment: [String: String] = ProcessInfo.processInfo.environment) -> URL? {
        let fm = FileManager.default
        if let bundled = bundle.resourceURL?.appendingPathComponent("offload"), fm.isExecutableFile(atPath: bundled.path) {
            return bundled
        }
        if let override = environment["OFFLOAD_MONITOR_CLI"], fm.isExecutableFile(atPath: override) {
            return URL(fileURLWithPath: override)
        }
        guard bundle.bundleURL.pathExtension != "app" else { return nil }
        let dirs = (environment["PATH"] ?? "").split(separator: ":").map(String.init) + ["/opt/homebrew/bin", "/usr/local/bin"]
        for d in dirs {
            let p = (d as NSString).appendingPathComponent("offload")
            if fm.isExecutableFile(atPath: p) { return URL(fileURLWithPath: p) }
        }
        return nil
    }

    func fetch(period: StatsPeriod) async throws -> StatsSnapshot {
        let out: ProcessOutput
        do {
            out = try await ProcessRunner.run(executable: executable,
                                              arguments: ["stats", "--json", "--period", period.rawValue],
                                              timeout: timeout)
        } catch {
            throw StatsError.launchFailed(error.localizedDescription)
        }
        if out.timedOut { throw StatsError.timedOut(Int(timeout)) }
        if out.truncated { throw StatsError.outputTooLarge }
        guard out.status == 0 else {
            let msg = String(decoding: out.stderr.prefix(400), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            throw StatsError.failed(status: out.status, message: msg)
        }
        let snapshot = try StatsDecoder.decode(out.stdout)
        guard snapshot.period == period.rawValue else {
            throw StatsError.periodMismatch(expected: period.rawValue, got: snapshot.period)
        }
        return snapshot
    }
}
