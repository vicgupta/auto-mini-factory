# amf — autonomous mini factory

One pure-Go binary (no cgo) that turns GitHub issues into pull requests with a
coding agent (Claude Code, Codex or opencode, whichever you have), or with a
plan → code → eval pipeline of them. For a solo developer or small team that wants coding agents without
running a platform.

## Table of contents

- [Commands](#commands)
- [Quick start](#quick-start)
- [Install](#install)
- [Configure](#configure)
  - [Configuration (.env)](#configuration-env)
  - [Authentication](#authentication)
  - [Check your setup](#check-your-setup)
- [Use](#use)
- [Security](#security)
- [Spec](#spec)
- [Develop](#develop)
- [License](#license)

## Commands

| Command | What it does |
|---|---|
| `amf init` | write `.env` and create amf's labels |
| `amf auth` | get a GitHub token: login, status, logout, token |
| `amf issue` | manage issues: `create`, `list`, `label`, `run`, `watch` |
| `amf pr` | manage pull requests: `list`, `merge`, `sweep`, `view`, `diff`, `close`, `reopen`, `ready`, `draft` |
| `amf run` | process one issue into a PR (alias of `issue run`) |
| `amf watch` | process `amf:ready` issues as they arrive (alias of `issue watch`) |
| `amf auto` | plan one sentence into up to 5 issues and merge their PRs, pausing after the first by default |
| `amf memory` | the repo's memory of issues, PRs and runs: sync, similar, show |
| `amf pipeline` | check the plan/code/eval pipeline in this checkout's `AGENTS.md` |
| `amf doctor` | check the setup here, changing nothing |
| `amf update` | replace this amf binary with the latest (or given) release |
| `amf version` | print amf's version |
| `amf help` | show help for a command or topic |

`amf help <command>` or `amf <command> --help` (`-h`) prints a command's details, and
[docs/COMMANDS.md](docs/COMMANDS.md) has the full reference. Every flag has a long
`--full-word` form, used in all examples here; common ones also have a one-letter
short form, such as `-a` for `--agent` (see
[Flag conventions](docs/COMMANDS.md#flag-conventions) or `amf help flags`).
For common failures and fixes, see the
[troubleshooting guide](docs/TROUBLESHOOTING.md).

## Quick start

Have `git` and at least one [coding agent](#install) on `PATH`, with a git
identity and credentials that can push to your repo. See
[Choosing the agent](docs/COMMANDS.md#choosing-the-agent) for key and login requirements.

```sh
curl -fsSL https://raw.githubusercontent.com/vicgupta/auto-mini-factory/main/install.sh | sh
cd your-repo             # a clone of the GitHub repo you want to work on
amf init                 # writes .env; fills GITHUB_REPO from origin
# Edit .env: set GITHUB_TOKEN (or use gh / amf auth login), check GITHUB_REPO and configure your agent.
amf init                 # creates any missing amf labels once configured
amf doctor               # check the setup
amf issue "Add dark mode" --body "Use CSS variables for the palette"
amf run 42               # replace 42 with the issue number in the URL printed above
```

Or run `amf watch` to process open issues labeled `amf:ready`, oldest first.

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
or [`opencode`](https://opencode.ai). See [Choosing the agent](docs/COMMANDS.md#choosing-the-agent)
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

In a clone of the repo you want amf to work on, run `amf init`. It writes
`.env` with the common settings (listed in
[Configuration (.env)](#configuration-env)) and creates amf's labels.

- **Backups:** if `.env` changes, `amf init` first copies the old file to
  `.env.NNNN`, numbered above the highest existing backup (starting at
  `.env.0001`), then keeps its real values in the new file, including any
  extra variables.
- **Repo:** `GITHUB_REPO` is filled in from the checkout's GitHub `origin`.
- **Git:** `.env` and `.env.NNNN` are added to `.git/info/exclude` (for the
  folder you ran `amf init` in, even a subdirectory), so the
  tokens never show up in `git status`.
- **Labels:** once `GITHUB_TOKEN` and `GITHUB_REPO` are filled in, `amf init`
  makes sure the repo has `amf:ready`, `amf:in-progress`, `amf:pr-open`,
  `amf:done` and `amf:failed`, creating any that are missing. Run it again after editing
  `.env`. If nothing changed, it makes no new backup.

amf reads `.env` in the current directory (or the file `--env-file` names), then at the top of the enclosing
git checkout. Real environment variables win over the file.
Keep `.env` out of git: add it to the target repo's `.gitignore`.

### Watch reliability

- **One amf per checkout.** `amf watch`, `amf run` and `amf auto` hold
  `.amf/watch.lock` (PID, host, start time) while they work. A second one
  refuses to start and names the holder. A lock whose process is dead on the
  same host, or that hasn't been refreshed for 10 minutes, is taken over.
  The lock is released on exit, Ctrl-C and SIGTERM included.
- **No duplicate PRs.** Before starting an issue, amf checks for an open PR
  from `amf/issue-N` (the issue becomes `amf:pr-open` and is skipped) and for
  an `amf/issue-N` branch on origin (skipped; delete the branch or use
  `amf run N --force` to retry). An `amf:failed` issue relabeled `amf:ready`
  is still retried.
- **`amf:pr-open` vs `amf:done`.** An issue whose PR is open but not merged
  is `amf:pr-open`; `amf:done` means amf merged its PR (`--merge` or `amf pr merge` on its `amf/issue-N` branch).
- **Stuck issues recover.** Each poll, `watch` puts `amf:in-progress` issues
  no amf is working on back to `amf:ready` after `AMF_STALE_TIMEOUT`
  (`--stale-timeout`; default twice the agent timeout, at least 2h).
- **Failure comments** list each agent tried with its exit code and
  duration, whether the fallback ran (and why not), and the redacted last
  lines of the agent's log.

### Configuration (.env)

Every configuration setting amf reads from `.env` or the environment (`amf help env`
prints the same list). Real environment variables win over the file, and
placeholders ending in `...`, and `owner/name`, count as unset.

| Variable | Default | Purpose |
|---|---|---|
| `GITHUB_TOKEN` | gh auth token, then amf auth login | API access: Contents, Issues, Pull requests (read & write). `GH_TOKEN` is read too. Unset, amf uses gh's token, then the one `amf auth login` saved (`amf help auth`) |
| `GITHUB_REPO` | required | `owner/name`; github.com URLs are accepted |
| `CLAUDE_CODE_OAUTH_TOKEN` | unset | Claude subscription token, passed to Claude. Makes `claude` usable without `--agent` |
| `ANTHROPIC_API_KEY` | unset | Claude API key, passed to Claude when there's no `CLAUDE_CODE_OAUTH_TOKEN`. Makes `claude` usable without `--agent` |
| `OPENAI_API_KEY` | unset | OpenAI API key, passed to Codex. Makes `codex` usable without `--agent` |
| `OPENCODE_API_KEY` | unset | OpenCode Console API key (`oc_sk_...`), passed to opencode only. One key covers pay-as-you-go `opencode/<id>` models (with credits) and, for a workspace subscribed to OpenCode Go, `opencode-go/<id>` models. Makes `opencode` usable without `--agent` with a paid `OPENCODE_MODEL`. Passed through the environment, never written to an agent configuration file; `amf init` preserves it in `.env` |
| `OPENCODE_MODEL` | `opencode/big-pickle` | The model opencode runs with: always without a pipeline, and for pipeline stages that name no model. `opencode/<id>` is a free or pay-as-you-go model, `opencode-go/<id>` an OpenCode Go one. `opencode models` lists the choices |
| `AMF_TRUSTED` | unset | GitHub logins, separated by commas or spaces (a leading `@` is allowed, case is ignored). When set, amf only works on issues they opened, except issues labeled `amf:auto`, which are always trusted. Unset means anyone's |
| `AMF_POSTGRES_URL` | unset | A PostgreSQL server that `run` and `watch` also write their logs to, in the `amflogs` database |
| `AMF_CLAUDE` | `claude` | Path to the Claude CLI |
| `AMF_PRIMARY_AGENT` | `codex` | The agent for issues without a pipeline: `claudecode` (or `claude`), `codex` or `opencode`, ignoring case. `--agent` and a pipeline override it |
| `AMF_SECONDARY_AGENT` | `claudecode` | The agent amf falls back to when the primary fails, in a fresh worktree; the same values, or `none` for no fallback |
| `AMF_AGENT_TIMEOUT` | `60m` | How long each agent run may take, as a Go duration; `--agent-timeout` (or its deprecated alias `--timeout`) overrides it. Must be at least 1m and at most 24h |
| `AMF_STALE_TIMEOUT` | `max(2x agent timeout, 2h)` | How long `watch` lets an issue stay `amf:in-progress` with no amf working on it before putting it back to `amf:ready`; `off` disables it. `--stale-timeout` overrides it |
| `AMF_LOG_LEVEL` | `info` (terminal), `debug` (event log) | `debug`, `info`, `warn` or `error`, for both the terminal and the event log of `run` and `watch`; `--log-level` overrides it |
| `AMF_CODEX` | `codex` | Path to the Codex CLI |
| `AMF_OPENCODE` | `opencode` | Path to the opencode CLI |
| `AMF_GITHUB_CLIENT_ID` | unset | The GitHub OAuth app `amf auth login` uses, when `--client-id` isn't given |
| `AMF_CONFIG_DIR` | the user config folder's amf | Where `amf auth login` saves tokens (`hosts.json`); environment only |
| `AMF_API_URL` | `https://api.github.com` | GitHub API base URL. `amf pr ready` and `amf pr draft` derive their GraphQL endpoint from it: `…/graphql` appended, or a GitHub Enterprise Server `…/api/v3` becomes `…/api/graphql` |
| `AMF_CLONE_URL` | `https://github.com/<repo>.git` | Where to clone `GITHUB_REPO` from |

`amf init` writes the common settings like this, the optional ones commented out:

```sh
GITHUB_TOKEN=github_pat_...   # Contents, Issues, Pull requests: read & write; or leave it unset and use gh or `amf auth login`
GITHUB_REPO=owner/name
CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-...   # optional: Claude subscription token from `claude setup-token`
ANTHROPIC_API_KEY=sk-ant-api03-...   # optional: Claude API key, used if there's no token above
OPENAI_API_KEY=sk-proj-...   # optional: OpenAI API key for codex
# OPENCODE_API_KEY=oc_sk_...   # optional: OpenCode Console API key, for pay-as-you-go and Go models
# OPENCODE_MODEL=opencode/big-pickle   # opencode/<id> for free and pay-as-you-go models, opencode-go/<id> for Go; 'opencode models' lists them
# AMF_PRIMARY_AGENT=codex   # optional: claudecode, codex or opencode; the agent for issues without a pipeline
# AMF_SECONDARY_AGENT=claudecode   # optional: claudecode, codex, opencode or none; runs when the primary fails
# AMF_AGENT_TIMEOUT=60m   # optional: how long each agent run may take (--agent-timeout overrides it)
# AMF_LOG_LEVEL=info   # optional: debug, info, warn or error, for the terminal and the event log in .amf/runs/
AMF_TRUSTED=login1,login2...   # optional: only work on issues opened by these GitHub users
# AMF_POSTGRES_URL=postgres://user:password@localhost:5432/postgres   # optional: also save the logs to database amflogs on this PostgreSQL server
```

The agent keys and `AMF_PRIMARY_AGENT`/`AMF_SECONDARY_AGENT` decide which
agent amf runs; see [Choosing the agent](docs/COMMANDS.md#choosing-the-agent)
or `amf help agents`. Free opencode models (`opencode/big-pickle` and the
`opencode/...-free` ones) need no key. For a pay-as-you-go `opencode/<id>`
model, or an [OpenCode Go](https://opencode.ai/v2/docs/console/go/)
`opencode-go/<id>` model, set `OPENCODE_API_KEY` to an OpenCode Console API
key; one key covers both, and the model's prefix picks the plan. amf passes
the key only to opencode, through its environment. `AMF_POSTGRES_URL` is
described in [Logs in PostgreSQL](docs/COMMANDS.md#logs-in-postgresql).

`CLAUDE_CODE_OAUTH_TOKEN` lets Claude run on your Pro/Max subscription, which
helps on servers or in CI where you can't log in interactively. Generate one with
`claude setup-token`. When it's set, amf passes it to Claude and drops any
`ANTHROPIC_API_KEY` from Claude's environment so the subscription is used
instead of API billing. When it's not set, Claude uses whatever login it
already has.

### Authentication

amf takes the first GitHub token it finds:

1. `GITHUB_TOKEN` (or `GH_TOKEN`) from the environment or `.env`
2. `gh auth token`, if the [GitHub CLI](https://cli.github.com) is installed and signed in
3. the token saved by `amf auth login`

`amf auth login` signs in with GitHub's device flow: it shows a code, opens
https://github.com/login/device, and saves the token to `hosts.json` (mode
0600) in `AMF_CONFIG_DIR`, or else `amf` in your user config folder
(`~/.config/amf`, `~/Library/Application Support/amf` on macOS). That is the
one path that doesn't default to the current directory, so the credential never
lands in a checkout an agent works in. `amf auth status` shows the source,
account and scopes; `amf auth logout` deletes the saved token; `amf auth token`
prints it for scripts. An OS keychain is future work.

The login needs the client ID of a GitHub OAuth app with the device flow on
(`--client-id` or `AMF_GITHUB_CLIENT_ID`, unless the release has one built in).
To register one: GitHub **Settings > Developer settings > OAuth Apps > New
OAuth App**, any homepage and callback URL, check **Enable Device Flow**,
register, and copy the **Client ID** (no secret needed). See
[amf auth](docs/COMMANDS.md#amf-auth).

### Check your setup

`amf doctor` checks everything amf needs, one line each, without changing
anything (`amf doctor --fix-labels` also corrects amf status labels that
disagree with the issue's pull requests):

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

For complete workflows, see the [examples cookbook](docs/EXAMPLES.md).

Run `run` and `watch` inside a clone of `GITHUB_REPO`, or from any folder that
holds your `.env`. There, amf clones the repo into `./<name>` the first time
(using your git credentials) and reuses that clone afterwards. amf checks
`GITHUB_REPO` against GitHub first, so a typo fails with a clear message.

### Paths default to the current directory

Every file or folder amf reads or writes defaults to the directory you run it
in (`os.Getwd`, never `$HOME` or `/tmp`), unless a command-line flag says
otherwise. Every command accepts these path flags, before or after the
command name (`amf --dir ~/work run 42` or `amf run 42 --dir ~/work`):

| Flag | Default | What it sets |
|---|---|---|
| `--dir <folder>` | the current directory | where amf works, as if started there: the checkout, `./.env`, `./<name>` and `.amf/` all follow it |
| `--env-file <file>` | `./.env`, then `.env` at the top of the enclosing checkout | the only `.env` amf reads, and the one `amf init` writes |
| `--clone-dir <folder>` | the enclosing checkout, else `./<name>` | the checkout run, watch and auto use (cloning `GITHUB_REPO` there if it is missing); it wins over the enclosing checkout |
| `--log-file <path>` (run, watch, auto) | `<checkout>/.amf/runs/<run_id>/events.jsonl` | the event log; `off` for none |

Relative flag values resolve against the directory amf was started in, before
`--dir` changes into its folder. amf's own state stays under `<checkout>/.amf/`
(worktrees, `logs/`, `runs/`, `auto/state.json`, `watch.lock`); `--memory-dir` (default: the current directory) holds `amf-memory/`, also excluded from git, which amf adds to
`.git/info/exclude`, so it is never committed. `amf update` still replaces the
running binary where it is, and its temporary download sits next to it.

```sh
amf version                      # show the installed version
amf doctor --dir ~/work           # check the setup in another directory
amf pipeline                     # inspect this checkout's pipeline
amf pipeline --fetch             # also check remote skills

amf issue "Add dark mode" --body "Use CSS variables for the palette"
amf issue "Fix login bug" --file bug.md
echo "details" | amf issue "Title"     # body from stdin
amf issue "Just a note" --no-label     # won't be picked up by watch

amf issue list                    # open issues, newest first, with their amf labels
amf issue list --state all        # include closed issues
amf issue list --label amf:failed # just the ones that failed
amf issue list --limit 5          # just the 5 newest
amf issue list --json --label amf:failed | jq -r '.[].url'  # as JSON: number, title, state, url, labels

amf run 42                  # process one issue now
amf watch --interval 60s    # process issues labeled `amf:ready`, one at a time
amf watch --agent codex     # without a pipeline, use Codex only, no fallback
amf watch --exclude-label blocked --exclude-label needs-human  # ...leaving issues with either label alone until it's removed
amf run 42 --draft          # open the PR as a draft
amf watch --draft           # ...opening each new PR as a draft
amf run 42 --merge          # ...and merge its PR with a merge commit
amf watch --merge=squash    # squash and merge each PR (or --merge=rebase)
amf auto "Add a dark mode toggle"  # plan issues, merge the first PR, then ask before the rest
amf auto "Add a dark mode toggle" --checkpoint-after 2 --wait-for-checks # review CI after two merges
amf auto "Add a dark mode toggle" --approve # approve the checkpoint without a terminal
amf auto "Add a dark mode toggle" --no-checkpoint # merge all issues without pausing
amf auto --continue        # approve the checkpoint and resume
amf auto status            # inspect saved progress
amf auto --abort           # forget the saved run, leaving GitHub issues and PRs unchanged
amf run 42 --dry-run        # make the change locally and show the PR, touching nothing on GitHub

amf pr reopen 57            # reopen a closed, unmerged PR
amf pr ready 57             # mark an open draft PR ready for review
amf pr draft 57             # convert an open PR to a draft

amf memory sync             # backfill the repo's memory from GitHub (then only what changed)
amf memory similar 42       # past issues, PRs and runs like issue 42
amf memory similar "flaky login test" --json
amf memory show 42          # issue 42's PRs and every run
amf watch --memory-backend markdown --memory-dir ~/amf-notes  # keep memory as Markdown files elsewhere
```

See [docs/COMMANDS.md](docs/COMMANDS.md) for the details:

- [Choosing the agent](docs/COMMANDS.md#choosing-the-agent): which agent runs, and the fallback
- [Auto: one sentence to merged PRs](docs/COMMANDS.md#auto-one-sentence-to-merged-prs): checkpoints, approval and recovery
- [Merge methods](docs/COMMANDS.md#merge-methods): how `--merge`, `--merge=squash` and `--merge=rebase` shape history
- [What `amf run N` does](docs/COMMANDS.md#what-amf-run-n-does), [logs](docs/COMMANDS.md#logs) and [logs in PostgreSQL](docs/COMMANDS.md#logs-in-postgresql)
- [Pipelines](docs/COMMANDS.md#pipelines-plan--code--eval) and [labels](docs/COMMANDS.md#labels)
- [Repo memory](docs/COMMANDS.md#repo-memory): similar past work, backends and where memory lives

## Security

Agents run with all permission checks off, so an issue body is effectively
code you're executing on your machine.

- Only label issues you trust. On a public repo only people with triage
  access can add labels, but anyone can write an issue body, and its author
  can edit it after you label it.
- Set `AMF_TRUSTED=you,teammate` to have amf work only on issues those
  GitHub users opened, except issues labeled `amf:auto`, which are always
  trusted. Other authors' issues without that label are marked `amf:failed`
  with a comment saying why, and no agent runs. Unset, amf works on anyone's
  issue, and `amf doctor` warns about it.
- `GITHUB_TOKEN` and `GH_TOKEN` are stripped from every agent's environment, but agents can
  still read files on disk, including `.env` at the top of your checkout and
  amf's `hosts.json`, and can run `gh auth token`. For full
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
