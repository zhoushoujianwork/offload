import Foundation

/// Fixed-size paging for the compact lists, so every entry stays reachable without scrolling.
enum Pagination {
    static func pageCount(total: Int, size: Int) -> Int {
        guard size > 0 else { return 1 }
        return max(1, (max(0, total) + size - 1) / size)
    }

    /// Keeps `page` inside 0..<pageCount, so a page that no longer exists after a data change falls back to the last one.
    static func clamp(_ page: Int, total: Int, size: Int) -> Int {
        min(max(0, page), pageCount(total: total, size: size) - 1)
    }

    static func slice<T>(_ items: [T], page: Int, size: Int) -> [T] {
        guard size > 0, !items.isEmpty else { return [] }
        let p = clamp(page, total: items.count, size: size)
        let lo = p * size
        return Array(items[lo..<min(items.count, lo + size)])
    }

    /// "4–6 of 9", or "0" when empty.
    static func rangeLabel(page: Int, total: Int, size: Int) -> String {
        guard total > 0, size > 0 else { return "0" }
        let p = clamp(page, total: total, size: size)
        return "\(p * size + 1)–\(min(total, p * size + size)) of \(total)"
    }
}

/// Fixed panel geometry. The main panel never scrolls, so every region has a set height.
enum Layout {
    static let width: CGFloat = 400
    static let height: CGFloat = 680
    static let notice: CGFloat = 36
    static let chart: CGFloat = 48
    static let list: CGFloat = 112
    static let pager: CGFloat = 16
    static let taskPageSize = 2
    static let rowPageSize = 3
    static let taskRow: CGFloat = 40
    static let historyRow: CGFloat = 28
    static let groupRow: CGFloat = 26
}
