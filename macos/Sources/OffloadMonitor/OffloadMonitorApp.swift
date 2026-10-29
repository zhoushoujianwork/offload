import SwiftUI

struct OffloadMonitorApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    private let state = AppState.shared

    var body: some Scene {
        MenuBarExtra {
            PopoverView(model: state.model, updates: state.updates, profiles: state.profiles)
        } label: {
            MenuBarLabel(model: state.model, updates: state.updates)
        }
        .menuBarExtraStyle(.window)
    }
}

struct MenuBarLabel: View {
    @ObservedObject var model: MonitorModel
    @ObservedObject var updates: UpdateManager

    var body: some View {
        // The running count is shown only when known; "–" means unknown, never zero.
        let count = model.runningCount.map(String.init) ?? "–"
        let warn = model.hasProblem || model.needsAttention
        HStack(spacing: 3) {
            Image(nsImage: BrandMarkImage.menuBar)
            if warn { Image(systemName: "exclamationmark.triangle.fill") }
            Text(count).monospacedDigit()
            if case .available = updates.phase { Image(systemName: "arrow.down.circle") }
        }
        .help(helpText)
    }

    private var helpText: String {
        if model.hasProblem { return "Offload Monitor: \(model.lastError ?? "problem")" }
        guard let s = model.snapshot else { return "Offload Monitor: loading" }
        var t = "\(s.runningCount) running"
        if s.stalledCount > 0 { t += ", \(s.stalledCount) stalled" }
        if s.lostCount > 0 { t += ", \(s.lostCount) lost" }
        return t
    }
}
