# amf specification

amf (autonomous mini factory) turns labeled GitHub issues into pull requests by
running coding agents headless in a git worktree. By default that's one run of
a single agent: the one `--agent` names, or else the primary agent
(`AMF_PRIMARY_AGENT`, default Codex), falling back to the secondary
(`AMF_SECONDARY_AGENT`, default Claude Code) if the primary fails. A repo can instead declare a plan → code → eval pipeline in its
`AGENTS.md`, with a skill, agent and model for each stage. amf is a single Go
binary (pure Go, no cgo; its one dependency is the SQLite driver for repo memory), for one developer or a small team
that doesn't want to run a platform.

**Needs:** `git`, a GitHub token for one repository, and an agent amf can
run: `claude` with `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN`, `codex`
with `OPENAI_API_KEY`, or `opencode` (with `OPENCODE_API_KEY` or its own
saved login for a paid `OPENCODE_MODEL`), or any of them named with `--agent`. A
pipeline needs whichever agents it names.

## Issue lifecycle

```
amf:ready ──▶ amf:in-progress ──▶ amf:pr-open (PR opened, not merged)
                       ├────────▶ amf:done    (PR merged with --merge)
                       └────────▶ amf:failed  (reason commented)
```

- `amf:ready` marks an issue for amf. Adding it again retries an issue that is
  done or failed.
- `amf:pr-open` means amf produced a PR for the issue, a draft one
  (`--draft`) included, that is open and not merged: no `--merge`, or the
  merge failed.
- `amf:done` means amf merged the issue's PR (with `--merge` or `amf pr merge`).
  A partial change (`--part` or `amf-status: partial`) is never linked with a
  closing keyword, so merging it leaves the issue open until the remaining
  work is done and the issue is marked `amf:ready` again.
- Before Claim, amf skips an issue with an open PR from `amf/issue-N`
  (relabeling it `amf:pr-open`) or with an unmerged `amf/issue-N` branch on
  origin (unless it is `amf:failed`). A leftover branch whose tip is an
  ancestor of `origin/<default>`, or is the head of a merged PR from that
  branch, is reused: amf starts fresh from `origin/<default>` and logs
  `issue.reuse`. `amf run N --force`, and `amf auto`, skip that guard. The
  skip is logged once as `issue.skip`; `amf run` prints that reason once,
  counts the issue as skipped in its summary, and exits 3.
  A failure never moves an issue from `amf:pr-open` or
  `amf:done` to `amf:failed`, but `amf:done` is kept only when GitHub lists
  a merged PR from `amf/issue-N`. After `--merge`, amf reads the PR back and
  sets `amf:done` only if it is merged (`amf:pr-open` otherwise).
- Each poll, `watch` relabels `amf:pr-open` up to five open `amf:failed`
  issues (without `amf:ready`) whose `amf/issue-N` PR is open, not a draft,
  and has passing checks: the failure is stale.
- `watch` puts an `amf:in-progress` issue no amf works on back to
  `amf:ready` once the label is older than `AMF_STALE_TIMEOUT`.
- `run`, `watch` and `auto` hold `.amf/watch.lock`; see
  [the repo lock](COMMANDS.md#the-repo-lock).
- amf removes `amf:ready` when it starts, and keeps an issue to at most one
  status label at a time. When it changes the status, it adds the new label
  first and then removes the old ones, so an interrupted change can leave an
  extra status label but never none.
- amf owns these five labels and every `amf/issue-<N>` branch.

## Commands

**Paging:** amf never invokes a pager. `amf pr diff` and `amf memory show`
write directly to stdout, even in a terminal; there is nothing to disable.
There is no `--no-pager` flag, and `PAGER` (including `PAGER=cat`) has no effect
on amf's output. Users can opt into paging with `amf pr diff 42 | less` or
`amf memory show 42 | less`, as documented in the README.

| Command | Behavior |
|---|---|
| `amf init` | Writes `./.env` and creates the labels (see below). |
| `amf issue <command>` | Groups issue operations: `create`, `list`, `label`, `unlabel`, `close`, `reopen`, `view`, `run`, `watch`. Bare `amf issue`, `amf issue help`, `amf issue -h` and `amf issue --help` print the same group usage to stdout and exit 0, leaving stderr empty, without reading configuration, contacting GitHub or reading stdin. A stdout write failure follows the normal error path (exit 1). `amf issue create` without a title remains a usage error (exit 2). A single-word title within two edits of a subcommand, such as `amf issue lst`, creates nothing: it prints `amf: unknown issue command "lst"`, `Did you mean "list"?` and the way to create it anyway (`amf issue create "lst"`), then the group usage, to stderr and exits 2. The top-level `amf issue "Title"`, `amf run` and `amf watch` forms are aliases for `amf issue create`, `amf issue run` and `amf issue watch`. Top-level `amf list` still runs `amf issue list` as an undocumented alias, kept for existing scripts, and is not listed in the help overview; a mistyped top-level `lst` suggests `issue list`. |
| `amf issue "Title" [--body text \| --file path \| --file -] [--label name] [--no-label]` | Creates an issue labeled `amf:ready` (or `--label`'s label; none with `--no-label`). `amf issue create -h` and `--help`, including either flag after a title in the explicit creation or title shortcut form, print the existing flag help to stderr, leave stdout empty, and exit 0 before validating the title, reading stdin, a body file or configuration, or contacting GitHub. Missing titles, unknown flags and missing flag values remain usage errors (exit 2); help follows normal argument parsing. The title's and the label's surrounding whitespace are trimmed before posting. The body can also come from stdin when it's piped; an explicitly supplied `--body` (`-b`), including an empty value, selects that body and `--body` and a nonempty `--file` (`-f`) together are an error (`use --body or --file, not both`). An explicit `--file -` reads stdin. An explicit `--label` with `--no-label` is an error. An empty or whitespace-only `--label` (without `--no-label`) fails with `--label can't be empty`, before anything is read or fetched. Prints `#N <url>`. |
| `amf issue list [--state open\|closed\|all] [--label name] [--search text] [--limit n] [--json]` | Lists issues, newest first (highest number first), with every page fetched and PRs left out. `-h` and `--help` print the flag help to stderr, leave stdout empty, and exit 0 before reading configuration or contacting GitHub. Surrounding whitespace on `--state` and `--label` is trimmed, and `--state` ignores letter case; whitespace inside a label name is preserved. An omitted, empty or whitespace-only label applies no label filter. `--search TEXT` (default empty) keeps only issues whose title contains the trimmed text, ignoring case; an empty or whitespace-only value applies no filter, and the filter runs before `--limit`. Columns: `ISSUE`, `STATE` (with `--state all` only), `AMF` (the issue's `amf` and `amf:*` labels, or `-`) and `TITLE`. `--limit N` (default `0`, no limit) prints only the first N issues after filtering, and when that hides some, the table output is followed on stderr by `Showing N of M issues; use --limit 0 to list them all.` (stdout is unchanged, and `--json` prints no hint); a negative value errors with `--limit must be 0 or more, got N`. Tabs in a title print as spaces. `--state` takes only `open`, `closed` or `all`, in any letter case, after trimming surrounding whitespace. Prints `No … in owner/name.` when nothing matches, using the trimmed label when one is set, and `with "TEXT" in the title` after it when `--search` is set. `--json` (default `false`) prints instead one JSON array and a newline, with no header: one object per issue, in the same order and after the same filters and limit, with exactly `number` (integer), `title`, `state`, `url` (the issue's GitHub page) and `labels` (every label name, in GitHub's order, `[]` when none); titles are kept as GitHub sent them, tabs included. Nothing matching prints `[]`. Argument, configuration and API errors leave stdout empty. A failure to write stdout, including the no-matching-issues message, is returned as an error and exits 1. Needs no git checkout. |
| `amf issue label [<N>] [label...] [--add name]... [--remove name]... [selectors]` | Adds positional labels and adds or removes labels with `--add` and `--remove` on open or closed issue N (`42` or `#42`; a pull request is rejected). Names may be comma-separated; flags repeat. At least one nonblank label is required (there is no default), and a name in both add and remove is an error. Only labels that change are written: labels already present are not re-added and absent ones are not removed. Adding an amf status label (`amf:in-progress`, `amf:pr-open`, `amf:done`, `amf:failed`) removes the issue's other status labels; adding two status labels is an error. Prints `#N: added A; removed B` or `#N unchanged`. `--` ends flag parsing, allowing names beginning with `-`. Needs no git checkout. `-h` and `--help` exit 0 before reading configuration or contacting GitHub. |
| `amf issue unlabel [<N>] [label...] [selectors]` | Removes the given labels from open or closed issue N (`42` or `#42`; a pull request is rejected), leaving other labels unchanged. Names may be comma-separated; at least one nonblank label is required (there is no default). Duplicate names are removed once; absent labels cost no write. Prints `#N: removed A, B` or `#N unchanged`. `--` ends flag parsing, allowing names beginning with `-`. Needs no git checkout. `-h` and `--help` exit 0 before reading configuration or contacting GitHub. |
| `amf issue close <N> [--comment text] [--delete-branch]` | Closes issue N (`42` or `#42`, surrounding whitespace trimmed; pull requests rejected). Posts a nonempty `--comment` first, with configured secrets redacted; comment failure prevents closing. Prints `Closed #N <url>`. With `--delete-branch`, deletes only the remote `amf/issue-N` branch in the configured repository after closing; no local branch or worktree is changed. A deletion failure (including a missing branch) exits 1 with `closed #N but could not delete amf/issue-N: …`, leaving the success line on stdout and the issue closed. An already closed issue prints `#N is already closed`, succeeds, and neither comments nor deletes. Labels are unchanged. Needs no git checkout. `-h` and `--help` exit 0 before reading configuration or contacting GitHub. Missing/extra arguments and unknown flags exit 2; invalid issue numbers exit 1. |
| `amf issue reopen <N>` | Reopens a closed issue N (`42` or `#42`, surrounding whitespace trimmed; pull requests rejected) and prints `Reopened #N <url>`. An already open issue prints `#N is already open`, succeeds, and sends no write. Labels are unchanged. Needs no git checkout. `-h` and `--help` exit 0 before reading configuration or contacting GitHub. Missing/extra arguments and unknown flags exit 2; invalid issue numbers exit 1. |
| `amf issue view <N> [--json]` | Shows an open or closed issue N (`42` or `#42`, surrounding whitespace trimmed; pull requests rejected). Text prints `#N <title>`, then `state:`, `author:`, `labels:` and `url:` fields (values start in column 12). Labels include every label in GitHub's order, comma-separated, or `-` when none. A nonempty body follows a blank line with trailing newlines normalized to one; an empty body adds no blank line. `--json` prints one object and a newline with exactly `number` (integer), `title`, `state`, `url`, `labels` (names in GitHub's order, `[]` when none), `author` (login) and `body` (as returned by GitHub). HTML characters are not escaped. No comments are fetched. Needs no git checkout. Argument, configuration and API failures leave stdout empty; stdout write failures exit 1. `-h` and `--help` exit 0 before reading configuration or contacting GitHub. Missing/extra arguments and unknown flags exit 2; invalid issue numbers exit 1. |
| `amf pr` | Prints the pull request command group's usage to stdout, as `amf pr help`, `amf pr -h` and `amf pr --help` do. An unknown subcommand prints `amf: unknown pr command "<word>"` and then that usage to stderr and exits 2; one close to a real subcommand, such as `lst`, also prints `Did you mean "list"?` before the usage. |
| `amf pr list [--state open\|closed\|merged\|all] [--limit N] [--issue N] [--json]` | Lists pull requests, including drafts, newest first. See [Pull requests](#pull-requests). |
| `amf pr merge <N> [--squash \| --rebase] [--delete-branch] [--update-branch] [--wait-for-checks]` | Merges a chosen PR (`42` or `#42`); no git checkout needed. See below. |
| `amf pr view <N> [--json]` | Shows one pull request without a git checkout. See below. |
| `amf pr diff <N> [--name-only]` | Prints a pull request's unified diff, or one changed path per line with `--name-only`; no git checkout needed. See below. |
| `amf pr comment <N> (--body text \| --file path)` | Posts a comment on a chosen PR (`42` or `#42`), with configured secrets redacted; no git checkout needed. Exactly one of `--body` and `--file` is required, `--file -` reads stdin, and an empty or whitespace-only comment is rejected. The number must be a pull request; an issue fails with `#N is not a pull request in OWNER/REPO` and nothing is posted. Prints the comment URL. See below. |
| `amf pr close <N> [--comment text] [--delete-branch]` | Closes a chosen PR (`42` or `#42`); no git checkout needed. See below. |
| `amf pr reopen <N>` | Reopens a chosen closed PR (`42` or `#42`); no git checkout needed. See below. |
| `amf pr ready <N>` | Marks a draft pull request ready for review; no git checkout needed. See below. |
| `amf pr draft <N>` | Converts a ready pull request back to a draft; no git checkout needed. See below. |
| `amf tag <command>` | Groups tag operations: `list`, `view`, `create`. Bare `amf tag`, `amf tag help`, `amf tag -h` and `amf tag --help` print the group usage to stdout and exit 0 without reading configuration or contacting GitHub. An unknown subcommand prints `amf: unknown tag command "<word>"` and the group usage to stderr and exits 2; a close match suggests it. amf can create a tag but never moves, force-updates or deletes one. |
| `amf tag list [--limit n] [--json]` | Lists every tag, sorted by name ascending, with every page fetched. Tags have no authoritative creation order, so no tag is called "latest". `--limit N` (default `0`, no limit) keeps the first N after sorting, and when it hides some prints `Showing N of M tags; use --limit 0 to list them all.` to stderr (not with `--json`); a negative value errors. Text columns are `TAG` and `COMMIT`. Empty prints `No tags in owner/name.`; `--json` prints one compact array of objects with exactly `name` and `commit` (the SHA GitHub lists for the tag), or `[]`; `amf tag view` peels annotated tags to the final commit. Needs no git checkout. |
| `amf tag view <name> [--json]` | Shows one tag and the commit it ultimately points to. A lightweight tag uses the ref's commit; annotated tags are peeled recursively through `GET /git/tags/{sha}`. Every response's ref name or tag object SHA is checked against the requested one, so another tag's object is never reported. Missing or incomplete responses, objects that are not commits, repeated tag objects (cycles), and nesting deeper than 16 tag objects are errors. `--json` prints one compact object with exactly `name` and `commit`. Needs no git checkout. |
| `amf tag create <name> --commit <sha>` | Creates a lightweight tag at a validated full lower-case 40- or 64-character commit SHA, after `GET /git/commits/{sha}` confirms it is a commit. Any new tag POST also requires the fixed `amf-release-policy.json` from that SHA and all its required exact-target CI checks/statuses to pass. Tag names follow git ref rules. amf reads `GET /git/ref/tags/{name}` and recursively peels any annotated tag; an existing ref at the same commit printed `Tag <name> already points to <sha>.` succeeds without a write, and a different commit is an error, so amf never moves a tag. If the create POST fails because a concurrent creator won the race, amf re-reads and accepts only the same immutable target. Prints `Created tag <name> at <sha>.` Needs no git checkout. |
| `amf release <command>` | Groups release operations: `list`, `view`, `latest`, `create`, `stage`, `publish`. Bare `amf release` and its help forms print the group usage to stdout and exit 0 without configuration or GitHub access. Unknown subcommands error as for tag. `create` and `stage` never publish; only `publish` does. amf never deletes or overwrites a release or asset. |
| `amf release list [--state all\|published\|draft\|prerelease] [--limit n] [--json]` | Lists releases newest first by creation time, ties by release ID descending, fetching every page. `--state` defaults to `all`; `published` keeps releases that are neither draft nor prerelease, `draft` keeps drafts, and `prerelease` keeps releases marked prerelease. The filter runs before `--limit`, which defaults to `0` and uses the same truncation hint as tag list. Every page of each shown release's assets is fetched from `/releases/{id}/assets`; inline asset arrays are not trusted as complete. Drafts are visible only when the token can see them. Text columns are `TAG`, `STATE`, `ASSETS` and `NAME`; when nothing matches it prints `No releases in owner/name.`, or `No published releases ...`/`No draft releases ...`/`No prerelease releases ...` for a state filter. `--json` prints one compact array, or `[]`; each object has exactly `id`, `tag`, `name`, `state`, `draft`, `prerelease`, `target_commitish`, `created_at`, `published_at`, `url` and `assets`, with each asset having `id`, `name`, `size`, `content_type`, `state`, `download_count` and `url`. Needs no git checkout. |
| `amf release view <tag> [--json]` | Shows the release for a tag and every asset. Displays the actual peeled commit separately from `target_commitish`; the latter is never treated as proof of the tag's ref. A draft whose tag does not exist yet shows `unknown` (JSON `null`) for commit; a published release with a missing tag is an error. `--json` prints one compact object with the release list fields plus `commit`. Needs no git checkout. |
| `amf release latest [--json]` | Shows GitHub's latest published, non-prerelease release through `GET /releases/latest`, with every asset and the tag's peeled commit. When none exists it prints `No published releases in owner/name.` and exits 0; `--json` prints `null`. Needs no git checkout. |
| `amf release create <tag> --plan <file> --manifest <file>` | Creates an unpublished draft release (POST `/releases` with `draft: true`) pinned to the plan's tag, full commit SHA and exact artifact set. The tag must already exist and recursively peel to that SHA. The required policy is read through the contents API pinned to `ref=<sha>` and must parse as strict, versioned JSON with at least one nonempty check or status; absent, empty, malformed or duplicate-key policy fails closed. The draft body stores a versioned ownership record binding repository, tag, SHA, policy path and contents, and each artifact's name, SHA-256 and size; human notes follow the marker. An existing draft is reused only when its record matches the requested plan, tag, SHA, policy and artifact set exactly. An unrelated draft or published release is never modified. Prints `Created draft release <id> for tag <tag> at <sha>.` or an already-matches message so the ID can be used by stage and publish. Never uploads or publishes. |
| `amf release stage <tag> --release <id> --plan <file> --manifest <file>` | Stages the plan's files on the pinned draft release ID. Every local path must resolve inside the plan file's directory, be regular (symlinks are rejected), and be named by exactly one safe single-segment asset name covered exactly once by the `sha256sum`-style manifest; duplicates, extras, missing entries, traversal and ambiguous names are errors. amf copies each file into a bounded temporary snapshot while hashing it, verifies the manifest checksum and records the size, then uploads only the verified snapshot, so a later rename or replacement cannot substitute bytes. Each existing asset is accepted only when its state is `uploaded` and its remote size and downloaded bytes match the record; otherwise, including an unfinished/open asset, an unexpected asset or a publication claim, amf fails closed and never deletes, overwrites or steals it. A lost upload response is resolved by re-listing and verifying the completed remote asset. Rerunning a successful or interrupted stage is safe. Never publishes. |
| `amf release publish <tag> --release <id> --plan <file> --manifest <file>` | Publishes only the pinned draft after rechecking the tag's actually peeled SHA, the exact recorded plan, release identity and policy, every expected artifact's remote size and downloaded bytes, and required CI on the exact commit. Required policy comes from the target commit's policy file and is compared with the durable record. CI follows every page of check runs (including older attempts) and commit statuses; the latest attempt per required name/context must be completed `success` from the recorded app ID or creator ID, and wrong-SHA, missing, pending, failed, inconclusive and wrong-source results fail. amf creates a create-only publication claim asset to serialize cooperating publishers, then sends one PATCH that only clears draft. There is no GitHub CAS or transaction; a lost create response fails closed, and an uncertain PATCH outcome is re-read and verified, otherwise amf refuses and documents manual operator recovery. An already published identical retry verifies and returns without modification. It never modifies an unrelated release. |
| `amf run <N> [--agent A] [--agent-timeout 60m] [--eval-rounds 2] [--dry-run] [--draft] [--part] [--merge[=squash\|rebase] [--wait-for-checks[=false]]] [--log-level L] [--log-file P]` | Processes issue N (`42` or `#42`, surrounding whitespace trimmed) once. Anything else, or `0`, fails with `invalid issue number "X"; use a positive number such as 42 or #42` (exit 1). |
| `amf watch [--agent A] [--interval 10m] [--label amf:ready] [--exclude-label L]... [--agent-timeout 60m] [--eval-rounds 2] [--draft] [--merge[=squash\|rebase]] [--log-level L] [--log-file P]` | Polls open issues with the label (every page), oldest first by issue number (GitHub's `created_at` can tie for issues opened in the same second), and processes them one at a time until interrupted, skipping any issue that has one of the `--exclude-label` labels. `-h` and `--help`, including in the `amf issue watch` form, print the existing watch help and flags to stderr, leave stdout empty, and exit 0 before reading configuration, resolving a checkout, contacting GitHub or starting a runner. Unknown flags and missing flag values remain usage errors (exit 2); help follows normal argument parsing, so an unknown flag before `--help` remains an error. |
| `amf pipeline [--fetch]` | Checks the pipeline in `AGENTS.md` of the enclosing checkout, as it is on disk. See below. |
| `amf doctor [--fix-labels]` | Checks the whole setup, one line per check, changing nothing unless `--fix-labels` is given. See below. |
| `amf update [--version vX.Y.Z] [--check]` | Replaces the running binary with the latest release, or with the given one, which can be a downgrade; surrounding whitespace on `--version` is trimmed. Needs no `.env`. See below. |
| `amf completion <bash\|zsh\|fish>` | Prints a self-contained script for the selected shell to stdout, with no configuration or GitHub access. Scripts complete visible commands, nested subcommands, help topics, shell names, and command-specific long and short flags, including the global flags (--quiet, --color and the path flags); all flags come from the same FlagSets as help and parsing, so adding a flag needs no completion change. Required flag values are skipped when finding the command; `--` stops command and flag suggestions. Scripts never invoke amf or GitHub during completion. Source Bash output in `.bashrc`, Zsh output after `compinit` in `.zshrc`, or pipe Fish output to `source` in `config.fish`; saved scripts must be regenerated after an upgrade to include new commands and flags. Missing or unsupported shells and extra positional arguments print usage to stderr, leave stdout empty and exit 2. `-h` and `--help` print help to stderr and exit 0; `amf help completion` prints the same help to stdout. A script write failure exits 1. |
| `amf version` | Prints `amf <version>`: the release tag, or the module version for `go install`. `amf --version` is the same. |
| `amf man` | Prints a section 1 man page in roff format to stdout, generated from the help overview and every listed command, subcommand and topic, including flags, examples and configuration settings. Output order is deterministic and has no generation date. Needs no configuration, git checkout, GitHub access or external tools. Takes no positional arguments or command-specific flags; extra arguments and unknown flags exit 2. `-h`, `--help` and `amf help man` print help and exit 0. A stdout write failure exits 1. Redirect output to `amf.1` and install it as `man1/amf.1` in a manual directory for `man amf`; see [Man page](COMMANDS.md#man-page). Generation and installation are manual: neither `install.sh` nor `amf update` installs the page. |
| `amf help` | Prints the usage, as `-h` and `--help` do. `amf help <command>` prints that command's help; every `pr` and `issue` subcommand has a topic reachable with `amf help <group> <subcommand>` (including `help`, which prints group usage), without reading configuration or contacting GitHub. Unknown multiword topics follow the same error path as unknown single-word topics. For `issue` and `pr` alone it prints the group usage, as `amf issue help` and `amf pr help` do. `amf help agents` describes agent selection, and `amf help env` lists every configuration setting with its default and purpose, matching the [configuration guide](CONFIGURATION.md#configuration-env) table. The overview command names and descriptions match the [command reference](COMMANDS.md#commands), and its topics match the command reference's [Topics](COMMANDS.md#topics) table. The overview, also printed by bare `amf`, points to `docs/EXAMPLES.md` for examples and `docs/TROUBLESHOOTING.md` for troubleshooting. An unknown topic is an error: `amf: unknown help topic "x"`, a `Did you mean` suggestion when one is close, then the usage on stderr, exit code 2. |

**Safe tag and staged release workflow.** Tags are created explicitly at a
full immutable commit SHA; amf never moves, force-updates or deletes one, and
`target_commitish` is never authority over an existing tag. `amf release
create` records a draft's exact tag, SHA, target policy and artifact plan,
`amf release stage` verifies bounded local snapshots and uploads them
idempotently without deleting anything, and only the separate `amf release
publish` command publishes, after rechecking the immutable target, the durable
record, every remote artifact's bytes and exact-target CI. The required CI
policy is target-committed release policy read from the exact target SHA and
persisted in the ownership record; it is not a claim to infer every current
branch protection or ruleset requirement. An unreadable, absent or empty policy
fails closed. Explicit CLI publish intent does not grant an assistant
permission to publish.

Draft lookup falls back from GitHub's published-by-tag endpoint to the fully
paginated visible release list; multiple matches are rejected as ambiguous.
No new credentials or authentication route is used.

Tag/release inspection and writes refuse HTTP redirects, including repository
checks, so API credentials are not forwarded. Asset downloads go through the
API; a redirect is followed only to the API host or GitHub's asset storage, and
Authorization is removed whenever the host or port changes. Upload URLs are
built by amf and must name exactly this repository and release on an approved
host. Annotated tag aliases are supported: the requested ref and object SHA are
checked, not equality with the object's historical embedded tag name.

The ownership record is strict, versioned JSON in the draft body. Duplicate JSON
keys, duplicate markers, unknown fields and changed, subset or extra plans are
rejected. Human notes after the marker are accepted only when they equal the
recorded notes. GitHub has no atomic tag+asset+publish transaction and does not
provide compare-and-swap; the publication claim serializes cooperating amf
processes, but an external mutation race remains. An interrupted publication
with genuinely ambiguous state is refused, and the drafted release plus claim
are left for an operator to inspect and recover manually; amf never deletes an
unfinished asset or steals a claim.

Native writer revalidation is fail-closed: new tag creation, draft creation and
uploads require exact-target CI, and publish repeats record/identity/policy/tag,
claim-byte and artifact verification after acquiring its create-only claim,
then refreshes current CI and tag immediately before the publish PATCH. JSON
case aliases (including Unicode aliases), missing commit proof, case-renamed
assets and duplicate visible releases are errors. Remote payload hashes stream
with an exact size bound. The only release-policy path is the target-committed
`amf-release-policy.json`, not a local or alternate-policy override. No native
writer deletes tags, assets or claims. Incomplete uploads or claims with a still
unpublished draft require operator investigation; ambiguous actions are never
silently retried. Concurrent external writers remain outside GitHub's non-atomic
check-and-publish boundary; the record is not a cryptographic signature.

For `issue label` and `issue unlabel`, replace the issue number with one or
more selectors to operate on a set: `--state open|closed|all` (`-s`),
`--label NAME` (`-l`), or `--unlabeled`. For example,
`amf issue label --state open --unlabeled amf:ready` queues every open issue
that has no labels. An explicit `--state` or `--label`, or a true
`--unlabeled`, selects bulk mode; omitting selectors retains the single-issue
form. `--unlabeled=false` alone does not select bulk mode. Bulk state defaults
to `open`; `closed` and `all` are also accepted. State ignores case and
surrounding whitespace, and label names have surrounding whitespace trimmed.
A blank `--label`, an invalid state, or combining `--label` with a true
`--unlabeled` errors before reading configuration or contacting GitHub.
`--label` selects issues already carrying that label; `--unlabeled` selects
issues with zero labels of any kind. Selectors combine.
The labels to add or remove remain positional (or `--add`/`--remove` for
`issue label`) and at least one nonblank label is required. Supplying an
issue number with selectors is rejected: in bulk mode a first positional
argument that parses as a positive issue number errors, so use `--add` for
such label names with `issue label`.

Bulk label operations fetch all pages before any writes, exclude pull
requests, and apply the single-issue label rules independently to each
match, newest first (highest number first). Each issue prints the same
result as in single-issue mode, including `#N unchanged`. With no matches,
stdout is `No matching issues in owner/name.` and the command succeeds.
A listing error leaves stdout empty and changes nothing. Processing stops
on the first API or stdout write error (exit 1); an update error names the
issue as `#N: ...`. Earlier changes remain, and an API failure partway through
an issue's label updates can leave that issue partially changed. Single-issue
label commands also return stdout write failures. No git checkout is needed.

- Flags follow one convention on every command (see
  [Flag conventions](COMMANDS.md#flag-conventions)). Each has a long
  `--kebab-case` name and a one-letter short form; a short form means the
  same on every command that has the flag and is unique within each command
  (unrelated commands may reuse a letter). Short
  forms are rewritten to their long names before parsing, so `-a codex`,
  `-a=codex`, `--agent codex` and `-agent codex` are equivalent; they can't
  be bundled (`-nf`). A command's help (`amf <command> --help`, `-h`, or
  `amf help <command>`, which prints the same text to stdout) shows a usage
  line, a description, every visible flag in one aligned column as
  `-x, --long <value>  description (default ...)`, with `-h, --help` last,
  and examples. Every flag and command-list entry stays on one line (no
  description continues onto a second line), and every line of every help
  output fits in 80 columns, including Usage, Examples and See also. Every
  command and subcommand has at least one example,
  including aliases; the overview and group usage also include examples.
  The `auto` examples include inspecting saved progress with `amf auto status`
  and removing the saved run with `amf auto --abort`.
  Command, group and topic help end with a blank line and a
  single `See also: amf help <topic>, amf help <topic>` line linking related
  topics, after any flags and examples. Aliases show the same footer as
  their canonical command; `run` links to `agents`, `env` and `memory`.
  The overview also ends with a See also line.
  `amf help flags` lists the shared letters and states that
  durations are Go durations: `30s`, `5m`, `1h30m`, `2h`. The descriptions of
  `--interval`, `--agent-timeout` and `--stale-timeout` identify Go durations.
- **Output controls.** Every command accepts `-q, --quiet` (default false)
  and `-O, --color=auto|always|never` (default `auto`), before or after the
  command name and before `--`. Quiet suppresses progress, including
  checkout and PostgreSQL connection announcements, while retaining warnings,
  errors, help, command results and the final run summary. It raises the
  terminal log threshold to at least `warn`, even with `--log-level debug`
  or `AMF_LOG_LEVEL=debug`; file logging and summary files keep their
  configured behavior. The final summary remains visible with quiet even
  when the log threshold is `error` or the event log is off.
  Color applies to run/watch/auto's `amf:` prefixes and doctor's status
  labels. Auto uses color only when that output's destination is a terminal,
  `NO_COLOR` is empty or unset, and `TERM` is not `dumb`. Any nonempty
  `NO_COLOR` value disables automatic color; `--color=always` overrides it
  and colors redirected status output too. `--color=never` disables color.
  `NO_COLOR` and `TERM` are special process environment variables, documented
  by `amf help env`; they are not read from `.env` or added to init's template
  (existing `.env` entries are preserved).
  Stored logs and JSON output remain plain. Invalid output flag values
  are usage errors (exit 2), before configuration or GitHub access.
- `--agent-timeout` limits each agent run, not the whole issue; the old
  `--timeout` is accepted as a deprecated alias of it, hidden from help. Both must be at least
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
- `AMF_AGENT_IDLE_TIMEOUT` stops an agent that produces no output, on stdout
  or stderr, and changes no file in its worktree for that Go duration
  (default 30m); `off` disables it. Unlike `AMF_AGENT_TIMEOUT` it has no
  flag. An unparsable, zero or negative value fails `run` and `watch` at
  startup with `AMF_AGENT_IDLE_TIMEOUT="5x": not a duration (use e.g. 30m,
  or off)`, and the default 30m applies when it is unset. The timer resets on
  every write and on every file created, changed or removed in the worktree
  (amf scans it periodically, ignoring `.git/`, at an interval well under the
  limit), so a long run that keeps printing or editing is not stopped. On
  expiry amf stops the run's process group just as the agent timeout does,
  and fails the run with `<agent> produced no output for <limit>; stopped
  it`; the fallback or the next stage can then run.
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
- `--draft` (`-D`, default off; `--draft=false` is the same as leaving it out)
  opens each PR that `run` or `watch` creates as a draft, with or without a
  pipeline. A retry that updates the branch's open PR leaves it a draft or
  ready for review as it was, whatever the flag says; amf never converts a
  PR either way; mark a draft ready for review with `amf pr ready`.
- `--merge` makes `run` and `watch` merge each PR they open or update (step
  11 below), without review. Off by default. Bare `--merge` (or
  `--merge=true`, or `--merge=merge`) makes a merge commit; `--merge=squash`
  squashes and `--merge=rebase` rebases, as GitHub's merge button does, and
  `--merge=false` turns it off. The short form is `-m`. The method may follow
  after `=` or as the next word: `--merge=squash`, `--merge squash`,
  `-m squash` and `-m=squash` are the same. Only the words `merge`, `squash`
  and `rebase` are taken as a method, so `amf run -m 42` merges issue 42's PR
  with a merge commit. Any other method after `=` fails at startup with
  `--merge: want merge, squash or rebase, got "<value>"`. `--merge` with `--draft` fails
  at startup with `--draft and --merge can't be used together: GitHub doesn't
  merge draft pull requests`.
  The `run`, `watch` and `auto` help texts link to
  [Merge methods](COMMANDS.md#merge-methods) for history diagrams and tradeoffs.
- `--part` (`-P`, `run` only; default off) treats the change as one part of a
  larger issue. The commit and PR title are `Part of #N: <title>` instead of
  `Fix #N: <title>`, and their closing line is `Part of #N` instead of
  `Closes #N`, so merging the PR never closes the issue. Any
  `Fix`/`Fixes`/`Fixed`, `Close`/`Closes`/`Closed` or
  `Resolve`/`Resolves`/`Resolved` before `#N`, `owner/repo#N` or the issue's
  `https://github.com/<owner>/<repo>/issues/N` URL in the agent's summary or
  the issue title is rewritten to `Part of`, and `amf-status:` lines are
  removed from the PR body. If a commit the agent made itself would close
  the issue, amf squashes the branch into its own single `Part of #N`
  commit. The agent (or a pipeline's code stage) can mark the same thing by ending its final message with `amf-status: partial`; an
  `amf-status: complete` line confirms a complete change. Without a
  recognized signal, a run is complete, as before. `--part` always wins over
  an `amf-status: complete` line.
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
- **Without `--agent`,** amf runs the primary agent, `AMF_PRIMARY_AGENT`
  (default `codex`), and if it fails, the secondary, `AMF_SECONDARY_AGENT`
  (default `claudecode`). Both take `claudecode` (or `claude`), `codex` or
  `opencode`, ignoring case; the secondary may also be `none`, for no
  fallback. A secondary equal to the primary is the same as `none`. An empty
  value counts as unset. Any other value is a config error that `run`,
  `watch`, `issue run`, `issue watch` and `pipeline` report before doing
  anything, and `doctor` shows as `FAIL agent settings`.
  - **Failure** is any of: the agent isn't usable (below; decided before any
    run), a non-zero exit or error result, the agent timeout or idle timeout,
    finishing without changes, or a change to a refused path. Interrupting
    amf (Ctrl-C)
    is not a failure and falls back to nothing.
  - **Fallback** starts the secondary in a fresh worktree on `amf/issue-N`,
    reset to `origin/<default>`, so nothing the primary left in the worktree
    is kept; a timed-out primary's changes were already saved on its
    `amf/wip/issue-N-<UTC time>` branch. Its
    output goes to `.amf/logs/issue-N-<agent>.log`. amf logs an
    `agent.fallback` event (`from`, `to`, `reason`: `unusable`, `error` or
    `agent_timeout`, `error`). Its message names the agent that didn't run or
    failed, why, and the agent used next: every skipped agent's reason, then
    `falling back to <agent>` (for example `codex is not installed; falling
    back to opencode`), or `<agent> failed (<error>); falling back to
    <agent>`. When the secondary made the change after the primary ran and
    failed, the PR body and the issue comment say `<agent> made the change,
    after amf fell back from <agent> failed: <error>.`
  - **Both failing** fails the issue as one agent failing would, with the
    error `every agent failed: <primary> failed: ..., then <secondary> failed: ...`.
  - **Precedence:** a pipeline's per-stage agent always wins; `--agent` runs
    exactly that agent with no fallback; otherwise the primary and
    secondary. If neither of them is usable, amf picks as before: the first
    usable agent of:

  | Agent | Usable when |
  |---|---|
  | `claude` | installed, and `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` is set |
  | `codex` | installed, and `OPENAI_API_KEY` is set |
  | `opencode` | installed, and either `OPENCODE_MODEL` is free (`opencode/big-pickle`, an `opencode/` id ending in `-free`, or another provider's model), `OPENCODE_API_KEY` is set, or opencode has a saved login for the model's provider |

  The same rule decides whether the primary and secondary are usable. Before
  `AMF_PRIMARY_AGENT`, amf always tried claude first; the default order is
  now codex, then claude.

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
  `usable` or `skipped`. Then it logs which agents issues without a pipeline
  run (`codex, then claude if it fails (primary codex, secondary claudecode)`),
  or a warning that none is available.

**Exit codes:** 0 on success, 1 on error, 2 on a usage error (no command,
an unknown one, a bad flag or the wrong arguments; no command or an unknown
one also prints the usage to stderr; an unknown command close to a real
one, such as `lst`, first prints `Did you mean "issue list"?`), and 3 when
`amf run` skips an issue because of the pre-start guard: an open PR, or an
unmerged leftover `amf/issue-N` branch (nothing changed; `--force` may run
it). The README,
command reference and usage overview document these statuses, and `run`'s
help repeats them. `amf --help` and `amf help <command>` exit 0; some
command-specific `--help` and `-h` paths, including `amf run --help`,
currently exit 2. Scripts needing help to exit successfully should use
`amf help <command>`. Command-specific value validation can exit 1, such as
`amf run 0`. Errors print as `amf: <error>` on
stderr. For SIGINT/SIGTERM handling in `run`, `watch` and `auto`, including
lock release and saved auto progress, see [Signals](../README.md#signals).

### Output

Command data (issue/PR numbers and URLs, tables, JSON and diffs) goes to
stdout; progress logs, warnings and errors go to stderr. Redirect or pipe
stdout to capture results while keeping diagnostics visible. Use `--json`
where available for machine-readable results. Workflow commands such as
`init`, `auto`, `pr merge`, `pr sweep --wait-for-checks` and `update` also
print status messages or prompts to stdout, so their output is not always
a data-only stream. The README and the `amf help output` topic describe
this contract; the help overview lists the topic.

### `amf auto`

For the step-by-step flow and `auto.*` events, see the
[`amf auto` walkthrough](AMF-PROCESS.md#from-a-sentence-amf-auto).

`amf auto "<sentence>"` plans a few issues, creates them labeled `amf:auto`
only, and processes them in order, merging each PR. It adds `amf:ready`
immediately before running each issue. Auto-owned issues waiting their turn
are not picked up by the default `amf watch`, including while auto waits at
its checkpoint. By default, before processing any remaining issues after the
first merge, it prints the merged issue, PR, commit, agent, checks and remaining
issues, then stops for approval. In a terminal, `y` or `yes` approves; any other
answer stops. Without terminal approval, `amf auto --continue` approves a reached
checkpoint and resumes.

- `--checkpoint-after N` delays the single checkpoint until N merges (default
  1). N must be positive; otherwise the command fails with
  `--checkpoint-after must be at least 1`. The checkpoint reports the latest
  merged issue and its commit. There is no checkpoint if no issues remain,
  including when N is at least the number of planned issues.
- `--no-checkpoint` skips the checkpoint entirely and merges all issues without
  prompting or recording approval. It takes precedence over the other
  checkpoint controls. Recovery progress is still saved and removed on success.
- `--approve` accepts the checkpoint without prompting, including when stdin
  is not a terminal. It still prints the checkpoint and records approval.
- `--wait-for-checks` is off by default. At the checkpoint, it polls `CheckRuns`
  on the latest merge commit every 10 seconds, for up to 5 minutes (including
  API requests), until at least one check run appears and all returned runs
  complete. It reports `passed` when every conclusion is `success`, `neutral`
  or `skipped`, and `failed` otherwise. Timeout reports the last observed
  checks; an API error or missing commit reports checks as unavailable.
  These results are printed before approval and do not prevent approval.
  Cancellation stops the wait without approving. Without the flag, checks
  are fetched once and summarized without waiting. `CheckRuns` returns the
  first 100 check runs; commit status contexts are not included.

The threshold, checkpoint-disabled setting and CI wait setting are saved for
retries. On `--continue`, `--checkpoint-after N` overrides the saved threshold;
`--no-checkpoint` and `--wait-for-checks` enable the respective saved setting.
If the checkpoint has not yet been reached, `--continue` resumes processing
without granting early approval. When it has been reached, `--continue`
prints the checkpoint again (including the CI wait if enabled) and approves
without a prompt. `--approve` applies to the current invocation only; once
approval is recorded, there are no further checkpoints.

Issues carrying `amf:auto` are trusted regardless of author, so the token
user need not be in `AMF_TRUSTED`: the plan came from a trusted sentence.
`--dry-run` prints the plan without creating issues or saving a new run.

Before creating any issue, amf saves the full plan to `.amf/auto/state.json`:
schema `version` (currently 1), sentence, repository, merge method, creation
time, approval status, checkpoint settings (`no_checkpoint`, `checkpoint_after`,
`wait_for_checks`), and every planned issue's title, scope, acceptance
criteria and progress. An issue's `number` is 0 and its URL is empty until it
is created. After each successful creation, amf saves its assigned number and
URL before printing them or creating the next issue. Writes use a temporary
file and rename it over the state file.
After processing an issue, the state also records its PR URL and `pr_number`.
Older version 1 files without `pr_number` still load, with the number set to 0.
Older files without checkpoint settings retain the default checkpoint after
one merge and no CI wait.

`amf auto --continue` creates only the saved issues whose number is still 0,
using their original scope, criteria and position in the plan, then resumes
processing. Before retrying an issue with a saved PR number, amf checks the PR
with GitHub. If it is already merged, amf marks the issue `merged`, clears its
saved error, logs `auto.reconciled` and skips running it, even if the issue is
closed. A closed issue without a confirmed merged PR still fails. A PR lookup
error stops the run without discarding the saved PR. A creation error or an
error adding `amf:ready` stops the run with the full plan saved for retry.
Loading an unsupported state version (including an old file without a
version) fails with `unsupported amf auto state version`. Continuing a run
whose saved repository differs from `GITHUB_REPO` fails with a message naming
both repositories, before changing the state or creating or processing issues.
An unfinished run blocks a new run unless `--dry-run` is set; the error
directs users to `amf auto --continue` to resume or `amf auto --abort` to
start over. Add `amf:ready` to any remaining issues to recover them for
processing by `amf watch`. The state file is removed after all issues merge.

`amf auto status` reads the saved state and prints its sentence, merge method,
and each planned issue's number (or `not created`), title, status, PR URL and
commit. PR and commit fields are empty when not yet recorded. This is a
snapshot: it does not reconcile with GitHub, change state, run agents or
create logs. With no state file it prints `No amf auto run saved.` and exits 0.
Malformed or unsupported state is an error.

`amf auto --abort` removes the state file, including corrupt or unsupported
state, so a new sentence can start. It prints
`Aborted amf auto run; saved state removed.` and leaves GitHub issues and PRs
unchanged. With `--close-issues`, it first loads the saved plan and closes
each numbered issue whose saved status is not `merged` using
`PATCH /repos/{repo}/issues/{number}` with `state=closed`. It skips uncreated
and merged issues, requires configured GitHub credentials, and rejects a
saved repository different from `GITHUB_REPO` before changing issues. Any
close failure keeps the state unchanged for retry; issues already closed
before a failure stay closed. State is removed only after all closes succeed.
With no state file, either abort form prints the same no-run message and exits
0. Abort does not stop an active auto process; stop it before aborting.

Status and abort take no sentence or execution flags, and `--close-issues`
requires `--abort`. Inside a checkout, status and plain abort need no
configuration or GitHub access. Outside a checkout, they read configuration
as usual and locate state under `./<repo name>/.amf/auto/state.json`; neither
command clones a repository or creates logs.

### Pull requests

`amf pr list` reads `.env` like `amf issue list`, checks that GitHub can access
`GITHUB_REPO` first, and needs no git checkout. It fetches every page of
`GET /repos/{repo}/pulls` with `sort=created` and `direction=desc`, then sorts
by number descending so the highest number is first even when creation times tie.

- `--state` defaults to `open`, including drafts, and surrounding whitespace is
  trimmed. `merged` requests `closed`
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
  sorting. When it hides some pull requests, the table output is followed on
  stderr by `Showing N of M pull requests; use --limit 0 to list them all.`
  (stdout is unchanged, and `--json` prints no hint). Negative values fail
  with `--limit must be 0 or more, got N`.
- `--issue N` defaults to `0` (no filter). When set, only pull requests whose
  `ISSUE` column is `#N` are kept — that is, a head branch named `amf/issue-N`
  in this repository, so forks and deleted head repositories never match. The
  filter applies before `--limit`, so `--issue 42 --limit 1` is the newest PR
  for issue 42. Negative values fail with `--issue must be 0 or more, got N`
  before anything is read or fetched. A filter that leaves nothing prints
  `No <state> pull requests for issue #N in owner/name.` (`No pull requests
  for issue #N in owner/name.` for `--state all`), and `--json` prints `[]`.
- `--json` defaults to `false`. It prints one array and a newline with exactly
  `number` (integer), `title`, `state` (as in the table), `draft` (boolean),
  `url` (the PR's GitHub page), `head` and `base` (branch names), and `issue`
  (the associated issue integer or `null`). Titles stay as GitHub sent them,
  including tabs. An empty result is `[]`.
- An empty table prints `No <state> pull requests in owner/name.`, or
  `No pull requests in owner/name.` for `all`. Argument, configuration and API
  errors leave stdout empty, including errors on later pages. A failure to
  write stdout is returned as an error.

`amf pr sweep` updates, merges and labels the open amf PRs, and `amf watch
--merge` runs the same sweep each poll. A PR whose head is behind its base is
updated with GitHub's Update branch, then its checks decide whether it waits,
blocks or, with `--merge`, merges; a PR that can't merge is `blocked` and gets
one status comment per head and reason, suggesting `amf run N --force` for a
conflict. `--dry-run` only reads from GitHub: it reports a conflicting PR
(GitHub's `mergeable` false or `mergeable_state` `dirty`) as `blocked` with
that suggestion, not `would-update`; non-conflicting PRs get the read-only
actions (`would-update`, `would-merge`, `waiting`, `ready` or `blocked`). It
exits 1 when at least one PR is blocked and none was merged or updated, so a
sweep that could only block fails; it exits 0 when there was nothing to
sweep or progress was made. With `--dry-run`, `would-merge` and
`would-update` count as progress. See `amf help pr sweep` for the text and
JSON output.

### `amf pr merge`

Reads `.env` and the environment and checks repository access, as `amf issue list`
does. Merges with a merge commit by default; `--squash` squashes and
`--rebase` rebases. Both flags together fail before any GitHub call with
`--squash and --rebase can't be used together`. Exactly one positive PR
number is required; its surrounding whitespace is trimmed. Missing or extra
arguments and unknown flags exit 2;
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

`--update-branch` (`-u`) runs before the merge: `GET
/repos/{repo}/compare/{base}...{head.sha}`, and when `behind_by` is above
zero `PUT /repos/{repo}/pulls/N/update-branch` with `expected_head_sha`,
printing `Updated #N with <base> (was K commits behind)` (or `#N is up to
date with <base>`). It then polls the PR every 2 seconds, up to a minute,
until GitHub reports a new head SHA, and the merge is pinned to that SHA.
GitHub's refusal (a conflict, or a moved head) fails with its message, and
nothing is merged. `--wait-for-checks` (`-w`) then polls the head's check
runs every 10 seconds, using the same wait logic as `amf auto
--wait-for-checks`: it merges when every run passed (success, neutral or
skipped), fails with `checks failed on #N (...); not merging` when one
failed, and fails after 30 minutes with `timed out ...; not merging`. A
head with no check runs after 2 minutes is merged, as the repo may have no
CI.

After a successful merge, a head branch `amf/issue-N` in this repository
marks issue N merged: it reads the issue and, touching only labels that
change, adds `amf:done` and removes `amf:failed`, `amf:pr-open` and
`amf:in-progress`, printing `Labeled #N amf:done` if anything changed.
Forks and other branch names leave labels alone. A labeling failure is a
stderr warning; the command still succeeds. `run`, `watch` and `auto` set
the same final labels after their own `--merge`.

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

`amf pr view <N> [--json]` reads `.env` like `amf issue list`, checks that GitHub can
access `GITHUB_REPO` first, fetches `GET /repos/{repo}/pulls/N`, and needs no
git checkout. `N` may be `57` or `#57`, must be positive, and its surrounding
whitespace is trimmed. Missing or extra
arguments print `usage: amf pr view <number> [--json]` to stderr and exit 2,
before reading configuration or GitHub. A non-number or non-positive argument
prints that same usage line plus `invalid pull request number "<N>": want a
positive number` (with the argument as typed) to stderr, and exits 2, still
before reading configuration or GitHub.

The first line is `#N <title>`, followed by fields aligned at column 12:

- `state:` is `merged` when `merged_at` is set, otherwise `closed` for a
  closed PR, otherwise `open (draft)` for a draft, otherwise `open`.
- `branch:` is the head branch, ` → ` and the base branch.
- `author:` is the login of the user who opened the PR.
- `issue:` is `#N` for a head branch named exactly `amf/issue-N` with a
  positive decimal number without leading zeroes; any other branch is `-`.
- `mergeable:` is `unknown` when GitHub's `mergeable` is `null` (or
  `mergeable_state` is `unknown`) after the command re-reads the PR up to
  three times, pausing 1, 2 and 3 seconds, while GitHub computes it. It is
  `no (conflicts)` when `mergeable` is false or `mergeable_state` is `dirty`,
  `yes` when it is true with a `clean` or empty state, and `blocked (<state>)`
  for any other state, such as `blocked`, `unstable` or `behind`.
- `behind:` (open PRs only) is `up to date with <base>`, `N commit(s) behind
  <base>` from GitHub's compare of the base branch with the head commit, or
  `unknown` when GitHub can't compare them.
- `checks:` (open PRs only) sums up the head commit's check runs: `none` when
  none has reported, else `pending`, `passed` (every run succeeded, or was
  neutral or skipped) or `failed`, then the counts in parentheses, such as
  `passed (5 checks: 5 success)`; `unknown` when GitHub can't list them.
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
`deletions`, `changed_files`, `behind_by` and `ahead_by` (integers, or `null`
for a closed PR or when GitHub can't compare), `checks` (`{"state", "summary"}`
as above, or `null`) and `body`. The body and title are kept as GitHub
sent them, and HTML is not escaped.

GitHub answers 404 both for an issue number and for a number that doesn't
exist; both fail with `#N is not a pull request in owner/name`. On any error
nothing is written to stdout, and a failure to write stdout is an error.

### `amf pr diff`

`amf pr diff <N> [--name-only]` reads `.env` like `amf issue list`, checks that
GitHub can access `GITHUB_REPO` first, and needs no git checkout. `N` may be
`57` or `#57`, must be positive, and its surrounding whitespace is trimmed.
Missing, extra or non-number arguments
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

### `amf pr comment`

`amf pr comment <N> (--body TEXT | --file PATH)` reads `.env` and the
environment and checks repository access, as `amf issue list` does, and needs
no git checkout. Exactly one positive PR number is required; its surrounding
whitespace is trimmed. Missing or extra arguments and unknown flags exit 2,
and an invalid number exits 1.

Exactly one of `--body` and `--file` must be given. `--file PATH` reads the
comment from PATH, and `--file -` reads stdin. Giving both fails with
`use --body or --file, not both`; giving neither fails with
`use --body or --file`. An empty or whitespace-only comment fails with
`comment body can't be empty` before configuration is read or GitHub is
contacted. After checking repository access, the command reads the pull
request with `GET /repos/{repo}/pulls/N`, as `amf pr view` does; when N is
an issue or does not exist it fails with
`#N is not a pull request in OWNER/REPO` and posts nothing. The comment is
then posted with `POST /repos/{repo}/issues/N/comments` `{"body":"..."}`;
configured secrets are redacted like amf's other comments. The command prints the `html_url` of
the created comment, one line on stdout. On any error nothing is written to
stdout, and a failure to write stdout is an error.

### `amf pr close` and `amf pr reopen`

Both read `.env` and the environment and check repository access, as `amf
list` does, and need no git checkout. Exactly one positive PR number is
required; its surrounding whitespace is trimmed. Missing or extra arguments
and unknown flags exit 2, and an invalid number exits 1.

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
required; its surrounding whitespace is trimmed. Missing, extra or
non-number arguments print
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

### `amf auth`

The token comes from one function (`resolveToken`), first hit wins:
`GITHUB_TOKEN`, then `GH_TOKEN` (environment, then the env file); then
`gh auth token --hostname <host>` (3 s timeout, errors ignored); then the token
`amf auth login` saved for the host in `hosts.json`. None: the error
`GITHUB_TOKEN is not set, and neither gh nor amf auth login has a token`. The
source (never the token) goes in `config.load`'s debug event and `amf doctor`.
The host is `AMF_API_URL`'s (`api.github.com` → `github.com`; `<h>/api/v3` → `<h>`),
or `--host`.

- `login`: device flow over `net/http` — `POST /login/device/code`
  (`client_id`, `scope`, default `repo`), print the user code and verification
  URL, open it unless `--no-browser`, poll `POST /login/oauth/access_token`
  every `interval` s (`slow_down` adds 5 s or takes GitHub's interval;
  `authorization_pending` keeps polling; `access_denied`, `expired_token` and
  `device_flow_disabled` stop with a message; `expires_in` bounds it). Then
  `GET /user`, save `{user, oauth_token, scopes}` under the host (temp file +
  rename, 0600; folder 0700), and print the login. The client ID is
  `--client-id`, `AMF_GITHUB_CLIENT_ID`, or `main.defaultClientID` (ldflags);
  empty: an error with the OAuth app registration steps.
- `status`: host, source, token type by prefix, account, `X-OAuth-Scopes`.
- `logout`: removes the host's entry (the file, when empty).
- `token`: prints the token; refuses a terminal stdout without `--force`.

### `amf init`

1. Builds `.env` from a template of `GITHUB_TOKEN`, `GITHUB_REPO`,
   `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`,
   `OPENCODE_API_KEY`, `OPENCODE_MODEL`, `AMF_PRIMARY_AGENT`,
   `AMF_SECONDARY_AGENT`, `AMF_AGENT_TIMEOUT`, `AMF_AGENT_IDLE_TIMEOUT`,
   `AMF_LOG_LEVEL`, `AMF_TRUSTED` and `AMF_POSTGRES_URL`.
   - `OPENCODE_API_KEY`, `OPENCODE_MODEL`, `AMF_PRIMARY_AGENT`,
     `AMF_SECONDARY_AGENT`, `AMF_AGENT_TIMEOUT`, `AMF_AGENT_IDLE_TIMEOUT`,
     `AMF_LOG_LEVEL`
     and `AMF_POSTGRES_URL` are written
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
   agents amf runs with its built-in prompt (the primary, then the
   secondary, as above, with the reason each skipped agent wasn't usable) and exits 0. If none is
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
it depends on failed). Any `FAIL` prints a link to
[the troubleshooting guide](TROUBLESHOOTING.md) on stderr, then ends with
`doctor found N problems` and exit 1; warnings alone exit 0. It never clones,
creates labels or pushes, and never prints a secret; only `--fix-labels`
changes anything (issue labels, below). The checks, in order:

| Check | `ok` when | Otherwise |
|---|---|---|
| `amf` | The running version is the latest release | `warn`: a newer release (run `amf update`), a development build, or no answer from GitHub |
| `git` | `git` runs; shows its version | `FAIL` |
| `.env` | A `.env` was found where `run` looks; shows its path | `warn` if there is none (the environment is used); `FAIL` if it doesn't parse |
| `GITHUB_TOKEN` | A token was found; says its source: `.env`, the environment, `gh auth token` or `amf auth login` | `FAIL` if none or a placeholder |
| `GITHUB_REPO` | Set and `owner/name` | `FAIL` |
| `github` | `GET /user` accepts the token; shows the login. A 403 (a token that can't read `/user`) is `ok`, without a login | `FAIL` on 401 or no connection |
| `scopes` | The token's `X-OAuth-Scopes` (classic and OAuth tokens); `ok` with none reported (fine-grained tokens) | `warn` if scopes are reported without `repo` |
| `repo` | The repo is readable; shows its default branch | `FAIL` on 404; `warn` if the account can't push |
| `labels` | All four amf labels exist | `warn`, naming the missing ones |
| `label state` | Every open or closed issue labeled `amf:done`, `amf:failed` or `amf:pr-open` (and not `amf:in-progress`) agrees with the PRs in this repo that close it: a PR from its `amf/issue-N` branch, any PR whose body has a closing keyword for it, or a merged PR its timeline cross-references at its close time. A merged PR means `amf:done`; otherwise an open PR means `amf:pr-open` (an `amf:failed` issue with an open PR is left to `watch`'s sweep), and neither means `amf:failed`. An issue with two status labels should keep one. Shows how many issues were checked | `warn` per disagreeing issue, naming the label it should have and `amf doctor --fix-labels`; with `--fix-labels`, `ok #N relabeled A → B: why` per issue, or `warn` if relabeling fails. Never causes exit 1 |
| `trusted` | `AMF_TRUSTED` is set; lists the logins | `warn` if unset (anyone's issues run), or if it leaves out the token's account, which `amf issue` opens issues as |
| `agent timeout` | Shows the limit each agent run gets: `AMF_AGENT_TIMEOUT` (and from where) or the 60m default | `FAIL` if `AMF_AGENT_TIMEOUT` isn't a duration or is out of range |
| `checkout` | The checkout `run` would use: the enclosing one, or `./<name>` | `warn` if `run` would clone first or origin isn't `GITHUB_REPO`; `FAIL` if `./<name>` isn't a clone or there's no origin |
| `auto state` | No line when the checkout has no `.amf/auto/state.json`, or there is no checkout | `warn` for a saved run: its quoted sentence, merged and remaining counts (including failed issues), and `amf auto --continue` / `amf auto --abort`; unreadable or invalid state also warns with the error and both commands. This check never causes exit 1 |
| `git push` | `git push --dry-run` to `origin` works with the user's git credentials, without prompting | `FAIL`, with git's message |
| `git identity` | git has a name and email to commit with, in the checkout (or here); shows them | `FAIL`, with the `git config` commands to set them |
| `log folder` | `.amf/runs/` in the checkout can be written: doctor writes and removes a file in it, or in the nearest folder above it while it doesn't exist yet | `FAIL`, naming the folder and the error; `skip` without a checkout |
| `primary agent`, `secondary agent` | `AMF_PRIMARY_AGENT` and `AMF_SECONDARY_AGENT`: the agent, where the value is from (the default, `.env` or the environment), its path and version, and its key status | `warn` if it isn't installed or has no key; `ok` for a secondary of `none` (or equal to the primary): amf doesn't fall back. `FAIL agent settings` for an invalid value |
| `agent order` | Which agents issues without a pipeline run, e.g. `codex, then claude if it fails` | `warn` when neither configured agent is usable and amf falls back to the first usable of claude, codex and opencode |
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

## Repo memory

`amf memory sync|similar|show` and the memory hooks in `run`, `watch` and
`auto`. See [COMMANDS.md](COMMANDS.md#repo-memory) for usage.

- **Store.** A `MemoryStore` interface with three peer backends, chosen per
  invocation by `--memory-backend sqlite|markdown|logfiles` (default
  `sqlite`; any other value is an error naming the three). No environment
  variable selects or locates memory.
- **Location and isolation.** `--memory-dir` (default `os.Getwd()`, made
  absolute) holds `amf-memory/<host>/<owner>/<repo>/`. The key is the
  lowercased host (`api.github.com` → `github.com`) and `GITHUB_REPO`; with no
  repo, `local/<hash of the repo root>`. A key with unusual characters is
  stored under `amf-memory/_hash/<hash>/`. The store records its key (SQLite
  `meta` table, a header line in each log file, front matter in `index.md`
  and a field in `index.json`); every record carries `repo`; opening a store
  recorded for another key fails; every read filters by key.
- **Records.** Issues (number, title, body, state, labels, timestamps); pull
  requests (number, title, body, state, merged, head branch, linked issue);
  runs (id, issue, command, start, finish, outcome, agent(s), exit code, PR,
  error, log tail); explicit issue↔PR and run↔issue/PR links. Label history
  is not kept.
- **SQLite schema.** Versioned by `schema_migrations`; tables `meta`,
  `issues`, `pull_requests`, `issue_prs`, `runs` and the FTS5 table
  `memory_fts` (bm25, title weighted ×2). Pure Go (`modernc.org/sqlite`), no
  cgo.
- **Markdown and logfiles** search with a pure-Go TF-IDF scorer (title tokens
  count twice) over the same records.
- **Sync** pages through issues (`since` the last sync, by `updated_at`) and
  pull requests (newest updated first, stopping at the cursor). Pull requests
  link to issues by `amf/issue-N` head branch or a closing keyword.
- **Similarity** ranks issues, PRs and runs, best first, excluding the issue
  itself. A hit is a duplicate signal when it is merged work and its title
  shares at least 60% of its words with the new title (Jaccard); amf only
  warns (`memory.duplicate`) and never closes anything.
- **Failure tolerance.** For `run`, `watch` and `auto`, any memory error
  (including a repo mismatch) is a `memory.error` warning; memory is then off
  for that process. `amf memory` commands fail on errors.

## Configuration (`.env`)

`amf help env` prints these settings. amf reads one `.env`: `./.env`, or else `.env` at the top of the git
checkout. Environment variables override the file. Values ending in `...`, and `owner/name`, are
placeholders and count as unset, except `AMF_REVIEW_COMMAND`, whose shell commands
may end in the Go package pattern `./...`. The file has `KEY=value` lines, optionally
prefixed with `export ` and quoted; `#` starts a comment line or, after a
space, a trailing comment. A leading byte order mark is ignored.

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
| `AMF_LOG_LEVEL` | `info` (terminal), `debug` (event log) | `debug`, `info`, `warn` or `error`, for both the terminal and the event log of `run` and `watch`; `--log-level` overrides it |
| `AMF_REVIEW_GATE` | `off` | `on` runs the opt-in pre-PR review gate before anything is pushed: `AMF_REVIEW_COMMAND`, then a review agent. Any other non-empty value is an error naming the variable |
| `AMF_REVIEW_COMMAND` | unset | Shell command the review gate runs in the worktree with `sh -c`, as the repo's test gate (for example `test -z "$(gofmt -l .)" && go test ./...`). Unset runs no command step |
| `AMF_CODEX` | `codex` | Path to the Codex CLI |
| `AMF_OPENCODE` | `opencode` | Path to the opencode CLI |
| `AMF_GITHUB_CLIENT_ID` | unset | The GitHub OAuth app `amf auth login` uses, when `--client-id` isn't given |
| `AMF_CONFIG_DIR` | the user config folder's amf | Where `amf auth login` saves tokens (`hosts.json`); environment only |
| `AMF_API_URL` | `https://api.github.com` | GitHub API base URL. `amf pr ready` and `amf pr draft` derive their GraphQL endpoint from it: `…/graphql` appended, or a GitHub Enterprise Server `…/api/v3` becomes `…/api/graphql` |
| `AMF_CLONE_URL` | `https://github.com/<repo>.git` | Where to clone `GITHUB_REPO` from |

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

After validating its arguments, `amf run` prints a link to
[the troubleshooting guide](TROUBLESHOOTING.md) on stderr if setup or issue
processing fails, including a PR that could not merge. The original error
and exit code are preserved.

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
   - **Trust:** issues labeled `amf:auto` bypass the author check. For all
     other issues, with `AMF_TRUSTED` set, an issue whose author (`user.login`,
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
     `.github/`, git or GitHub, and reply with only a PR description ending
     with `amf-status: complete` or `amf-status: partial`.
   - **Environment:** `PWD` is the worktree, and no agent gets
     `GITHUB_TOKEN` or `GH_TOKEN`. Only Claude gets `CLAUDE_CODE_OAUTH_TOKEN`, and only
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
     `AMF_AGENT_TIMEOUT`; the default is 60m). It must also keep producing
     output or changing files: when nothing is written to stdout or stderr,
     and no worktree file is created, changed or removed, for
     `AMF_AGENT_IDLE_TIMEOUT` (default 30m; `off` disables it), amf stops
     it. amf scans the worktree for changes (ignoring `.git/`) periodically,
     at an interval well under the limit. In either case amf signals the
     run's whole process group with SIGTERM, then, if it is still there
     after a 15s grace period, SIGKILL: the agent, the shells it spawns and
     their MCP servers are all stopped, as on Ctrl-C (on Windows,
     `taskkill /T /F` ends the tree at once). The run fails; whatever the
     agent printed stays in its log, ending with a
     `amf: killed after <elapsed> timeout (SIGTERM, then SIGKILL after 15s)`
     line for the agent timeout, or
     `amf: killed after <elapsed>: <agent> produced no output for <limit>;
     stopped it (SIGTERM, then SIGKILL after 15s)` for the idle timeout. An
     agent with a final-message file has what was written kept as
     `.amf/logs/<log>.partial.md`. Before the worktree is removed or reset
     for a fallback, amf commits whatever the agent changed, with the same
     exclusions as a normal commit, on a local-only
     `amf/wip/issue-N-<UTC time>` branch; it prints and logs the branch and
     how to inspect it (`git log -p <default>..amf/wip/...`), and the
     failure comment names it. Nothing is pushed and no PR is opened or
     updated from that branch; an agent that changed nothing leaves no WIP
     branch, and a failure to save one is only a warning. amf logs an
     `agent.timeout` event (with `idle: true` for the idle timeout), an
     `issue.wip` event when it saved partial work, and the issue's comment
     names the agent, the stage and the limit, and how to retry with more
     time or a longer idle limit. A timed-out run pushes nothing and opens
     or updates no PR, and its worktree is removed; the logs, partial
     output and WIP branch stay, as does the local `amf/issue-N` branch,
     which is never pushed and which a retry resets to `origin/<default>`.
   - **Summary:** the final message becomes the summary. Its `amf-status:`
     line is removed; the last `complete` or `partial` value sets the run's
     partial flag, which `--part` also sets.

   With a pipeline, this step and the next are replaced by the steps in
   "With a pipeline" below.
7. **Commit.** Stage everything and commit as `Fix #N: <title>` with the body
   `Closes #N`, or as `Part of #N: <title>` with the body `Part of #N` for a
   partial change; for a partial change, a branch where any commit message
   would close the issue (an agent's own commit) is squashed into that one
   commit. Fail with `<claude|codex|opencode> finished without making
   any changes` if the branch doesn't change any file compared with
   `origin/<default>`, even if the agent made commits of its own. The run
   also fails — before the push, so nothing is pushed and no PR is created —
   when any changed path is under `.github/`, where workflows and `CODEOWNERS` live;
   the error names the offending paths. Changes elsewhere, even under a
   directory whose name contains `.github`, are fine.

   **Review gate** (only with `AMF_REVIEW_GATE=on`; off, the default, leaves
   this and the PR body byte-for-byte unchanged). Before step 8, amf:
   1. Runs `AMF_REVIEW_COMMAND` with `sh -c` in the worktree, if set, with
      the agent environment rules and the same timeout as an agent run. It
      records the command, exit code, duration and the last 60 lines of
      combined output (redacted). A non-zero exit is blocking.
   2. Runs the agent that made the change (for a pipeline, the primary from
      `agentPlan`) with the same model and a review prompt containing the
      issue title and body. The prompt tells the agent to inspect
      `git diff origin/<default> HEAD` itself, check every acceptance
      criterion and look for regressions, security problems, missing tests
      and missing docs; not to modify, create or delete any file and not to
      commit; and to end with exactly one line `amf-review: pass` or
      `amf-review: block`, with blocking findings above it. Its log is
      `.amf/logs/issue-N-review.log`, and agents that write their final
      message to a file use `.amf/review.md` in the worktree.
   3. Blocks when the command failed, the review agent failed or timed out,
      its verdict line is missing or not `pass`, or the worktree changed
      during review (`git status --porcelain` changed, or `HEAD` moved).
      Missing verdict counts as block.

   A blocked gate pushes nothing and creates or updates no PR. The issue
   becomes `amf:failed` with a comment naming the review gate, the reasons,
   the command output and the reviewer's findings (redacted and trimmed),
   and saying the commit is kept on the local branch `amf/issue-N` and in
   the worktree. The worktree and local branch are kept, and amf logs
   `review.block` (warn). A passing gate logs `review.pass`, continues with
   Push, and appends to the PR body a `## Review gate` section: `_amf ran:_`
   the command, exit code and duration (observed), then `_Reviewer's notes
   (written by the agent; amf has not verified them):_` the reviewer's text
   without the verdict line. `--dry-run` skips the review gate.
8. **Push.** If `origin/amf/issue-N` doesn't exist, or its tip is an
   ancestor of the new head, amf runs `git push -u origin amf/issue-N` with
   the user's git credentials. Otherwise it replaces the remote tip with
   `git push --force-with-lease=amf/issue-N:<old-tip> -u origin amf/issue-N`,
   pinned to the tip it just inspected, so it never clobbers a push it
   didn't see. If `origin/amf/issue-N` has commits that are neither on the
   base nor in the new head and aren't already merged (by ancestry or a
   merged PR at that tip), amf first pushes its old tip to
   `amf/backup/issue-N-<shortsha>-<UTC time>`, logs it, and names it in the
   issue comment. If that backup push fails, amf stops without replacing
   the branch. A merged leftover is reused without a backup; when only a
   merged PR proves the merge, the new head replaces its tip.
9. **PR.** Open `amf/issue-N` → `<default>`, titled `Fix #N: <title>`, or
   `Part of #N: <title>` for a partial change. If the branch already has an
   open PR (a retry), the push has updated it, so amf replaces that PR's
   title and body instead. With `--draft`, the create request sends
   `"draft": true`, so a new PR opens as a draft; without it the request has
   no `draft` field and the PR is ready for review. An updated PR keeps its
   draft or ready state: the update sends only the title and body.
   The body starts with `## Changes` and, in a line marked as git-observed
   rather than agent-written, a fenced `git diff --stat origin/<default>
   HEAD`, then `Changed files:` and one backticked path per changed file. It
   then has `## Agent summary`, a line saying the agent wrote it and amf has
   not verified it, the summary (or `Resolves #N.` / `Part of #N.` if there
   isn't one), and any pipeline sections. It ends with `Closes #N` (or
   `Part of #N` for a partial change) and an "Opened by amf" footer. amf
   first removes Claude attribution lines ("Generated with Claude Code",
   `Co-Authored-By`) and the agent's own `Closes`/`Fixes`/`Resolves #N` line
   from the summary, and the attribution lines from the plan and review too.
   For a partial change it rewrites any other closing keyword for the issue
   to `Part of`. That way each PR is credited to the user alone and closes
   the issue once, and a partial PR never closes it.
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
12. **Finish.** Set `amf:done` if amf merged the PR, else `amf:pr-open`, and
    comment on the issue:
    - without `--merge`: `amf opened <PR URL> for this issue.` (`updated`
      for a retry's PR);
    - merged: `amf opened <PR URL> for this issue and merged it.` (`squashed
      and merged`, `rebased and merged`); the PR's `Closes #N` then closes
      the issue (a partial PR's `Part of #N` does not);
    - not merged: `amf opened <PR URL> for this issue, but could not merge
      it:`, the reason in a code block, and that the PR is still open for
      review. The issue is `amf:pr-open`, and `run` exits 1 with `opened
      <PR URL> but could not merge it: <reason>`, and `watch` logs that as
      a failure and goes on.

    The PR exists by now, so amf sets its final status even when watch is
    stopping, with a short timeout: a Ctrl-C doesn't leave `amf:in-progress`
    next to `amf:pr-open`. If the status still can't be set, amf logs a
    warning, keeps the real PR status (never `amf:failed`), comments as
    usual, and `run` exits 1 with `opened <PR URL> but could not set
    <label>: <reason>`; `watch` logs that failure and goes on. A failed
    comment is still only a warning.

### Stale pull requests

When the guard is skipped (`amf run N --force`, `amf auto`) and the issue's
`amf/issue-N` PR is open and behind the default branch (compare
`behind_by` > 0), amf brings that PR up to date instead of rebuilding it,
after Claim and the author check:

1. Fetch the default branch and `amf/issue-N`; create the worktree and reset
   it to `origin/amf/issue-N`.
2. `git merge --no-ff -m "Merge origin/<default> into amf/issue-N"
   origin/<default>`.
3. Clean: no agent runs. Conflicts: the files `git diff --name-only
   --diff-filter=U` lists go to one agent, the pipeline's code stage agent,
   else the first agent of the run plan, with a prompt naming only those
   files and telling it to keep both sides and not commit. Its log is
   `.amf/logs/issue-N-conflicts.log`. A file left with a line starting
   `<<<<<<< ` or `>>>>>>> `, or a path still unmerged after `git add -A`,
   fails the run (the merge is aborted, nothing is pushed). Otherwise amf
   commits the merge.
4. Conflicts with no usable agent: amf comments on the PR listing the
   conflicted files, aborts the merge, pushes nothing and fails the run
   with `merging origin/<default> into amf/issue-N conflicts in …, and no
   usable agent can resolve it`.
5. The `.github/` refusal applies to the merged tree. Push without
   `--force` (the merge only adds commits), then continue at Comment and
   label, and `--merge`, as for an updated PR. The PR's title and body are
   left as they were.

An open PR that isn't behind (or a compare that fails) is rebuilt by the
usual steps.

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
   - The final message becomes the summary. Its `amf-status:` line is
     removed; the last `complete` or `partial` value sets the run's partial
     flag, which `--part` also sets.
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
     amf pushes the branch for inspection (plain, or with a lease when the
     remote tip diverged, as in Push) and fails the issue. The comment
     names the eval agent, the number of rounds and the branch, and puts the
     last review in a collapsed block.
5. **Squash.** Replace the round commits with one commit, as in step 7,
   including the `Part of #N` title and body when the run is partial, and
   continue with Push.

**Pipeline sections in the PR body:** with an eval stage, an
`## Evaluation` section saying `PASS`, the eval agent and model, and the
number of code rounds, with the review in a collapsed block. With a plan
stage, the plan in a collapsed block. They appear under `## Agent summary`,
which still says amf has not verified the agent text.

### Dry run (`amf run --dry-run`)

amf only reads from GitHub: the issue, the repo and the branch's open PR.

1. **Preflight, Checkout and Check** run as in steps 1 to 3, and stop the
   run in the same cases.
2. **No claim.** The issue's labels aren't touched, so it stays as it was
   (`amf:ready`, say). The trust check of step 4 still runs, since the
   agent still runs the issue's text. Issues labeled `amf:auto` bypass it;
   otherwise an untrusted author fails the run.
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
   against `origin/<default>`, `Title: Fix #N: <title>` (or
   `Part of #N: <title>` for a partial change), the PR body as step 9 would
   post it, and `Nothing was pushed, and the issue and GitHub are unchanged.
   To see the change: git diff origin/<default> amf/issue-N`.
5. **Clean up.** The worktree is removed. The local branch and the logs are
   kept. A later run, dry or not, resets the branch to `origin/<default>`.

A dry run that fails exits 1 with the error, and leaves the issue as it
was: no label and no comment. Its worktree is kept for inspection, except
after the agent timeout or idle timeout, which remove it as usual; partial
work is still saved on an `amf/wip/issue-N-<UTC time>` branch.

**On failure** in any step after Claim, up to opening the PR (a failed
merge doesn't count; see step 11): set `amf:failed` (or keep `amf:pr-open`
when the branch still has an open PR, and `amf:done` when it was done),
comment the error on the issue (each agent tried with its exit code and
duration, whether the fallback ran and why not, and the redacted last 40
lines of the log), and keep the worktree and logs for inspection. A review
gate that blocks is this kind of failure: nothing is pushed, no PR is
created or updated, and the commit stays on `amf/issue-N` in the worktree.
This also happens if
the run is interrupted — except that a run that hit the agent timeout or
idle timeout leaves the worktree (removed, with its branch unpushed), keeps
only the logs and partial output, and saves the agent's changes on an
`amf/wip/issue-N-<UTC time>` branch. If Claim itself fails, the issue can be left on
`amf:in-progress`, which `watch` skips until the stale sweep puts it back
to `amf:ready`; `amf run N` recovers it too.

## Updating (`amf update`)

`amf --update` and `amf -u` are compatibility aliases of `amf update`, with
the same flags, output and exit status. Root `-u` is free under the
per-command short-flag convention; `amf pr merge -u` remains
`--update-branch` on that command only. (The update command's own
`-V/--version <tag>` selects a release tag; it is unrelated to root
`amf --version`.)

`amf help update` and the README's Install section link to
[release notes](https://github.com/vicgupta/auto-mini-factory/releases)
for changes in each version.

1. Resolve the target: `--version` (surrounding whitespace is trimmed, then
   a leading `v` is added if missing), or
   the tag that `https://github.com/vicgupta/auto-mini-factory/releases/latest`
   redirects to. If it equals the running version, print `amf <v> is up to
   date` and stop. `--check` stops here too: it prints `amf <v> is available
   (you have <current>); run amf update` for the latest release, or
   `amf <v> is available (you have <current>); run amf update --version <v>`
   for an explicit target, using the normalized version. Checking does not
   download or install a binary and exits successfully. When `--version` names
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
| `.amf/auto/state.json` | Versioned full `amf auto` plan and progress, saved before creation and after each created or processed issue |
| `.amf/worktrees/issue-N/` | Working copy for an issue in progress or failed |
| `amf/wip/issue-N-<UTC time>` (local branch) | A killed agent's partial work, committed and never pushed |
| `.amf/logs/issue-N.log` | The agent's output for the latest run of issue N, without a pipeline (stream-json for Claude) |
| `.amf/logs/issue-N-review.log` | The review agent's output when `AMF_REVIEW_GATE=on` |
| `.amf/logs/issue-N-<agent>.log` | The secondary agent's output, when amf fell back to it |
| `.amf/logs/issue-N[-<stage>].partial.md` | An agent's final-message file, kept (redacted) after a timeout cut the run short |
| `.amf/logs/issue-N-<stage>.log` | Each pipeline stage's output: `plan`, `code-K`, `eval-K` |
| `.amf/runs/issue-N/` | The latest pipeline run's `plan.md` and `eval-K.md` |
| `.amf/runs/<run_id>/events.jsonl` | The event log of one `run` or `watch` process, unless `--log-file` says otherwise |
| `.amf/runs/<run_id>/summary.json` | That process's summary, rewritten after each issue and at exit |
| `.amf/opencode.json` | The config amf runs opencode with |
| `<config-dir>/hosts.json` | Tokens saved by `amf auth login`, per host (0600, folder 0700); `<config-dir>` is `AMF_CONFIG_DIR` or `os.UserConfigDir()/amf` |
| `<memory-dir>/amf-memory/<host>/<owner>/<repo>/` | The repo's memory (see [Repo memory](#repo-memory)) |

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
| `agent.fallback` | warn | Without a pipeline, the primary agent failed or wasn't usable: `from`, `to`, `reason` (`unusable`, `error` or `agent_timeout`, which also covers an idle timeout) and `error` |
| `log.file` | info | `path`: the event log |
| `run.summary` | debug, info | The fields of `summary.json` (see Commands) as `done`, `failed`, `skipped`, `github_calls`, `ratelimit_lowest`, `git_calls`, `agent_ms`, `errors`, `log`, and `final`: false (debug) after an issue, true (info, the terminal's last line) at exit |
| `config.load` | debug | `paths` (the `.env` files looked for), `file` (the one read, or `""`), and `keys`: for each variable read, its `source` (`env`, `.env` or `default`) and `value`. Only `GITHUB_REPO`, `AMF_API_URL`, `OPENCODE_MODEL`, `AMF_AGENT_TIMEOUT`, `AMF_AGENT_IDLE_TIMEOUT`, `AMF_STALE_TIMEOUT`, `AMF_LOG_LEVEL`, `AMF_CLAUDE`, `AMF_CODEX` and `AMF_OPENCODE` show their value; every other one shows `set` or `unset` |
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
| `issue.skip` | info | `reason` (`pr_open` or `branch_exists`), plus `pr` and `url` for `pr_open`, or `branch` for `branch_exists`; the message says how `--force` may run the issue |
| `issue.reuse` | info | `branch`, `sha` (the old remote tip) and `reason` (`ancestor` or `merged_pr`): a leftover branch whose work is already merged is reused |
| `issue.picked` | info | `title`, `author`, `labels` |
| `issue.agent_ignored` | info | `agent`: the `--agent` a pipeline overrides |
| `issue.author_check` | debug | `author`, `trusted`; an untrusted issue then fails |
| `issue.status` | debug | `from`, `to`: an `amf:*` label change (`none` when there was none); warn with `to` and `error` when setting it failed |
| `issue.pipeline` | info / debug | `declared` (true, info, for a pipeline; false, debug, for one agent run), `stages`, `agents`, `models` |
| `issue.skill` | info | `stage`, `url` |
| `issue.worktree` | info | `path`, and `branch` and `base`, or `stale: true` when an old one is removed |
| `issue.wip` | info, warn | Saved a killed agent's partial work: `branch` and `base`; warn with `error` when saving it failed |
| `issue.agent` | info | `agent`, `model`, `timeout`: the one run without a pipeline |
| `issue.stage` | info | `stage` (`plan`, `code-K`, `eval-K`), `agent`, `model`, `timeout` |
| `issue.plan.done` | debug | `bytes`: the plan's length |
| `issue.plan.blocked` | debug | `question`: the first one |
| `issue.code.round` | debug | `round` |
| `issue.code.done` | info | `rounds`, without an eval stage |
| `issue.eval.verdict` | info | `round`, `pass`, `parsed` (whether eval gave a verdict), and `next_round`, or `final: true` when no rounds are left |
| `review.command` | debug | The review gate's test command: `command`, `exit_code`, `duration`, `output` (the redacted last 60 lines) |
| `review.pass` | info | `agent`, `command`, `exit_code`, `duration`, `pass: true` |
| `review.block` | warn | `reasons`, `agent`, `command`, `exit_code`, `duration`, `worktree` |
| `agent.start` | debug | `agent`, `binary`, `args`, `model`, `stage` (`agent run` without a pipeline), `dir`, `timeout`, `stdin_bytes` and `stdin_sha256` of the prompt, and `env_added` and `env_removed`: the names of the variables amf added, changed or removed for the agent, never their values |
| `agent.exit` | debug | `agent`, `stage`, `exit_code` (-1 when killed or not started), `signal` when a signal ended it, `timed_out`, `duration_ms`, `stdout_bytes`, `stderr_bytes`, `out_bytes` (codex's and opencode's final-message file), and `result_bytes` and `result` (the final message's first 512 bytes), or `error` |
| `agent.parse` | debug | Claude: `result_found`, `json` (whether its output was JSON); codex and opencode: `out_file` (whether they wrote their final message) |
| `agent.timeout` | warn | `agent`, `stage`, `limit`, `elapsed`, `grace`, `pgid`, `bytes` (stdout so far), `log`, and `idle: true` when the idle timeout stopped it |
| `issue.push` | info | `branch` |
| `issue.pr` | info | `action` (`opened`, `updated`, or `updating` when the branch has an open PR), `number`, `url` |
| `issue.merge` | info, warn, debug | `action`, `method`, `number`: `merging` (info) with `sha`, then `merged` (info) with `commit` (GitHub's merge commit) and `attempts`, or `failed` (warn) with `error`; `retry` (debug) with `attempt` and `error` when GitHub said not mergeable yet |
| `issue.comment` | debug | `bytes`; warn with `error` when posting failed |
| `issue.cleanup` | warn | `error`: a worktree that couldn't be removed or pruned |
| `issue.done` | debug | `pr`: its number; `dry_run: true` instead after a dry run |
| `issue.dry_run` | info | `branch`, `base`, `head` (the local commit), `files` (the paths it changes), `action` (`open` or `update`), `url` (the PR to update, or `""`), `draft`, `merge` (the method, `off`, or `refused` with `merge_error`), `title`, `body` |
| `issue.failed` | debug, info | `reason` (`agent_timeout` at info, with `stage`, for the agent timeout or idle timeout; `stage` for a stage's own failure such as a blocked plan or an untrusted author; else `error`), `error`, and `dry_run: true` in a dry run |
| `github.call` | debug | One per GitHub API request: `method`, `path` with `{repo}`, `{n}` and `{label}` in place of the repo, numbers and label names, `status` (0 when there was no response), `duration_ms`, `bytes_out` (request body), `bytes_in` (response body read), `ratelimit_remaining`, `ratelimit_reset`, `request_id` (GitHub's headers, `""` when absent), `page` for a later page of a list, and `error_body` (the first 512 bytes of an error response) or `error`. The token is never logged |
| `git.exec` | debug | One per git command: `subcommand`, `args` (a commit message cut to 80 characters), `dir`, `exit_code` (-1 when git didn't start), `duration_ms`, `stdout_bytes`, `stderr_bytes`, and on failure `stderr` (its first 512 bytes) |
| `git.push` | debug | After `git.exec` for a push: `branch`, `forced` (`--force` or `--force-with-lease`), `exit_code` |
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
  `AMF_TRUSTED` limits amf to issues opened by the listed users, except
  issues labeled `amf:auto`, which are trusted regardless of author because
  the plan came from a trusted sentence. Other people can't edit issues
  opened by the listed users, apart from collaborators with write access.
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
- No agent gets `GITHUB_TOKEN` or `GH_TOKEN`, and each agent's environment holds only
  its own key (step 6), but agents can read local files, including `.env`
  (and so every key in it, and `AMF_POSTGRES_URL`'s password) and amf's
  `hosts.json`, and can run `gh auth token`.
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

## Paths

Every path default resolves at run time to the current directory
(`os.Getwd`, made absolute) or a subpath of it, unless a command-line flag
names one. `$PWD`, `$HOME` and the OS temp folder never decide a default.
All commands take `-g, --dir <folder>` (change into DIR first),
`-E, --env-file <file>` (read and, for `amf init`, write only FILE) and
`-G, --clone-dir <folder>` (the checkout run, watch, auto and doctor use; it
wins over the enclosing checkout, and is cloned when missing), before or after
the command name. Relative values resolve
against the directory amf was started in; `~` is not expanded. run, watch and
auto also take `--log-file` and `--memory-dir`, made absolute the same way. No environment
variable sets a path; `AMF_CLAUDE`, `AMF_CODEX` and `AMF_OPENCODE` name
binaries, looked up on `PATH` when bare.

Left as they are: `amf update` writes to the running executable's path (and its
temporary file next to it); `amf init` and `amf doctor` create temporary files
next to the file they write or in the folder they test, and remove them at once;
`amf auth` keeps tokens in `AMF_CONFIG_DIR` or `os.UserConfigDir()/amf`, never
the current directory, so a credential can't land in a checkout an agent works in.
