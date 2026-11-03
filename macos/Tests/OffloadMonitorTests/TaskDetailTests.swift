import AppKit
import SwiftUI
import XCTest
@testable import OffloadMonitor

@MainActor final class TaskDetailTests: XCTestCase {
    let id = String(repeating: "a", count: 32)

    func state(_ phase: String) throws -> TaskSnapshot {
        let terminal = phase == "canceled"
        return try OffloadCLI.taskDecode(TaskSnapshot.self, Data("""
        {"schema_version":1,"run_id":"\(id)","caller":"agh","phase":"\(terminal ? "done" : phase)","status":"\(phase)","active":\(!terminal),"can_stop":\(phase == "running"),"can_resume":\(terminal),"resume_reason":"Resume creates a new execution in the existing session.","resume_pending":false,"task":"Inspect existing task effects before continuing."}
        """.utf8))
    }

    func page(seq: Int, text: String, cursor: String) throws -> TaskTranscript {
        let object: [String: Any] = ["schema_version": 1, "availability": "available", "completeness": "in_progress", "events": [["seq": seq, "type": "message", "role": "assistant", "text": text]], "next_cursor": cursor, "has_more": false]
        return try OffloadCLI.taskDecode(TaskTranscript.self, JSONSerialization.data(withJSONObject: object))
    }

    func testStructuredEventsDistinguishMessagesAndToolFailures() throws {
        let data = Data("""
        {"schema_version":1,"availability":"available","has_more":false,"events":[
          {"seq":1,"type":"message","role":"user","text":"Inspect"},
          {"seq":2,"type":"tool_call","tool_name":"Bash","arguments":"ls"},
          {"seq":3,"type":"tool_result","output":"failed","is_error":true,"exit_code":1}
        ]}
        """.utf8)
        let events = try OffloadCLI.taskDecode(TaskTranscript.self, data).events
        XCTAssertEqual(events.map(\.title), ["You", "Running · Bash", "Tool failed"])
        XCTAssertEqual(events[2].content, "failed")
        XCTAssertEqual(events[2].exitCode, 1)
    }

    func testCursorDedupBoundsAndFollowReset() throws {
        let m = TaskDetailModel(runID: id, cli: nil)
        let p = try page(seq: 1, text: "first", cursor: "opaque")
        m.ingest(p); m.ingest(p)
        XCTAssertEqual(m.events.count, 1)
        for seq in 2...20 { m.ingest(try page(seq: seq, text: String(repeating: "x", count: 65_536), cursor: "opaque-\(seq)")) }
        XCTAssertTrue(m.trimmed)
        XCTAssertLessThanOrEqual(m.events.reduce(0) { $0 + $1.content.utf8.count }, 1_048_576)
        m.follow(String(repeating: "b", count: 32))
        XCTAssertTrue(m.events.isEmpty); XCTAssertFalse(m.trimmed)
        XCTAssertTrue(OffloadCLI.validRunID(id)); XCTAssertFalse(OffloadCLI.validRunID("../invalid"))
    }

    func testControlsUseRunIdentityAndFollowOnlyNewExecution() async throws {
        let fake = try FakeCLI()
        let next = String(repeating: "b", count: 32)
        let script = """
        #!/bin/sh
        echo "$*" >> '\(fake.argvLog.path)'
        case "$1 $2" in
        'task resume') echo '{"schema_version":1,"run_id":"\(next)","caller":"agh","phase":"running","active":true,"can_stop":true,"can_resume":false,"resume_reason":"Wait for cleanup.","resume_pending":false,"resumed_from":"\(id)"}' ;;
        'task stop') sleep 0.1; echo '{"schema_version":1,"run_id":"\(id)","caller":"agh","phase":"stopping","active":true,"can_stop":false,"can_resume":false,"resume_reason":"Wait for cleanup.","resume_pending":false}' ;;
        *) exit 64 ;;
        esac
        """
        try script.write(to: fake.exe, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: fake.exe.path)
        let m = TaskDetailModel(runID: id, cli: fake.cli)
        m.snapshot = try state("running")
        async let first: Void = m.control("stop")
        async let duplicate: Void = m.control("stop")
        _ = await (first, duplicate)
        XCTAssertEqual(fake.argv.count, 1)
        XCTAssertEqual(m.snapshot?.phase, "stopping")
        m.snapshot = try state("canceled")
        await m.control("resume")
        XCTAssertEqual(m.runID, next)
        XCTAssertEqual(m.snapshot?.resumedFrom, id)
        XCTAssertEqual(fake.argv, ["task stop --id \(id) --json", "task resume --id \(id) --json"])
    }

    func testTaskStatesRenderOffscreen() throws {
        let dir = ProcessInfo.processInfo.environment["OFFLOAD_PANEL_RENDER_DIR"]
        for phase in ["running", "stopping", "canceled", "unavailable"] {
            let m = TaskDetailModel(runID: id, cli: nil)
            m.snapshot = try state(phase)
            m.ingest(try page(seq: 1, text: "## Progress\n\nCompleted the first inspection.\n\n```text\ncommand output retained\n```", cursor: "opaque"))
            let host = NSHostingView(rootView: TaskDetailView(model: m).background(Color(nsColor: .windowBackgroundColor)))
            host.appearance = NSAppearance(named: .aqua)
            host.frame = NSRect(x: 0, y: 0, width: 680, height: 560)
            host.layoutSubtreeIfNeeded()
            let bitmap = try XCTUnwrap(host.bitmapImageRepForCachingDisplay(in: host.bounds))
            host.cacheDisplay(in: host.bounds, to: bitmap)
            let data = try XCTUnwrap(bitmap.representation(using: .png, properties: [:]))
            XCTAssertGreaterThan(data.count, 1000)
            if let dir {
                try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
                try data.write(to: URL(fileURLWithPath: dir).appendingPathComponent("task-\(phase).png"))
            }
        }
    }
}
