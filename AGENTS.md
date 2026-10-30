# Offload delivery rules

## Complete delivery after each change

- After completing a feature, bug fix, or maintenance task, run the relevant checks, review the diff, commit only the task's changes, and push normally.
- Treat release delivery as part of completion: integrate through the repository's PR/check gates, then publish the next appropriate version from the verified merged commit. Do not stop at "code pushed, release not installed" unless a concrete blocker remains or the user explicitly requests source-only work.
- Include both macOS App archives (arm64 and amd64) in the release. Verify the release workflow, published asset digests, App signatures, and matching App/bundled CLI versions. Never reuse or force-move a published version tag.
- Install the verified latest release locally, updating the App and the normal CLI installation together. Keep a recoverable backup and verify the installed versions. Prefer the App's safe idle restart; never kill active chats to complete an upgrade.
- For documentation-only changes, do not manufacture a binary release solely for the documentation; include the change in an already pending release or report the documentation commit and checks.
- Preserve unrelated work. If the shared checkout has unfinished changes, release from an isolated worktree containing only reviewed, committed changes.

## Shared checkout hygiene

- Keep `/Users/mikas/github/offload` on `main`, fast-forwarded to `origin/main`, with a clean working tree whenever a task ends.
- Local and remote must match when a task ends: no uncommitted changes, no local commits missing from the remote, and no remote commits missing locally. Verify with `git fetch origin --prune`, then `git status -sb` and `git rev-list --left-right --count origin/main...main`, which must print `0	0`.
- Report the result only after that check. Say so explicitly when a change is committed but its PR is not yet merged.
- Do feature and fix work in a separate worktree or branch. When its PR is merged, return the shared checkout to `main` and update it with `git fetch origin --prune && git merge --ff-only origin/main`.
- Before switching branches, run `git status`. Commit or stash unfinished changes first, and never discard work that is not committed or pushed.
- Do not delete branches or worktrees without listing them and getting confirmation.

## Acceptance and reporting

- Use quiet headless checks and offscreen renders by default. Desktop smoke checks still require the user's explicit scoped request or an agreed testing window; release authorization alone does not authorize arbitrary desktop input.
- Verify required remote checks before merging. Do not bypass branch protections or replace failed checks with an unsupported success claim.
- Report the source commit, release link/version, installation result, validation evidence, and any remaining blocker. Distinguish successful packaging, local installation, safe restart, and live UI acceptance.
- When a user requests a release, proceed through these steps without asking again for routine push, merge, tag, or local installation confirmation.
