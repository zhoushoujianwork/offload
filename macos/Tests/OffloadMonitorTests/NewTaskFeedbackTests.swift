import AppKit
import SwiftUI
import XCTest
@testable import OffloadMonitor

private func task(_ pid: Int, _ phase: String = "running", started: String = "2026-10-29T18:00:00+08:00") throws -> ActiveTask {
    var s = try StatsDecoder.decode(Data(StatsDecodeTests.full.utf8))
    var t = s.active[0]
    s.active = []
    t.pid = pid; t.phase = phase; t.startedAt = started
    return t
}

private func started(_ minute: Int) -> String { String(format: "2026-10-29T18:%02d:00+08:00", minute) }

final class ObservedTaskTrackerTests: XCTestCase {
    let t0 = Date(timeIntervalSince1970: 1_000_000)
    func at(_ s: TimeInterval) -> Date { t0.addingTimeInterval(s) }

    func testFirstSnapshotOnlyBuildsBaseline() throws {
        var tr = ObservedTaskTracker()
        XCTAssertNil(tr.observe([try task(1), try task(2, "queued"), try task(3, "lost")], at: at(0)))
        XCTAssertTrue(tr.hasBaseline)
        XCTAssertNil(tr.observe([try task(1), try task(2, "queued"), try task(3, "lost")], at: at(5)), "unchanged: nothing new")
    }

    func testEmptyFirstSnapshotStillBaselines() throws {
        var tr = ObservedTaskTracker()
        XCTAssertNil(tr.observe([], at: at(0)))
        XCTAssertEqual(tr.observe([try task(1)], at: at(5)), NewTaskEvent(newCount: 1, lostCount: 0))
    }

    func testNewIdentityAnnouncedOnceAcrossPhaseChanges() throws {
        var tr = ObservedTaskTracker()
        _ = tr.observe([], at: at(0))
        XCTAssertEqual(tr.observe([try task(1, "queued")], at: at(5))?.newCount, 1)
        for (i, phase) in ["running", "salvaging", "cleaning"].enumerated() {
            XCTAssertNil(tr.observe([try task(1, phase)], at: at(10 + TimeInterval(i) * 5)), phase)
        }
    }

    func testDisappearAndReappearIsNotNewAgain() throws {
        var tr = ObservedTaskTracker()
        _ = tr.observe([try task(1)], at: at(0))
        XCTAssertNil(tr.observe([], at: at(5)))
        XCTAssertNil(tr.observe([try task(1)], at: at(10)))
    }

    func testReplacementAtSameCountIsDetected() throws {
        var tr = ObservedTaskTracker()
        _ = tr.observe([try task(1), try task(2)], at: at(0))
        let e = tr.observe([try task(1), try task(3)], at: at(5))
        XCTAssertEqual(e, NewTaskEvent(newCount: 1, lostCount: 0), "same total, B replaced A")
    }

    func testPidReuseWithDifferentStartIsNew() throws {
        var tr = ObservedTaskTracker()
        _ = tr.observe([try task(7, started: started(1))], at: at(0))
        XCTAssertEqual(tr.observe([try task(7, started: started(2))], at: at(5))?.newCount, 1)
    }

    func testSameSessionDifferentInvocationsAreDistinct() throws {
        var a = try task(10, started: started(1)); a.session = "resume-me"
        var b = try task(11, started: started(2)); b.session = "resume-me"
        var tr = ObservedTaskTracker()
        _ = tr.observe([a], at: at(0))
        XCTAssertEqual(tr.observe([a, b], at: at(5))?.newCount, 1, "a resumed session is not an identity")
    }

    func testSeveralNewIdentitiesMergeIntoOneEvent() throws {
        var tr = ObservedTaskTracker()
        _ = tr.observe([try task(1)], at: at(0))
        XCTAssertEqual(tr.observe([try task(1), try task(2), try task(3, "queued"), try task(4, "cleaning")], at: at(5))?.newCount, 3)
    }

    func testFirstSeenLostIsAdvisoryNotNew() throws {
        var tr = ObservedTaskTracker()
        _ = tr.observe([], at: at(0))
        let e = tr.observe([try task(5, "lost"), try task(6)], at: at(5))
        XCTAssertEqual(e, NewTaskEvent(newCount: 1, lostCount: 1))
        XCTAssertNil(tr.observe([try task(5, "lost"), try task(6)], at: at(10)))
    }

    func testRunningTaskThatLaterBecomesLostIsNotReannounced() throws {
        var tr = ObservedTaskTracker()
        _ = tr.observe([], at: at(0))
        XCTAssertEqual(tr.observe([try task(5)], at: at(5))?.newCount, 1)
        XCTAssertNil(tr.observe([try task(5, "lost")], at: at(10)))
    }

    func testReorderDoesNotEmit() throws {
        var tr = ObservedTaskTracker()
        let a = try task(1), b = try task(2), c = try task(3)
        _ = tr.observe([a, b, c], at: at(0))
        XCTAssertNil(tr.observe([c, a, b], at: at(5)))
    }

    func testLongGapRebaselinesInsteadOfLateAnnouncing() throws {
        var tr = ObservedTaskTracker()
        _ = tr.observe([try task(1)], at: at(0))
        XCTAssertNil(tr.observe([try task(1), try task(2)], at: at(ObservedTaskTracker.maxGap + 1)), "sleep/wake gap is absorbed")
        XCTAssertEqual(tr.observe([try task(1), try task(2), try task(3)], at: at(ObservedTaskTracker.maxGap + 6))?.newCount, 1)
    }

    func testRetentionIsBoundedAndEvictedIdentitiesDoNotReplay() throws {
        var tr = ObservedTaskTracker(capacity: 4)
        _ = tr.observe([], at: at(0))
        for i in 1...10 {
            XCTAssertEqual(tr.observe([try task(i, started: started(i))], at: at(TimeInterval(i) * 5))?.newCount, 1)
        }
        XCTAssertLessThanOrEqual(tr.seenCount, 4)
        // Task 1 was evicted long ago. If it shows up again it is older than the watermark: no replay.
        XCTAssertNil(tr.observe([try task(1, started: started(1)), try task(10, started: started(10))], at: at(60)))
        // A genuinely newer identity is still announced.
        XCTAssertEqual(tr.observe([try task(11, started: started(11))], at: at(65))?.newCount, 1)
    }

    func testCurrentSnapshotIdentitiesAreNeverEvicted() throws {
        var tr = ObservedTaskTracker(capacity: 2)
        _ = tr.observe([], at: at(0))
        let many = try (1...5).map { try task($0, started: started($0)) }
        XCTAssertEqual(tr.observe(many, at: at(5))?.newCount, 5)
        XCTAssertNil(tr.observe(many, at: at(10)), "all still active: nothing replays")
    }
}

final class NewTaskPulsePolicyTests: XCTestCase {
    let t0 = Date(timeIntervalSince1970: 2_000_000)

    func testDurationIsWithinTarget() {
        XCTAssertGreaterThanOrEqual(NewTaskPulsePolicy.duration, 0.6)
        XCTAssertLessThanOrEqual(NewTaskPulsePolicy.duration, 0.9)
    }

    func testBurstMergesIntoOnePulseWithFixedWidthText() {
        var p = NewTaskPulsePolicy.merge(nil, newCount: 1, nextID: 1, at: t0)
        XCTAssertEqual(p.text, "+1")
        for i in 1...12 {
            p = NewTaskPulsePolicy.merge(p, newCount: 1, nextID: 99, at: t0.addingTimeInterval(Double(i) * 0.1))
        }
        XCTAssertEqual(p.id, 1, "merged into the visible pulse, no second animation")
        XCTAssertEqual(p.count, 13)
        XCTAssertEqual(p.text, "+9+", "fixed-width text")
        XCTAssertLessThanOrEqual(p.expiresAt.timeIntervalSince(t0), NewTaskPulsePolicy.maxTotal + 0.0001)
    }

    func testMergingNeverExtendsPastTheCap() {
        var p = NewTaskPulsePolicy.merge(nil, newCount: 1, nextID: 1, at: t0)
        for i in 1...17 {   // events every 0.1 s keep arriving until just before the cap
            p = NewTaskPulsePolicy.merge(p, newCount: 1, nextID: 99, at: t0.addingTimeInterval(Double(i) * 0.1))
            XCTAssertLessThanOrEqual(p.expiresAt.timeIntervalSince(t0), NewTaskPulsePolicy.maxTotal + 0.0001)
        }
        // After the cap the pulse is over; the next event starts one new pulse, not a queue.
        let next = NewTaskPulsePolicy.merge(p, newCount: 1, nextID: 2, at: t0.addingTimeInterval(NewTaskPulsePolicy.maxTotal + 0.05))
        XCTAssertEqual(next.id, 2)
        XCTAssertEqual(next.count, 1)
    }

    func testExpiredPulseStartsFresh() {
        let p = NewTaskPulsePolicy.merge(nil, newCount: 2, nextID: 1, at: t0)
        let q = NewTaskPulsePolicy.merge(p, newCount: 1, nextID: 2, at: t0.addingTimeInterval(5))
        XCTAssertEqual(q.count, 1)
        XCTAssertEqual(q.id, 2)
    }
}

@MainActor
final class MonitorPulseTests: XCTestCase {
    let t0 = Date(timeIntervalSince1970: 3_000_000)

    func snapshot(active: [ActiveTask], runs: Int = 3, ok: Int = 2) throws -> StatsSnapshot {
        var s = try StatsDecoder.decode(Data(StatsDecodeTests.full.utf8))
        s.active = active
        s.totals.runs = runs; s.totals.ok = ok
        return s
    }

    func model() -> (MonitorModel, Box) {
        let m = MonitorModel(testingSnapshot: nil)
        let box = Box()
        m.announce = { box.said.append($0) }
        return (m, box)
    }
    final class Box { var said: [String] = [] }

    func testFirstSnapshotRefreshAndSortShowNothing() throws {
        let (m, box) = model()
        m.ingest(try snapshot(active: [try task(1), try task(2)]), at: t0)
        XCTAssertNil(m.pulse)
        m.ingest(try snapshot(active: [try task(2), try task(1)]), at: t0.addingTimeInterval(5))
        XCTAssertNil(m.pulse)
        XCTAssertTrue(box.said.isEmpty)
    }

    func testNewTaskShowsPulseAndAnnouncesCountOnly() throws {
        let (m, box) = model()
        m.ingest(try snapshot(active: []), at: t0)
        m.ingest(try snapshot(active: [try task(1), try task(2)]), at: t0.addingTimeInterval(5))
        XCTAssertEqual(m.pulse?.text, "+2")
        XCTAssertEqual(box.said, ["2 newly observed tasks"])
    }

    func testTotalsTokensAndCostChangesAreNotNewTasks() throws {
        let (m, _) = model()
        m.ingest(try snapshot(active: [try task(1)]), at: t0)
        var s = try snapshot(active: [try task(1)], runs: 50, ok: 49)
        s.totals.usage?.inputTokens += 1_000_000
        s.totals.costUsd = 99
        s.recent = []
        m.ingest(s, at: t0.addingTimeInterval(5))
        XCTAssertNil(m.pulse, "a finished run raises totals; it does not make a new task")
    }

    func testPeriodSwitchKeepsBaseline() throws {
        let (m, _) = model()
        m.ingest(try snapshot(active: [try task(1)]), at: t0)
        m.period = .all
        m.period = .week
        m.ingest(try snapshot(active: [try task(1)]), at: t0.addingTimeInterval(5))
        XCTAssertNil(m.pulse)
    }

    func testErrorBetweenSnapshotsKeepsBaselineAndSeenSet() throws {
        let (m, _) = model()
        m.ingest(try snapshot(active: [try task(1)]), at: t0)
        // A failed refresh never reaches ingest; the next success must not replay task 1.
        m.ingest(try snapshot(active: [try task(1)]), at: t0.addingTimeInterval(15))
        XCTAssertNil(m.pulse)
    }

    func testBurstAcrossPollsMergesIntoOnePulse() throws {
        let (m, _) = model()
        m.ingest(try snapshot(active: []), at: t0)
        m.ingest(try snapshot(active: [try task(1)]), at: t0.addingTimeInterval(0.2))
        let first = m.pulse?.id
        m.ingest(try snapshot(active: [try task(1), try task(2)]), at: t0.addingTimeInterval(0.4))
        XCTAssertEqual(m.pulse?.id, first)
        XCTAssertEqual(m.pulse?.count, 2)
    }

    func testExpiredPulseIsClearedOnNextSnapshot() throws {
        let (m, _) = model()
        m.ingest(try snapshot(active: []), at: t0)
        m.ingest(try snapshot(active: [try task(1)]), at: t0.addingTimeInterval(5))
        XCTAssertNotNil(m.pulse)
        m.ingest(try snapshot(active: [try task(1)]), at: t0.addingTimeInterval(30))
        XCTAssertNil(m.pulse, "no backlog after a long gap")
    }

    func testPulseClearsItselfAfterDuration() async throws {
        let (m, _) = model()
        m.ingest(try snapshot(active: []), at: Date())
        m.ingest(try snapshot(active: [try task(1)]), at: Date())
        XCTAssertNotNil(m.pulse)
        await waitUntil(3) { m.pulse == nil }
        XCTAssertNil(m.pulse)
    }

    func testLostOnlyIsAdvisoryAndShowsNoPulse() throws {
        let (m, box) = model()
        m.ingest(try snapshot(active: []), at: t0)
        m.ingest(try snapshot(active: [try task(9, "lost")]), at: t0.addingTimeInterval(5))
        XCTAssertNil(m.pulse)
        XCTAssertTrue(box.said.isEmpty)
    }
}

final class NewTaskBadgeLayoutTests: XCTestCase {
    @MainActor func testSlotWidthIsStableWithAndWithoutPulse() {
        func width(_ p: NewTaskPulse?) -> CGFloat {
            NSHostingView(rootView: NewTaskPulseBadge(pulse: p)).fittingSize.width
        }
        let now = Date()
        let one = NewTaskPulse(id: 1, count: 1, firstShownAt: now, expiresAt: now.addingTimeInterval(1))
        let big = NewTaskPulse(id: 2, count: 42, firstShownAt: now, expiresAt: now.addingTimeInterval(1))
        XCTAssertEqual(width(nil), width(one), accuracy: 0.5)
        XCTAssertEqual(width(nil), width(big), accuracy: 0.5)
        XCTAssertGreaterThan(width(nil), 0)
    }
}
