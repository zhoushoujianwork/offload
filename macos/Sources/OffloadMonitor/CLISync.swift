import Foundation

enum CLISyncError: LocalizedError, Equatable {
    case stale(String)
    case refused(String)
    case verification(String)
    case failed(String)

    var errorDescription: String? {
        switch self {
        case .stale(let m): return "Nothing was changed: \(m) Rescan and review it again."
        case .refused(let m): return "Nothing was changed: \(m)"
        case .verification(let m): return m
        case .failed(let m): return "Nothing was changed: \(m)"
        }
    }
}

struct CLISyncResult: Equatable {
    var path: String
    var fromVersion: String?
    var toVersion: String
    var backupPath: String
}

/// Copies the App's bundled CLI over one standalone copy the user chose. Every check runs again here,
/// against the files on disk, so a plan that went stale (file edited, replaced by a link, handed to a
/// package manager) is refused instead of applied.
struct CLISyncer {
    var backupDirectory: URL = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0]
        .appendingPathComponent("OffloadMonitor/cli-backups", isDirectory: true)
    var probe: @Sendable (URL) async -> String? = CLIScanner.runVersion
    var verifySignature: @Sendable (URL) async throws -> Void = { try await ArchiveValidator.verifySignature($0) }
    var now: @Sendable () -> Date = { Date() }
    var home: String = NSHomeDirectory()
    var environment: [String: String] = ProcessInfo.processInfo.environment

    func apply(_ plan: CLISyncPlan) async throws -> CLISyncResult {
        let fm = FileManager.default
        let target = URL(fileURLWithPath: plan.path)
        let bundled = URL(fileURLWithPath: plan.bundledPath)
        try preflight(plan)
        try check(digestOf: bundled, equals: plan.bundledDigest, what: "The App's bundled CLI")
        do { try await verifySignature(bundled) } catch {
            throw CLISyncError.refused("the App's bundled CLI failed its code signature check.")
        }

        let dir = target.deletingLastPathComponent()
        let backup = try makeBackup(of: target, plan: plan)
        let staged = dir.appendingPathComponent(".offload-sync-\(UUID().uuidString)")
        defer { try? fm.removeItem(at: staged) }
        do {
            try fm.copyItem(at: bundled, to: staged)
            try fm.setAttributes([.posixPermissions: 0o755], ofItemAtPath: staged.path)
        } catch {
            throw CLISyncError.failed("the new file could not be prepared next to the old one (\(error.localizedDescription)).")
        }
        try check(digestOf: staged, equals: plan.bundledDigest, what: "The prepared copy")
        guard await probe(staged) == plan.toVersion else {
            throw CLISyncError.failed("the prepared copy did not report version \(plan.toVersion).")
        }

        try preflight(plan) // last look before the atomic swap
        guard rename(staged.path, target.path) == 0 else {
            throw CLISyncError.failed("the file could not be replaced (\(String(cString: strerror(errno)))).")
        }

        let digestOK = (try? ArchiveValidator.sha256Hex(of: target)) == plan.bundledDigest
        let installedVersion = await probe(target)
        guard digestOK, installedVersion == plan.toVersion else {
            let restored = restore(backup, to: target, dir: dir)
            throw CLISyncError.verification(restored
                ? "The new file did not verify, so the previous one was put back from the backup."
                : "The new file did not verify and the previous one could not be put back automatically. Restore it with: cp \(CLIAdvisor.shellQuote(backup.path)) \(CLIAdvisor.shellQuote(plan.path))")
        }
        return CLISyncResult(path: plan.path, fromVersion: plan.fromVersion, toVersion: plan.toVersion, backupPath: backup.path)
    }

    // MARK: Checks

    /// The target must still be the exact regular file the user reviewed, and still not owned by a package manager or a link.
    private func preflight(_ plan: CLISyncPlan) throws {
        let fm = FileManager.default
        if CLIClassifier.isSymbolicLink(plan.path) { throw CLISyncError.refused("the path is now a symbolic link, which the App never overwrites.") }
        guard let type = (try? fm.attributesOfItem(atPath: plan.path))?[.type] as? FileAttributeType, type == .typeRegular else {
            throw CLISyncError.stale("The file is gone or is no longer a regular file.")
        }
        let resolved = CLIClassifier.realPath(plan.path)
        if resolved == CLIClassifier.realPath(plan.bundledPath) { throw CLISyncError.refused("the path is the App's own bundled CLI.") }
        if CLIClassifier.packageManager(resolved: resolved) != nil {
            throw CLISyncError.refused("a package manager owns this file now. Update it with that tool.")
        }
        let owner = CLIClassifier.management(path: plan.path, resolved: resolved, isSymlink: false, bundledResolved: nil, home: home, environment: environment)
        guard owner == plan.management else { throw CLISyncError.stale("The kind of installation changed.") }
        guard fm.isWritableFile(atPath: (plan.path as NSString).deletingLastPathComponent) else {
            throw CLISyncError.refused("the folder is not writable by the App.")
        }
        try check(digestOf: URL(fileURLWithPath: plan.path), equals: plan.targetDigest, what: "The file")
    }

    private func check(digestOf url: URL, equals expected: String, what: String) throws {
        guard let actual = try? ArchiveValidator.sha256Hex(of: url) else { throw CLISyncError.failed("\(what) could not be read.") }
        guard actual == expected else { throw CLISyncError.stale("\(what) changed after it was scanned.") }
    }

    // MARK: Backup and rollback

    private func makeBackup(of target: URL, plan: CLISyncPlan) throws -> URL {
        let fm = FileManager.default
        let stamp = DateFormatter.cliBackupStamp.string(from: now())
        let version = (plan.fromVersion ?? "unknown").filter { $0.isLetter || $0.isNumber || "._-+".contains($0) }
        let name = "offload-\(version)-\(stamp)-\(UUID().uuidString.prefix(6))"
        let backup = backupDirectory.appendingPathComponent(name)
        do {
            try fm.createDirectory(at: backupDirectory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            try fm.copyItem(at: target, to: backup)
        } catch {
            throw CLISyncError.failed("a backup of the current file could not be saved (\(error.localizedDescription)).")
        }
        guard (try? ArchiveValidator.sha256Hex(of: backup)) == plan.targetDigest else {
            try? fm.removeItem(at: backup)
            throw CLISyncError.failed("the backup did not match the current file.")
        }
        return backup
    }

    private func restore(_ backup: URL, to target: URL, dir: URL) -> Bool {
        let fm = FileManager.default
        let temp = dir.appendingPathComponent(".offload-restore-\(UUID().uuidString)")
        defer { try? fm.removeItem(at: temp) }
        guard (try? fm.copyItem(at: backup, to: temp)) != nil else { return false }
        return rename(temp.path, target.path) == 0
    }
}

private extension DateFormatter {
    static let cliBackupStamp: DateFormatter = {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.dateFormat = "yyyyMMdd-HHmmss"
        return f
    }()
}

/// Holds the latest scan and the one sync in flight. Scanning only reads; syncing happens only from `sync(_:)`,
/// which the UI calls after the user confirmed a specific plan.
@MainActor
final class CLIInstallsModel: ObservableObject {
    enum SyncState: Equatable {
        case idle
        case running(String)
        case done(CLISyncResult)
        case failed(String)
    }

    @Published private(set) var report: CLIReport?
    @Published private(set) var scanning = false
    @Published private(set) var syncState: SyncState = .idle
    private let scanner: CLIScanner
    private let syncer: CLISyncer

    init(scanner: CLIScanner = CLIScanner(), syncer: CLISyncer = CLISyncer()) {
        self.scanner = scanner
        self.syncer = syncer
    }

    /// Headless tests only.
    func setReportForTesting(_ r: CLIReport?) { report = r }

    var busy: Bool {
        if scanning { return true }
        if case .running = syncState { return true }
        return false
    }

    func refresh() async {
        guard !scanning else { return }
        scanning = true
        let r = await scanner.scan()
        report = r
        scanning = false
    }

    func startRefresh() { Task { await refresh() } }

    func assessed() -> [(install: CLIInstall, assessment: CLIAssessment)] {
        guard let report else { return [] }
        return report.standalone.map { ($0, CLIAdvisor.assess($0, bundled: report.bundled)) }
    }

    var attentionCount: Int { assessed().filter { $0.assessment.tone == .attention }.count }

    /// Identifies the current suggestion (which copies, which versions) so a dismissal only covers that situation.
    var suggestionFingerprint: String? {
        let flagged = assessed().filter { $0.assessment.tone == .attention }
        guard !flagged.isEmpty, let bundled = report?.bundled else { return nil }
        return (["app=" + (bundled.version ?? "?")] + flagged.map { "\($0.install.path)=\($0.install.version ?? "?")" }).joined(separator: "|")
    }

    func sync(_ plan: CLISyncPlan) async {
        guard !busy else { return }
        syncState = .running(plan.path)
        do {
            let result = try await syncer.apply(plan)
            syncState = .done(result)
        } catch {
            syncState = .failed(UpdateManager.message(error))
        }
        await refresh()
    }

    func dismissSyncState() { if !busy { syncState = .idle } }

    /// One line for the compact views.
    var summary: String {
        guard let report else { return scanning ? "Looking for installed copies…" : "Not checked yet." }
        guard let bundled = report.bundled else { return "This build has no bundled CLI, so nothing can be compared." }
        if report.standalone.isEmpty { return "No separate offload found. Only the App's CLI (\(bundled.version ?? "unknown version"))." }
        let n = attentionCount
        return n == 0 ? "\(report.standalone.count) separate copy found; no action needed."
            : "\(n) of \(report.standalone.count) separate copies need attention."
    }
}
