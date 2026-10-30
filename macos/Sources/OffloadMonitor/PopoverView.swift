import Charts
import SwiftUI

// Palette: restrained blue / teal / purple that adapts to light and dark.
extension Color {
    static let omBlue = Color(red: 0.25, green: 0.52, blue: 0.96)
    static let omTeal = Color(red: 0.18, green: 0.70, blue: 0.68)
    static let omPurple = Color(red: 0.60, green: 0.42, blue: 0.92)
}

extension TrendMetric {
    var tint: Color {
        switch self {
        case .runs: return .omBlue
        case .tokens: return .omTeal
        case .cost: return .omPurple
        }
    }
}

/// The main panel. It is a fixed-size layout and never scrolls: long lists are paged, and anything
/// longer (details, notes, reports, full notices) opens in its own window.
struct PopoverView: View {
    @ObservedObject var model: MonitorModel
    @ObservedObject var updates: UpdateManager
    @ObservedObject var profiles: ProfilesModel
    /// Motion for the header status symbol.
    @ObservedObject var motion: MotionController
    var isDashboard = false
    /// Layout tests measure the natural height instead of forcing the fixed one.
    var measuring = false

    var body: some View {
        let panel = VStack(alignment: .leading, spacing: 0) {
            header
            Divider()
            VStack(alignment: .leading, spacing: 8) {
                NoticeRow(model: model, updates: updates)
                content
                Spacer(minLength: 0)
            }
            .padding(.horizontal, 12)
            .padding(.vertical, 8)
            Divider()
            footer
        }
        .frame(width: Layout.width)
        if measuring { panel } else { panel.frame(height: Layout.height, alignment: .top).clipped() }
    }

    // MARK: Header

    /// Hierarchy: brand mark, then the work status (primary) with its counts (secondary), then the quiet actions.
    private var header: some View {
        VStack(spacing: 6) {
            HStack(spacing: 6) {
                BrandMark(size: 17)
                StatusSummary(model: model, motion: motion.header)
                NewTaskPulseBadge(pulse: model.pulse)
                Spacer(minLength: 2)
                ProfileMenu(profiles: profiles)
                Button { AppState.shared.newChat() } label: { Image(systemName: "plus.bubble") }
                    .buttonStyle(.borderless)
                    .help("New Chat")
                    .accessibilityLabel("New Chat")
                Button { model.refreshNow() } label: {
                    ZStack {
                        Image(systemName: "arrow.clockwise").opacity(model.isLoading ? 0 : 1)
                        if model.isLoading { ProgressView().controlSize(.mini) }
                    }
                    .frame(width: 16, height: 18)
                }
                    .buttonStyle(.borderless)
                    .help("Refresh now")
                    .accessibilityLabel("Refresh now")
                    .disabled(!model.hasCLI)
                Button { AppState.shared.showSettings() } label: { Image(systemName: "gearshape") }
                    .buttonStyle(.borderless)
                    .help("Settings")
                    .accessibilityLabel("Settings")
            }
            PanelSegments("Period", values: StatsPeriod.allCases, selection: $model.period) { $0.label }
        }
        .padding(.horizontal, 12)
        .padding(.vertical, 8)
    }

    @ViewBuilder private var content: some View {
        if let s = model.snapshot {
            SnapshotView(snapshot: s)
        } else if model.lastError == nil {
            HStack { ProgressView().controlSize(.small); Text("Loading…").font(.caption).foregroundStyle(.secondary) }
                .frame(maxWidth: .infinity, minHeight: 120)
        } else {
            Text("No data to show yet. Details are in the notice above.")
                .font(.caption).foregroundStyle(.secondary)
                .frame(maxWidth: .infinity, minHeight: 120)
        }
    }

    // MARK: Footer

    private var footer: some View {
        HStack(spacing: 8) {
            Text("v\(updates.currentVersionText)").font(.system(size: 10)).foregroundStyle(.secondary)
            if case .upToDate(let at) = updates.phase {
                Text("Up to date · \(at.formatted(date: .omitted, time: .shortened))")
                    .font(.system(size: 10)).foregroundStyle(.secondary).lineLimit(1)
            }
            Spacer()
            if !isDashboard {
                Button("Dashboard") { AppState.shared.showDashboard() }
            }
            Button("Quit") { NSApp.terminate(nil) }
                .keyboardShortcut("q")
        }
        .controlSize(.small)
        .padding(.horizontal, 12)
        .padding(.vertical, 6)
    }
}

// MARK: - Pieces

/// Equal-width segments avoid AppKit's intrinsic picker width crowding the fixed panel.
struct PanelSegments<Selection: Hashable>: View {
    let title: String
    let values: [Selection]
    @Binding var selection: Selection
    let label: (Selection) -> String

    init(_ title: String, values: [Selection], selection: Binding<Selection>, label: @escaping (Selection) -> String) {
        self.title = title
        self.values = values
        self._selection = selection
        self.label = label
    }

    var body: some View {
        HStack(spacing: 2) {
            ForEach(values, id: \.self) { value in
                Button { selection = value } label: {
                    Text(label(value)).font(.system(size: 10, weight: selection == value ? .semibold : .regular))
                        .lineLimit(1).minimumScaleFactor(0.85)
                        .frame(maxWidth: .infinity)
                        .padding(.vertical, 3)
                        .foregroundStyle(selection == value ? Color.white : .primary)
                        .background(selection == value ? Color.omBlue : .clear, in: RoundedRectangle(cornerRadius: 5))
                        .contentShape(Rectangle())
                }
                .buttonStyle(.plain)
                .accessibilityAddTraits(selection == value ? .isSelected : [])
            }
        }
        .padding(2)
        .background(.primary.opacity(0.06), in: RoundedRectangle(cornerRadius: 7))
        .accessibilityElement(children: .contain)
        .accessibilityLabel(title)
    }
}

/// The primary header element: a state symbol and title, with the counts below. The symbol shape differs
/// per state, so the state does not depend on color. The whole element reads as one accessibility value.
struct StatusSummary: View {
    @ObservedObject var model: MonitorModel
    @ObservedObject var motion: MotionModel
    /// Width of the text column. The header row has about this much room next to the actions.
    static let textWidth: CGFloat = 112

    var body: some View {
        // Re-evaluated on every poll result; the clock only matters once a refresh has failed.
        TimelineView(.periodic(from: .now, by: 5)) { ctx in
            let status = model.status(at: ctx.date)
            VStack(alignment: .leading, spacing: 1) {
                HStack(spacing: 4) {
                    if let symbol = status.symbol {
                        StatusSymbolView(name: symbol, motion: motion).font(.system(size: 11, weight: .semibold)).foregroundStyle(tint(status))
                    }
                    Text(status.title).font(.system(size: 12.5, weight: .semibold)).lineLimit(1).minimumScaleFactor(0.8)
                }
                Text(status.detail)
                    .font(.system(size: 10)).foregroundStyle(.secondary)
                    .lineLimit(1).minimumScaleFactor(0.8)
            }
            .frame(width: Self.textWidth, alignment: .leading)
            .help(status.accessibilityValue)
            .accessibilityElement(children: .ignore)
            .accessibilityLabel("Offload status")
            .accessibilityValue(status.accessibilityValue)
        }
        // The panel only animates while a view is on screen; closing it cancels the pending timer.
        .onAppear { motion.viewAppeared() }
        .onDisappear { motion.viewDisappeared() }
    }

    private func tint(_ s: OffloadStatus) -> Color {
        switch s.kind {
        case .attention, .unavailable, .stale: return .orange
        case .working: return .omTeal
        case .queued: return .omBlue
        case .idle, .unknown: return .secondary
        }
    }
}

/// The profile new runs use, as a compact header menu. Choosing one makes it the saved default.
struct ProfileMenu: View {
    @ObservedObject var profiles: ProfilesModel

    var body: some View {
        Menu {
            if let s = profiles.snapshot {
                if profiles.isStale {
                    Text("Profile list may be out of date: the last refresh failed").font(.caption)
                    Button("Retry") { profiles.refresh() }
                    Divider()
                }
                Text("Used for new runs").font(.caption)
                ForEach(s.profiles) { p in
                    Button {
                        Task { await profiles.use(p.name) }
                    } label: {
                        let t = p.model.isEmpty ? p.name : "\(p.name) — \(p.model)"
                        if p.name == s.active { Label(t, systemImage: "checkmark") } else { Text(t) }
                    }
                }
                if s.overriddenByEnvironment {
                    Divider()
                    Text("OFFLOAD_PROFILE overrides the saved default").font(.caption)
                }
                Divider()
                Button("Profile settings…") { AppState.shared.showSettings() }
            } else {
                Text(profiles.loadError ?? "Loading profiles…")
                Button("Retry") { profiles.refresh() }
            }
        } label: {
            HStack(spacing: 3) {
                Image(systemName: "person.crop.circle").font(.system(size: 10))
                Text(title).font(.system(size: 10.5)).lineLimit(1).truncationMode(.middle)
            }
        }
        .menuStyle(.borderlessButton)
        .menuIndicator(.visible)
        .frame(width: 104, alignment: .trailing)
        .disabled(profiles.busy)
        .help(help)
        .onAppear { profiles.refreshIfStale() }
    }

    private var title: String {
        guard let s = profiles.snapshot else { return profiles.loadError == nil ? "Profile…" : "Profile ⚠" }
        if s.active.isEmpty { return "No profile" }
        let name = s.overriddenByEnvironment ? "\(s.active) (env)" : s.active
        return profiles.isStale ? "\(name) ⚠" : name
    }

    private var help: String {
        if let s = profiles.snapshot {
            let stale = profiles.isStale ? "Unavailable: the profile list could not be refreshed (\(profiles.loadError ?? "unknown error")). Showing the last known state.\n" : ""
            return stale + s.activeDescription + (profiles.actionError.map { "\n" + $0 } ?? "")
        }
        return profiles.loadError ?? "Loading profiles…"
    }
}

struct Banner: View {
    var symbol: String
    var tint: Color
    var title: String
    var message: String
    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: symbol).foregroundStyle(tint)
            VStack(alignment: .leading, spacing: 2) {
                Text(title).font(.callout.weight(.semibold))
                if !message.isEmpty {
                    Text(message).font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                }
            }
            Spacer(minLength: 0)
        }
        .padding(10)
        .background(tint.opacity(0.12), in: RoundedRectangle(cornerRadius: 10))
    }
}

/// Full update status and actions, for the notices window. Preferences live in Settings > App.
struct UpdateBanner: View {
    @ObservedObject var updates: UpdateManager

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            if let n = updates.localNotice {
                Banner(symbol: n.symbol, tint: n.tint, title: n.title, message: n.message)
                Button("Restart App When Idle") { updates.restartInstalledApp() }.controlSize(.small)
            }
            if let n = updates.resultNotice {
                Banner(symbol: n.ok ? "checkmark.circle.fill" : "xmark.octagon.fill", tint: n.ok ? .green : .red,
                       title: n.ok ? "Update installed" : "Update failed", message: n.message)
                    .onTapGesture { updates.dismissNotice() }
            }
            switch updates.phase {
            case .idle, .upToDate: EmptyView()
            case .checking: Banner(symbol: "arrow.triangle.2.circlepath", tint: .omBlue, title: "Checking for updates…", message: "")
            case .available(let c):
                Banner(symbol: "arrow.down.circle.fill", tint: .omBlue, title: "Version \(c.version) is available", message: "You have \(updates.currentVersionText).")
                Button("Download Update") { updates.downloadAndPrepare() }.controlSize(.small)
            case .downloading(let c): Banner(symbol: "arrow.down.circle", tint: .omBlue, title: "Downloading \(c.version)…", message: "The download is verified before it is used.")
            case .ready(let c):
                Banner(symbol: "checkmark.seal.fill", tint: .green, title: "Version \(c.version) is verified and ready",
                       message: updates.restartWaitReason ?? "Installs the whole App and bundled CLI when idle.")
                Button("Update and Restart") { updates.installAndRestart() }.controlSize(.small)
            case .installing(let c): Banner(symbol: "arrow.triangle.2.circlepath", tint: .omBlue, title: "Installing \(c.version)…", message: "")
            case .failed(let m): Banner(symbol: "exclamationmark.triangle.fill", tint: .red, title: "Update problem", message: m + " The current version keeps running.")
            }
        }
    }
}

/// What the one-row notice slot says, most important first. The slot has a fixed height; the notices window shows all of them.
struct NoticeItem: Equatable {
    var symbol: String
    var tint: Color
    var title: String
    var message: String
    enum Action: Equatable { case none, download, restart, reloadApp }
    var action: Action = .none
}

enum Notices {
    static func items(error: String?, hasSnapshot: Bool, isStale: Bool, phase: UpdateManager.Phase,
                      result: UpdateReport?, currentVersion: String) -> [NoticeItem] {
        var out: [NoticeItem] = []
        if let error {
            out.append(NoticeItem(symbol: "exclamationmark.triangle.fill", tint: .orange,
                                  title: !hasSnapshot ? "Cannot read stats" : (isStale ? "Showing stale data" : "Last refresh failed"), message: error))
        }
        if let r = result {
            out.append(NoticeItem(symbol: r.ok ? "checkmark.circle.fill" : "xmark.octagon.fill", tint: r.ok ? .green : .red,
                                  title: r.ok ? "Update installed" : "Update failed", message: r.message))
        }
        switch phase {
        case .idle, .upToDate: break
        case .checking: out.append(NoticeItem(symbol: "arrow.triangle.2.circlepath", tint: .omBlue, title: "Checking for updates…", message: ""))
        case .available(let c):
            out.append(NoticeItem(symbol: "arrow.down.circle.fill", tint: .omBlue, title: "Version \(c.version) is available",
                                  message: "You have \(currentVersion).", action: .download))
        case .downloading(let c): out.append(NoticeItem(symbol: "arrow.down.circle", tint: .omBlue, title: "Downloading \(c.version)…", message: "Verified before use."))
        case .ready(let c):
            out.append(NoticeItem(symbol: "checkmark.seal.fill", tint: .green, title: "Version \(c.version) is verified",
                                  message: "The app will relaunch.", action: .restart))
        case .installing(let c): out.append(NoticeItem(symbol: "arrow.triangle.2.circlepath", tint: .omBlue, title: "Installing \(c.version)…", message: ""))
        case .failed(let m): out.append(NoticeItem(symbol: "exclamationmark.triangle.fill", tint: .red, title: "Update problem", message: m))
        }
        return out
    }
}

struct NoticeRow: View {
    @ObservedObject var model: MonitorModel
    @ObservedObject var updates: UpdateManager

    var body: some View {
        TimelineView(.periodic(from: .now, by: 5)) { ctx in
            let base = Notices.items(error: model.lastError, hasSnapshot: model.snapshot != nil,
                                      isStale: model.isStale(at: ctx.date), phase: updates.phase,
                                      result: updates.resultNotice, currentVersion: updates.currentVersionText)
            let items = updates.localNotice.map { [$0] + base } ?? base
            if let first = items.first {
                HStack(spacing: 6) {
                    Image(systemName: first.symbol).foregroundStyle(first.tint).font(.system(size: 12))
                    VStack(alignment: .leading, spacing: 0) {
                        Text(first.title).font(.system(size: 10.5, weight: .semibold)).lineLimit(1)
                        if !first.message.isEmpty {
                            Text(first.message).font(.system(size: 9.5)).foregroundStyle(.secondary).lineLimit(1)
                        }
                    }
                    Spacer(minLength: 4)
                    switch first.action {
                    case .download: Button("Download") { updates.downloadAndPrepare() }
                    case .restart: Button("Update and Restart") { updates.installAndRestart() }
                    case .reloadApp: Button("Restart App") { updates.restartInstalledApp() }
                    case .none: EmptyView()
                    }
                    Button(items.count > 1 ? "Details (\(items.count))" : "Details") { AppState.shared.showNotices() }
                }
                .controlSize(.mini)
                .padding(.horizontal, 8)
                .frame(height: Layout.notice)
                .background(first.tint.opacity(0.12), in: RoundedRectangle(cornerRadius: 8))
            }
        }
    }
}

struct Card<Content: View>: View {
    var tint: Color?
    @ViewBuilder var content: Content
    var body: some View {
        content
            .padding(8)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 10))
            .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder((tint ?? .secondary).opacity(0.18)))
    }
}

struct MetricCard: View {
    var title: String
    var value: String
    var subtitle: String
    var tint: Color
    var symbol: String
    @Environment(\.accessibilityReduceMotion) private var reduceMotion

    var body: some View {
        Card(tint: tint) {
            VStack(alignment: .leading, spacing: 1) {
                HStack(spacing: 4) {
                    Image(systemName: symbol).font(.system(size: 9)).foregroundStyle(tint)
                    Text(title).font(.system(size: 10)).foregroundStyle(.secondary)
                }
                // Digits roll when the value changes. This is presentation only: a changed total is
                // never reported as a new task. Reduce Motion switches the animation off.
                Text(value).font(.system(size: 19, weight: .semibold).monospacedDigit()).lineLimit(1).minimumScaleFactor(0.7)
                    .contentTransition(.numericText())
                    .animation(reduceMotion ? nil : .easeOut(duration: 0.3), value: value)
                Text(subtitle).font(.system(size: 9)).foregroundStyle(.secondary).lineLimit(1).minimumScaleFactor(0.85)
            }
        }
    }
}

// MARK: - Snapshot

enum DetailTab: String, CaseIterable, Identifiable {
    case tasks = "Tasks", history = "History", projects = "Projects", models = "Models"
    var id: String { rawValue }
}

struct SnapshotView: View {
    let snapshot: StatsSnapshot
    @State private var tab: DetailTab = .tasks
    @State private var page = 0

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            WorkloadPanel(snapshot: snapshot)
            SummaryStrip(totals: snapshot.totals)
            TrendCard(trend: snapshot.trend)
            PanelSegments("Detail", values: DetailTab.allCases, selection: $tab) { t in
                t == .tasks && !snapshot.active.isEmpty ? "Tasks (\(snapshot.active.count))" : t.rawValue
            }
            .onChange(of: tab) { _ in page = 0 }
            list
            HStack(spacing: 12) {
                Button { AppState.shared.showDetails() } label: { Label("Token and cost detail", systemImage: "chart.bar.doc.horizontal") }
                if !snapshot.warnings.isEmpty {
                    Button { AppState.shared.showNotes() } label: { Label("Data coverage and notes (\(snapshot.warnings.count))", systemImage: "info.circle") }
                }
                Spacer()
            }
            .buttonStyle(.link)
            .font(.system(size: 10))
            .lineLimit(1)
            .frame(height: 16)
        }
    }

    private var list: some View {
        VStack(spacing: 0) {
            switch tab {
            case .tasks:
                pagedList(snapshot.active, size: Layout.taskPageSize, empty: "No active tasks.") { ActiveRow(task: $0) }
            case .history:
                pagedList(snapshot.recent, size: Layout.rowPageSize, empty: "No finished runs in this period.") { RecentRow(run: $0) }
            case .projects:
                pagedList(snapshot.projects, size: Layout.rowPageSize, empty: "No projects in this period.") { GroupRow(group: $0) }
            case .models:
                pagedList(snapshot.models, size: Layout.rowPageSize, empty: "No models in this period.") { GroupRow(group: $0) }
            }
        }
        .frame(height: Layout.list, alignment: .top)
    }

    private func pagedList<T, Row: View>(_ items: [T], size: Int, empty: String, @ViewBuilder row: @escaping (T) -> Row) -> some View {
        let p = Pagination.clamp(page, total: items.count, size: size)
        let pages = Pagination.pageCount(total: items.count, size: size)
        return VStack(spacing: 0) {
            if items.isEmpty {
                VStack(spacing: 6) {
                    Image(systemName: tab == .tasks ? "checkmark.circle" : "tray")
                        .font(.system(size: 20, weight: .light)).foregroundStyle(tab == .tasks ? Color.omTeal : .secondary)
                    Text(empty).font(.system(size: 11, weight: .medium)).foregroundStyle(.secondary)
                    if tab == .tasks {
                        Button("New Chat") { AppState.shared.newChat() }
                    } else if tab == .history {
                        Button("Browse all conversations") { AppState.shared.browseHistory() }
                    }
                }
                .buttonStyle(.borderless)
                .controlSize(.small)
                .frame(maxWidth: .infinity, maxHeight: .infinity)
                .background(.quaternary.opacity(0.3), in: RoundedRectangle(cornerRadius: 10))
            } else {
                VStack(spacing: 3) {
                    // Items are identified by position within the page; the pages are rebuilt on every data change.
                    ForEach(Array(Pagination.slice(items, page: p, size: size).enumerated()), id: \.offset) { _, item in row(item) }
                }
                Spacer(minLength: 0)
                HStack(spacing: 6) {
                    Text(Pagination.rangeLabel(page: p, total: items.count, size: size)).font(.system(size: 9)).foregroundStyle(.secondary)
                    Spacer()
                    if tab == .history {
                        Button("Browse all") { AppState.shared.browseHistory() }
                            .help("Search and open every conversation")
                    }
                    if pages > 1 {
                        Button { page = p - 1 } label: { Image(systemName: "chevron.left") }.disabled(p == 0).help("Previous page")
                        Button { page = p + 1 } label: { Image(systemName: "chevron.right") }.disabled(p >= pages - 1).help("Next page")
                    }
                }
                .buttonStyle(.borderless)
                .controlSize(.mini)
                .frame(height: Layout.pager)
            }
        }
        .onChange(of: items.count) { _ in page = Pagination.clamp(page, total: items.count, size: size) }
    }
}

struct MetricGrid: View {
    let totals: StatsTotals

    var body: some View {
        Grid(horizontalSpacing: 6, verticalSpacing: 6) {
            GridRow {
                MetricCard(title: "Runs", value: "\(totals.runs)",
                           subtitle: "\(totals.ok) succeeded · \(totals.error + totals.timeout) failed", tint: .omBlue, symbol: "play.circle")
                MetricCard(title: "Tokens", value: totals.usage.map { Format.count($0.inputTokens + $0.outputTokens) } ?? "—",
                           subtitle: (totals.usage == nil ? "Not recorded · " : "In + out · ") + "\(totals.usageRuns) of \(totals.runs) runs",
                           tint: .omTeal, symbol: "text.word.spacing")
            }
            GridRow {
                MetricCard(title: "Estimated cost", value: totals.costUsd.map(Format.cost) ?? "—",
                           subtitle: (totals.costUsd == nil ? "Not recorded · " : "") + "\(totals.costRuns) of \(totals.runs) runs",
                           tint: .omPurple, symbol: "dollarsign.circle")
                MetricCard(title: "Sessions", value: "\(totals.sessions)", subtitle: "Distinct agent sessions", tint: .omBlue, symbol: "square.stack")
            }
        }
    }
}

// MARK: - Trend

struct TrendCard: View {
    let trend: Trend
    @State private var metric: TrendMetric = .runs
    @State private var selected: String?

    var body: some View {
        Card(tint: metric.tint) {
            VStack(alignment: .leading, spacing: 4) {
                HStack(spacing: 6) {
                    Text("Trend").font(.system(size: 11, weight: .semibold))
                    if !trend.label.isEmpty { Text(trend.label).font(.system(size: 9)).foregroundStyle(.secondary).lineLimit(1) }
                    Spacer()
                    PanelSegments("Metric", values: TrendMetric.allCases, selection: $metric) { $0.label }
                    .frame(width: 150)
                }
                if let message = TrendData.emptyMessage(trend, metric: metric) {
                    Text(message).font(.system(size: 10)).foregroundStyle(.secondary)
                        .frame(maxWidth: .infinity, minHeight: Layout.chart, maxHeight: Layout.chart)
                } else {
                    chart
                }
                detail
            }
        }
    }

    private var bars: [TrendBar] { TrendData.bars(trend, metric: metric) }
    private var selectedID: String? {
        if let s = selected, trend.points.contains(where: { $0.start == s }) { return s }
        return TrendData.defaultSelection(trend)
    }

    private var chart: some View {
        let bars = self.bars
        let sel = selectedID
        let unit: Calendar.Component = trend.interval == "hour" ? .hour : .day
        let domain = TrendData.xDomain(trend)
        return Chart(bars) { bar in
            if let v = bar.value {
                BarMark(x: .value("Time", bar.date, unit: unit), y: .value(metric.label, v))
                    .foregroundStyle(metric.tint.opacity(bar.id == sel ? 1 : 0.45))
                    .cornerRadius(2)
            }
        }
        .chartXScale(domain: domain ?? Date()...Date().addingTimeInterval(3600))
        .chartXAxis {
            AxisMarks(values: .automatic(desiredCount: 4)) { _ in
                AxisTick()
                AxisValueLabel(format: TrendData.axisFormat(interval: trend.interval)).font(.system(size: 8))
            }
        }
        .chartYAxis {
            AxisMarks(position: .leading, values: yAxisValues) { value in
                AxisGridLine()
                AxisValueLabel {
                    if let d = value.as(Double.self) { Text(metric.formatted(d)).font(.system(size: 8)) }
                }
            }
        }
        .chartOverlay { proxy in
            GeometryReader { geo in
                Rectangle().fill(.clear).contentShape(Rectangle())
                    .gesture(DragGesture(minimumDistance: 0).onChanged { drag in
                        let x = drag.location.x - geo[proxy.plotAreaFrame].origin.x
                        if let date: Date = proxy.value(atX: x), let hit = TrendData.containing(date, in: bars) {
                            selected = hit.id
                        }
                    })
            }
        }
        .frame(height: Layout.chart)
    }

    private var yAxisValues: AxisMarkValues {
        if metric == .runs {
            // Calls are integers: a half-call tick would otherwise format as another zero.
            return .stride(by: max(1, ceil((bars.compactMap(\.value).max() ?? 1) / 2)))
        }
        return .automatic(desiredCount: 3)
    }

    @ViewBuilder private var detail: some View {
        if let id = selectedID, let p = trend.points.first(where: { $0.start == id }) {
            let t = p.totals
            VStack(alignment: .leading, spacing: 0) {
                HStack {
                    Text(p.label).font(.system(size: 10, weight: .semibold))
                    Spacer()
                    Text(metric.exact(t) ?? "—").font(.system(size: 10, weight: .semibold).monospacedDigit())
                }
                Text(metric.coverage(t) + (metric == .runs ? "" : " · \(t.runs) run\(t.runs == 1 ? "" : "s") in bucket"))
                    .font(.system(size: 9)).foregroundStyle(.secondary).lineLimit(1).minimumScaleFactor(0.85)
            }
            .frame(height: 26, alignment: .top)
        }
    }
}

// MARK: - Rows

struct ActiveRow: View {
    let task: ActiveTask

    var body: some View {
        let t = task
        VStack(alignment: .leading, spacing: 0) {
            HStack(spacing: 4) {
                Text(t.projectName).font(.system(size: 11, weight: .medium)).lineLimit(1)
                if t.isLost { tag("lost", .red) } else if t.stalled { tag("stalled", .orange) }
                if t.phase == "salvaging" { tag("salvaging", .omBlue) }
                Spacer()
                if t.isRunning, let start = Format.parseTimestamp(t.startedAt) {
                    TimelineView(.periodic(from: .now, by: 1)) { ctx in
                        Text(Format.duration(ctx.date.timeIntervalSince(start))).font(.system(size: 10).monospacedDigit()).foregroundStyle(.secondary)
                    }
                }
            }
            Text(t.task).font(.system(size: 10)).lineLimit(1).truncationMode(.tail)
            Text(footnote).font(.system(size: 9)).foregroundStyle(.secondary).lineLimit(1)
        }
        .padding(.horizontal, 8)
        .frame(maxWidth: .infinity, minHeight: Layout.taskRow, maxHeight: Layout.taskRow, alignment: .leading)
        .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 7))
        .overlay(RoundedRectangle(cornerRadius: 7).strokeBorder((t.isLost || t.stalled ? Color.orange : Color.omTeal).opacity(0.3)))
    }

    private var footnote: String {
        if task.isLost { return "No longer updating; offload may have been killed." }
        let tool = (task.lastTool ?? "").isEmpty ? "no tool call yet" : task.lastTool!
        return "\(task.model) · \(tool)"
    }

    private func tag(_ s: String, _ c: Color) -> some View {
        Text(s).font(.system(size: 9, weight: .semibold)).padding(.horizontal, 4).padding(.vertical, 1)
            .background(c.opacity(0.2), in: Capsule()).foregroundStyle(c)
    }
}

struct GroupRow: View {
    let group: StatsGroup

    var body: some View {
        HStack {
            Text(group.name).font(.system(size: 10.5)).lineLimit(1)
            Spacer()
            Text("\(group.totals.runs) run\(group.totals.runs == 1 ? "" : "s")").font(.system(size: 10).monospacedDigit()).foregroundStyle(.secondary)
            Text(group.totals.costUsd.map(Format.cost) ?? "no cost data").font(.system(size: 10).monospacedDigit()).foregroundStyle(.secondary)
                .frame(minWidth: 64, alignment: .trailing)
        }
        .padding(.horizontal, 8)
        .frame(height: Layout.groupRow)
        .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 7))
    }
}

/// A finished run. Clicking opens the full report in its own window.
struct RecentRow: View {
    let run: RecentRun

    var body: some View {
        Button { AppState.shared.openChat(for: run) } label: {
            HStack(spacing: 6) {
                Image(systemName: icon.0).foregroundStyle(icon.1).font(.system(size: 11))
                VStack(alignment: .leading, spacing: 0) {
                    Text(run.task).font(.system(size: 10.5)).lineLimit(1)
                    Text("\(run.projectName) · \(run.ts)\(run.salvaged ? " · salvaged" : "")")
                        .font(.system(size: 9)).foregroundStyle(.secondary).lineLimit(1)
                }
                Spacer(minLength: 0)
                Image(systemName: "arrow.up.right.square").font(.system(size: 10)).foregroundStyle(.secondary)
            }
            .padding(.horizontal, 8)
            .frame(height: Layout.historyRow, alignment: .leading)
            .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 7))
            .contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .help("Open this conversation")
    }

    private var icon: (String, Color) {
        switch run.status {
        case "ok": return ("checkmark.circle.fill", .green)
        case "timeout": return ("clock.badge.exclamationmark.fill", .orange)
        default: return ("xmark.circle.fill", .red)
        }
    }
}
