# amf specification

amf (autonomous mini factory) turns labeled GitHub issues into pull requests by
running Claude Code headless in a git worktree. It is a single Go binary that
uses only the standard library, for one developer or a small team that doesn't
want to run a platform.

**Needs:** `git` and the `claude` CLI on `PATH`, and a GitHub token for one repository.

## Issue lifecycle

```
amf:ready ──▶ amf:in-progress ──▶ amf:done    (PR opened)
                       └────────▶ amf:failed  (reason commented)
```

- `amf:ready` marks an issue for amf. Adding it again retries an issue that is
  done or failed.
- amf removes `amf:ready` when it starts, and an issue has at most one status
  label at a time.
- amf owns these four labels and every `amf/issue-<N>` branch.

## Commands

| Command | Behavior |
|---|---|
| `amf init` | Writes `./.env` and creates the labels (see below). |
| `amf issue "Title" [-b body \| -f file \| -f -] [--label L] [--no-label]` | Creates an issue labeled `amf:ready`. The body can also come from stdin when it's piped. Prints `#N <url>`. |
| `amf list [--state open\|closed\|all] [--label L]` | Lists issues, newest first, with every page fetched and PRs left out. Columns: `ISSUE`, `STATE` (with `--state all` only), `AMF` (the issue's `amf` and `amf:*` labels, or `-`) and `TITLE`. Prints `No … in owner/name.` when nothing matches. Needs no git checkout. |
| `amf run <N> [--timeout 30m]` | Processes issue N once. |
| `amf watch [--interval 60s] [--label amf:ready] [--timeout 30m]` | Polls open issues with the label (every page), oldest first, and processes them one at a time until interrupted. |
| `amf update [--version vX.Y.Z] [--check]` | Replaces the running binary with the latest release, or with the given one, which can be a downgrade. Needs no `.env`. See below. |
| `amf version` | Prints the version: the release tag, or the module version for `go install`. |

**Exit codes:** 0 on success, 1 on error, 2 on a usage error. SIGINT and
SIGTERM cancel the current step; the issue is still marked failed.

### `amf init`

1. Builds `.env` from a template of `GITHUB_TOKEN`, `GITHUB_REPO` and
   `CLAUDE_CODE_OAUTH_TOKEN`.
   - Real values from an existing `.env` are kept, and so are any extra
     variables.
   - A missing `GITHUB_REPO` is filled in from the checkout's GitHub `origin`.
   - Anything else gets a placeholder.
2. If the result matches the current `.env`, nothing is written. Otherwise the
   old file is copied to the next free `.env.NNNN` (0001, 0002, …, numbered
   above the highest that exists), and the new file is written atomically with
   permissions 0600.
3. In a git checkout, adds `/.env` and `/.env.NNNN` to `.git/info/exclude`.
4. Once the token and repo are usable, checks that the repo exists and creates
   any of the four labels that are missing. Existing labels aren't changed.
   Until then it prints that labels weren't checked yet.

## Configuration

amf reads `./.env`, then `.env` at the top of the git checkout. Environment
variables override the file. Values ending in `...`, and `owner/name`, are
placeholders and count as unset.

| Variable | Default | Purpose |
|---|---|---|
| `GITHUB_TOKEN` | required | API access: Contents, Issues, Pull requests (read & write) |
| `GITHUB_REPO` | required | `owner/name`; github.com URLs are accepted |
| `CLAUDE_CODE_OAUTH_TOKEN` | unset | Claude subscription token, passed to Claude |
| `AMF_CLAUDE` | `claude` | Path to the Claude CLI |
| `AMF_API_URL` | `https://api.github.com` | GitHub API base URL |
| `AMF_CLONE_URL` | `https://github.com/<repo>.git` | Where to clone the repo from |

## Processing one issue

1. **Preflight.** Confirm `GITHUB_REPO` exists and `GITHUB_TOKEN` can reach it.
   A 404 means the repo or token is wrong; a 401 means the token was rejected.
2. **Checkout.** Use the enclosing git checkout. Outside one, clone the repo
   into `./<name>` with the user's git credentials, or reuse an existing clone
   there. Refuse if `./<name>` exists but isn't a clone of the repo. Add
   `/.amf/` to `.git/info/exclude`.
3. **Claim.** The issue must be an open issue, not a PR. Label it
   `amf:in-progress` and remove `amf:ready`.
4. **Worktree.** Fetch the default branch and create
   `.amf/worktrees/issue-N` on `amf/issue-N`, reset to `origin/<default>`. A
   worktree left from an earlier run is discarded first.
5. **Agent.** Run, in the worktree:
   `claude -p --dangerously-skip-permissions --output-format stream-json --verbose`
   - **Prompt (on stdin):** the issue's title and body, plus instructions to
     make the smallest complete change, run the project's tests and build, not
     touch git or GitHub, and reply with only a PR description.
   - **Environment:** without `GITHUB_TOKEN`. When `CLAUDE_CODE_OAUTH_TOKEN` is
     set, it is passed and `ANTHROPIC_API_KEY` is removed.
   - **Log:** output streams live to `.amf/logs/issue-N.log`.
   - **Timeout:** Claude is killed after `--timeout`.
   - **Summary:** the final `result` message becomes the summary. A
     `result` with an error fails the run.
6. **Commit.** Stage everything and commit as `Fix #N: <title>` with the body
   `Closes #N`. Fail if the branch has no commits beyond `origin/<default>`.
7. **Push.** `git push --force -u origin amf/issue-N`, with the user's git
   credentials.
8. **PR.** Open `amf/issue-N` → `<default>`, titled `Fix #N: <title>`. The
   body is the summary (or `Resolves #N.` if there isn't one), then `Closes #N`
   and an "Opened by amf" footer.
9. **Finish.** Comment the PR link on the issue, set `amf:done`, and remove the
   worktree. The branch and log are kept.

**On failure** in any step after Claim: set `amf:failed`, comment the error on
the issue, and keep the worktree and log for inspection. This also happens if
the run is interrupted. If Claim itself fails, the issue can be left on
`amf:in-progress`, which `watch` skips; `amf run N` recovers it.

## Updating (`amf update`)

1. Resolve the target: `--version` (a leading `v` is added if missing), or
   the tag that `https://github.com/vicgupta/auto-mini-factory/releases/latest`
   redirects to. If it equals the running version, print `amf <v> is up to
   date` and stop. `--check` stops here too and reports whether a newer
   release is available.
2. Download `amf_<tag>_<os>_<arch>.tar.gz` and `checksums.txt` from that
   release. If the archive is missing, fail with `no amf <tag> for <os>/<arch>`.
   If its SHA-256 doesn't match, fail.
3. Write the archive's `amf` next to the running executable (symlinks
   resolved) as a temporary file with permissions 0755. Run it with
   `version`, and fail unless it prints `amf <tag>`.
4. Rename it over the running executable. A folder that can't be written
   fails with a hint to rerun with permission.

Any failure leaves the installed binary unchanged.

## Files

| Path | Contents |
|---|---|
| `.env`, `.env.NNNN` | Configuration and its backups (permissions 0600, git-excluded) |
| `.amf/worktrees/issue-N/` | Working copy for an issue in progress or failed |
| `.amf/logs/issue-N.log` | Claude's stream-json output for the latest run of issue N |

amf only ever adds to `.git/info/exclude`. It never edits a tracked `.gitignore`.

## Security model

- Claude runs with every permission check off. An issue's body is effectively
  code run on the user's machine, so label only issues you trust.
- Claude never gets `GITHUB_TOKEN`, but it can read local files, including
  `.env`. For real isolation, run amf in a container or VM.
- amf does all git and GitHub writes itself, through the user's credentials
  and the configured token.

## Non-goals

- Running issues in parallel. `watch` processes one at a time.
- Reviewing, merging, or retrying failed PRs. People do that.
- Sandboxing Claude.
