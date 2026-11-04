package main

// subHelp is the help for each two-word subcommand: amf pr merge --help
// and amf help pr merge.
var subHelp = map[string]string{
	"memory sync": `Usage:
  amf memory sync [flags]

Backfills or refreshes the repo's memory from GitHub's issues and pull
requests: only what changed since the last sync, or everything with --full.
amf help memory describes the backends.
`,
	"memory similar": `Usage:
  amf memory similar <issue-number | "text"> [flags]

Ranks past issues, pull requests and runs like an issue or a piece of text.
amf help memory describes the backends.
`,
	"memory show": `Usage:
  amf memory show <issue-number> [flags]

Shows an issue's full history from memory: its pull requests and every run.
amf help memory describes the backends.
`,
	"issue create": `Usage:
  amf issue create "Title" [flags]
  amf issue "Title" [flags]

Creates an issue labeled amf:ready. The body comes from --body, --file, or
stdin when it is not a terminal.

--body sets the body, even when empty; --file reads a file, or stdin for
--file -. --body and a nonempty --file conflict. --label replaces amf:ready;
--no-label creates an unlabeled issue and conflicts with an explicit --label;
a blank label is rejected. Surrounding whitespace in the title and label is
trimmed. Prints #N <url>.
`,
	"issue label": `Usage:
  amf issue label <issue-number> [flags]

Adds and removes labels on an issue: --add and --remove may repeat or take
comma-separated names. Labels already present (or absent) are skipped.
Adding an amf status label (amf:in-progress, amf:pr-open, amf:done,
amf:failed) removes the issue's other status label, so an issue keeps one.
Prints #N: added ...; removed ..., or #N unchanged.
`,
	"pr list": `Usage:
  amf pr list [flags]

Lists pull requests, newest first, with the issue each was opened for.
Drafts are included.

--state open (the default) lists open PRs; closed excludes merged ones,
merged selects only merged ones, and all includes everything. --issue keeps
the PRs for one issue, before --limit applies (0, the default, means no
limit). The issue comes only from a canonical amf/issue-N head branch in
this repository. Text columns are PR, ISSUE, STATE, TITLE (- for no issue);
--json prints an array of {number, title, state, draft, url, head, base,
issue}, or [] for no matches.
`,
	"pr merge": `Usage:
  amf pr merge <number> [flags]

Merges a pull request: a merge commit, or --squash or --rebase.

--squash and --rebase conflict. Merged, closed and draft PRs are refused.
The merge is pinned to the fetched head SHA; GitHub's required checks and
reviews still apply. --delete-branch deletes the head afterwards only when it
is in this repository; a deletion failure is an error after the merge has
already succeeded. Prints Merged #N <url>, Squashed and merged #N <url>, or
Rebased and merged #N <url>.
--update-branch first merges the base into a head that is behind (GitHub's
Update branch) and merges the new head; --wait-for-checks waits up to 30
minutes for the head's checks to pass, failing if one fails (a head with
no checks after 2 minutes is merged).
When the head is amf/issue-N in this repository, issue N is labeled
amf:done and loses amf:failed, amf:pr-open and amf:in-progress.
`,
	"pr sweep": `Usage:
  amf pr sweep [flags]

Sweeps the open pull requests amf opened: those with an amf/issue-N head in
this repository whose issue is labeled amf:pr-open. Drafts are skipped.

For each, oldest first: a branch behind its base is updated (GitHub's Update
branch, as amf pr merge --update-branch does), and its checks then run on
the new head, so the next sweep merges it (--wait-for-checks waits instead).
With --merge, a PR whose checks passed (or that has none) is merged with
that method, pinned to its head, and its issue labeled amf:done; without
--merge nothing is merged. A PR that can't merge (a conflict, failed checks,
a change to AGENTS.md, or GitHub refusing) gets a status comment, once per
head and reason. Pending checks are left for the next sweep.

In a checkout, the sweep holds the repo lock (.amf/watch.lock), so it
fails while watch, run or auto works there. --dry-run only reads from
GitHub. amf watch --merge runs the same sweep each poll.
Prints one line per PR: #N (issue #M) <action>[: reason] <url>, where the
action is merged, updated, waiting, ready, blocked, or would-update or
would-merge with --dry-run. --json prints an array of {number, issue, url,
action, reason, commit}.
`,
	"pr view": `Usage:
  amf pr view <number> [flags]

Shows a pull request: title, state, branches, checks and body.

Also shows the author, inferred issue, mergeability, change counts and URL.
For an open pull request it also shows how many commits its branch is behind
the base, and its head commit's checks: none, pending, passed or failed.
While GitHub is still computing mergeability, it is re-read a few times
(about 6 seconds in all) before showing unknown.

--json prints one object with number, title, state, draft, url, head, base,
author, issue, mergeable, mergeable_state, additions, deletions,
changed_files, behind_by, ahead_by, checks ({state, summary}) and body;
issue, mergeable, behind_by, ahead_by and checks may be null.
`,
	"pr diff": `Usage:
  amf pr diff <number> [flags]

Shows a pull request's diff, or one changed path per line.

--name-only prints one changed path per line, fetching every page of
changed files.
`,
	"pr close": `Usage:
  amf pr close <number> [flags]

Closes a pull request, optionally commenting first. Issue labels are left as
they are.

--comment posts its text before closing. --delete-branch deletes the head
afterwards only when it is in this repository. A PR already closed succeeds
without commenting or deleting; a merged PR fails. Prints Closed #N <url>, or
#N is already closed.
`,
	"pr reopen": `Usage:
  amf pr reopen <number>

Reopens a closed pull request.

An open PR succeeds unchanged; a merged PR fails. Issue labels are left as
they are. Prints Reopened #N <url>, or #N is already open.
`,
	"pr ready": `Usage:
  amf pr ready <number>

Marks a draft pull request ready for review.

Closed or merged PRs fail. Issue labels are left as they are. Prints
#N is ready for review <url>, or #N is already ready for review.
`,
	"pr draft": `Usage:
  amf pr draft <number>

Converts a pull request to a draft.

Closed or merged PRs fail. Issue labels are left as they are. Prints
#N is now a draft <url>, or #N is already a draft.
`,
}

// helpExamples are the short examples at the end of a command's help.
var helpExamples = map[string]string{
	"memory sync":    "  amf memory sync\n  amf memory sync --full --memory-backend markdown\n",
	"memory similar": "  amf memory similar 42\n  amf memory similar \"login redirect loops\" --limit 10 --json\n",
	"memory show":    "  amf memory show 42 --json\n",
	"init":           "  amf init\n",
	"doctor":         "  amf doctor\n  amf doctor --fix-labels\n",
	"version":        "  amf version\n",
	"issue label":    "  amf issue label 42 --add amf:done\n  amf issue label 42 --remove amf:failed --add amf:ready\n",
	"issue create": `  amf issue create "Fix the login redirect" --body "Users land on /404 after login"
  amf issue create "Add a dark mode" --file spec.md --no-label
`,
	"list": `  amf issue list --label amf:ready
  amf issue list --state all --search login --limit 10 --json
`,
	"run": `  amf run 42
  amf run 42 --agent claudecode --agent-timeout 90m
  amf run 42 --dry-run
  amf run 42 --merge=squash          # or: -m squash
`,
	"watch": `  amf watch
  amf watch --interval 2m --exclude-label blocked
  amf watch --agent codex --merge=rebase
`,
	"auto": `  amf auto "add a --verbose flag to the CLI"
  amf auto "add a health check endpoint" --no-checkpoint --merge=squash
  amf auto --continue --approve
  amf auto status
`,
	"pipeline":  "  amf pipeline\n  amf pipeline --fetch\n",
	"update":    "  amf update --check\n  amf update --version v0.0.7\n",
	"pr list":   "  amf pr list --state merged --limit 5\n  amf pr list --issue 42 --json\n",
	"pr merge":  "  amf pr merge 57 --squash --delete-branch\n  amf pr merge 57 -u -w --squash\n",
	"pr sweep":  "  amf pr sweep --dry-run\n  amf pr sweep --merge=squash --wait-for-checks\n",
	"pr view":   "  amf pr view 57 --json\n",
	"pr diff":   "  amf pr diff 57 --name-only\n",
	"pr close":  "  amf pr close 57 --comment \"Superseded by #58\" --delete-branch\n",
	"pr reopen": "  amf pr reopen 57\n",
	"pr ready":  "  amf pr ready 57\n",
	"pr draft":  "  amf pr draft 57\n",
}

func init() {
	helpExamples["issue list"] = helpExamples["list"]
	subHelp["issue list"] = listHelp
	subHelp["issue run"] = runHelp
	subHelp["issue watch"] = watchHelp
}

const flagsHelp = `Flag conventions

Every flag has a long form, --full-word, which help and the docs use. Common
flags also have a one-letter short form, shown first in each command's help:
  -a, --agent <name>
A letter keeps its meaning on every command that has the flag.

  -a  --agent            run, watch, auto
  -b  --body             issue create
  -B  --no-browser       auth login
  -C  --client-id        auth login
  -c  --comment          pr close
  -c  --continue         auto
  -c  --check            update
  -d  --delete-branch    pr merge, pr close
  -e  --eval-rounds      run, watch, auto
  -f  --file             issue create
  -f  --force            run, auth token
  -h  --help             every command
  -H  --host             auth login, auth status, auth logout, auth token
  -i  --interval         watch
  -i  --issue            pr list
  -j  --json             issue list, pr list, pr sweep, pr view, memory
  -l  --label            issue create, issue list, watch
  -L  --limit            issue list, pr list, memory
  -m  --merge            run, watch, auto, pr sweep
  -n  --dry-run          run, auto, pr sweep
  -o  --scopes           auth login
  -r  --rebase           pr merge
  -s  --state            issue list, pr list
  -s  --squash           pr merge
  -S  --search           issue list
  -t  --agent-timeout    run, watch, auto
  -u  --update-branch    pr merge
  -w  --wait-for-checks  auto, pr merge, pr sweep
  -x  --exclude-label    watch
  -y  --approve          auto

Values follow the flag as the next word or after =: --agent codex,
--agent=codex, -a codex and -a=codex are the same. A boolean flag takes no
value (--dry-run), or =true / =false.

Durations are Go durations: 30s, 5m, 1h30m, 2h.

--merge's method is optional: --merge alone is a merge commit, and
--merge=squash, --merge squash, -m squash and -m=squash all squash. Only the
words merge, squash and rebase are taken as its method.

Go's flag parser also accepts one dash before a long name (-agent) and two
before a short one (--a), so older spellings such as -b, -f and --timeout
keep working; --timeout is a hidden, deprecated alias of --agent-timeout.
Flags without a short form are rarer ones, spelled out to stay clear.
The global path flags --dir, --env-file and --clone-dir work on any command,
before --, and have no short form.
`
