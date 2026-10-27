# amf specification

amf (autonomous mini factory) turns labeled GitHub issues into pull requests by
running coding agents headless in a git worktree. By default that's one run of
a single agent: the one `--agent` names, or else Claude Code or Codex if it's
installed and has an API key or token, then opencode. A repo can instead declare a plan → code → eval pipeline in its
`AGENTS.md`, with a skill, agent and model for each stage. amf is a single Go
binary that uses only the standard library, for one developer or a small team
that doesn't want to run a platform.

**Needs:** `git`, a GitHub token for one repository, and an agent amf can
run: `claude` with `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN`, `codex`
with `OPENAI_API_KEY`, or `opencode` (with `OPENCODE_API_KEY` or its own
saved login for a paid `OPENCODE_MODEL`), or any of them named with `--agent`. A
pipeline needs whichever agents it names.

## Issue lifecycle

```
amf:ready ──▶ amf:in-progress ──▶ amf:done    (PR opened, and merged with --merge)
                       └────────▶ amf:failed  (reason commented)
```

- `amf:ready` marks an issue for amf. Adding it again retries an issue that is
  done or failed.
- `amf:done` means amf produced a PR for the issue, a draft one (`--draft`)
  included.
- amf removes `amf:ready` when it starts, and an issue has at most one status
  label at a time.
- amf owns these four labels and every `amf/issue-<N>` branch.

## Commands

| Command | Behavior |
|---|---|
| `amf init` | Writes `./.env` and creates the labels (see below). |
| `amf issue "Title" [-b body \| -f file \| -f -] [--label L] [--no-label]` | Creates an issue labeled `amf:ready` (or `--label`'s label; none with `--no-label`). The body can also come from stdin when it's piped; `-b` and `-f` together are an error, and so is an explicit `--label` with `--no-label`. Prints `#N <url>`. |
| `amf list [--state open\|closed\|all] [--label L] [--limit N] [--json]` | Lists issues, newest first (highest number first), with every page fetched and PRs left out. Columns: `ISSUE`, `STATE` (with `--state all` only), `AMF` (the issue's `amf` and `amf:*` labels, or `-`) and `TITLE`. `--limit N` (default `0`, no limit) prints only the first N issues after filtering; a negative value errors with `--limit must be 0 or more, got N`. Tabs in a title print as spaces. `--state` takes only `open`, `closed` or `all`. Prints `No … in owner/name.` when nothing matches. `--json` (default `false`) prints instead one JSON array and a newline, with no header: one object per issue, in the same order and after the same filters and limit, with exactly `number` (integer), `title`, `state`, `url` (the issue's GitHub page) and `labels` (every label name, in GitHub's order, `[]` when none); titles are kept as GitHub sent them, tabs included. Nothing matching prints `[]`. On an error nothing is printed to stdout, and a failure to write stdout is an error. Needs no git checkout. |
| `amf pr` | Prints the pull request command group's usage to stdout, as `amf pr help`, `amf pr -h` and `amf pr --help` do. An unknown subcommand prints that usage to stderr and exits 2. |
| `amf pr list [--state open\|closed\|merged\|all] [--limit N] [--json]` | Lists pull requests, including drafts, newest first. See [Pull requests](#pull-requests). |
| `amf pr merge <N> [--squash \| --rebase] [--delete-branch]` | Merges a chosen PR (`42` or `#42`); no git checkout needed. See below. |
| `amf pr view <N> [--json]` | Shows one pull request without a git checkout. See below. |
| `amf pr diff <N> [--name-only]` | Prints a pull request's unified diff, or one changed path per line with `--name-only`; no git checkout needed. See below. |
| `amf pr close <N> [--comment text] [--delete-branch]` | Closes a chosen PR (`42` or `#42`); no git checkout needed. See below. |
| `amf pr reopen <N>` | Reopens a chosen closed PR (`42` or `#42`); no git checkout needed. See below. |
| `amf pr ready <N>` | Marks a draft pull request ready for review; no git checkout needed. See below. |
| `amf pr draft <N>` | Converts a ready pull request back to a draft; no git checkout needed. See below. |
| `amf run <N> [--agent A] [--agent-timeout 60m] [--eval-rounds 2] [--dry-run] [--draft] [--merge[=squash\|rebase]] [--log-level L] [--log-file P]` | Processes issue N (`42` or `#42`) once. |
| `amf watch [--agent A] [--interval 60s] [--label amf:ready] [--exclude-label L]... [--agent-timeout 60m] [--eval-rounds 2] [--draft] [--merge[=squash\|rebase]] [--log-level L] [--log-file P]` | Polls open issues with the label (every page), oldest first by issue number (GitHub's `created_at` can tie for issues opened in the same second), and processes them one at a time until interrupted, skipping any issue that has one of the `--exclude-label` labels. |
| `amf pipeline [--fetch]` | Checks the pipeline in `AGENTS.md` of the enclosing checkout, as it is on disk. See below. |
| `amf doctor` | Checks the whole setup, one line per check, changing nothing. See below. |
| `amf update [--version vX.Y.Z] [--check]` | Replaces the running binary with the latest release, or with the given one, which can be a downgrade. Needs no `.env`. See below. |
| `amf version` | Prints `amf <version>`: the release tag, or the module version for `go install`. `amf --version` is the same. |
| `amf help` | Prints the usage, as `-h` and `--help` do. |

- `--agent-timeout` limits each agent run, not the whole issue; the old
  `--timeout` is accepted as a deprecated alias of it. Both must be at least
  1m and at most 24h, and 0 and negatives are errors; every out-of-range or
  unparsable value fails `run` and `watch` at startup, naming where it came
  from, such as `AMF_AGENT_TIMEOUT="5x": not a duration`. Other commands
  ignore `AMF_AGENT_TIMEOUT` (`doctor` checks it), and a flag replaces it, so
  a bad value in `.env` is then never read. `AMF_AGENT_TIMEOUT` (.env or the
  environment) sets the limit without a flag, and a flag overrides it;
  without either, each agent run gets 60 minutes. The limit is per run, not
  per issue: a plan plus eval rounds run under pipelines can take several
  times it. A run's process group gets SIGTERM all together on expiry and, if
  it is still there after amf's 15s grace period, SIGKILL; Ctrl-C kills it the
  same way. On Windows, amf instead ends the run's whole process tree at once
  with `taskkill /T /F`, with no grace period. `--interval` also must be
  positive.
- `--label L` (`watch` only, default `amf:ready`) is the label `watch` polls
  for: each poll lists open issues with that exact label. An empty value
  (`amf watch --label ""`, such as an unset shell variable) fails `watch` at
  startup with `--label must not be empty`, before anything is read or
  fetched, so a typo can't silently widen the poll to every open issue.
- `--exclude-label L` (`watch` only; repeatable, none by default) holds
  issues in the queue: each poll, an issue that `watch` would otherwise work
  on but that has any of these labels (by exact name, as amf matches its own
  labels) is skipped and left as it is, with no label change, comment,
  worktree, agent run, commit, push or PR, while the issues after it go on
  as usual.
  The labels are those the poll's listing returned, so once a hold label is
  removed the issue is picked up on a later poll without restarting `watch`;
  an issue already running isn't stopped. Each value is trimmed of
  surrounding whitespace, duplicates count once, and an empty or blank value
  fails `watch` at startup with `--exclude-label can't be empty`, before
  anything is read or fetched. A value equal to the watched `--label`
  (compared ignoring case, on the trimmed values) would skip every issue, so
  `watch` fails at startup with `--exclude-label "…" is the label watch is
  processing, so every issue would be skipped`, before anything is read or
  fetched. A label the repository doesn't have matches
  nothing, and amf never creates one. `amf run N` doesn't take the flag and
  runs a held issue.
- `--eval-rounds` is how many times a pipeline's eval stage may send the
  change back to the code stage. A negative value is an error.
- `--draft` (default off; `--draft=false` is the same as leaving it out)
  opens each PR that `run` or `watch` creates as a draft, with or without a
  pipeline. A retry that updates the branch's open PR leaves it a draft or
  ready for review as it was, whatever the flag says; amf never converts a
  PR either way; mark a draft ready for review with `amf pr ready`.
- `--merge` makes `run` and `watch` merge each PR they open or update (step
  11 below), without review. Off by default. Bare `--merge` (or
  `--merge=true`, or `--merge=merge`) makes a merge commit; `--merge=squash`
  squashes and `--merge=rebase` rebases, as GitHub's merge button does, and
  `--merge=false` turns it off. The method needs the `=`: `--merge squash`
  fails with `--merge takes its method after =: --merge=squash, not --merge
  squash`, and any other method with `--merge: want merge, squash or
  rebase, got "<value>"`, both at startup. `--merge` with `--draft` fails
  at startup with `--draft and --merge can't be used together: GitHub doesn't
  merge draft pull requests`.
- `--dry-run` (`run` only; default off) makes the change on the local
  `amf/issue-N` branch and prints the pull request amf would open, without
  changing anything on GitHub: no label, comment, push, PR or merge. See
  [Dry run](#dry-run-amf-run---dry-run). `watch` doesn't take it, since it
  would redo every queued issue on each poll.
- **Event log.** Besides its `amf: …` lines on stderr, each `run` and
  `watch` process writes an event log: one JSON object per line, with
  `time`, `level`, `msg`, `run_id` and, for a line about an issue, `issue`.
  It is redacted like everything else (see "Redaction"). The `run_id` is
  the UTC start time and 6 random hex characters, like
  `20261023T222500Z-a1b2c3`. By default the log is
  `.amf/runs/<run_id>/events.jsonl` in the checkout; `--log-file P` writes
  it to P instead (its folder is created), and `--log-file off` writes
  none. Once the checkout is known, amf prints `logging to <path>`; lines
  printed before that are not in the file. Writes are buffered, flushed
  every second and when amf exits, Ctrl-C included. The events are listed
  under [Event log](#event-log).
- **Summary.** After each issue and when `run` or `watch` exits (Ctrl-C
  and failures included), amf writes `.amf/runs/<run_id>/summary.json`
  (none with `--log-file off`; with another `--log-file`, it still goes
  there): `run_id`, `issues` (`done`, `failed`, and `skipped`, each issue
  counted once however many polls skipped it), `github_calls`,
  `ratelimit_lowest` (the lowest `X-RateLimit-Remaining`, or null),
  `git_calls`, `agent_ms` (agent wall time per stage: `plan`, `code`,
  `eval`, or `agent run` without a pipeline), `errors` (warn and error
  events) and `log` (the event log's path). At exit amf also prints
  `summary: N issues (D done, F failed, S skipped); G GitHub calls; log
  <path>`, the last part only with an event log.
- `--log-level` is `debug`, `info`, `warn` (or `warning`) or `error` (case
  is ignored), and so is `AMF_LOG_LEVEL`, which the flag overrides. Either sets the
  level for both the terminal and the event log; without them the
  terminal shows `info` and the event log keeps `debug`. Any other value
  fails `run` and `watch` at startup, naming the flag or variable.
- `--agent` picks the agent for issues without a pipeline: `claudecode`
  (or `claude`), `codex` or `opencode`, ignoring case. A named agent must be
  installed, but needs no key, since it can use its own login. `run` and
  `watch` fail at startup with `--agent <agent>: <path> is not installed`,
  and never fall back to another. Any other value is an error. A repo with a
  pipeline ignores `--agent`, since the pipeline names each stage's agent;
  amf logs `ignoring --agent <agent>` for each such issue.
- **Without `--agent`,** amf picks for each issue the first usable agent of:

  | Agent | Usable when |
  |---|---|
  | `claude` | installed, and `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` is set |
  | `codex` | installed, and `OPENAI_API_KEY` is set |
  | `opencode` | installed, and either `OPENCODE_MODEL` is free (`opencode/big-pickle`, an `opencode/` id ending in `-free`, or another provider's model), `OPENCODE_API_KEY` is set, or opencode has a saved login for the model's provider |

  Keys come from the environment or `.env`; placeholders don't count. An
  installed claude or codex without its key is skipped, even if it has a
  login of its own. opencode's model picks its plan: `opencode/<id>` is a
  free or pay-as-you-go OpenCode Console model, `opencode-go/<id>` an
  OpenCode Go one, and both of the paid kinds need the Console API key.
  Without `OPENCODE_API_KEY`, amf runs `opencode auth list --format json`
  and counts a saved `credential` for `opencode` or `opencode-go` (matching
  the model) as a login. With none usable, the issue isn't touched (see
  step 3 below); amf passes over opencode as `opencode has no
  OPENCODE_API_KEY or saved login for <model>`.
- `watch` logs at startup one line per agent: where it's installed (or `not
  installed`), whether its key is set (naming the one that is; for
  opencode, `needs no key`, `uses its saved login` or `OPENCODE_API_KEY not
  set and no saved login`), for opencode the model it runs with, and
  `usable` or `skipped`. Then it logs which agent issues without a pipeline run, or a
  warning that none is available.

**Exit codes:** 0 on success, 1 on error, 2 on a usage error (no command,
an unknown one, a bad flag or the wrong arguments; no command or an unknown
one also prints the usage to stderr). Errors print as `amf: <error>` on
stderr. SIGINT and
SIGTERM cancel the current step, with the run's process group killed as on
expiry; the issue is still marked failed.

### Pull requests

`amf pr list` reads `.env` like `amf list`, checks that GitHub can access
`GITHUB_REPO` first, and needs no git checkout. It fetches every page of
`GET /repos/{repo}/pulls` with `sort=created` and `direction=desc`, then sorts
by number descending so the highest number is first even when creation times tie.

- `--state` defaults to `open`, including drafts. `merged` requests `closed`
  from GitHub and keeps PRs whose `merged_at` is set; `closed` keeps only
  unmerged closed PRs. `all` keeps everything. Other values fail with
  `--state must be open, closed, merged or all, got "<value>"`.
- Columns are `PR`, `ISSUE`, `STATE`, `TITLE`. `PR` is `#N`. `ISSUE` is `#N`
  only for a head branch named `amf/issue-N` in this repository (a positive
  decimal number without leading zeroes); other branches, forks and deleted
  head repositories show `-`. `STATE` is `merged` when `merged_at` is set,
  otherwise `closed` for a closed PR, otherwise `draft` or `open`. Tabs in
  titles print as spaces.
- `--limit N` defaults to `0` (no limit), and applies after filtering and
  sorting. Negative values fail with `--limit must be 0 or more, got N`.
- `--json` defaults to `false`. It prints one array and a newline with exactly
  `number` (integer), `title`, `state` (as in the table), `draft` (boolean),
  `url` (the PR's GitHub page), `head` and `base` (branch names), and `issue`
  (the associated issue integer or `null`). Titles stay as GitHub sent them,
  including tabs. An empty result is `[]`.
- An empty table prints `No <state> pull requests in owner/name.`, or
  `No pull requests in owner/name.` for `all`. Argument, configuration and API
  errors leave stdout empty, including errors on later pages. A failure to
  write stdout is returned as an error.

### `amf pr merge`

Reads `.env` and the environment and checks repository access, as `amf list`
does. Merges with a merge commit by default; `--squash` squashes and
`--rebase` rebases. Both flags together fail before any GitHub call with
`--squash and --rebase can't be used together`. Exactly one positive PR
number is required. Missing or extra arguments and unknown flags exit 2;
invalid numbers and conflicting method flags exit 1. A bare negative
number is parsed as an unknown flag and exits 2 (use `#-1` to pass it as
a number, which exits 1).

Preflight reads `GET /repos/{repo}/pulls/N`. A merged PR fails with
`#N is already merged`, a closed PR with `#N is closed`, and a draft with
`#N is a draft; mark it ready for review first`, without a merge request.
The merge is pinned to preflight's `head.sha` on every attempt; a missing
SHA fails rather than attempting an unpinned merge. If the branch changes,
GitHub's 409 refusal is reported. Only a 405 whose message contains
`not mergeable` (case insensitive) is retried: three attempts, five seconds
apart, stopping on Ctrl-C. Other errors, including conflicts, required
checks or reviews, and disallowed merge methods, fail immediately with
GitHub's message. These failures exit 1 and leave the PR open.

On success, stdout is `Merged #N <url>`, `Squashed and merged #N <url>` or
`Rebased and merged #N <url>`. `--delete-branch` then deletes the head using
`DELETE /repos/{repo}/git/refs/heads/<branch>`, only when its repository
matches `GITHUB_REPO` (case insensitive). A fork or unavailable head
repository prints a `Skipping branch deletion` note to stderr and succeeds.
If deletion fails, the success line remains on stdout, and the command
exits 1 with `merged #N but could not delete <branch>: <err>`: the merge
has already happened. Otherwise success exits 0. Labels and comments are
left alone. The PR's closing references close issues as usual.

### `amf pr view`

`amf pr view <N> [--json]` reads `.env` like `amf list`, checks that GitHub can
access `GITHUB_REPO` first, fetches `GET /repos/{repo}/pulls/N`, and needs no
git checkout. `N` may be `57` or `#57` and must be positive. Missing, extra or
non-number arguments print `usage: amf pr view <number> [--json]` to stderr
and exit 2, before reading configuration or GitHub.

The first line is `#N <title>`, followed by fields aligned at column 12:

- `state:` is `merged` when `merged_at` is set, otherwise `closed` for a
  closed PR, otherwise `open (draft)` for a draft, otherwise `open`.
- `branch:` is the head branch, ` → ` and the base branch.
- `author:` is the login of the user who opened the PR.
- `issue:` is `#N` for a head branch named exactly `amf/issue-N` with a
  positive decimal number without leading zeroes; any other branch is `-`.
- `mergeable:` is `unknown` when GitHub's `mergeable` is `null` (or
  `mergeable_state` is `unknown`), and the command does not retry. It is
  `no (conflicts)` when `mergeable` is false or `mergeable_state` is `dirty`,
  `yes` when it is true with a `clean` or empty state, and `blocked (<state>)`
  for any other state, such as `blocked`, `unstable` or `behind`.
- `changes:` is `+<additions> −<deletions> in N file` (`files` unless N is 1),
  using a Unicode minus sign.
- `url:` is the PR's GitHub page.

When the body is not empty, a blank line and the body follow, with trailing
newlines removed and one final newline added; an empty body adds nothing.

`--json` prints one object and a newline instead, with exactly `number`
(integer), `title`, `state` (`merged`, `closed` or `open`; drafts stay `open`),
`draft` (boolean), `url`, `head` and `base` (branch names), `author`, `issue`
(the associated issue integer or `null`), `mergeable` (boolean or `null`),
`mergeable_state` (GitHub's value, `""` when absent), `additions`,
`deletions`, `changed_files` and `body`. The body and title are kept as GitHub
sent them, and HTML is not escaped.

GitHub answers 404 both for an issue number and for a number that doesn't
exist; both fail with `#N is not a pull request in owner/name`. On any error
nothing is written to stdout, and a failure to write stdout is an error.

### `amf pr diff`

`amf pr diff <N> [--name-only]` reads `.env` like `amf list`, checks that
GitHub can access `GITHUB_REPO` first, and needs no git checkout. `N` may be
`57` or `#57` and must be positive. Missing, extra or non-number arguments
print `usage: amf pr diff <N> [--name-only]` to stderr and exit 2, before
reading configuration or GitHub.

Without `--name-only`, amf requests `GET /repos/{repo}/pulls/N` with
`Accept: application/vnd.github.diff` and writes GitHub's unified diff to
stdout byte for byte, adding no trailing newline. With `--name-only`, it
requests `GET /repos/{repo}/pulls/N/files`, follows every page, and prints one
changed path per line; a rename prints its new name (`filename`). A number
that isn't a pull request fails with `#N is not a pull request in owner/name`,
as `amf pr view` does.

A diff GitHub refuses as too large (a 406 or 422 whose message contains
`too large`, case insensitive) fails with
`the diff of #N is too large for GitHub to send; try --name-only, or git fetch origin pull/N/head`.

The other GitHub calls are unchanged: they still send
`Accept: application/vnd.github+json`. On any error nothing is written to
stdout, and a failure to write stdout is an error.

### `amf pr close` and `amf pr reopen`

Both read `.env` and the environment and check repository access, as `amf
list` does, and need no git checkout. Exactly one positive PR number is
required; missing or extra arguments and unknown flags exit 2, and an
invalid number exits 1.

`amf pr close` first reads `GET /repos/{repo}/pulls/N`. A merged PR fails
with `#N is merged; it can't be closed`, and an already-closed PR prints
`#N is already closed` and exits 0, both without a PATCH. Otherwise it
closes the PR with `PATCH /repos/{repo}/pulls/N` `{"state":"closed"}` and
prints `Closed #N <url>`. With `--comment text`, the comment is posted to
the issue first, redacted like amf's other comments. With `--delete-branch`,
amf deletes the head branch with
`DELETE /repos/{repo}/git/refs/heads/<branch>` only when its repository
matches `GITHUB_REPO` (case insensitive); a fork or unavailable head
repository prints a `Skipping branch deletion` note to stderr and succeeds.
If deletion fails, the `Closed #N <url>` line stays on stdout and the
command exits 1 with `closed #N but could not delete <branch>: <err>`: the
PR is already closed.

`amf pr reopen` first reads the PR. A merged PR fails with `#N is merged;
it can't be reopened`, and an open PR prints `#N is already open` and
exits 0, both without a PATCH. Otherwise it opens the PR with
`PATCH /repos/{repo}/pulls/N` `{"state":"open"}` and prints `Reopened #N
<url>`. If GitHub refuses (for example, the head branch was deleted), the
command fails with GitHub's message and the PR stays as it was.

Neither command touches the linked issue's labels; re-adding `amf:ready`
to retry an issue is still the user's step.

### `amf pr ready` and `amf pr draft`

Both read `.env` and the environment and check repository access, as `amf
list` does, and need no git checkout. Exactly one positive PR number is
required; missing, extra or non-number arguments print
`usage: amf pr <ready|draft> <N>` to stderr and exit 2, before reading
configuration or GitHub.

GitHub's REST API can't change a PR's draft state (`PATCH /pulls` ignores
`draft`), so both commands read `GET /repos/{repo}/pulls/N` for the PR's
`node_id`, `draft`, `state` and `merged_at`, then apply a GraphQL mutation.
A merged PR fails with `#N is merged` and a closed one with `#N is closed`,
without a mutation.

`amf pr ready` sends `markPullRequestReadyForReview` with the PR's
`node_id`, then prints `#N is ready for review <url>`. A PR that is already
ready prints `#N is already ready for review` and exits 0. `amf pr draft`
sends `convertPullRequestToDraft` and prints `#N is now a draft <url>`; a
PR that is already a draft prints `#N is already a draft` and exits 0.
Either no-op sends no mutation.

The GraphQL endpoint is derived from `AMF_API_URL`: `https://api.github.com`
maps to `https://api.github.com/graphql`, and a GitHub Enterprise Server
base `https://host/api/v3` maps to `https://host/api/graphql`. A GraphQL
response whose `errors` array is non-empty (for example, a token without
permission) fails with its first message and leaves the PR as it was.

### `amf init`

1. Builds `.env` from a template of `GITHUB_TOKEN`, `GITHUB_REPO`,
   `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`,
   `OPENCODE_API_KEY`, `OPENCODE_MODEL`, `AMF_AGENT_TIMEOUT`,
   `AMF_LOG_LEVEL`, `AMF_TRUSTED` and `AMF_POSTGRES_URL`.
   - `OPENCODE_API_KEY`, `OPENCODE_MODEL`, `AMF_AGENT_TIMEOUT`,
     `AMF_LOG_LEVEL` and `AMF_POSTGRES_URL` are written
     commented out, as `# OPENCODE_MODEL=opencode/big-pickle   # opencode/<id> for free and pay-as-you-go models, opencode-go/<id> for Go; 'opencode models' lists them`
     until they have a real value. None is one of the values `init` asks
     you to fill in.
   - Real values from an existing `.env` are kept, and so are any extra
     variables.
   - A missing `GITHUB_REPO` is filled in from the checkout's GitHub `origin`.
   - Anything else gets a placeholder.
2. If the result matches the current `.env`, nothing is written. Otherwise the
   old file is copied to the next free `.env.NNNN` (0001, 0002, …, numbered
   above the highest that exists), and the new file is written atomically with
   permissions 0600.
3. In a git checkout, adds the `.env` and `.env.NNNN` it writes to
   `.git/info/exclude`: `/.env` at the top of the checkout, or
   `/<subdirectory>/.env` when run in a subdirectory.
4. Once the token and repo are usable, checks that the repo exists and creates
   any of the four labels that are missing. Existing labels aren't changed.
   Until then it prints that labels weren't checked yet.

### `amf pipeline`

1. Must run inside a git checkout. Reads `AGENTS.md` at its top level from
   disk, so a change can be checked before it's merged.
2. Without `AGENTS.md`, or without an `## amf` table in it, prints which
   agent amf runs with its built-in prompt (picked as for `--agent` above,
   with the reason each earlier agent was skipped) and exits 0. If none is
   usable it says so and exits 1.
3. Otherwise prints the stages (`STAGE`, `AGENT`, `MODEL`, `SKILL`; an
   opencode stage with no model shows `<OPENCODE_MODEL> (OPENCODE_MODEL)`), then one
   line per agent: `ok` with its path, or `missing`. Agent paths come from
   the environment and `.env`; no token is needed.
4. With `--fetch`, also downloads each skill as `run` does, and prints `ok`
   with its size and SHA-256 (the value for a `#sha256=` pin), or why it
   failed.
5. A table that doesn't parse is an error. Missing agents and failed skills
   end with `the pipeline has N problems` and exit 1.

### `amf doctor`

Prints one line per check, as `<status>  <check>  <detail>`. The status is
`ok`, `warn` (works, but worth knowing), `FAIL`, or `skip` (an earlier check
it depends on failed). Any `FAIL` ends with `doctor found N problems` and
exit 1; warnings alone exit 0. It never clones, creates labels or pushes, and
never prints a secret. The checks, in order:

| Check | `ok` when | Otherwise |
|---|---|---|
| `amf` | The running version is the latest release | `warn`: a newer release (run `amf update`), a development build, or no answer from GitHub |
| `git` | `git` runs; shows its version | `FAIL` |
| `.env` | A `.env` was found where `run` looks; shows its path | `warn` if there is none (the environment is used); `FAIL` if it doesn't parse |
| `GITHUB_TOKEN` | Set; says whether from `.env` or the environment | `FAIL` if unset or a placeholder |
| `GITHUB_REPO` | Set and `owner/name` | `FAIL` |
| `github` | `GET /user` accepts the token; shows the login. A 403 (a token that can't read `/user`) is `ok`, without a login | `FAIL` on 401 or no connection |
| `repo` | The repo is readable; shows its default branch | `FAIL` on 404; `warn` if the account can't push |
| `labels` | All four amf labels exist | `warn`, naming the missing ones |
| `trusted` | `AMF_TRUSTED` is set; lists the logins | `warn` if unset (anyone's issues run), or if it leaves out the token's account, which `amf issue` opens issues as |
| `agent timeout` | Shows the limit each agent run gets: `AMF_AGENT_TIMEOUT` (and from where) or the 60m default | `FAIL` if `AMF_AGENT_TIMEOUT` isn't a duration or is out of range |
| `checkout` | The checkout `run` would use: the enclosing one, or `./<name>` | `warn` if `run` would clone first or origin isn't `GITHUB_REPO`; `FAIL` if `./<name>` isn't a clone or there's no origin |
| `git push` | `git push --dry-run` to `origin` works with the user's git credentials, without prompting | `FAIL`, with git's message |
| `git identity` | git has a name and email to commit with, in the checkout (or here); shows them | `FAIL`, with the `git config` commands to set them |
| `log folder` | `.amf/runs/` in the checkout can be written: doctor writes and removes a file in it, or in the nearest folder above it while it doesn't exist yet | `FAIL`, naming the folder and the error; `skip` without a checkout |
| `pipeline` | Fetches the default branch; shows the pipeline from `AGENTS.md` on it, or that there is none | `FAIL` if the table doesn't parse |
| `skill <stage>` | Each stage's skill downloads as in `run`; shows its size and SHA-256 | `FAIL` |
| `claudecode`, `codex`, `opencode` | Installed; shows the path and `--version`. With no pipeline, the agent amf picks adds `runs issues without a pipeline`, and the `pipeline` line names it | `FAIL` if a stage needs it. With no pipeline: one `FAIL` on `claudecode` if no agent is usable; `warn` for an installed `claude` or `codex` without its key, or `opencode` that can't run a paid `OPENCODE_MODEL` (see `opencode auth`), naming the agent amf runs instead; `warn` for one that isn't installed |
| `claude login` | Always; says whether `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY` is set (and from where), or that Claude uses its own login | |
| `codex login` | Always; says whether `OPENAI_API_KEY` is set (and from where), or that Codex uses its own login | |
| `opencode auth` | Without `OPENCODE_API_KEY`: `OPENCODE_MODEL` is free, or opencode has a saved login for it. With it (never shown): asks OpenCode's `https://opencode.ai/inference/v1/models` and `.../inference/go/v1/models` with the key, and says which plans it grants (`pay-as-you-go and Go`, or `pay-as-you-go, not Go` when Go answers 403) and that opencode runs `OPENCODE_MODEL` with it | `FAIL` if a paid model has neither key nor saved login, if OpenCode rejects the key (401), or if an `opencode-go/` model's key has no Go plan; `warn` if the plan's list lacks the model or the lists can't be fetched; `skip` without opencode and without a key |
| `opencode model` | `OPENCODE_MODEL` (or the default) is a line of `opencode models` | `warn` if it isn't, since every opencode run would fail, or if `opencode models` fails; `skip` without opencode |
| `postgres` | `AMF_POSTGRES_URL` is unset (logs are only local), or amf can log in to `amflogs` on it; says whether its tables exist yet. A server without `amflogs` is `ok`: `run` and `watch` create it | `FAIL` if the URL doesn't parse; `warn` if amf can't connect or log in, with the server's message. Doctor creates nothing |

Checks that need GitHub, the repo or a checkout are `skip` without them.
Claude's and Codex's own logins aren't checked: their CLIs can't report them
reliably.

## Configuration

amf reads one `.env`: `./.env`, or else `.env` at the top of the git
checkout. Environment variables override the file. Values ending in `...`, and `owner/name`, are
placeholders and count as unset. The file has `KEY=value` lines, optionally
prefixed with `export ` and quoted; `#` starts a comment line or, after a
space, a trailing comment. A leading byte order mark is ignored.

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
| `AMF_POSTGRES_URL` | unset | A PostgreSQL server that `run` and `watch` also write their logs to. See [Logging to PostgreSQL](#logging-to-postgresql) |
| `AMF_CLAUDE` | `claude` | Path to the Claude CLI |
| `AMF_AGENT_TIMEOUT` | `60m` | How long each agent run may take, as a Go duration; `--agent-timeout` (or its alias `--timeout`) overrides it. Must be at least 1m and at most 24h |
| `AMF_LOG_LEVEL` | `info` (terminal), `debug` (event log) | `debug`, `info`, `warn` or `error`, for both the terminal and the event log of `run` and `watch`; `--log-level` overrides it |
| `AMF_CODEX` | `codex` | Path to the Codex CLI |
| `AMF_OPENCODE` | `opencode` | Path to the opencode CLI |
| `AMF_API_URL` | `https://api.github.com` | GitHub API base URL. `amf pr ready` and `amf pr draft` derive their GraphQL endpoint from it: `…/graphql` appended, or a GitHub Enterprise Server `…/api/v3` becomes `…/api/graphql` |
| `AMF_CLONE_URL` | `https://github.com/<repo>.git` | Where to clone the repo from |

## Pipelines (`AGENTS.md`)

A repo declares a pipeline with a table under an `## amf` heading in
`AGENTS.md` at its top level:

```markdown
## amf

| Stage | Skill | Agent | Model |
|---|---|---|---|
| plan | https://raw.githubusercontent.com/vicgupta/auto-mini-factory/v0.2.0/skills/plan/SKILL.md | claudecode | claude-opus-5-5 |
| code | https://raw.githubusercontent.com/vicgupta/auto-mini-factory/v0.2.0/skills/code/SKILL.md | codex | default |
| eval | https://raw.githubusercontent.com/vicgupta/auto-mini-factory/v0.2.0/skills/eval/SKILL.md | opencode | opencode/deepseek-v4.1-flash-free |
```

- **Table:** the first table between `## amf` (any case) and the next
  heading. Fenced code blocks are skipped, so the file can show examples. Columns are matched by name, ignoring case and order. `Stage` and
  `Agent` are required; `Skill` and `Model` are optional. A cell may wrap
  its value in backticks, `<…>` or a `[text](url)` link.
- **Stage:** `plan`, `code` or `eval`, each at most once. `code` is
  required. Without `plan` there is no plan. Without `eval` the first code
  round goes straight to the PR.
- **Skill:** an `https://` URL of one `SKILL.md`, or `-` or empty for none.
  `#sha256=<64 hex digits>` pins its content; no other fragment is allowed,
  and neither is a username or password in the URL.
- **Agent:** `claudecode`, `codex` or `opencode`.
- **Model:** passed to the agent. `default`, `-` or empty means the agent's
  own default, except for `opencode`, which then gets `OPENCODE_MODEL`.

`run` and `watch` read the table from `AGENTS.md` on `origin/<default>`, so a
pipeline only changes through a merge, and `watch` uses it from the next
issue on. A repo without the file or the table is processed with one agent
run, as described below.

**Skills** are put into each stage's prompt as text, after amf's own
instructions, which win where they conflict. amf's prompt fixes each stage's
inputs and output (such as eval's verdict line); the skill says how to do the
work. Starter skills are in this repo under `skills/`.

### Agents

| Agent | Command, run in the worktree | Final message |
|---|---|---|
| `claudecode` | `claude -p --dangerously-skip-permissions --output-format stream-json --verbose [--model M]`, prompt on stdin | The `result` message; one with an error fails the run |
| `codex` | `codex exec --dangerously-bypass-approvals-and-sandbox [-m M] -o <file> -`, prompt on stdin | The `-o` file; fails if it's empty |
| `opencode` | `opencode run [--model M]`, prompt on stdin | A file the prompt asks it to write; what it printed, if it didn't |

- **Permissions:** every agent runs with permission checks off. opencode
  gets `OPENCODE_CONFIG` pointing at `.amf/opencode.json`, which allows
  edits, shell commands, web fetches and paths outside the worktree.
- **Environment:** see step 6 below. Agents keep their own logins and the
  API keys in amf's environment.
- **Output files:** the final-message files are `.amf/<stage>.md` inside the
  worktree (`.amf/code.md` without a pipeline), which git ignores.

## Processing one issue

1. **Preflight.** Confirm `GITHUB_REPO` exists and `GITHUB_TOKEN` can reach it.
   A 404 means the repo or token is wrong; a 401 means the token was rejected.
2. **Checkout.** Use the enclosing git checkout. Outside one, clone the repo
   into `./<name>` with the user's git credentials, or reuse an existing clone
   there. Refuse if `./<name>` exists but isn't a clone of the repo, or if
   the checkout has no `origin`. If `origin` isn't `GITHUB_REPO` (or
   `AMF_CLONE_URL`), warn `origin is <url> but GITHUB_REPO is <repo>` and go
   on. Add `/.amf/` to `.git/info/exclude`.
3. **Check.** The issue must be an open issue, not a PR. Fetch the default
   branch and read the pipeline from `AGENTS.md` on it. Without a pipeline,
   pick the agent (see `--agent` above). A table that doesn't parse, an agent
   that isn't installed (a stage's or the one `--agent` names), no usable
   agent without either (`no coding agent is available (…)`, giving each
   agent's reason), or git having no name and email
   to commit with (`git var GIT_AUTHOR_IDENT` and `GIT_COMMITTER_IDENT`)
   stops the run here without changing the issue. `watch` then logs the
   error and tries again on its next poll.
4. **Claim.** Label the issue `amf:in-progress` and remove `amf:ready`.
   - **Trust:** with `AMF_TRUSTED` set, an issue whose author (`user.login`,
     ignoring case) isn't listed fails here, before any worktree or agent.
     The comment names the author and says to reopen it from a trusted
     account or add the author to `AMF_TRUSTED`, then add `amf:ready`. Being
     marked failed, it isn't picked up by `watch` again.
5. **Worktree.** Fetch the default branch and create
   `.amf/worktrees/issue-N` on `amf/issue-N`, reset to `origin/<default>`. A
   worktree left from an earlier run is discarded first.
6. **Agent.** Without a pipeline, run the agent picked in step 3 as in the
   Agents table above: opencode with `--model` set to `OPENCODE_MODEL`, the
   others with no model. amf logs `#N: running <claude|codex|opencode>`
   (with opencode's model in parentheses).
   - **Prompt:** the issue's title and body, plus instructions to make the
     smallest complete change, run the project's tests and build, not touch
     `.github/`, git or GitHub, and reply with only a PR description.
   - **Environment:** `PWD` is the worktree, and no agent gets
     `GITHUB_TOKEN`. Only Claude gets `CLAUDE_CODE_OAUTH_TOKEN`, and only
     opencode gets `OPENCODE_API_KEY`: amf removes each from the other
     agents' environments, even when it's exported in the shell. When
     `CLAUDE_CODE_OAUTH_TOKEN` is set, it's passed to Claude and Claude's
     `ANTHROPIC_API_KEY` is removed. Otherwise an `ANTHROPIC_API_KEY` from
     `.env` or the environment is passed to Claude. `OPENAI_API_KEY` from
     `.env` or the environment is passed to Codex, and `OPENCODE_API_KEY` to
     opencode, each replacing any value the agent would inherit. An
     inherited `OPENCODE_CONFIG` is removed for Codex and replaced for
     opencode (see Permissions).
   - **Log:** output streams live to `.amf/logs/issue-N.log`, a line at a
     time and redacted (see "Redaction" under "Security model").
   - **Timeout:** each run may take at most `--agent-timeout` (or
     `AMF_AGENT_TIMEOUT`; the default is 60m). On expiry amf signals the
     run's whole process group with SIGTERM, then, if it is still there
     after a 15s grace period, SIGKILL: the agent, the shells it spawns and
     their MCP servers are all stopped, as on Ctrl-C (on Windows,
     `taskkill /T /F` ends the tree at once). The run fails;
     whatever the agent printed stays in its log, ending with a
     `amf: killed after <elapsed> timeout (SIGTERM, then SIGKILL after 15s)`
     line, and an agent with a final-message file has what was written kept
     as `.amf/logs/<log>.partial.md`. amf logs an `agent.timeout` event, and
     the issue's comment names the agent, the stage and the limit, and how
     to retry with more time. A timed-out run pushes nothing and opens or
     updates no PR, and its worktree is removed; the logs and partial output
     stay, as does the local `amf/issue-N` branch, which is never pushed and
     which a retry resets to `origin/<default>`.
   - **Summary:** the final message becomes the summary.

   With a pipeline, this step and the next are replaced by the steps in
   "With a pipeline" below.
7. **Commit.** Stage everything and commit as `Fix #N: <title>` with the body
   `Closes #N`. Fail with `<claude|codex|opencode> finished without making
   any changes` if the branch doesn't change any file compared with
   `origin/<default>`, even if the agent made commits of its own. The run
   also fails — before the push, so nothing is pushed and no PR is created —
   when any changed path is under `.github/`, where workflows and `CODEOWNERS` live;
   the error names the offending paths. Changes elsewhere, even under a
   directory whose name contains `.github`, are fine.
8. **Push.** `git push --force -u origin amf/issue-N`, with the user's git
   credentials.
9. **PR.** Open `amf/issue-N` → `<default>`, titled `Fix #N: <title>`. If
   the branch already has an open PR (a retry), the push has updated it, so
   amf replaces that PR's title and body instead. With `--draft`, the create
   request sends `"draft": true`, so a new PR opens as a draft; without it
   the request has no `draft` field and the PR is ready for review. An
   updated PR keeps its draft or ready state: the update sends only the
   title and body. The
   body is the summary (or `Resolves #N.` if there isn't one), then any
   pipeline sections, then `Closes #N` and an "Opened by amf" footer. amf
   first removes Claude attribution lines ("Generated with Claude Code",
   `Co-Authored-By`) and the agent's own `Closes`/`Fixes`/`Resolves #N` line
   from the summary, and the attribution lines from the plan and review too.
   That way each PR is credited to the user alone and closes the issue once.
10. **Clean up.** Remove the worktree. The branch and logs are kept.
11. **Merge** (with `--merge` only). Merge the PR with the method `--merge`
    names, through GitHub's merge API, pinned to the commit amf pushed
    (`sha`), so GitHub refuses if the branch changed since. GitHub's default
    commit title and message are used, and the branch isn't deleted.
    - A change to `AGENTS.md` at the top level, where the pipeline is
      declared, is never merged: amf fails the merge with `the change edits
      AGENTS.md, which declares amf's pipeline, so amf leaves it for review`
      without calling GitHub. An `AGENTS.md` in a subdirectory is fine.
    - GitHub can answer 405 `Pull Request is not mergeable` while it is
      still working out whether a new PR merges cleanly, so on that answer
      amf asks up to 3 times in all, 5 seconds apart. A conflict gets the
      same answer, so it fails after the third. Any other refusal (a
      required check or review, a merge method the repo doesn't allow, a
      changed branch, a retry's PR left a draft by an earlier `--draft`
      run) is final at once.
    - amf doesn't wait for checks to finish. With branch protection that
      requires them, the merge fails and the PR stays open.
12. **Finish.** Set `amf:done` and comment on the issue:
    - without `--merge`: `amf opened <PR URL> for this issue.` (`updated`
      for a retry's PR);
    - merged: `amf opened <PR URL> for this issue and merged it.` (`squashed
      and merged`, `rebased and merged`); the PR's `Closes #N` then closes
      the issue;
    - not merged: `amf opened <PR URL> for this issue, but could not merge
      it:`, the reason in a code block, and that the PR is still open for
      review. The issue is still `amf:done`, but `run` exits 1 with `opened
      <PR URL> but could not merge it: <reason>`, and `watch` logs that as
      a failure and goes on.

    The PR exists by now, so if setting the label or commenting fails, amf
    logs a warning and the run still succeeds.

### With a pipeline

1. **Skills.** Download each stage's skill: HTTPS only, following only HTTPS
   redirects, with no credentials sent, at most 256 KB, UTF-8, and matching
   its `sha256` pin if it has one. Any problem fails the issue.
2. **Plan** (if declared). The prompt has the issue, the skill, and
   instructions not to change files and to reply with only the plan, or with
   a first line starting `BLOCKED:` and the questions that need answers.
   - The plan is saved to `.amf/runs/issue-N/plan.md`, and its log is
     `.amf/logs/issue-N-plan.log`.
   - Afterwards amf resets the worktree to the commit it started from and
     removes untracked files (`git clean -fd`), so the stage can't change
     the branch.
   - A `BLOCKED:` plan fails the issue. The first line counts as `BLOCKED:`
     ignoring case and leading `*`, `_`, backticks, `#` and `>`. The comment
     shows the plan and asks for the issue to be edited and `amf:ready`
     added again.
3. **Code, round K** (K = 1, 2, …). The prompt is the one in step 6, plus
   the plan and the skill. From round 2 on, it also says the previous
   attempt is in the working tree, and gives eval's review to fix.
   - The log is `.amf/logs/issue-N-code-K.log`.
   - amf commits any changes as `amf: code round K`. The run fails with
     `the code stage (<agent>) finished without making any changes` if the
     branch doesn't change any file compared with `origin/<default>`, such
     as when a round undoes the one before. amf refuses `.github/` changes
     as in step 7, before any push.
   - The final message becomes the summary.
4. **Eval, round K** (if declared). The prompt has the issue, the plan, the
   code stage's summary, the skill, and `git diff origin/<default> HEAD`
   (cut at 100 KB, without splitting a character, and with a note). It tells the agent to run the tests, change
   nothing, and end with `VERDICT: PASS` or `VERDICT: FAIL`.
   - The review is saved to `.amf/runs/issue-N/eval-K.md`, with its log
     at `.amf/logs/issue-N-eval-K.log`. The worktree is then reset as after
     Plan.
   - The verdict is read from the last non-blank line, ignoring case,
     `*`, `_`, backticks, `#`, `>` and a trailing period. Without one, the result is
     `FAIL` and the review starts `eval gave no verdict.`
   - On `PASS`, continue to step 5.
   - On `FAIL`, go back to step 3 with the review, unless there have
     already been `--eval-rounds` code rounds after the first. In that case
     amf pushes the branch for inspection and fails the issue. The comment
     names the eval agent, the number of rounds and the branch, and puts the
     last review in a collapsed block.
5. **Squash.** Replace the round commits with one commit, as in step 7, and
   continue with Push.

**Pipeline sections in the PR body:** with an eval stage, an
`## Evaluation` section saying `PASS`, the eval agent and model, and the
number of code rounds, with the review in a collapsed block. With a plan
stage, the plan in a collapsed block.

### Dry run (`amf run --dry-run`)

amf only reads from GitHub: the issue, the repo and the branch's open PR.

1. **Preflight, Checkout and Check** run as in steps 1 to 3, and stop the
   run in the same cases.
2. **No claim.** The issue's labels aren't touched, so it stays as it was
   (`amf:ready`, say). The trust check of step 4 still runs, since the
   agent still runs the issue's text, and an untrusted author fails the
   run.
3. **Worktree, Agent and Commit** (steps 5 to 7, or the pipeline's steps)
   run as usual, with the same logs and files in `.amf/`, the same
   `.github/` refusal, and the change committed on the local
   `amf/issue-N` branch. A pipeline whose eval never passes doesn't push
   its last attempt.
4. **Report.** amf asks GitHub for the branch's open PR, then prints
   `#N: dry run: committed <commit> on the local branch amf/issue-N; amf
   would push it and <what>`, where `<what>` is `open a pull request into
   <default>` (`a draft pull request` with `--draft`) or `update <PR URL>`
   for a retry. With `--merge` it adds `, and merge it (<method>) if GitHub
   allows`, or `, but not merge it: <reason>` for a change to the
   top-level `AGENTS.md`. Then it prints the change's `git diff --stat`
   against `origin/<default>`, `Title: Fix #N: <title>`, the PR body as
   step 9 would post it, and `Nothing was pushed, and the issue and GitHub
   are unchanged. To see the change: git diff origin/<default>
   amf/issue-N`.
5. **Clean up.** The worktree is removed. The local branch and the logs are
   kept. A later run, dry or not, resets the branch to `origin/<default>`.

A dry run that fails exits 1 with the error, and leaves the issue as it
was: no label and no comment. Its worktree is kept for inspection, except
after `--agent-timeout`, which removes it as usual.

**On failure** in any step after Claim, up to opening the PR (a failed
merge doesn't count; see step 11): set `amf:failed`, comment the error on
the issue, and keep the worktree and logs for inspection. This also happens if
the run is interrupted — except that a run that hit `--agent-timeout` leaves
the worktree (removed, with its branch unpushed) and keeps only the logs and
partial output. If Claim itself fails, the issue can be left on
`amf:in-progress`, which `watch` skips; `amf run N` recovers it.

## Updating (`amf update`)

1. Resolve the target: `--version` (a leading `v` is added if missing), or
   the tag that `https://github.com/vicgupta/auto-mini-factory/releases/latest`
   redirects to. If it equals the running version, print `amf <v> is up to
   date` and stop. `--check` stops here too: it prints `amf <v> is available
   (you have <current>); run amf update`, except that when `--version` names
   a release older than the running one (both `vX.Y.Z`) it prints `amf <v>
   is older than this build (<current>); amf update --version <v> would
   downgrade`.
2. Download the archive for this platform and `checksums.txt` from that
   release: `amf_<tag>_<os>_<arch>.tar.gz` on macOS and Linux, or
   `amf_<tag>_windows_<arch>.zip` on Windows. If the archive is missing, fail
   with `no amf <tag> for <os>/<arch>`. If `checksums.txt` doesn't list it,
   or its SHA-256 doesn't match, fail.
   The binary is `amf_<tag>_<os>_<arch>/amf` inside the archive
   (`…/amf.exe` on Windows); if it isn't there, fail.
3. Write the binary next to the running executable (symlinks resolved) as a
   temporary file with permissions 0755, named `.amf-update-*` (with `.exe`
   on Windows, which runs only executable extensions). Run it with
   `version`, and fail unless it prints `amf <tag>`.
4. Rename it over the running executable. Windows can't replace a running
   `.exe`, so there amf first renames it to `amf.exe.old`, then moves the new
   one in, restoring the old one if that fails. The next update removes
   `amf.exe.old`. A folder that can't be written fails with a hint to rerun
   with permission (`sudo`, or a terminal run as administrator on Windows).

Releases have archives for `darwin`, `linux` and `windows`, each on `amd64`
and `arm64`. `install.sh` covers macOS and Linux; on Windows it points to the
release's `.zip`.

Any failure leaves the installed binary unchanged.

`AMF_RELEASES_URL`, read from the environment only (never from `.env`),
replaces `https://github.com/vicgupta/auto-mini-factory` as where `amf update`
and `doctor`'s `amf` check look for releases, as for a mirror or in tests; a
trailing `/` is ignored. That server must answer `/releases/latest` with a
redirect to a URL ending in the tag, and serve the archive and
`checksums.txt` under `/releases/download/<tag>/`.

## Files

| Path | Contents |
|---|---|
| `.env`, `.env.NNNN` | Configuration and its backups (permissions 0600, git-excluded) |
| `.amf/worktrees/issue-N/` | Working copy for an issue in progress or failed |
| `.amf/logs/issue-N.log` | The agent's output for the latest run of issue N, without a pipeline (stream-json for Claude) |
| `.amf/logs/issue-N[-<stage>].partial.md` | An agent's final-message file, kept (redacted) after a timeout cut the run short |
| `.amf/logs/issue-N-<stage>.log` | Each pipeline stage's output: `plan`, `code-K`, `eval-K` |
| `.amf/runs/issue-N/` | The latest pipeline run's `plan.md` and `eval-K.md` |
| `.amf/runs/<run_id>/events.jsonl` | The event log of one `run` or `watch` process, unless `--log-file` says otherwise |
| `.amf/runs/<run_id>/summary.json` | That process's summary, rewritten after each issue and at exit |
| `.amf/opencode.json` | The config amf runs opencode with |

amf only ever adds to `.git/info/exclude`. It never edits a tracked `.gitignore`.

## Event log

Each record in `events.jsonl` is one event. Every record has `time`,
`level` (`DEBUG`, `INFO`, `WARN` or `ERROR`), `msg`, `run_id` and `event`,
and `issue` when it is about one. `msg` is the line the terminal shows for
it (`amf: <msg>`); `DEBUG` events add detail the terminal doesn't show by
default, and their `msg` is a short description. Durations are Go
durations (`1m0s`). `github.call`, `git.exec`, `git.push` and `http.fetch`
are logged for the calls `watch` and each issue's run make, with the issue
they are for; the checks `run` and `watch` make at startup, before the
event log opens, are not. The fields of each event, besides those:

| Event | Level | Fields |
|---|---|---|
| `log.file` | info | `path`: the event log |
| `run.summary` | debug, info | The fields of `summary.json` (see Commands) as `done`, `failed`, `skipped`, `github_calls`, `ratelimit_lowest`, `git_calls`, `agent_ms`, `errors`, `log`, and `final`: false (debug) after an issue, true (info, the terminal's last line) at exit |
| `config.load` | debug | `paths` (the `.env` files looked for), `file` (the one read, or `""`), and `keys`: for each variable read, its `source` (`env`, `.env` or `default`) and `value`. Only `GITHUB_REPO`, `AMF_API_URL`, `OPENCODE_MODEL`, `AMF_AGENT_TIMEOUT`, `AMF_LOG_LEVEL`, `AMF_CLAUDE`, `AMF_CODEX` and `AMF_OPENCODE` show their value; every other one shows `set` or `unset` |
| `pg.connect` | debug | With `AMF_POSTGRES_URL`: `host`, `port`, `database`, and `tls` and `auth` (`trust`, `password`, `md5` or `scram-sha-256`), or `error`; `created` when amf created the database. The first connection's event follows `log.file` and `config.load` |
| `pg.retry` | debug | `attempt` (since the last success), `backoff`, `error` |
| `pg.drop` | debug | `rows` that never reached the database, and `error` when amf gave up at exit |
| `checkout.resolve` | debug | `how` (`checkout`, `clone` for a clone reused, `cloned`), `dir` |
| `watch.start` | info | `repo`, `label`, `exclude_labels` (the `--exclude-label` values, `[]` when none), `interval`, `timeout`, `eval_rounds`, `draft`, `merge` (the method, or `off`), `agent_flag`, `version`; with `--merge` the line adds `, merging each PR without review (<method>)` |
| `watch.agents` | info | The heading line has none; one per agent with `agent`, `path`, `key` (the variable's name, never its value, or `saved login` for opencode's own), `usable`, `model`; then `picked`, or `error` (warn) when none is usable |
| `watch.tick` | debug | `cycle` (1 for the first poll), `issues`: how many the poll returned; warn with `error` when listing failed |
| `watch.skip` | debug | `reason`: `pr`, `closed`, `in_progress` or `status_label` (done or failed without `amf:ready`), checked first; or `excluded_label`, with `label`: the first `--exclude-label` value the issue has |
| `watch.failed` | warn | `error`: an issue that failed (watch goes on) |
| `watch.stop` | info | `reason` (the context's error, such as `context canceled`), `cycles` |
| `issue.picked` | info | `title`, `author`, `labels` |
| `issue.agent_ignored` | info | `agent`: the `--agent` a pipeline overrides |
| `issue.author_check` | debug | `author`, `trusted`; an untrusted issue then fails |
| `issue.status` | debug | `from`, `to`: an `amf:*` label change (`none` when there was none); warn with `to` and `error` when setting it failed |
| `issue.pipeline` | info / debug | `declared` (true, info, for a pipeline; false, debug, for one agent run), `stages`, `agents`, `models` |
| `issue.skill` | info | `stage`, `url` |
| `issue.worktree` | info | `path`, and `branch` and `base`, or `stale: true` when an old one is removed |
| `issue.agent` | info | `agent`, `model`, `timeout`: the one run without a pipeline |
| `issue.stage` | info | `stage` (`plan`, `code-K`, `eval-K`), `agent`, `model`, `timeout` |
| `issue.plan.done` | debug | `bytes`: the plan's length |
| `issue.plan.blocked` | debug | `question`: the first one |
| `issue.code.round` | debug | `round` |
| `issue.code.done` | info | `rounds`, without an eval stage |
| `issue.eval.verdict` | info | `round`, `pass`, `parsed` (whether eval gave a verdict), and `next_round`, or `final: true` when no rounds are left |
| `agent.start` | debug | `agent`, `binary`, `args`, `model`, `stage` (`agent run` without a pipeline), `dir`, `timeout`, `stdin_bytes` and `stdin_sha256` of the prompt, and `env_added` and `env_removed`: the names of the variables amf added, changed or removed for the agent, never their values |
| `agent.exit` | debug | `agent`, `stage`, `exit_code` (-1 when killed or not started), `signal` when a signal ended it, `timed_out`, `duration_ms`, `stdout_bytes`, `stderr_bytes`, `out_bytes` (codex's and opencode's final-message file), and `result_bytes` and `result` (the final message's first 512 bytes), or `error` |
| `agent.parse` | debug | Claude: `result_found`, `json` (whether its output was JSON); codex and opencode: `out_file` (whether they wrote their final message) |
| `agent.timeout` | warn | `agent`, `stage`, `limit`, `elapsed`, `grace`, `pgid`, `bytes` (stdout so far), `log` |
| `issue.push` | info | `branch` |
| `issue.pr` | info | `action` (`opened`, `updated`, or `updating` when the branch has an open PR), `number`, `url` |
| `issue.merge` | info, warn, debug | `action`, `method`, `number`: `merging` (info) with `sha`, then `merged` (info) with `commit` (GitHub's merge commit) and `attempts`, or `failed` (warn) with `error`; `retry` (debug) with `attempt` and `error` when GitHub said not mergeable yet |
| `issue.comment` | debug | `bytes`; warn with `error` when posting failed |
| `issue.cleanup` | warn | `error`: a worktree that couldn't be removed or pruned |
| `issue.done` | debug | `pr`: its number; `dry_run: true` instead after a dry run |
| `issue.dry_run` | info | `branch`, `base`, `head` (the local commit), `files` (the paths it changes), `action` (`open` or `update`), `url` (the PR to update, or `""`), `draft`, `merge` (the method, `off`, or `refused` with `merge_error`), `title`, `body` |
| `issue.failed` | debug, info | `reason` (`agent_timeout` at info, with `stage`; `stage` for a stage's own failure such as a blocked plan or an untrusted author; else `error`), `error`, and `dry_run: true` in a dry run |
| `github.call` | debug | One per GitHub API request: `method`, `path` with `{repo}`, `{n}` and `{label}` in place of the repo, numbers and label names, `status` (0 when there was no response), `duration_ms`, `bytes_out` (request body), `bytes_in` (response body read), `ratelimit_remaining`, `ratelimit_reset`, `request_id` (GitHub's headers, `""` when absent), `page` for a later page of a list, and `error_body` (the first 512 bytes of an error response) or `error`. The token is never logged |
| `git.exec` | debug | One per git command: `subcommand`, `args` (a commit message cut to 80 characters), `dir`, `exit_code` (-1 when git didn't start), `duration_ms`, `stdout_bytes`, `stderr_bytes`, and on failure `stderr` (its first 512 bytes) |
| `git.push` | debug | After `git.exec` for a push: `branch`, `forced`, `exit_code` |
| `http.fetch` | debug | One per skill download: `url` (without any credentials), `status`, `redirects`, `duration_ms`, and `bytes` and `sha256` of the skill, or `error` |

## Logging to PostgreSQL

With `AMF_POSTGRES_URL` set, `amf run` and `amf watch` write every log line
to PostgreSQL as well. The terminal and `.amf/logs/` get exactly what they
get without it. Other commands don't log there.

- **URL:** `postgres://` or `postgresql://user:password@host:port/database`,
  with libpq's defaults: `localhost`, port 5432, and the OS user. Query
  parameters: `sslmode` (`disable`, `allow`, `prefer` (the default),
  `require`, `verify-ca`, `verify-full`; like libpq, only `verify-*` check
  the server's certificate, against the system's roots), `connect_timeout`
  in seconds (default 10), and `application_name` (ignored: amf is always
  `amf`). Any other parameter, or another scheme, makes `run` and `watch`
  fail at startup with `AMF_POSTGRES_URL: <problem>`. Authentication can be
  trust, password, MD5 or SCRAM-SHA-256 (without channel binding).
- **Database:** amf always logs to the database `amflogs`. If it doesn't
  exist, amf connects to the URL's database (or `postgres` when the URL names
  none, or names `amflogs`) and runs `CREATE DATABASE amflogs`, so the user
  needs `CREATEDB` the first time. Then it runs `CREATE TABLE IF NOT EXISTS`
  and `CREATE INDEX IF NOT EXISTS` for:

  | Table | One row per | Columns |
  |---|---|---|
  | `amf_log` | line amf prints, without its `amf: ` prefix, plus the error `run` or `watch` exits with | `id`, `logged_at`, `host`, `repo`, `issue` (from a leading `#N`, else NULL), `message` |
  | `agent_log` | line an agent prints (stdout and stderr) | `id`, `logged_at`, `host`, `repo`, `issue`, `agent` (`claude`, `codex` or `opencode`), `log_file` (the file in `.amf/logs/`, such as `issue-12-code-1.log`), `line` |

  Both have an index on `(repo, issue, logged_at)`. `logged_at` is when amf
  read the line, `host` this machine's hostname, `repo` `GITHUB_REPO`. NUL
  bytes are dropped and invalid UTF-8 replaced, since `text` can't hold
  them; lines are otherwise as printed, redacted, with a trailing `\r`
  removed.
- **At startup** amf connects and creates what's missing, then prints one
  of `amf: created database amflogs at user@host:port; logging there too`,
  `amf: logging to database amflogs at user@host:port too`, or
  `amf: warning: can't log to PostgreSQL at user@host:port: <error>; logging locally only, and retrying`.
  amf never prints the password.
- **In the background:** lines are queued in memory and sent at least every
  second, up to 500 per insert, each insert in one transaction. Logging
  never slows down or fails an issue. If the server can't be reached, amf
  warns once, keeps the lines queued (up to 10,000; beyond that the oldest
  are dropped) and retries after 1 s, doubling to at most 1 minute. It
  prints `amf: logging to PostgreSQL at user@host:port again` when it
  recovers, and how many lines it dropped. When `run` or `watch` ends, amf
  waits up to 15 seconds to send what's queued. If the server refuses them
  it warns how many lines only made it to the local logs; if the 15 seconds
  run out it warns that it gave up and the lines are still in the local
  logs.

## Security model

- Agents run with every permission check off. An issue's body is effectively
  code run on the user's machine, so label only issues you trust.
  `--dry-run` changes nothing on GitHub, but runs the agents just the same.
- Labels need triage access, but amf reads the issue's text when it runs,
  not when it was labeled, so its author can change it in between.
  `AMF_TRUSTED` limits amf to issues opened by the listed users. Other
  people can't edit those, apart from collaborators with write access.
  Comments are never read.
- A skill is instructions run the same way. Reference only skills you
  control or have reviewed, at a tag or commit, ideally with a `sha256` pin.
  The pipeline itself changes only through a merge to the default branch,
  which `--merge` never makes for a change to the top-level `AGENTS.md`.
- `--merge` puts what an agent wrote on the default branch with no human
  review, and the next issue starts from it. Use it only where the issue
  text and the agents are trusted, and let branch protection's required
  checks and reviews stand between amf and the branch if you want them:
  amf merges only what GitHub allows. The `.github/` refusal (step 7)
  still applies, so no workflow change is ever pushed.
- `amf pr merge` is a person explicitly choosing a PR to merge. It allows
  changes to `AGENTS.md` and uses GitHub's branch protection rules; the
  unattended `run`/`watch --merge` refusal still applies.
- No agent gets `GITHUB_TOKEN`, and each agent's environment holds only
  its own key (step 6), but agents can read local files, including `.env`
  (and so every key in it, and `AMF_POSTGRES_URL`'s password).
- With `AMF_POSTGRES_URL` set, everything the agents print, which can
  include code and the contents of files they read, is stored in
  `amflogs` (redacted, as below). Anyone who can read that database can
  read it. For real isolation, run amf in a container or VM.
- **Redaction.** `run` and `watch` mask secrets and personal data in
  everything they write: the terminal, the agent logs and partial final
  messages in `.amf/logs/`, the rows in `amflogs`, the comments and PR
  bodies they post, and the error they exit with. What is masked, as
  `[REDACTED:<kind>]`:
  - the values of `GITHUB_TOKEN`, `CLAUDE_CODE_OAUTH_TOKEN`,
    `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `OPENCODE_API_KEY` and the password in
    `AMF_POSTGRES_URL`, wherever they appear (values shorter than 8
    characters are not matched);
  - GitHub tokens (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_`),
    `sk-` and `oc_sk_` API keys, AWS access key ids (`AKIA`, `ASIA`);
  - the credential in `Authorization: Bearer|token|Basic`;
  - the value of `NAME=value` or `NAME: value` (also JSON-quoted) when NAME
    ends in `_TOKEN`, `_KEY` or `_SECRET`, or in `PASSWORD`;
  - `user:password@` in URLs, which becomes `[REDACTED]@`;
  - email addresses, except ssh remotes such as `git@github.com:`.

  The user's home directory becomes `~`, as macOS, Linux and Windows spell
  it. Redaction is pattern matching, not a guarantee: a secret in another
  format can still get through. Agents still see everything unredacted, and
  amf's own git commits and `.env` are untouched.
- Plan and eval can't change the branch: amf resets the worktree after them,
  so only the code stage's changes reach the PR.
- amf does all git and GitHub writes itself, through the user's credentials
  and the configured token.

## Non-goals

- Running issues in parallel. `watch` processes one at a time.
- Reviewing PRs, or retrying failed ones. People do that. amf merges with
  `--merge` or an explicit `amf pr merge`, and only what GitHub allows.
- Sandboxing agents.
- Skills that need authentication to download.
