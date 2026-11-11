import AppKit
import SwiftUI

/// Settings > App: which `offload` commands exist, their versions, what an App update changes, and the opt-in sync.
struct CLIInstallsSection: View {
    @ObservedObject var installs: CLIInstallsModel
    /// The CLI this App talks to, shown so it is clear which copy the App itself uses.
    var selected: URL?
    var onSync: (CLISyncPlan) -> Void = { AppState.shared.confirmCLISync($0) }

    static let listHeight: CGFloat = 152

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text("Command-line offload").font(.system(size: 13, weight: .semibold))
                Spacer()
                Button(installs.scanning ? "Scanning…" : "Rescan") { installs.startRefresh() }
                    .controlSize(.small).disabled(installs.busy)
            }
            Text(CLIInstallsCopy.scope).font(.system(size: 10)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            if let selected {
                Text("The App itself uses: " + selected.path).font(.system(size: 9, design: .monospaced)).foregroundStyle(.secondary)
                    .lineLimit(1).truncationMode(.middle).help(selected.path)
            }
            if let report = installs.report {
                ScrollView(showsIndicators: true) {
                    VStack(alignment: .leading, spacing: 6) {
                        if let b = report.bundled { InstallRow(install: b, assessment: nil, onSync: onSync, busy: installs.busy) }
                        ForEach(installs.assessed(), id: \.install.id) { item in
                            InstallRow(install: item.install, assessment: item.assessment, onSync: onSync, busy: installs.busy)
                        }
                        if report.standalone.isEmpty {
                            Text(report.bundled == nil
                                 ? "No bundled CLI in this build, and no offload found in PATH."
                                 : "No separate offload in PATH. Only the App's copy exists.")
                                .font(.system(size: 10)).foregroundStyle(.secondary)
                        }
                        ForEach(report.bundledLinks, id: \.self) { link in
                            Text("\(link) links to the App's CLI and follows App updates.")
                                .font(.system(size: 9, design: .monospaced)).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
                        }
                    }
                    .frame(maxWidth: .infinity, alignment: .leading)
                }
                .frame(height: Self.listHeight)
            } else {
                Text(installs.summary).font(.system(size: 10)).foregroundStyle(.secondary)
            }
            SyncStatus(state: installs.syncState, dismiss: { installs.dismissSyncState() })
            HStack(spacing: 6) {
                Text("Check in a new Terminal:").font(.system(size: 10)).foregroundStyle(.secondary)
                Text(CLIAdvisor.verifyCommand).font(.system(size: 10, design: .monospaced)).textSelection(.enabled)
                Button("Copy") {
                    NSPasteboard.general.clearContents()
                    NSPasteboard.general.setString(CLIAdvisor.verifyCommand, forType: .string)
                }.controlSize(.mini)
            }
        }
    }
}

enum CLIInstallsCopy {
    static let scope = "A release update replaces only Offload Monitor.app and the CLI inside it. A separate offload in your PATH is never changed without your confirmation."
}

private struct InstallRow: View {
    var install: CLIInstall
    var assessment: CLIAssessment?
    var onSync: (CLISyncPlan) -> Void
    var busy: Bool

    private var tint: Color {
        switch assessment?.tone {
        case .attention: return .orange
        case .ok: return .omTeal
        default: return .secondary
        }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            HStack(spacing: 6) {
                Image(systemName: assessment == nil ? "app.badge" : "terminal").foregroundStyle(tint).font(.system(size: 11))
                Text(assessment == nil ? "App bundled" : "PATH").font(.system(size: 10, weight: .semibold))
                Text(install.version ?? "version unknown").font(.system(size: 10, design: .monospaced))
                Text("· " + install.management.label).font(.system(size: 10)).foregroundStyle(.secondary)
                Spacer(minLength: 0)
                if case .sync(let plan)? = assessment?.action {
                    Button("Update…") { onSync(plan) }.controlSize(.mini).disabled(busy)
                }
            }
            Text(install.path).font(.system(size: 9, design: .monospaced)).foregroundStyle(.secondary)
                .lineLimit(1).truncationMode(.middle).help(install.resolvedPath == install.path ? install.path : "\(install.path) → \(install.resolvedPath)")
            if let a = assessment {
                Text(a.headline).font(.system(size: 10, weight: .medium)).foregroundStyle(a.tone == .attention ? Color.orange : Color.primary)
                    .fixedSize(horizontal: false, vertical: true)
                if !a.detail.isEmpty {
                    Text(a.detail).font(.system(size: 9.5)).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                        .textSelection(.enabled)
                }
            }
        }
        .padding(6)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(tint.opacity(0.08), in: RoundedRectangle(cornerRadius: 6))
    }
}

private struct SyncStatus: View {
    var state: CLIInstallsModel.SyncState
    var dismiss: () -> Void

    var body: some View {
        switch state {
        case .idle: EmptyView()
        case .running(let path):
            Text("Replacing \(path)…").font(.system(size: 10)).foregroundStyle(.secondary)
        case .done(let r):
            HStack(alignment: .top) {
                Text("Updated \(r.path) to \(r.toVersion). Backup: \(r.backupPath)")
                    .font(.system(size: 10)).foregroundStyle(.green).fixedSize(horizontal: false, vertical: true).textSelection(.enabled)
                Button("OK", action: dismiss).controlSize(.mini)
            }
        case .failed(let m):
            HStack(alignment: .top) {
                Text(m).font(.system(size: 10)).foregroundStyle(.red).fixedSize(horizontal: false, vertical: true).textSelection(.enabled)
                Button("OK", action: dismiss).controlSize(.mini)
            }
        }
    }
}
