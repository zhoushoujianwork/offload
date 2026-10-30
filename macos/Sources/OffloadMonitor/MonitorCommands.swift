import SwiftUI

/// Standard shortcuts are available while an Offload window or panel is active, never globally.
struct MonitorCommands: Commands {
    var body: some Commands {
        CommandGroup(replacing: .newItem) {
            Button("New Chat") { AppState.shared.newChat() }.keyboardShortcut("n")
            Button("Close Window") { NSApp.keyWindow?.performClose(nil) }.keyboardShortcut("w")
        }
        CommandGroup(replacing: .appSettings) {
            Button("Settings…") { AppState.shared.showSettings() }.keyboardShortcut(",")
        }
        CommandGroup(after: .appInfo) {
            Button("Dashboard") { AppState.shared.showDashboard() }.keyboardShortcut("d", modifiers: [.command, .shift])
            Button("Conversation History") { AppState.shared.browseHistory() }.keyboardShortcut("h", modifiers: [.command, .shift])
            Button("Refresh Data") { AppState.shared.model.refreshNow() }.keyboardShortcut("r")
            Button("Check for Updates…") { AppState.shared.reviewUpdate() }.keyboardShortcut("u", modifiers: [.command, .shift])
        }
    }
}

extension AppState {
    func reviewUpdate() {
        if updates.hasReleaseUpdate {
            confirmUpdate(updates)
        } else {
            updates.checkNow(manual: true)
            showNotices()
        }
    }

    func confirmUpdate(_ manager: UpdateManager) {
        guard let candidate = manager.releaseCandidate else { return }
        let alert = NSAlert()
        alert.messageText = "Update Offload Monitor to \(candidate.version)?"
        alert.informativeText = "Current version: \(manager.currentVersionText). The App and bundled CLI will be verified and updated together. Restart waits for active chats and unsaved edits."
        alert.addButton(withTitle: "Update When Idle")
        alert.addButton(withTitle: "Cancel")
        alert.buttons.last?.keyEquivalent = "\u{1b}"
        if alert.runModal() == .alertFirstButtonReturn { manager.confirmReleaseUpdate() }
    }
}
