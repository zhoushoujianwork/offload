import AppKit
import SwiftUI
import XCTest
@testable import OffloadMonitor

/// Temp directory tree standing in for a Mac: a fake App bundle, ~/go/bin, a Homebrew prefix and a manual bin.
final class CLIFixture {
    let root: URL
    let home: URL
    let goBin: URL
    let brewBin: URL
    let manualBin: URL
    let resources: URL

    init() throws {
        let fm = FileManager.default
        root = URL(fileURLWithPath: CLIClassifier.realPath(NSTemporaryDirectory())).appendingPathComponent("cli-\(UUID().uuidString)")
        home = root.appendingPathComponent("home with space")
        goBin = home.appendingPathComponent("go/bin")
        brewBin = root.appendingPathComponent("Cellar/offload/0.1/bin")
        manualBin = root.appendingPathComponent("manual-bin")
        resources = root.appendingPathComponent("Offload Monitor.app/Contents/Resources")
        for d in [goBin, brewBin, manualBin, resources] { try fm.createDirectory(at: d, withIntermediateDirectories: true) }
    }

    deinit { try? FileManager.default.removeItem(at: root) }

    /// The "version" a fake binary reports is its own first line, which keeps probes trivial and deterministic.
    @discardableResult
    func binary(_ url: URL, version: String, mode: Int = 0o755) throws -> URL {
        try Data("\(version)\n".utf8).write(to: url)
        try FileManager.default.setAttributes([.posixPermissions: mode], ofItemAtPath: url.path)
        return url
    }

    var bundled: URL { resources.appendingPathComponent("offload") }
    var fileProbe: @Sendable (URL) async -> String? {
        { url in (try? String(contentsOf: url, encoding: .utf8))?.split(separator: "\n").first.map(String.init) }
    }

    func scanner(path: [URL]) -> CLIScanner {
        CLIScanner(environment: ["PATH": path.map(\.path).joined(separator: ":")], home: home.path,
                   bundled: bundled, probe: fileProbe)
    }

    func syncer(backups: URL? = nil) -> CLISyncer {
        CLISyncer(backupDirectory: backups ?? root.appendingPathComponent("backups"), probe: fileProbe,
                  verifySignature: { _ in }, home: home.path, environment: [:])
    }
}

final class CLIVersionComparisonTests: XCTestCase {
    func testRelations() {
        typealias R = CLIVersionRelation
        XCTAssertEqual(R.compare(standalone: "0.9.3", bundled: "0.9.3"), .same)
        XCTAssertEqual(R.compare(standalone: "v0.9.3", bundled: "0.9.3"), .same)
        XCTAssertEqual(R.compare(standalone: "0.9.2", bundled: "0.9.3"), .behind)
        XCTAssertEqual(R.compare(standalone: "0.10.0", bundled: "0.9.3"), .ahead)
        XCTAssertEqual(R.compare(standalone: nil, bundled: "0.9.3"), .unreadable)
        XCTAssertEqual(R.compare(standalone: "0.9.3", bundled: nil), .bundledUnknown)
        XCTAssertEqual(R.compare(standalone: "0.9.3", bundled: "dev"), .bundledUnknown)
    }

    /// The exact machine from the issue: the PATH copy prints "dev" (a Go pseudo-version build), the App ships 0.9.3.
    func testIssueMachineDevBuildIsNotTreatedAsCurrent() {
        XCTAssertEqual(CLIVersionRelation.compare(standalone: "dev", bundled: "0.9.3"), .unknown)
        XCTAssertEqual(CLIVersionRelation.compare(standalone: "0.3.1-0.20261029152610-ee7d93d6c6dc", bundled: "0.9.3"), .behind)
    }

    func testSameNumberDifferentBuildIsNotOrdered() {
        XCTAssertEqual(CLIVersionRelation.compare(standalone: "0.9.3-dev.20260101.abc", bundled: "0.9.3"), .differentBuild)
    }

    func testVersionOutputSanitizing() {
        XCTAssertEqual(CLIScanner.sanitizeVersion("0.9.3\n"), "0.9.3")
        XCTAssertEqual(CLIScanner.sanitizeVersion("dev\nextra"), "dev")
        for bad in ["", "\n", "x y", "0.9.3; rm -rf /", String(repeating: "1", count: 65), "\u{1b}[31m1.0"] {
            XCTAssertNil(CLIScanner.sanitizeVersion(bad), bad)
        }
    }
}

final class CLIClassificationTests: XCTestCase {
    func testManagementKinds() throws {
        let fx = try CLIFixture()
        let goCopy = try fx.binary(fx.goBin.appendingPathComponent("offload"), version: "0.3.1")
        let brewCopy = try fx.binary(fx.brewBin.appendingPathComponent("offload"), version: "0.1.0")
        let manual = try fx.binary(fx.manualBin.appendingPathComponent("offload"), version: "0.2.0")
        let link = fx.root.appendingPathComponent("linkbin/offload")
        try FileManager.default.createDirectory(at: link.deletingLastPathComponent(), withIntermediateDirectories: true)
        try FileManager.default.createSymbolicLink(at: link, withDestinationURL: manual)
        let bundled = try fx.binary(fx.bundled, version: "0.9.3")

        func kind(_ url: URL, env: [String: String] = [:]) -> CLIInstall.Management {
            let resolved = CLIClassifier.realPath(url.path)
            return CLIClassifier.management(path: url.path, resolved: resolved, isSymlink: CLIClassifier.isSymbolicLink(url.path),
                                            bundledResolved: CLIClassifier.realPath(bundled.path), home: fx.home.path, environment: env)
        }
        XCTAssertEqual(kind(goCopy), .goInstall)
        XCTAssertEqual(kind(brewCopy), .homebrew)
        XCTAssertEqual(kind(manual), .manual)
        XCTAssertEqual(kind(link), .symlink(target: CLIClassifier.realPath(manual.path)))
        XCTAssertEqual(kind(bundled), .appBundle)
        XCTAssertEqual(kind(manual, env: ["GOBIN": fx.manualBin.path]), .goInstall, "GOBIN counts as a Go install directory")
    }

    func testPackageManagerPrefixes() {
        XCTAssertEqual(CLIClassifier.packageManager(resolved: "/opt/homebrew/bin/offload"), .homebrew)
        XCTAssertEqual(CLIClassifier.packageManager(resolved: "/usr/local/Cellar/offload/1/bin/offload"), .homebrew)
        XCTAssertEqual(CLIClassifier.packageManager(resolved: "/opt/local/bin/offload"), .packageManager("MacPorts"))
        XCTAssertEqual(CLIClassifier.packageManager(resolved: "/nix/store/abc-offload/bin/offload"), .packageManager("Nix"))
        XCTAssertNil(CLIClassifier.packageManager(resolved: "/Users/me/bin/offload"))
    }

    func testShellQuoteSurvivesSpacesAndQuotes() {
        XCTAssertEqual(CLIAdvisor.shellQuote("/a b/it's"), "'/a b/it'\\''s'")
    }
}

final class CLIScannerTests: XCTestCase {
    func testFindsBundledAndStandaloneCopiesInPathOrder() async throws {
        let fx = try CLIFixture()
        try fx.binary(fx.bundled, version: "0.9.3")
        try fx.binary(fx.goBin.appendingPathComponent("offload"), version: "dev")
        try fx.binary(fx.manualBin.appendingPathComponent("offload"), version: "0.5.0")
        let report = await fx.scanner(path: [fx.goBin, fx.manualBin]).scan()
        XCTAssertEqual(report.bundled?.version, "0.9.3")
        XCTAssertEqual(report.bundled?.management, .appBundle)
        XCTAssertEqual(report.standalone.map(\.path), [fx.goBin.appendingPathComponent("offload").path, fx.manualBin.appendingPathComponent("offload").path])
        XCTAssertEqual(report.standalone.map(\.version), ["dev", "0.5.0"])
        XCTAssertEqual(report.standalone.map(\.management), [.goInstall, .manual])
        XCTAssertNotNil(report.standalone[0].digest)
        XCTAssertTrue(report.standalone[0].directoryWritable)
    }

    func testLinkToBundledCopyIsNotStandalone() async throws {
        let fx = try CLIFixture()
        try fx.binary(fx.bundled, version: "0.9.3")
        let link = fx.manualBin.appendingPathComponent("offload")
        try FileManager.default.createSymbolicLink(at: link, withDestinationURL: fx.bundled)
        let report = await fx.scanner(path: [fx.manualBin]).scan()
        XCTAssertTrue(report.standalone.isEmpty)
        XCTAssertEqual(report.bundledLinks, [link.path])
    }

    func testDuplicatePathEntriesAndNonExecutablesAreIgnored() async throws {
        let fx = try CLIFixture()
        try fx.binary(fx.bundled, version: "0.9.3")
        try fx.binary(fx.goBin.appendingPathComponent("offload"), version: "0.4.0")
        try fx.binary(fx.manualBin.appendingPathComponent("offload"), version: "0.1.0", mode: 0o644)
        let report = await fx.scanner(path: [fx.goBin, fx.goBin, fx.manualBin]).scan()
        XCTAssertEqual(report.standalone.count, 1)
    }

    func testNoBundledCopyStillListsPathCopies() async throws {
        let fx = try CLIFixture()
        try fx.binary(fx.goBin.appendingPathComponent("offload"), version: "0.4.0")
        let report = await fx.scanner(path: [fx.goBin]).scan()
        XCTAssertNil(report.bundled)
        XCTAssertEqual(report.standalone.count, 1)
    }

    /// The real probe: runs `offload version` as a child process and accepts only a clean single token.
    func testRealProbeRunsVersionAndRejectsBadOutput() async throws {
        let fx = try CLIFixture()
        func script(_ name: String, _ body: String) throws -> URL {
            let url = fx.manualBin.appendingPathComponent(name)
            try Data("#!/bin/sh\n\(body)\n".utf8).write(to: url)
            try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: url.path)
            return url
        }
        let good = try script("good", "[ \"$1\" = version ] && echo 0.9.3")
        let failing = try script("failing", "echo 1.0.0; exit 3")
        let noisy = try script("noisy", "echo 'not a version at all'")
        let version = await CLIScanner.runVersion(good)
        XCTAssertEqual(version, "0.9.3")
        let failed = await CLIScanner.runVersion(failing)
        XCTAssertNil(failed)
        let rejected = await CLIScanner.runVersion(noisy)
        XCTAssertNil(rejected)
    }
}

final class CLIAdvisorTests: XCTestCase {
    func install(_ fx: CLIFixture, _ url: URL, version: String?, _ kind: CLIInstall.Management, writable: Bool = true) throws -> CLIInstall {
        CLIInstall(path: url.path, resolvedPath: url.path, management: kind, version: version,
                   digest: try ArchiveValidator.sha256Hex(of: url), directoryWritable: writable)
    }

    func bundled(_ fx: CLIFixture) async throws -> CLIInstall {
        try fx.binary(fx.bundled, version: "0.9.3")
        return await fx.scanner(path: []).scan().bundled!
    }

    func testOlderGoInstallOffersUserChosenSyncWithAccurateText() async throws {
        let fx = try CLIFixture()
        let b = try await bundled(fx)
        let go = try install(fx, try fx.binary(fx.goBin.appendingPathComponent("offload"), version: "0.3.1"), version: "0.3.1", .goInstall)
        let a = CLIAdvisor.assess(go, bundled: b)
        XCTAssertEqual(a.relation, .behind)
        XCTAssertEqual(a.tone, .attention)
        XCTAssertTrue(a.headline.contains("0.3.1") && a.headline.contains("0.9.3"), a.headline)
        XCTAssertTrue(a.detail.contains("go install github.com/zhoushoujianwork/offload@latest"), "tells Go users their own path")
        guard case .sync(let plan) = a.action else { return XCTFail("expected a sync offer, got \(a.action)") }
        XCTAssertEqual(plan.path, go.path)
        XCTAssertEqual(plan.toVersion, "0.9.3")
        XCTAssertEqual(plan.bundledDigest, b.digest)
        XCTAssertEqual(plan.targetDigest, go.digest)
    }

    func testDevBuildIsFlaggedNotAssumedCurrent() async throws {
        let fx = try CLIFixture()
        let b = try await bundled(fx)
        let dev = try install(fx, try fx.binary(fx.goBin.appendingPathComponent("offload"), version: "dev"), version: "dev", .goInstall)
        let a = CLIAdvisor.assess(dev, bundled: b)
        XCTAssertEqual(a.relation, .unknown)
        XCTAssertEqual(a.tone, .attention)
        XCTAssertTrue(a.headline.contains("dev"))
        if case .sync = a.action {} else { XCTFail("a Go-installed dev build may be synced after confirmation") }
    }

    func testHomebrewAndLinksAndPackageManagersNeverGetASyncAction() async throws {
        let fx = try CLIFixture()
        let b = try await bundled(fx)
        let brewFile = try fx.binary(fx.brewBin.appendingPathComponent("offload"), version: "0.1.0")
        for kind in [CLIInstall.Management.homebrew, .packageManager("Nix"), .symlink(target: "/somewhere/offload")] {
            let a = CLIAdvisor.assess(try install(fx, brewFile, version: "0.1.0", kind), bundled: b)
            XCTAssertEqual(a.action, .manual, "\(kind)")
            XCTAssertEqual(a.tone, .attention)
            XCTAssertFalse(a.detail.isEmpty)
        }
        let brew = CLIAdvisor.assess(try install(fx, brewFile, version: "0.1.0", .homebrew), bundled: b)
        XCTAssertTrue(brew.detail.contains("brew upgrade"))
        let link = CLIAdvisor.assess(try install(fx, brewFile, version: "0.1.0", .symlink(target: "/x")), bundled: b)
        XCTAssertTrue(link.detail.contains("ln -sfn"), "offers the link-to-App alternative as text only")
    }

    func testUnwritableFolderGivesCopyInstructionsInstead() async throws {
        let fx = try CLIFixture()
        let b = try await bundled(fx)
        let file = try fx.binary(fx.manualBin.appendingPathComponent("offload"), version: "0.1.0")
        let a = CLIAdvisor.assess(try install(fx, file, version: "0.1.0", .manual, writable: false), bundled: b)
        XCTAssertEqual(a.action, .manual)
        XCTAssertTrue(a.detail.contains("sudo install -m 755"))
    }

    func testCurrentNewerAndUnreadableCopies() async throws {
        let fx = try CLIFixture()
        let b = try await bundled(fx)
        let file = try fx.binary(fx.goBin.appendingPathComponent("offload"), version: "x")
        let same = CLIAdvisor.assess(try install(fx, file, version: "0.9.3", .goInstall), bundled: b)
        XCTAssertEqual(same.action, .none); XCTAssertEqual(same.tone, .ok)
        let ahead = CLIAdvisor.assess(try install(fx, file, version: "0.10.0", .goInstall), bundled: b)
        XCTAssertEqual(ahead.action, .none); XCTAssertEqual(ahead.tone, .neutral)
        XCTAssertTrue(ahead.detail.contains("never downgrades"))
        let build = CLIAdvisor.assess(try install(fx, file, version: "0.9.3-dev.1", .goInstall), bundled: b)
        XCTAssertEqual(build.action, .none, "cannot order two builds of one number, so no overwrite is offered")
        let unreadable = CLIAdvisor.assess(try install(fx, file, version: nil, .goInstall), bundled: b)
        XCTAssertEqual(unreadable.action, .manual)
        let noBundle = CLIAdvisor.assess(try install(fx, file, version: "0.1.0", .goInstall), bundled: nil)
        XCTAssertEqual(noBundle.action, .none)
    }

    func testScopeTextStatesWhatAnAppUpdateChanges() {
        XCTAssertTrue(CLIInstallsCopy.scope.contains("only Offload Monitor.app and the CLI inside it"))
        XCTAssertTrue(CLIInstallsCopy.scope.contains("never changed without your confirmation"))
    }
}

final class CLISyncerTests: XCTestCase {
    struct Setup {
        var fx: CLIFixture
        var target: URL
        var plan: CLISyncPlan
    }

    func setup(targetVersion: String = "0.3.1", management: CLIInstall.Management = .goInstall, dir: URL? = nil) async throws -> Setup {
        let fx = try CLIFixture()
        try fx.binary(fx.bundled, version: "0.9.3")
        let target = try fx.binary((dir ?? fx.goBin).appendingPathComponent("offload"), version: targetVersion, mode: 0o755)
        let report = await fx.scanner(path: [dir ?? fx.goBin]).scan()
        let install = try XCTUnwrap(report.standalone.first)
        guard case .sync(let plan) = CLIAdvisor.assess(install, bundled: report.bundled).action else {
            throw XCTSkip("no sync plan for \(install.management)")
        }
        XCTAssertEqual(plan.management, management)
        return Setup(fx: fx, target: target, plan: plan)
    }

    func read(_ url: URL) throws -> String { try String(contentsOf: url, encoding: .utf8) }

    func testSuccessReplacesAtomicallyKeepsBackupAndModeAndLeavesNoTempFiles() async throws {
        let s = try await setup()
        let result = try await s.fx.syncer().apply(s.plan)
        XCTAssertEqual(try read(s.target), "0.9.3\n")
        XCTAssertEqual(try read(URL(fileURLWithPath: result.backupPath)), "0.3.1\n", "the old bytes are kept")
        XCTAssertEqual(result.fromVersion, "0.3.1"); XCTAssertEqual(result.toVersion, "0.9.3")
        let mode = try FileManager.default.attributesOfItem(atPath: s.target.path)[.posixPermissions] as? Int
        XCTAssertEqual(mode, 0o755)
        let leftovers = try FileManager.default.contentsOfDirectory(atPath: s.fx.goBin.path)
        XCTAssertEqual(leftovers, ["offload"], "no staged or restore files remain")
        // A rescan now reports the copy as current: this is what the user sees after reopening the terminal.
        let after = await s.fx.scanner(path: [s.fx.goBin]).scan()
        let a = CLIAdvisor.assess(after.standalone[0], bundled: after.bundled)
        XCTAssertEqual(a.relation, .same)
    }

    func testFileChangedAfterScanIsRefusedAndUntouched() async throws {
        let s = try await setup()
        try Data("0.4.0\n".utf8).write(to: s.target)
        do { _ = try await s.fx.syncer().apply(s.plan); XCTFail("must refuse") } catch let e as CLISyncError {
            guard case .stale = e else { return XCTFail("\(e)") }
        }
        XCTAssertEqual(try read(s.target), "0.4.0\n")
        XCTAssertFalse(FileManager.default.fileExists(atPath: s.fx.root.appendingPathComponent("backups").path))
    }

    func testTargetTurnedIntoSymlinkIsRefused() async throws {
        let s = try await setup()
        let real = try s.fx.binary(s.fx.manualBin.appendingPathComponent("real-offload"), version: "0.3.1")
        try FileManager.default.removeItem(at: s.target)
        try FileManager.default.createSymbolicLink(at: s.target, withDestinationURL: real)
        do { _ = try await s.fx.syncer().apply(s.plan); XCTFail("must refuse") } catch let e as CLISyncError {
            guard case .refused = e else { return XCTFail("\(e)") }
        }
        XCTAssertEqual(try read(real), "0.3.1\n", "the link's target is never written")
        XCTAssertNotNil(try? FileManager.default.destinationOfSymbolicLink(atPath: s.target.path))
    }

    func testTargetNowOwnedByPackageManagerIsRefused() async throws {
        let fx = try CLIFixture()
        try fx.binary(fx.bundled, version: "0.9.3")
        let target = try fx.binary(fx.brewBin.appendingPathComponent("offload"), version: "0.1.0")
        let plan = CLISyncPlan(path: target.path, management: .manual, fromVersion: "0.1.0", toVersion: "0.9.3",
                               targetDigest: try ArchiveValidator.sha256Hex(of: target), bundledPath: fx.bundled.path,
                               bundledDigest: try ArchiveValidator.sha256Hex(of: fx.bundled))
        do { _ = try await fx.syncer().apply(plan); XCTFail("must refuse") } catch let e as CLISyncError {
            guard case .refused = e else { return XCTFail("\(e)") }
        }
        XCTAssertEqual(try read(target), "0.1.0\n")
    }

    func testBundledCopyChangedOrUnsignedIsRefused() async throws {
        let s = try await setup()
        try Data("0.9.4\n".utf8).write(to: s.fx.bundled)
        do { _ = try await s.fx.syncer().apply(s.plan); XCTFail("must refuse") } catch let e as CLISyncError {
            guard case .stale = e else { return XCTFail("\(e)") }
        }
        XCTAssertEqual(try read(s.target), "0.3.1\n")

        let t = try await setup()
        var syncer = t.fx.syncer()
        syncer.verifySignature = { _ in throw UpdateError.badBundle("bad") }
        do { _ = try await syncer.apply(t.plan); XCTFail("must refuse") } catch let e as CLISyncError {
            guard case .refused = e else { return XCTFail("\(e)") }
        }
        XCTAssertEqual(try read(t.target), "0.3.1\n")
    }

    func testFailedVerificationRestoresThePreviousFile() async throws {
        let s = try await setup()
        var syncer = s.fx.syncer()
        // The staged copy verifies, but the installed file does not report the right version afterwards.
        syncer.probe = { url in url.lastPathComponent.hasPrefix(".offload-sync-") ? "0.9.3" : "broken" }
        do { _ = try await syncer.apply(s.plan); XCTFail("must fail") } catch let e as CLISyncError {
            guard case .verification(let m) = e else { return XCTFail("\(e)") }
            XCTAssertTrue(m.contains("put back"), m)
        }
        XCTAssertEqual(try read(s.target), "0.3.1\n")
        XCTAssertEqual(try FileManager.default.contentsOfDirectory(atPath: s.fx.goBin.path), ["offload"])
    }

    func testPreparedCopyThatCannotRunNeverReplacesTheTarget() async throws {
        let s = try await setup()
        var syncer = s.fx.syncer()
        syncer.probe = { _ in nil }
        do { _ = try await syncer.apply(s.plan); XCTFail("must fail") } catch let e as CLISyncError {
            guard case .failed = e else { return XCTFail("\(e)") }
        }
        XCTAssertEqual(try read(s.target), "0.3.1\n")
        XCTAssertEqual(try FileManager.default.contentsOfDirectory(atPath: s.fx.goBin.path), ["offload"])
    }

    func testUnwritableFolderIsRefusedBeforeAnyChange() async throws {
        let s = try await setup()
        try FileManager.default.setAttributes([.posixPermissions: 0o555], ofItemAtPath: s.fx.goBin.path)
        defer { try? FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: s.fx.goBin.path) }
        try XCTSkipIf(getuid() == 0, "root ignores directory permissions")
        do { _ = try await s.fx.syncer().apply(s.plan); XCTFail("must refuse") } catch let e as CLISyncError {
            guard case .refused = e else { return XCTFail("\(e)") }
        }
        XCTAssertEqual(try read(s.target), "0.3.1\n")
    }
}

@MainActor
final class CLIInstallsModelTests: XCTestCase {
    func model(_ fx: CLIFixture, path: [URL]) -> CLIInstallsModel {
        CLIInstallsModel(scanner: fx.scanner(path: path), syncer: fx.syncer())
    }

    func testRefreshThenConfirmedSyncUpdatesStatus() async throws {
        let fx = try CLIFixture()
        try fx.binary(fx.bundled, version: "0.9.3")
        let target = try fx.binary(fx.goBin.appendingPathComponent("offload"), version: "0.3.1")
        let m = model(fx, path: [fx.goBin])
        XCTAssertEqual(m.summary, "Not checked yet.")
        await m.refresh()
        XCTAssertEqual(m.attentionCount, 1)
        XCTAssertTrue(m.summary.contains("1 of 1"))
        guard case .sync(let plan) = m.assessed()[0].assessment.action else { return XCTFail("expected sync") }
        XCTAssertEqual(try String(contentsOf: target, encoding: .utf8), "0.3.1\n", "scanning alone changes nothing")
        await m.sync(plan)
        guard case .done(let r) = m.syncState else { return XCTFail("\(m.syncState)") }
        XCTAssertEqual(r.toVersion, "0.9.3")
        XCTAssertEqual(m.attentionCount, 0)
        XCTAssertNil(m.suggestionFingerprint)
        m.dismissSyncState()
        XCTAssertEqual(m.syncState, .idle)
    }

    func testStalePlanSurfacesAnErrorAndKeepsFile() async throws {
        let fx = try CLIFixture()
        try fx.binary(fx.bundled, version: "0.9.3")
        let target = try fx.binary(fx.goBin.appendingPathComponent("offload"), version: "0.3.1")
        let m = model(fx, path: [fx.goBin])
        await m.refresh()
        guard case .sync(let plan) = m.assessed()[0].assessment.action else { return XCTFail("expected sync") }
        try Data("0.5.0\n".utf8).write(to: target)
        await m.sync(plan)
        guard case .failed(let message) = m.syncState else { return XCTFail("\(m.syncState)") }
        XCTAssertTrue(message.contains("Nothing was changed"), message)
        XCTAssertEqual(try String(contentsOf: target, encoding: .utf8), "0.5.0\n")
    }

    func testSuggestionAppearsDismissesAndReturnsWhenTheSituationChanges() async throws {
        let fx = try CLIFixture()
        try fx.binary(fx.bundled, version: "0.9.3")
        let target = try fx.binary(fx.goBin.appendingPathComponent("offload"), version: "0.3.1")
        let d = UserDefaults(suiteName: "cli-suggest-\(UUID().uuidString)")!
        let m = model(fx, path: [fx.goBin])
        let u = UpdateManager(prefs: Preferences(defaults: d), consumeReport: false, cliInstalls: m)
        XCTAssertNil(u.cliSuggestion, "nothing scanned yet")
        await m.refresh()
        let first = try XCTUnwrap(u.cliSuggestion)
        XCTAssertEqual(first.action, .reviewCLI)
        u.dismissCLISuggestion()
        XCTAssertNil(u.cliSuggestion)
        let again = UpdateManager(prefs: Preferences(defaults: d), consumeReport: false, cliInstalls: m)
        XCTAssertNil(again.cliSuggestion, "the dismissal is remembered")
        try Data("0.4.0\n".utf8).write(to: target)
        await m.refresh()
        XCTAssertNotNil(u.cliSuggestion, "a different state of the copy shows the suggestion again")
    }

    func testNoSuggestionWhenCurrentOrAbsent() async throws {
        let fx = try CLIFixture()
        try fx.binary(fx.bundled, version: "0.9.3")
        let absent = model(fx, path: [fx.goBin])
        await absent.refresh()
        XCTAssertNil(absent.suggestionFingerprint)
        XCTAssertTrue(absent.summary.contains("No separate offload"))
        try fx.binary(fx.goBin.appendingPathComponent("offload"), version: "0.9.3")
        let current = model(fx, path: [fx.goBin])
        await current.refresh()
        XCTAssertNil(current.suggestionFingerprint)
        XCTAssertTrue(current.summary.contains("no action needed"))
    }
}

@MainActor
final class CLIInstallsLayoutTests: XCTestCase {
    /// Settings has a fixed size and never scrolls as a whole, so the new section must fit even with many long entries.
    func testAppTabFitsWithManyLongEntries() throws {
        func inst(_ path: String, _ v: String?, _ m: CLIInstall.Management, writable: Bool = true) -> CLIInstall {
            CLIInstall(path: path, resolvedPath: path, management: m, version: v, digest: String(repeating: "a", count: 64), directoryWritable: writable)
        }
        let bundled = inst("/Applications/Offload Monitor.app/Contents/Resources/offload", "0.9.6", .appBundle)
        let long = "/Users/someone-with-a-long-name/Library/Application Support/tools/with spaces/in/the/path/bin/offload"
        let rows = [inst(long, "dev", .goInstall), inst("/opt/homebrew/bin/offload", "0.1.0", .homebrew),
                    inst("/usr/local/bin/offload", "0.2.0", .symlink(target: long)), inst("/opt/x/offload", nil, .manual, writable: false)]
        let installs = CLIInstallsModel()
        installs.setReportForTesting(CLIReport(bundled: bundled, standalone: rows, bundledLinks: ["/usr/local/bin/offload-app"], searched: []))
        installs.setReportForTesting(installs.report)
        let d = UserDefaults(suiteName: "cli-layout-\(UUID().uuidString)")!
        let updates = UpdateManager(prefs: Preferences(defaults: d), consumeReport: false, cliInstalls: installs)
        let model = MonitorModel(testingSnapshot: nil)
        let motion = MotionController(monitor: model, prefs: Preferences(defaults: d), scheduler: ManualScheduler(), observeSystem: false)
        let host = NSHostingView(rootView: AppSettingsView(updates: updates, motion: motion).frame(width: SettingsLayout.width))
        host.frame = NSRect(x: 0, y: 0, width: SettingsLayout.width, height: SettingsLayout.height)
        host.layoutSubtreeIfNeeded()
        XCTAssertLessThanOrEqual(host.fittingSize.height, SettingsLayout.height - 8, "keep a margin for localized or larger text")
        XCTAssertFalse(installs.assessed().isEmpty)
        // Offscreen only: nothing is shown on screen. Written when the CI render directory is set.
        if let dir = ProcessInfo.processInfo.environment["OFFLOAD_PANEL_RENDER_DIR"], !dir.isEmpty {
            try FileManager.default.createDirectory(atPath: dir, withIntermediateDirectories: true)
            // A bare hosting view has no window, so fix the appearance and background or the text resolves to white on clear.
            let shot = NSHostingView(rootView: AppSettingsView(updates: updates, motion: motion)
                .frame(width: SettingsLayout.width, height: SettingsLayout.height, alignment: .topLeading)
                .background(Color(nsColor: .windowBackgroundColor)))
            shot.appearance = NSAppearance(named: .aqua)
            shot.frame = NSRect(x: 0, y: 0, width: SettingsLayout.width, height: SettingsLayout.height)
            shot.layoutSubtreeIfNeeded()
            let rep = try XCTUnwrap(shot.bitmapImageRepForCachingDisplay(in: shot.bounds))
            shot.cacheDisplay(in: shot.bounds, to: rep)
            let png = try XCTUnwrap(rep.representation(using: .png, properties: [:]))
            XCTAssertGreaterThan(png.count, 1000)
            try png.write(to: URL(fileURLWithPath: dir).appendingPathComponent("settings-app-cli-installs.png"))
        }
    }
}
