# amf examples cookbook

These recipes use a POSIX shell. Install [amf and an agent](../README.md#install)
first, and clone the GitHub repository you want amf to work on. Git needs a
commit name and email and credentials that can push to that repository;
`GITHUB_TOKEN` authorizes API calls, not git pushes.

Replace paths and example values before pasting. After first-run setup, run
each recipe from the root of that same checkout. The examples use Codex with
`OPENAI_API_KEY`; a pipeline, if present, chooses its own agents instead.
See the [command reference](COMMANDS.md) for other agents and flags.

## 1. First-run setup

In the editor, fill in `GITHUB_TOKEN` (Contents, Issues and Pull requests:
read & write), check `GITHUB_REPO`, and set `OPENAI_API_KEY`. Set
`AMF_TRUSTED` to the GitHub logins allowed to open issues for amf, including
the token's account. Keep `.env` out of git.

```sh
cd /path/to/your-repo
amf init
"${EDITOR:-vi}" .env
amf init
amf doctor
```

The first init writes `.env` and infers the repository from `origin`. The
second creates any missing amf labels using the credentials you filled in.
Doctor changes nothing and exits 1 if a check fails; fix any `FAIL` before
continuing.

## 2. One issue to a PR

Create a small, concrete issue and pass its actual number to `run`, rather
than assuming it will be issue #1. `amf issue create` prints `#N URL`.

```sh
set -eu
created=$(amf issue create "Document the test command" \
  --body "Add the project's existing test command to README.md, with a short explanation of how to run it locally.")
amf run "${created%% *}" --agent codex
amf pr list
```

amf commits and pushes the change, opens a PR and comments its link on the
issue. The PR stays open for review. Success marks the issue `amf:done`;
failure marks it `amf:failed` and comments the reason. To retry after fixing
the cause, run `amf run` again with the same issue number.

## 3. Work through a queue with watch

Create issues, then leave a watcher running. Use a checkout where another
amf process is not already working.

```sh
set -eu
amf issue create "Document the build command" \
  --body "Add the project's existing build command to README.md and describe where its output goes."
amf issue create "Document required environment variables" \
  --body "List the environment variables the application currently requires, with descriptions and no real secrets."
amf watch --agent codex --interval 60s --draft
```

Issue creation adds `amf:ready`. Watch processes open issues with that label,
oldest first, one at a time, and keeps polling for more. This recipe opens
draft PRs for review. Stop the watcher with Ctrl-C.

## 4. Declare a plan → code → eval pipeline

Start on a clean checkout of your default branch, with permission to push
to it and no existing `## amf` section in `AGENTS.md`. This example uses
Codex's default model for all three stages and starter skills pinned to a
release. Review the skills before running them.

```sh
set -eu
cat >> AGENTS.md <<'EOF'

## amf

| Stage | Skill | Agent | Model |
|---|---|---|---|
| plan | https://raw.githubusercontent.com/vicgupta/auto-mini-factory/v0.2.0/skills/plan/SKILL.md | codex | default |
| code | https://raw.githubusercontent.com/vicgupta/auto-mini-factory/v0.2.0/skills/code/SKILL.md | codex | default |
| eval | https://raw.githubusercontent.com/vicgupta/auto-mini-factory/v0.2.0/skills/eval/SKILL.md | codex | default |
EOF
amf pipeline --fetch
git add AGENTS.md
git commit -m "Configure amf plan, code and eval stages"
git push origin HEAD
created=$(amf issue create "Document the development workflow" \
  --body "Document the existing install, build and test commands in README.md. Verify each against the project configuration.")
amf run "${created%% *}" --eval-rounds 2
amf pr list
```

`amf pipeline --fetch` checks the local table, agent installation and skill
downloads. Runs read `AGENTS.md` from the remote default branch, so publish
the table first. If your branch requires a PR, merge the table through
your normal review process before creating and running the issue.
Eval can send the change back to code up to twice with `--eval-rounds 2`.

## 5. One sentence to merged PRs with auto

Use a repository where you intend to merge agent changes without PR review.
Auto creates up to five issues by default, processes them in order and
merges each PR. Stop any watcher before starting this recipe.

```sh
set -eu
amf doctor
amf auto "Document the existing install, build and test commands in README.md" \
  --agent codex --max-issues 3 --merge=squash --wait-for-checks
amf auto status
amf pr list --state closed
```

When issues remain after the first merge, auto shows a checkpoint and asks
whether to continue. `--wait-for-checks` waits up to five minutes for checks
on that merge commit; it reports their result before approval and does not
prevent you from approving failed or unavailable checks. Branch protection
can refuse the merge itself.

Without a terminal, auto saves progress and exits at the checkpoint. After
reviewing it, `amf auto --continue` approves and resumes the saved run. That
command also retries a failed issue after you fix its cause. Status shows
saved progress; after completion it reports that no run is saved.

## 6. Run one issue in CI

Use this as a shell step on a runner with a checkout of the target repo and
`amf`, `git`, `gh` and `codex` on `PATH`. Supply `GITHUB_TOKEN`, `GITHUB_REPO`
and `OPENAI_API_KEY` through the CI environment, plus `AMF_TRUSTED` as an
allowlist of issue authors and `ISSUE_NUMBER` from a trusted manual input.
The token needs Contents, Issues and Pull requests read & write access, and
the repository must allow it to create PRs. This recipe uses environment
variables directly; it needs no `.env` file.

```sh
set -eu
: "${GITHUB_TOKEN:?Set the CI GitHub token}"
: "${GITHUB_REPO:?Set owner/name}"
: "${OPENAI_API_KEY:?Set the CI Codex API key}"
: "${AMF_TRUSTED:?Set the allowed GitHub issue authors}"
: "${ISSUE_NUMBER:?Set the issue number to process}"
export GH_TOKEN="$GITHUB_TOKEN"
gh auth setup-git
git config user.name "amf CI"
git config user.email "amf-ci@example.com"
amf init
amf doctor
amf run "$ISSUE_NUMBER" --agent codex --draft --agent-timeout 30m
amf pr list
```

Init ensures the labels exist; real environment variables override its
generated placeholders. The run opens a draft PR and exits without a
prompt. Allow enough job time for the agent runs (including any pipeline
stages), and serialize jobs for the same checkout. Agents run with all
permission checks off; use a runner intended for the trusted issues you
execute, as described in the [security model](../README.md#security).
