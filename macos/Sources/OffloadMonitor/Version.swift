import Foundation

/// A numeric dotted version such as 1.2.3. Pre-release or build suffixes ("-rc1", "+meta") are
/// recorded but never compared as newer than a stable version.
struct SemanticVersion: Equatable, Comparable, CustomStringConvertible {
    let components: [Int]
    let isPrerelease: Bool

    /// Accepts "v1.2.3", "1.2", "1.2.3-rc1". Rejects anything without a leading numeric core.
    init?(_ raw: String) {
        var s = raw.trimmingCharacters(in: .whitespacesAndNewlines)
        if s.hasPrefix("v") || s.hasPrefix("V") { s.removeFirst() }
        var pre = false
        if let i = s.firstIndex(where: { $0 == "-" || $0 == "+" }) {
            pre = s[i] == "-"
            s = String(s[s.startIndex..<i])
        }
        let parts = s.split(separator: ".", omittingEmptySubsequences: false)
        guard !parts.isEmpty, parts.count <= 4 else { return nil }
        var nums: [Int] = []
        for p in parts {
            guard !p.isEmpty, p.count <= 9, p.allSatisfy({ $0.isASCII && $0.isNumber }), let n = Int(p) else { return nil }
            nums.append(n)
        }
        components = nums
        isPrerelease = pre
    }

    var description: String { components.map(String.init).joined(separator: ".") + (isPrerelease ? "-pre" : "") }

    static func == (a: SemanticVersion, b: SemanticVersion) -> Bool {
        compareCore(a, b) == 0 && a.isPrerelease == b.isPrerelease
    }

    static func < (a: SemanticVersion, b: SemanticVersion) -> Bool {
        let c = compareCore(a, b)
        if c != 0 { return c < 0 }
        return a.isPrerelease && !b.isPrerelease
    }

    private static func compareCore(_ a: SemanticVersion, _ b: SemanticVersion) -> Int {
        for i in 0..<max(a.components.count, b.components.count) {
            let x = i < a.components.count ? a.components[i] : 0
            let y = i < b.components.count ? b.components[i] : 0
            if x != y { return x < y ? -1 : 1 }
        }
        return 0
    }
}
