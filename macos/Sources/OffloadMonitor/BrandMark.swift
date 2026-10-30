import AppKit
import SwiftUI

/// Two open hands passing a small task between them. The low-detail silhouette stays legible
/// at menu-bar size and uses the same geometry in the panel and template image.
struct BrandMarkShape: Shape {
    func path(in rect: CGRect) -> Path {
        let side = min(rect.width, rect.height)
        let origin = CGPoint(x: rect.midX - side / 2, y: rect.midY - side / 2)
        func point(_ x: CGFloat, _ y: CGFloat) -> CGPoint {
            CGPoint(x: origin.x + side * x, y: origin.y + side * y)
        }
        var p = Path()
        // Lower hand: cuff, open palm, and thumb supporting the task.
        p.move(to: point(0.08, 0.72))
        p.addLine(to: point(0.24, 0.88))
        p.addLine(to: point(0.58, 0.88))
        p.addQuadCurve(to: point(0.80, 0.65), control: point(0.72, 0.86))
        p.addQuadCurve(to: point(0.69, 0.58), control: point(0.80, 0.55))
        p.addLine(to: point(0.55, 0.70))
        p.addLine(to: point(0.35, 0.70))
        // Upper hand mirrors the lower one: another person receives the task.
        p.move(to: point(0.92, 0.28))
        p.addLine(to: point(0.76, 0.12))
        p.addLine(to: point(0.42, 0.12))
        p.addQuadCurve(to: point(0.20, 0.35), control: point(0.28, 0.14))
        p.addQuadCurve(to: point(0.31, 0.42), control: point(0.20, 0.45))
        p.addLine(to: point(0.45, 0.30))
        p.addLine(to: point(0.65, 0.30))
        // Task token, separate from the palms so the mark reads at 16 points.
        p.addRoundedRect(in: CGRect(x: origin.x + side * 0.40, y: origin.y + side * 0.42,
                                   width: side * 0.20, height: side * 0.16), cornerSize: CGSize(width: side * 0.03, height: side * 0.03))
        return p
    }
}

struct BrandMark: View {
    var size: CGFloat = 15
    var body: some View {
        BrandMarkShape()
            .stroke(style: StrokeStyle(lineWidth: max(1.1, size * 0.085), lineCap: .round, lineJoin: .round))
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
            ctx.setLineWidth(1.35)
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
