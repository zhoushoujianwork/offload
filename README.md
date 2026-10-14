# offload

Hand token-heavy grunt work from an expensive supervising agent (e.g. Claude Code on Opus) to a cheaper model, and get back **only the final report**.

`offload` runs `claude -p --bare` against any Anthropic-compatible gateway (MiniMax, DeepSeek, Kimi, GLM, a LiteLLM proxy, …). The worker reads files, searches code, runs tests; the supervisor's context receives one concise answer plus a one-line cost summary.

```
$ offload -d ~/src/app "where is retry logic for HTTP calls? list file:line, max 10"
internal/http/client.go:42  doWithRetry — exponential backoff, 3 attempts
...
[offload] ok minimax/MiniMax-M3 turns=6 cost=$0.0310 secs=41 session=5c1e…
```

把读文件、翻日志、代码检索、写样板代码这类耗 token 的活，从贵的主 Agent 卸给便宜模型，主会话只拿回结论。

## Install

Requires [Claude Code](https://docs.anthropic.com/en/docs/claude-code) (`claude` on PATH or at `~/.claude/local/claude`) and Go 1.24+.

```bash
go install github.com/zhoushoujianwork/offload@latest
```

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
| `model` | `ANTHROPIC_MODEL` |

`--bare` makes the worker ignore `~/.claude/settings.json`, so nothing there can override these. Secrets never appear in argv.

### Choosing a profile

Per run `-p name`, per shell `OFFLOAD_PROFILE=name`, otherwise `default` in the config. `offload profiles` shows them, `*` marking the one a run would use (tokens are never printed).

## Usage

```
offload [flags] "task"        run a task (task may also come on stdin)
offload ls [n]                list the last n runs (default 10)
offload show <session>        print every report of a session
offload profiles              list profiles (* = the one a run would use)
offload version

  -p profile   profile name (default: $OFFLOAD_PROFILE, then config default)
  -m model     override the profile's model
  -w           write mode (default: read-only)
  -r session   resume a session; reuses its original dir unless -d is given
  -d dir       working directory (default: cwd)
  -t 15m       timeout
  -budget 0.5  max USD per run ($OFFLOAD_BUDGET)
```

- **stdout**: the worker's final report only. **stderr**: one `[offload]` line with status, cost and session id.
- **Exit codes**: 0 ok, 1 worker error, 2 usage/config error or unreachable gateway, 124 timeout.
- **History**: every run is appended to `~/.local/state/offload/runs.jsonl` (task, dir, session, cost, full report), so a supervisor can follow up later with `-r` or re-read with `show`.

### Permissions

| mode | tools |
|---|---|
| read-only (default) | Read, Grep, Glob, `git log/diff/show`, `ls`, `rg` |
| `-w` | above + Edit/Write inside the working dir (`acceptEdits`), build/test commands (`go`, `make`, `npm test/run`, `pytest`, `cargo`, `idf.py build`) |

No mode allows commit, push, `rm`, or network tools. Review the diff and rerun tests yourself before trusting a `-w` run.

`--bare` skips hooks, plugins and CLAUDE.md auto-discovery, keeping the worker cheap; a `CLAUDE.md` / `AGENTS.md` in the working dir is appended explicitly.

## Use from Claude Code

Install the bundled skill so the supervising agent knows when and how to offload:

```bash
git clone https://github.com/zhoushoujianwork/offload ~/src/offload
ln -s ~/src/offload/skills/offload ~/.claude/skills/offload
```

## Use from Codex

Codex reads the same skill format, and `offload` must run outside its sandbox (it needs network and writes to `~/.claude` and `~/.local/state/offload`):

```bash
ln -s ~/src/offload/skills/offload ~/.codex/skills/offload
echo 'prefix_rule(pattern=["offload"], decision="allow")' >> ~/.codex/rules/default.rules
```

Optionally add a line to `~/.codex/AGENTS.md` telling Codex when to prefer `offload`.

## Known limits

- When launched from inside the Claude desktop app, the nested worker inherits host restrictions that disable file writes; `-w` works from a normal terminal.

## License

MIT
