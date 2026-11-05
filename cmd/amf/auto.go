package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// labelAuto groups the issues one amf auto run planned.
const labelAuto = "amf:auto"

var autoLabel = LabelSpec{labelAuto, "5319e7", "Planned by amf auto from one sentence"}

// defaultAutoMaxIssues is how many issues amf auto plans at most.
const defaultAutoMaxIssues = 5

const autoChecksTimeout = 5 * time.Minute
const autoChecksPoll = 10 * time.Second

const autoStateVersion = 1

const autoHelp = `Usage:
  amf auto "<one simple sentence>" [flags]
  amf auto --continue [flags]
  amf auto status
  amf auto --abort [--close-issues]

Turns one sentence into a few issues and works through them, merging each PR.
See diagrams and tradeoffs for --merge at:
https://github.com/vicgupta/auto-mini-factory/blob/main/docs/COMMANDS.md#merge-methods

1. Plan: the agent (picked as for amf run: see amf help agents) reads the
   repo and splits the sentence into at most --max-issues issues, each with a
   title, scope and acceptance criteria. amf creates them labeled amf:auto
   only and prints their URLs.
2. Build: amf adds amf:ready immediately before processing each issue in
   order exactly as amf run --merge does:
   branch, agent, PR, merge (a merge commit by default, or --merge=squash or
   --merge=rebase). Each PR merge waits up to 30 minutes for the PR head's
   checks, as amf run --merge does with --wait-for-checks (-w) on by default;
   amf auto has no flag to skip that wait. The safety rules hold: a change
   under .github/ is never pushed, and one that edits AGENTS.md is never
   merged.
3. Checkpoint: after the first PR is merged, amf stops and prints the issue,
   the PR, the merge commit, the agent, the checks on the merge commit and
   the issues left. Nothing else is merged until you approve:
     - in a terminal, answer y to the prompt (anything else stops);
     - otherwise (or after answering no), amf saves its state in
       .amf/auto/state.json and exits; run amf auto --continue to approve
       and resume.
   The remaining issues stay labeled amf:auto only, so amf watch does not
   pick them up while auto waits.

--checkpoint-after N delays the single checkpoint until N PRs have merged
(default 1; N must be positive). There is no checkpoint if no issues remain.
--no-checkpoint skips the checkpoint and approval entirely, merging all issues.
--approve accepts the checkpoint automatically, including without a terminal.
--wait-for-checks (-w) is the checkpoint wait only: it polls the checkpoint's
merge commit every 10 seconds for up to 5 minutes, waiting for check runs to
appear and all complete, then reports pass/fail or a timeout before approval.
It does not change the 30-minute wait each PR merge already makes for its
head's checks. By default checks are fetched once. Failed, unavailable or
timed-out checks are reported; approval can still proceed.
Checkpoint settings are saved for --continue; --checkpoint-after overrides the
saved threshold, and --no-checkpoint or --wait-for-checks enables that setting.

If an issue fails or its PR can't be merged, amf auto stops there and says
so; amf auto --continue retries that issue. --dry-run only plans: it prints
the issues it would create and creates nothing.

amf auto status prints the saved sentence, merge method and each issue's
status, PR and commit without contacting GitHub or running agents.
amf auto --abort removes the saved state so another run can start. With
--close-issues, it first closes the created, unmerged issues on GitHub;
if closing fails, the state is kept so you can retry. Plain --abort leaves
GitHub issues unchanged. Stop any active auto process before aborting.
Add amf:ready to any remaining issues to let amf watch process them.
`

// autoState is what amf auto saves in .amf/auto/state.json, so --continue
// can resume where it stopped.
type autoState struct {
	Version         int         `json:"version"`
	Sentence        string      `json:"sentence"`
	Repo            string      `json:"repo"`
	Merge           string      `json:"merge"`
	Created         time.Time   `json:"created"`
	Approved        bool        `json:"approved"` // the checkpoint was approved
	NoCheckpoint    bool        `json:"no_checkpoint,omitempty"`
	CheckpointAfter int         `json:"checkpoint_after,omitempty"`
	WaitForChecks   bool        `json:"wait_for_checks,omitempty"`
	Issues          []autoIssue `json:"issues"`
}

// autoIssue is one planned issue and how far amf got with it: pending,
// merged or failed. Number is zero until the issue has been created.
type autoIssue struct {
	Number     int      `json:"number"`
	Title      string   `json:"title"`
	Scope      string   `json:"scope"`
	Acceptance []string `json:"acceptance_criteria"`
	URL        string   `json:"url"`
	Status     string   `json:"status"`
	PR         string   `json:"pr,omitempty"`
	PRNumber   int      `json:"pr_number,omitempty"`
	Commit     string   `json:"commit,omitempty"`
	Agent      string   `json:"agent,omitempty"`
	Error      string   `json:"error,omitempty"`
}

func (st *autoState) merged() int {
	n := 0
	for _, is := range st.Issues {
		if is.Status == "merged" {
			n++
		}
	}
	return n
}

func (st *autoState) checkpointDue() bool {
	n := st.merged()
	return !st.NoCheckpoint && !st.Approved && n >= max(1, st.CheckpointAfter) && n < len(st.Issues)
}

func (st *autoState) remaining() []autoIssue {
	var out []autoIssue
	for _, is := range st.Issues {
		if is.Status != "merged" {
			out = append(out, is)
		}
	}
	return out
}

func autoStatePath(root string) string { return filepath.Join(root, ".amf", "auto", "state.json") }

func loadAutoState(path string) (*autoState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st autoState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if st.Version != autoStateVersion {
		return nil, fmt.Errorf("%s: unsupported amf auto state version %d (expected %d)", path, st.Version, autoStateVersion)
	}
	return &st, nil
}

func saveAutoState(path string, st *autoState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// plannedIssue is one issue as the planning agent proposes it.
type plannedIssue struct {
	Title      string   `json:"title"`
	Scope      string   `json:"scope"`
	Acceptance []string `json:"acceptance_criteria"`
}

var jsonFence = regexp.MustCompile("(?s)```(?:json|JSON)?[ \t]*\n(.*?)```")

// parsePlan reads the planning agent's reply: a JSON object
// {"issues": [...]}, or a bare array, in a fenced block or on its own. The
// last fenced block that parses wins. It checks there are 1 to max issues,
// each with a title, a scope and at least one acceptance criterion.
func parsePlan(out string, max int) ([]plannedIssue, error) {
	var candidates []string
	ms := jsonFence.FindAllStringSubmatch(out, -1)
	for i := len(ms) - 1; i >= 0; i-- {
		candidates = append(candidates, ms[i][1])
	}
	candidates = append(candidates, out)
	if i, j := strings.Index(out, "{"), strings.LastIndex(out, "}"); i >= 0 && j > i {
		candidates = append(candidates, out[i:j+1])
	}
	var issues []plannedIssue
	parsed := false
	for _, c := range candidates {
		c = strings.TrimSpace(c)
		var obj struct {
			Issues []plannedIssue `json:"issues"`
		}
		if json.Unmarshal([]byte(c), &obj) == nil && obj.Issues != nil {
			issues, parsed = obj.Issues, true
			break
		}
		var arr []plannedIssue
		if json.Unmarshal([]byte(c), &arr) == nil && arr != nil {
			issues, parsed = arr, true
			break
		}
	}
	if !parsed {
		return nil, fmt.Errorf(`the plan has no JSON {"issues": [...]} in it: %q`, truncate(strings.TrimSpace(out), 300))
	}
	if len(issues) == 0 {
		return nil, errors.New("the plan has no issues")
	}
	if len(issues) > max {
		return nil, fmt.Errorf("the plan has %d issues, more than the %d allowed (--max-issues)", len(issues), max)
	}
	for i := range issues {
		is := &issues[i]
		is.Title, is.Scope = strings.TrimSpace(is.Title), strings.TrimSpace(is.Scope)
		var ac []string
		for _, a := range is.Acceptance {
			if a = strings.TrimSpace(a); a != "" {
				ac = append(ac, a)
			}
		}
		is.Acceptance = ac
		switch {
		case is.Title == "":
			return nil, fmt.Errorf("planned issue %d has no title", i+1)
		case len(is.Title) > 200:
			return nil, fmt.Errorf("planned issue %d's title is longer than 200 characters", i+1)
		case is.Scope == "":
			return nil, fmt.Errorf("planned issue %d (%s) has no scope", i+1, is.Title)
		case len(is.Acceptance) == 0:
			return nil, fmt.Errorf("planned issue %d (%s) has no acceptance criteria", i+1, is.Title)
		}
	}
	return issues, nil
}

func autoPlanPrompt(repo, sentence string, max int) string {
	return fmt.Sprintf(`You are planning work in a git checkout of the GitHub repository %s.
Read the repository (README, docs, code layout) to understand it, but do not change any file.

Split this request into at most %d small, independent GitHub issues, in the order they should be done:

%s

Each issue must be small enough for one pull request and must not touch .github/ or AGENTS.md.
Prefer fewer issues: a simple request is often one issue.

Reply with only a JSON block in this exact shape, and nothing else:

`+"```json"+`
{"issues": [{"title": "short imperative title", "scope": "what to change and where, in a few sentences", "acceptance_criteria": ["a checkable criterion", "another"]}]}
`+"```"+`
`, repo, max, sentence)
}

// autoIssueBody is the body of a planned issue.
func autoIssueBody(p plannedIssue, sentence string, i, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Scope\n\n%s\n\n## Acceptance criteria\n\n", p.Scope)
	for _, a := range p.Acceptance {
		fmt.Fprintf(&b, "- [ ] %s\n", a)
	}
	fmt.Fprintf(&b, "\n---\n_Planned by `amf auto` (%d of %d) from: %q_\n", i, n, sentence)
	return b.String()
}

// autoPlan runs the plan's agents, in order, in a detached worktree of
// origin/base until one replies with a plan that parses.
func (r *Runner) autoPlan(ctx context.Context, sentence string, max int) ([]plannedIssue, error) {
	base, err := r.GH.DefaultBranch(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := r.agentPlan()
	if err != nil {
		return nil, err
	}
	wt := filepath.Join(r.Root, ".amf", "worktrees", "auto-plan")
	if _, err := git(ctx, r.Root, "fetch", "origin", base); err != nil {
		return nil, err
	}
	if _, err := os.Stat(wt); err == nil {
		if _, err := git(ctx, r.Root, "worktree", "remove", "--force", wt); err != nil {
			os.RemoveAll(wt)
		}
	}
	if _, err := git(ctx, r.Root, "worktree", "prune"); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		return nil, err
	}
	if _, err := git(ctx, r.Root, "worktree", "add", "--detach", wt, "origin/"+base); err != nil {
		return nil, err
	}
	defer func() {
		if _, err := git(context.WithoutCancel(ctx), r.Root, "worktree", "remove", "--force", wt); err != nil {
			r.event(slog.LevelWarn, "auto.plan", 0, fmt.Sprintf("could not remove the planning worktree: %v", err), "error", err)
		}
	}()
	var failed []string
	for _, agent := range plan.Agents {
		model := r.agentModel(agent, "")
		r.event(slog.LevelInfo, "auto.plan", 0, fmt.Sprintf("planning with %s (timeout %s)", agentName(agent), r.Timeout),
			"action", "planning", "agent", agentName(agent), "model", model, "sentence", sentence, "max_issues", max)
		log := filepath.Join(r.Root, ".amf", "logs", fmt.Sprintf("auto-plan-%s.log", agentName(agent)))
		out, err := r.runAgent(ctx, agent, "", model, wt, autoPlanPrompt(r.Repo, sentence, max), log, filepath.Join(wt, ".amf", "plan.md"))
		if err == nil {
			var issues []plannedIssue
			if issues, err = parsePlan(out, max); err == nil {
				r.lastAgent = agentName(agent)
				return issues, nil
			}
		}
		if ctx.Err() != nil {
			return nil, err
		}
		failed = append(failed, fmt.Sprintf("%s: %v", agentName(agent), err))
		r.event(slog.LevelWarn, "auto.plan", 0, fmt.Sprintf("planning with %s failed: %v", agentName(agent), err),
			"action", "failed", "agent", agentName(agent), "error", err)
	}
	return nil, fmt.Errorf("could not plan the work: %s", strings.Join(failed, "; "))
}

// approver asks whether to go on past the checkpoint. asked is false when
// it couldn't ask (no terminal) or approval was supplied by --approve.
type approver func(prompt string) (asked, yes bool)

// terminalApprover asks on stdin when it is a terminal.
func terminalApprover(stdin io.Reader, stdout io.Writer) approver {
	return func(prompt string) (bool, bool) {
		if !isTerminal(stdin) {
			return false, false
		}
		fmt.Fprint(stdout, prompt)
		line, _ := bufio.NewReader(stdin).ReadString('\n')
		a := strings.ToLower(strings.TrimSpace(line))
		return true, a == "y" || a == "yes"
	}
}

// autoCreateIssues creates only the issues not yet assigned a number,
// saving the full plan after each successful creation before continuing.
func (r *Runner) autoCreateIssues(ctx context.Context, st *autoState, path string, stdout io.Writer) error {
	ensuredLabel := false
	for i := range st.Issues {
		is := &st.Issues[i]
		if is.Number != 0 {
			continue
		}
		if !ensuredLabel {
			if _, err := r.GH.EnsureLabel(ctx, autoLabel); err != nil {
				return err
			}
			ensuredLabel = true
		}
		p := plannedIssue{Title: is.Title, Scope: is.Scope, Acceptance: is.Acceptance}
		r.memTriage(0, is.Title, is.Scope)
		body := r.Redact.Redact(autoIssueBody(p, st.Sentence, i+1, len(st.Issues)))
		created, err := r.GH.CreateIssue(ctx, is.Title, body, []string{labelAuto})
		if err != nil {
			return fmt.Errorf("creating issue %d (%s): %w", i+1, is.Title, err)
		}
		is.Number, is.URL = created.Number, created.HTMLURL
		if err := saveAutoState(path, st); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "#%d %s\n", is.Number, is.URL)
	}
	return nil
}

// autoWork processes st's issues in order, saving st to path after each.
// After the configured number of merges, unless disabled or approved, it
// stops at the checkpoint and asks approve; nothing more is merged without a yes.
func (r *Runner) autoWork(ctx context.Context, st *autoState, path string, approve approver, stdout io.Writer) error {
	for i := range st.Issues {
		is := &st.Issues[i]
		if is.Status == "merged" {
			continue
		}
		if is.PRNumber != 0 {
			pr, err := r.GH.GetPR(ctx, is.PRNumber)
			if err != nil {
				return fmt.Errorf("checking PR #%d for issue #%d: %w", is.PRNumber, is.Number, err)
			}
			if pr.Merged {
				is.Status, is.Error = "merged", ""
				if err := saveAutoState(path, st); err != nil {
					return err
				}
				r.event(slog.LevelInfo, "auto.reconciled", is.Number, fmt.Sprintf("#%d: PR #%d is already merged; skipping", is.Number, is.PRNumber),
					"pr", is.PRNumber)
				continue
			}
		}
		if st.checkpointDue() {
			if yes, err := r.autoCheckpoint(ctx, st, path, approve, stdout, "prompt"); err != nil || !yes {
				return err
			}
		}
		if err := r.GH.AddLabels(ctx, is.Number, labelReady); err != nil {
			return fmt.Errorf("marking issue #%d ready: %w", is.Number, err)
		}
		is.Status, is.Error = "pending", ""
		err := r.Run(ctx, is.Number)
		is.PR, is.PRNumber, is.Commit, is.Agent = r.lastPR.HTMLURL, r.lastPR.Number, r.lastCommit, r.lastAgent
		if err == nil && r.lastCommit == "" {
			err = errors.New("the pull request was not merged")
		}
		if err != nil {
			is.Status, is.Error = "failed", err.Error()
			if serr := saveAutoState(path, st); serr != nil {
				return serr
			}
			r.event(slog.LevelWarn, "auto.stopped", is.Number, fmt.Sprintf("#%d: stopping: %v", is.Number, err),
				"reason", "failed", "error", err)
			fmt.Fprintf(stdout, "\namf auto stopped at #%d (%s): %v\n%s\nFix the cause, then run amf auto --continue to retry it.\n",
				is.Number, is.URL, err, remainingLine(st))
			return fmt.Errorf("#%d: %w", is.Number, err)
		}
		is.Status = "merged"
		if err := saveAutoState(path, st); err != nil {
			return err
		}
	}
	r.event(slog.LevelInfo, "auto.done", 0, fmt.Sprintf("amf auto is done: %d %s merged", len(st.Issues), plural(len(st.Issues), "issue", "issues")),
		"issues", len(st.Issues))
	fmt.Fprintf(stdout, "\namf auto is done: merged all %d %s.\n", len(st.Issues), plural(len(st.Issues), "issue", "issues"))
	return os.Remove(path)
}

func remainingLine(st *autoState) string {
	var left []string
	for _, is := range st.remaining() {
		left = append(left, fmt.Sprintf("#%d %s", is.Number, is.Title))
	}
	if len(left) == 0 {
		return "Remaining: none"
	}
	return "Remaining: " + strings.Join(left, ", ")
}

// autoCheckpoint prints the latest merge and asks to go on. It reports
// whether approval was given; without it, st is saved for --continue.
func (r *Runner) autoCheckpoint(ctx context.Context, st *autoState, path string, approve approver, stdout io.Writer, how string) (bool, error) {
	var latest autoIssue
	for _, is := range st.Issues {
		if is.Status == "merged" {
			latest = is
		}
	}
	wait := time.Duration(0)
	if st.WaitForChecks {
		wait = autoChecksTimeout
		fmt.Fprintln(stdout, "Waiting up to 5 minutes for checks on the merge commit...")
	}
	checks := r.checkSummary(ctx, latest.Commit, wait, autoChecksPoll)
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	left := st.remaining()
	fmt.Fprintf(stdout, "\namf auto checkpoint: %d %s merged.\n"+
		"  issue:   #%d %s (%s)\n  PR:      %s\n  commit:  %s\n  agent:   %s\n  checks:  %s\n  %s\n\n",
		st.merged(), plural(st.merged(), "pull request is", "pull requests are"),
		latest.Number, latest.Title, latest.URL, latest.PR, latest.Commit, latest.Agent, checks, remainingLine(st))
	r.event(slog.LevelInfo, "auto.checkpoint", latest.Number, fmt.Sprintf("checkpoint after #%d: waiting for approval to process %d more", latest.Number, len(left)),
		"pr", latest.PR, "commit", latest.Commit, "agent", latest.Agent, "checks", checks, "remaining", len(left))
	if err := saveAutoState(path, st); err != nil {
		fmt.Fprintf(stdout, "amf: could not save %s: %v\n", path, err)
		return false, err
	}
	asked, yes := approve(fmt.Sprintf("Process the remaining %d %s, merging each PR? [y/N] ", len(left), plural(len(left), "issue", "issues")))
	if yes {
		st.Approved = true
		if err := saveAutoState(path, st); err != nil {
			fmt.Fprintf(stdout, "amf: could not save %s: %v\n", path, err)
			return false, err
		}
		if !asked && how == "prompt" {
			how = "flag"
		}
		message := "approved: processing the remaining issues"
		if how == "continue" {
			message = "approved with --continue: processing the remaining issues"
		}
		r.event(slog.LevelInfo, "auto.approved", 0, message, "how", how)
		return true, nil
	}
	if asked {
		r.event(slog.LevelInfo, "auto.declined", 0, "not approved: stopping", "remaining", len(left))
		fmt.Fprintf(stdout, "Stopped. The remaining issues stay labeled %s only, unprocessed.\n"+
			"To approve and resume later, run: amf auto --continue\n", labelAuto)
		return false, nil
	}
	fmt.Fprintf(stdout, "Waiting for approval (no terminal to ask on). State is saved in %s.\n"+
		"Review the merge above, then run: amf auto --continue\n", shownPath(r.Root, path))
	return false, nil
}

// checkSummary describes the check runs on commit, or why it can't.
func (r *Runner) checkSummary(ctx context.Context, commit string, wait, poll time.Duration) string {
	if commit == "" {
		return "unavailable (no merge commit)"
	}
	if wait > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, wait)
		defer cancel()
	}
	state, last, err := r.GH.waitChecks(ctx, commit, wait > 0, poll, 0)
	switch {
	case state == "timeout":
		return fmt.Sprintf("timed out waiting for checks (%s)", last)
	case err != nil:
		return fmt.Sprintf("unavailable (%v)", err)
	case wait == 0:
		return last
	}
	return state + ": " + last
}

// waitChecks reads the check runs on commit and, with wait, polls every
// poll until they all pass or one fails, or ctx ends. It returns
// checksState's state ("timeout" when ctx ended first) and the runs'
// summary. With noneGrace above zero, a commit that still has no check
// runs after that long counts as "none" rather than waiting on: the repo
// may have no CI.
func (g *GitHub) waitChecks(ctx context.Context, commit string, wait bool, poll, noneGrace time.Duration) (string, string, error) {
	start := time.Now()
	last := "none reported yet"
	for {
		runs, err := g.CheckRuns(ctx, commit)
		if ctx.Err() != nil {
			return "timeout", last, ctx.Err()
		}
		if err != nil {
			return "", last, err
		}
		last = summarizeCheckRuns(runs)
		state := checksState(runs)
		if !wait || state == "passed" || state == "failed" ||
			state == "none" && noneGrace > 0 && time.Since(start) >= noneGrace {
			return state, last, nil
		}
		select {
		case <-ctx.Done():
			return "timeout", last, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// checksState is "none" when no check has reported, "pending" while any is
// still running, else "passed" when every one succeeded (or was neutral or
// skipped) and "failed" otherwise.
func checksState(runs []checkRun) string {
	if len(runs) == 0 {
		return "none"
	}
	passed := true
	for _, c := range runs {
		if c.Status != "completed" {
			return "pending"
		}
		if c.Conclusion != "success" && c.Conclusion != "neutral" && c.Conclusion != "skipped" {
			passed = false
		}
	}
	if passed {
		return "passed"
	}
	return "failed"
}

func summarizeCheckRuns(runs []checkRun) string {
	if len(runs) == 0 {
		return "none reported yet"
	}
	counts := map[string]int{}
	var order []string
	for _, c := range runs {
		k := c.Conclusion
		if c.Status != "completed" {
			k = c.Status
		}
		if counts[k] == 0 {
			order = append(order, k)
		}
		counts[k]++
	}
	var parts []string
	for _, k := range order {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	return fmt.Sprintf("%d %s: %s", len(runs), plural(len(runs), "check", "checks"), strings.Join(parts, ", "))
}

type autoFlags struct {
	timeout         *time.Duration
	alias           *time.Duration
	evalRounds      *int
	agent           *string
	maxIssues       *int
	merge           *mergeFlag
	cont            *bool
	noCheckpoint    *bool
	checkpointAfter *int
	approve         *bool
	waitForChecks   *bool
	abort           *bool
	closeIssues     *bool
	dryRun          *bool
	log             *logOptions
	memory          *memoryOptions
}

func addAutoFlags(fs *flag.FlagSet) autoFlags {
	var f autoFlags
	f.timeout, f.alias = addTimeoutFlags(fs)
	f.evalRounds = fs.Int("eval-rounds", 2, "how many times a pipeline's eval stage may send the change back to code")
	f.agent = fs.String("agent", "", agentFlagHelp)
	f.maxIssues = fs.Int("max-issues", defaultAutoMaxIssues, "plan at most this many issues (1 to 10)")
	f.merge = &mergeFlag{value: "merge"}
	fs.Var(f.merge, "merge", "how to merge each PR: a merge commit, or a method after = or as the next word: --merge=squash, -m squash, -m rebase")
	f.cont = fs.Bool("continue", false, "approve the checkpoint and resume the saved amf auto run")
	f.noCheckpoint = fs.Bool("no-checkpoint", false, "merge all issues without a checkpoint or approval")
	f.checkpointAfter = fs.Int("checkpoint-after", 1, "checkpoint after this many merges (must be positive)")
	f.approve = fs.Bool("approve", false, "approve the checkpoint without prompting")
	f.waitForChecks = fs.Bool("wait-for-checks", false, "at the checkpoint, wait up to 5 minutes for checks on the merge commit (each PR merge already waits up to 30 minutes for head checks)")
	f.abort = fs.Bool("abort", false, "abandon the saved run by removing its state")
	f.closeIssues = fs.Bool("close-issues", false, "with --abort, close the created, unmerged issues first")
	f.dryRun = fs.Bool("dry-run", false, "plan only: print the issues amf would create, and create nothing")
	f.log = addLogFlags(fs)
	f.memory = addMemoryFlags(fs)
	return f
}

func cmdAuto(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	status := len(args) > 0 && args[0] == "status"
	if status {
		args = args[1:]
	}
	fs := flag.NewFlagSet("auto", flag.ContinueOnError)
	fs.SetOutput(stderr)
	withHelp(fs, stderr, "auto")
	f := addAutoFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if *f.closeIssues && !*f.abort {
		return errors.New("--close-issues requires --abort")
	}
	if status || *f.abort {
		mode := "status"
		if !status {
			mode = "--abort"
		}
		if len(pos) > 0 {
			return fmt.Errorf("amf auto %s takes no sentence", mode)
		}
		fs.Visit(func(f *flag.Flag) {
			if !status && (f.Name == "abort" || f.Name == "close-issues") {
				return
			}
			err = fmt.Errorf("%s and --%s can't be used together", mode, f.Name)
		})
		if err != nil {
			return err
		}
		return cmdAutoState(ctx, status, *f.closeIssues, stdout)
	}
	if err := f.merge.check(pos); err != nil {
		return err
	}
	if f.merge.value == "" {
		return errors.New("amf auto always merges: use --merge, --merge=squash or --merge=rebase")
	}
	sentence := strings.Join(strings.Fields(strings.Join(pos, " ")), " ")
	switch {
	case *f.cont && sentence != "":
		return errors.New("--continue resumes the saved run; it takes no sentence")
	case *f.cont && *f.dryRun:
		return errors.New("--continue and --dry-run can't be used together")
	case !*f.cont && sentence == "":
		fmt.Fprintln(stderr, `usage: amf auto "<one simple sentence>" [flags] | amf auto --continue | amf auto status | amf auto --abort [--close-issues] (amf auto --help lists the flags)`)
		return errUsage
	}
	if *f.maxIssues < 1 || *f.maxIssues > 10 {
		return errors.New("--max-issues must be between 1 and 10")
	}
	if *f.checkpointAfter < 1 {
		return errors.New("--checkpoint-after must be at least 1")
	}
	// Zero means preserve the saved threshold when continuing.
	checkpointSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "checkpoint-after" {
			checkpointSet = true
		}
	})
	if *f.cont && !checkpointSet {
		*f.checkpointAfter = 0
	}
	timeout, err := timeoutFlag(fs, f.timeout, f.alias)
	if err != nil {
		return err
	}
	if err := f.log.check(); err != nil {
		return err
	}
	if err := f.memory.check(); err != nil {
		return err
	}
	r, err := newRunner(ctx, timeout, *f.evalRounds, *f.agent, *f.log, stderr)
	if err != nil {
		return err
	}
	defer r.Close()
	// auto retries its own issues and updates their open PRs itself.
	r.Force = true
	if err := r.lockRepo("auto"); err != nil {
		return err
	}
	r.Command = "auto"
	if !*f.dryRun {
		r.openMemory(*f.memory)
	}
	err = redactErr(r.Redact, r.Auto(ctx, sentence, autoOptions{
		Continue: *f.cont, DryRun: *f.dryRun, MaxIssues: *f.maxIssues, Merge: f.merge.value,
		NoCheckpoint: *f.noCheckpoint, CheckpointAfter: *f.checkpointAfter, Approve: *f.approve, WaitForChecks: *f.waitForChecks,
	}, terminalApprover(stdin, stdout), stdout))
	r.DB.logError(err)
	return err
}

// cmdAutoState avoids runner setup: inspection and plain abort need neither
// GitHub nor agents, and must not clone a repo or create logs.
func cmdAutoState(ctx context.Context, status, closeIssues bool, stdout io.Writer) error {
	root, err := git(ctx, ".", "rev-parse", "--show-toplevel")
	if err != nil || paths.CloneDir != "" {
		// Outside a checkout (or with --clone-dir), auto uses the clone
		// dir. Locate that state without invoking checkoutRoot, which
		// could create a new clone.
		cfg, err := loadConfig(envPaths(ctx)...)
		if err != nil {
			return err
		}
		if root, err = cloneDirFor(cfg.Repo); err != nil {
			return err
		}
	}
	path := autoStatePath(root)
	if !status && !closeIssues {
		// Plain abort also recovers from corrupt or unsupported state.
		err = os.Remove(path)
	} else {
		var st *autoState
		st, err = loadAutoState(path)
		if err == nil {
			if status {
				fmt.Fprintf(stdout, "Sentence: %s\nMerge method: %s\n", st.Sentence, st.Merge)
				for _, is := range st.Issues {
					number := "not created"
					if is.Number != 0 {
						number = fmt.Sprintf("#%d", is.Number)
					}
					fmt.Fprintf(stdout, "%s %s: %s\n  PR: %s\n  commit: %s\n", number, is.Title, is.Status, is.PR, is.Commit)
				}
				return nil
			}
			cfg, err := loadConfig(envPaths(ctx)...)
			if err != nil {
				return err
			}
			if st.Repo != cfg.Repo {
				return fmt.Errorf("cannot close amf auto issues: state's repo %q differs from GITHUB_REPO %q", st.Repo, cfg.Repo)
			}
			gh := newGitHub(cfg)
			for _, is := range st.Issues {
				if is.Number == 0 || is.Status == "merged" {
					continue
				}
				if err := gh.CloseIssue(ctx, is.Number); err != nil {
					return redactErr(configRedactor(cfg), fmt.Errorf("closing issue #%d (state kept; retry amf auto --abort --close-issues): %w", is.Number, err))
				}
			}
			err = os.Remove(path)
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stdout, "No amf auto run saved.")
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "Aborted amf auto run; saved state removed.")
	return nil
}

// autoOptions are amf auto's flags that Auto needs.
type autoOptions struct {
	Continue, DryRun                     bool
	MaxIssues                            int
	Merge                                string
	NoCheckpoint, Approve, WaitForChecks bool
	CheckpointAfter                      int
}

// Auto is amf auto: plan sentence into issues and work through them, or
// with Continue resume the saved run, approving its checkpoint.
func (r *Runner) Auto(ctx context.Context, sentence string, o autoOptions, approve approver, stdout io.Writer) error {
	ctx = withEvents(ctx, r.Events, 0)
	if o.Approve {
		approve = func(string) (bool, bool) { return false, true }
	}
	path := autoStatePath(r.Root)
	saved, lerr := loadAutoState(path)
	if lerr != nil && !errors.Is(lerr, os.ErrNotExist) {
		return lerr
	}
	if o.Continue {
		if saved == nil {
			return fmt.Errorf("no amf auto run to continue (%s doesn't exist)", shownPath(r.Root, path))
		}
		if saved.Repo != r.Repo {
			return fmt.Errorf("cannot continue amf auto: state's repo %q differs from GITHUB_REPO %q", saved.Repo, r.Repo)
		}
		r.Merge = saved.Merge
		saved.NoCheckpoint = saved.NoCheckpoint || o.NoCheckpoint
		saved.WaitForChecks = saved.WaitForChecks || o.WaitForChecks
		if o.CheckpointAfter > 0 {
			saved.CheckpointAfter = o.CheckpointAfter
		}
		if saved.checkpointDue() {
			if _, err := r.autoCheckpoint(ctx, saved, path, func(string) (bool, bool) { return false, true }, stdout, "continue"); err != nil {
				return err
			}
		}
		r.event(slog.LevelInfo, "auto.resume", 0, fmt.Sprintf("resuming amf auto: %q, %s", saved.Sentence, remainingLine(saved)),
			"sentence", saved.Sentence, "remaining", len(saved.remaining()))
		if err := saveAutoState(path, saved); err != nil {
			return err
		}
		if err := r.autoCreateIssues(ctx, saved, path, stdout); err != nil {
			return err
		}
		return r.autoWork(ctx, saved, path, approve, stdout)
	}
	if saved != nil && !o.DryRun {
		return fmt.Errorf("an amf auto run is already in progress (%q, %s); run amf auto --continue to resume it, or amf auto --abort to start over",
			saved.Sentence, remainingLine(saved))
	}
	r.Merge = o.Merge
	issues, err := r.autoPlan(ctx, sentence, o.MaxIssues)
	if err != nil {
		return err
	}
	if o.DryRun {
		fmt.Fprintf(stdout, "amf auto --dry-run: %s planned %d %s (none created):\n", r.lastAgent, len(issues), plural(len(issues), "issue", "issues"))
		for i, p := range issues {
			fmt.Fprintf(stdout, "\n%d. %s\n\n%s", i+1, p.Title, autoIssueBody(p, sentence, i+1, len(issues)))
		}
		return nil
	}
	st := &autoState{Version: autoStateVersion, Sentence: sentence, Repo: r.Repo, Merge: r.Merge, Created: time.Now().UTC()}
	st.NoCheckpoint, st.CheckpointAfter, st.WaitForChecks = o.NoCheckpoint, o.CheckpointAfter, o.WaitForChecks
	for _, p := range issues {
		st.Issues = append(st.Issues, autoIssue{Title: p.Title, Scope: p.Scope, Acceptance: p.Acceptance, Status: "pending"})
	}
	if err := saveAutoState(path, st); err != nil {
		return err
	}
	if err := r.autoCreateIssues(ctx, st, path, stdout); err != nil {
		return err
	}
	var numbers []int
	for _, is := range st.Issues {
		numbers = append(numbers, is.Number)
	}
	r.event(slog.LevelInfo, "auto.plan", 0, fmt.Sprintf("planned %d %s with %s: %v", len(numbers), plural(len(numbers), "issue", "issues"), r.lastAgent, numbers),
		"action", "created", "issues", numbers, "agent", r.lastAgent, "sentence", sentence, "merge", r.Merge)
	return r.autoWork(ctx, st, path, approve, stdout)
}
