import Foundation
import XCTest
@testable import OffloadMonitor

@MainActor
final class CallerStatsTests: XCTestCase {
    func testTaskCallerIsDecodedWithoutInferringLegacySources() throws {
        let legacy = try StatsDecoder.decode(Data(StatsDecodeTests.full.utf8)).active[0]
        XCTAssertEqual(legacy.callerName, "unspecified")
        XCTAssertTrue(legacy.callerExplanation.contains("unknown"))
        var json = try XCTUnwrap(JSONSerialization.jsonObject(with: Data(StatsDecodeTests.full.utf8)) as? [String: Any])
        var active = try XCTUnwrap(json["active"] as? [[String: Any]])
        active[0]["caller"] = "agh"
        json["active"] = active
        var task = try StatsDecoder.decode(JSONSerialization.data(withJSONObject: json)).active[0]
        XCTAssertEqual(task.callerName, "agh")
        XCTAssertTrue(task.callerExplanation.contains("Agent-Host"))
        task.caller = "custom-runner"
        XCTAssertEqual(task.callerName, "custom-runner")
        XCTAssertTrue(task.callerExplanation.contains("custom-runner"))
        task.caller = " "
        XCTAssertEqual(task.callerName, "unspecified")
    }

    func testLegacyAndCallerGroupsDecode() throws {
        let legacy = try StatsDecoder.decode(Data(StatsDecodeTests.full.utf8))
        XCTAssertTrue(legacy.callers.isEmpty)
        XCTAssertNil(legacy.callerFilter)
        var json = try XCTUnwrap(JSONSerialization.jsonObject(with: Data(StatsDecodeTests.full.utf8)) as? [String: Any])
        json["callers"] = [["key": "agh", "name": "agh", "totals": json["totals"]!],
                           ["key": "unspecified", "name": "unspecified", "totals": json["totals"]!]]
        json["caller_filter"] = "agh"
        let s = try StatsDecoder.decode(JSONSerialization.data(withJSONObject: json))
        XCTAssertEqual(s.callers.map(\.key), ["agh", "unspecified"])
        XCTAssertEqual(s.callerFilter, "agh")
        XCTAssertEqual(s.totals, legacy.totals)
        let model = MonitorModel(testingSnapshot: s)
        XCTAssertEqual(model.callerGroups, s.callers)
        model.callerFilter = "agh"
        XCTAssertNil(model.snapshot, "do not relabel old unfiltered data as filtered")
        XCTAssertEqual(model.callerGroups, s.callers, "filter choices survive a refresh")
        model.callerFilter = nil
    }
}
