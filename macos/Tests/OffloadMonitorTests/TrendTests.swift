import XCTest
@testable import OffloadMonitor

final class TrendTests: XCTestCase {
    static func totals(runs: Int, ok: Int? = nil, usage: String = "null", usageRuns: Int = 0, cost: String = "null", costRuns: Int = 0) -> String {
        """
        {"runs":\(runs),"sessions":\(runs),"ok":\(ok ?? runs),"error":0,"timeout":0,"salvaged":0,"elapsed_ms":0,"elapsed_runs":\(runs),
         "usage":\(usage),"usage_runs":\(usageRuns),"cost_usd":\(cost),"cost_runs":\(costRuns),"excluded_salvage_cost_runs":0}
        """
    }

    static let usage = #"{"input_tokens":1000,"cached_input_tokens":400,"cache_write_input_tokens":0,"output_tokens":250}"#

    static var trendJSON: String {
        """
        {"interval":"hour","label":"Today by hour","points":[
          {"start":"2026-10-29T00:00:00+08:00","label":"00:00","totals":\(totals(runs: 0))},
          {"start":"2026-10-29T01:00:00+08:00","label":"01:00","totals":\(totals(runs: 2, usage: usage, usageRuns: 1, cost: "0.5", costRuns: 2))},
          {"start":"2026-10-29T02:00:00+08:00","label":"02:00","totals":\(totals(runs: 1))},
          {"start":"2026-10-29T03:00:00+08:00","label":"03:00","totals":\(totals(runs: 0))}]}
        """
    }

    static func doc(trend: String?) -> Data {
        let base = StatsDecodeTests.empty
        guard let trend else { return Data(base.utf8) }
        let i = base.lastIndex(of: "}")!
        return Data((base[..<i] + ",\"trend\":\(trend)}").utf8)
    }

    func decodeTrend() throws -> Trend { try StatsDecoder.decode(Self.doc(trend: Self.trendJSON)).trend }

    func testOldFixtureWithoutTrendDecodesAsEmpty() throws {
        let s = try StatsDecoder.decode(Self.doc(trend: nil))
        XCTAssertEqual(s.trend, .empty)
        XCTAssertTrue(s.trend.points.isEmpty)
        let full = try StatsDecoder.decode(Data(StatsDecodeTests.full.utf8))
        XCTAssertTrue(full.trend.points.isEmpty)
    }

    func testTrendDecodeKeepsNilAndZero() throws {
        let t = try decodeTrend()
        XCTAssertEqual(t.interval, "hour")
        XCTAssertEqual(t.label, "Today by hour")
        XCTAssertEqual(t.points.count, 4)
        XCTAssertNotNil(t.points[0].date)
        XCTAssertEqual(t.points[0].totals.runs, 0)
        XCTAssertNil(t.points[0].totals.usage)
        XCTAssertNil(t.points[0].totals.costUsd)
        XCTAssertEqual(t.points[1].totals.usage?.inputTokens, 1000)
        XCTAssertEqual(t.points[1].totals.costUsd, 0.5)
    }

    func testPartialTrendObjectAndBadTrendType() throws {
        let partial = try StatsDecoder.decode(Self.doc(trend: "{}")).trend
        XCTAssertEqual(partial.points, [])
        XCTAssertThrowsError(try StatsDecoder.decode(Self.doc(trend: "\"oops\""))) {
            guard case .invalidOutput = $0 as? StatsError else { return XCTFail("\($0)") }
        }
    }

    func testMetricValuesNeverInventZero() throws {
        let t = try decodeTrend()
        XCTAssertEqual(TrendMetric.runs.value(t.points[0].totals), 0) // genuine no runs
        XCTAssertNil(TrendMetric.tokens.value(t.points[0].totals))
        XCTAssertNil(TrendMetric.cost.value(t.points[0].totals))
        XCTAssertNil(TrendMetric.tokens.value(t.points[2].totals), "run without token data stays nil")
        XCTAssertEqual(TrendMetric.tokens.value(t.points[1].totals), 1250) // input + output
        XCTAssertEqual(TrendMetric.cost.value(t.points[1].totals), 0.5)
    }

    func testBarsKeepOrderAndNilValues() throws {
        let t = try decodeTrend()
        let tokens = TrendData.bars(t, metric: .tokens)
        XCTAssertEqual(tokens.map(\.point.label), ["00:00", "01:00", "02:00", "03:00"])
        XCTAssertEqual(tokens.map(\.value), [nil, 1250, nil, nil])
        XCTAssertEqual(TrendData.bars(t, metric: .runs).map(\.value), [0, 2, 1, 0])
        var bad = t
        bad.points[1].start = "not a date"
        XCTAssertEqual(TrendData.bars(bad, metric: .runs).count, 3)
    }

    func testDefaultSelectionIsLatestWithActivity() throws {
        let t = try decodeTrend()
        XCTAssertEqual(TrendData.defaultSelection(t), t.points[2].start)
        var quiet = t
        quiet.points = quiet.points.filter { $0.totals.runs == 0 }
        XCTAssertEqual(TrendData.defaultSelection(quiet), quiet.points.last?.start)
        XCTAssertNil(TrendData.defaultSelection(.empty))
    }

    func testContainingBucketHourly() throws {
        let bars = TrendData.bars(try decodeTrend(), metric: .runs)
        XCTAssertEqual(TrendData.containing(bars[1].date.addingTimeInterval(45 * 60), in: bars)?.id, bars[1].id, "right half stays in its bar")
        XCTAssertEqual(TrendData.containing(bars[1].date, in: bars)?.id, bars[1].id)
        XCTAssertEqual(TrendData.containing(bars[2].date, in: bars)?.id, bars[2].id, "exactly the next start selects the next bucket")
        XCTAssertEqual(TrendData.containing(bars[2].date.addingTimeInterval(-1), in: bars)?.id, bars[1].id)
        XCTAssertEqual(TrendData.containing(bars[0].date.addingTimeInterval(-7200), in: bars)?.id, bars[0].id, "before first clamps")
        XCTAssertEqual(TrendData.containing(bars[3].date.addingTimeInterval(86_400), in: bars)?.id, bars[3].id, "after last clamps")
        XCTAssertNil(TrendData.containing(Date(), in: []))
    }

    func testContainingBucketDaily() {
        let totals = (try? decodeTrend().points[0].totals)!
        let day = { (d: Int) in TrendPoint(start: String(format: "2026-10-%02dT00:00:00Z", d), label: "\(d)", totals: totals) }
        let trend = Trend(interval: "day", label: "x", points: [day(26), day(27), day(28)])
        let bars = TrendData.bars(trend, metric: .runs)
        XCTAssertEqual(TrendData.containing(bars[0].date.addingTimeInterval(20 * 3600), in: bars)?.id, bars[0].id)
        XCTAssertEqual(TrendData.containing(bars[1].date, in: bars)?.id, bars[1].id)
        XCTAssertEqual(TrendData.containing(bars[1].date.addingTimeInterval(13 * 3600), in: bars)?.id, bars[1].id)
    }

    func testXDomainCoversAllBucketsRegardlessOfMetric() throws {
        let t = try decodeTrend()
        let domain = try XCTUnwrap(TrendData.xDomain(t))
        XCTAssertEqual(domain.lowerBound, t.points[0].date)
        XCTAssertEqual(domain.upperBound, t.points[3].date!.addingTimeInterval(3600))
        // Only the middle bucket has token data, so the drawn bars alone would span one hour.
        XCTAssertEqual(TrendData.bars(t, metric: .tokens).compactMap { $0.value == nil ? nil : $0.date }.count, 1)
        // All-nil metric: the domain is unchanged.
        var noData = t
        for i in noData.points.indices { noData.points[i].totals.usage = nil; noData.points[i].totals.costUsd = nil }
        XCTAssertEqual(TrendData.xDomain(noData), domain)
    }

    func testXDomainSinglePointEmptyAndDayDST() throws {
        var t = try decodeTrend()
        t.points = [t.points[1]]
        let d = try XCTUnwrap(TrendData.xDomain(t))
        XCTAssertEqual(d.upperBound.timeIntervalSince(d.lowerBound), 3600)
        XCTAssertNil(TrendData.xDomain(.empty))
        // A day bucket ends at the next calendar midnight, so it is 23 h long on a spring-forward day.
        var cal = Calendar(identifier: .gregorian)
        cal.timeZone = TimeZone(identifier: "America/New_York")!
        let start = cal.date(from: DateComponents(year: 2026, month: 3, day: 8))!
        let iso = ISO8601DateFormatter().string(from: start)
        var day = t
        day.interval = "day"
        day.points = [TrendPoint(start: iso, label: "Mar 8", totals: t.points[0].totals)]
        let dom = try XCTUnwrap(TrendData.xDomain(day, calendar: cal))
        XCTAssertEqual(dom.upperBound.timeIntervalSince(dom.lowerBound), 23 * 3600)
    }

    func testEmptyMessages() throws {
        let t = try decodeTrend()
        XCTAssertNil(TrendData.emptyMessage(t, metric: .runs))
        XCTAssertNil(TrendData.emptyMessage(t, metric: .tokens))
        XCTAssertNotNil(TrendData.emptyMessage(.empty, metric: .runs))
        var noData = t
        for i in noData.points.indices { noData.points[i].totals.usage = nil; noData.points[i].totals.costUsd = nil }
        XCTAssertEqual(TrendData.emptyMessage(noData, metric: .tokens), "No token data recorded in this range.")
        XCTAssertEqual(TrendData.emptyMessage(noData, metric: .cost), "No cost data recorded in this range.")
        var noRuns = t
        for i in noRuns.points.indices { noRuns.points[i].totals.runs = 0 }
        XCTAssertEqual(TrendData.emptyMessage(noRuns, metric: .runs), "No runs in this range.")
    }

    func testCoverageText() throws {
        let t = try decodeTrend()
        XCTAssertEqual(TrendMetric.tokens.coverage(t.points[1].totals), "Tokens recorded for 1 of 2 runs")
        XCTAssertEqual(TrendMetric.cost.coverage(t.points[1].totals), "Cost recorded for 2 of 2 runs")
        XCTAssertEqual(TrendMetric.tokens.coverage(t.points[2].totals), "No token data recorded")
        XCTAssertEqual(TrendMetric.cost.coverage(t.points[0].totals), "No runs")
        XCTAssertEqual(TrendMetric.runs.coverage(t.points[1].totals), "2 succeeded, 0 failed")
    }

    func testExactValues() throws {
        let us = Locale(identifier: "en_US")
        let t = try decodeTrend()
        XCTAssertEqual(TrendMetric.tokens.exact(t.points[1].totals, locale: us), "1,250")
        XCTAssertEqual(TrendMetric.cost.exact(t.points[1].totals, locale: us), "$0.5000")
        XCTAssertEqual(TrendMetric.runs.exact(t.points[0].totals, locale: us), "0")
        XCTAssertNil(TrendMetric.tokens.exact(t.points[0].totals, locale: us))
        XCTAssertNil(TrendMetric.cost.exact(t.points[0].totals, locale: us))
        var big = t.points[1].totals
        big.usage = TokenUsage(inputTokens: 111_951_451, cachedInputTokens: 0, cacheWriteInputTokens: 0, outputTokens: 556_704)
        big.costUsd = 0.00123
        XCTAssertEqual(TrendMetric.tokens.exact(big, locale: us), "112,508,155")
        XCTAssertEqual(TrendMetric.cost.exact(big, locale: us), "$0.0012")
    }
}
