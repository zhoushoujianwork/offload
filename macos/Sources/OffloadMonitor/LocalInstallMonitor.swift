import CryptoKit
import Foundation

enum RestartSafety {
    static func reason(running: Bool, drafts: Bool, unsavedReplies: Bool, profileEdits: Bool, settingsBusy: Bool) -> String? {
        if running { return "Waiting for this App's chats to finish. No task will be stopped." }
        if drafts { return "Waiting for unsent chat messages. Send or clear them before restarting." }
        if unsavedReplies { return "Waiting for chat responses that have not been saved to history." }
        if profileEdits { return "Waiting for unsaved profile edits. Save or discard them before restarting." }
        if settingsBusy { return "Waiting for Settings or an open dialog to finish." }
        return nil
    }
}

/// Identity of the bytes at a path, not the marketing version. Atomic replacement and same-version
/// development builds are detected, while copying identical bytes does not request a restart.
struct BinaryFingerprint: Equatable {
    var stamp: String
    var digest: String

    static func read(_ url: URL, cached: Self? = nil) throws -> Self {
        func stamp() throws -> String {
            let a = try FileManager.default.attributesOfItem(atPath: url.path)
            let modified = (a[.modificationDate] as? Date)?.timeIntervalSince1970.bitPattern ?? 0
            return "\(a[.systemFileNumber] ?? ""):\(a[.size] ?? ""):\(modified):\(a[.posixPermissions] ?? "")"
        }
        let before = try stamp()
        if let cached, cached.stamp == before { return cached }
        let file = try FileHandle(forReadingFrom: url)
        defer { try? file.close() }
        var hash = SHA256()
        while let bytes = try file.read(upToCount: 1 << 20), !bytes.isEmpty { hash.update(data: bytes) }
        guard try stamp() == before else { throw CocoaError(.fileReadUnknown) }
        return Self(stamp: before, digest: hash.finalize().map { String(format: "%02x", $0) }.joined())
    }
}

struct InstalledAppFingerprint: Equatable {
    var executable: BinaryFingerprint
    var info: BinaryFingerprint
    var digest: String { executable.digest + ":" + info.digest }

    static func read(_ app: URL, executableURL: URL? = nil, cached: Self? = nil) throws -> Self {
        let executable = executableURL ?? app.appendingPathComponent("Contents/MacOS/OffloadMonitor")
        return try Self(executable: BinaryFingerprint.read(executable, cached: cached?.executable),
                        info: BinaryFingerprint.read(app.appendingPathComponent("Contents/Info.plist"), cached: cached?.info))
    }
}

struct LocalInstallObservation {
    private(set) var launchedApp: String?
    private(set) var previousCLI: String?
    private(set) var appChanged = false
    private(set) var stable = false
    private var candidate: String?
    private var samples = 0
    private var initialized = false

    /// Missing observations invalidate stability; never treat a temporarily absent file as an update.
    mutating func ingest(app: String?, cli: String?) -> Bool {
        guard initialized else {
            initialized = true; launchedApp = app; previousCLI = cli
            return false
        }
        let cliChanged = cli != nil && previousCLI != nil && cli != previousCLI
        if let cli { previousCLI = cli }
        if launchedApp == nil, let app { launchedApp = app }
        guard let app, let launchedApp else { stable = false; samples = 0; return cliChanged }
        appChanged = app != launchedApp
        if !appChanged { candidate = nil; samples = 0; stable = false }
        else {
            samples = candidate == app ? samples + 1 : 1
            candidate = app
            stable = samples >= 2
        }
        return cliChanged
    }
}

/// Started only by the actual app delegate, never by layout/unit-test construction. Polls file metadata;
/// hashes run off the main thread only when that metadata changes. It never launches a model or changes CLI selection.
@MainActor
final class LocalInstallMonitor: ObservableObject {
    @Published private(set) var observation = LocalInstallObservation()
    @Published private(set) var cliChangedAt: Date?
    @Published private(set) var problem: String?
    @Published private(set) var validatedDigest: String?
    let appURL: URL?
    let executableURL: URL?
    let cliURL: URL?
    var onCLIChange: () -> Void = {}
    var onPoll: () -> Void = {}
    private var appFingerprint: InstalledAppFingerprint?
    private var cliFingerprint: BinaryFingerprint?
    private var checkedDigest: String?
    private var loop: Task<Void, Never>?
    private var polling = false
    private let validate: (URL) async throws -> Void

    init(appURL: URL? = Bundle.main.bundleURL.pathExtension == "app" ? Bundle.main.bundleURL : nil,
         executableURL: URL? = Bundle.main.executableURL, cliURL: URL? = StatsClient.locateCLI(),
         validate: @escaping (URL) async throws -> Void = LocalInstallMonitor.validateApp) {
        self.appURL = appURL; self.executableURL = appURL == nil ? nil : executableURL; self.cliURL = cliURL
        self.validate = validate
    }

    deinit { loop?.cancel() }

    var readyToRestart: Bool {
        observation.appChanged && observation.stable && validatedDigest != nil && validatedDigest == appFingerprint?.digest
    }

    func start(interval: TimeInterval = 15) {
        guard loop == nil else { return }
        loop = Task { [weak self] in
            while !Task.isCancelled {
                await self?.poll()
                try? await Task.sleep(nanoseconds: UInt64(max(1, interval) * 1_000_000_000))
            }
        }
    }

    func stop() { loop?.cancel(); loop = nil }
    func retryValidation() { checkedDigest = nil; Task { await poll() } }

    func poll() async {
        guard !polling else { return }
        polling = true
        defer { polling = false; onPoll() }
        let appPath = appURL, executablePath = executableURL, cliPath = cliURL, oldApp = appFingerprint, oldCLI = cliFingerprint
        let fingerprints = await Task.detached {
            (appPath.flatMap { try? InstalledAppFingerprint.read($0, executableURL: executablePath, cached: oldApp) },
             cliPath.flatMap { try? BinaryFingerprint.read($0, cached: oldCLI) })
        }.value
        appFingerprint = fingerprints.0; cliFingerprint = fingerprints.1
        let changedCLI = observation.ingest(app: fingerprints.0?.digest, cli: fingerprints.1?.digest)
        if changedCLI { cliChangedAt = Date(); onCLIChange() }
        if appURL != nil && fingerprints.0 == nil {
            validatedDigest = nil; checkedDigest = nil
            problem = "The installed App is temporarily unreadable; waiting for installation to finish."
            return
        }
        guard observation.appChanged else { validatedDigest = nil; checkedDigest = nil; problem = nil; return }
        guard observation.stable, let fingerprint = fingerprints.0, let appURL, let executableURL else {
            validatedDigest = nil
            checkedDigest = nil
            return
        }
        guard checkedDigest != fingerprint.digest else { return }
        checkedDigest = fingerprint.digest; validatedDigest = nil
        do {
            try await validate(appURL)
            let current = await Task.detached { try? InstalledAppFingerprint.read(appURL, executableURL: executableURL) }.value
            guard current?.digest == fingerprint.digest else { checkedDigest = nil; return }
            validatedDigest = fingerprint.digest; problem = nil
        } catch {
            problem = "The installed App could not be verified. The running App is kept: \(UpdateManager.message(error))"
        }
    }

    nonisolated static func validateApp(_ url: URL) async throws {
        let plist = try Data(contentsOf: url.appendingPathComponent("Contents/Info.plist"))
        let info = try PropertyListSerialization.propertyList(from: plist, format: nil) as? [String: Any]
        guard let text = info?["CFBundleShortVersionString"] as? String, let version = SemanticVersion(text) else {
            throw UpdateError.helperFailed("the installed App has no valid version")
        }
        try ArchiveValidator.validateBundle(url, expectedVersion: version)
        try await ArchiveValidator.verifySignature(url)
    }
}
