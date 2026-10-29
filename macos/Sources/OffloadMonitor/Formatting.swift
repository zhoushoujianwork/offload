import Foundation

enum Format {
    static func count(_ n: Int64) -> String {
        let v = Double(n)
        switch abs(n) {
        case 0..<1_000: return "\(n)"
        case 1_000..<1_000_000: return String(format: "%.1fK", v / 1_000)
        case 1_000_000..<1_000_000_000: return String(format: "%.2fM", v / 1_000_000)
        default: return String(format: "%.2fB", v / 1_000_000_000)
        }
    }

    static func cost(_ usd: Double) -> String {
        if usd > 0 && usd < 0.01 { return "<$0.01" }
        return String(format: "$%.2f", usd)
    }

    static func duration(_ seconds: TimeInterval) -> String {
        let s = max(0, Int(seconds))
        if s < 60 { return "\(s)s" }
        if s < 3600 { return "\(s / 60)m \(s % 60)s" }
        return "\(s / 3600)h \((s % 3600) / 60)m"
    }

    static func coverage(_ covered: Int, of total: Int) -> String {
        "\(covered) of \(total) run\(total == 1 ? "" : "s")"
    }

    private static let plain: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime]
        return f
    }()
    private static let fractional: ISO8601DateFormatter = {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        return f
    }()

    /// Parses Go's RFC3339 and RFC3339Nano output.
    static func parseTimestamp(_ s: String) -> Date? {
        plain.date(from: s) ?? fractional.date(from: s)
    }
}
