# amf JSON output

Every command that takes `--json` is listed here with the JSON it prints.
Use it as an index for scripts; each command's full behavior and flags stay
in the [command reference](COMMANDS.md#subcommand-reference). JSON is written
to stdout with a final newline, like the other command data described under
[Output](../README.md#output).

| Command | JSON value | Fields |
|---|---|---|
| `amf issue list` (or `amf list`) | array of [issues](#issue) | `number`, `title`, `state`, `url`, `labels` |
| `amf issue view <N>` | one [issue](#issue) | `number`, `title`, `state`, `url`, `labels`, `author`, `body` |
| `amf pr list` | array of [pull requests](#pull-request-list) | `number`, `title`, `state`, `draft`, `url`, `head`, `base`, `issue` |
| `amf pr sweep` | array of [sweep results](#sweep-result) | `number`, `issue`, `url`, `action`, `reason`, `commit` |
| `amf pr view <N>` | one [pull request](#pull-request-view) | `number`, `title`, `state`, `draft`, `url`, `head`, `base`, `author`, `issue`, `mergeable`, `mergeable_state`, `additions`, `deletions`, `changed_files`, `behind_by`, `ahead_by`, `checks`, `body` |
| `amf memory sync` | one [sync result](#memory-sync) | `issues`, `pull_requests`, `linked`, `cursor` |
| `amf memory similar <N \| text>` | array of [hits](#memory-similar) | `kind`, `ref`, `issue`, `pr`, `title`, `score`, `what`, `merged` |
| `amf memory show <N>` | one [issue history](#memory-show) | `issue`, `pull_requests`, `runs` |
| `amf tag list` | array of [tags](#tag) | `name`, `commit` |
| `amf tag view <tag>` | one [tag](#tag-view) | `name`, `type`, `sha`, `commit`, `depth` |
| `amf release list` | array of [releases](#release) | `id`, `tag`, `name`, `state`, `draft`, `prerelease`, `target`, `url`, `created_at`, `published_at` |
| `amf release view <tag>` | one [release](#release-view) | the list fields plus `body`, `tag_commit`, `assets` |
| `amf release latest` | one [release](#release-view) | the same as `amf release view` |

`amf issue list`, `amf issue view`, `amf pr list` and `amf pr view` print a
compact value on one line and do not HTML-escape their strings. `amf pr sweep`
and the `amf memory` commands print the same values with two-space indentation.

## Issue

Used by `amf issue list` (array) and `amf issue view` (one object). The view
form adds `author` and `body`.

| Field | Type | Meaning |
|---|---|---|
| `number` | integer | issue number |
| `title` | string | issue title |
| `state` | string | `open` or `closed` |
| `url` | string | the issue's GitHub page |
| `labels` | array of strings | every label name, in GitHub's order; `[]` when there are none |
| `author` | string | the issue author's login (view only) |
| `body` | string | the issue body as GitHub returned it (view only) |

`amf issue view` does not HTML-escape its output. `amf list` is the
undocumented alias of `amf issue list` and prints the same schema.

```json
[{"number":42,"title":"Fix login","state":"open","url":"https://github.com/owner/repo/issues/42","labels":["amf:ready","bug"]}]
```

## Pull request list

Printed as an array by `amf pr list`.

| Field | Type | Meaning |
|---|---|---|
| `number` | integer | pull request number |
| `title` | string | pull request title |
| `state` | string | `open`, `closed` or `merged`; drafts stay `open` |
| `draft` | boolean | whether the pull request is a draft |
| `url` | string | the pull request's GitHub page |
| `head` | string | head branch name |
| `base` | string | base branch name |
| `issue` | integer or `null` | linked issue from a canonical `amf/issue-N` head, or `null` |

```json
[{"number":57,"title":"Fix login","state":"open","draft":false,"url":"https://github.com/owner/repo/pull/57","head":"amf/issue-42","base":"main","issue":42}]
```

## Sweep result

Printed as an array by `amf pr sweep`, one object per pull request.

| Field | Type | Meaning |
|---|---|---|
| `number` | integer | pull request number |
| `issue` | integer | linked issue number |
| `url` | string | the pull request's GitHub page |
| `action` | string | `merged`, `would-merge`, `updated`, `would-update`, `waiting`, `ready` or `blocked` |
| `reason` | string | why, when there is one; omitted when empty |
| `commit` | string | merge commit SHA after a merge; omitted otherwise |

```json
[{"number":57,"issue":42,"url":"https://github.com/owner/repo/pull/57","action":"merged","commit":"1a2b3c4"}]
```

## Pull request view

Printed as one object by `amf pr view <N>`.

| Field | Type | Meaning |
|---|---|---|
| `number` | integer | pull request number |
| `title` | string | pull request title |
| `state` | string | `open`, `closed` or `merged`; drafts stay `open` |
| `draft` | boolean | whether the pull request is a draft |
| `url` | string | the pull request's GitHub page |
| `head` | string | head branch name |
| `base` | string | base branch name |
| `author` | string | the author's login |
| `issue` | integer or `null` | linked issue from a canonical `amf/issue-N` head, or `null` |
| `mergeable` | boolean or `null` | GitHub's mergeability, or `null` when unknown |
| `mergeable_state` | string | GitHub's mergeable state, `""` when absent |
| `additions` | integer | added lines |
| `deletions` | integer | deleted lines |
| `changed_files` | integer | changed file count |
| `behind_by` | integer or `null` | commits behind the base, or `null` when unknown or closed |
| `ahead_by` | integer or `null` | commits ahead of the base, or `null` when unknown or closed |
| `checks` | object or `null` | `{state, summary}` for the head's checks, or `null` |
| `body` | string | the pull request body as GitHub returned it |

`checks.state` is `none`, `pending`, `passed` or `failed`; `checks.summary`
looks like `5 checks: 5 success`. Titles and bodies are not HTML-escaped.

```json
{"number":57,"title":"Fix login","state":"open","draft":false,"url":"https://github.com/owner/repo/pull/57","head":"amf/issue-42","base":"main","author":"octocat","issue":42,"mergeable":true,"mergeable_state":"clean","additions":3,"deletions":1,"changed_files":1,"behind_by":2,"ahead_by":1,"checks":{"state":"passed","summary":"5 checks: 5 success"},"body":""}
```

## Tag

Printed as an array by `amf tag list`, with the commit GitHub's tag list
reports.

| Field | Type | Meaning |
|---|---|---|
| `name` | string | tag name |
| `commit` | string | commit SHA GitHub lists for the tag |

## Tag view

Printed as one object by `amf tag view <tag>`.

| Field | Type | Meaning |
|---|---|---|
| `name` | string | tag name |
| `type` | string | `lightweight`, `annotated` or `annotated (N deep)` |
| `sha` | string | the tag's immediate object SHA |
| `commit` | string | the final commit after peeling every annotated tag |
| `depth` | integer | number of annotated tag objects peeled; 0 for a lightweight tag |

```json
{"name":"v1.2.3","type":"annotated","sha":"4b825dc642cb6eb9a060e54bf8d69288fbee4904","commit":"0123456789abcdef0123456789abcdef01234567","depth":1}
```

## Release

Printed as an array by `amf release list`.

| Field | Type | Meaning |
|---|---|---|
| `id` | integer | release ID |
| `tag` | string | tag name |
| `name` | string | release name as GitHub returned it |
| `state` | string | `draft`, `published` or `prerelease` |
| `draft` | boolean | whether the release is a draft |
| `prerelease` | boolean | whether it is a prerelease |
| `target` | string | GitHub's `target_commitish`; not authority over an existing tag |
| `url` | string | the release's GitHub page |
| `created_at` | string | RFC 3339 creation time |
| `published_at` | string | RFC 3339 publication time, or `""` for a draft |

## Release view

Printed as one object by `amf release view <tag>` and `amf release latest`;
`latest` always selects a published, non-prerelease release. The view adds:

| Field | Type | Meaning |
|---|---|---|
| `body` | string | release notes |
| `tag_commit` | string | the tag's peeled commit, or `"unknown"` when the tag does not exist |
| `assets` | array | `{name, size, state, url}` for each release asset |

## Memory sync

Printed as one object by `amf memory sync`.

| Field | Type | Meaning |
|---|---|---|
| `issues` | integer | issues stored by this sync |
| `pull_requests` | integer | pull requests stored by this sync |
| `linked` | integer | pull requests linked to an issue |
| `cursor` | string | the next sync's starting time, in RFC 3339 |

```json
{
  "issues": 3,
  "pull_requests": 3,
  "linked": 2,
  "cursor": "2026-01-02T15:04:05Z"
}
```

## Memory similar

Printed as an array by `amf memory similar`; `[]` when there are no matches.

| Field | Type | Meaning |
|---|---|---|
| `kind` | string | `issue`, `pr` or `run` |
| `ref` | string | the issue or pull request number, or the run ID |
| `issue` | integer | linked issue number; omitted when there is none |
| `pr` | integer | pull request number; omitted unless the hit is a pull request |
| `title` | string | the matching record's title |
| `score` | number | similarity score, best first |
| `what` | string | a short description; omitted when empty |
| `merged` | boolean | `true` for merged work; omitted otherwise |

```json
[
  {
    "kind": "pr",
    "ref": "12",
    "issue": 42,
    "pr": 12,
    "title": "Fix login redirect",
    "score": 0.87,
    "what": "PR #12 merged",
    "merged": true
  }
]
```

## Memory show

Printed as one object by `amf memory show <N>`.

| Field | Type | Meaning |
|---|---|---|
| `issue` | object or `null` | the stored [issue](#memory-issue), or `null` when none |
| `pull_requests` | array of [pull requests](#memory-pull-request) | linked pull requests; `null` when none |
| `runs` | array of [runs](#memory-run) | recorded runs for the issue; `null` when none |

### Memory issue

| Field | Type | Meaning |
|---|---|---|
| `repo` | string | the memory store's repository key |
| `number` | integer | issue number |
| `title` | string | issue title |
| `body` | string | issue body; omitted when empty |
| `state` | string | `open` or `closed` |
| `labels` | array of strings | label names; omitted when empty |
| `author` | string | author login; omitted when empty |
| `created_at` | string | creation time; omitted when empty |
| `updated_at` | string | last update time; omitted when empty |
| `closed_at` | string | close time; omitted when empty |

### Memory pull request

| Field | Type | Meaning |
|---|---|---|
| `repo` | string | the memory store's repository key |
| `number` | integer | pull request number |
| `title` | string | pull request title |
| `body` | string | pull request body; omitted when empty |
| `state` | string | `open`, `closed` or `merged` |
| `merged` | boolean | whether it was merged |
| `merged_at` | string | merge time; omitted when not merged |
| `head_branch` | string | head branch; omitted when empty |
| `issue` | integer | linked issue number; omitted when there is none |
| `updated_at` | string | last update time; omitted when empty |

### Memory run

| Field | Type | Meaning |
|---|---|---|
| `repo` | string | the memory store's repository key |
| `id` | string | run ID |
| `issue` | integer | issue number |
| `command` | string | `run`, `watch` or `auto` |
| `started_at` | string | start time |
| `finished_at` | string | finish time; omitted while unfinished |
| `outcome` | string | `success`, `failed` or `skipped` |
| `agents` | array of strings | agents used; omitted when empty |
| `exit_codes` | array of integers | agent exit codes; omitted when empty |
| `pr` | integer | pull request number; omitted when there is none |
| `error` | string | failure message; omitted when empty |
| `log_tail` | string | redacted tail of the agent log; omitted when empty |
| `summary` | string | the run's summary; omitted when empty |

```json
{
  "issue": {
    "repo": "github.com/owner/repo",
    "number": 42,
    "title": "Fix login",
    "state": "open",
    "labels": ["amf:ready"]
  },
  "pull_requests": [
    {
      "repo": "github.com/owner/repo",
      "number": 57,
      "title": "Fix login",
      "state": "open",
      "merged": false,
      "head_branch": "amf/issue-42",
      "issue": 42
    }
  ],
  "runs": null
}
```
