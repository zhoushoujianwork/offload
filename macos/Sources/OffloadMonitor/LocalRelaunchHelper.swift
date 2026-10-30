import Foundation

struct LocalRelaunchPlan {
    var pid: Int32
    var app: URL

    func run(wait: (Int32) -> Bool, open: (URL) throws -> Void) throws -> Bool {
        guard pid > 1, app.pathExtension == "app", wait(pid) else { return false }
        try open(app)
        return true
    }
}

/// A private, short-lived helper waits for our own process to exit, then opens the installed app.
/// No process is signalled, and a cancelled quit times out without opening a duplicate instance.
enum LocalRelaunchHelper {
    static let flag = "--relaunch-installed"
    static let prefix = ".OffloadMonitor-relaunch-"

    static func launch(app: URL, expectedDigest: String) throws {
        let executable = app.appendingPathComponent("Contents/MacOS/OffloadMonitor")
        let fingerprint = try InstalledAppFingerprint.read(app)
        guard fingerprint.digest == expectedDigest else {
            throw UpdateError.helperFailed("the installed App changed again; waiting for a stable build")
        }
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(prefix + UUID().uuidString, isDirectory: true)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
        do {
            let helper = directory.appendingPathComponent("helper")
            try FileManager.default.copyItem(at: executable, to: helper)
            guard try BinaryFingerprint.read(helper).digest == fingerprint.executable.digest,
                  try InstalledAppFingerprint.read(app).digest == expectedDigest else {
                throw UpdateError.helperFailed("the installed App changed while preparing its restart")
            }
            let process = Process()
            process.executableURL = helper
            process.arguments = [flag, String(ProcessInfo.processInfo.processIdentifier), app.path, directory.path]
            process.standardInput = FileHandle.nullDevice; process.standardOutput = FileHandle.nullDevice; process.standardError = FileHandle.nullDevice
            try process.run()
        } catch {
            try? FileManager.default.removeItem(at: directory)
            throw error
        }
    }

    static func runIfRequested(arguments: [String]) {
        guard arguments.dropFirst().first == flag else { return }
        guard arguments.count == 5, let pid = Int32(arguments[2]), pid > 1 else { exit(2) }
        let app = URL(fileURLWithPath: arguments[3])
        let directory = URL(fileURLWithPath: arguments[4]).resolvingSymlinksInPath()
        let helperParent = URL(fileURLWithPath: arguments[0]).resolvingSymlinksInPath().deletingLastPathComponent()
        let temporary = FileManager.default.temporaryDirectory.resolvingSymlinksInPath()
        guard directory == helperParent, directory.deletingLastPathComponent() == temporary,
              directory.lastPathComponent.hasPrefix(prefix),
              UUID(uuidString: String(directory.lastPathComponent.dropFirst(prefix.count))) != nil else { exit(2) }
        var opened = false
        do {
            opened = try LocalRelaunchPlan(pid: pid, app: app).run(wait: { UpdateHelper.waitForExit(pid: $0, timeout: 60) }, open: UpdateHelper.open)
        } catch {
            UpdateReport(ok: false, version: "", message: "Could not reopen the installed App: \(error.localizedDescription). Open Offload Monitor manually.").write()
        }
        try? FileManager.default.removeItem(at: directory)
        exit(opened ? 0 : 1)
    }
}
