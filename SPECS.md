# amf specification

amf (autonomous mini factory) turns labeled GitHub issues into pull requests by
running coding agents headless in a git worktree. By default that's one Claude
Code run. A repo can instead declare a plan → code → eval pipeline in its
`AGENTS.md`, with a skill, agent and model for each stage. amf is a single Go
binary that uses only the standard library, for one developer or a small team
that doesn't want to run a platform.

**Needs:** `git` and the `claude` CLI on `PATH`, and a GitHub token for one
repository. A pipeline also needs whichever of `codex` and `opencode` it names.

## Issue lifecycle

```
amf:ready ──▶ amf:in-progress ──▶ amf:done    (PR opened)
                       └────────▶ amf:failed  (reason commented)
```

- `amf:ready` marks an issue for amf. Adding it again retries an issue that is
  done or failed.
- amf removes `amf:ready` when it starts, and an issue has at most one status
  label at a time.
- amf owns these four labels and every `amf/issue-<N>` branch.

## Commands

| Command | Behavior |
|---|---|
| `amf init` | Writes `./.env` and creates the labels (see below). |
| `amf issue "Title" [-b body \| -f file \| -f -] [--label L] [--no-label]` | Creates an issue labeled `amf:ready`. The body can also come from stdin when it's piped. Prints `#N <url>`. |
| `amf list [--state open\|closed\|all] [--label L]` | Lists issues, newest first, with every page fetched and PRs left out. Columns: `ISSUE`, `STATE` (with `--state all` only), `AMF` (the issue's `amf` and `amf:*` labels, or `-`) and `TITLE`. Prints `No … in owner/name.` when nothing matches. Needs no git checkout. |
| `amf run <N> [--timeout 30m] [--eval-rounds 2]` | Processes issue N once. |
| `amf watch [--interval 60s] [--label amf:ready] [--timeout 30m] [--eval-rounds 2]` | Polls open issues with the label (every page), oldest first, and processes them one at a time until interrupted. |
| `amf pipeline [--fetch]` | Checks the pipeline in `AGENTS.md` of the enclosing checkout, as it is on disk. See below. |
| `amf doctor` | Checks the whole setup, one line per check, changing nothing. See below. |
| `amf update [--version vX.Y.Z] [--check]` | Replaces the running binary with the latest release, or with the given one, which can be a downgrade. Needs no `.env`. See below. |
| `amf version` | Prints the version: the release tag, or the module version for `go install`. |

- `--timeout` limits each agent run, not the whole issue. It and `--interval`
  must be positive.
- `--eval-rounds` is how many times a pipeline's eval stage may send the
  change back to the code stage. A negative value is an error.

**Exit codes:** 0 on success, 1 on error, 2 on a usage error. SIGINT and
SIGTERM cancel the current step; the issue is still marked failed.

### `amf init`

1. Builds `.env` from a template of `GITHUB_TOKEN`, `GITHUB_REPO` and
   `CLAUDE_CODE_OAUTH_TOKEN`.
   - Real values from an existing `.env` are kept, and so are any extra
     variables.
   - A missing `GITHUB_REPO` is filled in from the checkout's GitHub `origin`.
   - Anything else gets a placeholder.
2. If the result matches the current `.env`, nothing is written. Otherwise the
   old file is copied to the next free `.env.NNNN` (0001, 0002, …, numbered
   above the highest that exists), and the new file is written atomically with
   permissions 0600.
3. In a git checkout, adds `/.env` and `/.env.NNNN` to `.git/info/exclude`.
4. Once the token and repo are usable, checks that the repo exists and creates
   any of the four labels that are missing. Existing labels aren't changed.
   Until then it prints that labels weren't checked yet.

### `amf pipeline`

1. Must run inside a git checkout. Reads `AGENTS.md` at its top level from
   disk, so a change can be checked before it's merged.
2. Without `AGENTS.md`, or without an `## amf` table in it, prints that amf
   runs claude with its built-in prompt, and exits 0.
3. Otherwise prints the stages (`STAGE`, `AGENT`, `MODEL`, `SKILL`), then one
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
| `github` | `GET /user` accepts the token; shows the login | `FAIL` on 401 or no connection |
| `repo` | The repo is readable; shows its default branch | `FAIL` on 404; `warn` if the account can't push |
| `labels` | All four amf labels exist | `warn`, naming the missing ones |
| `checkout` | The checkout `run` would use: the enclosing one, or `./<name>` | `warn` if `run` would clone first or origin isn't `GITHUB_REPO`; `FAIL` if `./<name>` isn't a clone or there's no origin |
| `git push` | `git push --dry-run` to `origin` works with the user's git credentials, without prompting | `FAIL`, with git's message |
| `pipeline` | Fetches the default branch; shows the pipeline from `AGENTS.md` on it, or that there is none | `FAIL` if the table doesn't parse |
| `skill <stage>` | Each stage's skill downloads as in `run`; shows its size and SHA-256 | `FAIL` |
| `claudecode`, `codex`, `opencode` | Installed; shows the path and `--version` | `FAIL` if a stage needs it (or, with no pipeline, for `claude`); else `warn` |
| `claude login` | Always; says whether `CLAUDE_CODE_OAUTH_TOKEN` is set or Claude uses its own login | |

Checks that need GitHub, the repo or a checkout are `skip` without them.
Agents' own logins aren't checked: their CLIs can't report them reliably.

## Configuration

amf reads `./.env`, then `.env` at the top of the git checkout. Environment
variables override the file. Values ending in `...`, and `owner/name`, are
placeholders and count as unset. The file has `KEY=value` lines, optionally
prefixed with `export ` and quoted; `#` starts a comment line or, after a
space, a trailing comment. A leading byte order mark is ignored.

| Variable | Default | Purpose |
|---|---|---|
| `GITHUB_TOKEN` | required | API access: Contents, Issues, Pull requests (read & write) |
| `GITHUB_REPO` | required | `owner/name`; github.com URLs are accepted |
| `CLAUDE_CODE_OAUTH_TOKEN` | unset | Claude subscription token, passed to Claude |
| `AMF_CLAUDE` | `claude` | Path to the Claude CLI |
| `AMF_CODEX` | `codex` | Path to the Codex CLI, for pipelines |
| `AMF_OPENCODE` | `opencode` | Path to the opencode CLI, for pipelines |
| `AMF_API_URL` | `https://api.github.com` | GitHub API base URL |
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
  own default.

`run` and `watch` read the table from `AGENTS.md` on `origin/<default>`, so a
pipeline only changes through a merge, and `watch` uses it from the next
issue on. A repo without the file or the table is processed with one
`claude` run, as described below.

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
- **Environment:** see step 6 below. Codex and opencode keep their own
  logins and API keys.
- **Output files:** the final-message files are `.amf/<stage>.md` inside the
  worktree, which git ignores.

## Processing one issue

1. **Preflight.** Confirm `GITHUB_REPO` exists and `GITHUB_TOKEN` can reach it.
   A 404 means the repo or token is wrong; a 401 means the token was rejected.
2. **Checkout.** Use the enclosing git checkout. Outside one, clone the repo
   into `./<name>` with the user's git credentials, or reuse an existing clone
   there. Refuse if `./<name>` exists but isn't a clone of the repo. Add
   `/.amf/` to `.git/info/exclude`.
3. **Check.** The issue must be an open issue, not a PR. Fetch the default
   branch and read the pipeline from `AGENTS.md` on it. A table that doesn't
   parse, or an agent that isn't installed, stops the run here without
   changing the issue. `watch` then logs the error and tries again on its
   next poll.
4. **Claim.** Label the issue `amf:in-progress` and remove `amf:ready`.
5. **Worktree.** Fetch the default branch and create
   `.amf/worktrees/issue-N` on `amf/issue-N`, reset to `origin/<default>`. A
   worktree left from an earlier run is discarded first.
6. **Agent.** Without a pipeline, run Claude as in the Agents table above,
   with no model.
   - **Prompt:** the issue's title and body, plus instructions to make the
     smallest complete change, run the project's tests and build, not touch
     git or GitHub, and reply with only a PR description.
   - **Environment:** `PWD` is the worktree, and no agent gets
     `GITHUB_TOKEN`. When `CLAUDE_CODE_OAUTH_TOKEN` is set, it's passed to
     Claude only, and Claude's `ANTHROPIC_API_KEY` is removed.
   - **Log:** output streams live to `.amf/logs/issue-N.log`.
   - **Timeout:** the agent is killed after `--timeout`.
   - **Summary:** the final message becomes the summary.

   With a pipeline, this step and the next are replaced by the steps in
   "With a pipeline" below.
7. **Commit.** Stage everything and commit as `Fix #N: <title>` with the body
   `Closes #N`. Fail if the branch doesn't change any file compared with
   `origin/<default>`, even if Claude made commits of its own.
8. **Push.** `git push --force -u origin amf/issue-N`, with the user's git
   credentials.
9. **PR.** Open `amf/issue-N` → `<default>`, titled `Fix #N: <title>`. If
   the branch already has an open PR (a retry), the push has updated it, so
   amf replaces that PR's title and body instead. The
   body is the summary (or `Resolves #N.` if there isn't one), then any
   pipeline sections, then `Closes #N` and an "Opened by amf" footer. amf
   first removes Claude attribution lines ("Generated with Claude Code",
   `Co-Authored-By`) and the agent's own `Closes`/`Fixes`/`Resolves #N` line
   from the summary, and the attribution lines from the plan and review too.
   That way each PR is credited to the user alone and closes the issue once.
10. **Finish.** Comment `amf opened <PR URL> for this issue.` (`updated` for
    a retry's PR), set `amf:done`, and remove the worktree. The branch and
    logs are kept.

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
   - amf commits any changes as `amf: code round K`.
   - The run fails with `the code stage (<agent>) finished without making
     any changes` if the branch doesn't change any file compared with
     `origin/<default>`, such as when a round undoes the one before.
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

**On failure** in any step after Claim: set `amf:failed`, comment the error on
the issue, and keep the worktree and logs for inspection. This also happens if
the run is interrupted. If Claim itself fails, the issue can be left on
`amf:in-progress`, which `watch` skips; `amf run N` recovers it.

## Updating (`amf update`)

1. Resolve the target: `--version` (a leading `v` is added if missing), or
   the tag that `https://github.com/vicgupta/auto-mini-factory/releases/latest`
   redirects to. If it equals the running version, print `amf <v> is up to
   date` and stop. `--check` stops here too and reports whether a newer
   release is available.
2. Download `amf_<tag>_<os>_<arch>.tar.gz` and `checksums.txt` from that
   release. If the archive is missing, fail with `no amf <tag> for <os>/<arch>`.
   If its SHA-256 doesn't match, fail. The binary is
   `amf_<tag>_<os>_<arch>/amf` inside the archive; if it isn't there, fail.
3. Write the archive's `amf` next to the running executable (symlinks
   resolved) as a temporary file with permissions 0755. Run it with
   `version`, and fail unless it prints `amf <tag>`.
4. Rename it over the running executable. A folder that can't be written
   fails with a hint to rerun with permission.

Any failure leaves the installed binary unchanged.

## Files

| Path | Contents |
|---|---|
| `.env`, `.env.NNNN` | Configuration and its backups (permissions 0600, git-excluded) |
| `.amf/worktrees/issue-N/` | Working copy for an issue in progress or failed |
| `.amf/logs/issue-N.log` | Claude's stream-json output for the latest run of issue N, without a pipeline |
| `.amf/logs/issue-N-<stage>.log` | Each pipeline stage's output: `plan`, `code-K`, `eval-K` |
| `.amf/runs/issue-N/` | The latest pipeline run's `plan.md` and `eval-K.md` |
| `.amf/opencode.json` | The config amf runs opencode with |

amf only ever adds to `.git/info/exclude`. It never edits a tracked `.gitignore`.

## Security model

- Agents run with every permission check off. An issue's body is effectively
  code run on the user's machine, so label only issues you trust.
- A skill is instructions run the same way. Reference only skills you
  control or have reviewed, at a tag or commit, ideally with a `sha256` pin.
  The pipeline itself changes only through a merge to the default branch.
- No agent gets `GITHUB_TOKEN`, but agents can read local files, including
  `.env`. For real isolation, run amf in a container or VM.
- Plan and eval can't change the branch: amf resets the worktree after them,
  so only the code stage's changes reach the PR.
- amf does all git and GitHub writes itself, through the user's credentials
  and the configured token.

## Non-goals

- Running issues in parallel. `watch` processes one at a time.
- Reviewing, merging, or retrying failed PRs. People do that.
- Sandboxing agents.
- Skills that need authentication to download.
