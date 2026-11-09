# amf — autonomous mini factory

One pure-Go binary (no cgo) that turns GitHub issues into pull requests with a
coding agent (Claude Code, Codex or opencode, whichever you have), or with a
plan → code → eval pipeline of them. For a solo developer or small team that wants coding agents without
running a platform.

## Output

Command data (issue/PR numbers and URLs, tables, JSON and diffs) goes to
stdout; progress logs, warnings and errors go to stderr. Redirect or pipe
stdout to capture results while keeping diagnostics visible. Use `--json`
where available for machine-readable results. Workflow commands such as
`init`, `auto`, `pr merge`, `pr sweep --wait-for-checks` and `update` also
print status messages or prompts to stdout, so their output is not always
a data-only stream. `amf help output` describes the same contract. See
[JSON output](docs/JSON.md) for every `--json` command and its schema.

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

`amf --update` and `amf -u` are the same command as `amf update`, with the
same flags. Root `-u` is only this alias; `amf pr merge -u` remains
`--update-branch` on that command.

See [release notes](https://github.com/vicgupta/auto-mini-factory/releases)
for what changed in each version.

`amf update` downloads the same release archives as `install.sh`, checks them
against `checksums.txt`, makes sure the new binary runs, then swaps it in place
of the running one. It needs write access to the folder `amf` is in.

### Shell completion

`amf completion bash|zsh|fish` prints a script that completes commands,
subcommands and their long and short flags, including the global `--quiet`,
`--color` and path flags. It needs no `.env` or GitHub access.

For Bash, add this to `~/.bashrc`:

```bash
source <(amf completion bash)
```

For Zsh, add this to `~/.zshrc` after initializing completion (omit the first
line if your shell configuration already runs `compinit`):

```zsh
autoload -Uz compinit; compinit
source <(amf completion zsh)
```

For Fish, add this to `~/.config/fish/config.fish`:

```fish
amf completion fish | source
```

Start a new shell, or run the appropriate snippet to enable completion now.
You can also save the output to a file and source it; regenerate that file
after upgrading amf to pick up new commands and flags.

## Quick start

Have a git identity and credentials that can push to your repo. See
[Choosing the agent](docs/COMMANDS.md#choosing-the-agent) for agent key and
login requirements.

```sh
cd your-repo             # a clone of the GitHub repo you want to work on
amf init                 # writes .env; fills GITHUB_REPO from origin
# Edit .env: set GITHUB_TOKEN (or use gh / amf auth login), check GITHUB_REPO and configure your agent.
amf init                 # creates any missing amf labels once configured
amf doctor               # check the setup
amf issue "Add dark mode" --body "Use CSS variables for the palette"
amf run 42               # replace 42 with the issue number in the URL printed above

amf version                      # show the installed version
amf doctor --dir ~/work           # check the setup in another directory
amf pipeline                     # inspect this checkout's pipeline
amf pipeline --fetch             # also check remote skills

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
amf run 42 --draft          # open the PR as a draft (-D)
amf watch --draft           # ...opening each new PR as a draft
amf run 42 --merge          # ...and merge its PR with a merge commit
amf run 42 --merge --wait-for-checks  # ...after waiting for CI (-w)
amf watch --merge=squash    # squash and merge each PR (or --merge=rebase)
amf auto "Add a dark mode toggle"  # plan issues, merge the first PR, then ask before the rest
amf auto "Add a dark mode toggle" --checkpoint-after 2 --wait-for-checks # checkpoint: wait up to 5m for CI after two merges (each merge waits up to 30m for head checks)
amf auto "Add a dark mode toggle" --approve # approve the checkpoint without a terminal
amf auto "Add a dark mode toggle" --no-checkpoint # merge all issues without pausing
amf auto --continue        # approve the checkpoint and resume
amf auto status            # inspect saved progress
amf auto --abort           # forget the saved run, leaving GitHub issues and PRs unchanged
amf run 42 --dry-run        # make the change locally and show the PR, touching nothing on GitHub
amf run 42 --force          # rerun an issue with an open PR; dropped commits are saved on amf/backup/issue-42-<sha>-<time> (-f)

amf issue label 42 --add bug --remove needs-triage  # change labels by hand (-a/-R); select a set with --state/--label/--unlabeled
amf pr view 57              # state, branch, author, checks, mergeability and diff stats
amf pr comment 57 --body "LGTM"  # comment (-b); use --file review.md (-f) for a file or - for stdin
amf pr merge 57 --update-branch --wait-for-checks  # -u/-w: update from the base branch, wait for CI, then merge
amf pr sweep --merge --wait-for-checks --dry-run --json  # -m/-w/-n/-j: update, merge and label every open amf PR
amf pr reopen 57            # reopen a closed, unmerged PR
amf pr ready 57             # mark an open draft PR ready for review
amf pr draft 57             # convert an open PR to a draft
amf tag list --limit 10     # tags by name with their commits (read-only)
amf tag view v1.2.3         # the commit a lightweight or annotated tag points to
amf release list --state published  # released versions, newest first (read-only)
amf release latest          # GitHub's latest published non-prerelease release
amf doctor --fix-labels     # correct amf status labels that disagree with the PRs (-F)

amf memory sync             # backfill the repo's memory from GitHub (then only what changed)
amf memory similar 42       # past issues, PRs and runs like issue 42
amf memory similar "flaky login test" --json
amf memory show 42          # issue 42's PRs and every run
amf watch --memory-backend markdown --memory-dir ~/amf-notes  # keep memory as Markdown files elsewhere
```

Or run `amf watch` to process open issues labeled `amf:ready`, oldest first.

`amf auth login` (with `--client-id` or `-C`, else `AMF_GITHUB_CLIENT_ID`)
saves a token; `amf auth status`, `amf auth logout` and `amf auth token` (with
`-f, --force` to print to a terminal) manage it. Every flag has a long and a
short form: `amf help flags` lists them, including `-D, --draft` and
`-O, --color=auto|always|never`. An agent that prints no output for
`AMF_AGENT_IDLE_TIMEOUT` (default 15m; `off` disables it) is stopped and the
run fails, so the fallback agent can try.

For all settings and defaults, run `amf help env` or see the
[configuration guide](docs/CONFIGURATION.md).

## Documentation

amf never invokes a pager. Long output from `amf pr diff` and `amf memory show`
goes directly to stdout, even in a terminal; there is nothing to disable.
There is no `--no-pager` flag, and `PAGER` (including `PAGER=cat`) has no effect
on amf's output. To page it yourself, pipe to a pager:

```sh
amf pr diff 42 | less
amf memory show 42 | less
```

`amf help <command>` prints a command's details. See:

- [Command reference](docs/COMMANDS.md): commands, flags, exit status, paths and logs
- [JSON output](docs/JSON.md): every `--json` command and its schema
- [Man page](docs/COMMANDS.md#man-page): generate and install with `amf man`
- [Shell completion](#shell-completion): enable completion in bash, zsh or fish
- [Configuration](docs/CONFIGURATION.md): `.env`, authentication and `amf doctor`
- [Examples cookbook](docs/EXAMPLES.md): complete workflows
- [Troubleshooting](docs/TROUBLESHOOTING.md): common failures and fixes
- [Specification](docs/SPECS.md) and [issue processing](docs/AMF-PROCESS.md)
- [Development](docs/DEVELOPMENT.md): offline and live tests
- [Release notes](https://github.com/vicgupta/auto-mini-factory/releases): changes by version

### Signals

Ctrl-C (`SIGINT`) and `SIGTERM` request a graceful stop for `amf run`,
`amf watch` and `amf auto`: amf cancels the current step, cleans up and
releases `.amf/watch.lock` before exiting. `watch` stops polling and starts
no further issues. An interrupted issue normally becomes `amf:failed`;
an existing open or merged PR keeps its corresponding status.

On Unix, a running agent's whole process group receives `SIGTERM`, then
`SIGKILL` if it is still there after 15 seconds. On Windows, amf ends the
whole agent process tree with `taskkill /T /F`, without a grace period.

Once `auto` has saved a plan, partial progress is kept in
`.amf/auto/state.json`. Inspect it with `amf auto status`, then resume with
`amf auto --continue` (which also approves any pending checkpoint), or
discard the saved run with `amf auto --abort`. Interrupting the initial
planning step leaves no saved plan to resume.

## Security

Agents run with all permission checks off, so an issue body is effectively
code you're executing on your machine. Only label issues you trust; set
`AMF_TRUSTED` to limit issue authors. Agents can read local credentials even
though amf strips GitHub tokens from their environment. See the
[security model](docs/SPECS.md#security-model) before running amf or enabling
`--merge`.

## License

MIT; see [LICENSE](LICENSE).
