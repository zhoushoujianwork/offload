import CryptoKit
import XCTest
@testable import OffloadMonitor

final class ArchiveAndSwapTests: XCTestCase {
    var tmp: URL!
    let fm = FileManager.default

    override func setUpWithError() throws {
        tmp = fm.temporaryDirectory.appendingPathComponent("om-test-\(UUID().uuidString)", isDirectory: true)
        try fm.createDirectory(at: tmp, withIntermediateDirectories: true)
    }
    override func tearDownWithError() throws { try? fm.removeItem(at: tmp) }

    // MARK: helpers

    func makeApp(at parent: URL, version: String = "0.2.0", bundleID: String = AppIdentity.bundleID,
                 cli: Bool = true, exe: Bool = true, marker: String = "new") throws -> URL {
        let app = parent.appendingPathComponent(AppIdentity.appBundleName, isDirectory: true)
        try fm.createDirectory(at: app.appendingPathComponent("Contents/MacOS"), withIntermediateDirectories: true)
        try fm.createDirectory(at: app.appendingPathComponent("Contents/Resources"), withIntermediateDirectories: true)
        let plist: [String: Any] = ["CFBundleIdentifier": bundleID, "CFBundleShortVersionString": version, "CFBundleExecutable": "OffloadMonitor"]
        try PropertyListSerialization.data(fromPropertyList: plist, format: .xml, options: 0).write(to: app.appendingPathComponent("Contents/Info.plist"))
        func script(_ rel: String) throws {
            let u = app.appendingPathComponent(rel)
            try "#!/bin/sh\n".write(to: u, atomically: true, encoding: .utf8)
            try fm.setAttributes([.posixPermissions: 0o755], ofItemAtPath: u.path)
        }
        if exe { try script("Contents/MacOS/OffloadMonitor") }
        if cli { try script("Contents/Resources/offload") }
        try marker.write(to: app.appendingPathComponent("Contents/Resources/marker.txt"), atomically: true, encoding: .utf8)
        return app
    }

    func zip(_ dir: URL, entries: [String], to name: String = "a.zip") throws -> URL {
        let out = tmp.appendingPathComponent(name)
        let p = Process()
        p.executableURL = URL(fileURLWithPath: "/usr/bin/zip")
        p.currentDirectoryURL = dir
        p.arguments = ["-qry", out.path] + entries
        try p.run(); p.waitUntilExit()
        XCTAssertEqual(p.terminationStatus, 0)
        return out
    }

    func candidate(for zip: URL, version: String = "0.2.0", sha: String? = nil) throws -> UpdateCandidate {
        UpdateCandidate(version: SemanticVersion(version)!, tag: "v\(version)", assetName: "x.zip",
                        downloadURL: URL(string: "https://github.com/x")!, sha256: try sha ?? ArchiveValidator.sha256Hex(of: zip))
    }

    func stage(_ zip: URL, candidate c: UpdateCandidate) async throws -> UpdateService.Staged {
        let installed = tmp.appendingPathComponent("Applications", isDirectory: true)
        try fm.createDirectory(at: installed, withIntermediateDirectories: true)
        return try await UpdateService.stage(zip: zip, candidate: c, beside: installed.appendingPathComponent(AppIdentity.appBundleName), verifySignature: false)
    }

    func assertBadArchive(_ zip: URL, _ file: StaticString = #filePath, _ line: UInt = #line) async {
        do {
            _ = try await stage(zip, candidate: try candidate(for: zip))
            XCTFail("expected failure", file: file, line: line)
        } catch {
            XCTAssertTrue(error is UpdateError, "\(error)", file: file, line: line)
        }
        let leftovers = (try? fm.contentsOfDirectory(atPath: tmp.appendingPathComponent("Applications").path)) ?? []
        XCTAssertTrue(leftovers.isEmpty, "staging left \(leftovers)", file: file, line: line)
    }

    // MARK: digest

    func testDigestMatchesKnownVector() throws {
        let f = tmp.appendingPathComponent("abc")
        try Data("abc".utf8).write(to: f)
        XCTAssertEqual(try ArchiveValidator.sha256Hex(of: f), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
    }

    func testDigestMismatchRefusesBeforeExtraction() async throws {
        let src = tmp.appendingPathComponent("src"); try fm.createDirectory(at: src, withIntermediateDirectories: true)
        _ = try makeApp(at: src)
        let z = try zip(src, entries: [AppIdentity.appBundleName])
        let wrong = String(repeating: "0", count: 64)
        do {
            _ = try await stage(z, candidate: try candidate(for: z, sha: wrong))
            XCTFail()
        } catch { XCTAssertEqual(error as? UpdateError, .digestMismatch) }
        let leftovers = try fm.contentsOfDirectory(atPath: tmp.appendingPathComponent("Applications").path)
        XCTAssertTrue(leftovers.isEmpty)
    }

    // MARK: names

    func testEntryNameValidation() {
        XCTAssertNoThrow(try ArchiveValidator.validateEntryNames(["Offload Monitor.app/", "Offload Monitor.app/Contents/Info.plist"]))
        for bad in [["/etc/passwd"], ["Offload Monitor.app/../x"], ["../x"], ["Other.app/Contents"], ["x"],
                    ["Offload Monitor.app/a\\b"], ["Offload Monitor.app/./a"], ["~/x"], [], ["Offload Monitor.app/a\nb"]] {
            XCTAssertThrowsError(try ArchiveValidator.validateEntryNames(bad), "\(bad)")
        }
    }

    func testParsers() {
        XCTAssertEqual(ArchiveValidator.parseUncompressedSize(fromTotals: "12 files, 345678 bytes uncompressed, 123456 bytes compressed:  64.3%"), 345678)
        XCTAssertNil(ArchiveValidator.parseUncompressedSize(fromTotals: "garbage"))
        let listing = "-rw-a--     2.1 unx      12 tx stor 24-Oct-29 10:00 Offload Monitor.app/a.txt\nlrwxr-xr-x  2.1 unx      11 bx stor 24-Oct-29 10:00 Offload Monitor.app/link"
        XCTAssertEqual(ArchiveValidator.symlinkEntries(inShortListing: listing), ["Offload Monitor.app/link"])
    }

    // MARK: whole-archive cases

    func testValidArchiveStagesAndVerifies() async throws {
        let src = tmp.appendingPathComponent("src"); try fm.createDirectory(at: src, withIntermediateDirectories: true)
        _ = try makeApp(at: src)
        let z = try zip(src, entries: [AppIdentity.appBundleName])
        let staged = try await stage(z, candidate: try candidate(for: z))
        XCTAssertTrue(fm.isExecutableFile(atPath: staged.bundle.appendingPathComponent("Contents/Resources/offload").path))
        XCTAssertEqual(staged.bundle.deletingLastPathComponent(), staged.directory)
    }

    func testInternalSymlinkAllowedEscapingSymlinkRejected() async throws {
        let src = tmp.appendingPathComponent("src"); try fm.createDirectory(at: src, withIntermediateDirectories: true)
        let app = try makeApp(at: src)
        try fm.createSymbolicLink(atPath: app.appendingPathComponent("Contents/Resources/ok").path, withDestinationPath: "marker.txt")
        // Validator on the extracted tree accepts internal links.
        XCTAssertNoThrow(try ArchiveValidator.validateExtractedTree(root: src))
        try fm.removeItem(at: app.appendingPathComponent("Contents/Resources/ok"))
        try fm.createSymbolicLink(atPath: app.appendingPathComponent("Contents/Resources/esc").path, withDestinationPath: "../../../../outside")
        XCTAssertThrowsError(try ArchiveValidator.validateExtractedTree(root: src))
        try fm.removeItem(at: app.appendingPathComponent("Contents/Resources/esc"))
        try fm.createSymbolicLink(atPath: app.appendingPathComponent("Contents/Resources/abs").path, withDestinationPath: "/usr/bin/true")
        XCTAssertThrowsError(try ArchiveValidator.validateExtractedTree(root: src))
        // And the zip-level check refuses any symlink before extraction.
        let z = try zip(src, entries: [AppIdentity.appBundleName])
        await assertBadArchive(z)
    }

    func testArchiveWithExtraTopLevelOrWrongName() async throws {
        let src = tmp.appendingPathComponent("src"); try fm.createDirectory(at: src, withIntermediateDirectories: true)
        _ = try makeApp(at: src)
        try "x".write(to: src.appendingPathComponent("extra.txt"), atomically: true, encoding: .utf8)
        await assertBadArchive(try zip(src, entries: [AppIdentity.appBundleName, "extra.txt"], to: "extra.zip"))
        let other = tmp.appendingPathComponent("other"); try fm.createDirectory(at: other, withIntermediateDirectories: true)
        try fm.moveItem(at: try makeApp(at: other), to: other.appendingPathComponent("Evil.app"))
        await assertBadArchive(try zip(other, entries: ["Evil.app"], to: "evil.zip"))
    }

    func testTraversalArchiveRejected() async throws {
        let work = tmp.appendingPathComponent("w"); let inner = work.appendingPathComponent("inner")
        try fm.createDirectory(at: inner, withIntermediateDirectories: true)
        try "x".write(to: work.appendingPathComponent("evil.txt"), atomically: true, encoding: .utf8)
        let z = try zip(inner, entries: ["../evil.txt"], to: "trav.zip") // zip stores the literal ../ with -j absent
        await assertBadArchive(z)
        XCTAssertFalse(fm.fileExists(atPath: tmp.appendingPathComponent("evil.txt").path))
    }

    func testBundleValidation() async throws {
        func bundleCase(_ make: (URL) throws -> Void, expect: String) async throws {
            let src = tmp.appendingPathComponent("b-\(UUID().uuidString)"); try fm.createDirectory(at: src, withIntermediateDirectories: true)
            try make(src)
            let z = try zip(src, entries: [AppIdentity.appBundleName], to: "\(UUID().uuidString).zip")
            do {
                _ = try await stage(z, candidate: try candidate(for: z))
                XCTFail("expected \(expect)")
            } catch let e as UpdateError {
                guard case .badBundle(let m) = e else { return XCTFail("\(e)") }
                XCTAssertTrue(m.contains(expect), m)
            }
        }
        try await bundleCase({ _ = try self.makeApp(at: $0, bundleID: "com.other.app") }, expect: "bundle identifier")
        try await bundleCase({ _ = try self.makeApp(at: $0, version: "0.9.0") }, expect: "version is 0.9.0")
        try await bundleCase({ _ = try self.makeApp(at: $0, version: "0.2.0-rc1") }, expect: "release version")
        try await bundleCase({ _ = try self.makeApp(at: $0, cli: false) }, expect: "Contents/Resources/offload")
        try await bundleCase({ _ = try self.makeApp(at: $0, exe: false) }, expect: "Contents/MacOS/OffloadMonitor")
    }

    // MARK: swap and rollback

    struct Boom: Error {}

    func swapFixture() throws -> (cur: URL, staged: URL, backup: URL) {
        let apps = tmp.appendingPathComponent("Apps"); try fm.createDirectory(at: apps, withIntermediateDirectories: true)
        let stagingDir = apps.appendingPathComponent(".staging"); try fm.createDirectory(at: stagingDir, withIntermediateDirectories: true)
        let cur = try makeApp(at: apps, version: "0.1.0", marker: "old")
        let staged = try makeApp(at: stagingDir, version: "0.2.0", marker: "new")
        return (cur, staged, apps.appendingPathComponent(".Offload Monitor.app.backup"))
    }
    func marker(_ app: URL) -> String? { try? String(contentsOf: app.appendingPathComponent("Contents/Resources/marker.txt")) }

    func testSwapSucceeds() throws {
        let f = try swapFixture()
        var launched: URL?
        let out = AppSwapper(current: f.cur, staged: f.staged, backup: f.backup, relaunch: { launched = $0 }).run()
        XCTAssertEqual(out, .installed)
        XCTAssertEqual(marker(f.cur), "new")
        XCTAssertEqual(launched, f.cur)
        XCTAssertFalse(fm.fileExists(atPath: f.backup.path))
    }

    func testRelaunchFailureRollsBack() throws {
        let f = try swapFixture()
        var calls: [String?] = []
        let out = AppSwapper(current: f.cur, staged: f.staged, backup: f.backup, relaunch: { [self] url in
            calls.append(marker(url)); if calls.count == 1 { throw Boom() }
        }).run()
        guard case .rolledBack = out else { return XCTFail("\(out)") }
        XCTAssertEqual(marker(f.cur), "old")
        XCTAssertEqual(calls, ["new", "old"]) // the old app is relaunched after rollback
        XCTAssertFalse(fm.fileExists(atPath: f.backup.path))
    }

    func testMissingStagedChangesNothing() throws {
        let f = try swapFixture()
        try fm.removeItem(at: f.staged)
        let out = AppSwapper(current: f.cur, staged: f.staged, backup: f.backup, relaunch: { _ in XCTFail() }).run()
        guard case .rolledBack = out else { return XCTFail("\(out)") }
        XCTAssertEqual(marker(f.cur), "old")
    }

    func testFailedMoveIntoPlaceRestoresOldApp() throws {
        let f = try swapFixture()
        var swapper = AppSwapper(current: f.cur, staged: f.staged, backup: f.backup, relaunch: { _ in })
        swapper.fileManager = FailingMoveFileManager(failDestination: f.cur)
        let out = swapper.run()
        guard case .rolledBack = out else { return XCTFail("\(out)") }
        XCTAssertEqual(marker(f.cur), "old")
        XCTAssertTrue(fm.fileExists(atPath: f.staged.path))
        XCTAssertFalse(fm.fileExists(atPath: f.backup.path))
    }

    func testExistingBackupBlocksSwap() throws {
        let f = try swapFixture()
        try fm.createDirectory(at: f.backup, withIntermediateDirectories: true)
        let out = AppSwapper(current: f.cur, staged: f.staged, backup: f.backup, relaunch: { _ in XCTFail() }).run()
        guard case .rolledBack = out else { return XCTFail("\(out)") }
        XCTAssertEqual(marker(f.cur), "old")
    }

    func testUpdateReportRoundTrip() {
        let url = tmp.appendingPathComponent("r/update-result.json")
        UpdateReport(ok: false, version: "0.2.0", message: "m").write(to: url)
        XCTAssertEqual(UpdateReport.consume(from: url), UpdateReport(ok: false, version: "0.2.0", message: "m"))
        XCTAssertNil(UpdateReport.consume(from: url))
    }

    func testInstallability() {
        XCTAssertNotNil(UpdateService.installability(of: URL(fileURLWithPath: "/tmp/.build/debug/OffloadMonitor")))
        XCTAssertNotNil(UpdateService.installability(of: URL(fileURLWithPath: "/private/var/folders/x/AppTranslocation/y/d/Offload Monitor.app")))
        XCTAssertNil(UpdateService.installability(of: tmp.appendingPathComponent("Offload Monitor.app")))
    }
}

/// Fails the move whose destination is `failDestination`, to exercise the rollback path.
final class FailingMoveFileManager: FileManager {
    let failDestination: URL
    init(failDestination: URL) { self.failDestination = failDestination; super.init() }
    override func moveItem(at srcURL: URL, to dstURL: URL) throws {
        if dstURL.standardizedFileURL == failDestination.standardizedFileURL, !fileExists(atPath: dstURL.path) == true,
           srcURL.lastPathComponent == failDestination.lastPathComponent, srcURL.deletingLastPathComponent() != failDestination.deletingLastPathComponent() {
            throw CocoaError(.fileWriteNoPermission)
        }
        try super.moveItem(at: srcURL, to: dstURL)
    }
}
