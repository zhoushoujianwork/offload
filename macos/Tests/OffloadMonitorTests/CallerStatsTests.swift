import Foundation
import XCTest
@testable import OffloadMonitor

@MainActor
final class CallerStatsTests: XCTestCase {
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
