import CryptoKit
import Foundation

enum ArchiveValidator {
    static let topLevel = AppIdentity.appBundleName
    static let maxUncompressedBytes: Int64 = 600_000_000
    static let maxEntries = 5_000

    // MARK: Digest

    /// SHA-256 of a file as lowercase hex, read in chunks.
    static func sha256Hex(of url: URL) throws -> String {
        let handle = try FileHandle(forReadingFrom: url)
        defer { try? handle.close() }
        var hasher = SHA256()
        while let chunk = try handle.read(upToCount: 1 << 20), !chunk.isEmpty {
            hasher.update(data: chunk)
        }
        return hasher.finalize().map { String(format: "%02x", $0) }.joined()
    }

    static func verifyDigest(file: URL, expectedHex: String) throws {
        let actual = try sha256Hex(of: file)
        guard actual == expectedHex.lowercased() else { throw UpdateError.digestMismatch }
    }

    // MARK: Entry names

    /// Every entry must live under "Offload Monitor.app/" with no absolute path, "..", backslash or control character.
    static func validateEntryNames(_ names: [String]) throws {
        guard !names.isEmpty else { throw UpdateError.badArchive("it is empty") }
        guard names.count <= maxEntries else { throw UpdateError.badArchive("it has too many entries") }
        for name in names {
            guard !name.isEmpty else { throw UpdateError.badArchive("it has an empty entry name") }
            if name.hasPrefix("/") || name.hasPrefix("~") { throw UpdateError.badArchive("absolute path \"\(name)\"") }
            if name.contains("\\") || name.unicodeScalars.contains(where: { $0.value < 0x20 || $0.value == 0x7f }) {
                throw UpdateError.badArchive("unsafe characters in \"\(name.debugDescription)\"")
            }
            let parts = name.split(separator: "/", omittingEmptySubsequences: false)
            if parts.contains("..") || parts.contains(".") { throw UpdateError.badArchive("path traversal in \"\(name)\"") }
            guard parts.first.map(String.init) == topLevel else {
                throw UpdateError.badArchive("unexpected top-level entry \"\(name)\"")
            }
        }
    }

    /// Parses `unzip -Z` (short format) lines; returns names flagged as symbolic links.
    static func symlinkEntries(inShortListing listing: String) -> [String] {
        var result: [String] = []
        for line in listing.split(separator: "\n") {
            guard line.first == "l" else { continue }
            let fields = line.split(separator: " ", maxSplits: 8, omittingEmptySubsequences: true)
            result.append(fields.count == 9 ? String(fields[8]) : String(line))
        }
        return result
    }

    static func parseUncompressedSize(fromTotals line: String) -> Int64? {
        // "12 files, 345678 bytes uncompressed, 123456 bytes compressed:  64.3%"
        let tokens = line.split(whereSeparator: { $0 == " " || $0 == "," })
        guard let i = tokens.firstIndex(of: "uncompressed"), i >= 2, tokens[i - 1] == "bytes" else { return nil }
        return Int64(tokens[i - 2])
    }

    // MARK: Extraction

    /// Inspects `zip`, then extracts it into `destination` (which must already exist and be empty).
    static func extract(zip: URL, into destination: URL) async throws {
        let unzip = URL(fileURLWithPath: "/usr/bin/unzip")
        let names = try await run(unzip, ["-Z1", zip.path])
        try validateEntryNames(names.split(separator: "\n", omittingEmptySubsequences: true).map(String.init))

        let short = try await run(unzip, ["-Z", zip.path])
        let links = symlinkEntries(inShortListing: short)
        if let first = links.first { throw UpdateError.badArchive("it contains a symbolic link (\(first))") }

        let totals = try await run(unzip, ["-Zt", zip.path])
        guard let size = parseUncompressedSize(fromTotals: totals.trimmingCharacters(in: .whitespacesAndNewlines)) else {
            throw UpdateError.badArchive("its size could not be determined")
        }
        guard size <= maxUncompressedBytes else { throw UpdateError.badArchive("it expands to more than the allowed size") }

        _ = try await run(URL(fileURLWithPath: "/usr/bin/ditto"), ["-x", "-k", zip.path, destination.path])
        try validateExtractedTree(root: destination)
    }

    private static func run(_ exe: URL, _ args: [String]) async throws -> String {
        let out: ProcessOutput
        do { out = try await ProcessRunner.run(executable: exe, arguments: args, timeout: 120, maxOutput: 4_000_000) }
        catch { throw UpdateError.badArchive("\(exe.lastPathComponent) could not run: \(error.localizedDescription)") }
        guard out.status == 0, !out.timedOut, !out.truncated else {
            let msg = String(decoding: out.stderr.prefix(200), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            throw UpdateError.badArchive(msg.isEmpty ? "\(exe.lastPathComponent) failed" : msg)
        }
        return String(decoding: out.stdout, as: UTF8.self)
    }

    // MARK: Extracted tree

    /// `root` must contain exactly the app bundle, only regular files, directories and symlinks that stay inside it.
    static func validateExtractedTree(root: URL) throws {
        let fm = FileManager.default
        let top = try fm.contentsOfDirectory(atPath: root.path)
        guard top == [topLevel] else { throw UpdateError.badArchive("it must contain only \(topLevel)") }
        let bundleRoot = root.appendingPathComponent(topLevel).standardizedFileURL.path
        guard let walker = fm.enumerator(atPath: root.path) else { throw UpdateError.badArchive("it could not be read") }
        for case let rel as String in walker {
            let full = (root.path as NSString).appendingPathComponent(rel)
            let attrs = try fm.attributesOfItem(atPath: full)
            switch attrs[.type] as? FileAttributeType {
            case .typeSymbolicLink:
                let dest = try fm.destinationOfSymbolicLink(atPath: full)
                if dest.hasPrefix("/") { throw UpdateError.badBundle("symbolic link \(rel) points outside the app") }
                let parent = (full as NSString).deletingLastPathComponent
                let resolved = URL(fileURLWithPath: dest, relativeTo: URL(fileURLWithPath: parent, isDirectory: true)).standardizedFileURL.path
                if resolved != bundleRoot && !resolved.hasPrefix(bundleRoot + "/") {
                    throw UpdateError.badBundle("symbolic link \(rel) points outside the app")
                }
            case .typeRegular, .typeDirectory:
                break
            default:
                throw UpdateError.badArchive("unsupported file type at \(rel)")
            }
        }
    }

    // MARK: Bundle

    /// Checks identity, version and executables of an extracted bundle.
    static func validateBundle(_ bundle: URL, expectedVersion: SemanticVersion, allowLocalDevelopment: Bool = false) throws {
        let fm = FileManager.default
        let plistURL = bundle.appendingPathComponent("Contents/Info.plist")
        guard let data = try? Data(contentsOf: plistURL),
              let plist = try? PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any]
        else { throw UpdateError.badBundle("Info.plist is missing or unreadable") }
        guard plist["CFBundleIdentifier"] as? String == AppIdentity.bundleID else {
            throw UpdateError.badBundle("bundle identifier is not \(AppIdentity.bundleID)")
        }
        guard let v = plist["CFBundleShortVersionString"] as? String, let version = SemanticVersion(v),
              !version.isPrerelease || (allowLocalDevelopment && v.range(of: #"^[0-9]+(?:\.[0-9]+){1,3}-dev\.[0-9]+\.[a-f0-9]+(?:\.dirty)?$"#, options: .regularExpression) != nil) else {
            throw UpdateError.badBundle("version is missing or not a release version")
        }
        guard version == expectedVersion else {
            throw UpdateError.badBundle("version is \(v) but the release is \(expectedVersion)")
        }
        guard let exe = plist["CFBundleExecutable"] as? String, !exe.isEmpty, !exe.contains("/") else {
            throw UpdateError.badBundle("executable name is missing")
        }
        for rel in ["Contents/MacOS/\(exe)", "Contents/Resources/offload"] {
            let p = bundle.appendingPathComponent(rel).path
            var isDir: ObjCBool = false
            guard fm.fileExists(atPath: p, isDirectory: &isDir), !isDir.boolValue, fm.isExecutableFile(atPath: p) else {
                throw UpdateError.badBundle("\(rel) is missing or not executable")
            }
        }
    }

    /// `codesign --verify` on the staged bundle; catches a corrupt or half-written bundle.
    static func verifySignature(_ bundle: URL) async throws {
        let out = try await ProcessRunner.run(executable: URL(fileURLWithPath: "/usr/bin/codesign"),
                                              arguments: ["--verify", "--deep", "--strict", bundle.path], timeout: 60)
        guard out.status == 0, !out.timedOut else {
            throw UpdateError.badBundle("its code signature does not verify")
        }
    }
}
