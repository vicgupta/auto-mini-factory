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
with `OPENAI_API_KEY`, or `opencode`, or any of them named with `--agent`. A
pipeline needs whichever agents it names.

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
| `amf list [--state open\|closed\|all] [--label L]` | Lists issues, newest first (highest number first), with every page fetched and PRs left out. Columns: `ISSUE`, `STATE` (with `--state all` only), `AMF` (the issue's `amf` and `amf:*` labels, or `-`) and `TITLE`. Prints `No … in owner/name.` when nothing matches. Needs no git checkout. |
| `amf run <N> [--agent A] [--agent-timeout 60m] [--eval-rounds 2] [--log-level L] [--log-file P]` | Processes issue N once. |
| `amf watch [--agent A] [--interval 60s] [--label amf:ready] [--agent-timeout 60m] [--eval-rounds 2] [--log-level L] [--log-file P]` | Polls open issues with the label (every page), oldest first by issue number (GitHub's `created_at` can tie for issues opened in the same second), and processes them one at a time until interrupted. |
| `amf pipeline [--fetch]` | Checks the pipeline in `AGENTS.md` of the enclosing checkout, as it is on disk. See below. |
| `amf doctor` | Checks the whole setup, one line per check, changing nothing. See below. |
| `amf update [--version vX.Y.Z] [--check]` | Replaces the running binary with the latest release, or with the given one, which can be a downgrade. Needs no `.env`. See below. |
| `amf version` | Prints the version: the release tag, or the module version for `go install`. |

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
- `--eval-rounds` is how many times a pipeline's eval stage may send the
  change back to the code stage. A negative value is an error.
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
  every second and when amf exits, Ctrl-C included.
- `--log-level` is `debug`, `info`, `warn` or `error` (case is ignored),
  and so is `AMF_LOG_LEVEL`, which the flag overrides. Either sets the
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
  | `opencode` | installed (it needs no key) |

  Keys come from the environment or `.env`; placeholders don't count. An
  installed agent without its key is skipped, even if it has a login of its
  own. With none usable, the issue isn't touched (see step 3 below).
- `watch` logs at startup one line per agent: where it's installed (or `not
  installed`), whether its key is set (naming the one that is), for opencode
  the model it runs with, and `usable` or `skipped`. Then it logs which agent issues without a pipeline run, or a
  warning that none is available.

**Exit codes:** 0 on success, 1 on error, 2 on a usage error. SIGINT and
SIGTERM cancel the current step, with the run's process group killed as on
expiry; the issue is still marked failed.

### `amf init`

1. Builds `.env` from a template of `GITHUB_TOKEN`, `GITHUB_REPO`,
   `CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`,
   `OPENCODE_MODEL`, `AMF_AGENT_TIMEOUT`, `AMF_LOG_LEVEL`, `AMF_TRUSTED`
   and `AMF_POSTGRES_URL`.
   - `OPENCODE_MODEL`, `AMF_AGENT_TIMEOUT`, `AMF_LOG_LEVEL` and
     `AMF_POSTGRES_URL` are written
     commented out, as `# OPENCODE_MODEL=opencode/big-pickle   # Use 'opencode models' to list all the models`
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
| `github` | `GET /user` accepts the token; shows the login | `FAIL` on 401 or no connection |
| `repo` | The repo is readable; shows its default branch | `FAIL` on 404; `warn` if the account can't push |
| `labels` | All four amf labels exist | `warn`, naming the missing ones |
| `trusted` | `AMF_TRUSTED` is set; lists the logins | `warn` if unset (anyone's issues run), or if it leaves out the token's account, which `amf issue` opens issues as |
| `agent timeout` | Shows the limit each agent run gets: `AMF_AGENT_TIMEOUT` (and from where) or the 60m default | `FAIL` if `AMF_AGENT_TIMEOUT` isn't a duration or is out of range |
| `checkout` | The checkout `run` would use: the enclosing one, or `./<name>` | `warn` if `run` would clone first or origin isn't `GITHUB_REPO`; `FAIL` if `./<name>` isn't a clone or there's no origin |
| `git push` | `git push --dry-run` to `origin` works with the user's git credentials, without prompting | `FAIL`, with git's message |
| `git identity` | git has a name and email to commit with, in the checkout (or here); shows them | `FAIL`, with the `git config` commands to set them |
| `pipeline` | Fetches the default branch; shows the pipeline from `AGENTS.md` on it, or that there is none | `FAIL` if the table doesn't parse |
| `skill <stage>` | Each stage's skill downloads as in `run`; shows its size and SHA-256 | `FAIL` |
| `claudecode`, `codex`, `opencode` | Installed; shows the path and `--version`. With no pipeline, the agent amf picks adds `runs issues without a pipeline`, and the `pipeline` line names it | `FAIL` if a stage needs it. With no pipeline: one `FAIL` on `claudecode` if no agent is usable; `warn` for an installed `claude` or `codex` without its key, naming the agent amf runs instead; `warn` for one that isn't installed |
| `claude login` | Always; says whether `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY` is set (and from where), or that Claude uses its own login | |
| `codex login` | Always; says whether `OPENAI_API_KEY` is set (and from where), or that Codex uses its own login | |
| `opencode model` | `OPENCODE_MODEL` (or the default) is a line of `opencode models` | `warn` if it isn't, since every opencode run would fail, or if `opencode models` fails; `skip` without opencode |
| `postgres` | `AMF_POSTGRES_URL` is unset (logs are only local), or amf can log in to `amflogs` on it; says whether its tables exist yet. A server without `amflogs` is `ok`: `run` and `watch` create it | `FAIL` if the URL doesn't parse; `warn` if amf can't connect or log in, with the server's message. Doctor creates nothing |

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
| `CLAUDE_CODE_OAUTH_TOKEN` | unset | Claude subscription token, passed to Claude. Makes `claude` usable without `--agent` |
| `ANTHROPIC_API_KEY` | unset | Claude API key, passed to Claude when there's no `CLAUDE_CODE_OAUTH_TOKEN`. Makes `claude` usable without `--agent` |
| `OPENAI_API_KEY` | unset | OpenAI API key, passed to Codex. Makes `codex` usable without `--agent` |
| `OPENCODE_MODEL` | `opencode/big-pickle` | The model opencode runs with: always without a pipeline, and for pipeline stages that name no model. `opencode models` lists the choices |
| `AMF_TRUSTED` | unset | GitHub logins, separated by commas or spaces (a leading `@` is allowed, case is ignored). When set, amf only works on issues they opened. Unset means anyone's |
| `AMF_POSTGRES_URL` | unset | A PostgreSQL server that `run` and `watch` also write their logs to. See [Logging to PostgreSQL](#logging-to-postgresql) |
| `AMF_CLAUDE` | `claude` | Path to the Claude CLI |
| `AMF_AGENT_TIMEOUT` | `60m` | How long each agent run may take, as a Go duration; `--agent-timeout` (or its alias `--timeout`) overrides it. Must be at least 1m and at most 24h |
| `AMF_LOG_LEVEL` | `info` (terminal), `debug` (event log) | `debug`, `info`, `warn` or `error`, for both the terminal and the event log of `run` and `watch`; `--log-level` overrides it |
| `AMF_CODEX` | `codex` | Path to the Codex CLI |
| `AMF_OPENCODE` | `opencode` | Path to the opencode CLI |
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
   there. Refuse if `./<name>` exists but isn't a clone of the repo. Add
   `/.amf/` to `.git/info/exclude`.
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
     `GITHUB_TOKEN`. When `CLAUDE_CODE_OAUTH_TOKEN` is set, it's passed to
     Claude only, and Claude's `ANTHROPIC_API_KEY` is removed. Otherwise an
     `ANTHROPIC_API_KEY` from `.env` or the environment is passed to Claude.
     `OPENAI_API_KEY` from `.env` or the environment is passed to Codex.
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
     stay.
   - **Summary:** the final message becomes the summary.

   With a pipeline, this step and the next are replaced by the steps in
   "With a pipeline" below.
7. **Commit.** Stage everything and commit as `Fix #N: <title>` with the body
   `Closes #N`. Fail if the branch doesn't change any file compared with
   `origin/<default>`, even if the agent made commits of its own. The run
   also fails — before the push, so no branch or PR is created — when any
   changed path is under `.github/`, where workflows and `CODEOWNERS` live;
   the error names the offending paths. Changes elsewhere, even under a
   directory whose name contains `.github`, are fine.
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
10. **Finish.** Set `amf:done`, comment `amf opened <PR URL> for this
    issue.` (`updated` for a retry's PR), and remove the worktree. The branch
    and logs are kept. The PR exists by now, so if setting the label or
    commenting fails, amf logs a warning and the run still succeeds.

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

**On failure** in any step after Claim, up to opening the PR: set `amf:failed`, comment the error on
the issue, and keep the worktree and logs for inspection. This also happens if
the run is interrupted — except that a run that hit `--agent-timeout` leaves
the worktree (removed, with its branch unpushed) and keeps only the logs and
partial output. If Claim itself fails, the issue can be left on
`amf:in-progress`, which `watch` skips; `amf run N` recovers it.

## Updating (`amf update`)

1. Resolve the target: `--version` (a leading `v` is added if missing), or
   the tag that `https://github.com/vicgupta/auto-mini-factory/releases/latest`
   redirects to. If it equals the running version, print `amf <v> is up to
   date` and stop. `--check` stops here too and reports whether a newer
   release is available.
2. Download the archive for this platform and `checksums.txt` from that
   release: `amf_<tag>_<os>_<arch>.tar.gz` on macOS and Linux, or
   `amf_<tag>_windows_<arch>.zip` on Windows. If the archive is missing, fail
   with `no amf <tag> for <os>/<arch>`. If its SHA-256 doesn't match, fail.
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
| `.amf/opencode.json` | The config amf runs opencode with |

amf only ever adds to `.git/info/exclude`. It never edits a tracked `.gitignore`.

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
  waits up to 15 seconds to send what's queued, then warns how many lines
  only made it to the local logs.

## Security model

- Agents run with every permission check off. An issue's body is effectively
  code run on the user's machine, so label only issues you trust.
- Labels need triage access, but amf reads the issue's text when it runs,
  not when it was labeled, so its author can change it in between.
  `AMF_TRUSTED` limits amf to issues opened by the listed users. Other
  people can't edit those, apart from collaborators with write access.
  Comments are never read.
- A skill is instructions run the same way. Reference only skills you
  control or have reviewed, at a tag or commit, ideally with a `sha256` pin.
  The pipeline itself changes only through a merge to the default branch.
- No agent gets `GITHUB_TOKEN`, but agents can read local files, including
  `.env` (and so `AMF_POSTGRES_URL`'s password).
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
    `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` and the password in
    `AMF_POSTGRES_URL`, wherever they appear (values shorter than 8
    characters are not matched);
  - GitHub tokens (`ghp_`, `gho_`, `ghu_`, `ghs_`, `ghr_`, `github_pat_`),
    `sk-` API keys, AWS access key ids (`AKIA`, `ASIA`);
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
- Reviewing, merging, or retrying failed PRs. People do that.
- Sandboxing agents.
- Skills that need authentication to download.
