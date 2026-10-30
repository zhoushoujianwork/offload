import Foundation

/// Counts of invocations in the local Offload state directory. The CLI owns these numbers; the app only
/// displays them. A nil count means unavailable (the live state could not be read), never zero.
struct Workload: Codable, Equatable {
    var scope: String
    var stateDir: String
    var status: String          // ok | partial | unavailable
    var processing: Int?
    var salvaging: Int?
    var stalled: Int?
    var queued: Int?
    var cleaning: Int?
    var unresolved: Int?
    var lost: Int?
    var unreadableStates: Int
    var admissionLimit: Int?
    var admissionStatus: String // limited | unlimited | unavailable

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        scope = try c.decodeIfPresent(String.self, forKey: .scope) ?? ""
        stateDir = try c.decodeIfPresent(String.self, forKey: .stateDir) ?? ""
        status = try c.decodeIfPresent(String.self, forKey: .status) ?? "unavailable"
        processing = try c.decodeIfPresent(Int.self, forKey: .processing)
        salvaging = try c.decodeIfPresent(Int.self, forKey: .salvaging)
        stalled = try c.decodeIfPresent(Int.self, forKey: .stalled)
        queued = try c.decodeIfPresent(Int.self, forKey: .queued)
        cleaning = try c.decodeIfPresent(Int.self, forKey: .cleaning)
        unresolved = try c.decodeIfPresent(Int.self, forKey: .unresolved)
        lost = try c.decodeIfPresent(Int.self, forKey: .lost)
        unreadableStates = try c.decodeIfPresent(Int.self, forKey: .unreadableStates) ?? 0
        admissionLimit = try c.decodeIfPresent(Int.self, forKey: .admissionLimit)
        admissionStatus = try c.decodeIfPresent(String.self, forKey: .admissionStatus) ?? "unavailable"
    }

    init(status: String = "ok", processing: Int?, salvaging: Int? = 0, stalled: Int? = 0, queued: Int? = 0, cleaning: Int? = 0,
         unresolved: Int? = 0, lost: Int? = 0, unreadableStates: Int = 0, admissionLimit: Int? = nil, admissionStatus: String = "unavailable") {
        self.scope = ""; self.stateDir = ""; self.status = status
        self.processing = processing; self.salvaging = salvaging; self.stalled = stalled; self.queued = queued
        self.cleaning = cleaning; self.unresolved = unresolved; self.lost = lost
        self.unreadableStates = unreadableStates; self.admissionLimit = admissionLimit; self.admissionStatus = admissionStatus
    }

    private enum CodingKeys: String, CodingKey {
        case scope, stateDir, status, processing, salvaging, stalled, queued, cleaning, unresolved, lost
        case unreadableStates, admissionLimit, admissionStatus
    }
}

struct WorkloadGoals: Codable, Equatable {
    var processing: Int
    var dailyCalls: Int
    var note: String?

    static let `default` = WorkloadGoals(processing: 100, dailyCalls: 10_000, note: nil)
}

struct DailyDay: Codable, Equatable {
    var date: String
    var recordedCalls: Int?
    var covered: Bool
    var peakProcessing: Int?
    var peakAt: String?
    var peakCoverage: String     // full | partial | unavailable
    var peakIncompleteRuns: Int

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        date = try c.decodeIfPresent(String.self, forKey: .date) ?? ""
        recordedCalls = try c.decodeIfPresent(Int.self, forKey: .recordedCalls)
        covered = try c.decodeIfPresent(Bool.self, forKey: .covered) ?? false
        peakProcessing = try c.decodeIfPresent(Int.self, forKey: .peakProcessing)
        peakAt = try c.decodeIfPresent(String.self, forKey: .peakAt)
        peakCoverage = try c.decodeIfPresent(String.self, forKey: .peakCoverage) ?? "unavailable"
        peakIncompleteRuns = try c.decodeIfPresent(Int.self, forKey: .peakIncompleteRuns) ?? 0
    }

    init(date: String, recordedCalls: Int?, covered: Bool = true, peakProcessing: Int? = nil, peakCoverage: String = "unavailable") {
        self.date = date; self.recordedCalls = recordedCalls; self.covered = covered
        self.peakProcessing = peakProcessing; self.peakAt = nil; self.peakCoverage = peakCoverage; self.peakIncompleteRuns = 0
    }

    private enum CodingKeys: String, CodingKey { case date, recordedCalls, covered, peakProcessing, peakAt, peakCoverage, peakIncompleteRuns }
}

struct DailyAverage: Codable, Equatable {
    var windowStart: String
    var windowEnd: String
    var days: Int
    var coveredDays: Int
    var recordedCallsPerDay: Double?
    var coverage: String         // complete | partial | unavailable
    var goalMet: Bool
    var note: String?

    init(windowStart: String = "", windowEnd: String = "", days: Int = 7, coveredDays: Int, recordedCallsPerDay: Double?,
         coverage: String, goalMet: Bool = false, note: String? = nil) {
        self.windowStart = windowStart; self.windowEnd = windowEnd; self.days = days; self.coveredDays = coveredDays
        self.recordedCallsPerDay = recordedCallsPerDay; self.coverage = coverage; self.goalMet = goalMet; self.note = note
    }
}

struct Daily: Codable, Equatable {
    var timezone: String
    var label: String
    var today: DailyDay
    var days: [DailyDay]
    var average: DailyAverage
}

/// How the dashboard words the workload. Pure functions so decode and layout tests can pin the wording.
enum WorkloadPresentation {
    /// Grouped digits below one million, compact above.
    static func exact(_ n: Int) -> String {
        n < 1_000_000 ? n.formatted(.number.grouping(.automatic)) : Format.count(Int64(n))
    }

    static func number(_ n: Int?) -> String { n.map(exact) ?? "—" }

    /// Bar fill in 0...1. The bar saturates; the number next to it never does.
    static func progress(_ value: Int?, goal: Int) -> Double {
        guard let value, goal > 0 else { return 0 }
        return min(1, max(0, Double(value) / Double(goal)))
    }

    static func progress(_ value: Double?, goal: Int) -> Double {
        guard let value, goal > 0 else { return 0 }
        return min(1, max(0, value / Double(goal)))
    }

    static func overGoal(_ value: Int?, goal: Int) -> Bool { (value ?? 0) > goal }

    static func processingValue(_ w: Workload?) -> String { number(w?.processing) }

    static func processingNote(_ w: Workload?) -> String {
        guard let w else { return "Not reported by this CLI version" }
        guard let p = w.processing else { return "Live state unavailable" }
        var parts: [String] = []
        if let s = w.salvaging, s > 0 { parts.append("\(exact(s)) salvaging") }
        if let s = w.stalled, s > 0 { parts.append("\(exact(s)) stalled ⚠") }
        if w.status == "partial" { parts.append("at least · \(w.unreadableStates) unreadable") }
        if parts.isEmpty { parts.append(p == 0 ? "Idle" : "Processing now") }
        return parts.joined(separator: " · ")
    }

    /// Queued is always separate from processing. Cleanup, unresolved and lost are not processing either.
    static func queueLine(_ w: Workload?) -> String {
        guard let w else { return "Queue: not reported" }
        guard w.status != "unavailable" else { return "Queue: unavailable" }
        var s = "Queued \(number(w.queued))"
        let other: [(String, Int?)] = [("cleaning", w.cleaning), ("unresolved", w.unresolved), ("lost", w.lost)]
        for (name, n) in other { if let n, n > 0 { s += " · \(exact(n)) \(name)" } }
        return s
    }

    static func admissionLine(_ w: Workload?) -> String? {
        guard let w else { return nil }
        switch w.admissionStatus {
        case "limited": return w.admissionLimit.map { "Admission limit \(exact($0))" }
        case "unlimited": return "Admission limit: none"
        default: return nil
        }
    }

    static func callsValue(_ d: Daily?) -> String { number(d?.today.recordedCalls) }

    static func callsNote(_ d: Daily?, goal: Int) -> String {
        guard let n = d?.today.recordedCalls else { return "Not reported" }
        return "recorded calls" + (n > goal ? " · over target" : "")
    }

    static func peakValue(_ d: Daily?) -> String {
        guard let d, let p = d.today.peakProcessing else { return "—" }
        return d.today.peakCoverage == "full" ? exact(p) : "≥ \(exact(p))"
    }

    static func peakNote(_ d: Daily?) -> String {
        guard let d else { return "Not reported" }
        switch d.today.peakCoverage {
        case "full": return "Peak concurrency today"
        case "partial": return "Lower bound · partial"
        default: return "Peak unavailable"
        }
    }

    static func averageValue(_ d: Daily?) -> String {
        guard let a = d?.average, let v = a.recordedCallsPerDay else { return "—" }
        let text = exact(Int(v.rounded()))
        return a.coverage == "complete" ? text : "≈ \(text)"
    }

    static func averageNote(_ d: Daily?) -> String {
        guard let a = d?.average else { return "Not reported" }
        switch a.coverage {
        case "complete": return "7-day avg · completed days" + (a.goalMet ? " · target met" : "")
        case "partial": return "Partial · \(a.coveredDays) of \(a.days) days"
        default: return "No completed-day history"
        }
    }
}
