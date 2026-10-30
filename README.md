# offload

Delegate focused tasks from Codex or Claude Code to another model or account.
Get a final report, session history, token usage and elapsed time while the
supervising agent keeps design decisions and final review.

- **Two backends:** Codex CLI and Claude Code with Anthropic-compatible gateways.
- **Named profiles:** select a model, provider or managed Codex account per task.
- **Persistent sessions:** resume related work and inspect previous reports.
- **macOS dashboard:** track tasks, switch profiles and manage accounts.

<img src="docs/images/codex-account-profiles.png" alt="Offload Monitor with named Codex account profiles and concurrent tasks" width="480">

## Quick start

Requires Go 1.24+ and a signed-in `codex`, or `claude` configured with a supported
gateway. Both backend CLIs run locally.

```sh
go install github.com/zhoushoujianwork/offload@latest
offload config
offload -d /path/to/project "Find the retry logic. Return file:line references. Do not edit files."
```

Use `offload config` to create a profile. For a Codex profile using your existing
sign-in, you can also run:

```sh
offload profile save --json <<'JSON'
{"name":"codex","create":true,"backend":"codex"}
JSON
```

Creating a profile selects it as the default. New features may require building
from the current source with `go install .`.

## Everyday commands

```sh
offload profiles                         # list profiles
offload -p codex -json "Inspect this project without editing files."
offload -w -d /path/to/project "Apply the agreed fix and run the relevant tests."
offload -r <session> "Review the previous change for edge cases."
offload status                           # running and queued tasks
offload show <session>                   # saved reports
offload stats                            # usage and estimated costs
```

`-p` selects a profile for one task; `offload default <name>` changes the saved
default. `OFFLOAD_PROFILE` overrides that default. Resumes retain their recorded
account. Costs are estimates when available, not billing records.

**Execution scope:** default-policy Codex workers bypass approvals and sandboxing.
`-w` communicates write intent; it does not enforce a filesystem boundary. Give
workers a clear task and authorization scope. See [permissions](docs/guide.md#permissions).

## Codex account profiles

Use one profile per account, such as `codex-work` and `codex-personal`.
In the app, open **Settings → Profiles**, choose Codex, then **Sign in…** or
**Import…**. Select the account and save the profile.

```sh
offload accounts add work                # device sign-in
# Or: offload accounts import work < /path/to/auth.json
offload profile save --json <<'JSON'
{"name":"codex-work","create":true,"backend":"codex","codex_account":"work"}
JSON
offload -p codex-work "Your task"
```

Each managed account has its own credentials and sessions. CodexBar accounts can
be imported from their managed `auth.json`; imports are separate copies, not
continuous synchronization.

The [agent skill](skills/offload/SKILL.md) explains random account selection and
bounded failover on quota errors. This selection happens in the supervising
agent; a CLI call without `-p` still uses the configured default.
[Account setup and routing details →](docs/guide.md#managed-codex-accounts)

## Agent integration

```sh
offload install-agent
```

Installs the skill and delegation instructions for Codex and Claude Code.
Start a new agent session to load them. Explicit user instructions, including
turning delegation off, take precedence.
[Integration options →](docs/guide.md#set-up-codex-and-claude-code)

## macOS app

Download the Apple silicon or Intel app from [Releases](https://github.com/zhoushoujianwork/offload/releases).
The app includes its own CLI; updating a standalone CLI does not update the app.
Release installation requires confirmation and verifies the downloaded bundle.
The app is ad-hoc signed and not notarized.

For local development:

```sh
make dev          # build, replace the whole App + CLI, and restart
make dev-build    # build only
```

Dev builds display `-dev.<timestamp>.<commit>` and append `.dirty` for uncommitted
changes. `make dev` installs to `~/Applications/Offload Monitor.app`, requests a
normal quit and verifies a new process. Save current work first; it never
force-kills the app. [Development updates →](docs/guide.md#fast-local-development-updates)

## Documentation and development

- [User guide](docs/guide.md): configuration, scheduling, JSON interfaces, limits and troubleshooting.
- [Agent skill](skills/offload/SKILL.md): delegation workflow and account selection.
- [Development and checks](docs/guide.md#development): repository layout and CI commands.
- [Release packaging](docs/guide.md#release-downloads): assets, signatures and updater verification.

[MIT License](LICENSE)
