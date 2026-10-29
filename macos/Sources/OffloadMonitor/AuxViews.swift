import SwiftUI

/// Secondary windows. They hold the content that does not fit the fixed main panel. Long text scrolls here
/// with indicators hidden; the main panel never scrolls.
struct DetailsView: View {
    @ObservedObject var model: MonitorModel

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if let t = model.snapshot?.totals {
                Text("Tokens").font(.headline)
                Grid(alignment: .leading, horizontalSpacing: 16, verticalSpacing: 4) {
                    if let u = t.usage {
                        row("Input", Format.count(u.inputTokens), exact: u.inputTokens)
                        row("Cache read", Format.count(u.cachedInputTokens), exact: u.cachedInputTokens)
                        row("Cache write", Format.count(u.cacheWriteInputTokens), exact: u.cacheWriteInputTokens)
                        row("Output", Format.count(u.outputTokens), exact: u.outputTokens)
                    } else {
                        row("Tokens", "not recorded", exact: nil)
                    }
                    GridRow { Text("Coverage").foregroundStyle(.secondary); Text(Format.coverage(t.usageRuns, of: t.runs)); Text("") }
                }
                Divider()
                Text("Estimated cost").font(.headline)
                Grid(alignment: .leading, horizontalSpacing: 16, verticalSpacing: 4) {
                    GridRow { Text("Total").foregroundStyle(.secondary); Text(t.costUsd.map { String(format: "$%.4f", $0) } ?? "not recorded"); Text("") }
                    GridRow { Text("Coverage").foregroundStyle(.secondary); Text(Format.coverage(t.costRuns, of: t.runs)); Text("") }
                    if t.excludedSalvageCostRuns > 0 {
                        GridRow {
                            Text("Salvaged").foregroundStyle(.secondary)
                            Text("\(t.excludedSalvageCostRuns) run cost\(t.excludedSalvageCostRuns == 1 ? "" : "s") not counted")
                            Text("")
                        }
                    }
                }
                if t.timeout > 0 || t.salvaged > 0 {
                    Divider()
                    Text("\(t.error) errors, \(t.timeout) timed out, \(t.salvaged) salvaged").font(.caption).foregroundStyle(.secondary)
                }
            } else {
                Text("No data loaded yet.").foregroundStyle(.secondary)
            }
            Spacer(minLength: 0)
        }
        .font(.callout)
        .padding(16)
        .frame(minWidth: 300, maxWidth: .infinity, minHeight: 200, maxHeight: .infinity, alignment: .topLeading)
    }

    private func row(_ k: String, _ short: String, exact: Int64?) -> some View {
        GridRow {
            Text(k).foregroundStyle(.secondary)
            Text(short).monospacedDigit()
            Text(exact.map { "(\($0.formatted()))" } ?? "").font(.caption).foregroundStyle(.secondary).monospacedDigit()
        }
    }
}

struct NotesView: View {
    @ObservedObject var model: MonitorModel

    var body: some View {
        ScrollView(showsIndicators: false) {
            VStack(alignment: .leading, spacing: 8) {
                let notes = model.snapshot?.warnings ?? []
                if notes.isEmpty { Text("No notes for this period.").foregroundStyle(.secondary) }
                ForEach(Array(notes.enumerated()), id: \.offset) { _, w in
                    Text(w).fixedSize(horizontal: false, vertical: true).textSelection(.enabled)
                }
            }
            .font(.callout)
            .padding(16)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}

/// Every current notice in full, with its actions.
struct NoticesView: View {
    @ObservedObject var model: MonitorModel
    @ObservedObject var updates: UpdateManager

    var body: some View {
        ScrollView(showsIndicators: false) {
            VStack(alignment: .leading, spacing: 10) {
                if let err = model.lastError {
                    Banner(symbol: "exclamationmark.triangle.fill", tint: .orange,
                           title: model.snapshot == nil ? "Cannot read stats" : "Stats refresh problem", message: err)
                }
                UpdateBanner(updates: updates)
            }
            .padding(16)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}

struct ReportView: View {
    let run: RecentRun

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            // Sticky: short and single-line, so it cannot grow with the report.
            HStack(spacing: 6) {
                Image(systemName: icon.0).foregroundStyle(icon.1)
                Text(run.projectName).font(.headline).lineLimit(1)
                Spacer()
                Text(run.ts).font(.caption).foregroundStyle(.secondary).lineLimit(1)
            }
            .padding(.horizontal, 16)
            .padding(.vertical, 10)
            Divider()
            // Everything long scrolls together, so a huge task prompt cannot push the result out of reach.
            ScrollView(showsIndicators: false) {
                VStack(alignment: .leading, spacing: 8) {
                    Text(run.task).font(.callout.weight(.medium)).textSelection(.enabled)
                    Text(meta).font(.caption).foregroundStyle(.secondary).textSelection(.enabled)
                    Divider()
                    Text(run.result.isEmpty ? "No result text recorded." : run.result)
                        .font(.callout).textSelection(.enabled)
                }
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(16)
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity)
        }
        .frame(minWidth: 360, maxWidth: .infinity, minHeight: 260, maxHeight: .infinity, alignment: .topLeading)
    }

    private var meta: String {
        var parts = [run.model, run.status + (run.salvaged ? " (salvaged)" : "")]
        parts.append(run.costUsd.map { "est. " + String(format: "$%.4f", $0) } ?? "cost not recorded")
        if let u = run.usage { parts.append("\(Format.count(u.inputTokens)) in / \(Format.count(u.outputTokens)) out") }
        return parts.joined(separator: " · ")
    }

    private var icon: (String, Color) {
        switch run.status {
        case "ok": return ("checkmark.circle.fill", .green)
        case "timeout": return ("clock.badge.exclamationmark.fill", .orange)
        default: return ("xmark.circle.fill", .red)
        }
    }
}
