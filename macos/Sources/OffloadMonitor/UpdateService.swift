import Foundation

/// Rejects redirects that leave HTTPS or GitHub hosts.
final class RedirectGuard: NSObject, URLSessionTaskDelegate, @unchecked Sendable {
    func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
                    newRequest request: URLRequest, completionHandler: @escaping (URLRequest?) -> Void) {
        guard let url = request.url, (try? ReleasePolicy.validateRedirect(url)) != nil else {
            completionHandler(nil) // the 3xx response is then surfaced and rejected as an HTTP status
            return
        }
        completionHandler(request)
    }
}

/// Network and staging work for updates. Every method is async and runs off the main thread.
struct UpdateService {
    static let maxDownloadBytes: Int64 = 300_000_000
    static let maxReleaseJSONBytes = 2_000_000

    var session: URLSession = {
        let c = URLSessionConfiguration.ephemeral
        c.timeoutIntervalForRequest = 30
        c.timeoutIntervalForResource = 600
        c.waitsForConnectivity = false
        return URLSession(configuration: c)
    }()

    // MARK: Check

    func fetchLatestRelease(from url: URL = AppIdentity.latestReleaseURL) async throws -> GitHubRelease {
        var req = URLRequest(url: url)
        req.setValue("application/vnd.github+json", forHTTPHeaderField: "Accept")
        req.setValue("OffloadMonitor", forHTTPHeaderField: "User-Agent")
        req.setValue("2022-11-28", forHTTPHeaderField: "X-GitHub-Api-Version")
        do {
            let (data, resp) = try await session.data(for: req, delegate: RedirectGuard())
            try Self.checkStatus(resp)
            guard data.count <= Self.maxReleaseJSONBytes else { throw UpdateError.badReleaseData }
            return try GitHubRelease.decode(data)
        } catch let e as UpdateError {
            throw e
        } catch {
            throw UpdateError.network(error.localizedDescription)
        }
    }

    static func checkStatus(_ resp: URLResponse) throws {
        guard let http = resp as? HTTPURLResponse else { throw UpdateError.network("no HTTP response") }
        guard (200..<300).contains(http.statusCode) else { throw UpdateError.httpStatus(http.statusCode) }
    }

    // MARK: Download

    /// Downloads to a temporary file; the caller owns the returned file and must delete it.
    func download(_ candidate: UpdateCandidate) async throws -> URL {
        var req = URLRequest(url: candidate.downloadURL)
        req.setValue("OffloadMonitor", forHTTPHeaderField: "User-Agent")
        let tmp: URL, resp: URLResponse
        do { (tmp, resp) = try await session.download(for: req, delegate: RedirectGuard()) }
        catch { throw UpdateError.network(error.localizedDescription) }
        do {
            try Self.checkStatus(resp)
            if let final = resp.url { try ReleasePolicy.validateRedirect(final) }
            let size = (try? FileManager.default.attributesOfItem(atPath: tmp.path)[.size] as? Int64) ?? nil
            guard let size, size > 0, size <= Self.maxDownloadBytes else { throw UpdateError.tooLarge }
            return tmp
        } catch {
            try? FileManager.default.removeItem(at: tmp)
            throw error
        }
    }

    // MARK: Stage

    struct Staged: Equatable {
        var directory: URL
        var bundle: URL
    }

    /// Verifies the digest, extracts next to `installedApp`, validates the bundle. On any failure the staging
    /// directory is removed and the installed app is untouched.
    static func stage(zip: URL, candidate: UpdateCandidate, beside installedApp: URL, verifySignature: Bool = true) async throws -> Staged {
        try ArchiveValidator.verifyDigest(file: zip, expectedHex: candidate.sha256) // before any extraction
        let fm = FileManager.default
        let parent = installedApp.deletingLastPathComponent()
        let dir = parent.appendingPathComponent(".OffloadMonitor-update-\(UUID().uuidString)", isDirectory: true)
        do {
            try fm.createDirectory(at: dir, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
            try await ArchiveValidator.extract(zip: zip, into: dir)
            let bundle = dir.appendingPathComponent(ArchiveValidator.topLevel, isDirectory: true)
            try ArchiveValidator.validateBundle(bundle, expectedVersion: candidate.version)
            if verifySignature { try await ArchiveValidator.verifySignature(bundle) }
            return Staged(directory: dir, bundle: bundle)
        } catch {
            try? fm.removeItem(at: dir)
            throw error
        }
    }

    /// Whether this app can replace itself: a real .app in a writable folder, not run from a translocated or read-only place.
    static func installability(of app: URL) -> String? {
        guard app.pathExtension == "app" else { return "Updates can only be installed when running Offload Monitor.app." }
        if app.path.contains("/AppTranslocation/") {
            return "macOS is running the app from a temporary location. Move Offload Monitor to the Applications folder and reopen it."
        }
        let parent = app.deletingLastPathComponent().path
        guard FileManager.default.isWritableFile(atPath: parent) else {
            return "The folder \(parent) is not writable, so the app cannot replace itself there. Download the update manually."
        }
        return nil
    }

    // MARK: Hand-off

    /// Copies this executable out of the bundle and starts it as the install helper. The caller then quits.
    static func launchHelper(staged: Staged, installedApp: URL, version: SemanticVersion) throws {
        let fm = FileManager.default
        guard let exe = Bundle.main.executableURL else { throw UpdateError.helperFailed("executable not found") }
        let helper = staged.directory.appendingPathComponent("install-helper")
        let backup = installedApp.deletingLastPathComponent()
            .appendingPathComponent(".\(installedApp.lastPathComponent).backup", isDirectory: true)
        do {
            try fm.copyItem(at: exe, to: helper)
            let p = Process()
            p.executableURL = helper
            p.arguments = [UpdateHelper.flag, String(ProcessInfo.processInfo.processIdentifier), installedApp.path,
                           staged.bundle.path, backup.path, version.description]
            p.standardInput = FileHandle.nullDevice
            p.standardOutput = FileHandle.nullDevice
            p.standardError = FileHandle.nullDevice
            try p.run()
        } catch {
            throw UpdateError.helperFailed(error.localizedDescription)
        }
    }
}
