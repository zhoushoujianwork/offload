import AppKit
import SwiftUI

/// What the composer does with a key-down. Pure so it can be unit tested without a window.
enum ComposerKeyAction: Equatable {
    /// Start the send through the normal send path.
    case send
    /// Consume the event: no send and no text change (blocked state or key repeat).
    case swallow
    /// Leave the event to NSTextView (native editing, input methods, Shift+Enter newline, Command+Enter shortcut).
    case passThrough
}

enum ComposerKey {
    static let returnKeyCode: UInt16 = 36
    static let keypadEnterKeyCode: UInt16 = 76

    /// Decides what a key-down in the composer should do.
    /// - Parameters:
    ///   - hasMarkedText: the text view has an active input-method composition. The event then belongs to the input
    ///     method (it confirms a candidate) and must never send.
    ///   - canSend: the model's current `canSend`, so existing send gating is not bypassed.
    static func action(keyCode: UInt16, modifiers: NSEvent.ModifierFlags, isRepeat: Bool,
                       hasMarkedText: Bool, canSend: Bool) -> ComposerKeyAction {
        guard keyCode == returnKeyCode || keyCode == keypadEnterKeyCode else { return .passThrough }
        if hasMarkedText { return .passThrough }
        // The keypad Enter key also carries .numericPad and .function; only these four change its meaning.
        let mods = modifiers.intersection([.shift, .control, .option, .command])
        // Shift+Enter inserts a newline natively. Command+Enter is the Send button's shortcut, which AppKit
        // dispatches before the key reaches the text view. Any other combination keeps its native meaning.
        guard mods.isEmpty else { return .passThrough }
        if isRepeat { return .swallow }
        return canSend ? .send : .swallow
    }
}

/// NSTextView that sends on a plain Enter and keeps every other key native.
final class ComposerNSTextView: NSTextView {
    var canSend: () -> Bool = { false }
    var onSend: () -> Void = {}

    override func keyDown(with event: NSEvent) {
        let action = ComposerKey.action(keyCode: event.keyCode, modifiers: event.modifierFlags, isRepeat: event.isARepeat,
                                        hasMarkedText: hasMarkedText(), canSend: canSend())
        switch action {
        case .send: onSend()
        case .swallow: break
        case .passThrough: super.keyDown(with: event)
        }
    }
}

/// The chat composer: a multi-line plain-text editor where Enter sends and Shift+Enter inserts a newline.
struct ComposerTextView: NSViewRepresentable {
    @Binding var text: String
    /// Increment to move keyboard focus into the editor.
    var focusRequest: Int
    var canSend: () -> Bool
    var onSend: () -> Void

    static let help = "Enter sends. Shift+Enter inserts a new line. Command+Enter also sends."

    func makeCoordinator() -> Coordinator { Coordinator(self) }

    func makeNSView(context: Context) -> NSScrollView {
        let scroll = NSScrollView()
        scroll.drawsBackground = false
        scroll.hasVerticalScroller = false
        scroll.hasHorizontalScroller = false
        scroll.borderType = .noBorder

        let tv = ComposerNSTextView()
        tv.minSize = NSSize(width: 0, height: 0)
        tv.maxSize = NSSize(width: CGFloat.greatestFiniteMagnitude, height: CGFloat.greatestFiniteMagnitude)
        tv.isVerticallyResizable = true
        tv.isHorizontallyResizable = false
        tv.autoresizingMask = [.width]
        tv.textContainer?.widthTracksTextView = true
        tv.textContainer?.containerSize = NSSize(width: 0, height: CGFloat.greatestFiniteMagnitude)
        tv.drawsBackground = false
        tv.isRichText = false
        tv.importsGraphics = false
        tv.allowsUndo = true
        tv.font = .systemFont(ofSize: 13)
        tv.textColor = .labelColor
        tv.textContainerInset = .zero
        tv.delegate = context.coordinator
        tv.string = text
        tv.canSend = canSend
        tv.onSend = onSend
        tv.toolTip = Self.help
        tv.setAccessibilityLabel("Message")
        tv.setAccessibilityHelp(Self.help)
        scroll.documentView = tv

        context.coordinator.textView = tv
        context.coordinator.handledFocus = focusRequest
        DispatchQueue.main.async { [weak tv] in tv?.window?.makeFirstResponder(tv) }
        return scroll
    }

    func updateNSView(_ scroll: NSScrollView, context: Context) {
        context.coordinator.parent = self
        guard let tv = scroll.documentView as? ComposerNSTextView else { return }
        tv.canSend = canSend
        tv.onSend = onSend
        // Never rewrite the text during an input-method composition. Only replace when the model changed it
        // (for example the draft was cleared after a send).
        if tv.string != text, !tv.hasMarkedText() {
            tv.string = text
            if text.isEmpty { tv.undoManager?.removeAllActions(withTarget: tv) }
        }
        if context.coordinator.handledFocus != focusRequest {
            context.coordinator.handledFocus = focusRequest
            DispatchQueue.main.async { [weak tv] in tv?.window?.makeFirstResponder(tv) }
        }
    }

    final class Coordinator: NSObject, NSTextViewDelegate {
        var parent: ComposerTextView
        weak var textView: ComposerNSTextView?
        var handledFocus = 0
        init(_ parent: ComposerTextView) { self.parent = parent }

        func textDidChange(_ notification: Notification) {
            guard let tv = notification.object as? NSTextView, tv.string != parent.text else { return }
            parent.text = tv.string
        }
    }
}
