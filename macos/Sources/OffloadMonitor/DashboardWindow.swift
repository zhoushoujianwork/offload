import AppKit
import SwiftUI

/// Owns the process-wide model objects so the menu bar popover, the dashboard window and the app
/// delegate all use the same instances (and therefore one polling loop).
@MainActor
final class AppState {
    static let shared = AppState()
    let model = MonitorModel()
    let updates = UpdateManager()
    private var window: NSWindow?
    private var auxWindows: [String: NSWindow] = [:]

    /// Shows the dashboard window, creating it on first use. Closing it leaves the menu bar app running.
    func showDashboard() {
        if window == nil {
            let hosting = NSHostingController(rootView: PopoverView(model: model, updates: updates, isDashboard: true))
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
    func applicationDidFinishLaunching(_ notification: Notification) {
        MainActor.assumeIsolated {
            if LaunchPolicy.shouldShowDashboard(arguments: CommandLine.arguments, prefs: Preferences()) {
                AppState.shared.showDashboard()
            }
        }
    }

    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }
}
