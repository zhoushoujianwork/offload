import AppKit
import SwiftUI
import XCTest
@testable import OffloadMonitor

final class WorkloadDecodeTests: XCTestCase {
    static func doc(workload: String = "null", daily: String = "null", goals: String = "null") -> Data {
        var s = StatsDecodeTests.empty
        var extra = ""
        if workload != "null" { extra += ",\"workload\":\(workload)" }
        if goals != "null" { extra += ",\"goals\":\(goals)" }
        if daily != "null" { extra += ",\"daily\":\(daily)" }
        s.removeLast()
        return Data((s + extra + "}").utf8)
    }

    static let day = #"{"date":"2026-10-28","recorded_calls":10000,"covered":true,"peak_processing":100,"peak_at":"2026-10-28T10:00:00Z","peak_coverage":"full","peak_incomplete_runs":0}"#

    static func daily(todayCalls: String = "12000", peak: String = "100", peakCov: String = "full", avg: String = "10000.0", cov: String = "complete", covered: Int = 7, met: Bool = true) -> String {
        """
        {"timezone":"Local (CST)","label":"recorded calls","today":{"date":"2026-10-29","recorded_calls":\(todayCalls),"covered":true,"peak_processing":\(peak),"peak_coverage":"\(peakCov)","peak_incomplete_runs":0},
         "days":[\(day)],"average":{"window_start":"2026-10-22","window_end":"2026-10-28","days":7,"covered_days":\(covered),"recorded_calls_per_day":\(avg),"coverage":"\(cov)","goal_met":\(met)}}
        """
    }

    static func workload(processing: String = "100", queued: String = "3", status: String = "ok", stalled: String = "0", limit: String = "8") -> String {
        """
        {"scope":"this computer","state_dir":"/s","status":"\(status)","processing":\(processing),"salvaging":2,"stalled":\(stalled),"queued":\(queued),"cleaning":1,"unresolved":0,"lost":0,"unreadable_states":0,"admission_limit":\(limit),"admission_status":"limited"}
        """
    }

    func testOlderCLIJSONDecodesAndRendersNotReported() throws {
        let s = try StatsDecoder.decode(Data(StatsDecodeTests.full.utf8))
        XCTAssertNil(s.workload)
        XCTAssertNil(s.daily)
        XCTAssertEqual(s.goals, .default)
        XCTAssertEqual(s.processingCount, s.runningCount, "older CLIs fall back to running/salvaging entries")
        XCTAssertEqual(WorkloadPresentation.processingValue(nil), "—")
        XCTAssertEqual(WorkloadPresentation.callsValue(nil), "—")
        XCTAssertEqual(WorkloadPresentation.peakValue(nil), "—")
        XCTAssertEqual(WorkloadPresentation.averageValue(nil), "—")
        XCTAssertEqual(WorkloadPresentation.queueLine(nil), "Queue: not reported")
        XCTAssertTrue(WorkloadPresentation.processingNote(nil).contains("Not reported"))
    }

    func testDecodesWorkloadAndDaily() throws {
        let s = try StatsDecoder.decode(Self.doc(workload: Self.workload(), daily: Self.daily(), goals: #"{"processing":100,"daily_calls":10000,"note":"n"}"#))
        XCTAssertEqual(s.processingCount, 100)
        XCTAssertEqual(s.workload?.queued, 3)
        XCTAssertEqual(s.workload?.salvaging, 2)
        XCTAssertEqual(s.workload?.admissionLimit, 8)
        XCTAssertEqual(s.goals.dailyCalls, 10_000)
        XCTAssertEqual(s.daily?.today.recordedCalls, 12_000)
        XCTAssertEqual(s.daily?.average.coverage, "complete")
        XCTAssertEqual(s.daily?.days.count, 1)
        XCTAssertEqual(WorkloadPresentation.processingValue(s.workload), "100")
        XCTAssertEqual(WorkloadPresentation.queueLine(s.workload), "Queued 3 · 1 cleaning")
        XCTAssertEqual(WorkloadPresentation.admissionLine(s.workload), "Admission limit 8")
        XCTAssertEqual(WorkloadPresentation.callsValue(s.daily), "12,000")
        XCTAssertTrue(WorkloadPresentation.callsNote(s.daily, goal: 10_000).contains("over target"))
        XCTAssertEqual(WorkloadPresentation.progress(12_000, goal: 10_000), 1, "bar saturates")
        XCTAssertTrue(WorkloadPresentation.overGoal(12_000, goal: 10_000), "raw value stays above goal")
        XCTAssertEqual(WorkloadPresentation.peakValue(s.daily), "100")
        XCTAssertEqual(WorkloadPresentation.averageValue(s.daily), "10,000")
        XCTAssertTrue(WorkloadPresentation.averageNote(s.daily).contains("target met"))
    }

    func testUnavailableIsNotZero() throws {
        let s = try StatsDecoder.decode(Self.doc(workload: Self.workload(processing: "null", queued: "null", status: "unavailable"),
                                                 daily: Self.daily(todayCalls: "0", peak: "null", peakCov: "unavailable", avg: "null", cov: "unavailable", covered: 0, met: false)))
        XCTAssertNil(s.processingCount, "failed live read is unavailable, not 0")
        XCTAssertEqual(WorkloadPresentation.processingValue(s.workload), "—")
        XCTAssertEqual(WorkloadPresentation.queueLine(s.workload), "Queue: unavailable")
        XCTAssertEqual(WorkloadPresentation.peakValue(s.daily), "—")
        XCTAssertEqual(WorkloadPresentation.averageValue(s.daily), "—")
        XCTAssertEqual(WorkloadPresentation.callsValue(s.daily), "0", "a recorded zero is shown as zero")
        XCTAssertEqual(WorkloadPresentation.progress(nil as Int?, goal: 100), 0)
    }

    func testPartialCoverageIsLabelled() throws {
        let s = try StatsDecoder.decode(Self.doc(workload: Self.workload(stalled: "4"), daily: Self.daily(peak: "37", peakCov: "partial", avg: "8000.5", cov: "partial", covered: 3, met: false)))
        XCTAssertEqual(WorkloadPresentation.peakValue(s.daily), "≥ 37")
        XCTAssertTrue(WorkloadPresentation.peakNote(s.daily).contains("Lower bound"))
        XCTAssertEqual(WorkloadPresentation.averageValue(s.daily), "≈ 8,001")
        XCTAssertEqual(WorkloadPresentation.averageNote(s.daily), "Partial · 3 of 7 days")
        XCTAssertTrue(WorkloadPresentation.processingNote(s.workload).contains("4 stalled"))
        XCTAssertTrue(WorkloadPresentation.processingNote(s.workload).contains("2 salvaging"))
    }

    func testMalformedNewSectionDoesNotFailTheSnapshot() throws {
        let s = try StatsDecoder.decode(Self.doc(workload: #"{"processing":"many"}"#, daily: #"{"today":5}"#))
        XCTAssertNil(s.workload)
        XCTAssertNil(s.daily)
        XCTAssertEqual(s.goals, .default)
    }

    func testLargeAndOverTargetNumbers() {
        XCTAssertEqual(WorkloadPresentation.exact(100), "100")
        XCTAssertEqual(WorkloadPresentation.exact(999_999), "999,999")
        XCTAssertEqual(WorkloadPresentation.exact(12_345_678), "12.35M")
        XCTAssertEqual(WorkloadPresentation.progress(250, goal: 100), 1)
        XCTAssertEqual(WorkloadPresentation.progress(0, goal: 100), 0)
        XCTAssertEqual(WorkloadPresentation.progress(50, goal: 100), 0.5)
        XCTAssertEqual(WorkloadPresentation.progress(10, goal: 0), 0)
    }
}

@MainActor
final class WorkloadLayoutTests: XCTestCase {
    func model(_ workload: String, _ daily: String) throws -> MonitorModel {
        var s = try StatsDecoder.decode(StatsDecodeTests.fullData(workload: workload, daily: daily))
        s.warnings = Array(repeating: "A long coverage warning. " + String(repeating: "more ", count: 40), count: 4)
        return MonitorModel(testingSnapshot: s)
    }

    func fit(_ m: MonitorModel, notice: Bool = false) -> CGFloat {
        let d = UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!
        let u = UpdateManager(prefs: Preferences(defaults: d), consumeReport: false)
        let view = PopoverView(model: m, updates: u, profiles: ProfilesModel(testingSnapshot: nil), isDashboard: false, measuring: true)
        return NSHostingView(rootView: view).fittingSize.height
    }

    func testStatesFitThePanelWithoutOverflow() throws {
        let cases: [(String, String, String)] = [
            ("zero", WorkloadDecodeTests.workload(processing: "0", queued: "0"), WorkloadDecodeTests.daily(todayCalls: "0", peak: "0", avg: "0.0", cov: "complete", met: false)),
            ("100/100", WorkloadDecodeTests.workload(processing: "100"), WorkloadDecodeTests.daily()),
            ("over target", WorkloadDecodeTests.workload(processing: "250", queued: "120"), WorkloadDecodeTests.daily(todayCalls: "123456", peak: "250", avg: "12500.0")),
            ("huge", WorkloadDecodeTests.workload(processing: "99999", queued: "88888", stalled: "77"), WorkloadDecodeTests.daily(todayCalls: "98765432", peak: "99999", avg: "54321000.0")),
            ("unavailable", WorkloadDecodeTests.workload(processing: "null", queued: "null", status: "unavailable", limit: "null"), WorkloadDecodeTests.daily(todayCalls: "0", peak: "null", peakCov: "unavailable", avg: "null", cov: "unavailable", covered: 0, met: false)),
            ("partial + warnings", WorkloadDecodeTests.workload(stalled: "9", limit: "null"), WorkloadDecodeTests.daily(peak: "12", peakCov: "partial", avg: "4200.4", cov: "partial", covered: 2, met: false)),
        ]
        for (name, w, d) in cases {
            let h = fit(try model(w, d))
            XCTAssertLessThanOrEqual(h, Layout.height, "\(name) needs \(h) of \(Layout.height)")
        }
    }

    func testOlderCLIAndNoticeStillFit() throws {
        var s = try StatsDecoder.decode(Data(StatsDecodeTests.full.utf8))
        s.warnings = []
        XCTAssertLessThanOrEqual(fit(MonitorModel(testingSnapshot: s)), Layout.height)
        let m = try model(WorkloadDecodeTests.workload(), WorkloadDecodeTests.daily())
        let m2 = MonitorModel(testingSnapshot: m.snapshot!, error: "offload stats failed (exit 2): " + String(repeating: "x", count: 400))
        XCTAssertLessThanOrEqual(fit(m2), Layout.height)
    }

    func testWorkloadPanelShowsTheFourMetrics() throws {
        let s = try StatsDecoder.decode(WorkloadDecodeTests.doc(workload: WorkloadDecodeTests.workload(), daily: WorkloadDecodeTests.daily()))
        let host = NSHostingView(rootView: WorkloadPanel(snapshot: s).frame(width: Layout.width - 24))
        host.layoutSubtreeIfNeeded()
        XCTAssertLessThanOrEqual(host.fittingSize.width, Layout.width)
        XCTAssertLessThan(host.fittingSize.height, 200)
    }
}
