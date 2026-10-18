# offload

Hand token-heavy grunt work from a supervising agent, such as Codex or Claude Code, to a cheaper model. Get back the final report, estimated worker cost, and measured elapsed time.

`offload` runs Claude Code against an Anthropic-compatible gateway (MiniMax, DeepSeek, Kimi, GLM, a LiteLLM proxy, …), or runs `codex exec` using your Codex login and configuration. Use it for reading many files, code searches, log analysis, document summaries, and bounded mechanical edits. The supervisor plans the work and verifies the result.

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

`~/.config/offload/config.json` (chmod 600):

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
      "backend": "codex"
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

A profile with `"backend": "codex"` runs `codex exec --json`. It uses your existing Codex authentication, provider, and configuration, including `CODEX_HOME`. The optional `model` field or `-m` selects a model; omit both to use the Codex default. Gateway fields (`base_url`, `token`, `token_cmd`) apply only to Claude profiles. `OFFLOAD_CODEX` can override the Codex executable.

Codex runs with approval policy `never`, using the `read-only` sandbox by default and `workspace-write` with `-w`. These are Codex sandbox policies, separate from the Claude tool approval lists below. Instructions and the task are sent over stdin. Codex also discovers its own agent instructions. JSONL events supply the report, session ID, and live tool progress; `turns` counts tool calls. Codex does not report a USD cost, so offload shows `unknown` (`null` in JSON).

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

## Set up Codex and Claude Code

Install the bundled skill and proactive instructions into each supervising agent's main instruction file:

```bash
offload install-agent                         # both agents, user scope
offload install-agent -agent codex            # Codex only
offload install-agent -agent claude           # Claude Code only
offload install-agent -scope project -d /absolute/path/app
```

`-agent` accepts `codex`, `claude`, or `all` (default). `-scope` accepts `user` (default) or `project`. Project scope defaults to the current directory; `-d` is only valid with project scope. This installs instructions and skills; configure gateway credentials separately above.

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

The installed policy asks main agents and native subagents to choose offloading before loading large outputs. Call the CLI directly when another coordination layer adds no value. For parallel work or independent review, a native Codex subagent can launch the external worker:

```text
Codex main agent → native Codex subagent → offload → Claude CLI / MiniMax
                 ← verified conclusion + worker metrics ← final report
```

This uses an external worker process; it does not change Codex's native subagent model provider. Give a suitable subagent this instruction:

> Use offload for the bulk reading in /absolute/path/app, read-only. Find retry entry points and tests; return at most 10 file:line rows. Verify two key references and report the model, session, estimated worker cost, and elapsed time.

Keep short conversations, trivial lookups, architecture choices, sensitive changes, and final review with the supervisor. Workers already launched by offload should finish their assigned task directly instead of delegating again. Spot-check reports; after authorized `-w` work, inspect the diff and independently run relevant checks.

## Usage

```text
offload [flags] "task"        run a task (task may also come on stdin)
offload ls [n]                list the last n runs (default 10)
offload show <session>        print every report of a session
offload status                what each worker is doing right now
offload watch                 one line per event (started, progress, STALLED, finished) until all workers end
offload profiles              list profiles (* = the one a run would use)
offload default [profile]     show or set the saved default profile
offload install-agent [flags] install skills and proactive agent instructions
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

### Cost and elapsed time

Codex does not provide a USD cost; its `cost_usd` is `null`. For Claude, `cost_usd` comes from the worker's reported estimate; `cost_is_estimate` is `true`. It excludes the parent Codex/Claude agent and any native subagent coordination. Actual provider bills and total savings are unverified. Missing cost is `null` in JSON and `unknown` in text; a reported zero is valid. Old history still loads, with unavailable cost, exit code, or elapsed time shown as `unknown`.

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
