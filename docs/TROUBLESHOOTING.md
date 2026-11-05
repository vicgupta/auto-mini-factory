# Troubleshooting

Start with `amf doctor` in the folder where you run amf. It checks configuration,
GitHub access, git credentials, agents and pipeline skills without processing
an issue. `FAIL` means something needs fixing; warnings alone exit 0.
See the [command reference](COMMANDS.md) for flags and log locations.

The commands below use issue **42** and PR **101** as examples; substitute
your own numbers. Run them from the same checkout or folder holding `.env`
as the failed command.

## PR won't merge: conflicts

**Symptom:** `amf run 42 --merge` reports that the PR is not mergeable.

**Cause:** The PR conflicts with the default branch. amf retries GitHub's
temporary “not mergeable” response up to three times, but cannot resolve
conflicts during the merge step.

**Fix:** Inspect the existing PR with `amf pr view 101` and `amf pr diff 101`.
Resolve conflicts on its branch and push the resolution, then run
`amf pr merge 101`. Alternatively, `amf run 42 --merge --force` regenerates the
change from the latest default branch and updates the open PR. A rerun starts
with a fresh worktree and force-pushes the branch, replacing manual edits.

## PR won't merge: required checks

**Symptom:** The PR stays open and the merge error says required checks are
pending or failing. `amf run` exits 1 even though it opened a PR.

**Cause:** Branch protection requires checks. `amf run --merge` attempts the
merge immediately; it does not wait for CI. The issue is `amf:pr-open`
because its PR exists but isn't merged.

**Fix:** Open the PR with `amf pr view 101`, inspect its checks on GitHub,
and fix failing checks or wait for pending ones. Once they pass, run
`amf pr merge 101` (or `--squash` / `--rebase` for another merge method).
If checks never appear, see [check runs not appearing](#check-runs-not-appearing).

## PR won't merge: required review

**Symptom:** GitHub refuses the merge because approval is required, or the
PR is still a draft.

**Cause:** Repository rules require a review, or the PR was opened with
`--draft`. amf does not bypass review rules. It also leaves changes to the
top-level `AGENTS.md`, which controls the pipeline, for manual review.

**Fix:** Use `amf pr view 101` to inspect the PR. For a draft, run
`amf pr ready 101`. Obtain the required approvals on GitHub, then run
`amf pr merge 101`. Review pipeline changes before merging them.

## Agent is “not usable” or no coding agent is available

**Symptom:** amf skips an installed agent, reports “not usable”, or stops
before processing the issue because no agent is available.

**Cause:** The executable is missing from `PATH`, or automatic selection
cannot find its credentials. Claude needs `CLAUDE_CODE_OAUTH_TOKEN` or
`ANTHROPIC_API_KEY`; Codex needs `OPENAI_API_KEY`. Their saved interactive
logins alone do not make them eligible for automatic selection. Paid
`opencode/` and `opencode-go/` models need `OPENCODE_API_KEY` or a saved login
for the provider.

**Fix:** Run `amf doctor` to see the executable paths, credential sources,
and selected agent. Install the missing CLI or set the corresponding key in
`.env` or the environment. To use an installed agent's own login, log in to
it interactively and explicitly select it, for example
`amf run 42 --agent claudecode` or `amf run 42 --agent codex`.
For OpenCode, `opencode auth login` saves a login; check `OPENCODE_MODEL`
against `opencode models`. A pipeline selects each stage's agent and ignores
`--agent`, so fix the credentials for the agent named in its table.

## Saved auto state blocks a new run

**Symptom:** A new `amf auto` sentence is refused because
`.amf/auto/state.json` already exists.

**Cause:** A previous run paused at a checkpoint, failed, or was interrupted.
State stays until the run finishes or is aborted; it is not necessarily stale.

**Fix:** Stop any active auto process first. Run `amf auto status` to inspect
the saved run. Use `amf auto --continue` to resume it (and approve a reached
checkpoint). To abandon it, run `amf auto --abort`, then start a new sentence.
Plain abort also removes corrupt or unsupported state and leaves GitHub
issues unchanged. Use `amf auto --abort --close-issues` to close the saved
run's unmerged issues before removing state. Prefer these commands to
deleting the file by hand. Outside a checkout, state lives under
`./<repo name>/.amf/auto/state.json`.

## GITHUB_TOKEN has insufficient access

**Symptom:** API calls fail with 403, “Resource not accessible”, or 404 for a
private repo; reading the repo works but creating issues, labels or PRs fails.

**Cause:** The token lacks access to this repository or the permissions for
the failed operation. An expired or revoked token can instead produce 401.
The account also needs permission to perform the operation.

**Fix:** Edit `.env` to use a token authorized for the target repository.
For a fine-grained PAT, grant Contents, Issues and Pull requests read/write
access, as described in [configuration](CONFIGURATION.md#configuration-env);
check any required organization approval. A classic PAT needs `repo` for
private repositories. See GitHub's
[permission reference](https://docs.github.com/en/rest/authentication/permissions-required-for-fine-grained-personal-access-tokens).
Run `amf doctor`, then retry the failed command. A passing read check does
not prove every write permission is present.

Nonempty environment variables override `.env`; update or unset an old
exported `GITHUB_TOKEN` if editing the file has no effect. Never paste tokens
into issue comments or logs. Git pushes use your own git credentials,
separately from amf's API token: if the `git push` doctor check fails, run
`gh auth setup-git` after logging in to GitHub CLI, then rerun `amf doctor`.

## GITHUB_REPO is misspelled or points to the wrong repo

**Symptom:** amf says `GITHUB_REPO` was not found, or warns that `origin`
differs from `GITHUB_REPO`.

**Cause:** The owner/name is wrong, an old environment value overrides
`.env`, or amf is running inside a different checkout. A private repo hidden
from the token also returns 404; that response alone cannot identify a typo.

**Fix:** Run `git remote get-url origin` in the intended checkout and compare
it with `GITHUB_REPO` in `.env`. Set the correct `owner/name`; update or
unset an old exported `GITHUB_REPO`. Run `amf doctor` to confirm which value
and source it uses, then retry `amf run 42`. If the spelling is correct,
check [token access](#github_token-has-insufficient-access).

## Pipeline skill fails to fetch

**Symptom:** `amf doctor` shows `FAIL skill <stage>`, or a run reports an
error downloading a skill or a SHA-256 pin mismatch.

**Cause:** The URL is unavailable, returns an error or a non-HTTPS redirect,
or the content changed after it was pinned. Skills must be HTTPS, UTF-8
text no larger than 256 KB. amf sends no credentials when fetching them.

**Fix:** In the target checkout, run `amf pipeline --fetch` to test the
local `AGENTS.md` table. Correct its URL to an accessible raw text file,
restore network access, or reduce an oversized skill. For a pin mismatch,
review the changed skill before updating `#sha256=` to its new digest.
Merge the corrected table into the default branch: `amf run` and
`amf doctor` read that branch's table, while `amf pipeline --fetch` reads
the local file. Confirm with `amf doctor`, then retry `amf run 42`.

## Check runs not appearing

**Symptom:** An auto checkpoint reports “none reported yet”, unavailable
checks, or a timeout waiting for checks.

**Cause:** Auto reads check runs on the **merge commit**, once by default.
CI may still be starting, run only on the PR head, or exclude the default
branch through trigger filters. API credentials may also lack check access.

**Fix:** Run `amf auto status` to find the saved merge commit and inspect
that commit's checks and the Actions tab on GitHub. Ensure CI runs for
pushes to the default branch if you want it reported at auto checkpoints.
To poll every 10 seconds for up to 5 minutes at a saved checkpoint, run
`amf auto --continue --wait-for-checks`. This also approves the checkpoint
after displaying the result, even if checks fail or time out; inspect CI
first if you are not ready to approve. For future runs, use
`amf auto "Add a dark mode toggle" --wait-for-checks`.

For private-repo check reads, classic PATs need `repo`; GitHub App tokens
need Checks read access. Check the supported credentials in GitHub's
[check runs documentation](https://docs.github.com/en/rest/checks/runs#list-check-runs-for-a-git-reference)
if the endpoint returns 403. `amf doctor` checks repository access, but
does not test the Checks API. Waiting cannot create missing CI runs.

## Another amf holds the lock

**Symptom:** `amf watch` (or `run`, `auto`) exits with `another amf (watch,
pid 1234 on host, since ...) holds .amf/watch.lock`.

**Cause:** Only one amf works in a checkout at a time, so two watchers never
race on the same issue.

**Fix:** Stop the other amf. If it is gone (another machine, a crash), amf
takes the lock over by itself once the PID is dead on this host or the lock
is 10 minutes old; or delete `.amf/watch.lock`.

## Issue skipped: already has a PR or branch

**Symptom:** `watch` logs `#42 already has an open pull request ...` or
`#42: branch amf/issue-42 already exists on origin ...`; `amf run 42` prints
the reason once, reports it as skipped in its summary, and exits 3.

**Cause:** amf won't start an issue it already turned into a PR. An open PR
relabels the issue `amf:pr-open`.

**Fix:** Review and merge the PR. To regenerate it, run `amf run 42 --force`.
For a leftover branch without a PR, delete it (`git push origin --delete
amf/issue-42`) and add `amf:ready` again.

## Issue stuck on amf:in-progress

**Symptom:** An issue keeps `amf:in-progress` after amf was killed.

**Fix:** `amf watch` puts it back to `amf:ready` once the label is older than
`AMF_STALE_TIMEOUT` (default twice the agent timeout, at least 2h), and
comments that it did. Use `--stale-timeout 30m` for sooner, or run
`amf run 42` now.

## Fallback agent didn't run

**Symptom:** The failure comment says `Fallback: did not run, because ...`.

**Cause and fix:** The reason names it: `--agent` picks a single agent,
`AMF_SECONDARY_AGENT=none` (or the same as the primary), or the secondary
agent isn't usable (not installed, or no key: e.g. claude without
`CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY`). `amf doctor` shows each
agent's status.
