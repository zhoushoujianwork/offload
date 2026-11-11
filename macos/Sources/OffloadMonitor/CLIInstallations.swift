import Foundation

/// One copy of the `offload` command found on this Mac.
struct CLIInstall: Equatable, Identifiable {
    enum Management: Equatable {
        /// The App's own bundled copy, or a link that resolves to it.
        case appBundle
        case goInstall
        case homebrew
        case packageManager(String)
        case symlink(target: String)
        /// A plain file no package manager or Go tool claims.
        case manual

        var label: String {
            switch self {
            case .appBundle: return "bundled with the App"
            case .goInstall: return "Go install"
            case .homebrew: return "Homebrew"
            case .packageManager(let name): return name
            case .symlink: return "symbolic link"
            case .manual: return "manual copy"
            }
        }
    }

    var path: String
    var resolvedPath: String
    var management: Management
    /// First line of `offload version`; nil when the file did not run or printed something unexpected.
    var version: String?
    /// SHA-256 of the file bytes; nil for links and unreadable files.
    var digest: String?
    var directoryWritable: Bool
    var id: String { path }
}

struct CLIReport: Equatable {
    var bundled: CLIInstall?
    /// Separate copies in PATH order, without the bundled copy and without links to it.
    var standalone: [CLIInstall]
    /// PATH entries that are links to the bundled copy. They update together with the App.
    var bundledLinks: [String]
    var searched: [String]
}

// MARK: - Version comparison

enum CLIVersionRelation: Equatable {
    case same, behind, ahead
    /// Same version number, different build string (for example two development builds).
    case differentBuild
    /// The standalone copy printed something that is not a version number, such as `dev`.
    case unknown
    /// The standalone copy did not run or printed no version.
    case unreadable
    case bundledUnknown

    static func compare(standalone: String?, bundled: String?) -> CLIVersionRelation {
        guard let bundled, let b = SemanticVersion(bundled) else { return .bundledUnknown }
        guard let standalone else { return .unreadable }
        guard let s = SemanticVersion(standalone) else { return .unknown }
        // Only the numeric core is ordered. Dev builds are named <last tag>-dev.<time>.<commit>, so they come after
        // that tag even though semver would sort the suffix before it; two builds of one number cannot be ranked.
        let c = compareCore(s, b)
        if c < 0 { return .behind }
        if c > 0 { return .ahead }
        return Self.normalized(standalone) == Self.normalized(bundled) ? .same : .differentBuild
    }

    private static func compareCore(_ a: SemanticVersion, _ b: SemanticVersion) -> Int {
        for i in 0..<max(a.components.count, b.components.count) {
            let x = i < a.components.count ? a.components[i] : 0
            let y = i < b.components.count ? b.components[i] : 0
            if x != y { return x < y ? -1 : 1 }
        }
        return 0
    }

    private static func normalized(_ v: String) -> String {
        let t = v.trimmingCharacters(in: .whitespacesAndNewlines)
        return t.hasPrefix("v") || t.hasPrefix("V") ? String(t.dropFirst()) : t
    }
}

// MARK: - Classification

enum CLIClassifier {
    static func realPath(_ path: String) -> String {
        var buffer = [CChar](repeating: 0, count: Int(PATH_MAX))
        guard realpath(path, &buffer) != nil else { return path }
        return String(cString: buffer)
    }

    static func isSymbolicLink(_ path: String) -> Bool {
        (try? FileManager.default.destinationOfSymbolicLink(atPath: path)) != nil
    }

    /// Directories where `go install` puts binaries: `$GOBIN`, `$GOPATH/bin` and the default `~/go/bin`.
    static func goBinDirectories(home: String, environment: [String: String]) -> [String] {
        var dirs: [String] = []
        if let bin = environment["GOBIN"], !bin.isEmpty { dirs.append(bin) }
        for p in (environment["GOPATH"] ?? "").split(separator: ":") where !p.isEmpty { dirs.append(String(p) + "/bin") }
        dirs.append(home + "/go/bin")
        return dirs.map(realPath)
    }

    static func packageManager(resolved: String) -> CLIInstall.Management? {
        if resolved.contains("/Cellar/") || resolved.contains("/Homebrew/") || resolved.hasPrefix("/opt/homebrew/") { return .homebrew }
        if resolved.hasPrefix("/opt/local/") { return .packageManager("MacPorts") }
        if resolved.hasPrefix("/nix/store/") { return .packageManager("Nix") }
        return nil
    }

    static func management(path: String, resolved: String, isSymlink: Bool, bundledResolved: String?,
                           home: String, environment: [String: String]) -> CLIInstall.Management {
        if let bundledResolved, resolved == bundledResolved { return .appBundle }
        if let pm = packageManager(resolved: resolved) { return pm }
        if isSymlink { return .symlink(target: resolved) }
        let dir = realPath((path as NSString).deletingLastPathComponent)
        if goBinDirectories(home: home, environment: environment).contains(dir) { return .goInstall }
        return .manual
    }
}

// MARK: - Scanner

/// Finds copies of `offload` and asks each for its version. Read-only: it never writes anywhere.
struct CLIScanner {
    static let maxCandidates = 8

    var environment: [String: String] = ChatCommand.environment()
    var home: String = NSHomeDirectory()
    var bundled: URL? = Bundle.main.resourceURL?.appendingPathComponent("offload")
    var probe: @Sendable (URL) async -> String? = CLIScanner.runVersion

    func scan() async -> CLIReport {
        let fm = FileManager.default
        var bundledInstall: CLIInstall?
        if let bundled, fm.isExecutableFile(atPath: bundled.path) {
            bundledInstall = await install(at: bundled.path, bundledResolved: nil, forceManagement: .appBundle)
        }
        let bundledResolved = bundledInstall?.resolvedPath
        let dirs = (environment["PATH"] ?? "").split(separator: ":").map(String.init).filter { !$0.isEmpty }
        var seen = Set<String>()
        var standalone: [CLIInstall] = []
        var links: [String] = []
        for dir in dirs {
            let candidate = (dir as NSString).appendingPathComponent("offload")
            var isDir: ObjCBool = false
            guard fm.isExecutableFile(atPath: candidate), fm.fileExists(atPath: candidate, isDirectory: &isDir), !isDir.boolValue else { continue }
            let resolved = CLIClassifier.realPath(candidate)
            guard seen.insert(resolved).inserted else { continue }
            if let bundledResolved, resolved == bundledResolved {
                if candidate != bundled?.path { links.append(candidate) }
                continue
            }
            guard standalone.count < Self.maxCandidates else { break }
            standalone.append(await install(at: candidate, bundledResolved: bundledResolved, forceManagement: nil))
        }
        return CLIReport(bundled: bundledInstall, standalone: standalone, bundledLinks: links, searched: dirs)
    }

    private func install(at path: String, bundledResolved: String?, forceManagement: CLIInstall.Management?) async -> CLIInstall {
        let resolved = CLIClassifier.realPath(path)
        let symlink = CLIClassifier.isSymbolicLink(path)
        let management = forceManagement ?? CLIClassifier.management(
            path: path, resolved: resolved, isSymlink: symlink, bundledResolved: bundledResolved, home: home, environment: environment)
        let dir = (path as NSString).deletingLastPathComponent
        return CLIInstall(path: path, resolvedPath: resolved, management: management,
                          version: await probe(URL(fileURLWithPath: path)),
                          digest: symlink ? nil : try? ArchiveValidator.sha256Hex(of: URL(fileURLWithPath: path)),
                          directoryWritable: FileManager.default.isWritableFile(atPath: dir))
    }

    /// `offload version` prints one token. Anything else (or a failure) counts as "no version".
    @Sendable static func runVersion(_ url: URL) async -> String? {
        guard let out = try? await ProcessRunner.run(executable: url, arguments: ["version"], timeout: 5, maxOutput: 4096) else { return nil }
        guard out.status == 0, !out.timedOut, !out.truncated, !out.signalled else { return nil }
        return sanitizeVersion(String(decoding: out.stdout, as: UTF8.self))
    }

    static func sanitizeVersion(_ text: String) -> String? {
        guard let line = text.split(whereSeparator: \.isNewline).first.map({ $0.trimmingCharacters(in: .whitespaces) }),
              !line.isEmpty, line.count <= 64,
              line.unicodeScalars.allSatisfy({ $0.isASCII && (CharacterSet.alphanumerics.contains($0) || "._+-".unicodeScalars.contains($0)) })
        else { return nil }
        return line
    }
}

// MARK: - Assessment

/// What the App would replace, exactly as shown to the user. The update only runs if a fresh scan still matches.
struct CLISyncPlan: Equatable {
    var path: String
    var management: CLIInstall.Management
    var fromVersion: String?
    var toVersion: String
    var targetDigest: String
    var bundledPath: String
    var bundledDigest: String
}

enum CLIAction: Equatable {
    case none
    /// The user can choose to copy the bundled CLI over this file.
    case sync(CLISyncPlan)
    /// Owned by something else, or not writable: instructions only.
    case manual
}

struct CLIAssessment: Equatable {
    enum Tone: Equatable { case ok, attention, neutral }
    var relation: CLIVersionRelation
    var headline: String
    var detail: String
    var tone: Tone
    var action: CLIAction
}

enum CLIAdvisor {
    static let goInstallCommand = "go install github.com/zhoushoujianwork/offload@latest"
    static let verifyCommand = "which -a offload && offload version"

    /// Single-quotes a path for a shell command, so spaces and quotes survive copy and paste.
    static func shellQuote(_ s: String) -> String { "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'" }

    static func assess(_ install: CLIInstall, bundled: CLIInstall?) -> CLIAssessment {
        let relation = CLIVersionRelation.compare(standalone: install.version, bundled: bundled?.version)
        let b = bundled?.version ?? "unknown", s = install.version ?? "unknown"
        func result(_ headline: String, _ detail: String, _ tone: CLIAssessment.Tone, _ action: CLIAction = .none) -> CLIAssessment {
            CLIAssessment(relation: relation, headline: headline, detail: detail, tone: tone, action: action)
        }
        if case .appBundle = install.management {
            return result("Updates with the App", "This entry is the App's own bundled CLI.", .ok)
        }
        switch relation {
        case .same:
            return result("Up to date", "Same version as the App's bundled CLI.", .ok)
        case .ahead:
            return result("Newer than the App's CLI (\(s) > \(b))",
                          "Nothing is copied over a newer CLI. The App never downgrades a standalone copy.", .neutral)
        case .differentBuild:
            return result("Same version number as the App's CLI, different build",
                          "The App cannot tell which build is newer, so it does not offer to replace this copy.", .neutral)
        case .bundledUnknown:
            return result("Not compared", "The App's bundled CLI did not report a version, so nothing can be compared or copied.", .neutral)
        case .unreadable:
            return result("Did not report a version",
                          "The App does not touch a file it cannot identify. Check it in Terminal: \(shellQuote(install.path)) version", .attention, .manual)
        case .behind:
            return needsUpdate(install, bundled: bundled, headline: "Older than the App's CLI (\(s) < \(b))", relation: relation)
        case .unknown:
            return needsUpdate(install, bundled: bundled,
                               headline: "Version cannot be compared (reports \u{201C}\(s)\u{201D}; the App's is \(b))", relation: relation)
        }
    }

    private static func needsUpdate(_ install: CLIInstall, bundled: CLIInstall?, headline: String, relation: CLIVersionRelation) -> CLIAssessment {
        func manual(_ detail: String) -> CLIAssessment {
            CLIAssessment(relation: relation, headline: headline, detail: detail, tone: .attention, action: .manual)
        }
        let path = shellQuote(install.path)
        switch install.management {
        case .appBundle:
            return CLIAssessment(relation: relation, headline: "Updates with the App", detail: "", tone: .ok, action: .none)
        case .homebrew:
            return manual("Installed by Homebrew. Update it with brew (for example \u{201C}brew upgrade\u{201D}) so Homebrew's records stay correct. The App does not overwrite it.")
        case .packageManager(let name):
            return manual("Installed by \(name). Update it with that tool so its records stay correct. The App does not overwrite it.")
        case .symlink(let target):
            let link = bundled.map { " Or point the link at the App's bundled CLI, which follows App updates: ln -sfn \(shellQuote($0.path)) \(path)" } ?? ""
            return manual("A symbolic link to \(target). The App does not overwrite a link or the file it points to. Update it with whatever created it.\(link)")
        case .goInstall, .manual:
            guard let bundled, let toVersion = bundled.version, let targetDigest = install.digest, let bundledDigest = bundled.digest else {
                return manual("The file could not be read completely, so the App cannot copy over it safely.")
            }
            guard install.directoryWritable else {
                return manual("The folder is not writable by the App. Copy it yourself: sudo install -m 755 \(shellQuote(bundled.path)) \(path)")
            }
            let plan = CLISyncPlan(path: install.path, management: install.management, fromVersion: install.version, toVersion: toVersion,
                                   targetDigest: targetDigest, bundledPath: bundled.path, bundledDigest: bundledDigest)
            var detail = "You can copy the App's bundled CLI over this file. The App asks first and saves the old file."
            if case .goInstall = install.management { detail += " Or run: \(goInstallCommand)" }
            return CLIAssessment(relation: relation, headline: headline, detail: detail, tone: .attention, action: .sync(plan))
        }
    }
}
