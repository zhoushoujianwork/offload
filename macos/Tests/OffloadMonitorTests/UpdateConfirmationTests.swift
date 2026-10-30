import Foundation
import XCTest
@testable import OffloadMonitor

private final class ReleaseDiscoveryFixture: URLProtocol {
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        let name = Arch.assetName(for: Arch.current)
        let tag = "v999.0.0"
        let json = """
        {"tag_name":"\(tag)","draft":false,"prerelease":false,"assets":[{"name":"\(name)","browser_download_url":"https://github.com\(ReleasePolicy.downloadPath(tag: tag, asset: name))","digest":"sha256:\(String(repeating: "ab", count: 32))"}]}
        """
        client?.urlProtocol(self, didReceive: HTTPURLResponse(url: request.url!, statusCode: 200, httpVersion: nil, headerFields: nil)!, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data(json.utf8))
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

@MainActor
final class UpdateConfirmationTests: XCTestCase {
    func testDiscoveryNeverDownloadsDespiteLegacyAutomaticPreference() async throws {
        let defaults = UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!
        let prefs = Preferences(defaults: defaults)
        prefs.autoCheck = false
        prefs.autoInstall = true
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [ReleaseDiscoveryFixture.self]
        let session = URLSession(configuration: config)
        defer { session.invalidateAndCancel() }
        let manager = UpdateManager(prefs: prefs, service: UpdateService(session: session), consumeReport: false)
        XCTAssertFalse(manager.autoInstall)
        manager.autoInstall = true // Even an old binding cannot bypass confirmation.
        manager.checkNow(manual: false)
        for _ in 0..<200 {
            if case .checking = manager.phase { try await Task.sleep(nanoseconds: 10_000_000) } else { break }
        }
        guard case .available = manager.phase else { return XCTFail("Discovery must stop at review: \(manager.phase)") }
        XCTAssertTrue(manager.hasReleaseUpdate)
        XCTAssertEqual(manager.releaseCandidate?.version, SemanticVersion("999.0.0"))
        XCTAssertFalse(manager.pendingReleaseRestart)
    }

    func testReadyUpdateRemainsPendingUntilConfirmedAndIdle() {
        let prefs = Preferences(defaults: UserDefaults(suiteName: "om-test-\(UUID().uuidString)")!)
        prefs.autoCheck = false
        let manager = UpdateManager(prefs: prefs, consumeReport: false)
        let candidate = UpdateCandidate(version: SemanticVersion("999.0.0")!, tag: "v999.0.0", assetName: "app.zip",
                                        downloadURL: URL(string: "https://github.com/app.zip")!, sha256: "")
        manager.setPhaseForTesting(.ready(candidate), staged: UpdateService.Staged(directory: URL(fileURLWithPath: "/tmp/fixture"), bundle: URL(fileURLWithPath: "/tmp/fixture/App.app")))
        var blocker: String? = "Unsaved draft", requests = 0
        manager.configureLifecycle(blocker: { blocker }, refreshCLI: {}, requestTermination: { requests += 1 })
        manager.considerRestart()
        XCTAssertTrue(manager.hasReleaseUpdate)
        XCTAssertEqual(requests, 0)
        XCTAssertFalse(manager.pendingReleaseRestart, "opening or canceling a review must not queue installation")
        manager.confirmReleaseUpdate()
        XCTAssertTrue(manager.pendingReleaseRestart)
        XCTAssertEqual(requests, 0)
        blocker = nil
        manager.considerRestart()
        XCTAssertEqual(requests, 1)
    }
}
