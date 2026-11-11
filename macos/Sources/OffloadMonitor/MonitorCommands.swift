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
        alert.informativeText = "Current version: \(manager.currentVersionText). The App and bundled CLI will be verified and updated together. Restart waits for active chats and unsaved edits.\n\nOnly the App and its bundled CLI change. Any separate offload command in your PATH (for example ~/go/bin/offload) stays as it is; Settings > App shows it and lets you update it yourself."
        alert.addButton(withTitle: "Update When Idle")
        alert.addButton(withTitle: "Cancel")
        alert.buttons.last?.keyEquivalent = "\u{1b}"
        if alert.runModal() == .alertFirstButtonReturn { manager.confirmReleaseUpdate() }
    }
}

extension AppState {
    /// The only way a standalone CLI is ever replaced: the user confirms this exact file and version.
    func confirmCLISync(_ plan: CLISyncPlan) {
        let alert = NSAlert()
        alert.messageText = "Replace \(plan.path) with the App's offload \(plan.toVersion)?"
        alert.informativeText = """
            This file now reports \(plan.fromVersion ?? "no version"). The App copies its bundled offload (\(plan.bundledPath)) over it and checks the result. \
            The old file is saved first in ~/Library/Application Support/OffloadMonitor/cli-backups and is put back if the new one does not verify.

            Nothing else is changed. Commands already running keep running. Afterwards, open a new Terminal window and run: \(CLIAdvisor.verifyCommand)
            """
        alert.addButton(withTitle: "Replace")
        alert.addButton(withTitle: "Cancel")
        alert.buttons.last?.keyEquivalent = "\u{1b}"
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        Task { await updates.cliInstalls.sync(plan) }
    }
}
