import XCTest
@testable import OffloadMonitor

final class UpdatePolicyTests: XCTestCase {
    static let hex = String(repeating: "ab", count: 32)

    static func release(tag: String = "v0.2.0", draft: Bool = false, pre: Bool = false, assets: [(String, String?)]? = nil) -> GitHubRelease {
        let list = assets ?? [("offload-monitor-macos-arm64.zip", "sha256:\(hex)"), ("offload-monitor-macos-amd64.zip", "sha256:\(hex)")]
        return GitHubRelease(tagName: tag, draft: draft, prerelease: pre, assets: list.map {
            ReleaseAsset(name: $0.0, browserDownloadUrl: "https://github.com\(ReleasePolicy.downloadPath(tag: tag, asset: $0.0))", digest: $0.1)
        })
    }
    let cur = SemanticVersion("0.1.0")!

    func testVersionParsingAndOrdering() {
        XCTAssertLessThan(SemanticVersion("0.9.0")!, SemanticVersion("0.10.0")!)
        XCTAssertLessThan(SemanticVersion("v1.2")!, SemanticVersion("1.2.1")!)
        XCTAssertEqual(SemanticVersion("1.2")!, SemanticVersion("v1.2.0")!)
        XCTAssertLessThan(SemanticVersion("1.0.0-rc1")!, SemanticVersion("1.0.0")!)
        XCTAssertTrue(SemanticVersion("1.0.0-rc1")!.isPrerelease)
        XCTAssertFalse(SemanticVersion("1.0.0+build5")!.isPrerelease)
        for bad in ["", "v", "latest", "1..2", "1.a", "1.2.3.4.5", "-1.0", "99999999999.0"] {
            XCTAssertNil(SemanticVersion(bad), bad)
        }
    }

    func testNewerReleaseSelectsAssetByArch() throws {
        let a = try ReleasePolicy.candidate(from: Self.release(), current: cur, arch: "arm64")
        XCTAssertEqual(a?.assetName, "offload-monitor-macos-arm64.zip")
        XCTAssertEqual(a?.sha256, Self.hex)
        let b = try ReleasePolicy.candidate(from: Self.release(), current: cur, arch: "amd64")
        XCTAssertEqual(b?.assetName, "offload-monitor-macos-amd64.zip")
        XCTAssertEqual(Arch.assetName(for: Arch.current), "offload-monitor-macos-\(Arch.current).zip")
    }

    func testNoDowngradeOrSameVersion() throws {
        XCTAssertNil(try ReleasePolicy.candidate(from: Self.release(tag: "v0.1.0"), current: cur, arch: "arm64"))
        XCTAssertNil(try ReleasePolicy.candidate(from: Self.release(tag: "v0.0.9"), current: cur, arch: "arm64"))
        XCTAssertNil(try ReleasePolicy.candidate(from: Self.release(tag: "v0.1"), current: cur, arch: "arm64"))
    }

    func testRejectsDraftPrereleaseAndBadTags() {
        XCTAssertThrowsError(try ReleasePolicy.candidate(from: Self.release(draft: true), current: cur, arch: "arm64"))
        XCTAssertThrowsError(try ReleasePolicy.candidate(from: Self.release(pre: true), current: cur, arch: "arm64"))
        XCTAssertThrowsError(try ReleasePolicy.candidate(from: Self.release(tag: "v0.2.0-rc1"), current: cur, arch: "arm64"))
        XCTAssertThrowsError(try ReleasePolicy.candidate(from: Self.release(tag: "nightly"), current: cur, arch: "arm64")) {
            XCTAssertEqual($0 as? UpdateError, .badVersion("nightly"))
        }
    }

    func testMissingAssetAndDigest() {
        let noAsset = Self.release(assets: [("offload-monitor-macos-amd64.zip", "sha256:\(Self.hex)")])
        XCTAssertThrowsError(try ReleasePolicy.candidate(from: noAsset, current: cur, arch: "arm64")) {
            XCTAssertEqual($0 as? UpdateError, .noAsset("offload-monitor-macos-arm64.zip"))
        }
        for digest in [nil, "", "  "] as [String?] {
            let r = Self.release(assets: [("offload-monitor-macos-arm64.zip", digest)])
            XCTAssertThrowsError(try ReleasePolicy.candidate(from: r, current: cur, arch: "arm64")) {
                XCTAssertEqual($0 as? UpdateError, .missingDigest("offload-monitor-macos-arm64.zip"))
                XCTAssertTrue($0.localizedDescription.contains("no SHA-256 digest"))
            }
        }
        for digest in ["sha1:\(Self.hex)", "sha256:abc", "sha256:" + String(repeating: "z", count: 64), Self.hex] {
            let r = Self.release(assets: [("offload-monitor-macos-arm64.zip", digest)])
            XCTAssertThrowsError(try ReleasePolicy.candidate(from: r, current: cur, arch: "arm64")) {
                XCTAssertEqual($0 as? UpdateError, .badDigest("offload-monitor-macos-arm64.zip"), digest)
            }
        }
    }

    func testUppercaseDigestIsNormalized() throws {
        XCTAssertEqual(try ReleasePolicy.parseDigest("SHA256:" + Self.hex.uppercased(), asset: "a"), Self.hex)
    }

    func testAssetURLValidation() {
        let tag = "v0.2.0", name = "offload-monitor-macos-arm64.zip"
        let good = "https://github.com/zhoushoujianwork/offload/releases/download/v0.2.0/\(name)"
        XCTAssertNoThrow(try ReleasePolicy.validateAssetURL(good, tag: tag, asset: name))
        for bad in [
            "http://github.com/zhoushoujianwork/offload/releases/download/v0.2.0/\(name)",
            "https://evil.example/zhoushoujianwork/offload/releases/download/v0.2.0/\(name)",
            "https://github.com.evil.example/zhoushoujianwork/offload/releases/download/v0.2.0/\(name)",
            "https://github.com/other/offload/releases/download/v0.2.0/\(name)",
            "https://github.com/zhoushoujianwork/offload/releases/download/v0.3.0/\(name)",
            "https://github.com/zhoushoujianwork/offload/releases/download/v0.2.0/../../x/\(name)",
            good + "?x=1",
            "https://user@github.com/zhoushoujianwork/offload/releases/download/v0.2.0/\(name)",
            "https://github.com:8443/zhoushoujianwork/offload/releases/download/v0.2.0/\(name)",
            "file:///tmp/\(name)",
        ] {
            XCTAssertThrowsError(try ReleasePolicy.validateAssetURL(bad, tag: tag, asset: name), bad)
        }
    }

    func testRedirectValidation() {
        XCTAssertNoThrow(try ReleasePolicy.validateRedirect(URL(string: "https://objects.githubusercontent.com/x?y=1")!))
        XCTAssertNoThrow(try ReleasePolicy.validateRedirect(URL(string: "https://release-assets.githubusercontent.com/x")!))
        XCTAssertNoThrow(try ReleasePolicy.validateRedirect(URL(string: "https://github.com/x")!))
        for bad in ["http://objects.githubusercontent.com/x", "https://githubusercontent.com.evil.net/x",
                    "https://evilgithubusercontent.com/x", "https://example.com/x", "ftp://github.com/x"] {
            XCTAssertThrowsError(try ReleasePolicy.validateRedirect(URL(string: bad)!), bad)
        }
    }

    func testReleaseJSONDecode() throws {
        let json = """
        {"tag_name":"v0.3.0","draft":false,"prerelease":false,"name":"x","assets":[{"name":"offload-monitor-macos-arm64.zip","browser_download_url":"https://github.com/zhoushoujianwork/offload/releases/download/v0.3.0/offload-monitor-macos-arm64.zip","digest":"sha256:\(Self.hex)","size":5},{"name":"no-digest.zip","browser_download_url":"https://x","digest":null}]}
        """
        let r = try GitHubRelease.decode(Data(json.utf8))
        XCTAssertEqual(r.tagName, "v0.3.0")
        XCTAssertNil(r.assets[1].digest)
        XCTAssertThrowsError(try GitHubRelease.decode(Data("{}".utf8)))
    }
}
