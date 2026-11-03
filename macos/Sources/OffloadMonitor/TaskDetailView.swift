import SwiftUI

struct TaskSnapshot: Decodable, Equatable {
    var schemaVersion: Int
    var runId: String
    var caller: String
    var phase: String
    var status: String?
    var task: String?
    var result: String?
    var active: Bool
    var canStop: Bool
    var canResume: Bool
    var resumeReason: String
    var resumePending: Bool
    var resumedFrom: String?
    var resumedBy: String?
}

struct TaskOutputEvent: Decodable, Identifiable, Equatable {
    var seq: Int
    var type: String
    var role: String?
    var text: String?
    var toolName: String?
    var arguments: String?
    var output: String?
    var reason: String?
    var status: String?
    var truncated: [String: Int]?
    var isError: Bool?
    var exitCode: Int?
    var id: Int { seq }
    var title: String {
        switch type {
        case "message": return role == "user" ? "You" : "Assistant"
        case "tool_call": return toolName.map { "Running · \($0)" } ?? "Tool call"
        case "tool_result": return isError == true ? "Tool failed" : "Tool output"
        case "backend_error": return "Error"
        case "gap": return "Output unavailable"
        case "execution_end": return "Task finished"
        default: return type.replacingOccurrences(of: "_", with: " ").capitalized
        }
    }
    var content: String { text ?? output ?? arguments ?? reason ?? status ?? "" }
}

struct TaskTranscript: Decodable {
    var schemaVersion: Int
    var availability: String
    var reason: String?
    var events: [TaskOutputEvent]
    var nextCursor: String?
    var hasMore: Bool
    var completeness: String?
    var completenessReasons: [String]?
}

extension OffloadCLI {
    static func taskDecode<T: Decodable>(_ type: T.Type, _ data: Data) throws -> T {
        let decoder = JSONDecoder()
        decoder.keyDecodingStrategy = .convertFromSnakeCase
        return try decoder.decode(type, from: data)
    }

    func task(_ action: String, id: String) async throws -> TaskSnapshot {
        guard ["show", "stop", "resume"].contains(action), Self.validRunID(id) else {
            throw CLIError.invalidArgument("Invalid task control request.")
        }
        let value = try Self.taskDecode(TaskSnapshot.self, await run(["task", action, "--id", id, "--json"]))
        guard value.schemaVersion == 1, Self.validRunID(value.runId) else { throw CLIError.badOutput("task schema") }
        return value
    }

    static func validRunID(_ id: String) -> Bool {
        id.utf8.count == 32 && id.utf8.allSatisfy { (48...57).contains($0) || (97...102).contains($0) }
    }

    func taskOutput(id: String, cursor: String) async throws -> TaskTranscript {
        guard Self.validRunID(id) else { throw CLIError.invalidArgument("Invalid task identity.") }
        let value = try Self.taskDecode(TaskTranscript.self, await run([
            "transcript", "read", "--run-id", id, "--json", "--cursor", cursor,
            "--limit", "100", "--max-bytes", "262144"
        ]))
        guard value.schemaVersion == 1 else { throw CLIError.badOutput("transcript schema") }
        return value
    }
}

/// Observation never owns the worker. Closing the view cancels reads only.
@MainActor final class TaskDetailModel: ObservableObject {
    @Published var runID: String
    @Published var snapshot: TaskSnapshot?
    @Published var events: [TaskOutputEvent] = []
    @Published var coverage = "Waiting for captured output."
    @Published var error: String?
    @Published var busy = false
    @Published var trimmed = false
    private var moreOutput = false
    private var reading = false
    private var followingResume = false
    private var cursor = ""
    private var lastSeq = 0
    private var retainedBytes = 0
    private let cli: OffloadCLI?

    init(runID: String, cli: OffloadCLI? = StatsClient.locateCLI().map { OffloadCLI(executable: $0) }) {
        self.runID = runID; self.cli = cli
    }

    func observe() async {
        while !Task.isCancelled {
            if !busy { await refresh() }
            if snapshot?.phase == "done", snapshot?.resumePending != true, !moreOutput { return }
            do { try await Task.sleep(nanoseconds: 1_000_000_000) } catch { return }
        }
    }

    func refresh() async {
        guard let cli else { error = CLIError.unavailable.localizedDescription; return }
        guard !reading else { return }
        reading = true
        defer { reading = false }
        let id = runID
        do {
            let state = try await cli.task("show", id: id)
            guard !Task.isCancelled, id == runID else { return }
            snapshot = state
            // A slow caller can register after the resume acknowledgement.
            if followingResume, state.resumePending == false, let next = state.resumedBy, next != id {
                follow(next); return
            }
            for _ in 0..<4 {
                let page = try await cli.taskOutput(id: id, cursor: cursor)
                guard !Task.isCancelled, id == runID else { return }
                ingest(page)
                if !page.hasMore { break }
            }
            error = nil
        } catch { if !Task.isCancelled { self.error = error.localizedDescription } }
    }

    func ingest(_ page: TaskTranscript) {
        moreOutput = page.hasMore
        if page.availability == "cursor_invalid" {
            cursor = ""; lastSeq = 0; events = []; retainedBytes = 0
        } else {
            for event in page.events where event.seq > lastSeq {
                lastSeq = event.seq
                if !event.content.isEmpty {
                    events.append(event); retainedBytes += event.content.utf8.count
                }
            }
            while events.count > 1000 || retainedBytes > 1_048_576 {
                retainedBytes -= events.removeFirst().content.utf8.count; trimmed = true
            }
            if let next = page.nextCursor { cursor = next }
        }
        coverage = page.reason ?? ([page.completeness ?? page.availability] + (page.completenessReasons ?? [])).joined(separator: " · ")
    }

    func control(_ action: String) async {
        guard !busy, let cli else { return }
        guard action == "stop" ? snapshot?.canStop == true : (action == "resume" && snapshot?.canResume == true) else { return }
        busy = true
        defer { busy = false }
        while reading {
            do { try await Task.sleep(nanoseconds: 10_000_000) } catch { return }
        }
        do {
            let state = try await cli.task(action, id: runID)
            if state.runId != runID { follow(state.runId) }
            followingResume = action == "resume" && state.resumePending
            snapshot = state; error = nil
        } catch { self.error = error.localizedDescription }
    }

    func follow(_ id: String) {
        followingResume = false; moreOutput = false
        runID = id; snapshot = nil; cursor = ""; lastSeq = 0
        events = []; retainedBytes = 0; trimmed = false
    }
}

struct TaskDetailView: View {
    @StateObject private var model: TaskDetailModel
    init(runID: String) { _model = StateObject(wrappedValue: TaskDetailModel(runID: runID)) }
    init(model: TaskDetailModel) { _model = StateObject(wrappedValue: model) }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text("Caller: \(model.snapshot?.caller ?? "unknown")").font(.headline)
                Spacer()
                Text(model.snapshot?.status ?? model.snapshot?.phase ?? "Loading…").foregroundStyle(.secondary)
                Button("Stop", role: .destructive) { Task { await model.control("stop") } }
                    .disabled(model.busy || model.snapshot?.canStop != true)
                Button(model.snapshot?.resumePending == true ? "Resuming…" : "Resume") { Task { await model.control("resume") } }
                    .disabled(model.busy || model.snapshot?.canResume != true)
            }
            Text(model.runID).font(.caption.monospaced()).textSelection(.enabled)
            if let from = model.snapshot?.resumedFrom { Text("Resumed from \(from)").font(.caption).textSelection(.enabled) }
            if let task = model.snapshot?.task { Text(task).lineLimit(3) }
            Text(model.snapshot?.resumeReason ?? "Loading task state…").font(.caption).foregroundStyle(.secondary)
            if let next = model.snapshot?.resumedBy {
                Button("Open resumed task") { model.follow(next) }.disabled(model.busy)
            }
            Divider()
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 12) {
                    if model.trimmed { Text("Earlier output was trimmed from this view. The stored transcript is unchanged.").foregroundStyle(.orange) }
                    ForEach(model.events) { event in
                        VStack(alignment: .leading, spacing: 3) {
                            HStack(spacing: 6) {
                                Image(systemName: event.type == "message" ? (event.role == "user" ? "person.circle" : "sparkle") : (event.isError == true || event.type == "backend_error" ? "exclamationmark.circle" : "terminal"))
                                Text(event.title)
                                if let code = event.exitCode { Text("exit \(code)") }
                            }
                            .font(.caption.bold())
                            .foregroundStyle(event.isError == true || event.type == "backend_error" ? Color.red : Color.secondary)
                            if event.type == "message" {
                                MarkdownView(event.content)
                            } else {
                                Text(event.content).font(.system(size: 12, design: .monospaced))
                                    .textSelection(.enabled)
                                    .frame(maxWidth: .infinity, alignment: .leading)
                                    .padding(8)
                                    .background(Color.primary.opacity(0.04), in: RoundedRectangle(cornerRadius: 6))
                            }
                            if event.truncated?.isEmpty == false { Text("Content truncated by capture limits.").font(.caption).foregroundStyle(.orange) }
                        }
                        .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    if model.events.isEmpty { Text("No captured output is available yet.").foregroundStyle(.secondary) }
                }.padding(4)
            }
            Text(model.coverage).font(.caption).foregroundStyle(.secondary)
            if let error = model.error { Text(error).font(.caption).foregroundStyle(.red) }
        }
        .padding(16).frame(minWidth: 500, minHeight: 350)
        .task(id: model.runID) { await model.observe() }
    }
}
