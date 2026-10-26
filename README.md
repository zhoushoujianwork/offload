# offload

Hand token-heavy grunt work from a supervising agent, such as Codex or Claude Code, to a cheaper model. Get back the final report, estimated worker cost, and measured elapsed time.

`offload` runs Claude Code against an Anthropic-compatible gateway (MiniMax, DeepSeek, Kimi, GLM, a LiteLLM proxy, …), or runs `codex exec` using your Codex login and configuration. Use it throughout investigation, code localization, implementation, testing, validation, and documentation. The supervisor owns design decisions, delegates work with clear scope and acceptance criteria, and verifies the result.

```text
$ offload -d /absolute/path/app "Find retry logic; return file:line references, max 10 rows. Do not edit files."
internal/http/client.go:42  doWithRetry — exponential backoff, 3 attempts
...
[offload] ok exit_code=0 minimax/MiniMax-M3 turns=6 estimated_cost=$0.0753 elapsed=20.104s session=07bcaf2c-…
```

把读文件、翻日志、代码检索、机械性修改这类耗 token 的工作交给便宜模型，主 Agent 保留规划、核验和最终判断。

https://github.com/user-attachments/assets/5f77d50f-7338-4a12-a740-af4c91c6e50d

<sub>44 秒横屏介绍视频（1080p，中文字幕，合成配乐，无旁白）。画面是流程动画，非原生应用录屏；演练数据来自一次真实的 MiniMax-M3 检索，成本为 worker 估算值，不是实际账单。</sub>

## Install

Requires Go 1.24+ and the CLI for your selected backend: [Claude Code](https://docs.anthropic.com/en/docs/claude-code) (`claude` on PATH or at `~/.claude/local/claude`) or [Codex](https://developers.openai.com/codex/cli/) (`codex` on PATH, already signed in).

```bash
go install github.com/zhoushoujianwork/offload@latest
```

To install from a source checkout, run `go install .` in that checkout. If an older tagged release does not recognize `install-agent` or `-json`, use a checkout containing these features.

## Configuration

Run `offload config`, select **Profile**, then **Add profile** to save a bundle and use it immediately. This also works before a configuration file exists. Or create `~/.config/offload/config.json` (chmod 600):

```json
{
  "default": "minimax",
  "profiles": {
    "minimax": {
      "base_url": "https://api.minimaxi.com/anthropic",
      "model": "MiniMax-M3",
      "token_cmd": "security find-generic-password -s offload-minimax -w"
    },
    "codex": {
      "backend": "codex",
      "model": "gpt-6.1-sol",
      "reasoning_effort": "low",
      "service_tier": "default"
    },
    "codex-deep": {
      "backend": "codex",
      "model": "gpt-6.1-sol",
      "reasoning_effort": "xhigh",
      "service_tier": "priority"
    },
    "deepseek": {
      "base_url": "https://api.deepseek.com/anthropic",
      "model": "deepseek-chat",
      "token_cmd": "printenv DEEPSEEK_API_KEY"
    }
  }
}
```

`token_cmd` (preferred) prints the token on stdout, so secrets can live in the macOS Keychain, `pass`, 1Password CLI, etc. A literal `token` field also works.

### How switching works

Profiles select a `backend`: `claude` (the default for existing profiles) or `codex`. Claude profiles provide the three variables Claude Code reads. For each Claude run, offload drops every `ANTHROPIC_*` variable from the caller's environment and sets exactly:

| profile field | worker env |
|---|---|
| `base_url` | `ANTHROPIC_BASE_URL` |
| `token` / `token_cmd` | `ANTHROPIC_AUTH_TOKEN` (sent as `Authorization: Bearer`) |
| `model` (or `-m` override) | `ANTHROPIC_MODEL` |

`--setting-sources ""` disables user, project, and local settings so their environment overrides cannot replace the profile's gateway, token, or model. `--bare` alone does not isolate settings. Authentication is environment-only; no `apiKeyHelper` or `--settings` argument is used. Tokens never appear in argv.

### Codex workers

A profile with `"backend": "codex"` runs `codex exec --json`. It uses your existing Codex authentication, provider, and configuration, including `CODEX_HOME`. The optional `model` field or `-m` selects a model; omit both to use the Codex default. Log in with the native Codex CLI before running a Codex profile. Optional `reasoning_effort` and `service_tier` override native settings for that worker only; omitted or empty values inherit them. The wizard offers Inherit/Low/Medium/High/Extra high reasoning and Inherit/Standard/Fast speed (`default`/`priority`). Fast is not a token or cost savings guarantee. Gateway fields (`base_url`, `token`, `token_cmd`) apply only to Claude profiles. `OFFLOAD_CODEX` can override the Codex executable.

Codex runs with approval policy `never`, using the `read-only` sandbox by default and `workspace-write` with `-w`. These are Codex sandbox policies, separate from the Claude tool approval lists below. Instructions and the task are sent over stdin. Codex also discovers its own agent instructions. JSONL events supply the report, session ID, and live tool progress; `turns` counts tool calls. Codex reports token usage but no USD cost, so offload shows `unknown` (`null` in JSON), and `offload ls` explains it as `cost unavailable (Codex does not report USD cost)`. offload never prices tokens itself.

Interrupted Codex runs retain progress and a resume command. Automatic tool-less salvage is supported only for Claude.

### Choosing a profile

Choose per run with `-p name`, per shell with `OFFLOAD_PROFILE=name`, otherwise through `default` in the config. `offload profiles` lists profiles, with `*` marking the selected default; it never prints tokens.

```bash
offload profiles
offload default codex     # save Codex as the default
offload default minimax   # switch back to Claude through this profile
offload default           # show the saved default
offload -p codex -d /absolute/path/app "Summarize README.md, max 5 bullets."
offload -p minimax -d /absolute/path/app "Summarize README.md, max 5 bullets."
OFFLOAD_PROFILE=deepseek offload -d /absolute/path/app "Find retry entry points, max 10 file:line rows."
```

`offload default <profile>` validates the profile and saves it without changing other configuration fields. `OFFLOAD_PROFILE` still overrides the saved default for new runs. Resuming a known session reuses its original profile and directory; `-p` and `-d` explicitly override them.

`-m` overrides the selected profile's model for that run.

## Delegation mode

Delegation mode controls how proactively the installed policy tells a supervising agent to hand work to offload. It is independent of the provider `profile`/model selection above and does not change worker permissions (`-w` still opts in to edits).

| Mode | Behavior |
|---|---|
| `basic` | Mainly delegates research, code localization, summaries, and bounded mechanical edits. Whole-feature implementation stays with the main agent unless the user explicitly requests delegation. |
| `proactive` (default) | Also delegates bounded feature/bugfix implementation, its tests, and docs: the main agent decides requirements, interfaces, behavior, allowed files, and acceptance checks, then actually invokes `offload -w`. |

In both modes, actually invoking offload is the default before substantial bounded supporting work, to reduce the supervising model's token use: cross-file localization, bounded evidence gathering, comparisons, and bulk summaries go to a worker before the same material is read locally. One quick orientation lookup is fine; when it expands, delegate. Scope may be narrowed after that orientation, and an open-ended overall objective is not a reason to keep all bounded research local. Otherwise eligible execution work stays local only for a concrete tiny known-file check, a user restriction, a security boundary, or an unavailable worker, with a brief stated reason. Trivial tasks need no call, and savings are not claimed without measurement.

Both modes keep architecture/design decisions, important tradeoffs, and final independent review with the supervising agent, and keep the existing permission, monitoring, resume, and no-recursive-worker rules.

```bash
offload mode              # print the saved mode (unset config defaults to proactive)
offload mode basic        # switch both installed agents (user scope) to basic
offload mode proactive    # switch back to proactive
```

`offload mode` with no argument only reads the saved mode and does not create a config file or touch installed instructions. Setting it validates the mode first, then updates the installed policy/skill for both Codex and Claude Code (user scope) using the same install logic as `install-agent`, and finally saves `delegation_mode` in the config, preserving every other field (profiles, default, credentials). If persisting the choice fails after the instructions were already updated, the command reports that explicitly so you know the files and the saved mode are out of sync. Repeating the same mode leaves already matching files unchanged. **Start a new agent session after changing the mode** so it picks up the updated instructions; a session already running keeps its previously loaded copy.

## Interactive settings

```bash
offload config   # open settings in an interactive terminal
offload          # also opens settings when stdin and stdout are terminals
```

Use the up/down arrow keys to choose an item and Enter to confirm. Each change is automatically saved and applied when confirmed, with no separate Save or Apply step. Ctrl-C cancels a submenu or exits the main menu; cancelling does not save a change.

The **Offload level** setting uses a horizontal slider. Press Left/Right to choose **Less offload** (`basic`) or **More offload** (`proactive`), then Enter to apply. More offload shifts more work away from the caller model: it also delegates implementation, tests, and documentation after the main agent designs the change. Less offload covers research, code lookup, and mechanical changes. The main agent always owns design and final review. Actual caller token savings depend on the task and coordination overhead; this is a work-sharing setting, not a measured savings percentage. Existing mode values and CLI commands remain compatible.

The main menu contains **Profile**, **Offload level**, and **Exit**. Profile lists saved bundles with their backend, model, and explicit reasoning/speed settings. Enter on a saved bundle immediately selects it for the next task. **Add profile** collects a new bundle; **Edit current profile** updates the saved default's backend-specific fields while keeping its name and backend. There is no separate model setting. Long menu items shorten to fit the terminal width. An `OFFLOAD_PROFILE` shell override is shown separately and still takes precedence.

Codex models may be empty to inherit the native default. Claude gateway profiles require an HTTP(S) base URL, model, and token command or masked API token. Configuration never executes the token command. Editing can keep current authentication without displaying it, or replace it. A completed wizard saves the entire bundle atomically; Ctrl-C at any step leaves configuration unchanged.

Mode changes also update the installed Codex and Claude instructions. Start a new agent session to load them. The interactive menu hides successful installation/save logs and returns to the current settings; errors remain visible. Piped task input and existing non-interactive commands continue to work as before.

## Set up Codex and Claude Code

Install the bundled skill and delegation policy into each supervising agent's main instruction file:

```bash
offload install-agent                         # both agents, user scope, saved mode (default proactive)
offload install-agent -agent codex            # Codex only
offload install-agent -agent claude           # Claude Code only
offload install-agent -scope project -d /absolute/path/app
offload install-agent -mode basic             # install basic mode for this run only; does not change the saved mode
```

`-agent` accepts `codex`, `claude`, or `all` (default). `-scope` accepts `user` (default) or `project`. Project scope defaults to the current directory; `-d` is only valid with project scope. `-mode` accepts `basic` or `proactive` and overrides the mode for that install only; without it, `install-agent` uses the saved mode from `offload mode` (absent config defaults to `proactive`). This installs instructions and skills; configure gateway credentials separately above.

| Agent / scope | Skill directory | Main instruction file |
|---|---|---|
| Codex / user | `~/.agents/skills/offload` | `$CODEX_HOME/AGENTS.md` (default `~/.codex/AGENTS.md`) |
| Claude Code / user | `$CLAUDE_CONFIG_DIR/skills/offload` (default `~/.claude/skills/offload`) | `$CLAUDE_CONFIG_DIR/CLAUDE.md` (default `~/.claude/CLAUDE.md`) |
| Codex / project | `<dir>/.agents/skills/offload` | `<dir>/AGENTS.md` |
| Claude Code / project | `<dir>/.claude/skills/offload` | `<dir>/CLAUDE.md` |

The installer preserves unrelated instructions and maintains one `<!-- offload:begin -->` … `<!-- offload:end -->` block. Repeating the command updates that block and an unchanged installer-owned skill; it does not duplicate them. It prints `installed`, `updated`, or `unchanged` for each file.

Existing unmanaged, edited, or symlinked skill targets are preserved and installation fails before writing. Inspect and back up the existing skill directory or link before moving it aside and rerunning. Also migrate older `~/.codex/skills/offload` links to avoid duplicate skill names. There is no force-overwrite mode.

Codex's current skill locations are documented in [Build skills](https://learn.chatgpt.com/docs/build-skills#where-codex-loads-local-skills); `CODEX_HOME` controls its instruction/state directory, not this installer's user skill directory. The installer warns if `AGENTS.override.md` replaces `AGENTS.md`; include the policy in your active instructions. Start a new agent session to load changed instructions; verify that the `offload` skill is available.

### Main agent and subagent workflow

The installed policy asks main agents and native subagents to actually invoke offload before doing substantial bounded supporting work themselves, according to the active delegation mode. In **proactive** mode this includes bounded implementation once the design is decided; in **basic** mode it mainly covers research, localization, summaries, and bounded mechanical edits, without proactively handing off a whole feature's implementation. In both modes, the main agent owns requirements, architecture/design, tradeoffs, task decomposition, and final review/acceptance. If a worker finds the design, scope, or architecture needs to change, it reports findings/options back instead of redesigning on its own. Prefer calling the CLI directly, without native subagent overhead, and avoid duplicating work an active worker is already doing. For parallel work or independent review, a native Codex subagent can launch the external worker:

```text
Main agent → optional native subagent → offload → selected Claude/Codex profile
                 ← verified conclusion + worker metrics ← final report
```

This uses an external worker process; it does not change Codex's native subagent model provider. Give a suitable subagent this instruction for bulk reading:

> Use offload for the bulk reading in /absolute/path/app, read-only. Find retry entry points and tests; return at most 10 file:line rows. Verify two key references and report the model, session, estimated worker cost, and elapsed time.

In **proactive** mode, once the design is decided, hand off bounded implementation the same way, explicitly with `-w`:

```bash
offload -w -json -d /absolute/path/app "Design decided: expose the existing 3-attempt retry limit as maxAttempts (default 3); preserve backoff and error behavior. Only edit internal/http/client.go and internal/http/client_test.go. Update the API comment, test default and explicit limits, and run go test ./internal/http. No commit, push, or unrelated changes. Report changes, check results, and any design questions."
```

In **basic** mode, keep whole-feature implementation in the main agent; `-w` is still available for mechanical edits or work the user's task explicitly authorizes, such as a scoped rename across a package.

Check uncertain API assumptions before handing off a large implementation: use a scoped read-only probe of the installed API or a minimal reproduction, then delegate one coherent slice with known entry points and acceptance checks. Both worker backends are instructed to return a **BLOCKED** report after an inconclusive focused feasibility check, including evidence, edits made, checks run, and the decision needed from the supervisor. This is a model instruction, not a hard tool-call limit; timeout and cancellation behavior are unchanged. Avoid killing a still-active worker solely because it has not edited yet and then duplicating its investigation locally. Refine the task using its findings and resume the existing session; deliberate cancellation skips automatic salvage, so recover a report with a report-only resume if needed.

Keep short conversations, trivial lookups, architecture choices, sensitive changes, and final review with the supervisor. Workers already launched by offload should finish their assigned task directly instead of delegating again. Default to read-only for inspection; use `-w` explicitly for authorized implementation rather than silently falling back to read-only. Spot-check reports; after authorized `-w` work, inspect the actual diff and independently run relevant checks yourself.

## Usage

```text
offload [flags] "task"        run a task (task may also come on stdin)
offload config               open interactive settings (terminal only)
offload ls [n]                list the last n runs (default 10)
offload show <session>        print every report of a session
offload status                what each worker is doing right now
offload watch                 one line per event (started, progress, STALLED, finished) until all workers end
offload profiles              list profiles (* = the one a run would use)
offload default [profile]     show or set the saved default profile
offload mode [basic|proactive] show or set the saved delegation mode
offload install-agent [flags] install skills and delegation agent instructions
offload version

  -p profile   profile name (default: $OFFLOAD_PROFILE, then config default)
  -m model     override the profile's model
  -w           write mode (default: read-only)
  -r session   resume a session; reuses its original dir unless -d is given
  -d dir       working directory (default: cwd)
  -t 30m       worker timeout; a worker silent for 10 minutes is stopped earlier
  -no-salvage  do not ask a stopped worker for a report from what it already read
  -json        emit one JSON run record instead of report-only stdout
```

- **stdout**: the final report by default, or one record with `-json`. **stderr**: one summary line with status, exit code, profile/model, turns, estimated cost, elapsed time, and session.
- **Exit codes**: 0 ok, 1 worker error, 2 usage/config error or unreachable gateway, 124 timeout.
- **History**: runs, including failures, are appended to `$XDG_STATE_HOME/offload/runs.jsonl` (default `~/.local/state/offload/runs.jsonl`). Use `ls` for per-run metrics, `show` for reports and metrics, or `-r` to resume a session.

`offload ls 3` shows the last three runs in append order. Each compact four-line summary includes status, elapsed time, backend-dependent turns, estimated cost, project basename, profile, a task excerpt, and the full session ID. Unknown metrics remain explicit. Use `offload show <session>` for the full project path and report.

```text
2026-10-26T10:00:00Z | OK | 1m47s | 4 turns | est. $0.0123
  app | cheap / m-1
  Review the retry logic
  Session: 12345678-1234-1234-1234-123456789abc
```

### Cost and elapsed time

Codex does not provide a USD cost; its `cost_usd` is `null`. For Claude, `cost_usd` comes from the worker's reported estimate; `cost_is_estimate` is `true`. It excludes the parent Codex/Claude agent and any native subagent coordination. Actual provider bills and total savings are unverified. Missing cost is `null` in JSON and `unknown` in the summary line; a reported zero is valid and shown as `$0.0000`. `offload ls` marks recorded values `est.` and, when cost is missing, prints the reason: `cost unavailable (Codex does not report USD cost)`, `(worker did not report cost)`, `(legacy zero is ambiguous)` or `(invalid value recorded)`. The reason is derived when history is read and is not stored or added to JSON. A legacy record without `cost_is_estimate` whose cost is `0` cannot be told apart from a missing cost, so it is treated as unavailable; a legacy non-zero cost is kept as an estimate. Old history still loads, with unavailable cost, exit code, or elapsed time shown as `unknown`.

`elapsed_ms` measures wall-clock time from CLI entry, including setup and credential retrieval. `duration_ms` preserves the worker-reported duration. The `-t` timeout begins after prompt setup, credential/executable resolution, and the reachability preflight, so elapsed time can exceed it.

```bash
offload -json -d /absolute/path/app "Read README.md and summarize setup, max 5 bullets. Do not edit files."
```

Example JSON (illustrative):

```json
{
  "ts": "2026-10-14 12:00:00",
  "status": "ok",
  "exit_code": 0,
  "profile": "minimax",
  "model": "MiniMax-M3",
  "dir": "/absolute/path/app",
  "task": "Read README.md and summarize setup, max 5 bullets. Do not edit files.",
  "session": "07bcaf2c-example",
  "turns": 6,
  "cost_usd": 0.0753,
  "cost_is_estimate": true,
  "duration_ms": 19000,
  "elapsed_ms": 20104,
  "result": "Setup summary…"
}
```

Failures can also include an `error` field. Worker error types and messages are retained, such as `error_max_turns`, and printed when the worker returns no report. For each external worker, supervisors should report status/exit code, profile/model, turns, estimated cost, elapsed time, and session ID.

### Permissions

| Mode | Automatically approved tools |
|---|---|
| read-only (default) | Read, Grep, Glob, `git log/diff/show`, `ls`, `rg` |
| `-w` | Read, Grep, Glob, Edit/Write (`acceptEdits`), `git status/diff`, `ls`, `rg`, build/test commands (`go`, `make`, `npm test/run`, `pytest`, `cargo`, `idf.py build`) |

These Claude tool approval rules are not an operating-system sandbox or a guarantee that build scripts cannot change files, access other paths, or use the network. Set explicit task constraints, inspect scripts and edits, and honor the host's permissions. Commit, push, deletion, and network tools are not included in the approval list.

`--bare` skips hooks, plugins, and automatic CLAUDE.md discovery. Project `CLAUDE.md` / `AGENTS.md` instructions and worker constraints are combined into one private temporary prompt file, passed with `--append-system-prompt-file` and removed after the run. Their contents stay out of argv.

### Codex host permissions and reachability

Install the Codex instructions with `offload install-agent -agent codex`. The CLI needs network access and writes state outside the project (`~/.claude` or `$CODEX_HOME`, plus the offload history directory). Follow the host's permission policy; use approved escalation or an allowed environment only when available and needed.

Before launching Claude, a reachability preflight attempts a TCP connection to the configured gateway with a 5s timeout. If unreachable, offload exits 2 with `cannot reach gateway`, rather than leaving the worker to retry until its task timeout. A configured HTTP/SOCKS proxy skips this direct check because the proxy determines reachability. This check does not validate credentials or model access.

## Known limits

- The worker is headless, so permission prompts cannot be approved. With Claude, compound shell commands (pipes, `&&`, heredocs, `python -c`) are refused and burn a turn. Read-only `rtk` forms (`rtk ls`, `rtk read`, `rtk grep`, `rtk git log`, ...) are allowed; `rtk run`, `proxy`, `err`, `summary`, `pipe`, `test` and `find` are not; `permission_denials=N` in the summary line counts them.
- A Claude worker stopped by `-t`, by 10 minutes of silence, or ending without a result is asked once, with all tools off, for a report from what it already read. The result starts with `[SALVAGED REPORT ...]`, the summary line shows `salvaged=true`, and `-json` has `"salvaged": true`. Its `cost_usd` covers only that step. It is partial and unverified. `-no-salvage` skips the step; if nothing could be salvaged you get the progress and an `offload -r <session>` command. For such runs `turns` counts tool calls.
- A worker that prints nothing for 10 minutes (a hung model request) is stopped without waiting for `-t`; the idle limit is fixed. Large tasks are slow (20-40s per turn). Split broad audits instead of raising `-t`; keep `-t` at its default unless the task needs more.
- Requires network access and writes state outside the project (`~/.claude` or `$CODEX_HOME`, plus the offload history directory). Use host-approved permissions; when offloading is unavailable, report the failure and continue with local tools or native subagents.
- Some gateways reject Claude Code's `anthropic-beta: claude-code-*` header and cannot be used as workers.
- Nested workers inherit host restrictions. For example, the Claude desktop app may prevent file writes; verify the host supports the requested mode.

## Development

```text
main.go                    entry point; only wires the version into cli.Run
internal/cli/              flags, subcommands (ls, show, status, watch, profiles), run summary line
internal/worker/           Claude/Codex worker runs: args, env, prompt, stream-json parsing, timeout progress
internal/config/           gateway profiles and token lookup
internal/history/          the run log (runs.jsonl) and its Record type
internal/live/             per-process live state files that status and watch read
internal/agentinstall/     install-agent: skill copy and the managed policy block
internal/text/             small string helpers
skills/                    the embedded SKILL.md (skills/embed.go exposes it)
```

Run `gofmt -l .`, `go vet ./...` and `go test -race ./...` before pushing; CI runs the same. The worker prompt and the installed policy live in `internal/worker/worker.go` and `internal/agentinstall/install.go`; change them there, then run `offload install-agent` to refresh installed copies. `skills/offload/SKILL.md` is embedded into the binary, so edit it and rebuild.

## License

MIT
