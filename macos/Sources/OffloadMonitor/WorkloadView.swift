import SwiftUI

/// Concurrency and daily volume, the first thing in the panel. All numbers come from `offload stats --json`;
/// this view only formats them. Goals are observability targets, not limits or measured capacity.
struct WorkloadPanel: View {
    let snapshot: StatsSnapshot

    var body: some View {
        let w = snapshot.workload
        let d = snapshot.daily
        let g = snapshot.goals
        Card(tint: .omTeal) {
            VStack(alignment: .leading, spacing: 3) {
                HStack(alignment: .top, spacing: 10) {
                    tile(title: "Processing now", value: WorkloadPresentation.processingValue(w), goal: g.processing,
                         note: WorkloadPresentation.processingNote(w), fill: WorkloadPresentation.progress(w?.processing, goal: g.processing),
                         over: WorkloadPresentation.overGoal(w?.processing, goal: g.processing), tint: .omTeal,
                         warn: (w?.stalled ?? 0) > 0 || w?.status == "partial")
                    tile(title: "Calls today", value: WorkloadPresentation.callsValue(d), goal: g.dailyCalls,
                         note: WorkloadPresentation.callsNote(d, goal: g.dailyCalls),
                         fill: WorkloadPresentation.progress(d?.today.recordedCalls, goal: g.dailyCalls),
                         over: WorkloadPresentation.overGoal(d?.today.recordedCalls, goal: g.dailyCalls), tint: .omBlue, warn: false)
                }
                HStack(alignment: .top, spacing: 10) {
                    small(title: "Peak today", value: WorkloadPresentation.peakValue(d), goal: g.processing,
                          note: WorkloadPresentation.peakNote(d), warn: d?.today.peakCoverage == "partial")
                    small(title: "7-day average", value: WorkloadPresentation.averageValue(d), goal: g.dailyCalls,
                          note: WorkloadPresentation.averageNote(d), warn: d != nil && d?.average.coverage != "complete")
                }
                HStack(spacing: 6) {
                    Image(systemName: "tray.full").font(.system(size: 9)).foregroundStyle(.secondary)
                    Text(WorkloadPresentation.queueLine(w)).font(.system(size: 10, weight: .medium)).lineLimit(1).minimumScaleFactor(0.8)
                    Spacer(minLength: 4)
                    if let a = WorkloadPresentation.admissionLine(w) {
                        Text(a).font(.system(size: 9)).foregroundStyle(.secondary).lineLimit(1)
                    }
                }
                Text("Whole computer · goals are targets, not measured capacity")
                    .font(.system(size: 8.5)).foregroundStyle(.secondary).lineLimit(1).minimumScaleFactor(0.8)
                    .help(w?.scope ?? "Counts cover this computer's Offload state directory.")
            }
        }
    }

    private func tile(title: String, value: String, goal: Int, note: String, fill: Double, over: Bool, tint: Color, warn: Bool) -> some View {
        VStack(alignment: .leading, spacing: 1) {
            Text(title).font(.system(size: 9.5)).foregroundStyle(.secondary).lineLimit(1)
            HStack(alignment: .firstTextBaseline, spacing: 3) {
                Text(value).font(.system(size: 20, weight: .semibold).monospacedDigit()).lineLimit(1).minimumScaleFactor(0.5)
                Text("/ \(WorkloadPresentation.exact(goal))").font(.system(size: 10).monospacedDigit()).foregroundStyle(.secondary)
                    .lineLimit(1).minimumScaleFactor(0.7).layoutPriority(-1)
            }
            GoalBar(fill: fill, tint: over ? .orange : tint)
            noteLine(note, warn: warn)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    /// Secondary metrics: no bar, one value line and one note line.
    private func small(title: String, value: String, goal: Int, note: String, warn: Bool) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack(alignment: .firstTextBaseline, spacing: 4) {
                Text(title).font(.system(size: 9.5)).foregroundStyle(.secondary).lineLimit(1)
                Spacer(minLength: 2)
                Text(value).font(.system(size: 13, weight: .semibold).monospacedDigit()).lineLimit(1).minimumScaleFactor(0.5)
                Text("/ \(WorkloadPresentation.exact(goal))").font(.system(size: 9).monospacedDigit()).foregroundStyle(.secondary)
                    .lineLimit(1).minimumScaleFactor(0.7).layoutPriority(-1)
            }
            noteLine(note, warn: warn)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private func noteLine(_ note: String, warn: Bool) -> some View {
        HStack(spacing: 2) {
            if warn { Image(systemName: "exclamationmark.triangle.fill").font(.system(size: 8)).foregroundStyle(.orange) }
            Text(note).font(.system(size: 9)).foregroundStyle(.secondary).lineLimit(1).minimumScaleFactor(0.8)
        }
    }
}

/// One compact line of run totals. Token and cost detail keeps its own window.
struct SummaryStrip: View {
    let totals: StatsTotals

    var body: some View {
        Card(tint: .omBlue) {
            HStack(alignment: .top, spacing: 8) {
                cell("Runs", "\(totals.runs)", "\(totals.ok) ok · \(totals.error + totals.timeout) failed")
                cell("Tokens", totals.usage.map { Format.count($0.inputTokens + $0.outputTokens) } ?? "—", Format.coverage(totals.usageRuns, of: totals.runs))
                cell("Est. cost", totals.costUsd.map(Format.cost) ?? "—", Format.coverage(totals.costRuns, of: totals.runs))
                cell("Sessions", "\(totals.sessions)", "distinct")
            }
        }
    }

    private func cell(_ title: String, _ value: String, _ sub: String) -> some View {
        VStack(alignment: .leading, spacing: 0) {
            Text(title).font(.system(size: 9)).foregroundStyle(.secondary).lineLimit(1)
            Text(value).font(.system(size: 13, weight: .semibold).monospacedDigit()).lineLimit(1).minimumScaleFactor(0.6)
            Text(sub).font(.system(size: 8)).foregroundStyle(.secondary).lineLimit(1).minimumScaleFactor(0.7)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

/// A progress bar that saturates at the goal. The number beside it keeps the raw value.
struct GoalBar: View {
    var fill: Double
    var tint: Color
    var body: some View {
        GeometryReader { geo in
            ZStack(alignment: .leading) {
                Capsule().fill(tint.opacity(0.15))
                Capsule().fill(tint).frame(width: max(fill > 0 ? 3 : 0, geo.size.width * fill))
            }
        }
        .frame(height: 4)
    }
}
