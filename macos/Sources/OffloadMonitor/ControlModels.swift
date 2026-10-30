import Foundation

/// Profile list and actions. Reads and writes are async with a timeout and never overlap; nothing here
/// writes configuration on load.
@MainActor
final class ProfilesModel: ObservableObject {
    static let staleAfter: TimeInterval = 30

    @Published private(set) var snapshot: ProfilesSnapshot?
    @Published private(set) var loadError: String?
    @Published private(set) var actionError: String?
    @Published private(set) var busy = false
    /// View-owned drafts report only their dirty bit, never credential contents.
    @Published var hasUnsavedEdits = false

    @Published private(set) var accountLoginOutput = ""
    @Published private(set) var accountLoginRunning = false
    private var accountLoginProcess: Process?
    private let cli: OffloadCLI?
    private var lastLoad: Date?
    private var poller: Task<Void, Never>?

    /// With `autoLoad`, reads once now and then every `refreshInterval`, so the header follows a default changed
    /// outside the app. Reads never overlap (a busy model skips the tick). Tests pass `autoLoad: false`.
    init(cli: OffloadCLI?, autoLoad: Bool = true, refreshInterval: TimeInterval = 15) {
        self.cli = cli
        if cli == nil { loadError = CLIError.unavailable.errorDescription }
        if autoLoad && cli != nil { startPolling(every: refreshInterval) }
    }

    deinit { poller?.cancel() }

    /// Headless layout tests: shows the given state and never runs a command.
    init(testingSnapshot: ProfilesSnapshot?, error: String? = nil) {
        cli = nil
        snapshot = testingSnapshot
        loadError = error
    }

    /// Headless tests only.
    func setSnapshotForTesting(_ s: ProfilesSnapshot?) { snapshot = s }

    var isAvailable: Bool { cli != nil }

    /// A snapshot is on screen but the latest read failed, so it may no longer match the config.
    var isStale: Bool { snapshot != nil && loadError != nil }

    private func startPolling(every interval: TimeInterval) {
        poller = Task { [weak self] in
            while !Task.isCancelled {
                guard let self else { return }
                _ = await self.reload()
                try? await Task.sleep(nanoseconds: UInt64(max(0.05, interval) * 1_000_000_000))
            }
        }
    }

    /// Stops the periodic refresh. In-flight reads are bounded by the command timeout.
    func stopPolling() { poller?.cancel(); poller = nil }

    func refresh() { Task { _ = await reload() } }

    func refreshIfStale() {
        if let t = lastLoad, Date().timeIntervalSince(t) < Self.staleAfter, loadError == nil { return }
        refresh()
    }

    @discardableResult
    func reload() async -> Bool {
        guard let cli, !busy else { return false }
        busy = true
        defer { busy = false }
        return await fetch(cli)
    }

    private func fetch(_ cli: OffloadCLI) async -> Bool {
        do {
            snapshot = try await cli.profiles()
            loadError = nil
            lastLoad = Date()
            return true
        } catch {
            loadError = message(error)
            return false
        }
    }

    /// Makes `name` the saved default for new runs, then re-reads. A failed switch changes nothing and re-reads nothing.
    @discardableResult
    func use(_ name: String) async -> Bool {
        guard let cli, !busy else { return false }
        busy = true
        defer { busy = false }
        actionError = nil
        do { try await cli.useProfile(name) } catch {
            actionError = message(error)
            return false
        }
        if !(await fetch(cli)) { actionError = "Switched, but the profile list could not be refreshed." }
        return true
    }

    /// Creates or patches a profile. The reply is the refreshed redacted list.
    @discardableResult
    func save(_ request: ProfileSaveRequest) async -> Bool {
        guard let cli, !busy else { return false }
        busy = true
        defer { busy = false }
        actionError = nil
        do {
            snapshot = try await cli.saveProfile(request)
            loadError = nil
            lastLoad = Date()
            return true
        } catch {
            actionError = message(error)
            return false
        }
    }

    func cancelAccountLogin() { accountLoginProcess?.terminate() }

    func addAccount(name: String) {
        guard let cli, !busy, !accountLoginRunning else { return }
        let process = Process()
        let pipe = Pipe()
        process.executableURL = cli.executable
        process.arguments = ["accounts", "add", name]
        process.environment = ChatCommand.environment()
        process.standardInput = FileHandle.nullDevice
        process.standardOutput = pipe
        process.standardError = pipe
        accountLoginOutput = "Starting Codex device sign-in…"
        accountLoginRunning = true
        accountLoginProcess = process
        pipe.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let data = handle.availableData
            guard !data.isEmpty else { handle.readabilityHandler = nil; return }
            let text = String(decoding: data, as: UTF8.self)
            Task { @MainActor [weak self] in
                guard let self else { return }
                self.accountLoginOutput = String((self.accountLoginOutput + "\n" + text).suffix(8000))
            }
        }
        process.terminationHandler = { [weak self] finished in
            pipe.fileHandleForReading.readabilityHandler = nil
            Task { @MainActor [weak self] in
                guard let self else { return }
                self.accountLoginRunning = false
                self.accountLoginProcess = nil
                if finished.terminationStatus != 0 { self.actionError = "Account sign-in did not complete. Try again or import an existing sign-in." }
                _ = await self.reload()
            }
        }
        do { try process.run() } catch {
            pipe.fileHandleForReading.readabilityHandler = nil
            accountLoginRunning = false
            accountLoginProcess = nil
            actionError = "Could not start account sign-in."
        }
    }

    func importAccount(name: String, data: Data) async -> Bool {
        guard let cli, !busy else { return false }
        busy = true
        defer { busy = false }
        actionError = nil
        do {
            _ = try await cli.run(["accounts", "import", name], stdin: data)
            return await fetch(cli)
        } catch {
            actionError = message(error)
            return false
        }
    }

    func clearActionError() { actionError = nil }

    private func message(_ e: Error) -> String { (e as? LocalizedError)?.errorDescription ?? "The offload command failed." }
}

/// Agent skill status and one-click enable/disable. Operations are serialized, and state is re-read after
/// every operation, successful or not, so the display matches the files on disk.
@MainActor
final class AgentSkillsModel: ObservableObject {
    @Published private(set) var snapshot: SkillsSnapshot?
    @Published private(set) var loadError: String?
    @Published private(set) var actionError: String?
    @Published private(set) var busy = false
    @Published private(set) var busyAgent: String?

    private let cli: OffloadCLI?

    init(cli: OffloadCLI?) {
        self.cli = cli
        if cli == nil { loadError = CLIError.unavailable.errorDescription }
    }

    init(testingSnapshot: SkillsSnapshot?, error: String? = nil) {
        cli = nil
        snapshot = testingSnapshot
        loadError = error
    }

    var isAvailable: Bool { cli != nil }

    func refresh() { Task { _ = await reload() } }

    @discardableResult
    func reload() async -> Bool {
        guard let cli, !busy else { return false }
        busy = true
        defer { busy = false }
        return await fetch(cli)
    }

    private func fetch(_ cli: OffloadCLI) async -> Bool {
        do {
            snapshot = try await cli.agentStatus()
            loadError = nil
            return true
        } catch {
            loadError = message(error)
            return false
        }
    }

    @discardableResult
    func setEnabled(_ agent: String, _ enabled: Bool) async -> Bool {
        guard let cli, !busy else { return false }
        busy = true
        busyAgent = agent
        defer { busy = false; busyAgent = nil }
        actionError = nil
        var ok = true
        do { try await cli.setAgent(agent, enabled: enabled) } catch {
            ok = false
            actionError = message(error)
        }
        _ = await fetch(cli) // show the real state either way
        return ok
    }

    private func message(_ e: Error) -> String { (e as? LocalizedError)?.errorDescription ?? "The offload command failed." }
}
