import Foundation

/// A small block-level Markdown parser. It never drops text: anything it does not recognise is a paragraph,
/// and an unclosed code fence keeps everything after it as code.
enum MDBlock: Equatable {
    case heading(level: Int, text: String)
    case paragraph(String)
    case bullets([String])
    case numbered(start: Int, items: [String])
    case quote([MDBlock])
    case code(language: String, text: String, closed: Bool)
    case table(header: [String], aligns: [MDAlign], rows: [[String]])
    case rule
}

enum MDAlign: Equatable { case left, center, right }

enum Markdown {
    static func parse(_ source: String, depth: Int = 0) -> [MDBlock] {
        let lines = source.replacingOccurrences(of: "\r\n", with: "\n").replacingOccurrences(of: "\r", with: "\n")
            .split(separator: "\n", omittingEmptySubsequences: false).map(String.init)
        var blocks: [MDBlock] = []
        var para: [String] = []
        var i = 0

        func flush() {
            if !para.isEmpty { blocks.append(.paragraph(para.joined(separator: "\n"))); para = [] }
        }

        while i < lines.count {
            let line = lines[i]
            let t = line.trimmingCharacters(in: .whitespaces)

            if let fence = fenceOpen(t) {
                flush()
                var body: [String] = []
                var closed = false
                i += 1
                while i < lines.count {
                    let l = lines[i]
                    if isFenceClose(l.trimmingCharacters(in: .whitespaces), marker: fence.marker, count: fence.count) { closed = true; i += 1; break }
                    body.append(l)
                    i += 1
                }
                blocks.append(.code(language: fence.language, text: body.joined(separator: "\n"), closed: closed))
                continue
            }
            if t.isEmpty { flush(); i += 1; continue }
            if isRule(t) { flush(); blocks.append(.rule); i += 1; continue }
            if let h = heading(t) { flush(); blocks.append(.heading(level: h.0, text: h.1)); i += 1; continue }

            if t.hasPrefix(">"), depth < 4 {
                flush()
                var inner: [String] = []
                while i < lines.count {
                    let q = lines[i].trimmingCharacters(in: .whitespaces)
                    guard q.hasPrefix(">") else { break }
                    var rest = String(q.dropFirst())
                    if rest.hasPrefix(" ") { rest.removeFirst() }
                    inner.append(rest)
                    i += 1
                }
                blocks.append(.quote(parse(inner.joined(separator: "\n"), depth: depth + 1)))
                continue
            }
            if i + 1 < lines.count, t.contains("|"), let aligns = tableSeparator(lines[i + 1]) {
                let header = cells(t)
                if header.count == aligns.count {
                    flush()
                    var rows: [[String]] = []
                    i += 2
                    while i < lines.count {
                        let r = lines[i].trimmingCharacters(in: .whitespaces)
                        guard !r.isEmpty, r.contains("|") else { break }
                        var c = cells(r)
                        // A row with more cells than the header is malformed: end the table and keep its text as a paragraph.
                        if c.count > header.count { break }
                        if c.count < header.count { c += Array(repeating: "", count: header.count - c.count) }
                        rows.append(c)
                        i += 1
                    }
                    blocks.append(.table(header: header, aligns: aligns, rows: rows))
                    continue
                }
            }
            if bulletItem(t) != nil {
                flush()
                var items: [String] = []
                while i < lines.count, let item = bulletItem(lines[i].trimmingCharacters(in: .whitespaces)) {
                    items.append(item); i += 1
                    // indented continuation lines belong to the item
                    while i < lines.count, lines[i].hasPrefix("  "), !lines[i].trimmingCharacters(in: .whitespaces).isEmpty,
                          bulletItem(lines[i].trimmingCharacters(in: .whitespaces)) == nil, numberedItem(lines[i].trimmingCharacters(in: .whitespaces)) == nil {
                        items[items.count - 1] += " " + lines[i].trimmingCharacters(in: .whitespaces); i += 1
                    }
                }
                blocks.append(.bullets(items))
                continue
            }
            if let first = numberedItem(t) {
                flush()
                var items: [String] = []
                while i < lines.count, let item = numberedItem(lines[i].trimmingCharacters(in: .whitespaces)) {
                    items.append(item.text); i += 1
                    while i < lines.count, lines[i].hasPrefix("  "), !lines[i].trimmingCharacters(in: .whitespaces).isEmpty,
                          bulletItem(lines[i].trimmingCharacters(in: .whitespaces)) == nil, numberedItem(lines[i].trimmingCharacters(in: .whitespaces)) == nil {
                        items[items.count - 1] += " " + lines[i].trimmingCharacters(in: .whitespaces); i += 1
                    }
                }
                blocks.append(.numbered(start: first.number, items: items))
                continue
            }
            para.append(line)
            i += 1
        }
        flush()
        return blocks
    }

    // MARK: line classifiers

    static func fenceOpen(_ t: String) -> (marker: Character, count: Int, language: String)? {
        guard let c = t.first, c == "`" || c == "~" else { return nil }
        let n = t.prefix { $0 == c }.count
        guard n >= 3 else { return nil }
        let info = String(t.dropFirst(n)).trimmingCharacters(in: .whitespaces)
        if c == "`", info.contains("`") { return nil }
        return (c, n, String(info.split(separator: " ").first ?? ""))
    }

    static func isFenceClose(_ t: String, marker: Character, count: Int) -> Bool {
        guard let c = t.first, c == marker else { return false }
        let n = t.prefix { $0 == c }.count
        return n >= count && t.dropFirst(n).allSatisfy { $0 == " " }
    }

    static func isRule(_ t: String) -> Bool {
        let chars = t.filter { $0 != " " }
        guard chars.count >= 3, let c = chars.first, "-*_".contains(c) else { return false }
        return chars.allSatisfy { $0 == c }
    }

    static func heading(_ t: String) -> (Int, String)? {
        let n = t.prefix { $0 == "#" }.count
        guard (1...6).contains(n) else { return nil }
        let rest = t.dropFirst(n)
        guard rest.isEmpty || rest.first == " " else { return nil }
        var text = rest.trimmingCharacters(in: .whitespaces)
        // A closing run of # is decoration only when a space precedes it ("## Title ##"); "C#" keeps its #.
        if text.hasSuffix("#") {
            let run = text.reversed().prefix { $0 == "#" }.count
            let before = text.dropLast(run)
            if before.isEmpty || before.last == " " || before.last == "\t" {
                text = before.trimmingCharacters(in: .whitespaces)
            }
        }
        return (n, text)
    }

    static func bulletItem(_ t: String) -> String? {
        guard let f = t.first, "-*+".contains(f), t.dropFirst().first == " " else { return nil }
        if isRule(t) { return nil }
        return String(t.dropFirst(2)).trimmingCharacters(in: .whitespaces)
    }

    static func numberedItem(_ t: String) -> (number: Int, text: String)? {
        let digits = t.prefix { $0.isASCII && $0.isNumber }
        guard !digits.isEmpty, digits.count <= 9, let n = Int(digits) else { return nil }
        let rest = t.dropFirst(digits.count)
        guard let d = rest.first, d == "." || d == ")", rest.dropFirst().first == " " else { return nil }
        return (n, String(rest.dropFirst(2)).trimmingCharacters(in: .whitespaces))
    }

    static func cells(_ line: String) -> [String] {
        var t = line.trimmingCharacters(in: .whitespaces)
        if t.hasPrefix("|") { t.removeFirst() }
        if t.hasSuffix("|") { t.removeLast() }
        var out: [String] = []
        var cur = ""
        var escaped = false
        for ch in t {
            if escaped { cur.append(ch); escaped = false }
            else if ch == "\\" { escaped = true; cur.append(ch) }
            else if ch == "|" { out.append(cur.trimmingCharacters(in: .whitespaces)); cur = "" }
            else { cur.append(ch) }
        }
        out.append(cur.trimmingCharacters(in: .whitespaces))
        return out
    }

    static func tableSeparator(_ line: String) -> [MDAlign]? {
        let t = line.trimmingCharacters(in: .whitespaces)
        guard t.contains("-"), t.allSatisfy({ "|-: ".contains($0) }) else { return nil }
        let cs = cells(t)
        guard !cs.isEmpty else { return nil }
        var out: [MDAlign] = []
        for c in cs {
            guard c.contains("-"), c.allSatisfy({ "-:".contains($0) }) else { return nil }
            switch (c.hasPrefix(":"), c.hasSuffix(":")) {
            case (true, true): out.append(.center)
            case (false, true): out.append(.right)
            default: out.append(.left)
            }
        }
        return out
    }

    // MARK: inline

    /// Inline emphasis, code and links as an AttributedString. Failure falls back to the plain text, never to nothing.
    /// Images are shown as their alt text and never fetched. Only http(s) links stay clickable.
    static func inline(_ text: String) -> AttributedString {
        let stripped = removeImages(text)
        var options = AttributedString.MarkdownParsingOptions()
        options.interpretedSyntax = .inlineOnlyPreservingWhitespace
        options.failurePolicy = .returnPartiallyParsedIfPossible
        guard var out = try? AttributedString(markdown: stripped, options: options) else { return AttributedString(text) }
        for run in out.runs {
            if let link = run.link, !isSafeLink(link) { out[run.range].link = nil }
        }
        return out
    }

    static func isSafeLink(_ url: URL) -> Bool {
        guard let s = url.scheme?.lowercased(), s == "http" || s == "https" else { return false }
        return url.host?.isEmpty == false
    }

    /// `![alt](src)` becomes `alt` so no image URL is ever loaded.
    static func removeImages(_ text: String) -> String {
        guard text.contains("![") else { return text }
        var out = ""
        var i = text.startIndex
        while i < text.endIndex {
            if text[i...].hasPrefix("!["), let close = text[i...].firstIndex(of: "]"),
               text.index(after: close) < text.endIndex, text[text.index(after: close)] == "(",
               let end = text[text.index(after: close)...].firstIndex(of: ")") {
                let alt = text[text.index(i, offsetBy: 2)..<close]
                out += alt.isEmpty ? "[image]" : String(alt)
                i = text.index(after: end)
            } else {
                out.append(text[i]); i = text.index(after: i)
            }
        }
        return out
    }

    /// Plain text of a block, for tests and for copy.
    static func plain(_ blocks: [MDBlock]) -> String {
        blocks.map { b -> String in
            switch b {
            case .heading(_, let t), .paragraph(let t): return t
            case .bullets(let items): return items.joined(separator: "\n")
            case .numbered(_, let items): return items.joined(separator: "\n")
            case .quote(let inner): return plain(inner)
            case .code(_, let t, _): return t
            case .table(let h, _, let rows): return ([h] + rows).map { $0.joined(separator: " | ") }.joined(separator: "\n")
            case .rule: return "---"
            }
        }.joined(separator: "\n\n")
    }
}
