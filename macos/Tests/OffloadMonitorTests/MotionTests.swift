import AppKit
import SwiftUI
import XCTest
@testable import OffloadMonitor

/// Synthetic statuses only: no CLI, no model, no real time.
private func status(_ kind: OffloadStatus.Kind, freshness: OffloadStatus.Freshness = .fresh, age: TimeInterval = 1,
                    running: Int = 0, queued: Int = 0, stalled: Int = 0, lost: Int = 0) -> OffloadStatus {
    var c = OffloadStatus.Counts()
    c.running = running; c.queued = queued; c.stalled = stalled; c.lost = lost
    let hasCounts = kind != .unknown && kind != .unavailable
    return OffloadStatus(kind: kind, freshness: freshness, counts: hasCounts ? c : nil, dataAge: hasCounts ? age : nil)
}

private let on = MotionContext.enabled
private let t0 = Date(timeIntervalSince1970: 2_000_000)

final class MotionPolicyTests: XCTestCase {
    func testEveryStateMapsToItsPhase() {
        XCTAssertEqual(MotionPolicy.target(for: status(.unknown), context: on), .loading)
        XCTAssertEqual(MotionPolicy.target(for: status(.idle), context: on), .still)
        XCTAssertEqual(MotionPolicy.target(for: status(.queued, queued: 2), context: on), .still)
        XCTAssertEqual(MotionPolicy.target(for: status(.working, running: 1), context: on), .working)
        XCTAssertEqual(MotionPolicy.target(for: status(.attention, lost: 1), context: on), .attention)
        XCTAssertEqual(MotionPolicy.target(for: status(.unavailable), context: on), .still)
        XCTAssertEqual(MotionPolicy.target(for: status(.stale, freshness: .stale, age: 40), context: on), .still)
    }

    func testQueuedNeverMovesSoItIsNotMistakenForExecution() {
        for n in [1, 5, 120] {
            XCTAssertEqual(MotionPolicy.target(for: status(.queued, queued: n), context: on), .still)
        }
    }

    func testReduceMotionAndInAppSwitchForceStillForEveryState() {
        let kinds: [OffloadStatus.Kind] = [.unknown, .unavailable, .idle, .queued, .working, .attention, .stale]
        let off: [MotionContext] = [MotionContext(systemReduceMotion: true, decorativeEnabled: true),
                                    MotionContext(systemReduceMotion: false, decorativeEnabled: false),
                                    MotionContext(systemReduceMotion: true, decorativeEnabled: false)]
        for ctx in off {
            XCTAssertFalse(ctx.allowed)
            for k in kinds { XCTAssertEqual(MotionPolicy.target(for: status(k, running: 3), context: ctx), .still, "\(k)") }
            XCTAssertFalse(MotionPolicy.animatesCount(status(.working, running: 3), context: ctx))
        }
    }

    func testFailedOrOldDataNeverAsksForWorkingOrAttentionMotion() {
        XCTAssertEqual(MotionPolicy.target(for: status(.working, freshness: .refreshFailed, running: 2), context: on), .still)
        XCTAssertEqual(MotionPolicy.target(for: status(.attention, freshness: .refreshFailed, lost: 1), context: on), .still)
        XCTAssertEqual(MotionPolicy.target(for: status(.working, age: MotionPolicy.validityWindow + 1, running: 2), context: on), .still)
    }

    func testCountsAnimateOnlyWhenKnownAndFresh() {
        XCTAssertTrue(MotionPolicy.animatesCount(status(.working, running: 2), context: on))
        XCTAssertFalse(MotionPolicy.animatesCount(status(.unknown), context: on), "unknown is a dash, not a number")
        XCTAssertFalse(MotionPolicy.animatesCount(status(.stale, freshness: .stale, age: 40, running: 2), context: on))
        XCTAssertFalse(MotionPolicy.animatesCount(status(.working, freshness: .refreshFailed, running: 2), context: on))
    }

    func testSurfacesShareStatesAndDifferOnlyInStrength() {
        let menu = MotionPolicy.style(for: .menuBar), header = MotionPolicy.style(for: .panelHeader)
        XCTAssertNotEqual(menu, header)
        XCTAssertGreaterThan(menu.dimOpacity, header.dimOpacity, "the system menu bar is the quieter surface")
        // The phase comes from the status alone; neither surface can choose another phase.
        for k in [OffloadStatus.Kind.unknown, .idle, .queued, .working, .attention, .unavailable, .stale] {
            _ = MotionPolicy.target(for: status(k, running: 1), context: on) // no surface parameter exists
        }
    }

    func testOpacityIsFullWhenStillAndNeverBelowTheDimLevel() {
        for surface in [MotionSurface.menuBar, .panelHeader] {
            let st = MotionPolicy.style(for: surface)
            XCTAssertEqual(MotionPolicy.opacity(phase: .still, beat: false, style: st), 1)
            XCTAssertEqual(MotionPolicy.opacity(phase: .working, beat: true, style: st), 1)
            XCTAssertEqual(MotionPolicy.opacity(phase: .working, beat: false, style: st), st.dimOpacity)
            XCTAssertGreaterThanOrEqual(st.dimOpacity, 0.3, "symbol must stay readable at the dim step")
        }
    }

    func testStaticPhaseHasNoClock() {
        let st = MotionPolicy.style(for: .menuBar)
        XCTAssertNil(MotionPolicy.tickInterval(for: .still, style: st))
        XCTAssertNotNil(MotionPolicy.tickInterval(for: .working, style: st))
    }

    func testMotionIsLowFrequency() {
        for s in [MotionSurface.menuBar, .panelHeader] {
            XCTAssertGreaterThanOrEqual(MotionPolicy.style(for: s).halfPeriod, 1.0, "slower than one step per second")
        }
    }
}

final class MotionDriverTests: XCTestCase {
    func drive(_ d: inout MotionDriver, _ s: OffloadStatus, at t: TimeInterval, _ ctx: MotionContext = on) {
        d.observe(s, context: ctx, now: t0.addingTimeInterval(t))
    }

    func testWorkingStartsAndStopsWhenWorkEnds() {
        var d = MotionDriver()
        drive(&d, status(.working, running: 2), at: 0)
        XCTAssertEqual(d.phase, .working)
        drive(&d, status(.idle), at: 5)
        XCTAssertEqual(d.phase, .still)
        XCTAssertNil(d.deadline)
    }

    func testTaskDisappearingPlaysNothingNotEvenAFakeSuccess() {
        var d = MotionDriver()
        drive(&d, status(.working, running: 1), at: 0)
        let g = d.generation
        drive(&d, status(.idle), at: 5)
        XCTAssertEqual(d.phase, .still, "no terminal motion: disappearance is not evidence of success")
        XCTAssertEqual(d.generation, g + 1)
        drive(&d, status(.idle), at: 10)
        XCTAssertEqual(d.phase, .still)
    }

    func testQuickRoundTripCancelsTheOldAnimationEachTime() {
        var d = MotionDriver()
        var seen = Set<Int>()
        for i in 0..<10 {
            drive(&d, status(.working, running: 1), at: Double(i) * 10)
            seen.insert(d.generation)
            drive(&d, status(.idle), at: Double(i) * 10 + 5)
            seen.insert(d.generation)
        }
        XCTAssertEqual(seen.count, 20, "every change starts a new generation, so an old timer can never act")
        XCTAssertEqual(d.phase, .still)
    }

    func testMixedRunningAndQueuedIsWorkingAndKeepsOneGenerationAcrossPolls() {
        var d = MotionDriver()
        drive(&d, status(.working, running: 1, queued: 4), at: 0)
        let g = d.generation
        drive(&d, status(.working, running: 2, queued: 3), at: 5)
        drive(&d, status(.working, running: 2, queued: 0), at: 10)
        XCTAssertEqual(d.phase, .working)
        XCTAssertEqual(d.generation, g, "count changes do not restart the breathing")
    }

    func testQueuedOnlyAfterWorkingStopsTheMotion() {
        var d = MotionDriver()
        drive(&d, status(.working, running: 1), at: 0)
        drive(&d, status(.queued, queued: 3), at: 5)
        XCTAssertEqual(d.phase, .still)
    }

    func testErrorHasPriorityOverWorking() {
        var d = MotionDriver()
        drive(&d, status(.working, running: 1), at: 0)
        drive(&d, status(.working, freshness: .refreshFailed, age: 6, running: 1), at: 6)
        XCTAssertEqual(d.phase, .still, "a failed refresh must not keep claiming that work is running")
        drive(&d, status(.stale, freshness: .stale, age: 30, running: 1), at: 30)
        XCTAssertEqual(d.phase, .still)
        drive(&d, status(.working, running: 1), at: 35)
        XCTAssertEqual(d.phase, .working, "fresh data resumes it")
    }

    func testWorkingEndsByItselfWhenNoNewDataArrives() {
        var d = MotionDriver()
        drive(&d, status(.working, running: 1, ), at: 0)
        let deadline = try! XCTUnwrap(d.deadline)
        XCTAssertEqual(deadline.timeIntervalSince(t0), MotionPolicy.validityWindow - 1, accuracy: 0.001)
        d.expire(now: deadline.addingTimeInterval(-0.1))
        XCTAssertEqual(d.phase, .working)
        d.expire(now: deadline)
        XCTAssertEqual(d.phase, .still)
    }

    func testRepublishingAnOldSnapshotDoesNotExtendWorking() {
        var d = MotionDriver()
        drive(&d, status(.working, age: 1, running: 1), at: 0)
        let first = try! XCTUnwrap(d.deadline)
        drive(&d, status(.working, age: 11, running: 1), at: 10) // same snapshot, ten seconds older
        XCTAssertEqual(d.deadline, first)
    }

    func testAttentionPlaysOnceThenStaysStatic() {
        var d = MotionDriver()
        drive(&d, status(.idle), at: 0)
        drive(&d, status(.attention, lost: 1), at: 5)
        XCTAssertEqual(d.phase, .attention)
        let end = try! XCTUnwrap(d.deadline)
        XCTAssertEqual(end.timeIntervalSince(t0), 5 + MotionPolicy.attentionDuration, accuracy: 0.001)
        d.expire(now: end)
        XCTAssertEqual(d.phase, .still)
        for i in 1...20 { drive(&d, status(.attention, lost: 1), at: 5 + Double(i) * 5) }
        XCTAssertEqual(d.phase, .still, "a persisting warning is not raised again on every poll")
    }

    func testAttentionIsNotReplayedWhenItFlipsBackQuickly() {
        var d = MotionDriver()
        drive(&d, status(.working, running: 2), at: 0)
        drive(&d, status(.attention, running: 2, stalled: 1), at: 5)
        XCTAssertEqual(d.phase, .attention)
        d.expire(now: t0.addingTimeInterval(7))
        drive(&d, status(.working, running: 2), at: 10)
        drive(&d, status(.attention, running: 2, stalled: 1), at: 15)
        XCTAssertEqual(d.phase, .still, "inside the cooldown")
        drive(&d, status(.working, running: 2), at: 20)
        drive(&d, status(.attention, running: 2, stalled: 1), at: 5 + MotionPolicy.attentionCooldown + 1)
        XCTAssertEqual(d.phase, .attention, "after the cooldown a new problem is announced again")
    }

    func testAttentionAlreadyPresentAtLaunchIsABaselineNotAnAlarm() {
        var d = MotionDriver()
        drive(&d, status(.attention, lost: 2), at: 0)
        XCTAssertEqual(d.phase, .still)
    }

    func testAttentionDuringTheFirstFreshObservationAfterFailedOnesIsStillBaseline() {
        var d = MotionDriver()
        drive(&d, status(.unavailable, freshness: .unknown), at: 0)
        drive(&d, status(.attention, lost: 2), at: 5)
        XCTAssertEqual(d.phase, .still)
    }

    func testLoadingIsBoundedAndDoesNotRestartOnEveryPoll() {
        var d = MotionDriver()
        drive(&d, status(.unknown, freshness: .unknown), at: 0)
        XCTAssertEqual(d.phase, .loading)
        let end = try! XCTUnwrap(d.deadline)
        XCTAssertEqual(end.timeIntervalSince(t0), MotionPolicy.loadingDuration, accuracy: 0.001)
        d.expire(now: end)
        XCTAssertEqual(d.phase, .still)
        drive(&d, status(.unknown, freshness: .unknown), at: 10)
        XCTAssertEqual(d.phase, .still)
    }

    func testDataArrivalAndFailureStopLoadingWithoutShowingWorking() {
        var a = MotionDriver()
        drive(&a, status(.unknown, freshness: .unknown), at: 0)
        drive(&a, status(.idle), at: 1)
        XCTAssertEqual(a.phase, .still)
        var b = MotionDriver()
        drive(&b, status(.unknown, freshness: .unknown), at: 0)
        drive(&b, status(.unavailable, freshness: .unknown), at: 1)
        XCTAssertEqual(b.phase, .still)
    }

    func testTurningMotionOffStopsAtOnceAndOnRestartsFromFreshData() {
        var d = MotionDriver()
        drive(&d, status(.working, running: 1), at: 0)
        drive(&d, status(.working, running: 1), at: 5, MotionContext(systemReduceMotion: true, decorativeEnabled: true))
        XCTAssertEqual(d.phase, .still)
        drive(&d, status(.working, running: 1), at: 10)
        XCTAssertEqual(d.phase, .working)
    }

    func testSuspendCancelsAndResumeDoesNotReplayWhatHappenedMeanwhile() {
        var d = MotionDriver()
        drive(&d, status(.idle), at: 0)
        d.suspend()
        XCTAssertEqual(d.phase, .still)
        drive(&d, status(.attention, lost: 1), at: 100) // arrives while suspended: ignored
        XCTAssertEqual(d.phase, .still)
        d.resume()
        drive(&d, status(.attention, lost: 1), at: 200)
        XCTAssertEqual(d.phase, .still, "the first observation after wake is a baseline")
        drive(&d, status(.working, running: 1), at: 205)
        XCTAssertEqual(d.phase, .working, "continuous work still shows after wake")
    }
}

/// Fires only when a test says so, and records what was scheduled and cancelled.
@MainActor
final class ManualScheduler: MotionScheduling {
    struct Item { var id: Int; var due: TimeInterval; var fire: @MainActor () -> Void; var cancelled = false }
    private(set) var items: [Item] = []
    private(set) var cancelCount = 0
    var clock: TimeInterval = 0
    var live: Int { items.filter { !$0.cancelled }.count }
    var scheduledTotal: Int { items.count }

    func schedule(after seconds: TimeInterval, _ fire: @escaping @MainActor () -> Void) -> () -> Void {
        let id = items.count
        items.append(Item(id: id, due: clock + seconds, fire: fire))
        return { [weak self] in
            guard let self, !self.items[id].cancelled else { return }
            self.items[id].cancelled = true
            self.cancelCount += 1
        }
    }

    /// Advances the manual clock and runs every uncancelled item that comes due, in order.
    func advance(_ seconds: TimeInterval) {
        let end = clock + seconds
        while let next = items.filter({ !$0.cancelled && $0.due <= end }).min(by: { $0.due < $1.due }) {
            clock = next.due
            items[next.id].cancelled = true
            next.fire()
        }
        clock = end
    }
}

@MainActor
final class MotionModelTests: XCTestCase {
    var current = status(.idle)
    var context = MotionContext.enabled
    var sched = ManualScheduler()
    var model: MotionModel!

    override func setUp() {
        current = status(.idle); context = .enabled; sched = ManualScheduler()
        let s = sched
        model = MotionModel(surface: .menuBar, tracksVisibility: false, scheduler: s,
                            now: { t0.addingTimeInterval(s.clock) }, source: { [unowned self] in (self.current, self.context) })
        // A first fresh observation is the baseline; start from a known idle state.
        model.refresh()
    }

    func set(_ s: OffloadStatus) { current = s; model.refresh() }

    func testIdleAndQueuedScheduleNothingEvenAfterManyPolls() {
        for i in 0..<200 { set(i % 2 == 0 ? status(.idle) : status(.queued, queued: 2)); sched.advance(5) }
        XCTAssertEqual(sched.scheduledTotal, 0, "static states cost no timer")
        XCTAssertFalse(model.hasPendingTimer)
        XCTAssertEqual(model.symbolOpacity, 1)
    }

    func testWorkingKeepsExactlyOneTimerAndBreathes() {
        set(status(.working, age: 0, running: 1))
        XCTAssertEqual(model.phase, .working)
        XCTAssertEqual(sched.live, 1)
        let half = model.style.halfPeriod
        let before = model.beat
        sched.advance(half + 0.01)
        XCTAssertNotEqual(model.beat, before)
        XCTAssertEqual(sched.live, 1, "one pending step at any time")
        sched.advance(half)
        XCTAssertEqual(model.beat, before)
    }

    func testWorkingStopsAtTheValidityWindowWithoutNewData() {
        set(status(.working, age: 0, running: 1))
        sched.advance(MotionPolicy.validityWindow + 1)
        XCTAssertEqual(model.phase, .still)
        XCTAssertEqual(model.symbolOpacity, 1)
        XCTAssertEqual(sched.live, 0, "no timer left behind once the motion ends")
    }

    func testStateChangeCancelsThePendingStep() {
        set(status(.working, age: 0, running: 1))
        let cancelsBefore = sched.cancelCount
        set(status(.idle))
        XCTAssertEqual(sched.cancelCount, cancelsBefore + 1)
        XCTAssertEqual(sched.live, 0)
        sched.advance(60)
        XCTAssertEqual(model.phase, .still)
    }

    func testPollsDuringWorkingDoNotAddTimers() {
        set(status(.working, age: 0, running: 1))
        for _ in 0..<50 { set(status(.working, age: 1, running: 2, queued: 1)) }
        XCTAssertEqual(sched.live, 1)
        XCTAssertEqual(sched.scheduledTotal, 1)
    }

    func testAttentionIsBoundedThenStatic() {
        set(status(.working, age: 0, running: 1))
        set(status(.attention, age: 0, running: 1, stalled: 1))
        XCTAssertEqual(model.phase, .attention)
        sched.advance(MotionPolicy.attentionDuration + 0.1)
        XCTAssertEqual(model.phase, .still)
        XCTAssertEqual(model.symbolOpacity, 1)
        XCTAssertEqual(sched.live, 0)
    }

    func testLoadingStopsByItself() {
        current = status(.unknown, freshness: .unknown)
        model.refresh()
        // setUp already observed `idle`, so unknown is a state entry.
        XCTAssertEqual(model.phase, .loading)
        sched.advance(MotionPolicy.loadingDuration + 0.5)
        XCTAssertEqual(model.phase, .still)
        XCTAssertEqual(sched.live, 0)
    }

    func testReduceMotionStopsAtOnceAndLeavesNoTimer() {
        set(status(.working, age: 0, running: 1))
        context = MotionContext(systemReduceMotion: true, decorativeEnabled: true)
        model.refresh()
        XCTAssertEqual(model.phase, .still)
        XCTAssertEqual(sched.live, 0)
        XCTAssertEqual(model.symbolOpacity, 1)
    }

    func testPauseCancelsAndResumeStartsNothingUntilFreshData() {
        set(status(.working, age: 0, running: 1))
        model.pause()
        XCTAssertEqual(model.phase, .still)
        XCTAssertEqual(sched.live, 0)
        sched.advance(3600) // a long sleep
        let total = sched.scheduledTotal
        current = status(.attention, age: 0, lost: 3) // happened during sleep
        model.resume()
        XCTAssertEqual(model.phase, .still, "no backlog of old attention")
        XCTAssertEqual(sched.scheduledTotal, total, "nothing scheduled to catch up")
    }

    func testPausedModelIgnoresRefreshes() {
        model.pause()
        set(status(.working, age: 0, running: 1))
        XCTAssertEqual(model.phase, .still)
        XCTAssertEqual(sched.live, 0)
    }

    func testPanelOnlyAnimatesWhileAViewIsOnScreen() {
        let s = sched
        let panel = MotionModel(surface: .panelHeader, tracksVisibility: true, scheduler: s,
                                now: { t0.addingTimeInterval(s.clock) }, source: { [unowned self] in (self.current, self.context) })
        current = status(.working, age: 0, running: 1)
        panel.refresh()
        XCTAssertEqual(panel.phase, .still, "closed panel does not animate")
        let base = s.scheduledTotal
        panel.viewAppeared()
        XCTAssertEqual(panel.phase, .working)
        XCTAssertEqual(s.scheduledTotal, base + 1)
        panel.viewDisappeared()
        XCTAssertEqual(panel.phase, .still)
        let live = s.live
        s.advance(120)
        XCTAssertEqual(s.scheduledTotal, base + 1, "nothing fires or reschedules after close")
        XCTAssertLessThanOrEqual(live, 1)
        XCTAssertFalse(panel.hasPendingTimer)
    }

    func testTwoViewsOfThePanelCloseIndependently() {
        let s = sched
        let panel = MotionModel(surface: .panelHeader, tracksVisibility: true, scheduler: s,
                                now: { t0.addingTimeInterval(s.clock) }, source: { [unowned self] in (self.current, self.context) })
        current = status(.working, age: 0, running: 1)
        panel.viewAppeared(); panel.viewAppeared()
        panel.viewDisappeared()
        XCTAssertEqual(panel.phase, .working, "the dashboard window is still open")
        panel.viewDisappeared()
        XCTAssertEqual(panel.phase, .still)
    }

    func testStaleTimerFromAnOlderGenerationDoesNothing() {
        set(status(.working, age: 0, running: 1))
        let old = sched.items[0]
        set(status(.idle))
        let before = (model.phase, model.beat, sched.scheduledTotal)
        old.fire() // a callback that raced with the cancel
        XCTAssertEqual(model.phase, before.0)
        XCTAssertEqual(model.beat, before.1)
        XCTAssertEqual(sched.scheduledTotal, before.2)
    }
}

@MainActor
final class MotionControllerTests: XCTestCase {
    func prefs() -> Preferences { Preferences(defaults: UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!) }

    func working() throws -> StatsSnapshot {
        var s = try StatsDecoder.decode(Data(StatsDecodeTests.full.utf8))
        s.active = Array(s.active.prefix(1)).map { var t = $0; t.phase = "running"; t.stalled = false; return t }
        return s
    }

    func testDefaultsToOnAndPersistsTheSwitch() {
        let p = prefs()
        XCTAssertTrue(p.decorativeMotion)
        let c = MotionController(monitor: MonitorModel(testingSnapshot: nil), prefs: p, scheduler: ManualScheduler(), systemReduceMotion: false, observeSystem: false)
        XCTAssertTrue(c.decorativeEnabled)
        c.decorativeEnabled = false
        XCTAssertFalse(p.decorativeMotion)
        XCTAssertFalse(c.context.allowed)
    }

    func testSystemReduceMotionOverridesTheSwitch() throws {
        let sched = ManualScheduler()
        let m = MonitorModel(testingSnapshot: try working())
        let c = MotionController(monitor: m, prefs: prefs(), scheduler: sched, systemReduceMotion: false, observeSystem: false)
        c.refreshAll()
        XCTAssertEqual(c.menuBar.phase, .working)
        c.setSystemReduceMotion(true)
        XCTAssertEqual(c.menuBar.phase, .still)
        XCTAssertEqual(sched.live, 0)
        c.setSystemReduceMotion(false)
        XCTAssertEqual(c.menuBar.phase, .working)
    }

    func testSleepAndWakePauseThenResumeWithoutBacklog() throws {
        let sched = ManualScheduler()
        let c = MotionController(monitor: MonitorModel(testingSnapshot: try working()), prefs: prefs(), scheduler: sched, systemReduceMotion: false, observeSystem: false)
        c.refreshAll()
        XCTAssertEqual(sched.live, 1)
        c.systemWillSleep()
        XCTAssertEqual(c.menuBar.phase, .still)
        XCTAssertEqual(sched.live, 0)
        c.systemDidWake()
        XCTAssertLessThanOrEqual(sched.live, 1)
        XCTAssertEqual(c.menuBar.phase, .working, "work that is still running shows again, once")
    }

    func testCLIErrorStopsWorkingMotion() throws {
        let sched = ManualScheduler()
        let m = MonitorModel(testingSnapshot: try working(), error: "boom")
        let c = MotionController(monitor: m, prefs: prefs(), scheduler: sched, systemReduceMotion: false, observeSystem: false)
        c.refreshAll()
        XCTAssertEqual(c.menuBar.phase, .still)
        XCTAssertEqual(sched.live, 0)
    }

    func testMotionDoesNotChangeThePollingRhythm() {
        XCTAssertEqual(MonitorModel.pollInterval, 5)
        XCTAssertEqual(MonitorModel.staleAfter, OffloadStatus.staleDefault)
    }

    /// The motion layer must have no way to start a CLI process: it neither imports nor names the client types.
    func testMotionSourcesNeverReferenceProcessesOrTheStatsClient() throws {
        let dir = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent("Sources/OffloadMonitor")
        for name in ["MotionPolicy.swift", "MotionModel.swift", "MotionViews.swift"] {
            let text = try String(contentsOf: dir.appendingPathComponent(name), encoding: .utf8)
            for banned in ["StatsClient", "OffloadCLI", "ProcessRunner", "Process(", "fetch(", "refreshNow", "Timer.scheduled", "TimelineView", "repeatForever"] {
                XCTAssertFalse(text.contains(banned), "\(name) must not use \(banned)")
            }
        }
    }
}

/// Headless layout checks: motion changes opacity only, so geometry stays identical.
@MainActor
final class MotionLayoutTests: XCTestCase {
    func testLabelWidthDoesNotDependOnMotion() throws {
        let m = MonitorModel(testingSnapshot: try StatsDecoder.decode(Data(StatsDecodeTests.full.utf8)))
        let u = UpdateManager(prefs: Preferences(defaults: UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!), consumeReport: false)
        var widths = Set<CGFloat>()
        for allowed in [true, false] {
            let p = Preferences(defaults: UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!)
            let c = MotionController(monitor: m, prefs: p, scheduler: ManualScheduler(), systemReduceMotion: !allowed, observeSystem: false)
            c.refreshAll()
            widths.insert(NSHostingView(rootView: MenuBarLabel(model: m, updates: u, motion: c.menuBar)).fittingSize.width)
        }
        XCTAssertEqual(widths.count, 1)
        XCTAssertEqual(widths.first ?? 0, MenuBarLabel.Metrics.totalWidth, accuracy: 0.5)
    }
}
