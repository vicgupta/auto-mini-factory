# amf command reference

The details behind the [README](../README.md)'s overview: what each command
does, its flags, logs and labels. `amf help <command>` (or `amf <command> --help`,
`-h` for short) prints the same in the terminal; [Flag conventions](#flag-conventions)
explains the long and short forms. Every setting is in
[Configuration (.env)](../README.md#configuration-env) and `amf help env`.

For complete command sequences, see the [examples cookbook](EXAMPLES.md).
For common failures and fixes, see the [troubleshooting guide](TROUBLESHOOTING.md).

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

## Exit status

- `0` — success.
- `1` — a command failed (for example, a GitHub error, agent failure, or refused merge).
- `2` — a usage error (for example, no command, an unknown command or flag, or missing arguments).

`amf --help` and `amf help <command>` exit `0`; unknown commands exit `2`.
Some commands currently return `2` for `amf <command> --help` (or `-h`),
including `amf run --help`. Use `amf help <command>` when scripts need help
to exit successfully. Command-specific value validation can return `1`,
such as `amf run 0`.

## issue commands

| Command | What it does |
|---|---|
| `amf issue create "Title" [--body text \| --file path] [--label name] [--no-label]` | create an issue (the body may also come from stdin) |
| `amf issue list [--state open\|closed\|all] [--label name] [--search text] [--limit n] [--json]` | list issues, newest first, with their amf labels |
| `amf issue label <N> [--add name]... [--remove name]...` | add or remove an issue's labels |
| `amf issue run <N> [flags]` | process one issue into a PR: see amf help run |
| `amf issue watch [flags]` | process amf:ready issues as they arrive: see amf help watch |

## pr commands

| Command | What it does |
|---|---|
| `amf pr list [--state open\|closed\|merged\|all] [--limit n] [--issue n] [--json]` | list pull requests, newest first |
| `amf pr merge <N> [--squash \| --rebase] [--delete-branch] [--update-branch] [--wait-for-checks]` | merge a pull request |
| `amf pr sweep [--merge[=method]] [--wait-for-checks] [--dry-run] [--json]` | update, merge and label the open amf pull requests |
| `amf pr view <N> [--json]` | show a pull request |
| `amf pr diff <N> [--name-only]` | show a pull request's diff, or one changed path per line |
| `amf pr close <N> [--comment text] [--delete-branch]` | close a pull request |
| `amf pr reopen <N>` | reopen a pull request |
| `amf pr ready <N>` | mark a draft pull request ready for review |
| `amf pr draft <N>` | convert a pull request to a draft |

## Subcommand reference

`amf help pr merge`, `amf help issue list` and so on print the same help as
`amf pr merge --help`: the summary below, then every flag (see
[Flag reference](#flag-reference) and [Flag conventions](#flag-conventions)).
PR numbers accept positive integers such as `42` or `"#42"`.
The commands below read configuration from the environment and `.env`;
they need GitHub access but no git checkout.

### amf issue create

```text
amf issue create "Title" [--body text | --file file.md | --file -] [--label L] [--no-label]
```

Creates an issue; amf issue "Title" is a shortcut.
--body selects the body, even when empty; --file reads a file, or stdin for --file -.
Without either, piped stdin supplies the body. --body and a nonempty --file conflict.
--label defaults to amf:ready; --no-label creates an unlabeled issue.
An explicit --label conflicts with --no-label; a blank label is rejected.
Title and label surrounding whitespace is trimmed. Prints #N <url>.
There is no --json flag.

Example output:

```text
#42 https://github.com/owner/repo/issues/42
```

### amf issue list

```text
amf issue list [--state open|closed|all] [--label L] [--search TEXT] [--limit N] [--json]
```

Lists issues, newest first, excluding pull requests.
--state defaults to open, ignores case and trims surrounding whitespace.
--label filters by one label (default empty, no filter).
--search matches title text ignoring case (default empty, no filter).
--limit defaults to 0 (unlimited); negative limits are rejected.
Filters apply before the limit. Text columns are ISSUE, AMF, TITLE, plus
STATE with --state all. AMF shows only amf and amf:* labels, or -.
--json prints an array of {number, title, state, url, labels}; number is an
integer, labels contains every label as strings, and the other fields are
strings. No matches prints []. JSON emits no truncation hint; text reports
a limit hiding matches on stderr.

Example output:

```text
ISSUE  AMF        TITLE
#42    amf:ready  Fix login
```

With `--json`:

```json
[{"number":42,"title":"Fix login","state":"open","url":"https://github.com/owner/repo/issues/42","labels":["amf:ready","bug"]}]
```

### amf issue label

```text
amf issue label <N> [--add L]... [--remove L]...
```

Adds and removes labels on one issue (not a pull request). --add and
--remove repeat, or take comma-separated names; at least one is required,
and a label can't be in both. Labels already present, or already absent,
cost no API call. Adding an amf status label (amf:in-progress, amf:pr-open,
amf:done or amf:failed) also removes the issue's other status label;
adding two fails. Prints #N: added ...; removed ..., or #N unchanged.
There is no --json flag.

Example output:

```text
#42: added amf:done; removed amf:failed
```

### amf pr list

```text
amf pr list [--state open|closed|merged|all] [--limit N] [--issue N] [--json]
```

Lists pull requests, newest first, including drafts.
--state defaults to open; closed excludes merged PRs, merged selects only
merged PRs, and all includes everything. Values are case sensitive.
--limit defaults to 0 (unlimited). --issue defaults to 0 (no filter).
Both require nonnegative integers. Issue filtering precedes the limit.
An issue is inferred only from a canonical amf/issue-N head branch in this
repository; forks and unavailable head repositories have no inferred issue.
Text columns are PR, ISSUE, STATE, TITLE; missing ISSUE is -.
--json prints an array of {number, title, state, draft, url, head, base,
issue}. number is an integer, draft a boolean, issue an integer or null;
the others are strings. state is open, draft, closed or merged.
No matches prints []; text instead prints No ... in owner/name.
Text reports a limit hiding matches on stderr; JSON emits no such hint.

Example output:

```text
PR   ISSUE  STATE  TITLE
#57  #42    open   Fix login
```

With `--json`:

```json
[{"number":57,"title":"Fix login","state":"open","draft":false,"url":"https://github.com/owner/repo/pull/57","head":"amf/issue-42","base":"main","issue":42}]
```

### amf pr merge

```text
amf pr merge <N> [--squash | --rebase] [--delete-branch] [--update-branch] [--wait-for-checks]
```

Merges using a merge commit by default; --squash or --rebase selects that
method. The two flags conflict. Refuses merged, closed or draft PRs.
Pins the merge to the fetched head SHA; GitHub checks and reviews still apply.
--delete-branch deletes the head after merging only in this repository;
forks or unavailable head repositories are skipped with a stderr note.
A deletion failure is an error after the merge has already succeeded.
Prints Merged #N <url>, Squashed and merged #N <url>, or
Rebased and merged #N <url>. There is no --json flag.
--update-branch first compares the head with the base and, if it is
behind, asks GitHub to merge the base into it (as the Update branch button
does), then waits up to a minute for the new head and merges that. A
conflict fails without merging. --wait-for-checks then waits up to 30
minutes for the head's check runs: it merges once all pass, fails if one
fails or time runs out, and merges if no check reports within 2 minutes.
Use both for a PR that is behind a base whose rules require it be up to
date: amf pr merge N -u -w --squash.
When the head is amf/issue-N in this repository, issue N is then labeled
amf:done and loses amf:failed, amf:pr-open and amf:in-progress
(printing Labeled #N amf:done when that changed anything); a labeling
failure is a stderr warning, not an error.

Example output:

```text
Merged #57 https://github.com/owner/repo/pull/57
```

### amf pr sweep

```text
amf pr sweep [--merge[=method]] [--wait-for-checks] [--dry-run] [--json]
```

Sweeps the open pull requests amf opened: an `amf/issue-N` head in this
repository whose issue is labeled `amf:pr-open`. Drafts are skipped. For
each, oldest first:

1. A branch behind its base is updated with GitHub's Update branch (as
   `amf pr merge --update-branch` does). Its checks then run on the new
   head, so the PR is reported `updated` and merged by the next sweep;
   `--wait-for-checks` waits for them (up to 30 minutes) instead.
2. Pending checks leave the PR `waiting` for the next sweep; failed checks
   block it.
3. With `--merge` (`-m` alone for a merge commit, `--merge=squash`,
   `-m rebase`), a PR whose checks passed, or that has none, is merged
   pinned to its head, and its issue is labeled `amf:done`, losing
   `amf:failed`, `amf:pr-open` and `amf:in-progress`. A change to
   `AGENTS.md` is never merged. Without `--merge` the PR is reported
   `ready` and left open.

A PR that can't merge (an update that conflicts, failed checks, `AGENTS.md`,
or GitHub refusing the merge) is reported `blocked` and gets a status
comment, once per head commit and reason. For a conflict, the comment
suggests `amf run N --force`, which has an agent resolve it.

In a git checkout the sweep holds the [repo lock](#the-repo-lock), so it
fails while `amf watch`, `amf run` or `amf auto` works there. `--dry-run`
only reads from GitHub and reports `would-update` or `would-merge`. It costs
two list calls, then a few reads per PR; writes only when there is something
to do. `amf watch --merge` runs the same sweep each poll, without waiting
for checks.

Prints one line per PR, `#N (issue #M) <action>[: reason] <url>`, or
`No open amf pull requests to sweep.` `--json` prints an array of `number`,
`issue`, `url`, `action`, `reason` (may be empty) and `commit` (set once
merged).

Example output:

```text
#57 (issue #42) merged 1a2b3c4 https://github.com/owner/repo/pull/57
#58 (issue #43) waiting: checks pending (5 checks: 3 success, 2 in_progress) https://github.com/owner/repo/pull/58
```

### amf pr sweep

| Flag | What it does |
|---|---|
| `-n, --dry-run` | only read from GitHub and print what the sweep would do |
| `-j, --json` | print a JSON array of number, issue, url, action, reason and commit |
| `-m, --merge[=<method>]` | merge each ready PR: --merge (or -m) alone for a merge commit, or --merge=squash, -m squash, -m rebase (default: don't merge) |
| `-w, --wait-for-checks` | wait for pending checks on each PR (up to 30 minutes) instead of leaving it for the next sweep |

### amf pr view

```text
amf pr view <N> [--json]
```

Shows title, state, branches, author, inferred issue, mergeability, change
counts, URL and body. For an open PR it adds `behind:` (commits behind the
base branch) and `checks:` (none, pending, passed or failed, with counts, for
the head commit). While GitHub is still computing mergeability, amf re-reads
the PR up to three times (about 6 seconds) before showing unknown.
--json prints one object with number, title, state, draft, url, head, base,
author, issue, mergeable, mergeable_state, additions, deletions,
changed_files, behind_by, ahead_by, checks and body. number and change counts
are integers, draft is a boolean, issue is an integer or null, mergeable is a
boolean or null, behind_by and ahead_by are integers or null (closed PRs, or
GitHub couldn't compare), checks is {state, summary} or null, and the rest
are strings. state is open, closed or merged; drafts remain open.
issue is inferred from a canonical amf/issue-N branch, including forks.

Example output:

```text
#57 Fix login
state:     open
branch:    amf/issue-42 → main
author:    octocat
issue:     #42
mergeable: yes
behind:    2 commits behind main
checks:    passed (5 checks: 5 success)
changes:   +3 −1 in 1 file
url:       https://github.com/owner/repo/pull/57
```

With `--json`:

```json
{"number":57,"title":"Fix login","state":"open","draft":false,"url":"https://github.com/owner/repo/pull/57","head":"amf/issue-42","base":"main","author":"octocat","issue":42,"mergeable":true,"mergeable_state":"clean","additions":3,"deletions":1,"changed_files":1,"behind_by":2,"ahead_by":1,"checks":{"state":"passed","summary":"5 checks: 5 success"},"body":""}
```

### amf pr diff

```text
amf pr diff <N> [--name-only]
```

Prints the unified diff. --name-only instead prints one changed path per
line, fetching every page of changed files. There is no --json flag.

Example output with `--name-only`:

```text
src/login.go
```

### amf pr close

```text
amf pr close <N> [--comment text] [--delete-branch]
```

Closes an unmerged PR. --comment posts text before closing (default none).
--delete-branch deletes the head after closing only in this repository;
forks or unavailable head repositories are skipped with a stderr note.
A deletion failure is an error after the close has already succeeded.
Already closed PRs succeed without commenting or deleting; merged PRs fail.
Prints Closed #N <url>, or #N is already closed.
Leaves linked issue labels unchanged. There is no --json flag.

Example output:

```text
Closed #57 https://github.com/owner/repo/pull/57
```

### amf pr reopen

```text
amf pr reopen <N>
```

Reopens a closed, unmerged PR. Already open PRs succeed without changing it;
merged PRs fail. No command-specific flags.
Prints Reopened #N <url>, or #N is already open.
Leaves linked issue labels unchanged. There is no --json flag.

Example output:

```text
Reopened #57 https://github.com/owner/repo/pull/57
```

### amf pr ready

```text
amf pr ready <N>
```

Marks an open draft PR ready for review using GitHub GraphQL.
Closed or merged PRs fail. No command-specific flags.
Prints #N is ready for review <url>, or #N is already ready for review.
Leaves linked issue labels unchanged. There is no --json flag.

Example output:

```text
#57 is ready for review https://github.com/owner/repo/pull/57
```

### amf pr draft

```text
amf pr draft <N>
```

Converts an open PR to a draft using GitHub GraphQL.
Closed or merged PRs fail. No command-specific flags.
Prints #N is now a draft <url>, or #N is already a draft.
Leaves linked issue labels unchanged. There is no --json flag.

Example output:

```text
#57 is now a draft https://github.com/owner/repo/pull/57
```

### amf issue run and amf issue watch

These are aliases of `amf run` and `amf watch`: all flags, defaults and
output are identical. Use `amf help issue run` or `amf help issue watch`.
See [Run and watch](#run-and-watch), [Choosing the agent](#choosing-the-agent),
[Merge methods](#merge-methods), and [Logs](#logs) for flags and output.
Shared flags: `--agent claudecode|codex|opencode` selects one agent with no
fallback (default uses configured selection); `--agent-timeout` sets a Go
duration per agent run (default `AMF_AGENT_TIMEOUT` or `60m`, range `1m` to
`24h`), with deprecated alias `--timeout`. `--eval-rounds` defaults to 2 and accepts nonnegative integers.
`--draft` defaults off and cannot be combined with `--merge`.
`--merge[=merge|squash|rebase]` defaults off; bare `--merge` uses a merge
commit. `--log-level` selects debug, info, warn or error (default info in the
terminal, debug in the event log, or `AMF_LOG_LEVEL`). `--log-file PATH`
overrides the event log path; `off` disables it.

Run alone takes `--dry-run` (default off), making only local changes.
Watch alone takes `--interval` (default `60s`, must be positive), `--label` (default
`amf:ready`) and repeatable `--exclude-label` (default none).
Neither supports `--json`. Output includes progress and a final summary,
for example `amf: summary: 1 issues (1 done, 0 failed, 0 skipped); 12 GitHub calls; log …`;
counts and log path depend on the run.

### Group help

`amf help issue help` and `amf help pr help` print their group's command
list, just like `amf issue help` and `amf pr help`; they take no flags.

## Flag conventions

Every flag has a long form, `--full-word` in kebab-case, and that is what the
examples in these docs use. Common flags also have a one-letter short form,
shown first in each command's help (`-a, --agent <name>`). A letter keeps its
meaning on every command that has the flag, and no command has two flags with
the same letter. `amf help flags` prints this table; `amf <command> --help`
(or `-h`, or `amf help <command>`) lists each command's flags in one column as
`-x, --long <value>  description (default ...)`.

Durations are Go durations: `30s`, `5m`, `1h30m`, `2h`.

| Short | Long | Commands |
|---|---|---|
| `-a` | `--agent` | run, watch, auto |
| `-b` | `--body` | issue create |
| `-B` | `--no-browser` | auth login |
| `-C` | `--client-id` | auth login |
| `-c` | `--check` | update |
| `-c` | `--comment` | pr close |
| `-c` | `--continue` | auto |
| `-d` | `--delete-branch` | pr merge, pr close |
| `-e` | `--eval-rounds` | run, watch, auto |
| `-f` | `--file` | issue create |
| `-f` | `--force` | run, auth token |
| `-h` | `--help` | every command |
| `-H` | `--host` | auth login, auth status, auth logout, auth token |
| `-i` | `--interval` | watch |
| `-i` | `--issue` | pr list |
| `-j` | `--json` | issue list, pr list, pr sweep, pr view, memory |
| `-l` | `--label` | issue create, issue list, watch |
| `-L` | `--limit` | issue list, pr list, memory |
| `-m` | `--merge` | run, watch, auto, pr sweep |
| `-n` | `--dry-run` | run, auto, pr sweep |
| `-o` | `--scopes` | auth login |
| `-r` | `--rebase` | pr merge |
| `-s` | `--squash` | pr merge |
| `-s` | `--state` | issue list, pr list |
| `-S` | `--search` | issue list |
| `-t` | `--agent-timeout` | run, watch, auto |
| `-u` | `--update-branch` | pr merge |
| `-w` | `--wait-for-checks` | auto, pr merge, pr sweep |
| `-x` | `--exclude-label` | watch |
| `-y` | `--approve` | auto |

How values are written:

- A value follows the flag as the next word or after `=`: `--agent codex`,
  `--agent=codex`, `-a codex` and `-a=codex` are the same.
- A boolean flag takes no value (`--dry-run`, `-n`), or `=true` / `=false`.
- `--merge` takes an optional method. `--merge` (or `-m`) alone is a merge
  commit; `--merge=squash`, `--merge squash`, `-m squash` and `-m=squash` all
  squash. Only the words `merge`, `squash` and `rebase` are taken as the
  method, so `amf run -m 42` is a merge commit for issue 42.
- Short forms can't be bundled: write `-n -f`, not `-nf`.

Flags with no short form are the rarer ones, spelled out to stay clear:
`--draft`, `--log-level`, `--log-file`, `--stale-timeout`, `--max-issues`,
`--checkpoint-after`, `--no-checkpoint`, `--abort`, `--close-issues`,
`--no-label`, `--name-only`, `--fetch`, `--full`, `--memory-backend`,
`--memory-dir` and `amf update --version` (whose letter would read as
`amf --version`).

The global path flags `--dir <folder>`, `--env-file <file>` and
`--clone-dir <folder>` work on any command, anywhere before `--`, and have no
short form so they never collide with a command's letters (`-d` is
`--delete-branch`, `-f` is `--file` or `--force`). Each command's help lists
them under "Global flags".

Older spellings keep working, though help shows only the forms above. Go's
flag parser accepts one or two dashes for any flag, so `-agent` and `--a`
work; `amf issue create`'s old `-b` and `-f` are now the short forms of
`--body` and `--file`; and `--timeout` is a hidden, deprecated alias of
`--agent-timeout`.

## amf auth

```text
amf auth login [--host host] [--client-id id] [--scopes "repo"] [--no-browser]
amf auth status [--host host]
amf auth logout [--host host]
amf auth token [--host host] [--force]
```

amf needs a GitHub token. Every command that talks to GitHub takes the first of:

1. `GITHUB_TOKEN`, then `GH_TOKEN`, from the environment or `.env` (`--env-file`)
2. `gh auth token --hostname <host>`, if the GitHub CLI is on `PATH` (3 second
   timeout; any error is ignored)
3. the token `amf auth login` saved for the host

With none, it stops with `GITHUB_TOKEN is not set, and neither gh nor amf auth
login has a token: run amf auth login, or set GITHUB_TOKEN`. `amf doctor`, `amf
auth status` and the `config.load` debug event say which source was used, never
the token.

- `amf auth login` runs GitHub's OAuth device flow: it prints a one-time code
  and https://github.com/login/device, opens that page (`--no-browser` skips
  it), polls until you authorize amf (honoring `interval` and `slow_down`;
  `access_denied` and `expired_token` stop it), checks the token with
  `GET /user`, prints the login and saves the token. `--scopes` defaults to
  `repo`; add `workflow` (`--scopes "repo workflow"`) to let amf merge pull
  requests that change `.github/workflows`.
- `amf auth status` shows the host, the token source, the token type (from its
  prefix), the account and the scopes from GitHub's `X-OAuth-Scopes` header.
- `amf auth logout` deletes the saved token for the host.
- `amf auth token` prints the token for scripts (`GH_TOKEN=$(amf auth token)`),
  and refuses when stdout is a terminal unless `--force` is given.

The host defaults to `AMF_API_URL`'s (`github.com` for `https://api.github.com`).
`--host ghe.example.com` targets GitHub Enterprise Server: the device flow at
`https://ghe.example.com/login/...` and the API at `https://ghe.example.com/api/v3`.

**Where tokens are stored.** `hosts.json` (mode 0600, folder 0700), keyed by
host, in `AMF_CONFIG_DIR` or else `amf` in the user config folder
(`~/.config/amf` on Linux, `~/Library/Application Support/amf` on macOS,
`%AppData%\amf` on Windows). This is the one deliberate exception to
[paths defaulting to the current directory](#paths-default-to-the-current-directory):
a credential must never land in a checkout an agent works in. The OS keychain
is not used yet (future work).

**The OAuth app.** `amf auth login` needs the client ID of a GitHub OAuth app
with the device flow enabled, from `--client-id`, else `AMF_GITHUB_CLIENT_ID`,
else one built into the release (`-ldflags "-X main.defaultClientID=<id>"`). To
register one:

1. GitHub **Settings > Developer settings > OAuth Apps > New OAuth App**
   (https://github.com/settings/applications/new).
2. Any name, homepage URL and authorization callback URL (e.g. the repo's URL);
   the device flow doesn't use the callback.
3. Check **Enable Device Flow** and register the app.
4. Copy the **Client ID**. No client secret is needed.
5. Run `amf auth login --client-id <id>`, or set `AMF_GITHUB_CLIENT_ID=<id>`.

## Flag reference

Generated from each command's `--help`; `amf init`,
`amf version`, `amf pr reopen`, `amf pr ready` and `amf pr draft` take only
`-h, --help`.

### amf auth login

| Flag | What it does |
|---|---|
| `-C, --client-id <id>` | the OAuth app's client id (default: AMF_GITHUB_CLIENT_ID) |
| `-H, --host <host>` | the GitHub host (default: AMF_API_URL's, github.com) |
| `-B, --no-browser` | don't open the browser; just print the link |
| `-o, --scopes <scopes>` | the OAuth scopes to ask for, separated by spaces or commas (default repo) |

### amf auth status, amf auth logout

| Flag | What it does |
|---|---|
| `-H, --host <host>` | the GitHub host (default: AMF_API_URL's, github.com) |

### amf auth token

| Flag | What it does |
|---|---|
| `-f, --force` | print the token even to a terminal |
| `-H, --host <host>` | the GitHub host (default: AMF_API_URL's, github.com) |

### amf issue create

| Flag | What it does |
|---|---|
| `-b, --body <text>` | text of the issue body |
| `-f, --file <path>` | read the issue body from the file at path (- for stdin) |
| `-l, --label <name>` | label that marks the issue for amf (default amf:ready) |
| `--no-label` | don't label the issue for amf |

### amf issue list

| Flag | What it does |
|---|---|
| `-j, --json` | print a JSON array of number, title, state, url and labels |
| `-l, --label <name>` | only issues with this label, e.g. amf:ready |
| `-L, --limit <n>` | print at most N issues, 0 means no limit |
| `-S, --search <text>` | only issues whose title contains this text, ignoring case |
| `-s, --state <state>` | open, closed or all (default open) |

### amf issue label

| Flag | What it does |
|---|---|
| `--add <value>` | add this label (repeatable, or comma-separated) |
| `--remove <value>` | remove this label (repeatable, or comma-separated) |

### amf doctor

| Flag | What it does |
|---|---|
| `--fix-labels` | relabel issues whose amf status label disagrees with their pull requests |

### amf run

| Flag | What it does |
|---|---|
| `-a, --agent <name>` | agent for issues without a pipeline: claudecode, codex or opencode with no fallback (default: AMF_PRIMARY_AGENT, codex, then AMF_SECONDARY_AGENT, claudecode, if it fails) |
| `-t, --agent-timeout <duration>` | maximum time each agent run may take (Go duration, e.g. 90m or 1h30m) (default 1h) |
| `--draft` | open new pull requests as drafts (a retry leaves an open PR a draft or ready for review as it was) |
| `-n, --dry-run` | make the change on the local branch only: no label, comment, push, PR or merge |
| `-e, --eval-rounds <n>` | how many times a pipeline's eval stage may send the change back to code (default 2) |
| `-f, --force` | run even if the issue already has an open PR or an amf/issue-N branch, updating that PR |
| `--log-file <path>` | where the event log goes (default .amf/runs/<run_id>/events.jsonl; "off" for none) |
| `--log-level <level>` | debug, info, warn or error, for the terminal and the event log (default info, and debug in the event log) |
| `--memory-backend <backend>` | where amf keeps the repo's memory: sqlite, markdown or logfiles (default sqlite) |
| `--memory-dir <dir>` | directory the amf-memory folder goes in (default: the current directory) |
| `-m, --merge[=<method>]` | merge each PR amf opens or updates: --merge (or -m) alone for a merge commit, or a method after = or as the next word: --merge=squash, -m squash, -m rebase |

### amf watch

| Flag | What it does |
|---|---|
| `-a, --agent <name>` | agent for issues without a pipeline: claudecode, codex or opencode with no fallback (default: AMF_PRIMARY_AGENT, codex, then AMF_SECONDARY_AGENT, claudecode, if it fails) |
| `-t, --agent-timeout <duration>` | maximum time each agent run may take (Go duration, e.g. 90m or 1h30m) (default 1h) |
| `--draft` | open new pull requests as drafts (a retry leaves an open PR a draft or ready for review as it was) |
| `-e, --eval-rounds <n>` | how many times a pipeline's eval stage may send the change back to code (default 2) |
| `-x, --exclude-label <name>` | skip issues with this label, leaving them as they are (repeatable: any of them skips) |
| `-i, --interval <duration>` | how often to poll GitHub (Go duration, e.g. 30s or 5m) (default 1m) |
| `-l, --label <name>` | process open issues with this label (default amf:ready) |
| `--log-file <path>` | where the event log goes (default .amf/runs/<run_id>/events.jsonl; "off" for none) |
| `--log-level <level>` | debug, info, warn or error, for the terminal and the event log (default info, and debug in the event log) |
| `--memory-backend <backend>` | where amf keeps the repo's memory: sqlite, markdown or logfiles (default sqlite) |
| `--memory-dir <dir>` | directory the amf-memory folder goes in (default: the current directory) |
| `-m, --merge[=<method>]` | merge each PR amf opens or updates: --merge (or -m) alone for a merge commit, or a method after = or as the next word: --merge=squash, -m squash, -m rebase |
| `--stale-timeout <duration>` | put amf:in-progress issues no amf works on back to amf:ready after this long, as a Go duration (default AMF_STALE_TIMEOUT, else max(2x agent timeout, 2h); off disables) |

### amf auto

| Flag | What it does |
|---|---|
| `--abort` | abandon the saved run by removing its state |
| `-a, --agent <name>` | agent for issues without a pipeline: claudecode, codex or opencode with no fallback (default: AMF_PRIMARY_AGENT, codex, then AMF_SECONDARY_AGENT, claudecode, if it fails) |
| `-t, --agent-timeout <duration>` | maximum time each agent run may take (Go duration, e.g. 90m or 1h30m) (default 1h) |
| `-y, --approve` | approve the checkpoint without prompting |
| `--checkpoint-after <n>` | checkpoint after this many merges (must be positive) (default 1) |
| `--close-issues` | with --abort, close the created, unmerged issues first |
| `-c, --continue` | approve the checkpoint and resume the saved amf auto run |
| `-n, --dry-run` | plan only: print the issues amf would create, and create nothing |
| `-e, --eval-rounds <n>` | how many times a pipeline's eval stage may send the change back to code (default 2) |
| `--log-file <path>` | where the event log goes (default .amf/runs/<run_id>/events.jsonl; "off" for none) |
| `--log-level <level>` | debug, info, warn or error, for the terminal and the event log (default info, and debug in the event log) |
| `--max-issues <n>` | plan at most this many issues (1 to 10) (default 5) |
| `--memory-backend <backend>` | where amf keeps the repo's memory: sqlite, markdown or logfiles (default sqlite) |
| `--memory-dir <dir>` | directory the amf-memory folder goes in (default: the current directory) |
| `-m, --merge[=<method>]` | how to merge each PR: a merge commit, or a method after = or as the next word: --merge=squash, -m squash, -m rebase (default merge) |
| `--no-checkpoint` | merge all issues without a checkpoint or approval |
| `-w, --wait-for-checks` | wait up to 5 minutes for completed checks before the checkpoint |

### amf memory sync

| Flag | What it does |
|---|---|
| `--full` | with sync, reread every issue and PR instead of only what changed |
| `-j, --json` | print JSON |
| `-L, --limit <n>` | how many results similar prints (default 5) |
| `--memory-backend <backend>` | where amf keeps the repo's memory: sqlite, markdown or logfiles (default sqlite) |
| `--memory-dir <dir>` | directory the amf-memory folder goes in (default: the current directory) |

### amf memory similar

| Flag | What it does |
|---|---|
| `--full` | with sync, reread every issue and PR instead of only what changed |
| `-j, --json` | print JSON |
| `-L, --limit <n>` | how many results similar prints (default 5) |
| `--memory-backend <backend>` | where amf keeps the repo's memory: sqlite, markdown or logfiles (default sqlite) |
| `--memory-dir <dir>` | directory the amf-memory folder goes in (default: the current directory) |

### amf memory show

| Flag | What it does |
|---|---|
| `--full` | with sync, reread every issue and PR instead of only what changed |
| `-j, --json` | print JSON |
| `-L, --limit <n>` | how many results similar prints (default 5) |
| `--memory-backend <backend>` | where amf keeps the repo's memory: sqlite, markdown or logfiles (default sqlite) |
| `--memory-dir <dir>` | directory the amf-memory folder goes in (default: the current directory) |

### amf pr list

| Flag | What it does |
|---|---|
| `-i, --issue <n>` | only pull requests opened for issue N, 0 means no filter |
| `-j, --json` | print a JSON array of number, title, state, draft, url, head, base and issue |
| `-L, --limit <n>` | print at most N pull requests, 0 means no limit |
| `-s, --state <state>` | open, closed, merged or all (default open) |

### amf pr merge

| Flag | What it does |
|---|---|
| `-d, --delete-branch` | delete the head branch after merging, if it is in this repository |
| `-r, --rebase` | rebase and merge |
| `-s, --squash` | squash and merge |
| `-u, --update-branch` | first merge the base branch into the head if it is behind |
| `-w, --wait-for-checks` | wait for the head's checks to pass before merging (up to 30 minutes) |

### amf pr view

| Flag | What it does |
|---|---|
| `-j, --json` | print one JSON object |

### amf pr diff

| Flag | What it does |
|---|---|
| `--name-only` | print one changed path per line instead of the diff |

### amf pr close

| Flag | What it does |
|---|---|
| `-c, --comment <text>` | post a comment on the pull request before closing |
| `-d, --delete-branch` | delete the head branch after closing, if it is in this repository |

### amf pipeline

| Flag | What it does |
|---|---|
| `--fetch` | also download and check each stage's skill |

### amf update

| Flag | What it does |
|---|---|
| `-c, --check` | only report whether an update is available |
| `--version <tag>` | release to install, e.g. v0.0.7 (default: latest) |

## Run and watch

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
amf run 42 --dry-run        # make the change locally and show the PR, touching nothing on GitHub
```

For `run` and `watch`, [`--merge[=merge|squash|rebase]`](#merge-methods)
selects how each PR is merged; without it, PRs stay open for review.

## Auto: one sentence to merged PRs

```sh
amf auto "Add a dark mode toggle to the settings page"
amf auto --dry-run "..."    # print the plan, create nothing
amf auto --continue         # approve the checkpoint and resume
amf auto status             # inspect saved progress without doing work
amf auto --abort            # forget the run, leaving GitHub issues unchanged
amf auto --abort --close-issues  # close unmerged issues, then forget the run
```

`amf auto` asks the agent to plan the sentence into at most `--max-issues`
issues (default 5, at most 10), each with a title, scope and acceptance
criteria. A plan with more issues, or an unusable one, is an error and creates
nothing. The issues are created labeled `amf:auto` only, their URLs are
printed, and `amf:ready` is added immediately before each issue runs. They
run one at a time like `amf run`, each PR merged
([`--merge=merge`](#merge-methods) by default, or `--merge=squash` /
`--merge=rebase`).

By default, after the first merge amf stops at a checkpoint and shows the PR, its merge
commit, the agent and the checks. On a terminal it asks for approval; on
no, the rest stay open and labeled. Without a terminal it saves its state to
`.amf/auto/state.json` and exits; `amf auto --continue` is the approval and
runs the rest. If the first merge fails, auto stops. A failed later issue
stops the run too; `--continue` retries it. While state exists, a new
`amf auto` is refused. The remaining issues stay labeled `amf:auto` only,
so the default `amf watch` does not pick them up while auto waits.

Use `--checkpoint-after N` (positive, default 1) to delay the single checkpoint
until N PRs have merged. It shows the latest merge and only appears if issues
remain. `--approve` accepts it without prompting, even without a terminal.
`--no-checkpoint` skips it entirely, merging all issues without recording
approval; recovery progress is still saved until the run finishes.

Checks are fetched once by default. Add `--wait-for-checks` to poll the merge
commit every 10 seconds for up to 5 minutes, waiting for checks to appear and
all complete. It reports pass/fail or timeout before approval; failed or
unavailable checks can still be approved. Cancellation stops without approval.
The checkpoint threshold and the skip/wait settings persist across retries.
`--continue` approves only once the saved threshold has been reached; before
that, it resumes work toward the checkpoint. A reached checkpoint is shown
again, including the CI wait if enabled, before `--continue` approves it.

`amf auto status` prints the saved sentence, merge method and every planned
issue's title, status, PR URL and commit, marking issues not yet created.
It reads local state without contacting GitHub, running agents or creating
logs. With no saved run it prints `No amf auto run saved.` and exits 0.

To abandon a run, stop any active auto process, then run `amf auto --abort`.
It removes `.amf/auto/state.json`, allowing a new sentence to start cleanly,
and leaves GitHub issues unchanged. Add `--close-issues` to first close the
created, unmerged issues in the saved plan. If any close fails, state stays
in place; retry the same command after fixing the cause. Already merged
issues and planned issues not yet created are skipped. `--close-issues`
requires `--abort`; status and abort cannot be combined with execution flags
or a sentence. Neither command creates a checkout: outside a checkout they
read configuration to find state under `./<repo name>/.amf/auto/state.json`.
Plain abort also removes corrupt or unsupported state; with no saved run,
abort prints the same no-run message and exits 0.

Add `amf:ready` to any remaining open issues to let watch process them.
Events are logged as `auto.plan`,
`auto.checkpoint`, `auto.approved`, `auto.declined`, `auto.resume`,
`auto.stopped` and `auto.done`.

## Choosing the agent

Without a pipeline, each issue gets one agent run: the primary agent,
`AMF_PRIMARY_AGENT` (default `codex`). If it fails (it isn't ready, exits
with an error, times out, or makes no change), amf runs the secondary,
`AMF_SECONDARY_AGENT` (default `claudecode`), in a clean worktree reset to
the default branch, and the PR and issue comment say which agent made the
change. If both fail, the error names both. Set `AMF_SECONDARY_AGENT=none`
for no fallback. Either takes `claudecode` (or `claude`), `codex` or
`opencode`; [Configuration (.env)](../README.md#configuration-env) lists both.

**Changed:** amf used to try claude first; the default order is now codex,
then claude. Set `AMF_PRIMARY_AGENT=claudecode` and
`AMF_SECONDARY_AGENT=codex` for the old order.

If neither is ready, amf uses the first of these that's ready:

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
amf: issues without a pipeline run claude (primary codex, secondary claudecode); codex is not usable: codex has no OPENAI_API_KEY
```

To pick one yourself, pass `--agent claudecode` (or `claude`), `--agent codex`
or `--agent opencode` to `amf run` or `amf watch`. A named agent must be
installed but needs no key, so `--agent claudecode` runs Claude on its own
login. amf never falls back from a named agent to another. A pipeline's
per-stage agent wins over both `--agent` and the primary/secondary settings. If no agent
is ready, amf leaves the issue on `amf:ready` and `watch` tries again on its
next poll. `amf doctor` shows which agent amf would use, and warns if
`OPENCODE_MODEL` isn't one opencode lists. With `OPENCODE_API_KEY` set, it
also asks OpenCode which plans the key grants (pay-as-you-go, Go) and fails
if the key is rejected or an `opencode-go/` model's workspace isn't
subscribed to Go. A repo with a pipeline ignores
`--agent`, because the pipeline names each stage's agent.

## What `amf run N` does

Without a pipeline (see below), amf runs one agent once. With Claude Code:

1. Checks the issue isn't already turned into a PR: with an open PR from
   `amf/issue-N` it labels the issue `amf:pr-open` and stops; with an
   `amf/issue-N` branch on origin and no open PR it stops too (unless the
   issue is `amf:failed`, which is a retry). `--force` skips this check.
   Then labels the issue `amf:in-progress` and removes `amf:ready`.
2. Fetches the default branch and creates a worktree at
   `.amf/worktrees/issue-N` on branch `amf/issue-N` from `origin/<default>`.
3. Runs `claude -p --dangerously-skip-permissions --output-format stream-json --verbose` in the
   worktree with the issue as the prompt. Claude is told to make the change,
   run the project's tests/build, and not touch git. Output streams to
   `.amf/logs/issue-N.log` as it works, one JSON message per line
   (`tail -f` it to follow along). Each agent run may take at most 60 minutes
   (`--agent-timeout`; amf then kills the agent and everything it spawned, or
   `AMF_AGENT_TIMEOUT` sets it without a flag; see [Configuration (.env)](../README.md#configuration-env)).
   Codex and opencode run the same way, with the commands in
   [docs/SPECS.md](SPECS.md#agents).
4. Commits the changes (`Fix #N: <title>`), fails if there are none, and runs
   `git push --force -u origin amf/issue-N` with your own git credentials.
   If the change touches anything under `.github/`, amf refuses to push it
   and the issue fails, since a pushed workflow change runs with your
   Actions secrets before anyone reviews it.
5. Opens a PR with the agent's final message as the body plus `Closes #N`, as
   a draft with `--draft` (default off; `amf run` and `amf watch` take it). On
   a `--force` retry whose PR is still open, it updates that PR's title and body
   instead, and leaves it a draft or ready for review as it was.
6. Comments the PR link on the issue, labels it `amf:pr-open` (a draft PR
   counts; `amf:done` once `--merge` merged it), removes the worktree.

A `--force` run (or `amf auto`) on an issue whose open PR is behind the
default branch (GitHub's compare reports `behind_by` > 0) doesn't rebuild the
change. amf checks out `amf/issue-N` from origin in the worktree and runs
`git merge origin/<default>`:

- a clean merge is pushed (without `--force`), with no agent run;
- on conflicts, one agent (the pipeline's code stage, else the first agent
  amf would run) gets a prompt naming only the conflicted files and resolves
  them; amf fails the run if conflict markers or unmerged paths remain,
  otherwise commits the merge and pushes it;
- on conflicts with no usable agent, amf comments on the PR listing the
  conflicted files, leaves the branch unchanged and fails the run.

The `.github/` refusal applies to the merged branch too. Then the issue is
labeled and commented, and `--merge` merges, as in steps 5–6. An open PR
that is up to date is rebuilt as above. Close the PR to force a rebuild of a
stale one.

With [`--merge`](#merge-methods), amf also merges the PR before commenting,
with a merge commit (or `--merge=squash`, `--merge=rebase`), pinned to the
commit it pushed. It doesn't wait for checks, so with required checks or
reviews GitHub refuses and the PR stays open; amf comments why, and
`amf run` exits 1. amf never
merges a change to the top-level `AGENTS.md`, which holds the pipeline.
`--merge` can't be combined with `--draft`, since GitHub doesn't merge drafts.

With `--dry-run`, `amf run N` only reads from GitHub. It leaves the issue's
labels alone, runs the agent (or the pipeline) and commits on the local
`amf/issue-N` branch as usual, then prints the diff stat and the PR it would
open or update (and whether `--merge` would merge it) instead of pushing.
Nothing is labeled, commented, pushed, opened or merged. See the change with
`git diff origin/main amf/issue-N`. The agent still runs with every
permission, so the trust rules below still apply.

On any failure the issue gets `amf:failed` plus a comment with the reason:
each agent amf tried, in order, with its exit code and duration, whether
the fallback agent ran and if not why, and the last 40 lines of the agent's
log, redacted, with the log's path. An issue that already had an open PR
keeps `amf:pr-open` instead (and one whose PR GitHub shows merged, `amf:done`): a failed retry
never marks it `amf:failed`.
The worktree is kept for inspection. A run that hit the agent timeout
loses only the worktree: amf comments with how to retry with more time and
keeps the logs and partial output. Re-running `amf run N` starts again from a
fresh worktree.

## Merge methods

`amf run N --merge=METHOD`, `amf watch --merge=METHOD` and
`amf auto "sentence" --merge=METHOD` select the same three methods.
Bare `--merge` means `--merge=merge`. `run` and `watch` leave PRs open by
default; `auto` always merges, using a merge commit unless you select another
method. Give the method after `=` or as the next word: `--merge=squash`,
`--merge squash` and `-m squash` are the same.

These diagrams start from the same history: `main` is the default branch,
and `C` and `D` are the PR's commits, branched from `A`:

```text
A---B       main
 \
  C---D     PR
```

**Merge commit (`--merge` or `--merge=merge`)** keeps the original commits
and adds a merge commit `M` with two parents:

```text
A---B---M   main
 \     /
  C---D
```

Preserves the full history and an explicit PR boundary, but adds a merge
commit and a branch in the history.

**Squash (`--merge=squash`)** combines the PR's changes into one new commit
`S` on the default branch:

```text
A---B---S   main (S combines C and D)
```

Keeps history linear with one commit per PR, making the whole change easy
to revert. The PR's individual commits are lost from the default branch's
history.

**Rebase (`--merge=rebase`)** replays the PR's commits individually on top
of the default branch, without a merge commit:

```text
A---B---C'---D'   main
```

Keeps history linear and retains the individual changes, but GitHub gives
them new commit SHAs (`C'`, `D'`) and there is no merge commit marking the PR
boundary. Best when the individual commits are worth keeping.

These are [GitHub's merge methods](https://docs.github.com/en/pull-requests/reference/pull-request-merges).
The repository must allow the selected method; branch protection and amf's
merge safety rules still apply.

## Logs

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

- `--log-level debug|info|warn|error` (or `AMF_LOG_LEVEL`, see [Configuration (.env)](../README.md#configuration-env)) sets what the
  terminal and the log show. By default the terminal shows `info` and the
  log keeps `debug`.
- `--log-file PATH` writes the log somewhere else, and `--log-file off`
  writes none.
- Everything amf writes is **redacted**: tokens and keys from `.env`,
  well-known secret formats, credentials in URLs, emails and your home
  directory are masked as `[REDACTED:<kind>]` on the terminal, in every log,
  in PostgreSQL and in the comments and PRs amf posts. It's pattern
  matching, so a secret in an unusual format can still get through; see
  [docs/SPECS.md](SPECS.md#security-model).

`amf doctor` checks that `.amf/runs/` is writable.

## Logs in PostgreSQL

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
the lines in memory and retries. amf has no PostgreSQL driver, so it speaks the
PostgreSQL protocol itself: TLS (`sslmode`), and password, MD5 or
SCRAM-SHA-256 logins. `amf doctor` checks the connection. Agents' output can
include your code, so restrict who can read `amflogs`.

## Repo memory

amf keeps a memory of each repo's issues, pull requests and runs, so a new
issue can be pointed at similar past work.

```sh
amf memory sync                  # backfill from GitHub; later syncs fetch only what changed (--full rereads all)
amf memory similar 42            # past work like issue 42, best first
amf memory similar "text" --json # ...or like any text, as JSON
amf memory show 42 --json        # issue 42, its PRs and every run
```

`amf run`, `watch` and `auto` record every attempt (command, start and
finish, outcome, agent, exit code, PR, error and a redacted log tail), on a
best-effort basis: a memory failure is a warning and never fails a run.
`--dry-run` records nothing. When `run` or `watch` picks up an issue, and
when `auto` plans one, amf logs similar past work (event `memory.similar`)
and adds a short "Similar past work" note to the agent's prompt. A close
match to merged work also logs `memory.duplicate`, a warning only: amf never
closes an issue as a duplicate.

PRs are linked to their issue by an `amf/issue-N` branch or a
`Closes/Fixes/Resolves #N` line in the body.

### Backends

`--memory-backend` on `run`, `watch`, `auto` and `amf memory` picks the store;
there is no environment variable. With no flag amf uses SQLite.

| Backend | Files in the repo's folder |
|---|---|
| `sqlite` (default) | `memory.db`: versioned schema, FTS5 search |
| `markdown` | `issues/N.md`, `prs/N.md`, `runs/<id>.md` with front matter, plus `index.md` and `index.json` |
| `logfiles` | append-only `issues.jsonl`, `prs.jsonl`, `runs.jsonl`, `sync.jsonl`; the latest line per record wins |

All three keep the same records and links. Markdown and log files are only
ever written when that backend is chosen on the command line. Each backend
is a separate store: memory synced into one isn't seen by another.

### Where memory lives

`--memory-dir <dir>` (default: the current directory; a relative path is taken
from it, and a missing one is created) holds
`D/amf-memory/<host>/<owner>/<repo>/`. The repo key is the lowercased
host/owner/name from `GITHUB_REPO` (for example
`github.com/vicgupta/auto-mini-factory`). Every store records its key and
refuses to open for another repo, and every query is filtered by it, so
repos sharing a memory dir never see each other's work. amf adds the
`amf-memory` folder to `.git/info/exclude` when it lies inside the checkout,
and leaves it out of its own commits.

## Pipelines: plan → code → eval

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

Starter skills are in [`skills/`](../skills): [plan](../skills/plan/SKILL.md),
[code](../skills/code/SKILL.md) and [eval](../skills/eval/SKILL.md). Write your own
by copying one; amf's prompt already fixes each stage's inputs and output
format, so a skill only needs to say how to do the work.

## Labels

An issue moves `amf:ready` → `amf:in-progress` → `amf:pr-open` (PR open,
not merged), `amf:done` (PR merged by `--merge`) or `amf:failed`. `amf watch`
creates `amf:pr-open` if the repo lacks it; `amf init` creates all five.
`amf:done` is set only after amf confirms the PR is merged. Each poll,
`watch` also moves up to five open `amf:failed` issues whose `amf/issue-N`
PR is open with passing checks to `amf:pr-open`.
`amf issue` adds `amf:ready`, and `amf watch` picks up open `amf:ready` issues
oldest first. To retry an issue, add `amf:ready` again or run `amf run N`.
`amf issue label N --add L --remove L` changes labels by hand (adding a
status label drops the old one), and `amf doctor` warns about status labels
that disagree with the issue's PRs; `amf doctor --fix-labels` corrects them.
If amf is killed mid-run and an issue is stuck on `amf:in-progress`, run
`amf run N`, or let `amf watch` recover it: each poll it puts
`amf:in-progress` issues no amf is working on back to `amf:ready` once the
label is older than `--stale-timeout` (`AMF_STALE_TIMEOUT`; default twice
the agent timeout and at least 2h; `off` disables it), using the time the
label was added from the issue's events, else the issue's `updated_at`. An
issue whose PR is open becomes `amf:pr-open` instead.

## The repo lock

`amf run`, `amf watch` and `amf auto` take `.amf/watch.lock` in the checkout
before they work, written atomically with the PID, host, command and start
time, and refreshed every minute. If another live amf holds it, the command
exits with an error naming it. A lock is stale, and taken over, when its PID
is dead on the same host or it hasn't been refreshed for 10 minutes. It is
removed on exit, including Ctrl-C and SIGTERM.

Issues labeled `amf` by amf v0.0.4 and earlier aren't picked up any more.
Relabel them `amf:ready`, or run `amf watch --label amf`.

`.amf/` is added to `.git/info/exclude`, so it never shows up in `git status`.
