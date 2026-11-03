package main

// subcommandHelp supplies details without changing the top-level overview.
var subcommandHelp = map[string]string{
	"issue create": `Usage:
  amf issue create "Title" [-b body | -f file.md | -f -] [--label L] [--no-label]

Creates an issue; amf issue "Title" is a shortcut.
-b selects the body, even when empty; -f reads a file, or stdin for -f -.
Without either, piped stdin supplies the body. -b and nonempty -f conflict.
--label defaults to amf:ready; --no-label creates an unlabeled issue.
An explicit --label conflicts with --no-label; a blank label is rejected.
Title and label surrounding whitespace is trimmed. Prints #N <url>.
There is no --json flag.
`,
	"issue list": `Usage:
  amf issue list [--state open|closed|all] [--label L] [--search TEXT] [--limit N] [--json]

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
`,
	"pr list": `Usage:
  amf pr list [--state open|closed|merged|all] [--limit N] [--issue N] [--json]

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
`,
	"pr merge": `Usage:
  amf pr merge <N> [--squash | --rebase] [--delete-branch]

Merges using a merge commit by default; --squash or --rebase selects that
method. The two flags conflict. Refuses merged, closed or draft PRs.
Pins the merge to the fetched head SHA; GitHub checks and reviews still apply.
--delete-branch deletes the head after merging only in this repository;
forks or unavailable head repositories are skipped with a stderr note.
A deletion failure is an error after the merge has already succeeded.
Prints Merged #N <url>, Squashed and merged #N <url>, or
Rebased and merged #N <url>. There is no --json flag.
`,
	"pr view": `Usage:
  amf pr view <N> [--json]

Shows title, state, branches, author, inferred issue, mergeability, change
counts, URL and body. Mergeability may be unknown; it is not polled.
--json prints one object with number, title, state, draft, url, head, base,
author, issue, mergeable, mergeable_state, additions, deletions,
changed_files and body. number and change counts are integers, draft is a
boolean, issue is an integer or null, mergeable is a boolean or null, and
the rest are strings. state is open, closed or merged; drafts remain open.
issue is inferred from a canonical amf/issue-N branch, including forks.
`,
	"pr diff": `Usage:
  amf pr diff <N> [--name-only]

Prints the unified diff. --name-only instead prints one changed path per
line, fetching every page of changed files. There is no --json flag.
`,
	"pr close": `Usage:
  amf pr close <N> [--comment text] [--delete-branch]

Closes an unmerged PR. --comment posts text before closing (default none).
--delete-branch deletes the head after closing only in this repository;
forks or unavailable head repositories are skipped with a stderr note.
A deletion failure is an error after the close has already succeeded.
Already closed PRs succeed without commenting or deleting; merged PRs fail.
Prints Closed #N <url>, or #N is already closed.
Leaves linked issue labels unchanged. There is no --json flag.
`,
	"pr reopen": `Usage:
  amf pr reopen <N>

Reopens a closed, unmerged PR. Already open PRs succeed without changing it;
merged PRs fail. No command-specific flags.
Prints Reopened #N <url>, or #N is already open.
Leaves linked issue labels unchanged. There is no --json flag.
`,
	"pr ready": `Usage:
  amf pr ready <N>

Marks an open draft PR ready for review using GitHub GraphQL.
Closed or merged PRs fail. No command-specific flags.
Prints #N is ready for review <url>, or #N is already ready for review.
Leaves linked issue labels unchanged. There is no --json flag.
`,
	"pr draft": `Usage:
  amf pr draft <N>

Converts an open PR to a draft using GitHub GraphQL.
Closed or merged PRs fail. No command-specific flags.
Prints #N is now a draft <url>, or #N is already a draft.
Leaves linked issue labels unchanged. There is no --json flag.
`,
	"issue run":   runHelp,
	"issue watch": watchHelp,
	"issue help":  issueUsage,
	"pr help":     prUsage,
}
