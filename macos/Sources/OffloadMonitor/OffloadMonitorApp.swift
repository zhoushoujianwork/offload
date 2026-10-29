import SwiftUI

struct OffloadMonitorApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    private let state = AppState.shared

    var body: some Scene {
        MenuBarExtra {
            PopoverView(model: state.model, updates: state.updates, profiles: state.profiles, motion: state.motion)
        } label: {
            MenuBarLabel(model: state.model, updates: state.updates, motion: state.motion.menuBar)
        }
        .menuBarExtraStyle(.window)
    }
}

/// Menu-bar identity: brand mark, a state symbol and a count. Both trailing slots have a fixed width, so a
/// change of state or count (0, 9, 10, 99, 99+) never moves neighbouring menu-bar items. The update hint is a
/// small badge on the mark instead of another item in the row.
struct MenuBarLabel: View {
    @ObservedObject var model: MonitorModel
    @ObservedObject var updates: UpdateManager
    /// Drives the opacity of the state symbol. It reads the same status; it never changes what is shown.
    @ObservedObject var motion: MotionModel

    /// Slot sizes in points. The count slot fits "99+" in the menu-bar font with monospaced digits.
    enum Metrics {
        static let symbolSlot: CGFloat = 13
        static let countSlot: CGFloat = 28
        static let spacing: CGFloat = 3
        static var totalWidth: CGFloat { 16 + spacing + symbolSlot + spacing + countSlot }
    }

    private var updateAvailable: Bool {
        if case .available = updates.phase { return true }
        return false
    }

    var body: some View {
        // The model republishes on every poll, success or failure, so this re-evaluates with a fresh `now`.
        let status = model.status(at: Date())
        HStack(spacing: Metrics.spacing) {
            Image(nsImage: BrandMarkImage.menuBar)
                .overlay(alignment: .bottomTrailing) {
                    if updateAvailable { Circle().frame(width: 5, height: 5).offset(x: 1, y: 1) }
                }
            Group {
                if let symbol = status.symbol { StatusSymbolView(name: symbol, motion: motion) } else { Color.clear }
            }
            .frame(width: Metrics.symbolSlot)
            Text(status.menuBarCountText)
                .monospacedDigit()
                .lineLimit(1)
                .fixedSize()
                .frame(width: Metrics.countSlot, alignment: .leading)
                .countTransition(animates: MotionPolicy.animatesCount(status, context: motion.context), value: status.counts?.working ?? 0)
        }
        .frame(width: Metrics.totalWidth)
        .help(MenuBarLabel.helpText(status, updateAvailable: updateAvailable))
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("Offload")
        .accessibilityValue(MenuBarLabel.helpText(status, updateAvailable: updateAvailable))
    }

    static func helpText(_ status: OffloadStatus, updateAvailable: Bool) -> String {
        let base = "Offload: " + status.accessibilityValue
        return updateAvailable ? base + " An update is available." : base
    }
}
