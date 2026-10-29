import AppKit
import Combine
import Foundation

/// Render state of one surface's status symbol. It owns at most one pending timer, and only while a bounded
/// phase or working motion is active. It never talks to the CLI and never changes how often the CLI runs.
///
/// Why stepped state changes instead of self-repeating SwiftUI animations: the menu-bar label is hosted by the system, and whether
/// those run there is not guaranteed. Every step here is an ordinary state change
/// that re-renders the label, so it works wherever the label re-renders; if the host does not animate the
/// change, the symbol simply steps between two opacities, which is still the same message.
@MainActor
final class MotionModel: ObservableObject {
    @Published private(set) var phase: MotionPhase = .still
    /// Alternates on each step of a repeating or bounded phase. Always true when the phase is `.still`.
    @Published private(set) var beat = true

    let surface: MotionSurface
    /// The context used for the last decision. Views read it to decide whether digits may animate.
    var context: MotionContext { source().1 }
    var style: MotionStyle { MotionPolicy.style(for: surface) }
    /// Symbol opacity for the current step.
    var symbolOpacity: Double { MotionPolicy.opacity(phase: phase, beat: beat, style: style) }

    private var driver = MotionDriver()
    private let scheduler: MotionScheduling
    private let now: () -> Date
    private let source: () -> (OffloadStatus, MotionContext)
    /// Surfaces inside a window (the panel) only run while a view is on screen; the menu-bar label is always on.
    private let tracksVisibility: Bool
    private var visibleViews = 0
    private var paused = false
    private var cancelPending: (() -> Void)?
    private var scheduledGeneration = 0
    /// How many timers were scheduled in total. Tests use it to prove a static state costs nothing.
    private(set) var scheduledCount = 0

    init(surface: MotionSurface, tracksVisibility: Bool, scheduler: MotionScheduling = TaskMotionScheduler(),
         now: @escaping () -> Date = Date.init, source: @escaping () -> (OffloadStatus, MotionContext)) {
        self.surface = surface
        self.tracksVisibility = tracksVisibility
        self.scheduler = scheduler
        self.now = now
        self.source = source
    }

    var hasPendingTimer: Bool { cancelPending != nil }
    private var runnable: Bool { !paused && (!tracksVisibility || visibleViews > 0) }

    /// Reads the current status and context and applies them. Called after every poll result, and whenever the
    /// context or visibility changes.
    func refresh() {
        guard runnable else { stop(); return }
        let (status, context) = source()
        driver.observe(status, context: context, now: now())
        apply()
    }

    func viewAppeared() {
        visibleViews += 1
        if visibleViews == 1 { driver.resume(); refresh() }
    }

    func viewDisappeared() {
        visibleViews = max(0, visibleViews - 1)
        if visibleViews == 0, tracksVisibility { stop() }
    }

    /// Sleep, display sleep. Cancels the timer; nothing that happens meanwhile is replayed on `resume()`.
    func pause() {
        paused = true
        stop()
    }

    func resume() {
        guard paused else { return }
        paused = false
        driver.resume()
        refresh()
    }

    private func stop() {
        cancelPending?(); cancelPending = nil
        driver.suspend()
        publish()
    }

    private func apply() {
        publish()
        schedule()
    }

    private func publish() {
        if phase != driver.phase {
            phase = driver.phase
            beat = phase != .attention // the attention pulse opens with a dip, then returns to full
        }
        if phase == .still, !beat { beat = true }
    }

    /// The next step, shortened so it never runs past the phase's own deadline.
    private func nextStep() -> TimeInterval? {
        guard let step = MotionPolicy.tickInterval(for: driver.phase, style: style) else { return nil }
        guard let d = driver.deadline else { return step }
        return min(step, max(0.01, d.timeIntervalSince(now())))
    }

    private func schedule() {
        guard let step = nextStep() else {
            cancelPending?(); cancelPending = nil
            return
        }
        // A pending timer of the same generation keeps running. A phase change cancels the old one.
        if cancelPending != nil, scheduledGeneration == driver.generation { return }
        cancelPending?()
        scheduledGeneration = driver.generation
        let generation = driver.generation
        scheduledCount += 1
        cancelPending = scheduler.schedule(after: step) { [weak self] in self?.fire(generation: generation) }
    }

    private func fire(generation: Int) {
        cancelPending = nil
        guard generation == driver.generation, runnable else { return }
        driver.expire(now: now())
        if driver.phase != phase { publish(); return } // ended by itself: back to the static symbol
        beat.toggle()
        let next = driver.generation
        guard let step = nextStep() else { return }
        scheduledGeneration = next
        scheduledCount += 1
        cancelPending = scheduler.schedule(after: step) { [weak self] in self?.fire(generation: next) }
    }
}

/// Owns the user-visible switch, the system Reduce Motion setting and the sleep/wake pause for all surfaces.
@MainActor
final class MotionController: ObservableObject {
    @Published var decorativeEnabled: Bool {
        didSet { prefs.decorativeMotion = decorativeEnabled; refreshAll() }
    }
    @Published private(set) var systemReduceMotion: Bool

    let menuBar: MotionModel
    let header: MotionModel

    private let prefs: Preferences
    private let monitor: MonitorModel
    private var bag = Set<AnyCancellable>()

    var context: MotionContext { MotionContext(systemReduceMotion: systemReduceMotion, decorativeEnabled: decorativeEnabled) }

    init(monitor: MonitorModel, prefs: Preferences = Preferences(), scheduler: MotionScheduling = TaskMotionScheduler(),
         systemReduceMotion: Bool = NSWorkspace.shared.accessibilityDisplayShouldReduceMotion, observeSystem: Bool = true) {
        self.monitor = monitor
        self.prefs = prefs
        self.decorativeEnabled = prefs.decorativeMotion
        self.systemReduceMotion = systemReduceMotion
        var ctx: () -> (OffloadStatus, MotionContext) = { (OffloadStatus(kind: .unknown, freshness: .unknown, counts: nil, dataAge: nil), .enabled) }
        menuBar = MotionModel(surface: .menuBar, tracksVisibility: false, scheduler: scheduler) { ctx() }
        header = MotionModel(surface: .panelHeader, tracksVisibility: true, scheduler: scheduler) { ctx() }
        ctx = { [weak self] in
            guard let self else { return (OffloadStatus(kind: .unknown, freshness: .unknown, counts: nil, dataAge: nil), .enabled) }
            return (self.monitor.status(at: Date()), self.context)
        }
        // Each poll result (success or failure) publishes once; that is the only trigger from data. The polling
        // rate itself is untouched.
        monitor.objectWillChange
            .sink { [weak self] _ in Task { @MainActor in self?.refreshAll() } }
            .store(in: &bag)
        if observeSystem { observeSystemEvents() }
        refreshAll()
    }

    func refreshAll() {
        menuBar.refresh()
        header.refresh()
    }

    func setSystemReduceMotion(_ on: Bool) {
        guard on != systemReduceMotion else { return }
        systemReduceMotion = on
        refreshAll()
    }

    func systemWillSleep() { menuBar.pause(); header.pause() }
    func systemDidWake() { menuBar.resume(); header.resume() }

    private func observeSystemEvents() {
        let ws = NSWorkspace.shared.notificationCenter
        for name in [NSWorkspace.willSleepNotification, NSWorkspace.screensDidSleepNotification] {
            ws.publisher(for: name).sink { [weak self] _ in Task { @MainActor in self?.systemWillSleep() } }.store(in: &bag)
        }
        for name in [NSWorkspace.didWakeNotification, NSWorkspace.screensDidWakeNotification] {
            ws.publisher(for: name).sink { [weak self] _ in Task { @MainActor in self?.systemDidWake() } }.store(in: &bag)
        }
        ws.publisher(for: NSWorkspace.accessibilityDisplayOptionsDidChangeNotification)
            .sink { [weak self] _ in
                Task { @MainActor in self?.setSystemReduceMotion(NSWorkspace.shared.accessibilityDisplayShouldReduceMotion) }
            }
            .store(in: &bag)
    }
}
