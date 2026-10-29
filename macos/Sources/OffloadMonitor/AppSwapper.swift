import Foundation

/// Replaces an installed app bundle with a staged one and rolls back on failure. Only same-volume
/// renames are used, so each step is atomic and undoable. No privileges are needed or requested.
struct AppSwapper {
    var current: URL
    var staged: URL
    var backup: URL
    /// Launches the app at the given URL; throws if it cannot start.
    var relaunch: (URL) throws -> Void
    var fileManager: FileManager = .default

    enum Outcome: Equatable {
        case installed
        /// Nothing changed (or everything was restored). `restored` says whether the old app was relaunched.
        case rolledBack(reason: String)
        /// The old app could not be put back; `backup` still holds it.
        case needsManualRecovery(reason: String)
    }

    func run() -> Outcome {
        let fm = fileManager
        guard fm.fileExists(atPath: staged.path) else { return .rolledBack(reason: "the downloaded update is missing") }
        guard fm.fileExists(atPath: current.path) else { return .rolledBack(reason: "the installed app is missing") }
        if fm.fileExists(atPath: backup.path) { return .rolledBack(reason: "a backup from an earlier update is in the way") }

        do { try fm.moveItem(at: current, to: backup) } catch {
            return .rolledBack(reason: "could not move the current app aside: \(error.localizedDescription)")
        }
        do { try fm.moveItem(at: staged, to: current) } catch {
            return restoreBackup(after: "could not move the new app into place: \(error.localizedDescription)", removeNew: false)
        }
        do { try relaunch(current) } catch {
            return restoreBackup(after: "the new app did not start: \(error.localizedDescription)", removeNew: true)
        }
        try? fm.removeItem(at: backup)
        return .installed
    }

    private func restoreBackup(after reason: String, removeNew: Bool) -> Outcome {
        let fm = fileManager
        if removeNew {
            let failed = backup.deletingLastPathComponent().appendingPathComponent(".failed-\(UUID().uuidString)")
            if (try? fm.moveItem(at: current, to: failed)) != nil { try? fm.removeItem(at: failed) }
            else { try? fm.removeItem(at: current) }
        }
        do { try fm.moveItem(at: backup, to: current) } catch {
            return .needsManualRecovery(reason: "\(reason); the previous app is kept at \(backup.path)")
        }
        try? relaunch(current)
        return .rolledBack(reason: reason)
    }
}

/// Result file the helper leaves for the next app launch.
struct UpdateReport: Codable, Equatable {
    var ok: Bool
    var version: String
    var message: String

    static var url: URL {
        let base = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        return base.appendingPathComponent("OffloadMonitor", isDirectory: true).appendingPathComponent("update-result.json")
    }

    func write(to url: URL = UpdateReport.url) {
        try? FileManager.default.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
        if let data = try? JSONEncoder().encode(self) { try? data.write(to: url, options: .atomic) }
    }

    static func consume(from url: URL = UpdateReport.url) -> UpdateReport? {
        guard let data = try? Data(contentsOf: url) else { return nil }
        try? FileManager.default.removeItem(at: url)
        return try? JSONDecoder().decode(UpdateReport.self, from: data)
    }
}

/// Runs inside the detached helper process (a copy of this executable started with `--apply-update`).
enum UpdateHelper {
    static let flag = "--apply-update"

    /// Arguments: pid current staged backup version
    static func runIfRequested(arguments: [String]) {
        guard arguments.count == 7, arguments[1] == flag, let pid = Int32(arguments[2]) else { return }
        let current = URL(fileURLWithPath: arguments[3])
        let staged = URL(fileURLWithPath: arguments[4])
        let backup = URL(fileURLWithPath: arguments[5])
        let version = arguments[6]

        let exited = waitForExit(pid: pid, timeout: 60)
        guard exited else {
            UpdateReport(ok: false, version: version, message: "The app did not quit in time, so the update was not installed. The current version is unchanged.").write()
            cleanup(staged: staged)
            exit(1)
        }
        let swapper = AppSwapper(current: current, staged: staged, backup: backup, relaunch: open)
        let report: UpdateReport
        switch swapper.run() {
        case .installed:
            report = UpdateReport(ok: true, version: version, message: "Updated to \(version).")
        case .rolledBack(let reason):
            report = UpdateReport(ok: false, version: version, message: "Update to \(version) failed and the previous version was kept: \(reason).")
        case .needsManualRecovery(let reason):
            report = UpdateReport(ok: false, version: version, message: "Update to \(version) failed: \(reason).")
        }
        report.write()
        cleanup(staged: staged)
        exit(report.ok ? 0 : 1)
    }

    private static func cleanup(staged: URL) {
        // The staged bundle lives in a private staging directory next to the app.
        let dir = staged.deletingLastPathComponent()
        if dir.lastPathComponent.hasPrefix(".OffloadMonitor-update-") { try? FileManager.default.removeItem(at: dir) }
    }

    static func waitForExit(pid: Int32, timeout: TimeInterval) -> Bool {
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if kill(pid, 0) != 0 && errno == ESRCH { return true }
            Thread.sleep(forTimeInterval: 0.2)
        }
        return false
    }

    static func open(_ app: URL) throws {
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/open")
        p.arguments = ["-n", app.path]
        p.standardOutput = FileHandle.nullDevice
        p.standardError = FileHandle.nullDevice
        try p.run()
        p.waitUntilExit()
        guard p.terminationStatus == 0 else {
            throw NSError(domain: "OffloadMonitor", code: Int(p.terminationStatus),
                          userInfo: [NSLocalizedDescriptionKey: "open exited with status \(p.terminationStatus)"])
        }
    }
}
