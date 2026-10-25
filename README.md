# amf — autonomous mini factory

One stdlib-only Go binary that turns GitHub issues into pull requests with a
coding agent (Claude Code, Codex or opencode, whichever you have), or with a
plan → code → eval pipeline of them. For a solo developer or small team that wants coding agents without
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

On Windows, download `amf_<version>_windows_amd64.zip` (or `_arm64`) from the
[latest release](https://github.com/vicgupta/auto-mini-factory/releases/latest),
unzip it, and put `amf.exe` in a folder on your `PATH`. After that,
`amf update` keeps it current. The Windows build is cross-compiled and its
self-update is unit-tested, but it hasn't been run on a Windows machine yet.

Or with Go 1.27 or newer:

```sh
go install github.com/vicgupta/auto-mini-factory/cmd/amf@latest
# or, from a clone: go build -o amf ./cmd/amf
```

Needs `git` and at least one coding agent on `PATH`:
[`claude`](https://claude.com/claude-code), [`codex`](https://github.com/openai/codex)
or [`opencode`](https://opencode.ai). See [Choosing the agent](#choosing-the-agent)
for which one amf runs.

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
ANTHROPIC_API_KEY=sk-ant-api03-...   # optional: Claude API key, used if there's no token above
OPENAI_API_KEY=sk-proj-...   # optional: OpenAI API key for codex
# OPENCODE_API_KEY=oc_sk_...   # optional: OpenCode Console API key, for pay-as-you-go and Go models
# OPENCODE_MODEL=opencode/big-pickle   # opencode/<id> for free and pay-as-you-go models, opencode-go/<id> for Go; 'opencode models' lists them
# AMF_AGENT_TIMEOUT=60m   # optional: how long each agent run may take (--agent-timeout overrides it)
# AMF_LOG_LEVEL=info   # optional: debug, info, warn or error, for the terminal and the event log in .amf/runs/
AMF_TRUSTED=login1,login2...   # optional: only work on issues opened by these GitHub users
# AMF_POSTGRES_URL=postgres://user:password@localhost:5432/postgres   # optional: also save the logs to database amflogs on this PostgreSQL server
```

Edit it and replace the placeholders. Placeholders ending in `...` count as
unset, so you can leave the optional ones as they are. The agent keys decide
which agent amf runs (see below). To run opencode with another model,
uncomment `OPENCODE_MODEL` and set it to one that `opencode models` lists.
Free models (`opencode/big-pickle` and the `opencode/...-free` ones) need no
key. For a pay-as-you-go `opencode/<id>` model, or an
[OpenCode Go](https://opencode.ai/v2/docs/console/go/) `opencode-go/<id>`
model, also uncomment `OPENCODE_API_KEY` and set it to an OpenCode Console
API key; one key covers both, and the model's prefix picks the plan. amf
passes the key only to opencode, through its environment.
To keep the logs in PostgreSQL too, uncomment `AMF_POSTGRES_URL` (see
[Logs in PostgreSQL](#logs-in-postgresql)).

- **Backups:** if `.env` already exists, `amf init` first copies it to the next
  free `.env.0001`, `.env.0002`, and so on, then keeps its real values in the
  new file, including any extra variables.
- **Repo:** `GITHUB_REPO` is filled in from the checkout's GitHub `origin`.
- **Git:** `.env` and `.env.NNNN` are added to `.git/info/exclude` (for the
  folder you ran `amf init` in, even a subdirectory), so the
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

### Check your setup

`amf doctor` checks everything amf needs, one line each, without changing
anything:

```
$ amf doctor
ok    amf             v0.9.0, the latest release
ok    git             git version 2.56.0
ok    .env            /tmp/amf/.env
ok    GITHUB_TOKEN    set, from /tmp/amf/.env
ok    GITHUB_REPO     vicgupta/factory-demo-app, from /tmp/amf/.env
ok    github          token accepted for @vicgupta at https://api.github.com
ok    repo            vicgupta/factory-demo-app, default branch main
ok    labels          all 4 amf labels exist
ok    trusted         only issues opened by @vicgupta
ok    agent timeout   1h0m0s, the default (AMF_AGENT_TIMEOUT or --agent-timeout changes it)
ok    checkout        /tmp/amf/factory-demo-app, the clone run and watch use here
ok    git push        your git credentials can push to origin (checked with --dry-run)
ok    git identity    commits as Vic Gupta <vicgupta@gmail.com>
ok    log folder      /tmp/amf/factory-demo-app/.amf/runs is writable
ok    pipeline        none in AGENTS.md on origin/main; amf runs claude once per issue
ok    claudecode      /Users/vic/.local/bin/claude, 2.1.309 (Claude Code); runs issues without a pipeline
warn  codex           codex is not installed; amf runs claude instead, unless --agent or a pipeline names codex
ok    opencode        /Users/vic/.opencode/bin/opencode, opencode v2.1.3
ok    claude login    CLAUDE_CODE_OAUTH_TOKEN is set, from /tmp/amf/.env; claude runs with it
ok    codex login     OPENAI_API_KEY is not set; codex uses its own login, and amf runs it only for a pipeline or --agent codex
ok    opencode auth   OPENCODE_API_KEY, from /tmp/amf/.env, grants pay-as-you-go and Go; opencode runs opencode-go/kimi-k3 with it
ok    opencode model  opencode-go/kimi-k3; opencode lists it
ok    postgres        logging to database amflogs at amf@db.example.com:5432, from /tmp/amf/.env
```

Each line is `ok`, `warn` (works, but worth knowing), `FAIL`, or `skip` (an
earlier check it needs failed). Any `FAIL` makes it exit 1. With a pipeline,
it also downloads each skill, and an agent the pipeline uses but that isn't
installed is a `FAIL`.

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
amf list --limit 5          # just the 5 newest
amf list --json --label amf:failed | jq -r '.[].url'  # as JSON: number, title, state, url, labels

amf run 42                  # process one issue now
amf watch --interval 60s    # process issues labeled `amf:ready`, one at a time
amf watch --agent codex     # ...with Codex, even if Claude Code is installed
amf watch --exclude-label blocked --exclude-label needs-human  # ...leaving issues with either label alone until it's removed
amf run 42 --draft          # open the PR as a draft
amf watch --draft           # ...opening each new PR as a draft
amf run 42 --merge          # ...and merge its PR with a merge commit
amf watch --merge=squash    # squash and merge each PR (or --merge=rebase)
```

### Choosing the agent

Without a pipeline, each issue gets one agent run. Unless you name one, amf
uses the first of these that's ready:

| Agent | Ready when |
|---|---|
| `claude` | installed, and `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` is set |
| `codex` | installed, and `OPENAI_API_KEY` is set |
| `opencode` | installed; runs with `OPENCODE_MODEL` (default `opencode/big-pickle`). A paid `opencode/` or Go `opencode-go/` model also needs `OPENCODE_API_KEY` or a saved `opencode auth login` |

Keys can be in `.env` or the environment. An installed claude or codex
without its key is skipped, even if you're logged in to it. `amf watch` prints what it found
for each agent when it starts:

```
amf: agents for issues without a pipeline (claude and codex need a key, opencode one for a paid model):
amf:   claude   installed at /Users/vic/.local/bin/claude; CLAUDE_CODE_OAUTH_TOKEN is set: usable
amf:   codex    installed at /Users/vic/.local/bin/codex; OPENAI_API_KEY not set: skipped
amf:   opencode installed at /Users/vic/.opencode/bin/opencode; needs no key, model opencode/big-pickle: usable
amf: issues without a pipeline run claude
```

To pick one yourself, pass `--agent claudecode` (or `claude`), `--agent codex`
or `--agent opencode` to `amf run` or `amf watch`. A named agent must be
installed but needs no key, so `--agent claudecode` runs Claude on its own
login. amf never quietly falls back from a named agent to another. If no agent
is ready, amf leaves the issue on `amf:ready` and `watch` tries again on its
next poll. `amf doctor` shows which agent amf would use, and warns if
`OPENCODE_MODEL` isn't one opencode lists. With `OPENCODE_API_KEY` set, it
also asks OpenCode which plans the key grants (pay-as-you-go, Go) and fails
if the key is rejected or an `opencode-go/` model's workspace isn't
subscribed to Go. A repo with a pipeline ignores
`--agent`, because the pipeline names each stage's agent.

### What `amf run N` does

Without a pipeline (see below), amf runs one agent once. With Claude Code:

1. Labels the issue `amf:in-progress` and removes `amf:ready`.
2. Fetches the default branch and creates a worktree at
   `.amf/worktrees/issue-N` on branch `amf/issue-N` from `origin/<default>`.
3. Runs `claude -p --dangerously-skip-permissions --output-format stream-json --verbose` in the
   worktree with the issue as the prompt. Claude is told to make the change,
   run the project's tests/build, and not touch git. Output streams to
   `.amf/logs/issue-N.log` as it works, one JSON message per line
   (`tail -f` it to follow along). Each agent run may take at most 60 minutes
   (`--agent-timeout`; amf then kills the agent and everything it spawned, or
   `AMF_AGENT_TIMEOUT` in `.env` sets it without a flag).
   Codex and opencode run the same way, with the commands in
   [docs/SPECS.md](docs/SPECS.md#agents).
4. Commits the changes (`Fix #N: <title>`), fails if there are none, and runs
   `git push --force -u origin amf/issue-N` with your own git credentials.
   If the change touches anything under `.github/`, amf refuses to push it
   and the issue fails, since a pushed workflow change runs with your
   Actions secrets before anyone reviews it.
5. Opens a PR with the agent's final message as the body plus `Closes #N`, as
   a draft with `--draft` (default off; `amf run` and `amf watch` take it). On
   a retry whose PR is still open, it updates that PR's title and body
   instead, and leaves it a draft or ready for review as it was.
6. Comments the PR link on the issue, labels it `amf:done` (a draft PR
   counts), removes the worktree.

With `--merge`, amf also merges the PR before commenting, with a merge commit
(or `--merge=squash`, `--merge=rebase`), pinned to the commit it pushed. It
doesn't wait for checks, so with required checks or reviews GitHub refuses
and the PR stays open; amf comments why, and `amf run` exits 1. amf never
merges a change to the top-level `AGENTS.md`, which holds the pipeline.
`--merge` can't be combined with `--draft`, since GitHub doesn't merge drafts.

On any failure the issue gets `amf:failed` plus a comment with the reason,
and the worktree is kept for inspection. A run that hit the agent timeout
loses only the worktree: amf comments with how to retry with more time and
keeps the logs and partial output. Re-running `amf run N` starts again from a
fresh worktree.

### Logs

Besides the `amf: …` lines on the terminal and the agents' logs in
`.amf/logs/`, each `amf run` and `amf watch` writes an event log to
`.amf/runs/<run_id>/events.jsonl`: one JSON object per line for every step,
GitHub call, git command and agent run, with the issue it was for.

```sh
jq -c 'select(.issue == 12)' .amf/runs/*/events.jsonl   # everything about #12
jq -c 'select(.event == "github.call") | [.method, .path, .status]' .amf/runs/*/events.jsonl
```

After each issue, and when amf exits (Ctrl-C included), it writes
`summary.json` next to the log: issues done, failed and skipped, GitHub and
git calls, agent time per stage and errors. It prints the same as the last
line, `amf: summary: 3 issues (2 done, 1 failed, 0 skipped); 41 GitHub calls; log …`.

- `--log-level debug|info|warn|error` (or `AMF_LOG_LEVEL`) sets what the
  terminal and the log show. By default the terminal shows `info` and the
  log keeps `debug`.
- `--log-file PATH` writes the log somewhere else, and `--log-file off`
  writes none.
- Everything amf writes is **redacted**: tokens and keys from `.env`,
  well-known secret formats, credentials in URLs, emails and your home
  directory are masked as `[REDACTED:<kind>]` on the terminal, in every log,
  in PostgreSQL and in the comments and PRs amf posts. It's pattern
  matching, so a secret in an unusual format can still get through; see
  [docs/SPECS.md](docs/SPECS.md#security-model).

`amf doctor` checks that `.amf/runs/` is writable.

### Logs in PostgreSQL

Set `AMF_POSTGRES_URL` in `.env` and `amf run` and `amf watch` also write
every log line to PostgreSQL, while still printing it and writing
`.amf/logs/` as usual:

```sh
AMF_POSTGRES_URL=postgres://amf:secret@db.example.com:5432/postgres?sslmode=require
```

amf logs to a database named `amflogs` on that server. If it doesn't exist,
amf creates it (the user needs `CREATEDB` that first time), along with two
tables:

- `amf_log`: amf's own lines, such as `#12: running claude`, with the issue
  number when the line is about one.
- `agent_log`: every line the agents print, with the issue, the agent and
  the log file it also went to.

```sql
SELECT logged_at, message FROM amf_log WHERE issue = 12 ORDER BY id;
```

Logging never holds up an issue. If the server is down, amf warns, keeps
the lines in memory and retries. amf has no dependencies, so it speaks the
PostgreSQL protocol itself: TLS (`sslmode`), and password, MD5 or
SCRAM-SHA-256 logins. `amf doctor` checks the connection. Agents' output can
include your code, so restrict who can read `amflogs`.

### Pipelines: plan → code → eval

Instead of one agent run, a repo can have amf plan the change, code it, and
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
- **code** follows the plan, then amf commits the round. A round that
  changes anything under `.github/` fails the issue: amf refuses to push
  such changes, as it does without a pipeline.
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

Agents run with all permission checks off, so an issue body is effectively
code you're executing on your machine.

- Only label issues you trust. On a public repo only people with triage
  access can add labels, but anyone can write an issue body, and its author
  can edit it after you label it.
- Set `AMF_TRUSTED=you,teammate` to have amf work only on issues those
  GitHub users opened. Anyone else's issue is marked `amf:failed` with a
  comment saying why, and no agent runs. Unset, amf works on anyone's issue,
  and `amf doctor` warns about it.
- `GITHUB_TOKEN` is stripped from every agent's environment, but agents can
  still read files on disk, including `.env` at the top of your checkout. For full
  isolation, run amf in a container or VM.
- A pipeline's skills are instructions run the same way. Use skills you
  control or have reviewed, at a tag, ideally pinned with `#sha256=`.
- `--merge` puts agent-written code on your default branch with no review.
  Use it only with issues and agents you trust, and keep branch protection's
  required checks if you want them to gate the merge.

## Spec

See [docs/SPECS.md](docs/SPECS.md). For a step-by-step walk through how amf
processes an issue, see [docs/AMF-PROCESS.md](docs/AMF-PROCESS.md).

## Develop

```sh
go test -race ./...
```

CI runs `gofmt`, `go vet` and these tests on Linux and macOS for every push
and PR, and builds every release platform.

Tests use `httptest` for GitHub, real git repos in temp dirs, and shell
scripts standing in for `claude`, `codex` and `opencode`.

The live tests run the real binary and agents against a real repository. They
create issues, branches and PRs there and close or delete them afterwards:

```sh
AMF_LIVE_DIR=/path/with/.env go test -tags live -run TestLive -v -timeout 90m ./cmd/amf
```

`AMF_LIVE_DIR` holds a `.env` for a test repository and is where amf runs.
`AMF_LIVE_PIPELINE=1` also tests pipelines, which commits an `AGENTS.md` to
that repository's default branch and removes it afterwards. See
`cmd/amf/live_test.go` for the other settings.

## License

MIT; see [LICENSE](LICENSE).
