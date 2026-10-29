import AppKit
import Combine
import Foundation

struct ChatEntry: Identifiable, Equatable {
    enum Role { case user, assistant }
    var id: String
    var role: Role
    var text: String
    var meta: Meta?
    /// Held only in memory (not yet in, or never written to, the run log).
    var local = false

    struct Meta: Equatable {
        var status: String
        var ts: String
        var model: String
        var profile: String
        var elapsedMs: Int64?
        var usage: TokenUsage?
        var costUsd: Double?
        var salvaged: Bool
        var error: String
    }

    static func entries(from runs: [ConversationRun]) -> [ChatEntry] {
        runs.flatMap { r -> [ChatEntry] in
            let meta = Meta(status: r.status, ts: r.ts, model: r.model, profile: r.profile, elapsedMs: r.elapsedMs,
                            usage: r.usage, costUsd: r.costUsd, salvaged: r.salvaged, error: r.error)
            let reply = r.result.isEmpty ? (r.error.isEmpty ? "" : r.error) : r.result
            return [ChatEntry(id: r.id + ":u", role: .user, text: r.task, meta: nil),
                    ChatEntry(id: r.id + ":a", role: .assistant, text: reply, meta: meta)]
        }
    }
}

/// Everything a chat needs from the rest of the app.
@MainActor
final class ChatEnv {
    let cli: OffloadCLI?
    let profiles: ProfilesModel
    let monitor: MonitorModel
    let history: HistoryModel
    var prefs: Preferences
    var generalWorkspace: URL

    init(cli: OffloadCLI?, profiles: ProfilesModel, monitor: MonitorModel, history: HistoryModel,
         prefs: Preferences = Preferences(), generalWorkspace: URL = Workspaces.general()) {
        self.cli = cli
        self.profiles = profiles
        self.monitor = monitor
        self.history = history
        self.prefs = prefs
        self.generalWorkspace = generalWorkspace
    }
}

/// One conversation (new or existing) and its single in-flight send. Owned by `ChatStore`, so closing the
/// window never stops a run.
@MainActor
final class ChatModel: ObservableObject, Identifiable {
    /// `finalizing`: the process has ended and the canonical history is being re-read. Send stays blocked until it is done.
    enum Phase: Equatable { case idle, loading, sending, stopping, finalizing }

    nonisolated let localID = UUID()
    nonisolated var id: UUID { localID }
    let env: ChatEnv

    /// Provider session id (or an opaque id for a run that never got a session). Nil for an unsent new chat.
    @Published private(set) var conversationID: String?
    @Published private(set) var summary: ConversationSummary?
    @Published private(set) var entries: [ChatEntry] = []
    @Published private(set) var phase: Phase = .idle
    @Published private(set) var notice: String?
    @Published private(set) var notPersisted = false
    @Published private(set) var live: ActiveTask?
    @Published private(set) var loadError: String?
    @Published var draft = ""
    @Published var allowWrites = false
    /// New chats: the profile new runs will use, captured from the active profile when the chat is opened.
    @Published var profileName: String
    /// New chats: a folder the user chose. Nil means the General workspace.
    @Published var workspace: String?
    /// Resumed chats whose original folder is gone: an explicit replacement.
    @Published var dirOverride: String?
    @Published var focusRequest = 0
    @Published private(set) var sendStarted: Date?

    private var control: ProcessControl?
    private var runTask: Task<Void, Never>?
    private(set) var ownPID: Int32?
    private var liveSession: String?
    private var canonicalRuns = 0
    private var cancellable: AnyCancellable?
    private var profileCancellable: AnyCancellable?
    /// The user picked a profile for this new chat, so it no longer follows the active profile.
    private var profileChosen = false
    /// Responses the CLI could not write to the run log. They stay visible after every refresh.
    private var pinned: [ChatEntry] = []
    private struct StoppedTurn: Hashable {
        var ts: String
        var task: String
    }
    private var stoppedTurns = Set<StoppedTurn>()

    init(env: ChatEnv) {
        self.env = env
        profileName = env.profiles.snapshot?.active ?? ""
        workspace = env.prefs.lastWorkspace.flatMap { Workspaces.exists($0) ? $0 : nil }
        cancellable = env.monitor.$snapshot.sink { [weak self] s in
            MainActor.assumeIsolated { self?.observe(s) }
        }
        profileCancellable = env.profiles.$snapshot.sink { [weak self] s in
            MainActor.assumeIsolated { self?.followActiveProfile(s) }
        }
    }

    /// An unsent new chat tracks the active profile until the user picks one. A chat that already has a session never changes profile.
    private func followActiveProfile(_ s: ProfilesSnapshot?) {
        guard isNew, !isBusy, !profileChosen, let s, !s.active.isEmpty else { return }
        if profileName != s.active { profileName = s.active }
    }

    /// The user's explicit choice for a new chat. Ignored for existing conversations and while a run is in flight.
    func chooseProfile(_ name: String) {
        guard isNew, !isBusy else { return }
        profileChosen = true
        profileName = name
    }

    /// Profiles a new chat can pick from.
    var profileChoices: [ProfileEntry] { env.profiles.snapshot?.profiles ?? [] }
    var profileMissing: Bool {
        guard isNew, !profileName.isEmpty, let snap = env.profiles.snapshot else { return false }
        return snap.entry(profileName) == nil
    }

    var sidebarTitle: String {
        if let t = summary?.title, !t.isEmpty { return t }
        if let first = entries.first(where: { $0.role == .user })?.text, !first.isEmpty {
            return String(first.split(whereSeparator: \.isNewline).first ?? "").prefix(80).description
        }
        let d = draft.trimmingCharacters(in: .whitespacesAndNewlines)
        return d.isEmpty ? "New chat" : String(d.split(whereSeparator: \.isNewline).first ?? "").prefix(80).description
    }

    var statusText: String {
        switch phase {
        case .sending: return "Running"
        case .stopping: return "Stopping"
        case .finalizing: return "Saving"
        case .loading: return "Loading"
        case .idle: return isNew ? (draft.isEmpty && entries.isEmpty ? "Not sent" : "Draft") : "Open"
        }
    }

    // MARK: state

    var isNew: Bool { conversationID == nil }
    var session: String? { summary?.session.isEmpty == false ? summary?.session : (conversationID.flatMap { $0.hasPrefix("run:") ? nil : $0 }) }
    var isBusy: Bool { phase == .sending || phase == .stopping || phase == .finalizing }
    var title: String { summary?.title.isEmpty == false ? summary!.title : (isNew ? "New chat" : "Conversation") }
    var originalProfile: String { summary?.profile ?? "" }
    var originalDir: String { summary?.dir ?? "" }

    /// The folder this chat will run in.
    var effectiveDir: String {
        if isNew { return workspace ?? env.generalWorkspace.path }
        return dirOverride ?? originalDir
    }

    var needsFolderChoice: Bool { !isNew && dirOverride == nil && !Workspaces.exists(originalDir) }

    /// Another offload run (not ours) is working in this conversation.
    var externalBusy: Bool {
        guard let s = session else { return false }
        if let active = env.monitor.snapshot?.active,
           active.contains(where: { $0.isRunning && $0.session == s && Int32($0.pid) != ownPID }) { return true }
        return false
    }

    var missingProfile: String? {
        guard !isNew, !originalProfile.isEmpty, let snap = env.profiles.snapshot else { return nil }
        return snap.entry(originalProfile) == nil ? originalProfile : nil
    }

    /// Why Send is unavailable, if it is.
    var blockReason: String? {
        if env.cli == nil { return CLIError.unavailable.errorDescription }
        if isBusy { return "A message is already running in this conversation." }
        if phase == .loading { return "Loading the conversation…" }
        if draft.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty { return "Type a message first." }
        if externalBusy { return "Another offload run is using this conversation. Send is available when it finishes." }
        if isNew {
            if profileName.isEmpty { return "No profile is selected. Choose one in Settings > Profiles." }
            if profileMissing { return "The profile “\(profileName)” no longer exists. Choose another." }
            return nil
        }
        if session == nil { return "This record has no session, so it cannot be continued." }
        if let p = missingProfile { return "The profile “\(p)” no longer exists. Restore it in Settings > Profiles to continue this conversation." }
        if needsFolderChoice { return "The original folder is gone. Choose a folder to continue." }
        return nil
    }

    var canSend: Bool { blockReason == nil }

    // MARK: loading

    func openExisting(_ s: ConversationSummary) {
        conversationID = s.id
        summary = s
        profileName = s.profile
        entries = []
        allowWrites = false
        Task { await loadDetail() }
    }

    func openByID(_ id: String) {
        conversationID = id
        entries = []
        allowWrites = false
        Task { await loadDetail() }
    }

    func loadDetail() async {
        guard let cli, let id = conversationID else { return }
        if phase == .idle { phase = .loading }
        defer { if phase == .loading { phase = .idle } }
        do {
            let d = try await cli.historyShow(id: id)
            applyDetail(d)
            loadError = nil
        } catch {
            loadError = (error as? LocalizedError)?.errorDescription ?? "The conversation could not be loaded."
        }
    }

    private var cli: OffloadCLI? { env.cli }

    private func applyDetail(_ d: HistoryDetail) {
        let pinnedIDs = Set(pinned.map(\.id))
        let pending = entries.filter { $0.local && !pinnedIDs.contains($0.id) }
        summary = d.conversation
        conversationID = d.conversation.id
        canonicalRuns = d.runs.count
        var canonical = ChatEntry.entries(from: d.runs)
        for (index, run) in d.runs.enumerated() where stoppedTurns.contains(StoppedTurn(ts: run.ts, task: run.task)) {
            canonical[index * 2 + 1].meta?.status = "stopped"
        }
        entries = canonical + pinned
        // The latest response that the log does not have yet stays visible.
        if !pending.isEmpty && !localCoveredBy(d.runs.count) { entries += pending }
    }

    private var expectedRunsAfterSend = 0
    private func localCoveredBy(_ count: Int) -> Bool { count >= expectedRunsAfterSend }

    // MARK: progress

    private func observe(_ snap: StatsSnapshot?) {
        guard isBusy, let snap else { return }
        let match = snap.active.first { ownPID != nil && Int32($0.pid) == ownPID }
            ?? snap.active.first { $0.isRunning && session != nil && $0.session == session }
        live = match
        if let s = match?.session, !s.isEmpty { liveSession = s }
    }

    // MARK: send / stop

    func requestFocus() { focusRequest += 1 }

    func send() {
        guard canSend else { return }
        let task = draft.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let cli else { return }

        // Target. The general workspace is created only now, at the first send.
        let dir: String
        if isNew {
            if let w = workspace {
                guard Workspaces.exists(w) else { notice = "The chosen folder no longer exists. Choose another."; return }
                dir = w
            } else {
                do { dir = try Workspaces.ensure(env.generalWorkspace).path } catch {
                    notice = "Could not create the General workspace folder."
                    return
                }
            }
        } else {
            dir = effectiveDir
            guard Workspaces.exists(dir) else { notice = "That folder does not exist. Choose a folder to continue."; return }
        }
        let target = ChatTarget(session: isNew ? nil : session, profile: isNew ? profileName : nil, dir: dir, allowWrites: allowWrites)

        notice = nil
        notPersisted = false
        live = nil
        liveSession = nil
        ownPID = nil
        draft = ""
        phase = .sending
        sendStarted = Date()
        expectedRunsAfterSend = canonicalRuns + 1
        let stamp = UUID().uuidString
        entries.append(ChatEntry(id: "local-\(stamp):u", role: .user, text: task, meta: nil, local: true))

        let ctl = ProcessControl()
        control = ctl
        let runner = ChatRunner(executable: cli.executable)
        let onStart: @Sendable (Int32) -> Void = { [weak self] pid in
            Task { @MainActor in self?.ownPID = pid }
        }
        runTask = Task { [weak self] in
            let outcome = await runner.send(task: task, target: target, control: ctl, onStart: onStart)
            await self?.finish(outcome, task: task, stamp: stamp, dir: dir)
        }
    }

    func stop() {
        guard phase == .sending, let control else { return }
        phase = .stopping
        control.cancel()
    }

    /// Completes when the in-flight run has fully ended (used before quitting).
    func waitUntilIdle(timeout: TimeInterval) async {
        let end = Date().addingTimeInterval(timeout)
        while isBusy, Date() < end { try? await Task.sleep(nanoseconds: 100_000_000) }
    }

    private func finish(_ o: ChatOutcome, task: String, stamp: String, dir: String) async {
        control = nil
        ownPID = nil
        live = nil
        sendStarted = nil
        notPersisted = o.notPersisted

        if o.kind == .couldNotRun && o.result == nil {
            // Nothing ran. Take the user's message back out so they can fix the problem and send again.
            entries.removeAll { $0.id == "local-\(stamp):u" }
            if draft.isEmpty { draft = task }
            notice = o.message
            phase = .idle
            env.monitor.refreshNow()
            return
        }
        // Stay busy until the canonical history has been re-read, so a new send cannot race this finalization.
        phase = .finalizing

        let r = o.result
        if o.kind == .stopped, let ts = r?.ts { stoppedTurns.insert(StoppedTurn(ts: ts, task: task)) }
        let sessionID = o.session ?? (o.kind == .stopped ? liveSession : nil)
        var text = r?.result ?? ""
        if text.isEmpty { text = r?.error ?? o.message ?? "" }
        if o.kind == .stopped { text = text.isEmpty ? "Stopped." : text }
        let meta = ChatEntry.Meta(status: o.kind == .stopped ? "stopped" : (r?.status ?? "error"), ts: r?.ts ?? "",
                                  model: r?.model ?? "", profile: r?.profile ?? (isNew ? profileName : originalProfile),
                                  elapsedMs: r?.elapsedMs, usage: r?.salvaged == true ? nil : r?.usage,
                                  costUsd: r?.salvaged == true ? nil : r?.costUsd,
                                  salvaged: r?.salvaged ?? false, error: r?.error ?? "")
        entries.append(ChatEntry(id: "local-\(stamp):a", role: .assistant, text: text, meta: meta, local: true))
        if o.notPersisted { pinned += entries.suffix(2) }

        switch o.kind {
        case .stopped: notice = "Stopped. " + (sessionID == nil ? "No session was created, so this cannot be continued." : "You can continue the conversation.")
        case .timedOut: notice = o.message
        case .failed: notice = o.message
        default: break
        }
        if o.notPersisted { notice = "This response was not saved to the history log. It is shown here only until you close the app." }

        if let sid = sessionID, isNew || conversationID == nil || conversationID == sid {
            conversationID = sid
            if summary == nil || summary?.session.isEmpty == true {
                summary = ConversationSummary(id: sid, session: sid, title: String(task.prefix(80)), dir: dir, profile: r?.profile ?? profileName,
                                              backend: "", model: r?.model ?? "", startedAt: r?.ts ?? "", updatedAt: r?.ts ?? "", runCount: 1,
                                              status: r?.status ?? "", active: false, usage: nil, usageRuns: 0, costUsd: nil, costRuns: 0)
            }
        }
        env.monitor.refreshNow()
        await env.history.reload()
        if conversationID != nil { await loadDetail() }
        phase = .idle
    }
}

// MARK: - Store

/// Owns every chat for the app's lifetime and tracks which one the window shows.
@MainActor
final class ChatStore: ObservableObject {
    let env: ChatEnv
    @Published var selected: ChatModel?
    @Published private(set) var all: [ChatModel] = []
    private var byConversation: [String: ChatModel] = [:]
    private var forwards: [UUID: AnyCancellable] = [:]

    init(env: ChatEnv) { self.env = env }

    private func register(_ m: ChatModel) {
        guard !all.contains(where: { $0 === m }) else { return }
        all.append(m)
        // Phase and title changes in a chat redraw the sidebar's local section.
        forwards[m.localID] = m.objectWillChange.sink { [weak self] _ in self?.objectWillChange.send() }
    }

    /// Chats the history cannot show: unsent drafts, and runs this app started that are not in the list yet.
    func localChats(historyIDs: Set<String>) -> [ChatModel] {
        all.filter { m in
            if m.conversationID == nil { return true }
            return m.isBusy && !(m.conversationID.map(historyIDs.contains) ?? false)
        }
    }

    func select(_ m: ChatModel) { selected = m; m.requestFocus() }

    /// The existing empty new chat if there is one, otherwise a fresh one.
    @discardableResult
    func newChat() -> ChatModel {
        if let m = all.first(where: { $0.isNew && !$0.isBusy && $0.entries.isEmpty }) { selected = m; m.requestFocus(); return m }
        let m = ChatModel(env: env)
        register(m)
        selected = m
        m.requestFocus()
        return m
    }

    /// Opens the model for a conversation. The same conversation always maps to the same model, so a second
    /// open never starts a second run.
    @discardableResult
    func open(_ s: ConversationSummary) -> ChatModel {
        if let m = existing(s.id, session: s.session) { selected = m; return m }
        let m = ChatModel(env: env)
        register(m)
        byConversation[s.id] = m
        if !s.session.isEmpty { byConversation[s.session] = m }
        m.openExisting(s)
        selected = m
        return m
    }

    @discardableResult
    func open(id: String) -> ChatModel {
        if let m = byConversation[id] ?? all.first(where: { $0.conversationID == id }) { selected = m; return m }
        let m = ChatModel(env: env)
        register(m)
        byConversation[id] = m
        m.openByID(id)
        selected = m
        return m
    }

    private func existing(_ id: String, session: String) -> ChatModel? {
        byConversation[id] ?? (session.isEmpty ? nil : byConversation[session])
            ?? all.first { $0.conversationID == id || (!session.isEmpty && $0.session == session) }
    }

    /// From a main-panel history row: focus its session, or seed the search when the record has no session.
    func focus(run: RecentRun) {
        if !run.session.isEmpty { open(id: run.session); return }
        selected = nil
        env.history.query = run.task.split(whereSeparator: \.isNewline).first.map(String.init) ?? run.task
    }

    var runningOwn: [ChatModel] { all.filter(\.isBusy) }
    var hasRunningOwn: Bool { !runningOwn.isEmpty }

    /// Stops only runs this app started, then waits for each to end. Runs started elsewhere are never touched.
    func stopAllOwn(timeout: TimeInterval = 45) async {
        let running = runningOwn
        for m in running { m.stop() }
        for m in running { await m.waitUntilIdle(timeout: timeout) }
    }
}
