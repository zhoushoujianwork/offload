import Foundation

/// Identity constants shared by the app, the updater and the build scripts.
enum AppIdentity {
    static let bundleID = "local.offload.monitor"
    static let appBundleName = "Offload Monitor.app"
    static let repoOwner = "zhoushoujianwork"
    static let repoName = "offload"
    static let latestReleaseURL = URL(string: "https://api.github.com/repos/zhoushoujianwork/offload/releases/latest")!
    static let supportedSchema = 1
}

enum StatsPeriod: String, CaseIterable, Identifiable, Codable {
    case today, week, month, all
    var id: String { rawValue }
    var label: String {
        switch self {
        case .today: return "Today"
        case .week: return "Week"
        case .month: return "Month"
        case .all: return "All"
        }
    }
}

struct TokenUsage: Codable, Equatable {
    var inputTokens: Int64
    var cachedInputTokens: Int64
    var cacheWriteInputTokens: Int64
    var outputTokens: Int64
}

/// Sums over one period. `usage` and `costUsd` are nil when no covered run recorded them;
/// `usageRuns`/`costRuns`/`elapsedRuns` say how many of `runs` the sums cover.
struct StatsTotals: Codable, Equatable {
    var runs: Int
    var sessions: Int
    var ok: Int
    var error: Int
    var timeout: Int
    var salvaged: Int
    var elapsedMs: Int64
    var elapsedRuns: Int
    var usage: TokenUsage?
    var usageRuns: Int
    var costUsd: Double?
    var costRuns: Int
    var excludedSalvageCostRuns: Int
}

struct StatsGroup: Codable, Equatable, Identifiable {
    var key: String
    var name: String
    var totals: StatsTotals
    var id: String { key }
}

struct ActiveTask: Codable, Equatable, Identifiable {
    var pid: Int
    var phase: String
    var status: String?
    var exitCode: Int?
    var dir: String
    var task: String
    var profile: String
    var model: String
    var session: String?
    var startedAt: String
    var updatedAt: String
    var lastEventAt: String
    var toolCalls: Int
    var permissionDenials: Int
    var lastTool: String?
    var stalled: Bool

    var id: String { "\(pid)|\(startedAt)" }
    /// Running and salvaging workers count as running; "lost" is advisory only.
    var isRunning: Bool { phase == "running" || phase == "salvaging" }
    var isLost: Bool { phase == "lost" }
    var projectName: String { (dir as NSString).lastPathComponent.isEmpty ? dir : (dir as NSString).lastPathComponent }
}

struct RecentRun: Codable, Equatable {
    var ts: String
    var status: String
    var dir: String
    var task: String
    var session: String
    var model: String
    var costUsd: Double?
    var usage: TokenUsage?
    var result: String
    var salvaged: Bool
    var projectName: String { (dir as NSString).lastPathComponent.isEmpty ? dir : (dir as NSString).lastPathComponent }
}

/// One time bucket of the trend chart. `totals` has the same fields as the period totals, so a bucket
/// with no recorded usage or cost carries nil there, never zero.
struct TrendPoint: Codable, Equatable, Identifiable {
    var start: String
    var label: String
    var totals: StatsTotals
    var id: String { start }
    var date: Date? { Format.parseTimestamp(start) }
}

/// Optional in the JSON: older CLIs omit it, which decodes as an empty trend.
struct Trend: Codable, Equatable {
    var interval: String
    var label: String
    var points: [TrendPoint]

    static let empty = Trend(interval: "day", label: "", points: [])

    init(interval: String, label: String, points: [TrendPoint]) {
        self.interval = interval; self.label = label; self.points = points
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        interval = try c.decodeIfPresent(String.self, forKey: .interval) ?? "day"
        label = try c.decodeIfPresent(String.self, forKey: .label) ?? ""
        points = try c.decodeIfPresent([TrendPoint].self, forKey: .points) ?? []
    }

    private enum CodingKeys: String, CodingKey { case interval, label, points }
}

struct StatsSnapshot: Codable, Equatable {
    var schemaVersion: Int
    var generatedAt: String
    var period: String
    var totals: StatsTotals
    var projects: [StatsGroup]
    var models: [StatsGroup]
    var active: [ActiveTask]
    var recent: [RecentRun]
    var warnings: [String]
    var trend: Trend
    /// Optional in the JSON: older CLIs omit these, and the dashboard then says the metric is not reported.
    var workload: Workload?
    var goals: WorkloadGoals
    var daily: Daily?
    var callers: [StatsGroup] = []
    var callerFilter: String? = nil

    var runningCount: Int { active.filter(\.isRunning).count }
    /// Tasks processing now as the CLI counts them. nil when the live state could not be read: unavailable is
    /// not zero. Older CLIs send no workload, so their running/salvaging entries are counted instead.
    var processingCount: Int? { workload.map { $0.processing } ?? runningCount }
    var stalledCount: Int { active.filter { $0.isRunning && $0.stalled }.count }
    var lostCount: Int { active.filter(\.isLost).count }
}

extension StatsSnapshot {
    private enum CodingKeys: String, CodingKey {
        case schemaVersion, generatedAt, period, totals, projects, models, active, recent, warnings, trend, workload, goals, daily, callers, callerFilter
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        schemaVersion = try c.decode(Int.self, forKey: .schemaVersion)
        generatedAt = try c.decode(String.self, forKey: .generatedAt)
        period = try c.decode(String.self, forKey: .period)
        totals = try c.decode(StatsTotals.self, forKey: .totals)
        projects = try c.decode([StatsGroup].self, forKey: .projects)
        models = try c.decode([StatsGroup].self, forKey: .models)
        active = try c.decode([ActiveTask].self, forKey: .active)
        recent = try c.decode([RecentRun].self, forKey: .recent)
        warnings = try c.decode([String].self, forKey: .warnings)
        trend = try c.decodeIfPresent(Trend.self, forKey: .trend) ?? .empty
        callers = try c.decodeIfPresent([StatsGroup].self, forKey: .callers) ?? []
        callerFilter = try c.decodeIfPresent(String.self, forKey: .callerFilter)
        // A malformed new section degrades to "not reported" instead of failing the whole snapshot.
        workload = (try? c.decodeIfPresent(Workload.self, forKey: .workload)) ?? nil
        goals = ((try? c.decodeIfPresent(WorkloadGoals.self, forKey: .goals)) ?? nil) ?? .default
        daily = (try? c.decodeIfPresent(Daily.self, forKey: .daily)) ?? nil
        // A malformed new section degrades to "not reported" instead of failing the whole snapshot.
        workload = (try? c.decodeIfPresent(Workload.self, forKey: .workload)) ?? nil
        goals = ((try? c.decodeIfPresent(WorkloadGoals.self, forKey: .goals)) ?? nil) ?? .default
        daily = (try? c.decodeIfPresent(Daily.self, forKey: .daily)) ?? nil
    }
}

enum StatsDecoder {
    private struct SchemaProbe: Decodable { var schemaVersion: Int }

    static func decode(_ data: Data) throws -> StatsSnapshot {
        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        do {
            let probe = try decoder.decode(SchemaProbe.self, from: data)
            guard probe.schemaVersion == AppIdentity.supportedSchema else {
                throw StatsError.unsupportedSchema(probe.schemaVersion)
            }
            return try decoder.decode(StatsSnapshot.self, from: data)
        } catch let e as StatsError {
            throw e
        } catch {
            throw StatsError.invalidOutput(describe(error))
        }
    }

    private static func describe(_ error: Error) -> String {
        guard let e = error as? DecodingError else { return error.localizedDescription }
        switch e {
        case .keyNotFound(let k, _): return "missing field \(k.stringValue)"
        case .typeMismatch(_, let c), .valueNotFound(_, let c): return "unexpected value at \(c.codingPath.map(\.stringValue).joined(separator: "."))"
        case .dataCorrupted: return "not valid JSON"
        @unknown default: return "decode failure"
        }
    }
}
