import AppKit
import SwiftUI

enum ChatLayout {
    static let width: CGFloat = 1000
    static let height: CGFloat = 720
    static let sidebar: CGFloat = 300
}

/// The New Chat / history browser window: a searchable history sidebar and the conversation on the right.
struct ChatWindowView: View {
    @ObservedObject var store: ChatStore
    @ObservedObject var history: HistoryModel
    @ObservedObject var profiles: ProfilesModel

    var body: some View {
        HStack(spacing: 0) {
            HistorySidebar(store: store, history: history)
                .frame(width: ChatLayout.sidebar)
            Divider()
            Group {
                if let chat = store.selected {
                    ChatDetailView(chat: chat, profiles: profiles, monitor: store.env.monitor)
                        .id(chat.localID)
                } else {
                    VStack(spacing: 10) {
                        BrandMark(size: 34).opacity(0.5)
                        Text("Choose a conversation or start a new chat").foregroundStyle(.secondary)
                        Button("New chat") { store.newChat() }.buttonStyle(.borderedProminent)
                    }
                    .frame(maxWidth: .infinity, maxHeight: .infinity)
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        .frame(minWidth: 760, minHeight: 520)
        .task { if history.items.isEmpty { await history.reload() } }
    }
}

// MARK: - Sidebar

struct HistorySidebar: View {
    @ObservedObject var store: ChatStore
    @ObservedObject var history: HistoryModel

    var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 6) {
                Image(systemName: "magnifyingglass").foregroundStyle(.secondary)
                TextField("Search all history", text: $history.query).textFieldStyle(.plain)
                if !history.query.isEmpty {
                    Button { history.query = "" } label: { Image(systemName: "xmark.circle.fill") }.buttonStyle(.borderless).foregroundStyle(.secondary)
                }
            }
            .padding(8)
            .background(Color.primary.opacity(0.06), in: RoundedRectangle(cornerRadius: 8))
            .padding([.horizontal, .top], 10)

            HStack {
                Text(summaryLine).font(.system(size: 10)).foregroundStyle(.secondary)
                Spacer()
                if history.loading { ProgressView().controlSize(.mini) }
                Button { store.newChat() } label: { Label("New chat", systemImage: "plus.bubble") }
                    .controlSize(.small)
            }
            .padding(.horizontal, 12).padding(.vertical, 6)
            Divider()

            let locals = store.localChats(historyIDs: Set(history.items.map(\.id)))
            if !locals.isEmpty {
                VStack(alignment: .leading, spacing: 4) {
                    Text("On this Mac, not in history yet").font(.system(size: 10, weight: .semibold)).foregroundStyle(.secondary)
                    ForEach(locals) { m in LocalChatRow(chat: m, selected: store.selected === m) { store.select(m) } }
                }
                .padding(.horizontal, 10).padding(.vertical, 8)
                Divider()
            }

            if let e = history.error, history.items.isEmpty {
                Text(e).font(.system(size: 11)).foregroundStyle(.orange).padding(12)
                Spacer()
            } else if history.items.isEmpty && !history.loading {
                Text(history.query.isEmpty ? "No conversations yet." : "No conversations match “\(history.query)”.")
                    .font(.system(size: 11)).foregroundStyle(.secondary).padding(12)
                Spacer()
            } else {
                ScrollView(showsIndicators: false) {
                    LazyVStack(spacing: 4) {
                        ForEach(history.items) { c in
                            SidebarRow(item: c, selected: store.selected?.conversationID == c.id) { store.open(c) }
                        }
                        if history.hasMore {
                            Button(history.loading ? "Loading…" : "Load more (\(history.items.count) of \(history.total))") {
                                Task { await history.loadMore() }
                            }
                            .disabled(history.loading).controlSize(.small).padding(8)
                        }
                    }
                    .padding(8)
                }
            }
            if let e = history.error, !history.items.isEmpty {
                Text(e).font(.system(size: 10)).foregroundStyle(.orange).padding(8)
            }
        }
        .background(.regularMaterial)
    }

    private var summaryLine: String {
        history.query.isEmpty ? "\(history.total) conversation\(history.total == 1 ? "" : "s")" : "\(history.total) match\(history.total == 1 ? "" : "es")"
    }
}

/// A chat that exists only in this app so far. It observes its model, so the title and status stay current.
struct LocalChatRow: View {
    @ObservedObject var chat: ChatModel
    let selected: Bool
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            HStack(spacing: 6) {
                if chat.isBusy { ProgressView().controlSize(.mini) } else { Image(systemName: "square.and.pencil").font(.system(size: 10)).foregroundStyle(.secondary) }
                Text(chat.sidebarTitle).font(.system(size: 12, weight: .medium)).lineLimit(1)
                Spacer(minLength: 4)
                Text(chat.statusText).font(.system(size: 10)).foregroundStyle(chat.isBusy ? Color.omTeal : .secondary)
            }
            .padding(8)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(selected ? Color.omBlue.opacity(0.18) : Color.primary.opacity(0.04), in: RoundedRectangle(cornerRadius: 8))
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .help("\(chat.sidebarTitle) · \(chat.statusText)")
    }
}

struct SidebarRow: View {
    let item: ConversationSummary
    let selected: Bool
    let action: () -> Void

    var body: some View {
        Button(action: action) {
            VStack(alignment: .leading, spacing: 3) {
                HStack(spacing: 5) {
                    Circle().fill(statusColor).frame(width: 7, height: 7)
                    Text(item.title.isEmpty ? "Untitled" : item.title).font(.system(size: 12, weight: .medium)).lineLimit(2)
                    Spacer(minLength: 0)
                    if item.active { Text("running").font(.system(size: 9, weight: .semibold)).foregroundStyle(Color.omTeal) }
                }
                HStack(spacing: 6) {
                    Text(item.projectName).lineLimit(1)
                    Text("·")
                    Text("\(item.runCount) run\(item.runCount == 1 ? "" : "s")")
                    Spacer(minLength: 0)
                    Text(HistoryDates.short(item.updatedAt))
                }
                .font(.system(size: 10)).foregroundStyle(.secondary)
            }
            .padding(8)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(selected ? Color.omBlue.opacity(0.18) : Color.primary.opacity(0.04), in: RoundedRectangle(cornerRadius: 8))
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
    }

    private var statusColor: Color {
        switch item.status {
        case "ok": return .green
        case "timeout": return .orange
        case "": return .secondary
        default: return .red
        }
    }
}

// MARK: - Conversation

struct ChatDetailView: View {
    @ObservedObject var chat: ChatModel
    @ObservedObject var profiles: ProfilesModel
    @ObservedObject var monitor: MonitorModel

    var body: some View {
        VStack(spacing: 0) {
            header
            Divider()
            transcript
            Divider()
            composer
        }
    }

    // MARK: header

    private var header: some View {
        VStack(alignment: .leading, spacing: 5) {
            HStack(spacing: 8) {
                Text(chat.title).font(.system(size: 14, weight: .semibold)).lineLimit(1).truncationMode(.tail).help(chat.title)
                Spacer()
                if !chat.isNew, chat.session != nil {
                    Button { chat.requestFocus() } label: { Label("Continue conversation", systemImage: "arrowshape.turn.up.left") }
                        .controlSize(.small)
                }
            }
            if chat.isNew {
                badgeRow {
                    badge("person.crop.circle", chat.profileName.isEmpty ? "no profile" : chat.profileName)
                    badge("folder", chat.workspace.map { ($0 as NSString).lastPathComponent } ?? "General workspace", help: chat.effectiveDir)
                }
            } else if let s = chat.summary {
                // Three short rows: who, where and when, accounting. Each badge truncates on its own and shows its full value on hover.
                badgeRow {
                    badge("person.crop.circle", s.profile.isEmpty ? "unknown profile" : s.profile)
                    if !s.model.isEmpty { badge("cpu", s.model) }
                }
                badgeRow {
                    badge("folder", s.projectName, help: s.dir)
                    badge("calendar", HistoryDates.short(s.startedAt), help: s.startedAt)
                    badge("number", "\(s.runCount) run\(s.runCount == 1 ? "" : "s")")
                }
                badgeRow {
                    if let u = s.usage {
                        badge("text.word.spacing", "\(Format.count(u.inputTokens + u.outputTokens)) tokens · \(s.usageRuns)/\(s.runCount)",
                              help: "\(u.inputTokens) in, \(u.outputTokens) out; recorded for \(s.usageRuns) of \(s.runCount) runs")
                    } else { badge("text.word.spacing", "tokens not recorded") }
                    if let c = s.costUsd {
                        badge("dollarsign.circle", "est. \(Format.cost(c)) · \(s.costRuns)/\(s.runCount)",
                              help: "Estimated \(String(format: "$%.4f", c)); recorded for \(s.costRuns) of \(s.runCount) runs")
                    } else { badge("dollarsign.circle", "cost not recorded") }
                }
            }
        }
        .padding(.horizontal, 14).padding(.vertical, 10)
    }

    private func badgeRow<Content: View>(@ViewBuilder _ content: () -> Content) -> some View {
        HStack(spacing: 6) { content(); Spacer(minLength: 0) }
    }

    private func badge(_ symbol: String, _ text: String, help: String? = nil) -> some View {
        HStack(spacing: 3) {
            Image(systemName: symbol).font(.system(size: 9))
            Text(text).lineLimit(1).truncationMode(.middle)
        }
        .font(.system(size: 10)).foregroundStyle(.secondary)
        .padding(.horizontal, 6).padding(.vertical, 2)
        .background(Color.primary.opacity(0.06), in: Capsule())
        .frame(maxWidth: 260, alignment: .leading)
        .help(help ?? text)
    }

    // MARK: transcript

    private var transcript: some View {
        ScrollViewReader { proxy in
            ScrollView(showsIndicators: false) {
                VStack(alignment: .leading, spacing: 14) {
                    if let e = chat.loadError { Label(e, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.orange).font(.system(size: 12)) }
                    if chat.phase == .loading && chat.entries.isEmpty { ProgressView().frame(maxWidth: .infinity) }
                    if chat.isNew && chat.entries.isEmpty { newChatHint }
                    ForEach(chat.entries) { MessageBubble(entry: $0).id($0.id) }
                    if chat.isBusy { ProgressRow(chat: chat, monitor: monitor) }
                    Color.clear.frame(height: 1).id("end")
                }
                .padding(16)
                .frame(maxWidth: 820, alignment: .leading)
                .frame(maxWidth: .infinity)
            }
            .onChange(of: chat.entries.count) { _ in withAnimation { proxy.scrollTo("end") } }
            .onChange(of: chat.phase) { _ in withAnimation { proxy.scrollTo("end") } }
        }
    }

    private var newChatHint: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text("New chat").font(.system(size: 16, weight: .semibold))
            Text("Your message runs through the offload profile “\(chat.profileName.isEmpty ? "none selected" : chat.profileName)” in \(chat.workspace ?? "the General workspace"). The reply appears here when the run finishes.")
                .font(.system(size: 12)).foregroundStyle(.secondary)
        }
        .padding(.vertical, 30)
    }

    // MARK: composer

    private var composer: some View {
        VStack(alignment: .leading, spacing: 6) {
            if let n = chat.notice {
                Label(n, systemImage: chat.notPersisted ? "exclamationmark.triangle.fill" : "info.circle.fill")
                    .font(.system(size: 11)).foregroundStyle(chat.notPersisted ? .orange : .secondary)
                    .lineLimit(3).fixedSize(horizontal: false, vertical: true)
            }
            if let p = chat.missingProfile {
                Label("The profile “\(p)” was removed. Restore it in Settings > Profiles to continue this conversation.", systemImage: "person.crop.circle.badge.exclamationmark")
                    .font(.system(size: 11)).foregroundStyle(.orange)
                Button("Open Settings") { AppState.shared.showSettings() }.controlSize(.small)
            }
            if chat.needsFolderChoice {
                Label("The original folder \(chat.originalDir) no longer exists.", systemImage: "folder.badge.questionmark")
                    .font(.system(size: 11)).foregroundStyle(.orange)
            }
            HStack(spacing: 8) {
                if chat.isNew {
                    Picker("Profile", selection: Binding(get: { chat.profileName }, set: { chat.chooseProfile($0) })) {
                        if chat.profileName.isEmpty || chat.profileMissing { Text(chat.profileName.isEmpty ? "No profile" : "\(chat.profileName) (missing)").tag(chat.profileName) }
                        ForEach(chat.profileChoices) { p in
                            Text(p.model.isEmpty ? p.name : "\(p.name) — \(p.model)").lineLimit(1).truncationMode(.middle).tag(p.name)
                        }
                    }
                    .controlSize(.small).frame(maxWidth: 200).disabled(chat.isBusy || chat.profileChoices.isEmpty)
                    .help("The profile this new conversation will use. It is fixed once the first message is sent.")
                }
                Button { chooseFolder() } label: { Label(folderLabel, systemImage: "folder").lineLimit(1) }
                    .controlSize(.small).disabled(chat.isBusy)
                    .help(chat.effectiveDir + "\nChoose the working folder")
                Toggle("Allow file changes", isOn: $chat.allowWrites).controlSize(.small).disabled(chat.isBusy)
                    .help("Tells the worker it may modify files. This is an instruction, not a sandbox.")
                Spacer()
                if let reason = chat.blockReason, !chat.draft.isEmpty { Text(reason).font(.system(size: 10)).foregroundStyle(.secondary).lineLimit(1) }
            }
            HStack(alignment: .bottom, spacing: 8) {
                ComposerTextView(text: $chat.draft, focusRequest: chat.focusRequest,
                                 canSend: { chat.canSend }, onSend: { chat.send() })
                    .frame(height: 74)
                    .padding(6)
                    .background(Color.primary.opacity(0.06), in: RoundedRectangle(cornerRadius: 8))
                VStack(spacing: 6) {
                    if chat.isBusy {
                        Button { chat.stop() } label: { Label(chat.phase == .stopping ? "Stopping…" : "Stop", systemImage: "stop.fill") }
                            .disabled(chat.phase != .sending)
                    } else {
                        Button { chat.send() } label: { Label("Send", systemImage: "arrow.up.circle.fill") }
                            .buttonStyle(.borderedProminent)
                            .keyboardShortcut(.return, modifiers: .command)
                            .disabled(!chat.canSend)
                            .help(chat.blockReason ?? "Send (Enter or ⌘↩). Shift+Enter adds a new line.")
                            .accessibilityHint("Press Enter or Command+Enter to send. Shift+Enter adds a new line.")
                    }
                    Text("Enter to send · Shift+Enter for new line").font(.system(size: 9)).foregroundStyle(.secondary)
                        .multilineTextAlignment(.center).frame(maxWidth: 90)
                }
            }
        }
        .padding(12)
    }

    private var folderLabel: String {
        let dir = chat.effectiveDir
        return chat.isNew && chat.workspace == nil ? "General workspace" : (dir as NSString).lastPathComponent
    }

    private func chooseFolder() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.prompt = "Choose"
        guard panel.runModal() == .OK, let url = panel.url else { return }
        if chat.isNew {
            chat.workspace = url.path
            chat.env.prefs.lastWorkspace = url.path
        } else {
            chat.dirOverride = url.path
        }
    }
}

struct ProgressRow: View {
    @ObservedObject var chat: ChatModel
    @ObservedObject var monitor: MonitorModel

    var body: some View {
        HStack(spacing: 8) {
            ProgressView().controlSize(.small)
            TimelineView(.periodic(from: .now, by: 1)) { ctx in
                Text(text(at: ctx.date)).font(.system(size: 11)).foregroundStyle(.secondary)
            }
            Spacer()
        }
        .padding(10)
        .background(Color.omTeal.opacity(0.1), in: RoundedRectangle(cornerRadius: 10))
    }

    private func text(at now: Date) -> String {
        if chat.phase == .stopping { return "Stopping… waiting for the worker to finish its current step." }
        if chat.phase == .finalizing { return "Finished. Saving to history…" }
        var parts = ["Working"]
        if let start = chat.sendStarted { parts[0] += " · \(Format.duration(now.timeIntervalSince(start)))" }
        if let l = chat.live {
            parts.append("\(l.toolCalls) tool call\(l.toolCalls == 1 ? "" : "s")")
            if let t = l.lastTool, !t.isEmpty { parts.append(String(t.prefix(60))) }
        }
        return parts.joined(separator: " · ")
    }
}

// MARK: - Messages

struct MessageBubble: View {
    let entry: ChatEntry

    var body: some View {
        switch entry.role {
        case .user: UserBubble(text: entry.text, local: entry.local)
        case .assistant: AssistantBubble(entry: entry)
        }
    }
}

struct UserBubble: View {
    let text: String
    let local: Bool
    @State private var expanded = false
    private static let collapsedLines = 4
    /// Wrapped text (CJK especially) can pass four lines without a newline, so the link appears early. Showing it for a
    /// message that was not cut is harmless; hiding it for one that was cut would lose text.
    static let longThreshold = 80

    var body: some View {
        HStack {
            Spacer(minLength: 60)
            VStack(alignment: .trailing, spacing: 4) {
                Text(text).font(.system(size: 13)).lineSpacing(3).textSelection(.enabled)
                    .lineLimit(expanded ? nil : Self.collapsedLines)
                    .frame(maxWidth: .infinity, alignment: .leading)
                if Self.isLong(text) {
                    Button(expanded ? "Show less" : "Show full message") { withAnimation(.easeInOut(duration: 0.15)) { expanded.toggle() } }
                        .buttonStyle(.link).font(.system(size: 11))
                }
            }
            .padding(12)
            .background(Color.omBlue.opacity(0.16), in: RoundedRectangle(cornerRadius: 14))
        }
    }

    /// More than four lines, or long enough to wrap past four at this width.
    static func isLong(_ s: String) -> Bool {
        s.split(separator: "\n", omittingEmptySubsequences: false).count > collapsedLines || s.count > longThreshold
    }
}

struct AssistantBubble: View {
    let entry: ChatEntry

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if entry.text.isEmpty {
                Text("No result text recorded.").font(.system(size: 12)).foregroundStyle(.secondary)
            } else {
                MarkdownView(entry.text)
            }
            if let m = entry.meta { MetaLine(meta: m, local: entry.local) }
        }
        .padding(12)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 14))
        .overlay(RoundedRectangle(cornerRadius: 14).strokeBorder(Color.primary.opacity(0.08)))
    }
}

struct MetaLine: View {
    let meta: ChatEntry.Meta
    let local: Bool

    var body: some View {
        HStack(spacing: 6) {
            Image(systemName: icon.0).foregroundStyle(icon.1)
            Text(parts.joined(separator: " · ")).lineLimit(2)
        }
        .font(.system(size: 10)).foregroundStyle(.secondary)
    }

    private var parts: [String] {
        var p = [label]
        if meta.salvaged { p.append("salvaged report") }
        if !meta.model.isEmpty { p.append(meta.model) }
        if let ms = meta.elapsedMs { p.append(Format.duration(Double(ms) / 1000)) }
        if let u = meta.usage { p.append("\(Format.count(u.inputTokens)) in / \(Format.count(u.outputTokens)) out") } else { p.append("tokens not recorded") }
        p.append(meta.costUsd.map { "est. \(Format.cost($0))" } ?? "cost not recorded")
        if !meta.ts.isEmpty { p.append(HistoryDates.short(meta.ts)) }
        if local { p.append("not yet in history") }
        return p
    }

    private var label: String {
        switch meta.status {
        case "ok": return "Done"
        case "stopped": return "Stopped"
        case "timeout": return "Timed out"
        default: return "Failed"
        }
    }

    private var icon: (String, Color) {
        switch meta.status {
        case "ok": return ("checkmark.circle.fill", .green)
        case "stopped": return ("stop.circle.fill", .secondary)
        case "timeout": return ("clock.badge.exclamationmark.fill", .orange)
        default: return ("xmark.circle.fill", .red)
        }
    }
}
