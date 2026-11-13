import Foundation

/// Persisted user choices. Stored in UserDefaults under the app's bundle identifier.
struct Preferences {
    private enum Key {
        static let period = "statsPeriod"
        static let autoCheck = "autoCheckUpdates"
        static let autoInstall = "autoInstallUpdates"
        static let lastCheck = "lastUpdateCheck"
        static let dashboardShown = "dashboardShownOnce"
        static let lastWorkspace = "chatLastWorkspace"
        static let decorativeMotion = "decorativeMotion"
        static let restartOnAppChange = "restartOnInstalledAppChange"
        static let dismissedCLISuggestion = "dismissedStandaloneCLISuggestion"
    }

    var defaults: UserDefaults = .standard

    init(defaults: UserDefaults = .standard) { self.defaults = defaults }

    var period: StatsPeriod {
        get {
            switch defaults.string(forKey: Key.period) {
            case "today": return .today
            case "last30", "month": return .last30
            case "all": return .all
            case "last7", "week", nil: return .last7
            default: return .last7
            }
        }
        nonmutating set { defaults.set(newValue.rawValue, forKey: Key.period) }
    }
    /// Default on.
    var autoCheck: Bool {
        get { defaults.object(forKey: Key.autoCheck) as? Bool ?? true }
        nonmutating set { defaults.set(newValue, forKey: Key.autoCheck) }
    }
    /// True once the dashboard window has been shown automatically. Default false.
    var dashboardShown: Bool {
        get { defaults.bool(forKey: Key.dashboardShown) }
        nonmutating set { defaults.set(newValue, forKey: Key.dashboardShown) }
    }
    /// The last folder chosen for a new chat. Nil means the default General workspace.
    var lastWorkspace: String? {
        get { defaults.string(forKey: Key.lastWorkspace) }
        nonmutating set { defaults.set(newValue, forKey: Key.lastWorkspace) }
    }
    /// Decorative status motion in the menu bar and the panel header. Default on; the system Reduce Motion
    /// setting turns it off regardless.
    var decorativeMotion: Bool {
        get { defaults.object(forKey: Key.decorativeMotion) as? Bool ?? true }
        nonmutating set { defaults.set(newValue, forKey: Key.decorativeMotion) }
    }
    /// Default off.
    var autoInstall: Bool {
        get { defaults.object(forKey: Key.autoInstall) as? Bool ?? false }
        nonmutating set { defaults.set(newValue, forKey: Key.autoInstall) }
    }

    /// Fingerprint of the standalone-CLI suggestion the user dismissed. A different situation shows it again.
    var dismissedCLISuggestion: String? {
        get { defaults.string(forKey: Key.dismissedCLISuggestion) }
        nonmutating set { defaults.set(newValue, forKey: Key.dismissedCLISuggestion) }
    }

    /// Applies an App already installed on disk; does not enable automatic release downloads.
    var restartOnAppChange: Bool {
        get { defaults.object(forKey: Key.restartOnAppChange) as? Bool ?? true }
        nonmutating set { defaults.set(newValue, forKey: Key.restartOnAppChange) }
    }
}

enum LaunchPolicy {
    static let dashboardArgument = "--dashboard"

    /// The dashboard opens by itself on the first launch only (recorded in preferences), or whenever
    /// `--dashboard` is passed. Later launches stay menu-only.
    static func shouldShowDashboard(arguments: [String], prefs: Preferences) -> Bool {
        let first = !prefs.dashboardShown
        if first { prefs.dashboardShown = true }
        return first || arguments.dropFirst().contains(dashboardArgument)
    }
}
