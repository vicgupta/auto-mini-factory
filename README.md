# amf — autonomous mini factory

One stdlib-only Go binary that turns GitHub issues into pull requests with a
coding agent (Claude Code, Codex or opencode, whichever you have), or with a
plan → code → eval pipeline of them. For a solo developer or small team that wants coding agents without
running a platform.

## Commands

| Command | What it does |
|---|---|
| `amf init` | write `.env` and create amf's labels |
| `amf issue` | manage issues: `create`, `list`, `run`, `watch` |
| `amf pr` | manage pull requests: `list`, `merge`, `view`, `diff`, `close`, `reopen`, `ready`, `draft` |
| `amf run N` | process one issue into a PR (alias of `issue run`) |
| `amf watch` | process `amf:ready` issues as they arrive (alias of `issue watch`) |
| `amf auto` | plan one sentence into up to 5 issues and merge their PRs, pausing for approval after the first merge by default |
| `amf pipeline` | check the plan/code/eval pipeline in `AGENTS.md` |
| `amf doctor` | check the setup here, changing nothing |
| `amf update` | replace this amf binary with the latest (or given) release |
| `amf version` | print amf's version |
| `amf help` | show help for a command, or the `agents` and `env` topics |

`amf help <command>` or `amf <command> -h` prints a command's details, and
[docs/COMMANDS.md](docs/COMMANDS.md) has the full reference.
For common failures and fixes, see the
[troubleshooting guide](docs/TROUBLESHOOTING.md).

## Quick start

```sh
curl -fsSL https://raw.githubusercontent.com/vicgupta/auto-mini-factory/main/install.sh | sh
cd your-repo
amf init                 # writes .env: fill in GITHUB_TOKEN and an agent key
amf doctor               # check the setup
amf issue "Add dark mode" -b "Use CSS variables for the palette"
amf run 1                # or: amf watch, to handle amf:ready issues as they come
```

<details>
<summary><b>Install options</b></summary>

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

</details>

## Configure

In a clone of the repo you want amf to work on, run `amf init`. It writes
`.env` with every setting amf uses (listed in
[Configuration (.env)](#configuration-env)) and creates amf's labels.

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
Keep `.env` out of git: add it to the target repo's `.gitignore`.

### Configuration (.env)

Every setting amf reads, from `.env` or the environment (`amf help env`
prints the same list). Real environment variables win over the file, and
placeholders ending in `...` count as unset.

| Variable | Default | Purpose |
|---|---|---|
| `GITHUB_TOKEN` | required | API access: Contents, Issues, Pull requests (read & write) |
| `GITHUB_REPO` | required | `owner/name`; github.com URLs are accepted |
| `CLAUDE_CODE_OAUTH_TOKEN` | unset | Claude subscription token, passed to Claude. Makes `claude` usable without `--agent` |
| `ANTHROPIC_API_KEY` | unset | Claude API key, passed to Claude when there's no `CLAUDE_CODE_OAUTH_TOKEN`. Makes `claude` usable without `--agent` |
| `OPENAI_API_KEY` | unset | OpenAI API key, passed to Codex. Makes `codex` usable without `--agent` |
| `OPENCODE_API_KEY` | unset | OpenCode Console API key (`oc_sk_...`), passed to opencode only. One key covers pay-as-you-go `opencode/<id>` models (with credits) and, for a workspace subscribed to OpenCode Go, `opencode-go/<id>` models. Makes `opencode` usable without `--agent` with a paid `OPENCODE_MODEL`. Never written to a file |
| `OPENCODE_MODEL` | `opencode/big-pickle` | The model opencode runs with: always without a pipeline, and for pipeline stages that name no model. `opencode/<id>` is a free or pay-as-you-go model, `opencode-go/<id>` an OpenCode Go one. `opencode models` lists the choices |
| `AMF_TRUSTED` | unset | GitHub logins, separated by commas or spaces (a leading `@` is allowed, case is ignored). When set, amf only works on issues they opened. Unset means anyone's |
| `AMF_POSTGRES_URL` | unset | A PostgreSQL server that `run` and `watch` also write their logs to. See [Logging to PostgreSQL](docs/COMMANDS.md#logs-in-postgresql) |
| `AMF_CLAUDE` | `claude` | Path to the Claude CLI |
| `AMF_PRIMARY_AGENT` | `codex` | The agent for issues without a pipeline: `claudecode` (or `claude`), `codex` or `opencode`, ignoring case. `--agent` and a pipeline override it |
| `AMF_SECONDARY_AGENT` | `claudecode` | The agent amf falls back to when the primary fails, in a fresh worktree; the same values, or `none` for no fallback |
| `AMF_AGENT_TIMEOUT` | `60m` | How long each agent run may take, as a Go duration; `--agent-timeout` (or its alias `--timeout`) overrides it. Must be at least 1m and at most 24h |
| `AMF_LOG_LEVEL` | `info` (terminal), `debug` (event log) | `debug`, `info`, `warn` or `error`, for both the terminal and the event log of `run` and `watch`; `--log-level` overrides it |
| `AMF_CODEX` | `codex` | Path to the Codex CLI |
| `AMF_OPENCODE` | `opencode` | Path to the opencode CLI |
| `AMF_API_URL` | `https://api.github.com` | GitHub API base URL. `amf pr ready` and `amf pr draft` derive their GraphQL endpoint from it: `…/graphql` appended, or a GitHub Enterprise Server `…/api/v3` becomes `…/api/graphql` |
| `AMF_CLONE_URL` | `https://github.com/<repo>.git` | Where to clone the repo from |

`amf init` writes them like this, the optional ones commented out:

```sh
GITHUB_TOKEN=github_pat_...   # Contents, Issues, Pull requests: read & write
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

For complete workflows, see the [examples cookbook](docs/EXAMPLES.md).

Run `run` and `watch` inside a clone of `GITHUB_REPO`, or from any folder that
holds your `.env`. There, amf clones the repo into `./<name>` the first time
(using your git credentials) and reuses that clone afterwards. amf checks
`GITHUB_REPO` against GitHub first, so a typo fails with a clear message.

```sh
amf issue "Add dark mode" -b "Use CSS variables for the palette"
amf issue "Fix login bug" -f bug.md
echo "details" | amf issue "Title"     # body from stdin
amf issue "Just a note" --no-label     # won't be picked up by watch

amf issue list                    # open issues, newest first, with their amf labels
amf issue list --state all        # include closed issues
amf issue list --label amf:failed # just the ones that failed
amf issue list --limit 5          # just the 5 newest
amf issue list --json --label amf:failed | jq -r '.[].url'  # as JSON: number, title, state, url, labels

amf run 42                  # process one issue now
amf watch --interval 60s    # process issues labeled `amf:ready`, one at a time
amf watch --agent codex     # ...with Codex only, no fallback
amf watch --exclude-label blocked --exclude-label needs-human  # ...leaving issues with either label alone until it's removed
amf run 42 --draft          # open the PR as a draft
amf watch --draft           # ...opening each new PR as a draft
amf run 42 --merge          # ...and merge its PR with a merge commit
amf watch --merge=squash    # squash and merge each PR (or --merge=rebase)
amf auto "Add a dark mode toggle"  # plan issues, merge the first PR, then ask before the rest
amf auto "Add a dark mode toggle" --checkpoint-after 2 --wait-for-checks # review CI after two merges
amf auto "Add a dark mode toggle" --approve # approve the checkpoint without a terminal
amf auto "Add a dark mode toggle" --no-checkpoint # merge all issues without pausing
amf auto --continue        # approve and resume when there was no terminal to ask on
amf run 42 --dry-run        # make the change locally and show the PR, touching nothing on GitHub
```

See [docs/COMMANDS.md](docs/COMMANDS.md) for the details:

- [Choosing the agent](docs/COMMANDS.md#choosing-the-agent): which agent runs, and the fallback
- [Merge methods](docs/COMMANDS.md#merge-methods): how `--merge`, `--merge=squash` and `--merge=rebase` shape history
- [What `amf run N` does](docs/COMMANDS.md#what-amf-run-n-does), [logs](docs/COMMANDS.md#logs) and [logs in PostgreSQL](docs/COMMANDS.md#logs-in-postgresql)
- [Pipelines](docs/COMMANDS.md#pipelines-plan--code--eval) and [labels](docs/COMMANDS.md#labels)

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
