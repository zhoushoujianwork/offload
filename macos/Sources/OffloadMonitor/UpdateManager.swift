import AppKit
import Combine
import Foundation

@MainActor
final class UpdateManager: ObservableObject {
    static let checkInterval: TimeInterval = 6 * 3600

    enum Phase: Equatable {
        case idle
        case checking
        case upToDate(checkedAt: Date)
        case available(UpdateCandidate)
        case downloading(UpdateCandidate)
        case ready(UpdateCandidate)
        case installing(UpdateCandidate)
        case failed(String)
    }

    @Published private(set) var phase: Phase = .idle
    @Published private(set) var resultNotice: UpdateReport?
    @Published private(set) var restartWaitReason: String?
    @Published var restartOnAppChange: Bool {
        didSet { prefs.restartOnAppChange = restartOnAppChange; considerRestart() }
    }
    let localChanges: LocalInstallMonitor
    @Published var autoCheck: Bool {
        didSet { prefs.autoCheck = autoCheck; if autoCheck { scheduleChecks() } else { scheduler?.cancel(); scheduler = nil } }
    }
    @Published var autoInstall: Bool {
        didSet {
            prefs.autoInstall = autoInstall
            if !autoInstall && !manualReleaseRestart { pendingReleaseRestart = false }
            // Release installation always needs an explicit confirmation.
        }
    }

    let currentVersion: SemanticVersion
    private let prefs: Preferences
    private let service: UpdateService
    private let arch: String
    private var scheduler: Task<Void, Never>?
    private var work: Task<Void, Never>?
    private var staged: UpdateService.Staged?
    private var localSubscription: AnyCancellable?
    private var restartBlocker: () -> String? = { "Waiting for the app to finish starting." }
    private var requestTermination: () -> Void = {}
    private var lifecycleConfigured = false
    private var terminationRequested = false
    private var helperLaunched = false
    private var failedLocalDigest: String?
    private var manualLocalRestart = false
    private var manualReleaseRestart = false
    private(set) var pendingLocalRestart = false
    private(set) var pendingReleaseRestart = false
    /// Injected by tests; only the application delegate's approved quit path launches helpers.
    var launchLocalRestart: (URL, String) throws -> Void = { try LocalRelaunchHelper.launch(app: $0, expectedDigest: $1) }
    var launchReleaseRestart: (UpdateService.Staged, URL, SemanticVersion) throws -> Void = {
        try UpdateService.launchHelper(staged: $0, installedApp: $1, version: $2)
    }

    var installedApp: URL { Bundle.main.bundleURL }
    var currentVersionText: String { Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "development" }

    init(prefs: Preferences = Preferences(), service: UpdateService = UpdateService(), arch: String = Arch.current, consumeReport: Bool = true,
         localChanges: LocalInstallMonitor? = nil) {
        self.prefs = prefs
        self.service = service
        self.arch = arch
        self.autoCheck = prefs.autoCheck
        self.autoInstall = false // Legacy preference must not bypass confirmation.
        self.restartOnAppChange = prefs.restartOnAppChange
        self.localChanges = localChanges ?? LocalInstallMonitor()
        let v = Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String
        self.currentVersion = v.flatMap(SemanticVersion.init) ?? SemanticVersion("0.0.0")!
        self.resultNotice = consumeReport ? UpdateReport.consume() : nil
        localSubscription = self.localChanges.objectWillChange.sink { [weak self] _ in self?.objectWillChange.send() }
        self.localChanges.onPoll = { [weak self] in self?.considerRestart() }
        if autoCheck { scheduleChecks() }
    }

    var isBusy: Bool {
        switch phase {
        case .checking, .downloading, .installing: return true
        default: return false
        }
    }

    /// Headless layout tests only.
    func setPhaseForTesting(_ p: Phase, staged: UpdateService.Staged? = nil) { phase = p; self.staged = staged }

    func dismissNotice() { resultNotice = nil }

    func configureLifecycle(blocker: @escaping () -> String?, refreshCLI: @escaping () -> Void,
                            requestTermination: @escaping () -> Void) {
        self.restartBlocker = blocker
        self.requestTermination = requestTermination
        localChanges.onCLIChange = refreshCLI
        lifecycleConfigured = true
    }

    func startLocalObservation() { localChanges.start() }

    func restartInstalledApp() {
        guard localChanges.observation.appChanged else { return }
        failedLocalDigest = nil
        manualLocalRestart = true
        pendingLocalRestart = true
        localChanges.retryValidation()
        considerRestart()
    }

    /// Called after a stable observation or an idle-state change. A pending request never cancels work.
    func considerRestart() {
        guard lifecycleConfigured, !terminationRequested else { return }
        if !localChanges.observation.appChanged { pendingLocalRestart = false; failedLocalDigest = nil }
        let localWanted = pendingLocalRestart || (restartOnAppChange && localChanges.observation.appChanged
            && (failedLocalDigest == nil || failedLocalDigest != localChanges.validatedDigest))
        guard pendingReleaseRestart || localWanted else {
            if failedLocalDigest == nil { restartWaitReason = nil }
            return
        }
        if !pendingReleaseRestart {
            guard localChanges.readyToRestart else {
                restartWaitReason = localChanges.problem ?? "Waiting for the installed App to be stable and verified."
                return
            }
            guard !isBusy else { restartWaitReason = "Waiting for the release update to finish."; return }
        }
        if let reason = restartBlocker() { restartWaitReason = reason; return }
        restartWaitReason = nil
        if !pendingReleaseRestart { pendingLocalRestart = true }
        terminationRequested = true
        requestTermination()
    }

    /// Returns nil for an ordinary Quit. The app delegate calls this synchronously at its quit gate,
    /// after user input can no longer sneak between a safety check and the helper handoff.
    func approvePendingRestart() -> Bool? {
        guard terminationRequested, pendingReleaseRestart || pendingLocalRestart else { return nil }
        if helperLaunched { return true }
        if let reason = restartBlocker() {
            restartWaitReason = reason; terminationRequested = false
            if !manualLocalRestart { pendingLocalRestart = false }
            return false
        }
        do {
            if pendingReleaseRestart, case .ready(let candidate) = phase, let staged {
                try launchReleaseRestart(staged, installedApp, candidate.version)
                phase = .installing(candidate)
            } else if pendingLocalRestart, localChanges.readyToRestart,
                      let app = localChanges.appURL, let digest = localChanges.validatedDigest {
                try launchLocalRestart(app, digest)
            } else {
                terminationRequested = false
                return false
            }
            helperLaunched = true
            return true
        } catch {
            if pendingLocalRestart { failedLocalDigest = localChanges.validatedDigest }
            terminationRequested = false; pendingLocalRestart = false; pendingReleaseRestart = false
            restartWaitReason = "Could not restart: \(Self.message(error))"
            return false
        }
    }

    var localNotice: NoticeItem? {
        guard localChanges.observation.appChanged else { return nil }
        return NoticeItem(symbol: "arrow.triangle.2.circlepath", tint: .omBlue, title: "New App build installed",
                          message: restartWaitReason ?? localChanges.problem ?? (restartOnAppChange
                              ? "The App will restart when chats and unsaved edits are clear." : "Restart to load the installed build."), action: .reloadApp)
    }

    private func scheduleChecks() {
        scheduler?.cancel()
        scheduler = Task { [weak self] in
            while !Task.isCancelled {
                self?.checkNow(manual: false)
                try? await Task.sleep(nanoseconds: UInt64(Self.checkInterval * 1_000_000_000))
            }
        }
    }

    /// `manual` checks report errors; background checks stay quiet unless an update is found or install fails.
    func checkNow(manual: Bool = true) {
        guard !isBusy else { return }
        if case .available = phase, !manual { return }
        if case .ready = phase { return }
        phase = .checking
        work = Task { [weak self] in
            guard let self else { return }
            do {
                let release = try await self.service.fetchLatestRelease()
                if let c = try ReleasePolicy.candidate(from: release, current: self.currentVersion, arch: self.arch) {
                    self.phase = .available(c)
                    // Discovery only: the update badge leads to a confirmation.
                } else {
                    self.phase = .upToDate(checkedAt: Date())
                }
            } catch {
                self.phase = manual ? .failed(Self.message(error)) : .idle // background failures stay quiet
            }
        }
    }

    var hasReleaseUpdate: Bool {
        switch phase {
        case .available, .ready: return true
        default: return false
        }
    }

    var releaseCandidate: UpdateCandidate? {
        switch phase {
        case .available(let c), .ready(let c): return c
        default: return nil
        }
    }

    /// Called only after the user confirms the displayed release version.
    func confirmReleaseUpdate() {
        switch phase {
        case .available(let c): beginDownload(c, thenInstall: true)
        case .ready: installAndRestart()
        default: break
        }
    }

    func downloadAndPrepare() {
        guard case .available(let c) = phase else { return }
        beginDownload(c, thenInstall: false)
    }

    private func beginDownload(_ c: UpdateCandidate, thenInstall: Bool) {
        if let problem = UpdateService.installability(of: installedApp) {
            phase = .failed(problem)
            return
        }
        phase = .downloading(c)
        work = Task { [weak self] in
            guard let self else { return }
            var zip: URL?
            do {
                zip = try await self.service.download(c)
                let s = try await UpdateService.stage(zip: zip!, candidate: c, beside: self.installedApp)
                try? FileManager.default.removeItem(at: zip!)
                self.staged = s
                self.phase = .ready(c)
                if thenInstall { self.installAndRestart(manual: true) }
            } catch {
                if let zip { try? FileManager.default.removeItem(at: zip) }
                self.phase = .failed(Self.message(error))
            }
        }
    }

    /// Queues a verified whole-App update. The helper is launched only at the app delegate's idle quit gate.
    func installAndRestart(manual: Bool = true) {
        guard case .ready = phase, staged != nil else { return }
        manualReleaseRestart = manual
        pendingReleaseRestart = true
        considerRestart()
    }

    static func message(_ error: Error) -> String {
        (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
    }
}
