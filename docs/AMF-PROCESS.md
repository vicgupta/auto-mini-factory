# How amf processes an issue

This is what happens, step by step, when amf works on an issue with Claude
Code, as of v0.4.0. It covers the default case, where the repo has no
pipeline table and Claude runs once; "With a pipeline" at the end covers the
rest. Each step names the function in `cmd/amf` that does it. [SPECS.md](SPECS.md) is
the authoritative description of amf's behavior.

## Before any issue: starting `amf run` or `amf watch` (`newRunner`, main.go)

1. **Settings are checked.** `--timeout` must be positive and
   `--eval-rounds` can't be negative. For `watch`, `--interval` must also be
   positive.
2. **Config is loaded.** amf reads `./.env`, then `.env` at the top of the
   checkout; environment variables win. That gives the token, the repo, the
   binary paths, `CLAUDE_CODE_OAUTH_TOKEN` and `AMF_TRUSTED`.
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
   `origin/<default>`; with no `## amf` table there, Claude runs once. It
   also checks that git has a name and email to commit with.

   If any of this fails, amf stops without touching the issue, and `watch`
   tries again on its next poll.

## Claim

8. **The issue is labeled `amf:in-progress`,** and its other status labels
   and `amf:ready` are removed.
9. **A failure handler is set up.** From here until the PR exists, any error
   makes amf label the issue `amf:failed` and comment the reason. This also
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
      `ANTHROPIC_API_KEY` is removed, so your subscription is used. Otherwise
      Claude uses its own login.
14. **While Claude works,** it's an ordinary headless Claude Code session
    with every permission prompt off. It reads code, edits files and runs
    shell commands, including the tests. It also picks up whatever your
    machine and the repo give it:
    - your `~/.claude` settings, hooks and MCP servers;
    - the repo's `CLAUDE.md`, as Claude Code always does.
15. **Logging and time limit.** Every JSON message streams to
    `.amf/logs/issue-N.log` as it arrives, so `tail -f` follows along. After
    `--timeout` (30 minutes by default) Claude is killed, with 10 seconds for
    its output to drain.
16. **The result.** amf reads the final `result` message.
    - An error result fails the issue ("claude reported an error…"), and so
      does output with no result message.
    - Output that isn't JSON at all is used as plain text.

## Commit and push

17. **Commit** (`commitAll`). amf stages everything and commits it as
    `Fix #N: <title>`, with `Closes #N` in the message body. Then it compares
    the branch with `origin/<default>`. If no file changed, the issue fails
    with "claude finished without making any changes". That includes the case
    where Claude made commits of its own that cancel out.
18. **Push.** `git push --force -u origin amf/issue-N`, using your own git
    credentials, not `GITHUB_TOKEN`. amf owns `amf/issue-*` branches, so a
    retry replaces the earlier push.

## The PR

19. **The PR description** starts with Claude's final message, cleaned up:
    - "Generated with Claude Code" and `Co-Authored-By` lines are removed.
    - Claude's own `Closes`/`Fixes`/`Resolves #N` line is removed.

    If nothing is left, the description is "Resolves #N.". amf then adds
    `Closes #N` and an "Opened by amf" footer.
20. **The PR is opened** from `amf/issue-N` to the default branch, titled
    `Fix #N: <title>`. If GitHub refuses because that branch already has an
    open PR (a retry), amf updates that PR's title and description instead.
21. **The worktree is removed.** The branch and the log are kept.

## Finish

22. **The issue is labeled `amf:done`, and amf comments "amf opened <PR URL>
    for this issue."** For a retry the comment says "updated". The PR exists
    by now, so if either GitHub call fails, amf only logs a warning and the
    run still counts as a success.

## After a failure

The issue has `amf:failed` and a comment with the error, and the worktree and
log are kept so you can look at them. To retry, add `amf:ready` again; amf
starts over from a fresh worktree.

## With a pipeline

If `AGENTS.md` on the default branch has an `## amf` table, steps 12–17 are
replaced:

- **Skills:** amf downloads each stage's skill.
- **Plan (if declared):** runs once and changes no files. A plan whose first
  line starts with `BLOCKED:` fails the issue with its questions.
- **Code, then eval:** code runs, and its changes are committed for that
  round. Eval reviews the change and must end with `VERDICT: PASS` or
  `VERDICT: FAIL`. On FAIL, the review goes back to code, up to
  `--eval-rounds` times.
- **Squash:** on PASS, the rounds become one `Fix #N` commit, and the PR
  description gets an Evaluation section and the plan.

Each stage uses the agent and model the table names, and the steps from the
push on are the same.

## Two things worth knowing

- **"Don't touch git or GitHub" is only an instruction.** Nothing enforces it
  during the run. amf removes `GITHUB_TOKEN`, but Claude can still use your
  own logins, for example a `gh` CLI you're logged into, your git
  credentials, or files like `.env`. Afterwards amf commits whatever is in
  the worktree, and its own push and PR override anything Claude did there.
- **The issue text is read once, at step 6.** Edits made to the issue while
  Claude is working aren't seen.
