import Foundation

enum TrendMetric: String, CaseIterable, Identifiable {
    case runs, tokens, cost
    var id: String { rawValue }
    var label: String {
        switch self {
        case .runs: return "Runs"
        case .tokens: return "Tokens"
        case .cost: return "Cost"
        }
    }

    /// The bar height for a bucket. Nil means nothing was recorded, which is not the same as zero.
    /// Runs are always known: 0 means no runs happened.
    func value(_ t: StatsTotals) -> Double? {
        switch self {
        case .runs: return Double(t.runs)
        case .tokens: return t.usage.map { Double($0.inputTokens + $0.outputTokens) }
        case .cost: return t.costUsd
        }
    }

    func formatted(_ v: Double) -> String {
        switch self {
        case .runs: return "\(Int(v))"
        case .tokens: return Format.count(Int64(v))
        case .cost: return Format.cost(v)
        }
    }

    /// The exact value for the selected-bucket detail: a grouped integer, or USD to four decimals.
    func exact(_ t: StatsTotals, locale: Locale = .current) -> String? {
        guard let v = value(t) else { return nil }
        switch self {
        case .runs, .tokens:
            let f = NumberFormatter()
            f.numberStyle = .decimal
            f.locale = locale
            f.maximumFractionDigits = 0
            return f.string(from: NSNumber(value: v))
        case .cost:
            return "$" + String(format: "%.4f", locale: locale, v)
        }
    }

    /// One-line statement of what the bucket's number covers.
    func coverage(_ t: StatsTotals) -> String {
        switch self {
        case .runs:
            return "\(t.ok) succeeded, \(t.error + t.timeout) failed"
        case .tokens:
            return t.runs == 0 ? "No runs" : (t.usage == nil ? "No token data recorded" : "Tokens recorded for \(Format.coverage(t.usageRuns, of: t.runs))")
        case .cost:
            return t.runs == 0 ? "No runs" : (t.costUsd == nil ? "No cost data recorded" : "Cost recorded for \(Format.coverage(t.costRuns, of: t.runs))")
        }
    }
}

struct TrendBar: Identifiable, Equatable {
    var point: TrendPoint
    var date: Date
    /// Nil bars are not drawn.
    var value: Double?
    var id: String { point.start }
}

enum TrendData {
    /// Bars in the order given. Points whose start time cannot be parsed are dropped.
    static func bars(_ trend: Trend, metric: TrendMetric) -> [TrendBar] {
        trend.points.compactMap { p in
            p.date.map { TrendBar(point: p, date: $0, value: metric.value(p.totals)) }
        }
    }

    /// The latest bucket with runs, otherwise the last bucket.
    static func defaultSelection(_ trend: Trend) -> String? {
        (trend.points.last { $0.totals.runs > 0 } ?? trend.points.last)?.start
    }

    /// The bucket that contains `date`: the last one whose start is at or before it. A bar spans its
    /// whole interval, so the right half of a bar still selects that bar. Dates before the first
    /// bucket select the first, dates after the last select the last. `bars` must be in time order.
    static func containing(_ date: Date, in bars: [TrendBar]) -> TrendBar? {
        bars.last { $0.date <= date } ?? bars.first
    }

    /// X-axis range covering every bucket, from the first start to the end of the last bucket, whatever
    /// the metric. Buckets without a value for the metric still take up their space.
    static func xDomain(_ trend: Trend, calendar: Calendar = .current) -> ClosedRange<Date>? {
        let dates = trend.points.compactMap(\.date)
        guard let first = dates.min(), let lastStart = dates.max() else { return nil }
        let component: Calendar.Component = trend.interval == "hour" ? .hour : .day
        guard let end = calendar.date(byAdding: component, value: 1, to: lastStart) else { return nil }
        return first...end
    }

    /// Why there is nothing to draw, or nil when at least one bar has a value to show.
    static func emptyMessage(_ trend: Trend, metric: TrendMetric) -> String? {
        if trend.points.isEmpty { return "No trend data. The bundled offload command may be older than this app." }
        let bars = bars(trend, metric: metric)
        if bars.isEmpty { return "Trend times could not be read." }
        switch metric {
        case .runs:
            return trend.points.allSatisfy({ $0.totals.runs == 0 }) ? "No runs in this range." : nil
        case .tokens:
            return bars.allSatisfy({ $0.value == nil }) ? "No token data recorded in this range." : nil
        case .cost:
            return bars.allSatisfy({ $0.value == nil }) ? "No cost data recorded in this range." : nil
        }
    }

    static func axisFormat(interval: String) -> Date.FormatStyle {
        interval == "hour" ? .dateTime.hour() : .dateTime.month(.defaultDigits).day()
    }
}
