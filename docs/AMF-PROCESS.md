# How amf processes an issue

This is what happens, step by step, when amf works on an issue with Claude
Code, as of v0.9.0. It covers the default case, where the repo has no
pipeline table and Claude runs once; "With a pipeline" covers the rest.
"From a sentence: `amf auto`" below walks through planning and merging several
issues. Each step names the function in `cmd/amf` that does it. [SPECS.md](SPECS.md) is
the authoritative description of amf's behavior.

## Before any issue: starting `amf run` or `amf watch` (`newRunner`, main.go)

1. **Settings are checked.** `--agent-timeout` (or its deprecated alias
   `--timeout`) must be a duration of at least 1m and at most 24h, and
   `--eval-rounds` can't be negative, and `--log-level` must be `debug`,
   `info`, `warn` or `error`. For `watch`, `--interval` must also be
   positive.
2. **Config is loaded.** amf reads `./.env`, then `.env` at the top of the
   checkout; environment variables win. That gives the token, the repo, the
   binary paths, the agents' keys (`CLAUDE_CODE_OAUTH_TOKEN`,
   `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`), `OPENCODE_MODEL`,
   `AMF_AGENT_TIMEOUT`, `AMF_LOG_LEVEL`, `AMF_TRUSTED` and
   `AMF_POSTGRES_URL`. An `AMF_AGENT_TIMEOUT`, `AMF_LOG_LEVEL` or
   `AMF_POSTGRES_URL` that doesn't parse stops amf here, and so does an
   `--agent` that isn't installed.
3. **The repo is checked on GitHub.** A wrong repo name gets a clear "not
   found" message, and a rejected token gets "GitHub rejected GITHUB_TOKEN".
4. **The checkout is found.** amf uses the git checkout you're in. Anywhere
   else it clones the repo into `./<name>`, or reuses that clone. It warns if
   the checkout's `origin` isn't `GITHUB_REPO`.
5. **`.amf/` is kept out of git.** amf adds `/.amf/` to `.git/info/exclude`.

`watch` then lists open issues with the label, oldest first, on every page.
It picks the ones that need work (`needsWork`) and runs the steps below for
each, one at a time.

## Checks before the issue is touched (`Run`, runner.go)

6. **The issue is fetched.** If it's a PR or isn't open, amf stops there.
7. **The repo's setup is checked.** amf gets the default branch from GitHub
   and runs `git fetch origin <default>`. It reads `AGENTS.md` on
   `origin/<default>`; with no `## amf` table there, one agent runs once:
   the one `--agent` names, or else the primary agent (`AMF_PRIMARY_AGENT`,
   default codex), falling back to the secondary (`AMF_SECONDARY_AGENT`,
   default claude) if it fails (here, Claude with `ANTHROPIC_API_KEY` or
   `CLAUDE_CODE_OAUTH_TOKEN`). It also checks that git has a name and email
   to commit with.

   If any of this fails, amf stops without touching the issue, and `watch`
   tries again on its next poll.

## Claim

8. **The issue is labeled `amf:in-progress`,** unless it already has an
   open PR from `amf/issue-N` (then it is labeled `amf:pr-open` and
   skipped) or an `amf/issue-N` branch on origin (skipped, unless it is
   `amf:failed`); `--force` skips that check. Otherwise and its other status labels
   and `amf:ready` are removed.
9. **A failure handler is set up.** From here until the PR exists, any error
   makes amf label the issue `amf:failed` (or `amf:pr-open` if the branch
   still has an open PR) and comment the reason, each agent tried with its
   exit code and duration, whether the fallback ran, and the redacted end of
   the log. This also
   happens on Ctrl-C, with up to 30 seconds allowed for those two GitHub
   calls.
10. **The author is checked (trust).** If `AMF_TRUSTED` is set and doesn't
    list the author, the issue fails with an explanatory comment. No worktree
    is created and no agent runs.

## Worktree (`prepareWorktree`)

11. **The branch is set up from scratch.** amf fetches the default branch
    again and discards any worktree an earlier run left behind. It then
    creates `.amf/worktrees/issue-N` on branch `amf/issue-N`, reset to
    `origin/<default>`. Each run starts from the current default branch, not
    from an earlier attempt.

## Claude runs (`runAgent`)

12. **The prompt** (`buildPrompt`) says, in order:
    - Claude is in a worktree of `<repo>` on `amf/issue-N`.
    - The issue: `# #N: <title>` and the body, or "(no description)".
    - Make the smallest complete change, in the existing style.
    - Find and run the tests, build and linters, and fix any failures.
    - Don't commit, push, create branches or open PRs.
    - Reply with only a PR description: what changed, why, and how it was
      verified.
13. **The command** is
    `claude -p --dangerously-skip-permissions --output-format stream-json --verbose`,
    with the prompt on stdin.
    - It runs in the worktree, and `PWD` is set to the worktree.
    - `GITHUB_TOKEN` is removed from Claude's environment.
    - If `CLAUDE_CODE_OAUTH_TOKEN` is set, Claude gets it and
      `ANTHROPIC_API_KEY` is removed, so your subscription is used.
      Otherwise Claude gets `ANTHROPIC_API_KEY` from `.env` or the
      environment, or, with neither (only with `--agent claudecode`), uses
      its own login.
14. **While Claude works,** it's an ordinary headless Claude Code session
    with every permission prompt off. It reads code, edits files and runs
    shell commands, including the tests. It also picks up whatever your
    machine and the repo give it:
    - your `~/.claude` settings, hooks and MCP servers;
    - the repo's `CLAUDE.md`, as Claude Code always does.
15. **Logging and time limit.** Every JSON message streams to
    `.amf/logs/issue-N.log` as it arrives, so `tail -f` follows along. After
    `--agent-timeout` (60 minutes by default, `AMF_AGENT_TIMEOUT` without a
    flag) Claude's whole process group is signalled: SIGTERM first, and if
    the group is still there after a 15s grace period, SIGKILL.
16. **The result.** amf reads the final `result` message.
    - An error result fails the issue ("claude reported an error…"), and so
      does output with no result message.
    - Output that isn't JSON at all is used as plain text.

## Commit and push

17. **Commit** (`commitAll`). amf stages everything and commits it as
    `Fix #N: <title>`, with `Closes #N` in the message body. With `--part`, or
    when the agent ends its final message with `amf-status: partial`, the
    subject is `Part of #N: <title>` and the body is `Part of #N`, so merging
    the PR will not close the issue. Then it compares
    the branch with `origin/<default>`. If no file changed, the issue fails
    with "claude finished without making any changes". That includes the case
    where Claude made commits of its own that cancel out. If any changed path
    is under `.github/`, the run fails with `the agent changed files under
    .github/ (<paths>), which amf refuses to push`: a pushed workflow change
    could run whatever the agent put there with the repo's Actions secrets
    the moment the pull request opens. This comes before the push, so no
    branch or PR is created; the issue prompt also tells the agent not to
    touch `.github/`.
18. **Push.** `git push --force -u origin amf/issue-N`, using your own git
    credentials, not `GITHUB_TOKEN`. amf owns `amf/issue-*` branches, so a
    retry replaces the earlier push.

## The PR

19. **The PR description** starts with the diffstat and changed paths amf
    read from git, then Claude's final message, cleaned up:
    - "Generated with Claude Code" and `Co-Authored-By` lines are removed.
    - Claude's own `Closes`/`Fixes`/`Resolves #N` line is removed.
    - An `amf-status: complete` or `amf-status: partial` line is removed.

    If nothing is left, the description is "Resolves #N.". amf then adds
    `Closes #N` and an "Opened by amf" footer. A partial change says
    `Part of #N` instead, and any closing keyword for the issue in Claude's
    text is rewritten to `Part of`.
20. **The PR is opened** from `amf/issue-N` to the default branch, titled
    `Fix #N: <title>`, or `Part of #N: <title>` for a partial change. If
    GitHub refuses because that branch already has an open PR (a retry), amf
    updates that PR's title and description instead.
21. **The worktree is removed.** The branch and the log are kept.

## Finish

22. **The issue is labeled `amf:pr-open` (`amf:done` once `--merge` merged
    the PR), and amf comments "amf opened <PR URL>
    for this issue."** For a retry the comment says "updated". The PR exists
    by now. amf sets the label with a short, cancellation-safe timeout, so a
    Ctrl-C doesn't leave `amf:in-progress` behind; if the label still can't
    be set it keeps the real PR status (never `amf:failed`), logs a warning,
    and reports the error. A failed comment is still only a warning.

## After a failure

The issue has `amf:failed` and a comment with the error, and the worktree and
log are kept so you can look at them. To retry, add `amf:ready` again; amf
starts over from a fresh worktree. A run that hit `--agent-timeout` is the
exception: its comment says how to retry with more time, its log and
partial output are kept, and its worktree is removed. Nothing was pushed
and no PR was opened or updated; the local `amf/issue-N` branch stays,
and a retry resets it to `origin/<default>`.

## With a pipeline

If `AGENTS.md` on the default branch has an `## amf` table, steps 12–17 are
replaced:

- **Skills:** amf downloads each stage's skill.
- **Plan (if declared):** runs once and changes no files. A plan whose first
  line starts with `BLOCKED:` fails the issue with its questions.
- **Code, then eval:** code runs, and its changes are committed for that
  round. Eval reviews the change and must end with `VERDICT: PASS` or
  `VERDICT: FAIL`. On FAIL, the review goes back to code, up to
  `--eval-rounds` times. A round that changes a path under `.github/` fails
  the issue before any push, as step 17 does.
- **Squash:** on PASS, the rounds become one `Fix #N` commit, and the PR
  description gets an Evaluation section and the plan.

Each stage uses the agent and model the table names, and the steps from the
push on are the same.

## From a sentence: `amf auto`

`amf auto "Add a dark mode toggle"` plans issues and processes them one at a
time, merging each PR. It uses the same runner setup as `run` (`cmdAuto`,
auto.go), then follows these steps:

1. **Plan in a separate worktree** (`autoPlan`). amf fetches the default
   branch and creates a detached `.amf/worktrees/auto-plan` at
   `origin/<default>`. The planning agent is chosen as for a run without a
   pipeline: `--agent` alone, or the primary and secondary agents in order.
   The prompt (`autoPlanPrompt`) asks it to read the repo without changing
   files and return JSON with a title, scope and acceptance criteria for
   each issue, in execution order. Issues must fit in one PR each and avoid
   `.github/` and `AGENTS.md`. `--max-issues` defaults to 5 and accepts 1–10.
   An agent failure or invalid plan tries the next available agent; if none
   succeeds, no issues are created. Output goes to
   `.amf/logs/auto-plan-<agent>.log`, and the planning worktree is removed
   afterwards. `--dry-run` prints the plan and stops without creating issues
   or saving a new run.
2. **Save the plan, then create issues** (`Auto`, `autoCreateIssues`). Before
   creating anything on GitHub, amf writes the full plan to
   `.amf/auto/state.json`. It creates each issue with a Scope section,
   acceptance checkboxes and a footer identifying the original sentence and
   its position in the plan (`autoIssueBody`). Each gets only `amf:auto`;
   amf saves its assigned number and URL before printing them and creating
   the next issue. Waiting issues have no `amf:ready`, so the default
   `amf watch` leaves them alone, including during the checkpoint.
3. **Run and merge sequentially** (`autoWork`). Immediately before each
   issue, amf adds `amf:ready`, then calls `Run`: the same worktree, agent
   or pipeline, commit, push and PR flow described above, with merging
   enabled. The default is a merge commit; `--merge=squash` and
   `--merge=rebase` select the other methods. Each issue starts from the
   updated default branch after the previous merge. The normal safeguards
   apply: changes under `.github/` are never pushed, and a change to
   `AGENTS.md` is left as an open PR for review instead of merged. Issues
   labeled `amf:auto` pass the author trust check regardless of
   `AMF_TRUSTED`, because the plan came from the supplied sentence. A failed
   run or merge stops auto at that issue and saves the error for retry.
4. **Checkpoint before the rest** (`autoCheckpoint`). By default, after the
   first merge, if issues remain, amf prints the merged issue, PR URL, merge
   commit, agent, checks on that commit and remaining issues. It saves state
   and asks for approval in a terminal: `y` or `yes` continues; any other
   answer stops. Without a terminal it saves state and exits, waiting for
   `amf auto --continue`. Declining or waiting for approval is a successful
   exit, not a failed issue. Once approval is saved, there are no further
   checkpoints.
5. **Finish** (`autoWork`). After every issue merges, amf prints the total
   and removes `.amf/auto/state.json`.

### Checkpoint controls

- `--checkpoint-after N` moves the single checkpoint to after N merges
  (positive, default 1). It reports the latest merge and only happens if
  issues remain, so a one-issue plan has no checkpoint.
- `--approve` prints and accepts the checkpoint without prompting, including
  without a terminal. It applies to the current invocation only.
- `--no-checkpoint` skips the checkpoint and approval entirely, taking
  precedence over the other checkpoint controls. Progress is still saved.
- `--wait-for-checks` polls check runs on the checkpoint's merge commit every
  10 seconds for up to 5 minutes, waiting for at least one run and for all
  returned runs to complete. It reports pass/fail, timeout or unavailable
  checks before approval; these results do not prevent approval.
  Cancellation stops without approving. Without the flag, checks are
  fetched once. These are check runs, not commit status contexts.

### Saved state, recovery and abort

`state.json` is versioned (currently version 1) and stores the sentence,
repo, merge method, creation time, approval and checkpoint settings, plus
each planned issue's title, scope, criteria, number, URL and progress
(`pending`, `merged` or `failed`). After processing an issue it also records
the PR URL and number, merge commit, agent and error, when available. Writes
use a temporary file and rename it over the state file (`saveAutoState`).

`amf auto --continue` takes no sentence and reuses this plan and merge
method. It creates only issues whose saved number is still 0, skips issues
saved as merged, and retries the remaining work. Before retrying an issue
with a saved PR number, it checks GitHub: an already merged PR marks the
issue merged and clears its saved error without rerunning it
(`auto.reconciled`). A PR lookup error stops with state kept; a closed issue
without a confirmed merged PR still fails.

If the saved checkpoint is already due, `--continue` displays it again,
including any enabled CI wait, and approves without prompting. Before the
threshold is reached, it resumes work without granting early approval.
The threshold and skip/wait settings persist: `--checkpoint-after N`
overrides the saved threshold, while `--no-checkpoint` and
`--wait-for-checks` enable their saved settings. An unfinished run blocks a
new sentence unless `--dry-run` is used. An unsupported state version or a
saved repo different from `GITHUB_REPO` prevents continuation.

`amf auto status` reads the saved sentence, merge method and each issue's
number (or `not created`), title, status, PR and commit (`cmdAutoState`). It
is a local snapshot; it does not reconcile with GitHub or run agents.

To abandon a run, stop any active auto process, then use `amf auto --abort`.
It removes state, even if corrupt or unsupported, and leaves GitHub issues
and PRs unchanged. `amf auto --abort --close-issues` first closes created
issues whose saved status is not `merged`; it skips uncreated issues and
keeps state if any close fails so the command can be retried. Closing needs
GitHub credentials and a matching saved repo. Add `amf:ready` to remaining
open issues if you want `amf watch` to process them instead.

Status and abort take no sentence or execution flags. Inside a checkout,
status and plain abort need no configuration or GitHub access. Outside one,
they read configuration to locate `./<repo name>/.amf/auto/state.json`
without cloning. Neither creates logs. With no saved run, both print
`No amf auto run saved.` and exit successfully.

### Auto events (`auto.go`)

These events accompany the normal issue, agent, git and GitHub events:

| Event | When it is emitted |
|---|---|
| `auto.plan` | Planning starts, a planning agent fails, issues have been created, or planning worktree cleanup fails. |
| `auto.checkpoint` | The checkpoint is displayed, with PR, commit, agent, checks and remaining count. |
| `auto.approved` | Approval is recorded; `how` is `prompt`, `flag` or `continue`. |
| `auto.declined` | The terminal prompt was answered without approval; waiting without a terminal does not emit this event. |
| `auto.resume` | A saved run resumes, with the sentence and remaining count. |
| `auto.reconciled` | A saved PR is already merged, so its issue is skipped. |
| `auto.stopped` | An issue run or merge fails, with the issue and error. |
| `auto.done` | All planned issues are merged. |

## Two things worth knowing

- **"Don't touch git or GitHub" is only an instruction.** Nothing enforces it
  during the run. amf removes `GITHUB_TOKEN`, but Claude can still use your
  own logins, for example a `gh` CLI you're logged into, your git
  credentials, or files like `.env`. Afterwards amf commits whatever is in
  the worktree, and its own push and PR override anything Claude did there.
- **The issue text is read once, at step 6.** Edits made to the issue while
  Claude is working aren't seen.
