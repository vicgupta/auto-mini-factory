# amf specification

amf (autonomous mini factory) turns GitHub issues into pull requests with
Claude Code. It is one Go binary with no dependencies outside the standard
library, for a solo developer or small team that doesn't want to run a platform.

## Commands

| Command | Behavior |
|---|---|
| `amf init` | Writes `.env` with every variable amf uses. Backs up an existing `.env` to the next free `.env.NNNN` (only if it changes) and keeps its real values. Once the token and repo are set, creates any missing amf labels. |
| `amf issue "Title" [-b body \| -f file \| stdin] [--no-label]` | Creates an issue labeled `amf:ready`. |
| `amf run <N> [--timeout 30m]` | Processes one issue into a PR. |
| `amf watch [--interval 60s] [--label amf:ready] [--timeout 30m]` | Polls for open `amf:ready` issues not already in progress and runs them one at a time, oldest first. |
| `amf version` | Prints the version. |

## Configuration

amf reads `.env` from the current directory, then from the top of the git
checkout. Values set in the environment win over the file. Values ending in
`...` (the `amf init` placeholders) count as unset.

| Variable | Required | Purpose |
|---|---|---|
| `GITHUB_TOKEN` | yes | GitHub API: Contents, Issues, Pull requests read & write |
| `GITHUB_REPO` | yes | `owner/name` |
| `CLAUDE_CODE_OAUTH_TOKEN` | no | Claude subscription token. When set, `ANTHROPIC_API_KEY` is dropped from Claude's environment. |
| `AMF_CLAUDE` | no | Path to `claude` (default `claude`) |
| `AMF_API_URL` | no | API base URL (default `https://api.github.com`) |
| `AMF_CLONE_URL` | no | Clone URL (default `https://github.com/<repo>.git`) |

## Processing an issue (`amf run N`)

1. Check that `GITHUB_REPO` exists and the token can reach it.
2. Use the enclosing git checkout. Outside one, clone the repo into `./<name>`,
   or reuse that clone.
3. Label the issue `amf:in-progress` and remove `amf:ready`.
4. Fetch the default branch and create the worktree `.amf/worktrees/issue-N`
   on branch `amf/issue-N`, starting from `origin/<default>`.
5. Run `claude -p --dangerously-skip-permissions --output-format stream-json --verbose`
   in the worktree, with the issue as the prompt. Claude is told to change the
   code, run the tests and build, leave git alone, and reply with a PR
   description. Its output streams to `.amf/logs/issue-N.log`.
6. Commit as `Fix #N: <title>`. Fail if nothing changed.
7. Run `git push --force -u origin amf/issue-N` with the user's git credentials.
8. Open a PR whose body is Claude's final message plus `Closes #N`.
9. Comment the PR link on the issue, label it `amf:done`, and remove the worktree.

**On any failure:** label the issue `amf:failed`, comment the reason, and keep
the worktree for inspection. Re-running starts from a fresh worktree.

## Invariants

- amf owns `amf/issue-*` branches and the `amf:ready`, `amf:in-progress`,
  `amf:done` and `amf:failed` labels. An issue moves ready → in-progress →
  done or failed. Adding `amf:ready` again retries it.
- Claude never receives `GITHUB_TOKEN`. amf does all git and GitHub work itself.
- `.amf/`, `.env` and `.env.NNNN` are added to `.git/info/exclude`, never to
  the tracked `.gitignore`.
- Each issue is processed in its own worktree, so the user's checkout is
  never modified.

## Non-goals

- Running issues in parallel. `watch` is sequential.
- Sandboxing. Claude runs with permissions bypassed and can read local files,
  so label only issues you trust.
- Merging, reviewing or retrying PRs. People review and merge them.
- Repos with more than 100 open `amf:ready` issues. `watch` reads only the
  first page.
