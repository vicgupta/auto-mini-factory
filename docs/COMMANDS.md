# amf command reference

The details behind the [README](../README.md)'s overview: what each command
does, its flags, logs and labels. `amf help <command>` (or `amf <command> -h`)
prints the same in the terminal. Every setting is in
[Configuration (.env)](../README.md#configuration-env) and `amf help env`.

## Run and watch

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
amf watch --agent codex     # ...with Codex, even if Claude Code is installed
amf watch --exclude-label blocked --exclude-label needs-human  # ...leaving issues with either label alone until it's removed
amf run 42 --draft          # open the PR as a draft
amf watch --draft           # ...opening each new PR as a draft
amf run 42 --merge          # ...and merge its PR with a merge commit
amf watch --merge=squash    # squash and merge each PR (or --merge=rebase)
amf auto "Add a dark mode toggle"  # plan issues, merge the first PR, then ask before the rest
amf run 42 --dry-run        # make the change locally and show the PR, touching nothing on GitHub
```

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
(`--merge=merge` by default, or `squash` / `rebase`).

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

1. Labels the issue `amf:in-progress` and removes `amf:ready`.
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
   a retry whose PR is still open, it updates that PR's title and body
   instead, and leaves it a draft or ready for review as it was.
6. Comments the PR link on the issue, labels it `amf:done` (a draft PR
   counts), removes the worktree.

With `--merge`, amf also merges the PR before commenting, with a merge commit
(or `--merge=squash`, `--merge=rebase`), pinned to the commit it pushed. It
doesn't wait for checks, so with required checks or reviews GitHub refuses
and the PR stays open; amf comments why, and `amf run` exits 1. amf never
merges a change to the top-level `AGENTS.md`, which holds the pipeline.
`--merge` can't be combined with `--draft`, since GitHub doesn't merge drafts.

With `--dry-run`, `amf run N` only reads from GitHub. It leaves the issue's
labels alone, runs the agent (or the pipeline) and commits on the local
`amf/issue-N` branch as usual, then prints the diff stat and the PR it would
open or update (and whether `--merge` would merge it) instead of pushing.
Nothing is labeled, commented, pushed, opened or merged. See the change with
`git diff origin/main amf/issue-N`. The agent still runs with every
permission, so the trust rules below still apply.

On any failure the issue gets `amf:failed` plus a comment with the reason,
and the worktree is kept for inspection. A run that hit the agent timeout
loses only the worktree: amf comments with how to retry with more time and
keeps the logs and partial output. Re-running `amf run N` starts again from a
fresh worktree.

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
the lines in memory and retries. amf has no dependencies, so it speaks the
PostgreSQL protocol itself: TLS (`sslmode`), and password, MD5 or
SCRAM-SHA-256 logins. `amf doctor` checks the connection. Agents' output can
include your code, so restrict who can read `amflogs`.

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

Starter skills are in [`skills/`](skills): [plan](skills/plan/SKILL.md),
[code](skills/code/SKILL.md) and [eval](skills/eval/SKILL.md). Write your own
by copying one; amf's prompt already fixes each stage's inputs and output
format, so a skill only needs to say how to do the work.

## Labels

An issue moves `amf:ready` → `amf:in-progress` → `amf:done` or `amf:failed`.
`amf issue` adds `amf:ready`, and `amf watch` picks up open `amf:ready` issues
oldest first. To retry an issue, add `amf:ready` again or run `amf run N`.
If amf is killed mid-run and an issue is stuck on `amf:in-progress`, run
`amf run N`.

Issues labeled `amf` by amf v0.0.4 and earlier aren't picked up any more.
Relabel them `amf:ready`, or run `amf watch --label amf`.

`.amf/` is added to `.git/info/exclude`, so it never shows up in `git status`.
