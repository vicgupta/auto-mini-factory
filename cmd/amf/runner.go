package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// An issue moves amf:ready -> amf:in-progress -> amf:pr-open (a pull
// request is open, not merged), amf:done (amf merged it) or amf:failed.
// Adding amf:ready again asks amf to retry it.
const (
	labelReady      = "amf:ready"
	labelInProgress = "amf:in-progress"
	labelDone       = "amf:done"
	labelFailed     = "amf:failed"
	labelPROpen     = "amf:pr-open"
)

var statusLabels = []string{labelInProgress, labelPROpen, labelDone, labelFailed}

// finalStatusTimeout bounds the status label cleanup after a PR exists, so a
// Ctrl-C can't leave the issue on both amf:in-progress and its real status.
var finalStatusTimeout = 10 * time.Second

// amfLabels are the labels amf uses, as `amf init` creates them.
var amfLabels = []LabelSpec{
	{labelReady, "0e8a16", "Ready for amf to turn into a pull request"},
	{labelInProgress, "fbca04", "amf is working on this issue"},
	{labelPROpen, "c5def5", "amf opened a pull request for this issue; it is not merged yet"},
	{labelDone, "1d76db", "amf's pull request for this issue was merged"},
	{labelFailed, "d93f0b", "amf could not process this issue; see its comment"},
}

// labelSpec is amf's spec of label name, from amfLabels.
func labelSpec(name string) LabelSpec {
	for _, l := range amfLabels {
		if l.Name == name {
			return l
		}
	}
	return LabelSpec{Name: name}
}

// Runner turns issues into pull requests from a local checkout of the repo.
type Runner struct {
	GH       *GitHub
	Repo     string // owner/name, for the prompt
	Root     string // top level of the local checkout
	Claude   string // claude binary
	Codex    string // codex binary
	Opencode string // opencode binary
	// Agent is the agent for issues without a pipeline (claudecode, codex or
	// opencode), from --agent. "" means amf picks: see defaultAgent.
	Agent string
	// Primary is the agent amf runs first for an issue without a pipeline,
	// and Secondary the one it falls back to when the primary fails or isn't
	// usable, from AMF_PRIMARY_AGENT and AMF_SECONDARY_AGENT. --agent
	// overrides both. Secondary "" means no fallback; Primary "" (as in
	// tests) means amf picks as it did before them: see agentPlan.
	Primary, Secondary string
	// fellBack, set by build, says which agent made the change after which
	// failed, for the PR body and the issue's comment; "" without a fallback.
	fellBack string
	// ClaudeToken, when set, is passed to Claude as CLAUDE_CODE_OAUTH_TOKEN.
	ClaudeToken string
	// AnthropicKey, when set (and ClaudeToken isn't), is passed to Claude as
	// ANTHROPIC_API_KEY.
	AnthropicKey string
	// OpenAIKey, when set, is passed to Codex as OPENAI_API_KEY.
	OpenAIKey string
	// OpencodeKey, when set, is passed to opencode as OPENCODE_API_KEY.
	OpencodeKey string
	// OpencodeModel is the model opencode runs with, from OPENCODE_MODEL:
	// always without a pipeline, and for pipeline stages that name none.
	OpencodeModel string
	// Trusted, when set, limits amf to issues opened by these lower-case
	// logins: agents run an issue's text with every permission check off.
	Trusted []string
	Timeout time.Duration // per agent run
	// IdleTimeout is how long an agent may produce no output before amf
	// stops it. 0 disables it.
	IdleTimeout time.Duration
	// KillGrace is how long an agent gets to exit after SIGTERM before amf
	// SIGKILLs its process group, in tests small. 0 means agentKillGrace.
	KillGrace time.Duration
	// EvalRounds is how many times a pipeline's eval stage may send a change
	// back to the code stage.
	EvalRounds int
	// Draft opens new pull requests as drafts, from --draft. A retry that
	// updates an open PR leaves it a draft or ready for review as it was.
	Draft bool
	// ExcludeLabels, from watch's --exclude-label, makes watch skip issues
	// with any of these labels, leaving them as they are.
	ExcludeLabels []string
	// Merge, from --merge, is how amf merges each PR it opens or updates:
	// merge, squash or rebase. "" leaves the PR for review.
	Merge string
	// DryRun, from run's --dry-run, makes the change on the local branch
	// only: no label, comment, push, PR or merge. See dryRun.
	DryRun bool
	// MergeRetry is how long amf waits before asking GitHub again to merge a
	// PR it reports as not mergeable yet, in tests small. 0 means mergeRetry.
	MergeRetry time.Duration
	// MergeSkipChecks merges without waiting for the PR head's checks
	// (--wait-for-checks=false). By default a merge waits up to
	// ChecksTimeout for them to pass, polling every ChecksPoll, and treats a
	// head with no checks after NoChecksGrace as a repo without CI; zero
	// values mean prChecksTimeout, prChecksPoll and prNoChecksGrace.
	MergeSkipChecks                          bool
	ChecksTimeout, ChecksPoll, NoChecksGrace time.Duration
	HTTP                                     *http.Client // for downloading skills; nil means a default client
	Log                                      io.Writer    // progress output
	// DB, when set, also gets every agent's output; Log already sends it
	// amf's own lines.
	DB *dbLog
	// Redact masks secrets and personal data in the agents' logs, the
	// comments and the PR body. newRunner also redacts Log with it. Nil
	// changes nothing.
	Redact *redactor
	// Events, when set, is where amf's events go: to Log as before and, as
	// JSON, to the run's event file, which Close flushes and closes.
	Events *slog.Logger
	events *eventFile
	// Quiet hides terminal progress but keeps diagnostics and the final summary.
	Quiet bool
	// checkout is how newRunner found Root: checkout (the enclosing one),
	// clone (reused) or cloned.
	checkout string
	// stats counts the events for the run summary, which goes to
	// summaryPath ("" for none) after each issue and at Close.
	stats       *statsHandler
	summaryPath string
	// lastPR, lastCommit and lastAgent describe the latest Run: the PR it
	// opened or updated, the merge commit (with --merge) and the agent that
	// made the change. amf auto reports them at its checkpoint.
	lastPR     PullRequest
	lastCommit string
	lastAgent  string

	// Force, from run's --force (and set by amf auto), skips the
	// pre-start guard: amf runs the issue even though it already has an
	// open PR or an amf/issue-N branch, and updates that PR.
	Force bool
	// backupNote tells the issue comment where process saved the commits
	// a force push replaced, "" when it saved none.
	backupNote string
	// StaleTimeout is how long an issue may stay amf:in-progress, with no
	// amf process working on it, before watch puts it back to amf:ready
	// (see sweepStale). 0 turns the sweep off.
	StaleTimeout time.Duration
	// working is the issue this process is running now, 0 for none.
	working int
	// attempts are the agent runs of the latest build without a pipeline,
	// in order, and noFallback why the fallback agent didn't run ("" when
	// it did, or the issue had a pipeline). The failure comment lists them.
	attempts   []agentAttempt
	noFallback string
	// lock is the repo's .amf/watch.lock while this process holds it.
	lock *repoLock
	// Memory, when set, is the repo's memory: Run records each attempt in
	// it and looks up similar past work. Its failures are only warnings.
	Memory MemoryStore
	// Command is run, watch or auto, for the runs memory records.
	Command string
}

// Close writes out the event file and sends the logs still queued for
// PostgreSQL.
func (r *Runner) Close() {
	r.summarize(true)
	r.releaseLock()
	if err := r.events.Close(); err != nil {
		fmt.Fprintf(r.Log, "amf: could not write the event log: %v\n", err)
	}
	r.DB.Close(15 * time.Second)
	if r.Memory != nil {
		r.memWarn(0, "close", r.Memory.Close())
	}
}

// Run processes one issue end to end. On failure the issue is labeled
// amf:failed and the reason is commented. A run that timed out leaves no PR
// and pushes nothing: it removes the worktree, keeping the logs, the partial
// output and the unpushed local branch, which a retry resets; any other
// failure keeps the worktree for inspection.
func (r *Runner) Run(ctx context.Context, n int) (err error) {
	defer r.summarize(false)           // runs last, after the deferred failure handling
	ctx = withEvents(ctx, r.Events, n) // git, GitHub and skill calls log there
	r.lastPR, r.lastCommit, r.lastAgent = PullRequest{}, "", ""
	r.attempts, r.noFallback = nil, ""
	r.working = n
	defer func() { r.working = 0 }()
	issue, err := r.GH.GetIssue(ctx, n)
	if err != nil {
		return err
	}
	if issue.PullRequest != nil {
		return fmt.Errorf("#%d is a pull request, not an issue", n)
	}
	if issue.State != "open" {
		return fmt.Errorf("#%d is %s", n, issue.State)
	}

	// Problems with the repo's pipeline or this machine are found before the
	// issue is touched.
	base, err := r.GH.DefaultBranch(ctx)
	if err != nil {
		return err
	}
	pipeline, err := r.loadPipeline(ctx, base)
	if err != nil {
		return err
	}
	var agent runPlan
	if pipeline == nil {
		if agent, err = r.agentPlan(); err != nil {
			return err
		}
	} else if r.Agent != "" {
		r.event(slog.LevelInfo, "issue.agent_ignored", n, fmt.Sprintf("#%d: ignoring --agent %s: AGENTS.md on origin/%s declares a pipeline, which names each stage's agent", n, r.Agent, base),
			"agent", agentName(r.Agent))
	}
	if _, err := gitIdentity(ctx, r.Root); err != nil {
		return err
	}

	r.event(slog.LevelInfo, "issue.picked", n, fmt.Sprintf("#%d: %s", n, issue.Title),
		"title", issue.Title, "author", issue.User.Login, "labels", labelNames(issue))
	if r.DryRun {
		return r.dryRun(ctx, issue, base, pipeline, agent)
	}
	if err := r.guard(ctx, issue, base); err != nil {
		return err
	}
	wasDone := issue.HasLabel(labelDone)
	if r.Memory != nil {
		r.memWarn(n, "record the issue", r.Memory.UpsertIssue(memIssueFrom(issue)))
		if note := r.memTriage(n, issue.Title, issue.Body); note != "" {
			issue.Body = strings.TrimSpace(issue.Body) + "\n\n---\n" + note
		}
		started := time.Now()
		defer func() { r.memRecordRun(n, started, err) }()
	}
	if err := r.setStatus(ctx, n, labelInProgress); err != nil {
		return err
	}
	r.statusEvent(n, statusOf(issue), labelInProgress)
	// The issue is no longer waiting; adding amf:ready again retries it.
	if err := r.GH.RemoveLabel(ctx, n, labelReady); err != nil {
		return err
	}
	defer func() {
		// A PR amf couldn't merge, or whose final label couldn't be set, is
		// still open: the issue is amf:pr-open.
		if err == nil || errors.As(err, new(*mergeError)) || errors.As(err, new(*statusError)) {
			return
		}
		// Report even if ctx was cancelled (e.g. Ctrl-C), but don't hang on it.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		// An issue that has an open PR, or whose PR was merged, keeps that
		// status: a failed retry doesn't undo the earlier PR.
		final := labelFailed
		if _, open, ferr := r.GH.OpenPR(cctx, issueBranch(n)); ferr == nil && open {
			final = labelPROpen
		} else if wasDone {
			// Keep amf:done only for a PR GitHub confirms is merged.
			if _, merged, merr := r.GH.MergedPR(cctx, issueBranch(n)); merr == nil && merged {
				final = labelDone
			}
		}
		if serr := r.setStatus(cctx, n, final); serr != nil {
			r.event(slog.LevelWarn, "issue.status", n, fmt.Sprintf("#%d: could not set %s: %v", n, final, serr), "to", final, "error", serr)
		} else {
			r.statusEvent(n, labelInProgress, final)
		}
		msg := fmt.Sprintf("amf could not turn this issue into a pull request:\n\n```\n%s\n```", err)
		var se *stageError
		if errors.As(err, &se) {
			msg = se.comment
		}
		var ate *agentTimeoutError
		if errors.As(err, &ate) {
			msg = ate.comment(r.Root)
			r.event(slog.LevelInfo, "issue.failed", n, fmt.Sprintf("issue.failed: issue=%d reason=agent_timeout", n),
				"reason", "agent_timeout", "stage", stageName(ate.Stage), "error", err)
			// The agent's process group is dead, so nothing still writes
			// into the worktree, and its partial work is never pushed.
			r.removeWorktree(cctx, n)
		}
		if ate == nil {
			reason := "error"
			if se != nil {
				reason = "stage"
			}
			r.event(slog.LevelDebug, "issue.failed", n, fmt.Sprintf("#%d: failed: %v", n, err), "reason", reason, "error", err)
		}
		if se == nil {
			msg += r.attemptReport()
		}
		if final != labelFailed {
			msg += fmt.Sprintf("\n\nThe issue keeps its `%s` label: its earlier pull request is unaffected.", final)
		}
		r.comment(cctx, n, msg)
	}()

	trusted := r.checkAuthor(issue)
	r.event(slog.LevelDebug, "issue.author_check", n, fmt.Sprintf("#%d: opened by @%s", n, issue.User.Login),
		"author", issue.User.Login, "trusted", trusted == nil)
	if trusted != nil {
		return trusted
	}

	pr, updated, err := r.refreshStale(ctx, issue, base, pipeline, agent)
	if err != nil {
		return err
	}
	if !updated {
		if pr, updated, err = r.process(ctx, issue, base, pipeline, agent); err != nil {
			return err
		}
	}

	r.lastPR = pr
	// The PR exists, so the issue is pr-open (or done) even if GitHub fails
	// from here on.
	verb := "opened"
	if updated {
		verb = "updated"
	}
	r.event(slog.LevelInfo, "issue.pr", n, fmt.Sprintf("#%d: %s %s", n, verb, pr.HTMLURL),
		"action", verb, "number", pr.Number, "url", pr.HTMLURL)
	msg := fmt.Sprintf("amf %s %s for this issue.", verb, pr.HTMLURL)
	if r.fellBack != "" {
		msg += " " + r.fellBack
	}
	var merr error
	if r.Merge != "" {
		if merr = r.mergePR(ctx, n, base, pr); merr != nil {
			msg = fmt.Sprintf("amf %s %s for this issue, but could not merge it:\n\n```\n%s\n```\n\n"+
				"The pull request is still open for you to review and merge.", verb, pr.HTMLURL, merr)
		} else {
			msg = fmt.Sprintf("amf %s %s for this issue and %s it.", verb, pr.HTMLURL, mergedVerb[r.Merge])
		}
	}
	// amf:done only once the PR is merged; an open one is amf:pr-open.
	status := labelPROpen
	if r.Merge != "" && merr == nil && r.confirmMerged(ctx, pr.Number) {
		status = labelDone
	}
	// The PR exists now, so finish its status even when watch is stopping.
	// WithoutCancel keeps a Ctrl-C from leaving both the old and the new
	// status label behind, and the timeout keeps shutdown bounded.
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalStatusTimeout)
	serr := r.setStatus(sctx, n, status)
	cancel()
	if serr != nil {
		r.event(slog.LevelWarn, "issue.status", n, fmt.Sprintf("#%d: could not set %s: %v", n, status, serr), "to", status, "error", serr)
	} else {
		r.statusEvent(n, labelInProgress, status)
	}
	if r.backupNote != "" {
		msg += "\n\n" + r.backupNote
	}
	r.comment(ctx, n, msg)
	r.event(slog.LevelDebug, "issue.done", n, fmt.Sprintf("#%d: done", n), "pr", pr.Number)
	if merr != nil {
		return &mergeError{url: pr.HTMLURL, err: merr}
	}
	if serr != nil {
		return &statusError{url: pr.HTMLURL, label: status, err: serr}
	}
	return nil
}

// statusError is a PR amf opened or updated but whose final issue status label
// could not be set. The PR is real, so the issue never becomes amf:failed.
type statusError struct {
	url   string
	label string
	err   error
}

func (e *statusError) Error() string {
	return fmt.Sprintf("opened %s but could not set %s: %v", e.url, e.label, e.err)
}
func (e *statusError) Unwrap() error { return e.err }

// comment posts msg, redacted, on issue n, logging how it went.
func (r *Runner) comment(ctx context.Context, n int, msg string) {
	msg = r.Redact.Redact(msg)
	if err := r.GH.Comment(ctx, n, msg); err != nil {
		r.event(slog.LevelWarn, "issue.comment", n, fmt.Sprintf("#%d: could not comment: %v", n, err), "error", err)
		return
	}
	r.event(slog.LevelDebug, "issue.comment", n, fmt.Sprintf("#%d: commented", n), "bytes", len(msg))
}

// statusEvent records that issue n's status label went from one to another.
func (r *Runner) statusEvent(n int, from, to string) {
	r.event(slog.LevelDebug, "issue.status", n, fmt.Sprintf("#%d: %s -> %s", n, from, to), "from", from, "to", to)
}

// checkAuthor exempts amf:auto issues and fails other authors not in Trusted.
// It runs after Claim, so the issue is marked failed once and watch doesn't retry it.
func (r *Runner) checkAuthor(issue Issue) error {
	if issue.HasLabel(labelAuto) || r.Trusted == nil || slices.Contains(r.Trusted, strings.ToLower(issue.User.Login)) {
		return nil
	}
	who := "@" + issue.User.Login
	if issue.User.Login == "" {
		who = "an unknown author"
	}
	return &stageError{
		msg: fmt.Sprintf("#%d was opened by %s, who is not in AMF_TRUSTED", issue.Number, who),
		comment: fmt.Sprintf("amf only works on issues opened by the GitHub users in its `AMF_TRUSTED` setting, "+
			"and this one was opened by %s, so amf didn't run it. To have amf work on it, open it again from a "+
			"trusted account, or add the author to `AMF_TRUSTED` and add the `%s` label again.", who, labelReady),
	}
}

// process turns the issue into a PR: with a single run of agent, or with the
// repo's pipeline when its AGENTS.md declares one. updated is true when the
// branch already had an open PR, which now has the new title and body.
func (r *Runner) process(ctx context.Context, issue Issue, base string, p *Pipeline, agent runPlan) (pr PullRequest, updated bool, err error) {
	n := issue.Number
	branch, wt := issueBranch(n), r.worktreePath(n)
	summary, extra, err := r.build(ctx, issue, base, p, agent)
	if err != nil {
		return PullRequest{}, false, err
	}

	r.event(slog.LevelInfo, "issue.push", n, fmt.Sprintf("#%d: pushing %s", n, branch), "branch", branch)
	// amf owns amf/issue-* branches, so a re-run may replace an earlier push.
	// Any commits on it that the new push would drop are saved first.
	r.backupNote = ""
	old, err := remoteTip(ctx, wt, branch)
	if err != nil {
		return PullRequest{}, false, err
	}
	if err := r.backupBranch(ctx, wt, n, branch, base, old); err != nil {
		return PullRequest{}, false, err
	}
	args, err := pushArgs(ctx, wt, branch, old)
	if err != nil {
		return PullRequest{}, false, err
	}
	if _, err := git(ctx, wt, args...); err != nil {
		return PullRequest{}, false, err
	}
	head, err := git(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		return PullRequest{}, false, err
	}

	title, body := prTitle(issue), r.prBody(summary, extra, n)
	pr, err = r.GH.CreatePR(ctx, title, branch, base, body, r.Draft)
	if isStatus(err, http.StatusUnprocessableEntity) {
		// A retry of an issue whose PR is still open: the push updated it,
		// so bring its description up to date too. It stays a draft or
		// ready for review as it was, whatever --draft says.
		if open, ok, ferr := r.GH.OpenPR(ctx, branch); ferr == nil && ok {
			r.event(slog.LevelInfo, "issue.pr", n, fmt.Sprintf("#%d: %s already has %s; updating it", n, branch, open.HTMLURL),
				"action", "updating", "number", open.Number, "url", open.HTMLURL)
			pr, err = r.GH.UpdatePR(ctx, open.Number, title, body)
			updated = true
		}
	}
	if err != nil {
		return PullRequest{}, false, err
	}
	pr.Head = head

	if _, err := git(ctx, r.Root, "worktree", "remove", wt); err != nil {
		r.event(slog.LevelWarn, "issue.cleanup", n, fmt.Sprintf("#%d: could not remove worktree: %v", n, err), "error", err)
	}
	return pr, updated, nil
}

// issueBranch is the branch amf works on for issue n.
func issueBranch(n int) string { return fmt.Sprintf("amf/issue-%d", n) }

// worktreePath is where issue n's worktree goes.
func (r *Runner) worktreePath(n int) string {
	return filepath.Join(r.Root, ".amf", "worktrees", fmt.Sprintf("issue-%d", n))
}

// prTitle is the title of the PR, and of the commit, for an issue.
func prTitle(issue Issue) string { return fmt.Sprintf("Fix #%d: %s", issue.Number, issue.Title) }

// prBody is the redacted PR description: the agent's summary, cleaned, then
// the pipeline's sections and amf's footer.
func (r *Runner) prBody(summary, extra string, n int) string {
	summary = cleanSummary(summary, n)
	if summary == "" {
		summary = fmt.Sprintf("Resolves #%d.", n)
	}
	if extra != "" {
		summary += "\n\n" + extra
	}
	return r.Redact.Redact(fmt.Sprintf("%s\n\nCloses #%d\n\n---\n_Opened by amf._", summary, n))
}

// build makes the change on the issue's branch in a fresh worktree, with
// the agents of plan (see buildOne) or with the pipeline p, and leaves it as
// one commit, unpushed. It returns the agent's summary and the pipeline's
// sections, or the fallback note, for the PR body.
func (r *Runner) build(ctx context.Context, issue Issue, base string, p *Pipeline, plan runPlan) (summary, extra string, err error) {
	n := issue.Number
	r.fellBack = ""
	if p == nil {
		return r.buildFallback(ctx, issue, base, plan)
	}
	r.lastAgent = "pipeline from AGENTS.md"
	branch, wt := issueBranch(n), r.worktreePath(n)

	r.event(slog.LevelInfo, "issue.worktree", n, fmt.Sprintf("#%d: creating worktree %s on %s from origin/%s", n, wt, branch, base),
		"path", wt, "branch", branch, "base", base)
	if err := r.prepareWorktree(ctx, wt, branch, base); err != nil {
		return "", "", err
	}

	var stages, agents, models []string
	for _, s := range p.stages() {
		stages, agents, models = append(stages, s.Name), append(agents, s.Agent), append(models, s.model())
	}
	r.event(slog.LevelInfo, "issue.pipeline", n, fmt.Sprintf("#%d: running the pipeline from AGENTS.md: %s", n, describePipeline(p)),
		"declared", true, "stages", stages, "agents", agents, "models", models)
	return r.runPipeline(ctx, wt, issue, branch, base, p)
}

// buildOne makes the change with a single run of agent in a fresh worktree
// reset to origin/base, and commits it. log is the agent's log file.
//
// exit is how the agent's run ended, for the failure comment: "0", the
// exit code, "timed out", or "" when it didn't run or amf can't tell.
func (r *Runner) buildOne(ctx context.Context, issue Issue, base, agent, log string) (summary, exit string, err error) {
	n := issue.Number
	branch, wt := issueBranch(n), r.worktreePath(n)
	r.event(slog.LevelInfo, "issue.worktree", n, fmt.Sprintf("#%d: creating worktree %s on %s from origin/%s", n, wt, branch, base),
		"path", wt, "branch", branch, "base", base)
	if err := r.prepareWorktree(ctx, wt, branch, base); err != nil {
		return "", "", err
	}
	model := r.agentModel(agent, "")
	r.event(slog.LevelDebug, "issue.pipeline", n, fmt.Sprintf("#%d: no pipeline; one %s run", n, agentName(agent)),
		"declared", false, "stages", []string{"code"}, "agents", []string{agentName(agent)}, "models", []string{model})
	text := fmt.Sprintf("#%d: running %s (timeout %s)", n, agentName(agent), r.Timeout)
	if model != "" {
		text = fmt.Sprintf("#%d: running %s (%s, timeout %s)", n, agentName(agent), model, r.Timeout)
	}
	r.event(slog.LevelInfo, "issue.agent", n, text, "agent", agentName(agent), "model", model, "timeout", r.Timeout.String())
	out := filepath.Join(wt, ".amf", "code.md") // for agents that write their final message to a file
	if summary, err = r.runAgent(ctx, agent, "", model, wt, buildPrompt(r.Repo, branch, issue), log, out); err != nil {
		return "", exitOf(err), err
	}
	if err := commitAll(ctx, wt, prTitle(issue), n, base, agent); err != nil {
		return "", "0", err
	}
	return summary, "0", nil
}

// buildFallback is build without a pipeline: it runs the plan's agents in
// order until one makes a change. Any failure of an agent's run (not
// installed or usable, a non-zero exit, an error result, a timeout) or of
// its commit (no changes, a refused path) moves on to the next agent, which
// starts from a fresh worktree reset to origin/base, so nothing the failed
// one left behind is kept. Cancelling amf (ctx) stops at once. When the
// last agent fails too, the error names every failure.
func (r *Runner) buildFallback(ctx context.Context, issue Issue, base string, plan runPlan) (summary, extra string, err error) {
	n := issue.Number
	var failed []string // how each agent that ran failed, in order
	// An agent passed over as not usable is a fallback too, but one decided
	// before anything ran, so only the event records it, not the PR body.
	// Name every skipped agent, so the message says which one was tried
	// first and why it didn't run.
	if len(plan.Skipped) > 0 {
		reasons := make([]string, len(plan.Skipped))
		for i, s := range plan.Skipped {
			reasons[i] = s.Reason
		}
		r.fallbackEvent(n, plan.Skipped[0].Agent, plan.Agents[0], "unusable", strings.Join(reasons, "; "))
	}
	for i, agent := range plan.Agents {
		log := filepath.Join(r.Root, ".amf", "logs", fmt.Sprintf("issue-%d.log", n))
		if i > 0 { // keep the failed agent's log
			log = filepath.Join(r.Root, ".amf", "logs", fmt.Sprintf("issue-%d-%s.log", n, agentName(agent)))
		}
		start := time.Now()
		var exit string
		summary, exit, err = r.buildOne(ctx, issue, base, agent, log)
		r.attempts = append(r.attempts, agentAttempt{Agent: agentName(agent), Exit: exit, Duration: time.Since(start), Log: log, Err: err})
		if err == nil {
			r.lastAgent = agentName(agent)
			if len(failed) > 0 {
				r.fellBack = fmt.Sprintf("%s made the change, after amf fell back from %s.", agentName(agent), strings.Join(failed, ", then "))
				return summary, "_" + r.fellBack + "_", nil
			}
			return summary, "", nil
		}
		if ctx.Err() != nil {
			if i+1 < len(plan.Agents) {
				r.noFallback = "amf was stopped before it could fall back to " + agentName(plan.Agents[i+1])
			}
			return "", "", err
		}
		failed = append(failed, fmt.Sprintf("%s failed: %v", agentName(agent), err))
		if i+1 < len(plan.Agents) {
			reason := "error"
			if errors.As(err, new(*agentTimeoutError)) {
				reason = "agent_timeout"
			}
			r.fallbackEvent(n, agent, plan.Agents[i+1], reason, err.Error())
		}
	}
	if len(failed) == 1 {
		r.noFallback = plan.NoFallback
		return "", "", err
	}
	var ate *agentTimeoutError
	if errors.As(err, &ate) {
		ate.Before = strings.Join(failed[:len(failed)-1], "; ")
	}
	return "", "", fmt.Errorf("every agent failed: %s: %w", strings.Join(failed[:len(failed)-1], "; "), err)
}

// fallbackEvent records that amf moves on from one agent to the next.
func (r *Runner) fallbackEvent(n int, from, to, reason, detail string) {
	text := fmt.Sprintf("#%d: %s failed (%s); falling back to %s", n, agentName(from), detail, agentName(to))
	// detail already names the agent and the reason for these, so saying
	// "failed" again is noise: "codex is not installed; falling back to
	// opencode" is clearer.
	if reason == "unusable" || reason == "agent_timeout" {
		text = fmt.Sprintf("#%d: %s; falling back to %s", n, detail, agentName(to))
	}
	r.event(slog.LevelWarn, "agent.fallback", n, text,
		"from", agentName(from), "to", agentName(to), "reason", reason, "error", detail)
}

// removeWorktree force-removes an issue's worktree, even one git has already
// forgotten about, and prunes the record. Used after a timeout, once the
// agent's process group is confirmed dead.
func (r *Runner) removeWorktree(ctx context.Context, n int) {
	wt := filepath.Join(r.Root, ".amf", "worktrees", fmt.Sprintf("issue-%d", n))
	if _, err := git(ctx, r.Root, "worktree", "remove", "--force", wt); err != nil {
		// Not a worktree git knows about, such as a leftover folder.
		if err := os.RemoveAll(wt); err != nil {
			r.event(slog.LevelWarn, "issue.cleanup", n, fmt.Sprintf("#%d: could not remove the worktree: %v", n, err), "error", err)
			return
		}
	}
	if _, err := git(ctx, r.Root, "worktree", "prune"); err != nil {
		r.event(slog.LevelWarn, "issue.cleanup", n, fmt.Sprintf("#%d: could not prune the worktrees: %v", n, err), "error", err)
	}
}

// prepareWorktree creates a fresh worktree on branch, starting at origin/base.
// A worktree left behind by an earlier failed run is discarded.
func (r *Runner) prepareWorktree(ctx context.Context, wt, branch, base string) error {
	if _, err := git(ctx, r.Root, "fetch", "origin", base); err != nil {
		return err
	}
	if _, err := os.Stat(wt); err == nil {
		r.event(slog.LevelInfo, "issue.worktree", 0, fmt.Sprintf("removing stale worktree %s", wt), "path", wt, "stale", true)
		if _, err := git(ctx, r.Root, "worktree", "remove", "--force", wt); err != nil {
			// Not a worktree git knows about, such as a leftover folder.
			if err := os.RemoveAll(wt); err != nil {
				return err
			}
		}
	}
	if _, err := git(ctx, r.Root, "worktree", "prune"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		return err
	}
	_, err := git(ctx, r.Root, "worktree", "add", "-B", branch, wt, "origin/"+base)
	return err
}

// commitAll commits whatever the agent changed and fails if the branch ends
// up changing nothing relative to base.
func commitAll(ctx context.Context, wt, title string, n int, base, agent string) error {
	if _, err := git(ctx, wt, "add", "-A", "--", ".", memoryExclude); err != nil {
		return err
	}
	status, err := git(ctx, wt, "status", "--porcelain", "--", ".", memoryExclude)
	if err != nil {
		return err
	}
	if status != "" {
		if _, err := git(ctx, wt, "commit", "-m", title, "-m", fmt.Sprintf("Closes #%d", n)); err != nil {
			return err
		}
	}
	// The agent may have committed on its own despite the prompt, so compare
	// trees rather than look for amf's commit.
	changed, err := changedPaths(ctx, wt, base, "HEAD")
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		return fmt.Errorf("%s finished without making any changes", agentName(agent))
	}
	return pathRefusal(changed)
}

// changedPaths lists the paths rev changes relative to origin/base. -z
// keeps git from quoting unusual names, and --no-renames lists both sides
// of a move, so neither can hide a path from pathRefusal.
func changedPaths(ctx context.Context, dir, base, rev string) ([]string, error) {
	out, err := git(ctx, dir, "diff", "--name-only", "--no-renames", "-z", "origin/"+base, rev)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// pathRefusal returns an error when any of changed is under .github/: a
// pushed workflow change could run whatever the agent put there with the
// repo's Actions secrets the moment the PR opens. amf runs the check before
// any push, so nothing is pushed and no PR is created.
// protectedPaths decides which paths count.
func pathRefusal(changed []string) error {
	if bad := protectedPaths(changed); bad != nil {
		return fmt.Errorf("the agent changed files under .github/ (%s), "+
			"which amf refuses to push", strings.Join(bad, ", "))
	}
	return nil
}

// protectedPaths returns the paths in changed that start with .github/,
// where workflows, CODEOWNERS and the like live.
func protectedPaths(changed []string) []string {
	var bad []string
	for _, p := range changed {
		if strings.HasPrefix(p, ".github/") {
			bad = append(bad, p)
		}
	}
	return bad
}

func buildPrompt(repo, branch string, issue Issue) string {
	body := strings.TrimSpace(issue.Body)
	if body == "" {
		body = "(no description)"
	}
	return fmt.Sprintf(`You are working in a git worktree of the GitHub repository %s, on branch %s.
Resolve this GitHub issue:

# #%d: %s

%s

Instructions:
- Make the smallest complete change that resolves the issue, following the existing code style.
- Find and run the project's tests, build and linters. Fix any failures before you finish.
- Do not create, edit or delete anything under .github/: amf refuses to push such changes.
- Do not commit, push, create branches or open pull requests. amf handles git and GitHub.
- When you are done, reply with only a pull request description in Markdown:
  a short summary of what changed and why, and how you verified it.
`, repo, branch, issue.Number, issue.Title, body)
}

// cleanSummary prepares Claude's summary for a PR body. It drops attribution
// lines (Claude Code's "Generated with" footer, Co-Authored-By trailers) so
// the PR is credited to the user alone, and Claude's own "Closes #N" line,
// since amf adds that itself.
func cleanSummary(summary string, n int) string {
	closes := regexp.MustCompile(fmt.Sprintf(`(?i)^(closes|fixes|resolves) #%d\.?$`, n))
	var keep []string
	for _, line := range strings.Split(summary, "\n") {
		t := strings.TrimSpace(line)
		low := strings.ToLower(t)
		if strings.Contains(low, "generated with [claude code]") || strings.Contains(low, "generated with claude code") ||
			strings.HasPrefix(low, "co-authored-by:") || closes.MatchString(t) {
			continue
		}
		keep = append(keep, line)
	}
	out := strings.TrimSpace(strings.Join(keep, "\n"))
	return regexp.MustCompile(`\n{3,}`).ReplaceAllString(out, "\n\n")
}

// claudeResult is the final message Claude prints in its JSON output formats.
type claudeResult struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

// parseClaudeResult extracts the final message from Claude's output. That is
// stream-json (one message per line, ending in a result), or for
// `--output-format json` a single result object or an array of messages
// ending in one, depending on the Claude Code version. Output that isn't JSON
// is treated as plain text.
func parseClaudeResult(out []byte) (string, error) {
	out = bytes.TrimSpace(out)
	res, ok, isJSON := lastResult(out)
	if !isJSON {
		return string(out), nil
	}
	if !ok {
		return "", errors.New("claude output has no result message")
	}
	if res.IsError {
		return "", fmt.Errorf("claude reported an error (%s): %s", res.Subtype, truncate(res.Result, 500))
	}
	return res.Result, nil
}

// lastResult finds the last result message in out. isJSON is false when out
// isn't in any of Claude's JSON formats.
func lastResult(out []byte) (res claudeResult, ok, isJSON bool) {
	if json.Unmarshal(out, &res) == nil {
		return res, res.Type == "result", true
	}
	var msgs []claudeResult
	if json.Unmarshal(out, &msgs) == nil {
		return lastOfType(msgs)
	}
	lines := bytes.Split(out, []byte("\n"))
	msgs = msgs[:0]
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var m claudeResult
		if json.Unmarshal(line, &m) != nil {
			return claudeResult{}, false, false
		}
		msgs = append(msgs, m)
	}
	return lastOfType(msgs)
}

func lastOfType(msgs []claudeResult) (claudeResult, bool, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Type == "result" {
			return msgs[i], true, true
		}
	}
	return claudeResult{}, false, true
}

func (r *Runner) setStatus(ctx context.Context, n int, status string) error {
	if err := r.GH.AddLabels(ctx, n, status); err != nil {
		return err
	}
	for _, l := range statusLabels {
		if l == status {
			continue
		}
		if err := r.GH.RemoveLabel(ctx, n, l); err != nil {
			return err
		}
	}
	return nil
}

// needsWork reports whether watch should pick up an issue: one that isn't
// already in progress, and either is marked amf:ready (a retry if it was
// done or failed before) or carries a custom --label and no status yet.
func needsWork(is Issue) bool { return skipReason(is) == "" }

// skipReason is why watch passes over an issue: pr, closed, in_progress or
// status_label (done or failed, without amf:ready), or "" to work on it.
func skipReason(is Issue) string {
	switch {
	case is.PullRequest != nil:
		return "pr"
	case is.State != "open":
		return "closed"
	case is.HasLabel(labelInProgress):
		return "in_progress"
	case is.HasLabel(labelReady):
		return ""
	}
	for _, l := range statusLabels {
		if is.HasLabel(l) {
			return "status_label"
		}
	}
	return ""
}

// excludedLabel is the first of exclude that the issue has, or "".
func excludedLabel(is Issue, exclude []string) string {
	for _, l := range exclude {
		if is.HasLabel(l) {
			return l
		}
	}
	return ""
}

// Watch polls for open issues labeled label and runs them one at a time
// until ctx is cancelled, skipping those with any of r.ExcludeLabels.
func (r *Runner) Watch(ctx context.Context, label string, interval time.Duration) error {
	ctx = withEvents(ctx, r.Events, 0)
	except := ""
	if len(r.ExcludeLabels) > 0 {
		except = fmt.Sprintf(", except those labeled %q", r.ExcludeLabels)
	}
	merging := ""
	if r.Merge != "" {
		merging = fmt.Sprintf(", merging each PR without review (%s)", r.Merge)
	}
	r.event(slog.LevelInfo, "watch.start", 0, fmt.Sprintf("watching %s for issues labeled %q%s every %s%s (Ctrl-C to stop)", r.Repo, label, except, interval, merging),
		"repo", r.Repo, "label", label, "exclude_labels", append([]string{}, r.ExcludeLabels...), "interval", interval.String(), "timeout", r.Timeout.String(),
		"eval_rounds", r.EvalRounds, "draft", r.Draft, "merge", cmp.Or(r.Merge, "off"), "agent_flag", agentName(r.Agent), "version", buildVersion())
	r.event(slog.LevelInfo, "watch.agents", 0, "agents for issues without a pipeline (claude and codex need a key, opencode one for a paid model):")
	for _, st := range r.agentStatuses() {
		r.event(slog.LevelInfo, "watch.agents", 0, "  "+st.describe(),
			"agent", st.Agent, "path", st.Path, "key", st.Key, "usable", st.usable(), "model", st.Model)
	}
	switch plan, err := r.agentPlan(); {
	case r.Agent != "":
		r.event(slog.LevelInfo, "watch.agents", 0, fmt.Sprintf("issues without a pipeline run %s (--agent)", agentName(r.Agent)), "picked", agentName(r.Agent))
	case err != nil:
		r.event(slog.LevelWarn, "watch.agents", 0, fmt.Sprintf("warning: %v; issues without a pipeline wait until one is available", err), "error", err)
	default:
		text := fmt.Sprintf("issues without a pipeline run %s", plan.describe())
		if r.Primary != "" {
			text += fmt.Sprintf(" (primary %s, secondary %s)", agentName(r.Primary), agentNameOrNone(r.Secondary))
		}
		for _, s := range plan.Skipped {
			text += fmt.Sprintf("; %s is not usable: %s", agentName(s.Agent), s.Reason)
		}
		r.event(slog.LevelInfo, "watch.agents", 0, text, "picked", agentName(plan.Agents[0]), "fallback", plan.describe())
	}
	// amf:pr-open is newer than most repos' amf init: create it if missing.
	if _, err := r.GH.EnsureLabel(ctx, labelSpec(labelPROpen)); err != nil {
		r.event(slog.LevelDebug, "watch.labels", 0, fmt.Sprintf("could not create %s: %v", labelPROpen, err), "error", err)
	}
	for cycle := 1; ; cycle++ {
		r.sweepStale(ctx)
		r.sweepFailed(ctx)
		r.sweepPRs(ctx)
		issues, err := r.GH.ListIssues(ctx, "open", label, true)
		switch {
		case err != nil && ctx.Err() == nil:
			r.event(slog.LevelWarn, "watch.tick", 0, fmt.Sprintf("listing issues: %v", err), "cycle", cycle, "error", err)
		case err == nil:
			r.event(slog.LevelDebug, "watch.tick", 0, fmt.Sprintf("poll %d: %d open %s labeled %s", cycle, len(issues), plural(len(issues), "issue", "issues"), label),
				"cycle", cycle, "issues", len(issues))
		}
		for _, is := range issues {
			if ctx.Err() != nil {
				break
			}
			if why := skipReason(is); why != "" {
				r.event(slog.LevelDebug, "watch.skip", is.Number, fmt.Sprintf("#%d: skipped (%s)", is.Number, why), "reason", why)
				continue
			}
			if l := excludedLabel(is, r.ExcludeLabels); l != "" {
				r.event(slog.LevelDebug, "watch.skip", is.Number, fmt.Sprintf("#%d: skipped (excluded_label %q)", is.Number, l), "reason", "excluded_label", "label", l)
				continue
			}
			if err := r.Run(ctx, is.Number); err != nil && !errors.As(err, new(*skipError)) {
				r.event(slog.LevelWarn, "watch.failed", is.Number, fmt.Sprintf("#%d failed: %v", is.Number, err), "error", err)
			}
		}
		select {
		case <-ctx.Done():
			r.event(slog.LevelInfo, "watch.stop", 0, "stopped", "reason", ctx.Err(), "cycles", cycle)
			return nil
		case <-time.After(interval):
		}
	}
}

// claudeEnv is the environment for Claude: never the GitHub token or the
// OpenCode key, and with a
// subscription token configured, no API key that would take precedence over it.
func (r *Runner) claudeEnv(env []string) []string {
	switch {
	case r.ClaudeToken != "":
		// The subscription token wins: an API key would take precedence over it.
		env = withoutEnv(env, "GITHUB_TOKEN", "GH_TOKEN", "OPENCODE_API_KEY", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN")
		return append(env, "CLAUDE_CODE_OAUTH_TOKEN="+r.ClaudeToken)
	case r.AnthropicKey != "":
		env = withoutEnv(env, "GITHUB_TOKEN", "GH_TOKEN", "OPENCODE_API_KEY", "ANTHROPIC_API_KEY")
		return append(env, "ANTHROPIC_API_KEY="+r.AnthropicKey)
	}
	return withoutEnv(env, "GITHUB_TOKEN", "GH_TOKEN", "OPENCODE_API_KEY")
}

func withoutEnv(env []string, keys ...string) []string {
	out := env[:0:0]
outer:
	for _, kv := range env {
		for _, k := range keys {
			if strings.HasPrefix(kv, k+"=") {
				continue outer
			}
		}
		out = append(out, kv)
	}
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// confirmMerged reads pull request n back after a merge that GitHub
// accepted, so amf:done is set only on a PR that is merged. A failed read
// trusts the merge response.
func (r *Runner) confirmMerged(ctx context.Context, n int) bool {
	pr, err := r.GH.GetPR(ctx, n)
	if err != nil {
		return true
	}
	return pr.Merged
}

// remoteTip is the commit origin/branch points at, or "" when the branch
// does not exist there.
func remoteTip(ctx context.Context, dir, branch string) (string, error) {
	out, err := git(ctx, dir, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("could not check origin/%s: %w", branch, err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}

// mergedIntoBase reports whether tip is already in origin/base: a commit on
// it (a merge-commit or fast-forward merge), or the head of a merged pull
// request from branch (a squash or rebase merge leaves no ancestry). It
// assumes origin/base and tip's commit are available locally.
func (r *Runner) mergedIntoBase(ctx context.Context, dir, branch, base, tip string) (merged bool, reason string, err error) {
	ancestor, err := isAncestor(ctx, dir, tip, "origin/"+base)
	if err != nil {
		return false, "", err
	}
	if ancestor {
		return true, "ancestor", nil
	}
	mergedPR, err := r.GH.MergedPRAt(ctx, branch, tip)
	if err != nil {
		return false, "", err
	}
	if mergedPR {
		return true, "merged_pr", nil
	}
	return false, "", nil
}

// isAncestor reports whether commit a is an ancestor of b.
func isAncestor(ctx context.Context, dir, a, b string) (bool, error) {
	out, err := git(ctx, dir, "rev-list", "--count", a, "^"+b)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "0", nil
}

// pushArgs builds the git push for branch. When old, the remote tip just
// inspected, is missing or an ancestor of the new head, the push is plain.
// Otherwise it replaces the remote tip, but only with a lease pinned to the
// tip amf inspected, so it cannot clobber a push it did not see.
func pushArgs(ctx context.Context, dir, branch, old string) ([]string, error) {
	args := []string{"push", "-u", "origin", branch}
	if old == "" {
		return args, nil
	}
	head, err := git(ctx, dir, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	ancestor, err := isAncestor(ctx, dir, old, head)
	if err != nil {
		return nil, err
	}
	if ancestor {
		return args, nil
	}
	return []string{"push", "--force-with-lease=" + branch + ":" + old, "-u", "origin", branch}, nil
}

// backupBranch saves the remote branch before process replaces it: when
// origin/branch has commits that are neither on origin/base nor in the new
// local head, they are pushed to amf/backup/issue-N-<sha>-<time> first. A
// merged branch, including a squash- or rebase-merged one, loses nothing and
// is not backed up. A failed backup is an error, so amf never overwrites
// commits it could not save.
func (r *Runner) backupBranch(ctx context.Context, wt string, n int, branch, base, old string) error {
	if old == "" {
		return nil // a new branch: nothing to replace
	}
	if _, err := git(ctx, wt, "fetch", "origin", old); err != nil {
		return fmt.Errorf("could not fetch origin/%s before replacing it: %w", branch, err)
	}
	lost, err := git(ctx, wt, "rev-list", "--count", old, "^HEAD", "^origin/"+base)
	if err != nil {
		return fmt.Errorf("could not compare origin/%s before replacing it: %w", branch, err)
	}
	if strings.TrimSpace(lost) == "0" {
		return nil
	}
	merged, _, err := r.mergedIntoBase(ctx, wt, branch, base, old)
	if err != nil {
		return fmt.Errorf("could not check whether origin/%s is merged: %w", branch, err)
	}
	if merged {
		return nil
	}
	backup := fmt.Sprintf("amf/backup/issue-%d-%s-%s", n, shortSHA(old), time.Now().UTC().Format("20060102T150405Z"))
	if _, err := git(ctx, wt, "push", "origin", old+":refs/heads/"+backup); err != nil {
		return fmt.Errorf("not replacing %s: could not back up its %s commit(s) to %s: %w", branch, strings.TrimSpace(lost), backup, err)
	}
	r.event(slog.LevelInfo, "issue.backup", n, fmt.Sprintf("#%d: backed up %s (%s, %s commit(s) not in the new push) to %s", n, branch, shortSHA(old), strings.TrimSpace(lost), backup),
		"branch", branch, "sha", old, "backup", backup)
	r.backupNote = fmt.Sprintf("The earlier `%s` (%s) had %s commit(s) this push replaced; amf saved them to the `%s` branch.", branch, shortSHA(old), strings.TrimSpace(lost), backup)
	return nil
}
