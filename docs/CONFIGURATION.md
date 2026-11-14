# amf configuration

Back to the [README](../README.md). See the [command reference](COMMANDS.md)
for flags, agent selection and issue processing.

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

### Per-repo settings

A repo can have its own settings file outside the checkout:
`<config-dir>/repos/<host>/<owner>/<name>.env`. `<config-dir>` is
`AMF_CONFIG_DIR` or the user config folder's `amf` (the folder holding
`hosts.json`), `<host>` is `AMF_API_URL`'s host (`github.com` for
`https://api.github.com`), and `<owner>/<name>` is the lower-cased
`GITHUB_REPO`. The file uses the same `KEY=value` syntax as `.env`, and a
missing file is normal. Only keys starting with `AMF_PR_` or `AMF_REVIEW_`
are read; any other key is an error naming the file and the key, so a token
can never come from it. For example, for `vicgupta/tinyagent` on github.com:

```sh
mkdir -p ~/.config/amf/repos/github.com/vicgupta
printf 'AMF_PR_SECTIONS=on\n' > ~/.config/amf/repos/github.com/vicgupta/tinyagent.env
```

It is read after `.env` and before the defaults, so the environment wins
first, then `.env`, then this file.

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
placeholders ending in `...`, and `owner/name`, count as unset (except
`AMF_REVIEW_COMMAND`, whose shell commands may end in `./...`).

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
| `AMF_AGENT_IDLE_TIMEOUT` | `30m` | How long an agent may produce no output on stdout or stderr, and change no file in the worktree, before amf stops it and fails the run, as a Go duration; `off` disables it. Must be positive |
| `AMF_STALE_TIMEOUT` | `max(2x agent timeout, 2h)` | How long `watch` lets an issue stay `amf:in-progress` with no amf working on it before putting it back to `amf:ready`; `off` disables it. `--stale-timeout` overrides it |
| `AMF_PR_SECTIONS` | `off` | `on` asks the agent for `## Testing` and `## Decisions on open questions` sections, and adds those, the docs changed from git, and a README feature-list check to the PR body. Any other value is an error |
| `AMF_LOG_LEVEL` | `info` (terminal), `debug` (event log) | `debug`, `info`, `warn` or `error`, for both the terminal and the event log of `run` and `watch`; `--log-level` overrides it |
| `AMF_REVIEW_GATE` | `off` | `on` runs the opt-in pre-PR review gate before anything is pushed: `AMF_REVIEW_COMMAND`, then a review agent. Any other non-empty value is an error naming the variable |
| `AMF_REVIEW_COMMAND` | unset | Shell command the review gate runs in the worktree with `sh -c`, as the repo's test gate (for example `test -z "$(gofmt -l .)" && go test ./...`). Unset runs no command step |
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
# AMF_AGENT_IDLE_TIMEOUT=30m   # optional: stop an agent that produces no output or changes no worktree file for this long (off disables it)
# AMF_PR_SECTIONS=off   # optional: on adds Testing, Docs updated, Decisions and README sections to PRs
# AMF_LOG_LEVEL=info   # optional: debug, info, warn or error, for the terminal and the event log in .amf/runs/
# AMF_REVIEW_GATE=off   # optional: on runs a test command and a review agent before amf pushes
# AMF_REVIEW_COMMAND=   # optional: shell command the review gate runs in the worktree with sh -c
AMF_TRUSTED=login1,login2...   # optional: only work on issues opened by these GitHub users
# AMF_POSTGRES_URL=postgres://user:password@localhost:5432/postgres   # optional: also save the logs to database amflogs on this PostgreSQL server
```

The agent keys and `AMF_PRIMARY_AGENT`/`AMF_SECONDARY_AGENT` decide which
agent amf runs; see [Choosing the agent](COMMANDS.md#choosing-the-agent)
or `amf help agents`. Free opencode models (`opencode/big-pickle` and the
`opencode/...-free` ones) need no key. For a pay-as-you-go `opencode/<id>`
model, or an [OpenCode Go](https://opencode.ai/v2/docs/console/go/)
`opencode-go/<id>` model, set `OPENCODE_API_KEY` to an OpenCode Console API
key; one key covers both, and the model's prefix picks the plan. amf passes
the key only to opencode, through its environment. `AMF_POSTGRES_URL` is
described in [Logs in PostgreSQL](COMMANDS.md#logs-in-postgresql).

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
[amf auth](COMMANDS.md#amf-auth).

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
ok    labels          all 5 amf labels exist
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

