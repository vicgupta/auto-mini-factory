# amf — autonomous mini factory

One stdlib-only Go binary that turns GitHub issues into pull requests with
Claude Code, or with a plan → code → eval pipeline of Claude Code, Codex and
opencode. For a solo developer or small team that wants coding agents without
running a platform.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/vicgupta/auto-mini-factory/main/install.sh | sh
```

This downloads the latest release binary for macOS or Linux (amd64/arm64),
checks it against the release's `checksums.txt`, and installs it to
`/usr/local/bin` if that's writable, otherwise `~/.local/bin`. To choose,
set `AMF_VERSION=v0.0.2` or `AMF_INSTALL_DIR=...` on the `sh` side of the
pipe, e.g. `... | AMF_INSTALL_DIR=~/bin sh`.

Or with Go:

```sh
go install github.com/vicgupta/auto-mini-factory/cmd/amf@latest
# or, from a clone: go build -o amf ./cmd/amf
```

Needs `git` and the [`claude`](https://claude.com/claude-code) CLI on `PATH`.

To update later:

```sh
amf update            # replace this amf with the latest release
amf update --check    # only say whether there is a newer one
amf update --version v0.0.7   # install a specific release (also downgrades)
```

`amf update` downloads the same release archives as `install.sh`, checks them
against `checksums.txt`, makes sure the new binary runs, then swaps it in place
of the running one. It needs write access to the folder `amf` is in.

## Configure

In a clone of the repo you want amf to work on, run:

```sh
amf init
```

This writes `.env` with every variable amf uses:

```sh
GITHUB_TOKEN=github_pat_...   # Contents, Issues, Pull requests: read & write
GITHUB_REPO=owner/name
CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-...   # optional: Claude subscription token from `claude setup-token`
```

Edit it and replace the placeholders. Placeholders ending in `...` count as
unset, so you can leave the optional `CLAUDE_CODE_OAUTH_TOKEN` as it is.

- **Backups:** if `.env` already exists, `amf init` first copies it to the next
  free `.env.0001`, `.env.0002`, and so on, then keeps its real values in the
  new file, including any extra variables.
- **Repo:** `GITHUB_REPO` is filled in from the checkout's GitHub `origin`.
- **Git:** `.env` and `.env.NNNN` are added to `.git/info/exclude`, so the
  tokens never show up in `git status`.
- **Labels:** once `GITHUB_TOKEN` and `GITHUB_REPO` are filled in, `amf init`
  makes sure the repo has `amf:ready`, `amf:in-progress`, `amf:done` and
  `amf:failed`, creating any that are missing. Run it again after editing
  `.env`. If nothing changed, it makes no new backup.

amf reads `.env` in the current directory, then at the top of the enclosing
git checkout. Real environment variables win over the file.

`CLAUDE_CODE_OAUTH_TOKEN` lets Claude run on your Pro/Max subscription, which
helps on servers or in CI where you can't log in interactively. Generate one with
`claude setup-token`. When it's set, amf passes it to Claude and drops any
`ANTHROPIC_API_KEY` from Claude's environment so the subscription is used
instead of API billing. When it's not set, Claude uses whatever login it
already has.

Keep `.env` out of git: add it to the target repo's `.gitignore`.

## Use

Run `run` and `watch` inside a clone of `GITHUB_REPO`, or from any folder that
holds your `.env`. There, amf clones the repo into `./<name>` the first time
(using your git credentials) and reuses that clone afterwards. amf checks
`GITHUB_REPO` against GitHub first, so a typo fails with a clear message.

```sh
amf issue "Add dark mode" -b "Use CSS variables for the palette"
amf issue "Fix login bug" -f bug.md
echo "details" | amf issue "Title"     # body from stdin
amf issue "Just a note" --no-label     # won't be picked up by watch

amf list                    # open issues, newest first, with their amf labels
amf list --state all        # include closed issues
amf list --label amf:failed # just the ones that failed

amf run 42                  # process one issue now
amf watch --interval 60s    # process issues labeled `amf:ready`, one at a time
```

### What `amf run N` does

Without a pipeline (see below), amf runs Claude once:

1. Labels the issue `amf:in-progress` and removes `amf:ready`.
2. Fetches the default branch and creates a worktree at
   `.amf/worktrees/issue-N` on branch `amf/issue-N` from `origin/<default>`.
3. Runs `claude -p --dangerously-skip-permissions --output-format stream-json --verbose` in the
   worktree with the issue as the prompt. Claude is told to make the change,
   run the project's tests/build, and not touch git. Output streams to
   `.amf/logs/issue-N.log` as it works, one JSON message per line
   (`tail -f` it to follow along). Default timeout is 30 minutes (`--timeout`).
4. Commits the changes (`Fix #N: <title>`), fails if there are none, and runs
   `git push --force -u origin amf/issue-N` with your own git credentials.
5. Opens a PR with Claude's final message as the body plus `Closes #N`.
6. Comments the PR link on the issue, labels it `amf:done`, removes the worktree.

On any failure the issue gets `amf:failed` plus a comment with the reason,
and the worktree is kept for inspection. Re-running `amf run N` starts again
from a fresh worktree.

### Pipelines: plan → code → eval

Instead of one Claude run, a repo can have amf plan the change, code it, and
have a reviewer agent check it before any PR is opened. Each stage gets its
own skill, agent (`claudecode`, `codex` or `opencode`) and model. Declare the
pipeline in the target repo's `AGENTS.md`:

```markdown
## amf

| Stage | Skill | Agent | Model |
|---|---|---|---|
| plan | https://raw.githubusercontent.com/vicgupta/auto-mini-factory/v0.2.0/skills/plan/SKILL.md | claudecode | claude-opus-5-5 |
| code | https://raw.githubusercontent.com/vicgupta/auto-mini-factory/v0.2.0/skills/code/SKILL.md | codex | default |
| eval | https://raw.githubusercontent.com/vicgupta/auto-mini-factory/v0.2.0/skills/eval/SKILL.md | opencode | opencode/deepseek-v4.1-flash-free |
```

- **plan** writes a plan with acceptance criteria. It can stop with
  `BLOCKED:` and questions when the issue is too unclear, and amf comments
  them on the issue.
- **code** follows the plan, then amf commits the round.
- **eval** runs the tests, reviews the diff against the plan, and ends with
  `VERDICT: PASS` or `VERDICT: FAIL`. On a fail, its review goes back to
  code, up to `--eval-rounds` times (default 2). If it still fails, amf
  pushes the branch for you to look at and marks the issue `amf:failed`.
- The PR gets one `Fix #N` commit, with eval's verdict and review and the
  plan in collapsed sections of the body.

Only `code` is required. A skill URL can be pinned with `#sha256=…`, and
amf refuses the skill if it changes. Plan and eval can't change the branch,
because amf resets the worktree after them. Each stage logs to
`.amf/logs/issue-N-<stage>.log`.

`amf run` and `amf watch` read the pipeline from `AGENTS.md` on the default
branch, so a change takes effect once merged. To check a change before
merging, run `amf pipeline --fetch` in your checkout: it shows the stages,
checks the agents are installed, and downloads each skill.

Starter skills are in [`skills/`](skills): [plan](skills/plan/SKILL.md),
[code](skills/code/SKILL.md) and [eval](skills/eval/SKILL.md). Write your own
by copying one; amf's prompt already fixes each stage's inputs and output
format, so a skill only needs to say how to do the work.

### Labels

An issue moves `amf:ready` → `amf:in-progress` → `amf:done` or `amf:failed`.
`amf issue` adds `amf:ready`, and `amf watch` picks up open `amf:ready` issues
oldest first. To retry an issue, add `amf:ready` again or run `amf run N`.
If amf is killed mid-run and an issue is stuck on `amf:in-progress`, run
`amf run N`.

Issues labeled `amf` by amf v0.0.4 and earlier aren't picked up any more.
Relabel them `amf:ready`, or run `amf watch --label amf`.

`.amf/` is added to `.git/info/exclude`, so it never shows up in `git status`.

## Security

Claude runs with all permission checks off, so an issue body is effectively
code you're executing on your machine.

- Only label issues you trust. On a public repo only people with triage
  access can add labels, but anyone can write an issue body.
- `GITHUB_TOKEN` is stripped from Claude's environment, but Claude can still
  read files on disk, including `.env` at the top of your checkout. For full
  isolation, run amf in a container or VM.
- A pipeline's skills are instructions run the same way. Use skills you
  control or have reviewed, at a tag, ideally pinned with `#sha256=`.

## Spec

See [SPECS.md](SPECS.md).

## Develop

```sh
go test -race ./...
```

Tests use `httptest` for GitHub, real git repos in temp dirs, and a shell
script standing in for `claude`.
