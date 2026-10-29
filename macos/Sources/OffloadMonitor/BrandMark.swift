import AppKit
import SwiftUI

/// A ring with an arrow leaving it toward the upper right: work offloaded outward. Drawn as strokes so it is
/// monochrome and takes the surrounding foreground color in light and dark appearance.
struct BrandMarkShape: Shape {
    func path(in rect: CGRect) -> Path {
        let side = min(rect.width, rect.height)
        let c = CGPoint(x: rect.midX, y: rect.midY)
        let r = side * 0.36
        var p = Path()
        // Ring, open around the upper-right direction (-45 degrees in screen coordinates) for the arrow to pass.
        let gap = Double.pi / 5
        let start = -Double.pi / 4 + gap / 2
        let end = start + 2 * Double.pi - gap
        let steps = 48
        for i in 0...steps {
            let a = start + (end - start) * Double(i) / Double(steps)
            let pt = CGPoint(x: c.x + r * CGFloat(cos(a)), y: c.y + r * CGFloat(sin(a)))
            if i == 0 { p.move(to: pt) } else { p.addLine(to: pt) }
        }
        // Arrow shaft from the middle to the corner, with a head.
        let tip = CGPoint(x: rect.midX + side * 0.46, y: rect.midY - side * 0.46)
        let tail = CGPoint(x: c.x - side * 0.04, y: c.y + side * 0.04)
        p.move(to: tail)
        p.addLine(to: tip)
        let head = side * 0.26
        p.move(to: CGPoint(x: tip.x - head, y: tip.y))
        p.addLine(to: tip)
        p.addLine(to: CGPoint(x: tip.x, y: tip.y + head))
        return p
    }
}

struct BrandMark: View {
    var size: CGFloat = 15
    var body: some View {
        BrandMarkShape()
            .stroke(style: StrokeStyle(lineWidth: max(1.2, size * 0.11), lineCap: .round, lineJoin: .round))
            .frame(width: size, height: size)
            .foregroundStyle(.primary)
            .accessibilityLabel("Offload")
    }
}

enum BrandMarkImage {
    /// The same shape as `BrandMark`, drawn into a template image so the menu bar tints it for light, dark and highlight.
    static let menuBar: NSImage = {
        let size = NSSize(width: 16, height: 16)
        let img = NSImage(size: size, flipped: true) { rect in
            guard let ctx = NSGraphicsContext.current?.cgContext else { return false }
            ctx.setStrokeColor(NSColor.black.cgColor)
            ctx.setLineWidth(1.6)
            ctx.setLineCap(.round)
            ctx.setLineJoin(.round)
            ctx.addPath(BrandMarkShape().path(in: rect.insetBy(dx: 0.5, dy: 0.5)).cgPath)
            ctx.strokePath()
            return true
        }
        img.isTemplate = true
        return img
    }()
}
