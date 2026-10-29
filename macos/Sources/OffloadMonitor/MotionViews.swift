import SwiftUI

/// The state symbol with its motion. The symbol shape never changes with motion; only its opacity steps.
/// With motion off (`.still`) this is the plain symbol and carries no animation at all.
struct StatusSymbolView: View {
    let name: String
    @ObservedObject var motion: MotionModel

    var body: some View {
        Image(systemName: name)
            .opacity(motion.symbolOpacity)
            .allowsHitTesting(false) // decoration never takes a click from the control or the menu-bar item
            // `value: beat` limits the animation to the steps; a state change is not animated by this modifier.
            .animation(motion.phase == .still ? nil : .easeInOut(duration: MotionPolicy.tickInterval(for: motion.phase, style: motion.style) ?? 0.3),
                       value: motion.beat)
    }
}

/// Animates a changing count with a numeric transition when allowed, and leaves it untouched otherwise.
/// `numericText(countsDown:)` exists on macOS 13; `numericText(value:)` needs macOS 14, so it is guarded.
struct CountTransition: ViewModifier {
    let animates: Bool
    let value: Int

    @ViewBuilder func body(content: Content) -> some View {
        if !animates {
            content
        } else if #available(macOS 14.0, *) {
            content
                .contentTransition(.numericText(value: Double(value)))
                .animation(.easeInOut(duration: 0.25), value: value)
        } else {
            content
                .contentTransition(.numericText(countsDown: false))
                .animation(.easeInOut(duration: 0.25), value: value)
        }
    }
}

extension View {
    func countTransition(animates: Bool, value: Int) -> some View {
        modifier(CountTransition(animates: animates, value: value))
    }
}
