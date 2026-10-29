import AppKit
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
    @Published var autoCheck: Bool {
        didSet { prefs.autoCheck = autoCheck; if autoCheck { scheduleChecks() } else { scheduler?.cancel(); scheduler = nil } }
    }
    @Published var autoInstall: Bool {
        didSet {
            prefs.autoInstall = autoInstall
            if autoInstall, case .available(let c) = phase { beginDownload(c, thenInstall: true) }
        }
    }

    let currentVersion: SemanticVersion
    private let prefs: Preferences
    private let service: UpdateService
    private let arch: String
    private var scheduler: Task<Void, Never>?
    private var work: Task<Void, Never>?
    private var staged: UpdateService.Staged?

    var installedApp: URL { Bundle.main.bundleURL }
    var currentVersionText: String { Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "development" }

    init(prefs: Preferences = Preferences(), service: UpdateService = UpdateService(), arch: String = Arch.current, consumeReport: Bool = true) {
        self.prefs = prefs
        self.service = service
        self.arch = arch
        self.autoCheck = prefs.autoCheck
        self.autoInstall = prefs.autoInstall
        let v = Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String
        self.currentVersion = v.flatMap(SemanticVersion.init) ?? SemanticVersion("0.0.0")!
        self.resultNotice = consumeReport ? UpdateReport.consume() : nil
        if autoCheck { scheduleChecks() }
    }

    var isBusy: Bool {
        switch phase {
        case .checking, .downloading, .installing: return true
        default: return false
        }
    }

    /// Headless layout tests only.
    func setPhaseForTesting(_ p: Phase) { phase = p }

    func dismissNotice() { resultNotice = nil }

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
                    if self.autoInstall { self.beginDownload(c, thenInstall: true) }
                } else {
                    self.phase = .upToDate(checkedAt: Date())
                }
            } catch {
                self.phase = manual ? .failed(Self.message(error)) : .idle // background failures stay quiet
            }
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
                if thenInstall { self.installAndRestart() }
            } catch {
                if let zip { try? FileManager.default.removeItem(at: zip) }
                self.phase = .failed(Self.message(error))
            }
        }
    }

    /// Hands the staged bundle to the helper and quits. The helper keeps the old app if anything goes wrong.
    func installAndRestart() {
        guard case .ready(let c) = phase, let staged else { return }
        phase = .installing(c)
        do {
            try UpdateService.launchHelper(staged: staged, installedApp: installedApp, version: c.version)
            NSApp.terminate(nil)
        } catch {
            try? FileManager.default.removeItem(at: staged.directory)
            self.staged = nil
            phase = .failed(Self.message(error))
        }
    }

    static func message(_ error: Error) -> String {
        (error as? LocalizedError)?.errorDescription ?? error.localizedDescription
    }
}
