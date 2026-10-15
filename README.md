# offload

Hand token-heavy grunt work from a supervising agent, such as Codex or Claude Code, to a cheaper model. Get back the final report, estimated worker cost, and measured elapsed time.

`offload` runs `claude -p --bare` against an Anthropic-compatible gateway (MiniMax, DeepSeek, Kimi, GLM, a LiteLLM proxy, …). Use it for reading many files, code searches, log analysis, document summaries, and bounded mechanical edits. The supervisor plans the work and verifies the result.

```text
$ offload -d /absolute/path/app -t 90s -budget 0.15 "Find HTTP retry logic; return file:line references, max 10 rows. Do not edit files."
internal/http/client.go:42  doWithRetry — exponential backoff, 3 attempts
...
[offload] ok exit_code=0 minimax/MiniMax-M3 turns=6 estimated_cost=$0.0753 elapsed=20.104s session=07bcaf2c-…
```

把读文件、翻日志、代码检索、机械性修改这类耗 token 的工作交给便宜模型，主 Agent 保留规划、核验和最终判断。

[![offload demo video](https://github.com/zhoushoujianwork/offload/releases/download/demo-video/offload-demo-poster.png)](https://github.com/zhoushoujianwork/offload/releases/download/demo-video/offload-demo.mp4)

<sub>63 秒演示视频（点击封面播放或下载）。示例输出摘自本文档，成本为 worker 估算值，不是实际账单。</sub>

## Install

Requires [Claude Code](https://docs.anthropic.com/en/docs/claude-code) (`claude` on PATH or at `~/.claude/local/claude`) and Go 1.24+.

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

A profile is just the three variables Claude Code reads. For each run, offload drops every `ANTHROPIC_*` variable from the caller's environment and sets exactly:

| profile field | worker env |
|---|---|
| `base_url` | `ANTHROPIC_BASE_URL` |
| `token` / `token_cmd` | `ANTHROPIC_AUTH_TOKEN` (sent as `Authorization: Bearer`) |
| `model` (or `-m` override) | `ANTHROPIC_MODEL` |

`--setting-sources ""` disables user, project, and local settings so their environment overrides cannot replace the profile's gateway, token, or model. `--bare` alone does not isolate settings. Authentication is environment-only; no `apiKeyHelper` or `--settings` argument is used. Tokens never appear in argv.

### Choosing a profile

Choose per run with `-p name`, per shell with `OFFLOAD_PROFILE=name`, otherwise through `default` in the config. `offload profiles` lists profiles, with `*` marking the selected default; it never prints tokens.

```bash
offload profiles
offload -p minimax -d /absolute/path/app "Summarize README.md, max 5 bullets."
OFFLOAD_PROFILE=deepseek offload -d /absolute/path/app "Find retry entry points, max 10 file:line rows."
```

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

> Use offload for the bulk reading in /absolute/path/app, read-only, with a 90s timeout and $0.15 budget. Find retry entry points and tests; return at most 10 file:line rows. Verify two key references and report the model, session, estimated worker cost, and elapsed time.

Keep short conversations, trivial lookups, architecture choices, sensitive changes, and final review with the supervisor. Workers already launched by offload should finish their assigned task directly instead of delegating again. Spot-check reports; after authorized `-w` work, inspect the diff and independently run relevant checks.

## Usage

```text
offload [flags] "task"        run a task (task may also come on stdin)
offload ls [n]                list the last n runs (default 10)
offload show <session>        print every report of a session
offload profiles              list profiles (* = the one a run would use)
offload install-agent [flags] install skills and proactive agent instructions
offload version

  -p profile   profile name (default: $OFFLOAD_PROFILE, then config default)
  -m model     override the profile's model
  -w           write mode (default: read-only)
  -r session   resume a session; reuses its original dir unless -d is given
  -d dir       working directory (default: cwd)
  -t 15m       worker timeout
  -budget 0.5  max USD per run ($OFFLOAD_BUDGET), passed to Claude CLI
  -json        emit one JSON run record instead of report-only stdout
```

- **stdout**: the final report by default, or one record with `-json`. **stderr**: one summary line with status, exit code, profile/model, turns, estimated cost, elapsed time, and session.
- **Exit codes**: 0 ok, 1 worker error, 2 usage/config error or unreachable gateway, 124 timeout.
- **History**: runs, including failures, are appended to `$XDG_STATE_HOME/offload/runs.jsonl` (default `~/.local/state/offload/runs.jsonl`). Use `ls` for per-run metrics, `show` for reports and metrics, or `-r` to resume a session.

### Cost and elapsed time

`cost_usd` comes from the Claude worker's reported estimate; `cost_is_estimate` is `true`. It excludes the parent Codex/Claude agent and any native subagent coordination. Actual provider bills and total savings are unverified. Missing cost is `null` in JSON and `unknown` in text; a reported zero is valid. Old history still loads, with unavailable cost, exit code, or elapsed time shown as `unknown`.

`elapsed_ms` measures wall-clock time from CLI entry, including setup and credential retrieval. `duration_ms` preserves the worker-reported duration. The `-t` timeout begins after prompt setup, credential/executable resolution, and the reachability preflight, so elapsed time can exceed it.

```bash
offload -json -d /absolute/path/app -t 90s -budget 0.15 "Read README.md and summarize setup, max 5 bullets. Do not edit files."
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

Failures can also include an `error` field. Worker error types and messages are retained, including `error_max_budget_usd: Reached maximum budget (...)`, and printed when the worker returns no report. For each external worker, supervisors should report status/exit code, profile/model, turns, estimated cost, elapsed time, and session ID.

### Permissions

| Mode | Automatically approved tools |
|---|---|
| read-only (default) | Read, Grep, Glob, `git log/diff/show`, `ls`, `rg` |
| `-w` | Read, Grep, Glob, Edit/Write (`acceptEdits`), `git status/diff`, `ls`, `rg`, build/test commands (`go`, `make`, `npm test/run`, `pytest`, `cargo`, `idf.py build`) |

These Claude tool approval rules are not an operating-system sandbox or a guarantee that build scripts cannot change files, access other paths, or use the network. Set explicit task constraints, inspect scripts and edits, and honor the host's permissions. Commit, push, deletion, and network tools are not included in the approval list.

`--bare` skips hooks, plugins, and automatic CLAUDE.md discovery. Project `CLAUDE.md` / `AGENTS.md` instructions and worker constraints are combined into one private temporary prompt file, passed with `--append-system-prompt-file` and removed after the run. Their contents stay out of argv.

### Codex host permissions and reachability

Install the Codex instructions with `offload install-agent -agent codex`. The CLI needs network access and writes state outside the project (`~/.claude` and the offload history directory). Follow the host's permission policy; use approved escalation or an allowed environment only when available and needed.

Before launching Claude, a reachability preflight attempts a TCP connection to the configured gateway with a 5s timeout. If unreachable, offload exits 2 with `cannot reach gateway`, rather than leaving the worker to retry until its task timeout. A configured HTTP/SOCKS proxy skips this direct check because the proxy determines reachability. This check does not validate credentials or model access.

## Known limits

- Requires network access and writes state outside the project (`~/.claude` and the offload history directory). Use host-approved permissions; when offloading is unavailable, report the failure and continue with local tools or native subagents.
- Some gateways reject Claude Code's `anthropic-beta: claude-code-*` header and cannot be used as workers.
- Nested workers inherit host restrictions. For example, the Claude desktop app may prevent file writes; verify the host supports the requested mode.

## License

MIT
