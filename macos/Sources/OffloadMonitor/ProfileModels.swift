import Foundation

// MARK: - `offload profiles --json`

struct ProfileEntry: Codable, Equatable, Identifiable {
    var codexAccount: String? = nil
    var name: String
    var backend: String
    var baseUrl: String
    var baseUrlRedacted: Bool
    var model: String
    var endpoint: String
    var reasoningEffort: String
    var serviceTier: String
    var authMode: String
    var id: String { name }
    var isCodex: Bool { backend == "codex" }
}

struct ProfilesSnapshot: Codable, Equatable {
    var managedAccounts: [String]? = nil
    var schemaVersion: Int
    var configExists: Bool
    var configuredDefault: String
    var active: String
    var activeSource: String
    var delegationMode: String
    var profiles: [ProfileEntry]
    var warnings: [String]

    static func decode(_ data: Data) throws -> ProfilesSnapshot {
        let d = JSONDecoder()
        d.keyDecodingStrategy = .convertFromSnakeCase
        do {
            struct Probe: Decodable { var schemaVersion: Int }
            guard try d.decode(Probe.self, from: data).schemaVersion == 1 else { throw CLIError.badOutput("unsupported schema") }
            return try d.decode(ProfilesSnapshot.self, from: data)
        } catch let e as CLIError { throw e } catch { throw CLIError.badOutput("profiles") }
    }

    func entry(_ name: String) -> ProfileEntry? { profiles.first { $0.name == name } }

    /// What new runs use and why. Honest about an environment override that a saved default cannot change.
    var activeDescription: String {
        switch activeSource {
        case "environment":
            var t = "New runs use \"\(active)\" because OFFLOAD_PROFILE is set."
            if !configuredDefault.isEmpty && configuredDefault != active { t += " The saved default is \"\(configuredDefault)\" and applies where OFFLOAD_PROFILE is not set." }
            return t
        case "config": return "New runs use the saved default \"\(active)\"."
        default: return configExists ? "No profile is selected for new runs." : "No offload config found yet."
        }
    }
    var overriddenByEnvironment: Bool { activeSource == "environment" }
}

// MARK: - `offload profile save --json` request

/// Pointer-style patch: nil fields are left out of the JSON so the backend keeps the stored value.
struct ProfileSaveRequest: Encodable, Equatable, CustomStringConvertible, CustomDebugStringConvertible {
    var name: String
    var create: Bool
    var codexAccount: String?
    var backend: String?
    var baseUrl: String?
    var model: String?
    var endpoint: String?
    var reasoningEffort: String?
    var serviceTier: String?
    var authMode: String?
    var secret: String?

    enum CodingKeys: String, CodingKey {
        case name, create, backend, model, endpoint, secret
        case codexAccount = "codex_account"
        case baseUrl = "base_url", reasoningEffort = "reasoning_effort", serviceTier = "service_tier", authMode = "auth_mode"
    }

    var isEmptyPatch: Bool {
        codexAccount == nil && backend == nil && baseUrl == nil && model == nil && endpoint == nil && reasoningEffort == nil
            && serviceTier == nil && authMode == nil && secret == nil
    }

    // Never print the secret, even from a debugger or a stray log line.
    var description: String { "ProfileSaveRequest(name: \(name), create: \(create), secret: \(secret == nil ? "none" : "<redacted>"))" }
    var debugDescription: String { description }
}

// MARK: - Editing

enum ProfileDraftError: LocalizedError, Equatable {
    case nameRequired, nameInvalid, nameTaken
    case modelRequired, urlRequired, urlInvalid
    case badEffort, badTier, badEndpoint
    case secretRequired(String)

    var errorDescription: String? {
        switch self {
        case .nameRequired: return "Enter a profile name."
        case .nameInvalid: return "Use letters, numbers, dots, dashes or underscores in the name."
        case .nameTaken: return "A profile with that name already exists."
        case .modelRequired: return "A Claude profile needs a model."
        case .urlRequired: return "A Claude profile needs a gateway URL."
        case .urlInvalid: return "Enter a full http or https gateway URL."
        case .badEffort: return "Choose a listed reasoning effort."
        case .badTier: return "Choose a listed service tier."
        case .badEndpoint: return "Choose none, cc or llm for the endpoint."
        case .secretRequired(let what): return "Enter the new \(what), or choose Keep saved."
        }
    }
}

struct ProfileDraft: Equatable {
    enum Credential: String, CaseIterable, Identifiable {
        case keep, none, token, tokenCmd
        var id: String { rawValue }
        var label: String {
            switch self {
            case .keep: return "Keep saved"
            case .none: return "None"
            case .token: return "API token"
            case .tokenCmd: return "Token command"
            }
        }
        var authMode: String {
            switch self {
            case .keep: return "preserve"
            case .none: return "none"
            case .token: return "token"
            case .tokenCmd: return "token_cmd"
            }
        }
    }

    static let efforts = ["", "low", "medium", "high", "xhigh", "max", "ultra"]
    static let tiers = ["", "default", "priority"]
    static let endpoints = ["", "cc", "llm"]

    var original: ProfileEntry?
    var name = ""
    var backend = "claude"
    var model = ""
    var baseURL = ""
    var replaceBaseURL = false
    var endpoint = ""
    var reasoning = ""
    var tier = ""
    var codexAccount = ""
    /// A new Claude profile needs a credential, so it starts on API token.
    var credential: Credential = .token
    var secret = ""

    var isNew: Bool { original == nil }

    /// A new profile starts with Fast off (the standard service tier), never inheriting Fast from the native CLI.
    static func new() -> ProfileDraft { var d = ProfileDraft(); d.tier = "default"; return d }

    /// The explicit Fast choices. `notSet` is only offered while editing a profile that has no saved tier, so
    /// the form can show that state honestly instead of guessing On or Off.
    enum Fast: String, CaseIterable, Identifiable {
        case off, on, notSet
        var id: String { rawValue }
        var title: String {
            switch self {
            case .off: return "Off"
            case .on: return "On"
            case .notSet: return "Not set"
            }
        }
        var tier: String {
            switch self {
            case .off: return "default"
            case .on: return "priority"
            case .notSet: return ""
            }
        }
    }

    var fast: Fast {
        get { tier == "priority" ? .on : tier == "default" ? .off : .notSet }
        set { tier = newValue.tier }
    }

    /// Off and On always; Not set only while the saved profile has no tier and the user has not chosen one yet.
    var fastChoices: [Fast] {
        let inherited = (original?.serviceTier ?? "").isEmpty && !isNew
        return inherited ? Fast.allCases : [.off, .on]
    }

    init() {}

    init(editing e: ProfileEntry) {
        original = e
        name = e.name
        backend = e.backend.isEmpty ? "claude" : e.backend
        model = e.model
        baseURL = e.baseUrlRedacted ? "" : e.baseUrl
        endpoint = e.endpoint
        reasoning = e.reasoningEffort
        tier = e.serviceTier
        codexAccount = e.codexAccount ?? ""
        credential = e.authMode == "none" ? .token : .keep
    }

    var baseURLHidden: Bool { original?.baseUrlRedacted == true }
    var isCodex: Bool { backend == "codex" }
    /// The credential choices the backend accepts. A Claude profile needs a token or a token command. Keeping the
    /// saved credential is offered only when the profile has one (a profile switched from Codex has none).
    var credentialChoices: [Credential] {
        guard let o = original, o.authMode != "none" else { return [.token, .tokenCmd] }
        return [.keep, .token, .tokenCmd]
    }

    static func validName(_ n: String) -> Bool {
        guard !n.isEmpty, n.count <= 64, let f = n.unicodeScalars.first, f.properties.isAlphabetic || ("0"..."9").contains(Character(f)) else { return false }
        return n.unicodeScalars.allSatisfy { $0.isASCII && ($0.properties.isAlphabetic || ("0"..."9").contains(Character($0)) || "._-".unicodeScalars.contains($0)) }
    }

    static func validURL(_ s: String) -> Bool {
        guard let u = URL(string: s), let scheme = u.scheme?.lowercased(), scheme == "http" || scheme == "https",
              let host = u.host, !host.isEmpty else { return false }
        return true
    }

    /// Builds the patch to send. Only fields the user changed are included; a gateway URL that the CLI withheld
    /// (redacted) is never sent unless the user explicitly chose to replace it.
    func request(existingNames: [String]) throws -> ProfileSaveRequest {
        let trimmedName = name.trimmingCharacters(in: .whitespaces)
        var r = ProfileSaveRequest(name: original?.name ?? trimmedName, create: isNew)
        let o = original
        let model = self.model.trimmingCharacters(in: .whitespaces)
        let url = baseURL.trimmingCharacters(in: .whitespaces)

        if isNew {
            guard !trimmedName.isEmpty else { throw ProfileDraftError.nameRequired }
            guard Self.validName(trimmedName) else { throw ProfileDraftError.nameInvalid }
            guard !existingNames.contains(trimmedName) else { throw ProfileDraftError.nameTaken }
        }
        guard Self.efforts.contains(reasoning) else { throw ProfileDraftError.badEffort }
        guard Self.tiers.contains(tier) else { throw ProfileDraftError.badTier }
        guard Self.endpoints.contains(endpoint) else { throw ProfileDraftError.badEndpoint }

        if isNew || backend != (o?.backend.isEmpty == false ? o!.backend : "claude") { r.backend = backend }

        if isCodex {
            if codexAccount != (o?.codexAccount ?? "") { r.codexAccount = codexAccount }
            if model != (o?.model ?? "") || (isNew && !model.isEmpty) { r.model = model }
            if reasoning != (o?.reasoningEffort ?? "") { r.reasoningEffort = reasoning }
            if tier != (o?.serviceTier ?? "") { r.serviceTier = tier }
            if let o, !o.endpoint.isEmpty { r.endpoint = "" } // Codex has no gateway endpoint
            return r // native Codex sign-in: no credential fields
        }

        // Claude
        guard !model.isEmpty else { throw ProfileDraftError.modelRequired }
        if isNew || model != (o?.model ?? "") { r.model = model }

        if baseURLHidden && !replaceBaseURL {
            // Withheld by the CLI: keep the stored value, send nothing.
        } else {
            guard !url.isEmpty else { throw ProfileDraftError.urlRequired }
            guard Self.validURL(url) else { throw ProfileDraftError.urlInvalid }
            if isNew || replaceBaseURL || url != (o?.baseUrl ?? "") { r.baseUrl = url }
        }
        if isNew ? !endpoint.isEmpty : endpoint != (o?.endpoint ?? "") { r.endpoint = endpoint }

        switch credential {
        case .keep:
            guard !isNew, o?.authMode != "none" else { throw ProfileDraftError.secretRequired("API token or token command") }
            r.authMode = "preserve"
        case .none:
            throw ProfileDraftError.secretRequired("API token or token command") // Claude cannot run without a credential
        case .token, .tokenCmd:
            let s = secret.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !s.isEmpty else { throw ProfileDraftError.secretRequired(credential == .token ? "API token" : "token command") }
            r.authMode = credential.authMode
            r.secret = s
        }
        return r
    }

    /// True when saving would change something (always true for a new profile).
    func hasChanges(existingNames: [String]) -> Bool {
        guard let r = try? request(existingNames: existingNames) else { return true } // invalid input is still an edit
        return isNew || !r.isEmptyPatch && !(r.authMode == "preserve" && r.secret == nil && onlyPreserve(r))
    }

    private func onlyPreserve(_ r: ProfileSaveRequest) -> Bool {
        r.backend == nil && r.baseUrl == nil && r.model == nil && r.endpoint == nil && r.reasoningEffort == nil && r.serviceTier == nil
    }
}

// MARK: - Reacting to a changed stored profile

/// What the Settings form does when a background refresh brings a stored profile that differs from the one
/// the form was filled from.
enum DraftSync: Equatable {
    /// Nothing to do.
    case none
    /// The form has no edits: show the new stored values.
    case reload(ProfileEntry)
    /// The form has edits: keep them and tell the user the profile changed on disk.
    case keepEdits
    /// The profile is gone and the form has no edits: go back to a blank new profile.
    case removed
}

extension ProfileDraft {
    /// True when the user changed anything (including typing a replacement secret) since the form was filled.
    var isDirty: Bool {
        guard let o = original else { return self != ProfileDraft.new() }
        return self != ProfileDraft(editing: o)
    }

    /// Pure policy: never discards unsaved input because of a background refresh. A new profile is unaffected.
    static func sync(draft: ProfileDraft, stored: ProfileEntry?) -> DraftSync {
        guard let original = draft.original else { return .none }
        guard let stored else { return draft.isDirty ? .keepEdits : .removed }
        if stored == original { return .none }
        return draft.isDirty ? .keepEdits : .reload(stored)
    }
}
