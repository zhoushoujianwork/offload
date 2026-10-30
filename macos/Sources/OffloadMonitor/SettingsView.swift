import SwiftUI

enum SettingsTab: String, CaseIterable, Identifiable {
    case profiles = "Profiles", skills = "Agent skills", app = "App"
    var id: String { rawValue }
}

enum SettingsLayout {
    static let width: CGFloat = 580
    static let height: CGFloat = 560
}

/// The native Settings window. Fixed size; every tab is laid out to fit without scrolling.
struct SettingsView: View {
    @ObservedObject var profiles: ProfilesModel
    @ObservedObject var skills: AgentSkillsModel
    @ObservedObject var updates: UpdateManager
    @ObservedObject var motion: MotionController
    @State var tab: SettingsTab = .profiles

    var body: some View {
        VStack(spacing: 0) {
            Picker("Settings", selection: $tab) {
                ForEach(SettingsTab.allCases) { Text($0.rawValue).tag($0) }
            }
            .pickerStyle(.segmented)
            .labelsHidden()
            .padding(12)
            Divider()
            Group {
                switch tab {
                case .profiles: ProfilesSettingsView(profiles: profiles)
                case .skills: SkillsSettingsView(skills: skills)
                case .app: AppSettingsView(updates: updates, motion: motion)
                }
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        }
        .frame(width: SettingsLayout.width, height: SettingsLayout.height)
        .font(.system(size: 12))
    }
}

// MARK: - Profiles

struct ProfilesSettingsView: View {
    @ObservedObject var profiles: ProfilesModel
    @State private var accountName = ""
    @State private var selection: String = ""
    @State private var draft = ProfileDraft.new()
    @State private var formError: String?
    @State private var notice: String?
    /// A background refresh found the stored profile different while the form has unsaved edits.
    @State private var changedOnDisk = false

    private var snap: ProfilesSnapshot? { profiles.snapshot }
    private var names: [String] { snap?.profiles.map(\.name) ?? [] }

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            if let s = snap {
                Text(s.activeDescription).font(.system(size: 11)).foregroundStyle(.secondary)
                    .lineLimit(3).fixedSize(horizontal: false, vertical: true)
                Text("Choosing a profile affects new runs only. Workers that are already running keep the profile they started with.")
                    .font(.system(size: 10)).foregroundStyle(.secondary).lineLimit(2).fixedSize(horizontal: false, vertical: true)
            }
            picker
            Divider()
            form
            Spacer(minLength: 0)
            messages
            actions
        }
        .padding(14)
        .onAppear { profiles.refreshIfStale(); syncSelection() }
        .onChange(of: profiles.snapshot) { _ in syncSelection() }
        .onChange(of: draft) { _ in profiles.hasUnsavedEdits = draft.isDirty }
    }

    // MARK: pieces

    private var picker: some View {
        HStack(spacing: 8) {
            Picker("Profile", selection: Binding(get: { draft.isNew ? "" : selection }, set: { choose($0) })) {
                if draft.isNew { Text("New profile").tag("") }
                ForEach(snap?.profiles ?? []) { p in
                    Text(p.model.isEmpty ? p.name : "\(p.name) — \(p.model)").lineLimit(1).truncationMode(.middle).tag(p.name)
                }
            }
            .frame(maxWidth: 360)
            .disabled(profiles.busy || snap == nil)
            Button("New") { draft = ProfileDraft.new(); selection = ""; formError = nil; notice = nil }
                .disabled(profiles.busy || snap == nil)
            Spacer()
            if profiles.busy { ProgressView().controlSize(.small) }
        }
    }

    @ViewBuilder private var form: some View {
        if snap == nil {
            Text(profiles.loadError ?? "Loading profiles…").foregroundStyle(.secondary)
                .frame(maxWidth: .infinity, minHeight: 120)
        } else {
            Grid(alignment: .leadingFirstTextBaseline, horizontalSpacing: 10, verticalSpacing: 8) {
                GridRow {
                    label("Name")
                    if draft.isNew {
                        TextField("e.g. work", text: $draft.name).textFieldStyle(.roundedBorder)
                    } else {
                        Text(draft.name).lineLimit(1).truncationMode(.middle)
                            .help("The name of an existing profile cannot be changed.")
                    }
                }
                GridRow {
                    label("Backend")
                    Picker("Backend", selection: $draft.backend) {
                        Text("Claude").tag("claude")
                        Text("Codex").tag("codex")
                    }
                    .pickerStyle(.segmented).labelsHidden().frame(width: 200)
                }
                if draft.isCodex { codexRows } else { claudeRows }
            }
            Text(draft.isCodex ? "Codex uses its native sign-in."
                               : "Tokens are saved in your local offload config file, readable only by you. They are never shown again.")
                .font(.system(size: 10)).foregroundStyle(.secondary).lineLimit(2).fixedSize(horizontal: false, vertical: true)
        }
    }

    @ViewBuilder private var claudeRows: some View {
        GridRow {
            label("Model")
            TextField("required", text: $draft.model).textFieldStyle(.roundedBorder)
        }
        GridRow {
            label("Gateway URL")
            VStack(alignment: .leading, spacing: 4) {
                if draft.baseURLHidden && !draft.replaceBaseURL {
                    Text("Saved (hidden: it contains private parts)").foregroundStyle(.secondary)
                    Toggle("Replace gateway URL", isOn: $draft.replaceBaseURL).controlSize(.small)
                } else {
                    TextField("https://…", text: $draft.baseURL).textFieldStyle(.roundedBorder)
                    if draft.baseURLHidden { Toggle("Replace gateway URL", isOn: $draft.replaceBaseURL).controlSize(.small) }
                }
            }
        }
        GridRow {
            label("Endpoint")
            Picker("Endpoint", selection: $draft.endpoint) {
                Text("none").tag(""); Text("cc").tag("cc"); Text("llm").tag("llm")
            }
            .pickerStyle(.segmented).labelsHidden().frame(width: 200)
        }
        GridRow {
            label("Credential")
            VStack(alignment: .leading, spacing: 4) {
                Picker("Credential", selection: $draft.credential) {
                    ForEach(draft.credentialChoices) { Text($0.label).tag($0) }
                }
                .pickerStyle(.segmented).labelsHidden()
                if let a = draft.original, draft.credential == .keep {
                    Text("Currently: " + authText(a.authMode)).font(.system(size: 10)).foregroundStyle(.secondary)
                }
                if draft.credential == .token || draft.credential == .tokenCmd {
                    SecureField(draft.credential == .token ? "New API token" : "New token command", text: $draft.secret)
                        .textFieldStyle(.roundedBorder)
                }
            }
        }
    }

    @ViewBuilder private var codexRows: some View {
        GridRow {
            label("Account")
            Picker("Account", selection: $draft.codexAccount) {
                Text("System Codex account").tag("")
                ForEach(snap?.managedAccounts ?? [], id: \.self) { Text($0).tag($0) }
                if !draft.codexAccount.isEmpty && !(snap?.managedAccounts ?? []).contains(draft.codexAccount) {
                    Text(draft.codexAccount + " (unavailable)").tag(draft.codexAccount)
                }
            }.labelsHidden()
        }
        GridRow {
            label("Add account")
            HStack {
                TextField("account-name", text: $accountName).textFieldStyle(.roundedBorder)
                Button("Sign in…") { profiles.addAccount(name: accountName) }
                    .disabled(profiles.accountLoginRunning || profiles.busy || accountName.isEmpty)
                Button("Import…") { importAccount() }
                    .disabled(profiles.busy || accountName.isEmpty)
            }
        }

        GridRow {
            label("Model")
            TextField("optional: inherit Codex default", text: $draft.model).textFieldStyle(.roundedBorder)
        }
        GridRow {
            label("Reasoning")
            Picker("Reasoning", selection: $draft.reasoning) {
                Text("inherit").tag("")
                ForEach(ProfileDraft.efforts.dropFirst(), id: \.self) { Text($0).tag($0) }
            }
            .labelsHidden().frame(width: 200)
        }
        GridRow {
            label("Fast")
            VStack(alignment: .leading, spacing: 3) {
                Picker("Fast", selection: $draft.fast) {
                    ForEach(draft.fastChoices) { Text($0.title).tag($0) }
                }
                .pickerStyle(.segmented).labelsHidden().frame(width: 200)
                .accessibilityLabel("Fast")
                .accessibilityValue(draft.fast.title)
                Text(draft.fast == .notSet
                     ? "Not set: follows your native Codex configuration. Choose Off or On to set it for this profile."
                     : "Fast changes the execution service tier. It does not guarantee a shorter task time.")
                    .font(.system(size: 10)).foregroundStyle(.secondary).lineLimit(2).fixedSize(horizontal: false, vertical: true)
            }
        }
    }

    @ViewBuilder private var messages: some View {
        if profiles.accountLoginRunning {
            Button("Cancel sign-in") { profiles.cancelAccountLogin() }
        }
        if !profiles.accountLoginOutput.isEmpty {
            ScrollView {
                Text(profiles.accountLoginOutput).font(.system(size: 11, design: .monospaced))
                    .textSelection(.enabled).frame(maxWidth: .infinity, alignment: .leading)
            }.frame(maxHeight: 90)
        }

        if changedOnDisk && !draft.isNew {
            HStack(spacing: 6) {
                Image(systemName: "info.circle.fill").foregroundStyle(Color.omBlue)
                Text("Profile changed on disk; your edits are kept.").font(.system(size: 11)).lineLimit(2)
                Spacer(minLength: 4)
                Button("Reload saved values") { choose(selection) }.controlSize(.small)
            }
        }
        if let e = formError ?? profiles.actionError {
            Label(e, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.orange)
                .font(.system(size: 11)).lineLimit(3).fixedSize(horizontal: false, vertical: true)
        } else if let n = notice {
            Label(n, systemImage: "checkmark.circle.fill").foregroundStyle(.green).font(.system(size: 11)).lineLimit(2)
        }
    }

    private var actions: some View {
        HStack {
            if !draft.isNew {
                Button("Use profile") { use(draft.name) }
                    .disabled(profiles.busy || draft.name == snap?.configuredDefault)
                    .help("Make this the default for new runs")
            }
            Spacer()
            Button(draft.isNew ? "Create and use" : "Save") { save() }
                .keyboardShortcut(.defaultAction)
                .disabled(profiles.busy || snap == nil || missingSecret)
                .help(missingSecret ? "Enter the new credential first" : "")
        }
    }

    private var missingSecret: Bool {
        !draft.isCodex && (draft.credential == .token || draft.credential == .tokenCmd)
            && draft.secret.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty
    }

    private func label(_ s: String) -> some View {
        Text(s).foregroundStyle(.secondary).frame(width: 84, alignment: .trailing)
    }

    private func authText(_ mode: String) -> String {
        switch mode {
        case "token": return "API token saved"
        case "token_cmd": return "token command saved"
        default: return "no credential"
        }
    }

    // MARK: actions

    private func choose(_ name: String) {
        guard let e = snap?.entry(name) else { return }
        selection = name
        draft = ProfileDraft(editing: e)
        changedOnDisk = false
        formError = nil
        notice = nil
        profiles.clearActionError()
    }

    private func syncSelection() {
        guard let s = snap else { return }
        if draft.isNew {
            // First load: select the active profile. A form the user is filling in is never replaced.
            if selection.isEmpty && !s.profiles.isEmpty && !draft.isDirty {
                choose(s.entry(s.active)?.name ?? s.profiles[0].name)
            }
            return
        }
        switch ProfileDraft.sync(draft: draft, stored: s.entry(selection)) {
        case .none: break
        case .reload(let e): draft = ProfileDraft(editing: e); changedOnDisk = false
        case .keepEdits: changedOnDisk = true
        case .removed: draft = ProfileDraft.new(); selection = ""; changedOnDisk = false
        }
    }

    private func importAccount() {
        let panel = NSOpenPanel()
        panel.title = "Import Codex auth.json"
        panel.message = "Choose an existing Codex OAuth auth.json. A private managed copy will be created."
        panel.canChooseDirectories = false
        panel.allowsMultipleSelection = false
        panel.begin { response in
            guard response == .OK, let url = panel.url else { return }
            Task { @MainActor in
                do {
                    let values = try url.resourceValues(forKeys: [.fileSizeKey])
                    guard (values.fileSize ?? 0) <= 1_048_576 else { formError = "Authentication file is too large."; return }
                    let data = try Data(contentsOf: url)
                    let name = accountName
                    if await profiles.importAccount(name: name, data: data) {
                        draft.codexAccount = name
                        accountName = ""
                        notice = "Managed account added. Save the profile to use it."
                    }
                } catch { formError = "Could not read the selected authentication file." }
            }
        }
    }

    private func save() {
        formError = nil
        notice = nil
        profiles.clearActionError()
        let request: ProfileSaveRequest
        do { request = try draft.request(existingNames: names) } catch {
            formError = (error as? LocalizedError)?.errorDescription ?? "Check the profile fields."
            return
        }
        let wasNew = draft.isNew
        Task {
            if await profiles.save(request) {
                draft.secret = ""
                if wasNew { selection = request.name }
                changedOnDisk = false
                if let e = profiles.snapshot?.entry(request.name) { draft = ProfileDraft(editing: e) }
                notice = wasNew ? "Created \"\(request.name)\" and set it as the default for new runs." : "Saved \"\(request.name)\"."
            }
        }
    }

    private func use(_ name: String) {
        formError = nil
        notice = nil
        Task {
            if await profiles.use(name) { notice = "\"\(name)\" is now the default for new runs." }
        }
    }
}

// MARK: - Agent skills

struct SkillsSettingsView: View {
    @ObservedObject var skills: AgentSkillsModel

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack {
                Text("Offload skill and delegation policy for your coding agents (user scope).").font(.system(size: 11)).foregroundStyle(.secondary)
                Spacer()
                if skills.busy { ProgressView().controlSize(.small) }
                Button { skills.refresh() } label: { Image(systemName: "arrow.clockwise") }
                    .buttonStyle(.borderless).disabled(skills.busy || !skills.isAvailable).help("Re-read the state on disk")
            }
            if let s = skills.snapshot {
                ForEach(s.agents) { AgentCard(agent: $0, skills: skills) }
                if !s.warnings.isEmpty {
                    Text(s.warnings.joined(separator: " ")).font(.system(size: 10)).foregroundStyle(.secondary)
                        .lineLimit(3).fixedSize(horizontal: false, vertical: true)
                }
            } else {
                Text(skills.loadError ?? "Loading…").foregroundStyle(.secondary).frame(maxWidth: .infinity, minHeight: 120)
            }
            if let e = skills.actionError ?? (skills.snapshot == nil ? nil : skills.loadError) {
                Label(e, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.orange)
                    .font(.system(size: 11)).lineLimit(3).fixedSize(horizontal: false, vertical: true)
            }
            Spacer(minLength: 0)
            Label("Restart running agent sessions to apply changes.", systemImage: "arrow.triangle.2.circlepath")
                .font(.system(size: 11)).foregroundStyle(.secondary)
            Text("This shows what is installed on disk for new sessions, not what an already-running session has loaded.")
                .font(.system(size: 10)).foregroundStyle(.secondary).lineLimit(2).fixedSize(horizontal: false, vertical: true)
        }
        .padding(14)
        .onAppear { if skills.snapshot == nil { skills.refresh() } }
    }
}

struct AgentCard: View {
    let agent: AgentSkill
    @ObservedObject var skills: AgentSkillsModel

    var body: some View {
        Card(tint: tint) {
            VStack(alignment: .leading, spacing: 5) {
                HStack {
                    Text(agent.title).font(.system(size: 13, weight: .semibold))
                    Text(agent.statusLabel).font(.system(size: 10, weight: .semibold))
                        .padding(.horizontal, 6).padding(.vertical, 1)
                        .background(tint.opacity(0.2), in: Capsule()).foregroundStyle(tint)
                    Spacer()
                    if skills.busyAgent == agent.agent { ProgressView().controlSize(.small) }
                    ForEach(agent.actions, id: \.self) { action in
                        Button(title(action)) { Task { await skills.setEnabled(agent.agent, action != .disable) } }
                            .disabled(skills.busy)
                    }
                }
                if !agent.message.isEmpty {
                    Text(agent.message).font(.system(size: 11)).foregroundStyle(.secondary)
                        .lineLimit(3).fixedSize(horizontal: false, vertical: true)
                }
                if agent.overrideShadowed {
                    Text("A more specific local override shadows this installation.").font(.system(size: 10)).foregroundStyle(.orange)
                }
                Text("Skill: \(agent.skillState) · Policy: \(agent.policyState)").font(.system(size: 10)).foregroundStyle(.secondary)
                Text(agent.skillPath).font(.system(size: 9, design: .monospaced)).foregroundStyle(.secondary)
                    .lineLimit(1).truncationMode(.middle).help(agent.skillPath)
                Text(agent.policyPath).font(.system(size: 9, design: .monospaced)).foregroundStyle(.secondary)
                    .lineLimit(1).truncationMode(.middle).help(agent.policyPath)
            }
        }
    }

    private func title(_ a: AgentSkill.Action) -> String {
        switch a {
        case .enable: return "Enable"
        case .update: return "Update"
        case .disable: return "Disable"
        }
    }

    private var tint: Color {
        switch agent.status {
        case "enabled": return agent.updateAvailable ? .omBlue : .omTeal
        case "blocked", "shadowed": return .orange
        case "partial": return .omPurple
        default: return .secondary
        }
    }
}

// MARK: - App

struct AppSettingsView: View {
    @ObservedObject var updates: UpdateManager
    @ObservedObject var motion: MotionController

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("Updates").font(.system(size: 13, weight: .semibold))
            Toggle("Check for updates automatically", isOn: $updates.autoCheck)
            Text("New releases show a red dot. Review the version and confirm before downloading and installing.")
                .font(.system(size: 10)).foregroundStyle(.secondary)
            Toggle("Restart when the installed App changes", isOn: $updates.restartOnAppChange)
            Text("Release updates replace the whole App and its bundled CLI. Restart waits for chats, drafts and profile edits.")
                .font(.system(size: 10)).foregroundStyle(.secondary)
            if let reason = updates.restartWaitReason {
                Text(reason).font(.system(size: 10)).foregroundStyle(.orange).lineLimit(2)
            } else if let problem = updates.localChanges.problem {
                Text(problem).font(.system(size: 10)).foregroundStyle(.orange).lineLimit(2)
            }
            if let cli = updates.localChanges.cliURL {
                Text("Selected CLI: " + cli.path).font(.system(size: 9, design: .monospaced)).foregroundStyle(.secondary)
                    .lineLimit(1).truncationMode(.middle).help(cli.path)
                Text(updates.localChanges.cliChangedAt.map { "CLI changed on disk · data refreshed \($0.formatted(date: .omitted, time: .shortened))" }
                     ?? "CLI-only changes refresh data. A global CLI update does not replace this App's bundled CLI.")
                    .font(.system(size: 10)).foregroundStyle(.secondary).lineLimit(2)
            }
            Divider()
            Text("Appearance").font(.system(size: 13, weight: .semibold))
            Toggle("Animate status symbol", isOn: $motion.decorativeEnabled)
                .disabled(motion.systemReduceMotion)
            Text(motion.systemReduceMotion
                 ? "Off because Reduce Motion is on in System Settings. The status text and counts are unchanged."
                 : "A slow fade while work is running, and one short pulse when something needs attention. The status text and counts are the same with it off.")
                .font(.system(size: 10)).foregroundStyle(.secondary).lineLimit(2).fixedSize(horizontal: false, vertical: true)
            Divider()
            HStack {
                Button("Check for Updates Now") { updates.checkNow(manual: true) }.disabled(updates.isBusy)
                Text(status).font(.system(size: 11)).foregroundStyle(.secondary).lineLimit(2)
            }
            Spacer(minLength: 0)
            Text("Offload Monitor \(updates.currentVersionText)").font(.system(size: 10)).foregroundStyle(.secondary)
        }
        .padding(14)
    }

    private var status: String {
        switch updates.phase {
        case .idle: return ""
        case .checking: return "Checking…"
        case .upToDate(let at): return "Up to date · checked \(at.formatted(date: .omitted, time: .shortened))"
        case .available(let c): return "Version \(c.version) is available."
        case .downloading(let c): return "Downloading \(c.version)…"
        case .ready(let c): return "Version \(c.version) is ready. Use Update and Restart in the main panel."
        case .installing(let c): return "Installing \(c.version)…"
        case .failed(let m): return m
        }
    }
}
