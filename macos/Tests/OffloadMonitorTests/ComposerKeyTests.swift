import AppKit
import XCTest
@testable import OffloadMonitor

final class ComposerKeyTests: XCTestCase {
    private func act(_ code: UInt16 = ComposerKey.returnKeyCode, _ mods: NSEvent.ModifierFlags = [], rep: Bool = false,
                      marked: Bool = false, can: Bool = true) -> ComposerKeyAction {
        ComposerKey.action(keyCode: code, modifiers: mods, isRepeat: rep, hasMarkedText: marked, canSend: can)
    }

    func testPlainReturnAndKeypadEnterSend() {
        XCTAssertEqual(act(), .send)
        XCTAssertEqual(act(ComposerKey.keypadEnterKeyCode, [.numericPad]), .send)
        XCTAssertEqual(act(ComposerKey.keypadEnterKeyCode, [.numericPad, .function]), .send)
    }

    func testModifiedReturnKeepsNativeBehavior() {
        XCTAssertEqual(act(ComposerKey.returnKeyCode, [.shift]), .passThrough, "Shift+Enter is a native newline")
        XCTAssertEqual(act(ComposerKey.returnKeyCode, [.command]), .passThrough, "Command+Enter is the Send button shortcut")
        XCTAssertEqual(act(ComposerKey.returnKeyCode, [.option]), .passThrough)
        XCTAssertEqual(act(ComposerKey.returnKeyCode, [.control]), .passThrough)
        XCTAssertEqual(act(ComposerKey.returnKeyCode, [.shift], rep: true, can: false), .passThrough)
    }

    func testMarkedTextBelongsToTheInputMethod() {
        XCTAssertEqual(act(marked: true), .passThrough)
        XCTAssertEqual(act(ComposerKey.keypadEnterKeyCode, [.numericPad], marked: true), .passThrough)
    }

    func testBlockedAndRepeatedReturnIsSwallowed() {
        XCTAssertEqual(act(can: false), .swallow, "blocked Enter neither sends nor inserts a newline")
        XCTAssertEqual(act(rep: true), .swallow, "key repeat never sends")
    }

    func testOtherKeysPassThrough() {
        XCTAssertEqual(act(0), .passThrough)
        XCTAssertEqual(act(49), .passThrough)
    }

    // MARK: through the real NSTextView subclass

    private func key(_ code: UInt16, _ chars: String, _ mods: NSEvent.ModifierFlags = [], rep: Bool = false) -> NSEvent {
        NSEvent.keyEvent(with: .keyDown, location: .zero, modifierFlags: mods, timestamp: 0, windowNumber: 0, context: nil,
                         characters: chars, charactersIgnoringModifiers: chars, isARepeat: rep, keyCode: code)!
    }

    @MainActor
    func testTextViewSendsOnceAndDoesNotInsertNewline() {
        let tv = ComposerNSTextView()
        var sends = 0
        var allowed = true
        tv.canSend = { allowed }
        tv.onSend = { sends += 1; allowed = false } // mirrors ChatModel: a started send makes canSend false
        tv.string = "hello"
        tv.setSelectedRange(NSRange(location: 5, length: 0))
        tv.keyDown(with: key(36, "\r"))
        tv.keyDown(with: key(36, "\r", rep: true))
        tv.keyDown(with: key(36, "\r"))
        XCTAssertEqual(sends, 1)
        XCTAssertEqual(tv.string, "hello", "Enter never inserts a newline and the draft is kept")
    }

    @MainActor
    func testTextViewBlockedEnterKeepsDraft() {
        let tv = ComposerNSTextView()
        var sends = 0
        tv.canSend = { false }
        tv.onSend = { sends += 1 }
        tv.string = "draft"
        tv.keyDown(with: key(36, "\r"))
        XCTAssertEqual(sends, 0)
        XCTAssertEqual(tv.string, "draft")
    }

    // MARK: send gating and mock runner

    @MainActor
    func testRepeatedEnterStartsOneRunAndBlockedStatesStartNone() async throws {
        let fake = try FakeChatCLI(mode: .slow, runJSON: FakeChatCLI.runOK(session: "s1", status: "error", exit: 130))
        let tmp = FileManager.default.temporaryDirectory.appendingPathComponent("composer-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: tmp, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: tmp) }
        let d = UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!
        let profiles = ProfilesModel(testingSnapshot: try ProfilesSnapshot.decode(Data(Fixtures.profiles([Fixtures.profile(name: "work")], active: "work", configured: "work").utf8)))
        let env = ChatEnv(cli: fake.cli, profiles: profiles, monitor: MonitorModel(testingSnapshot: nil),
                          history: HistoryModel(cli: fake.cli, debounce: 0), prefs: Preferences(defaults: d),
                          generalWorkspace: tmp.appendingPathComponent("General"))
        let m = ChatModel(env: env)
        m.workspace = tmp.path
        let tv = ComposerNSTextView()
        tv.canSend = { m.canSend }
        tv.onSend = { m.send() }
        let enter = key(36, "\r")

        tv.keyDown(with: enter) // empty draft: blocked
        XCTAssertEqual(m.phase, .idle)
        m.draft = "go"
        tv.string = "go"
        tv.keyDown(with: enter)
        XCTAssertEqual(m.phase, .sending)
        for _ in 0..<5 { tv.keyDown(with: key(36, "\r", rep: true)); tv.keyDown(with: enter) }
        _ = await fake.waitForFile("ready")
        XCTAssertEqual(fake.runCount, 1)
        m.draft = "typed while running"
        tv.keyDown(with: enter)
        XCTAssertEqual(fake.runCount, 1)
        XCTAssertEqual(m.draft, "typed while running", "a blocked Enter never drops the draft")
        m.stop()
        tv.keyDown(with: enter) // stopping/finalizing
        await waitUntil { m.phase == .idle }
        XCTAssertEqual(fake.runCount, 1)
        XCTAssertEqual(m.draft, "typed while running")
    }
}
