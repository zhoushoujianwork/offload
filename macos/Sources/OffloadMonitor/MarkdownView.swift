import AppKit
import SwiftUI

/// Renders parsed Markdown. Text is selectable. Images are never loaded; only http(s) links are clickable.
struct MarkdownView: View {
    let blocks: [MDBlock]
    var baseSize: CGFloat = 13

    init(_ source: String, baseSize: CGFloat = 13) {
        blocks = Markdown.parse(source)
        self.baseSize = baseSize
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            ForEach(Array(blocks.enumerated()), id: \.offset) { _, b in BlockView(block: b, size: baseSize) }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

private struct BlockView: View {
    let block: MDBlock
    let size: CGFloat

    var body: some View {
        switch block {
        case .heading(let level, let text):
            Text(Markdown.inline(text))
                .font(.system(size: size + CGFloat(max(0, 4 - level)) * 2, weight: .semibold))
                .padding(.top, level <= 2 ? 4 : 0)
                .textSelection(.enabled)
        case .paragraph(let text):
            Text(Markdown.inline(text)).font(.system(size: size)).lineSpacing(3).textSelection(.enabled)
                .fixedSize(horizontal: false, vertical: true)
        case .bullets(let items):
            VStack(alignment: .leading, spacing: 3) {
                ForEach(Array(items.enumerated()), id: \.offset) { _, t in
                    HStack(alignment: .firstTextBaseline, spacing: 6) {
                        Text("•").font(.system(size: size))
                        Text(Markdown.inline(t)).font(.system(size: size)).lineSpacing(2).textSelection(.enabled)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
            }
        case .numbered(let start, let items):
            VStack(alignment: .leading, spacing: 3) {
                ForEach(Array(items.enumerated()), id: \.offset) { i, t in
                    HStack(alignment: .firstTextBaseline, spacing: 6) {
                        Text("\(start + i).").font(.system(size: size).monospacedDigit()).foregroundStyle(.secondary)
                        Text(Markdown.inline(t)).font(.system(size: size)).lineSpacing(2).textSelection(.enabled)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                }
            }
        case .quote(let inner):
            HStack(spacing: 8) {
                Rectangle().fill(Color.omBlue.opacity(0.5)).frame(width: 3)
                VStack(alignment: .leading, spacing: 6) {
                    ForEach(Array(inner.enumerated()), id: \.offset) { _, b in BlockView(block: b, size: size) }
                }
                .foregroundStyle(.secondary)
            }
        case .code(let language, let text, let closed):
            CodeBlock(language: language, text: text, closed: closed)
        case .table(let header, let aligns, let rows):
            TableBlock(header: header, aligns: aligns, rows: rows, size: size)
        case .rule:
            Divider().padding(.vertical, 2)
        }
    }
}

private struct CodeBlock: View {
    let language: String
    let text: String
    let closed: Bool
    @State private var copied = false

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            HStack {
                Text(language.isEmpty ? "code" : language).font(.system(size: 10, weight: .medium)).foregroundStyle(.secondary)
                if !closed { Text("unclosed").font(.system(size: 10)).foregroundStyle(.orange) }
                Spacer()
                Button(copied ? "Copied" : "Copy") {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(text, forType: .string)
                    copied = true
                    DispatchQueue.main.asyncAfter(deadline: .now() + 1.5) { copied = false }
                }
                .buttonStyle(.borderless).font(.system(size: 10))
            }
            .padding(.horizontal, 10).padding(.vertical, 5)
            Divider()
            ScrollView(.horizontal, showsIndicators: false) {
                Text(text.isEmpty ? " " : text).font(.system(size: 12, design: .monospaced)).textSelection(.enabled)
                    .padding(10).fixedSize(horizontal: true, vertical: true)
            }
        }
        .background(Color.primary.opacity(0.06), in: RoundedRectangle(cornerRadius: 8))
        .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.primary.opacity(0.08)))
    }
}

private struct TableBlock: View {
    let header: [String]
    let aligns: [MDAlign]
    let rows: [[String]]
    let size: CGFloat

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            Grid(alignment: .leading, horizontalSpacing: 14, verticalSpacing: 5) {
                GridRow { ForEach(Array(header.enumerated()), id: \.offset) { i, h in cell(h, i, bold: true) } }
                Divider().gridCellUnsizedAxes(.horizontal)
                ForEach(Array(rows.enumerated()), id: \.offset) { _, r in
                    GridRow { ForEach(Array(r.enumerated()), id: \.offset) { i, c in cell(c, i, bold: false) } }
                }
            }
            .padding(8)
        }
        .background(Color.primary.opacity(0.04), in: RoundedRectangle(cornerRadius: 8))
        .overlay(RoundedRectangle(cornerRadius: 8).strokeBorder(Color.primary.opacity(0.08)))
    }

    private func cell(_ t: String, _ i: Int, bold: Bool) -> some View {
        Text(Markdown.inline(t)).font(.system(size: size - 1, weight: bold ? .semibold : .regular))
            .textSelection(.enabled)
            .frame(maxWidth: .infinity, alignment: alignment(i))
    }

    private func alignment(_ i: Int) -> Alignment {
        guard i < aligns.count else { return .leading }
        switch aligns[i] {
        case .left: return .leading
        case .center: return .center
        case .right: return .trailing
        }
    }
}
