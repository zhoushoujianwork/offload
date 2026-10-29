import AppKit
import SwiftUI

/// Owns the process-wide model objects so the menu bar popover, the dashboard window and the app
/// delegate all use the same instances (and therefore one polling loop).
@MainActor
final class AppState {
    static let shared = AppState()
    let model = MonitorModel()
    let updates = UpdateManager()
    let profiles: ProfilesModel
    let skills: AgentSkillsModel
    let history: HistoryModel
    let chats: ChatStore
    private var chatWindow: NSWindow?
    private var settingsWindow: NSWindow?
    private var window: NSWindow?
    private var auxWindows: [String: NSWindow] = [:]

    private init() {
        let cli = StatsClient.locateCLI().map { OffloadCLI(executable: $0) }
        profiles = ProfilesModel(cli: cli)
        skills = AgentSkillsModel(cli: cli)
        history = HistoryModel(cli: cli)
        chats = ChatStore(env: ChatEnv(cli: cli, profiles: profiles, monitor: model, history: history))
    }

    /// Shows the dashboard window, creating it on first use. Closing it leaves the menu bar app running.
    func showDashboard() {
        if window == nil {
            let hosting = NSHostingController(rootView: PopoverView(model: model, updates: updates, profiles: profiles, isDashboard: true))
            let w = NSWindow(contentViewController: hosting)
            w.title = "Offload Monitor"
            w.styleMask = [.titled, .closable, .miniaturizable]
            w.setContentSize(NSSize(width: Layout.width, height: Layout.height))
            w.isReleasedWhenClosed = false
            w.center()
            window = w
        }
        NSApp.activate(ignoringOtherApps: true)
        window?.makeKeyAndOrderFront(nil)
    }
}

extension AppState {
    /// The chat and history browser. Closing the window keeps any run this app started alive in `chats`.
    func showChat() {
        if chatWindow == nil {
            let hosting = NSHostingController(rootView: ChatWindowView(store: chats, history: history, profiles: profiles))
            let w = NSWindow(contentViewController: hosting)
            w.title = "Offload Chat"
            w.styleMask = [.titled, .closable, .miniaturizable, .resizable]
            w.setContentSize(NSSize(width: ChatLayout.width, height: ChatLayout.height))
            w.contentMinSize = NSSize(width: 760, height: 520)
            w.isReleasedWhenClosed = false
            w.center()
            chatWindow = w
        }
        NSApp.activate(ignoringOtherApps: true)
        chatWindow?.makeKeyAndOrderFront(nil)
    }

    func newChat() {
        profiles.refreshIfStale()
        chats.newChat()
        showChat()
    }

    /// From a main-panel history row.
    func openChat(for run: RecentRun) {
        chats.focus(run: run)
        showChat()
    }

    func browseHistory() {
        showChat()
        Task { await history.reload() }
    }

    /// The Settings window is created once, so edits in progress survive closing and reopening it.
    func showSettings() {
        if settingsWindow == nil {
            let hosting = NSHostingController(rootView: SettingsView(profiles: profiles, skills: skills, updates: updates))
            let w = NSWindow(contentViewController: hosting)
            w.title = "Offload Monitor Settings"
            w.styleMask = [.titled, .closable]
            w.setContentSize(NSSize(width: SettingsLayout.width, height: SettingsLayout.height))
            w.isReleasedWhenClosed = false
            w.center()
            settingsWindow = w
        } else {
            profiles.refreshIfStale()
        }
        NSApp.activate(ignoringOtherApps: true)
        settingsWindow?.makeKeyAndOrderFront(nil)
    }

    /// Opens (or refreshes) a secondary window. The main panel never scrolls, so anything long lives here.
    func showAux<V: View>(_ id: String, title: String, size: NSSize, resizable: Bool = true, @ViewBuilder content: () -> V) {
        let hosting = NSHostingController(rootView: content())
        let w: NSWindow
        if let existing = auxWindows[id] {
            w = existing
            w.contentViewController = hosting
        } else {
            w = NSWindow(contentViewController: hosting)
            w.styleMask = resizable ? [.titled, .closable, .resizable] : [.titled, .closable]
            w.isReleasedWhenClosed = false
            auxWindows[id] = w
            w.setContentSize(size)
            w.center()
        }
        w.title = title
        NSApp.activate(ignoringOtherApps: true)
        w.makeKeyAndOrderFront(nil)
    }

    func showDetails() { showAux("details", title: "Token and cost detail", size: NSSize(width: 340, height: 300)) { DetailsView(model: self.model) } }
    func showNotes() { showAux("notes", title: "Data coverage and notes", size: NSSize(width: 380, height: 260)) { NotesView(model: self.model) } }
    func showNotices() { showAux("notices", title: "Status and updates", size: NSSize(width: 380, height: 260)) { NoticesView(model: self.model, updates: self.updates) } }
    func showReport(_ run: RecentRun) { showAux("report", title: "Run report", size: NSSize(width: 460, height: 420)) { ReportView(run: run) } }
}

final class AppDelegate: NSObject, NSApplicationDelegate {
    /// Quitting while this app has runs of its own in flight asks first. Runs started elsewhere are never touched.
    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply {
        let chats = MainActor.assumeIsolated { AppState.shared.chats }
        guard MainActor.assumeIsolated({ chats.hasRunningOwn }) else { return .terminateNow }
        let alert = NSAlert()
        alert.messageText = "A chat is still running"
        alert.informativeText = "Stopping sends the worker an interrupt and waits for it to finish. Runs started outside this app are not affected."
        alert.addButton(withTitle: "Keep Open")
        alert.addButton(withTitle: "Stop and Quit")
        guard alert.runModal() == .alertSecondButtonReturn else { return .terminateCancel }
        Task { @MainActor in
            await chats.stopAllOwn()
            NSApp.reply(toApplicationShouldTerminate: true)
        }
        return .terminateLater
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        MainActor.assumeIsolated {
            if LaunchPolicy.shouldShowDashboard(arguments: CommandLine.arguments, prefs: Preferences()) {
                AppState.shared.showDashboard()
            }
        }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }
}
