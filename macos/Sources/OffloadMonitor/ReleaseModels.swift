import Foundation

struct ReleaseAsset: Decodable, Equatable {
    var name: String
    var browserDownloadUrl: String
    var digest: String?
}

struct GitHubRelease: Decodable, Equatable {
    var tagName: String
    var draft: Bool
    var prerelease: Bool
    var assets: [ReleaseAsset]

    static func decode(_ data: Data) throws -> GitHubRelease {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        do { return try d.decode(GitHubRelease.self, from: data) } catch {
            throw UpdateError.badReleaseData
        }
    }
}

enum UpdateError: LocalizedError, Equatable {
    case badReleaseData
    case notStable(String)
    case badVersion(String)
    case noAsset(String)
    case missingDigest(String)
    case badDigest(String)
    case badURL(String)
    case badRedirect(String)
    case network(String)
    case httpStatus(Int)
    case tooLarge
    case digestMismatch
    case badArchive(String)
    case badBundle(String)
    case notInstallable(String)
    case helperFailed(String)

    var errorDescription: String? {
        switch self {
        case .badReleaseData: return "GitHub returned release data this app cannot read."
        case .notStable(let t): return "Release \(t) is a draft or pre-release and was ignored."
        case .badVersion(let t): return "Release tag \"\(t)\" is not a version number."
        case .noAsset(let n): return "The latest release has no \(n) download for this Mac."
        case .missingDigest(let n): return "GitHub published no SHA-256 digest for \(n), so the update was not installed. The download cannot be verified."
        case .badDigest(let n): return "The SHA-256 digest GitHub published for \(n) is malformed, so the update was not installed."
        case .badURL(let u): return "Refusing download address outside the offload release path: \(u)"
        case .badRedirect(let u): return "Refusing download redirect to an unexpected address: \(u)"
        case .network(let m): return "Network problem: \(m)"
        case .httpStatus(let c): return "GitHub answered with HTTP status \(c)."
        case .tooLarge: return "The download is larger than the allowed size."
        case .digestMismatch: return "The downloaded file does not match the SHA-256 digest published by GitHub. It was discarded."
        case .badArchive(let m): return "The update archive is invalid: \(m)"
        case .badBundle(let m): return "The update app bundle is invalid: \(m)"
        case .notInstallable(let m): return m
        case .helperFailed(let m): return "Could not start the installer: \(m)"
        }
    }
}

/// An update that passed every pre-download check.
struct UpdateCandidate: Equatable {
    var version: SemanticVersion
    var tag: String
    var assetName: String
    var downloadURL: URL
    var sha256: String
}

enum Arch {
    static var current: String {
        #if arch(arm64)
        return "arm64"
        #else
        return "amd64"
        #endif
    }
    static func assetName(for arch: String) -> String { "offload-monitor-macos-\(arch).zip" }
}

enum ReleasePolicy {
    static let allowedRedirectSuffix = ".githubusercontent.com"

    static func downloadPath(tag: String, asset: String) -> String {
        "/\(AppIdentity.repoOwner)/\(AppIdentity.repoName)/releases/download/\(tag)/\(asset)"
    }

    /// The URL GitHub lists for the asset must be exactly the repo's release download path.
    static func validateAssetURL(_ raw: String, tag: String, asset: String) throws -> URL {
        guard let url = URL(string: raw),
              url.scheme == "https", url.host == "github.com",
              url.port == nil, url.user == nil, url.password == nil,
              url.query == nil, url.fragment == nil,
              url.path == downloadPath(tag: tag, asset: asset)
        else { throw UpdateError.badURL(raw) }
        return url
    }

    /// Redirect targets (GitHub serves assets from its CDN) must stay on HTTPS and GitHub hosts.
    static func validateRedirect(_ url: URL) throws {
        guard url.scheme == "https", url.user == nil, url.port == nil, let host = url.host?.lowercased(),
              host == "github.com" || host == "api.github.com" || host.hasSuffix(allowedRedirectSuffix)
        else { throw UpdateError.badRedirect(url.absoluteString) }
    }

    /// Returns the digest as 64 lowercase hex characters.
    static func parseDigest(_ digest: String?, asset: String) throws -> String {
        guard let digest, !digest.trimmingCharacters(in: .whitespaces).isEmpty else { throw UpdateError.missingDigest(asset) }
        let lower = digest.lowercased()
        guard lower.hasPrefix("sha256:") else { throw UpdateError.badDigest(asset) }
        let hex = String(lower.dropFirst("sha256:".count))
        guard hex.count == 64, hex.allSatisfy({ $0.isHexDigit && $0.isASCII }) else { throw UpdateError.badDigest(asset) }
        return hex
    }

    /// Nil means the release is not newer than `current`.
    static func candidate(from release: GitHubRelease, current: SemanticVersion, arch: String) throws -> UpdateCandidate? {
        if release.draft || release.prerelease { throw UpdateError.notStable(release.tagName) }
        guard let version = SemanticVersion(release.tagName) else { throw UpdateError.badVersion(release.tagName) }
        if version.isPrerelease { throw UpdateError.notStable(release.tagName) }
        guard version > current else { return nil }
        let name = Arch.assetName(for: arch)
        guard let asset = release.assets.first(where: { $0.name == name }) else { throw UpdateError.noAsset(name) }
        let sha = try parseDigest(asset.digest, asset: name)
        let url = try validateAssetURL(asset.browserDownloadUrl, tag: release.tagName, asset: name)
        return UpdateCandidate(version: version, tag: release.tagName, assetName: name, downloadURL: url, sha256: sha)
    }
}
