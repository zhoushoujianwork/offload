import Foundation

/// Persisted user choices. Stored in UserDefaults under the app's bundle identifier.
struct Preferences {
    private enum Key {
        static let period = "statsPeriod"
        static let autoCheck = "autoCheckUpdates"
        static let autoInstall = "autoInstallUpdates"
        static let lastCheck = "lastUpdateCheck"
        static let dashboardShown = "dashboardShownOnce"
    }

    var defaults: UserDefaults = .standard

    init(defaults: UserDefaults = .standard) { self.defaults = defaults }

    var period: StatsPeriod {
        get { defaults.string(forKey: Key.period).flatMap(StatsPeriod.init(rawValue:)) ?? .today }
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
    /// Default off.
    var autoInstall: Bool {
        get { defaults.object(forKey: Key.autoInstall) as? Bool ?? false }
        nonmutating set { defaults.set(newValue, forKey: Key.autoInstall) }
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
