import Foundation

// MARK: - `offload history list/show --json`

struct ConversationSummary: Codable, Equatable, Identifiable {
    var id: String
    var session: String
    var title: String
    var dir: String
    var profile: String
    var backend: String
    var model: String
    var startedAt: String
    var updatedAt: String
    var runCount: Int
    var status: String
    var active: Bool
    var usage: TokenUsage?
    var usageRuns: Int
    var costUsd: Double?
    var costRuns: Int

    var projectName: String { dir.isEmpty ? "" : ((dir as NSString).lastPathComponent.isEmpty ? dir : (dir as NSString).lastPathComponent) }
    /// Provider session ids are conversation ids; early failures have an opaque id and no session to resume.
    var canResume: Bool { !session.isEmpty }
}

struct HistoryList: Codable, Equatable {
    var schemaVersion: Int
    var total: Int
    var offset: Int
    var limit: Int
    var hasMore: Bool
    var conversations: [ConversationSummary]

    static func decode(_ data: Data) throws -> HistoryList {
        try HistoryCoding.decode(HistoryList.self, from: data, what: "history list")
    }
}

struct ConversationRun: Codable, Equatable, Identifiable {
    var id: String
    var ts: String
    var status: String
    var exitCode: Int?
    var task: String
    var result: String
    var error: String
    var dir: String
    var profile: String
    var backend: String
    var model: String
    var session: String
    var usage: TokenUsage?
    var costUsd: Double?
    var salvaged: Bool
    var elapsedMs: Int64?
    var turns: Int
}

/// The live worker state attached to a conversation. Every field except `pid` and `phase` may be missing.
struct LiveInfo: Codable, Equatable {
    var pid: Int
    var phase: String
    var session: String?
    var lastTool: String?
    var toolCalls: Int?
    var startedAt: String?
    var isRunning: Bool { phase == "running" || phase == "salvaging" }
}

struct HistoryDetail: Codable, Equatable {
    var schemaVersion: Int
    var conversation: ConversationSummary
    var runs: [ConversationRun]
    var active: LiveInfo?

    static func decode(_ data: Data) throws -> HistoryDetail {
        try HistoryCoding.decode(HistoryDetail.self, from: data, what: "history")
    }
}

enum HistoryCoding {
    private struct Probe: Decodable { var schemaVersion: Int }

    static func decode<T: Decodable>(_ type: T.Type, from data: Data, what: String) throws -> T {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        do {
            guard try d.decode(Probe.self, from: data).schemaVersion == 1 else { throw CLIError.badOutput("unsupported schema") }
            return try d.decode(T.self, from: data)
        } catch let e as CLIError { throw e } catch { throw CLIError.badOutput(what) }
    }
}

enum HistoryDates {
    private static let local: DateFormatter = {
        let f = DateFormatter()
        f.locale = Locale(identifier: "en_US_POSIX")
        f.dateFormat = "yyyy-MM-dd HH:mm:ss"
        return f
    }()

    /// RFC 3339, or the run log's local "yyyy-MM-dd HH:mm:ss".
    static func parse(_ s: String) -> Date? { Format.parseTimestamp(s) ?? local.date(from: s) }

    static func short(_ s: String, now: Date = Date(), calendar: Calendar = .current) -> String {
        guard let d = parse(s) else { return s }
        if calendar.isDate(d, inSameDayAs: now) { return d.formatted(date: .omitted, time: .shortened) }
        if let y = calendar.date(byAdding: .day, value: -1, to: now), calendar.isDate(d, inSameDayAs: y) { return "Yesterday" }
        return d.formatted(.dateTime.month(.abbreviated).day())
    }
}

// MARK: - History list model

/// Paged, searchable list of conversations. Reads never overlap: a change while one is running is applied
/// after it finishes, and an answer for an older query is discarded.
@MainActor
final class HistoryModel: ObservableObject {
    static let pageSize = 50

    @Published private(set) var items: [ConversationSummary] = []
    @Published private(set) var total = 0
    @Published private(set) var hasMore = false
    @Published private(set) var loading = false
    @Published private(set) var error: String?
    @Published var query = "" { didSet { if query != oldValue { scheduleSearch() } } }

    private let cli: OffloadCLI?
    private var generation = 0
    private var inFlight = false
    private var pending = false
    private var debounce: Task<Void, Never>?
    private let debounceDelay: TimeInterval

    init(cli: OffloadCLI?, debounce: TimeInterval = 0.3) {
        self.cli = cli
        self.debounceDelay = debounce
        if cli == nil { error = CLIError.unavailable.errorDescription }
    }

    init(testingItems: [ConversationSummary], total: Int? = nil, hasMore: Bool = false) {
        cli = nil
        debounceDelay = 0
        items = testingItems
        self.total = total ?? testingItems.count
        self.hasMore = hasMore
    }

    var isAvailable: Bool { cli != nil }

    private func scheduleSearch() {
        debounce?.cancel()
        let delay = debounceDelay
        debounce = Task { [weak self] in
            if delay > 0 { try? await Task.sleep(nanoseconds: UInt64(delay * 1_000_000_000)) }
            guard !Task.isCancelled else { return }
            await self?.reload()
        }
    }

    /// Re-reads the first page for the current query.
    func reload() async {
        generation += 1
        if inFlight { pending = true; return }
        await load(offset: 0, append: false)
    }

    func loadMore() async {
        guard hasMore, !inFlight else { return }
        await load(offset: items.count, append: true)
    }

    private func load(offset: Int, append: Bool) async {
        guard let cli else { return }
        inFlight = true
        loading = true
        let gen = generation
        let q = query
        defer { inFlight = false; loading = false }
        do {
            let page = try await cli.historyList(search: q, offset: offset, limit: Self.pageSize)
            if gen == generation, q == query {
                if append {
                    let known = Set(items.map(\.id))
                    items += page.conversations.filter { !known.contains($0.id) }
                } else {
                    items = page.conversations
                }
                total = page.total
                hasMore = page.hasMore
                error = nil
            }
        } catch {
            if gen == generation { self.error = (error as? LocalizedError)?.errorDescription ?? "The history could not be read." }
        }
        if pending {
            pending = false
            await load(offset: 0, append: false)
        }
    }
}
