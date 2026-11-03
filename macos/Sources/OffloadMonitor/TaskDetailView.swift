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
    var toolCallId: String?
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
                if !event.content.isEmpty || event.type == "tool_call" || event.type == "tool_result" {
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

/// Pair adjacent calls and results only; interleaved events remain in their original order.
struct TaskOutputRow: Identifiable {
    let event: TaskOutputEvent
    let result: TaskOutputEvent?
    var id: Int { event.seq }
    var isTool: Bool { event.type == "tool_call" || event.type == "tool_result" }
    var failed: Bool { event.isError == true || result?.isError == true || (result?.exitCode ?? event.exitCode ?? 0) != 0 }
    var summary: String {
        let value = event.arguments ?? (event.type == "tool_result" ? event.output : nil) ?? ""
        return value.split(whereSeparator: \.isNewline).first.map(String.init) ?? ""
    }

    static func make(_ events: [TaskOutputEvent]) -> [TaskOutputRow] {
        var rows: [TaskOutputRow] = []
        var i = 0
        while i < events.count {
            let event = events[i]
            var result: TaskOutputEvent?
            if event.type == "tool_call", let id = event.toolCallId, !id.isEmpty,
               i + 1 < events.count, events[i + 1].type == "tool_result", events[i + 1].toolCallId == id {
                result = events[i + 1]
                i += 1
            }
            rows.append(TaskOutputRow(event: event, result: result))
            i += 1
        }
        return rows
    }
}

struct TaskToolRow: View {
    let row: TaskOutputRow
    @Binding var expanded: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            Button { expanded.toggle() } label: {
                HStack(alignment: .firstTextBaseline, spacing: 7) {
                    Image(systemName: expanded ? "chevron.down" : "chevron.right").font(.caption2)
                    Image(systemName: row.failed ? "exclamationmark.circle" : "terminal")
                    Text(row.event.toolName ?? "Tool").fontWeight(.semibold)
                    if !row.summary.isEmpty { Text(row.summary).lineLimit(1).foregroundStyle(.secondary) }
                    Spacer(minLength: 4)
                    if row.failed { Text("Failed").foregroundStyle(.red) }
                    else if row.result != nil || row.event.type == "tool_result" { Text("Done").foregroundStyle(.secondary) }
                    else { Text("Started").foregroundStyle(.secondary) }
                    if let code = row.result?.exitCode ?? row.event.exitCode { Text("exit \(code)").foregroundStyle(.secondary) }
                }
                .font(.system(size: 11))
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            .accessibilityLabel("\(row.event.toolName ?? "Tool"), \(expanded ? "collapse" : "expand")")
            if expanded {
                if let args = row.event.arguments, !args.isEmpty { detail("Input", args) }
                if let output = row.result?.output ?? row.event.output, !output.isEmpty { detail("Output", output) }
                if row.event.truncated?.isEmpty == false || row.result?.truncated?.isEmpty == false {
                    Text("Content truncated by capture limits.").font(.caption).foregroundStyle(.orange)
                }
            }
        }
        .padding(10)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(Color.primary.opacity(0.04), in: RoundedRectangle(cornerRadius: 7))
    }

    private func detail(_ title: String, _ content: String) -> some View {
        VStack(alignment: .leading, spacing: 4) {
            Text(title).font(.caption2.weight(.semibold)).foregroundStyle(.secondary)
            Text(content).font(.system(size: 12, design: .monospaced)).textSelection(.enabled)
                .frame(maxWidth: .infinity, alignment: .leading)
        }
    }
}

struct TaskDetailView: View {
    @StateObject private var model: TaskDetailModel
    @State private var expandedTools: Set<Int> = []
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
            HStack {
                Text("Activity").font(.subheadline.weight(.semibold))
                Spacer()
                Button("Expand all") { expandedTools = Set(TaskOutputRow.make(model.events).filter(\.isTool).map(\.id)) }
                Button("Collapse all") { expandedTools.removeAll() }
            }.font(.caption)
            Divider()
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 12) {
                    if model.trimmed { Text("Earlier output was trimmed from this view. The stored transcript is unchanged.").foregroundStyle(.orange) }
                    ForEach(TaskOutputRow.make(model.events)) { row in
                        if row.isTool {
                            TaskToolRow(row: row, expanded: Binding(
                                get: { expandedTools.contains(row.id) },
                                set: { if $0 { expandedTools.insert(row.id) } else { expandedTools.remove(row.id) } }
                            ))
                        } else {
                            VStack(alignment: .leading, spacing: 5) {
                                Label(row.event.title, systemImage: row.event.type == "message" ? (row.event.role == "user" ? "person.circle" : "sparkle") : "info.circle")
                                    .font(.caption.bold()).foregroundStyle(row.event.type == "backend_error" ? Color.red : Color.secondary)
                                if row.event.type == "message" { MarkdownView(row.event.content) }
                                else { Text(row.event.content).textSelection(.enabled) }
                                if row.event.truncated?.isEmpty == false { Text("Content truncated by capture limits.").font(.caption).foregroundStyle(.orange) }
                            }.frame(maxWidth: .infinity, alignment: .leading)
                        }
                    }
                    if model.events.isEmpty { Text("No captured output is available yet.").foregroundStyle(.secondary) }
                }.padding(4)
            }
            Text(model.coverage).font(.caption).foregroundStyle(.secondary)
            if let error = model.error { Text(error).font(.caption).foregroundStyle(.red) }
        }
        .padding(16).frame(minWidth: 500, minHeight: 350)
        .onChange(of: model.runID) { _ in expandedTools.removeAll() }
        .task(id: model.runID) { await model.observe() }
    }
}
