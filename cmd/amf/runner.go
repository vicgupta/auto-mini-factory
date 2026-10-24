package main

import (
	"bytes"
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

// An issue moves amf:ready -> amf:in-progress -> amf:done or amf:failed.
// Adding amf:ready again asks amf to retry it.
const (
	labelReady      = "amf:ready"
	labelInProgress = "amf:in-progress"
	labelDone       = "amf:done"
	labelFailed     = "amf:failed"
)

var statusLabels = []string{labelInProgress, labelDone, labelFailed}

// amfLabels are the labels amf uses, as `amf init` creates them.
var amfLabels = []LabelSpec{
	{labelReady, "0e8a16", "Ready for amf to turn into a pull request"},
	{labelInProgress, "fbca04", "amf is working on this issue"},
	{labelDone, "1d76db", "amf opened a pull request for this issue"},
	{labelFailed, "d93f0b", "amf could not process this issue; see its comment"},
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
	HTTP          *http.Client // for downloading skills; nil means a default client
	Log           io.Writer    // progress output
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
	// checkout is how newRunner found Root: checkout (the enclosing one),
	// clone (reused) or cloned.
	checkout string
	// stats counts the events for the run summary, which goes to
	// summaryPath ("" for none) after each issue and at Close.
	stats       *statsHandler
	summaryPath string
}

// Close writes out the event file and sends the logs still queued for
// PostgreSQL.
func (r *Runner) Close() {
	r.summarize(true)
	if err := r.events.Close(); err != nil {
		fmt.Fprintf(r.Log, "amf: could not write the event log: %v\n", err)
	}
	r.DB.Close(15 * time.Second)
}

// Run processes one issue end to end. On failure the issue is labeled
// amf:failed and the reason is commented. A run that timed out leaves no PR
// and pushes nothing: it removes the worktree, keeping the logs, the partial
// output and the unpushed local branch, which a retry resets; any other
// failure keeps the worktree for inspection.
func (r *Runner) Run(ctx context.Context, n int) (err error) {
	defer r.summarize(false)           // runs last, after the deferred failure handling
	ctx = withEvents(ctx, r.Events, n) // git, GitHub and skill calls log there
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
	agent := ""
	if pipeline == nil {
		if agent, err = r.defaultAgent(); err != nil {
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
	if err := r.setStatus(ctx, n, labelInProgress); err != nil {
		return err
	}
	r.statusEvent(n, statusOf(issue), labelInProgress)
	// The issue is no longer waiting; adding amf:ready again retries it.
	if err := r.GH.RemoveLabel(ctx, n, labelReady); err != nil {
		return err
	}
	defer func() {
		if err == nil {
			return
		}
		// Report even if ctx was cancelled (e.g. Ctrl-C), but don't hang on it.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if serr := r.setStatus(cctx, n, labelFailed); serr != nil {
			r.event(slog.LevelWarn, "issue.status", n, fmt.Sprintf("#%d: could not set %s: %v", n, labelFailed, serr), "to", labelFailed, "error", serr)
		} else {
			r.statusEvent(n, labelInProgress, labelFailed)
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
		r.comment(cctx, n, msg)
	}()

	trusted := r.checkAuthor(issue)
	r.event(slog.LevelDebug, "issue.author_check", n, fmt.Sprintf("#%d: opened by @%s", n, issue.User.Login),
		"author", issue.User.Login, "trusted", trusted == nil)
	if trusted != nil {
		return trusted
	}

	pr, updated, err := r.process(ctx, issue, base, pipeline, agent)
	if err != nil {
		return err
	}

	// The PR exists, so the issue is done even if GitHub fails from here on.
	verb := "opened"
	if updated {
		verb = "updated"
	}
	r.event(slog.LevelInfo, "issue.pr", n, fmt.Sprintf("#%d: %s %s", n, verb, pr.HTMLURL),
		"action", verb, "number", pr.Number, "url", pr.HTMLURL)
	if err := r.setStatus(ctx, n, labelDone); err != nil {
		r.event(slog.LevelWarn, "issue.status", n, fmt.Sprintf("#%d: could not set %s: %v", n, labelDone, err), "to", labelDone, "error", err)
	} else {
		r.statusEvent(n, labelInProgress, labelDone)
	}
	r.comment(ctx, n, fmt.Sprintf("amf %s %s for this issue.", verb, pr.HTMLURL))
	r.event(slog.LevelDebug, "issue.done", n, fmt.Sprintf("#%d: done", n), "pr", pr.Number)
	return nil
}

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

// checkAuthor fails an issue whose author isn't in Trusted. It runs after
// Claim, so the issue is marked failed once and watch doesn't retry it.
func (r *Runner) checkAuthor(issue Issue) error {
	if r.Trusted == nil || slices.Contains(r.Trusted, strings.ToLower(issue.User.Login)) {
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
func (r *Runner) process(ctx context.Context, issue Issue, base string, p *Pipeline, agent string) (pr PullRequest, updated bool, err error) {
	n := issue.Number
	branch := fmt.Sprintf("amf/issue-%d", n)
	wt := filepath.Join(r.Root, ".amf", "worktrees", fmt.Sprintf("issue-%d", n))

	r.event(slog.LevelInfo, "issue.worktree", n, fmt.Sprintf("#%d: creating worktree %s on %s from origin/%s", n, wt, branch, base),
		"path", wt, "branch", branch, "base", base)
	if err := r.prepareWorktree(ctx, wt, branch, base); err != nil {
		return PullRequest{}, false, err
	}

	title := fmt.Sprintf("Fix #%d: %s", n, issue.Title)
	var summary, extra string
	if p == nil {
		model := r.agentModel(agent, "")
		r.event(slog.LevelDebug, "issue.pipeline", n, fmt.Sprintf("#%d: no pipeline; one %s run", n, agentName(agent)),
			"declared", false, "stages", []string{"code"}, "agents", []string{agentName(agent)}, "models", []string{model})
		text := fmt.Sprintf("#%d: running %s (timeout %s)", n, agentName(agent), r.Timeout)
		if model != "" {
			text = fmt.Sprintf("#%d: running %s (%s, timeout %s)", n, agentName(agent), model, r.Timeout)
		}
		r.event(slog.LevelInfo, "issue.agent", n, text, "agent", agentName(agent), "model", model, "timeout", r.Timeout.String())
		log := filepath.Join(r.Root, ".amf", "logs", fmt.Sprintf("issue-%d.log", n))
		out := filepath.Join(wt, ".amf", "code.md") // for agents that write their final message to a file
		if summary, err = r.runAgent(ctx, agent, "", model, wt, buildPrompt(r.Repo, branch, issue), log, out); err != nil {
			return PullRequest{}, false, err
		}
		if err := commitAll(ctx, wt, title, n, base, agent); err != nil {
			return PullRequest{}, false, err
		}
	} else {
		var stages, agents, models []string
		for _, s := range p.stages() {
			stages, agents, models = append(stages, s.Name), append(agents, s.Agent), append(models, s.model())
		}
		r.event(slog.LevelInfo, "issue.pipeline", n, fmt.Sprintf("#%d: running the pipeline from AGENTS.md: %s", n, describePipeline(p)),
			"declared", true, "stages", stages, "agents", agents, "models", models)
		if summary, extra, err = r.runPipeline(ctx, wt, issue, branch, base, p); err != nil {
			return PullRequest{}, false, err
		}
	}

	r.event(slog.LevelInfo, "issue.push", n, fmt.Sprintf("#%d: pushing %s", n, branch), "branch", branch)
	// amf owns amf/issue-* branches, so a re-run may replace an earlier push.
	if _, err := git(ctx, wt, "push", "--force", "-u", "origin", branch); err != nil {
		return PullRequest{}, false, err
	}

	summary = cleanSummary(summary, n)
	if summary == "" {
		summary = fmt.Sprintf("Resolves #%d.", n)
	}
	if extra != "" {
		summary += "\n\n" + extra
	}
	body := r.Redact.Redact(fmt.Sprintf("%s\n\nCloses #%d\n\n---\n_Opened by amf._", summary, n))
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

	if _, err := git(ctx, r.Root, "worktree", "remove", wt); err != nil {
		r.event(slog.LevelWarn, "issue.cleanup", n, fmt.Sprintf("#%d: could not remove worktree: %v", n, err), "error", err)
	}
	return pr, updated, nil
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
	if _, err := git(ctx, wt, "add", "-A"); err != nil {
		return err
	}
	status, err := git(ctx, wt, "status", "--porcelain")
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
	changed, err := changedPaths(ctx, wt, base)
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		return fmt.Errorf("%s finished without making any changes", agentName(agent))
	}
	return pathRefusal(changed)
}

// changedPaths lists the paths HEAD changes relative to origin/base. -z
// keeps git from quoting unusual names, and --no-renames lists both sides
// of a move, so neither can hide a path from pathRefusal.
func changedPaths(ctx context.Context, wt, base string) ([]string, error) {
	out, err := git(ctx, wt, "diff", "--name-only", "--no-renames", "-z", "origin/"+base, "HEAD")
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
	for _, l := range statusLabels {
		if l == status {
			continue
		}
		if err := r.GH.RemoveLabel(ctx, n, l); err != nil {
			return err
		}
	}
	return r.GH.AddLabels(ctx, n, status)
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
	r.event(slog.LevelInfo, "watch.start", 0, fmt.Sprintf("watching %s for issues labeled %q%s every %s (Ctrl-C to stop)", r.Repo, label, except, interval),
		"repo", r.Repo, "label", label, "exclude_labels", append([]string{}, r.ExcludeLabels...), "interval", interval.String(), "timeout", r.Timeout.String(),
		"eval_rounds", r.EvalRounds, "draft", r.Draft, "agent_flag", agentName(r.Agent), "version", buildVersion())
	r.event(slog.LevelInfo, "watch.agents", 0, "agents for issues without a pipeline (claude and codex need a key, opencode one for a paid model):")
	for _, st := range r.agentStatuses() {
		r.event(slog.LevelInfo, "watch.agents", 0, "  "+st.describe(),
			"agent", st.Agent, "path", st.Path, "key", st.Key, "usable", st.usable(), "model", st.Model)
	}
	switch agent, _, err := r.pickAgent(); {
	case r.Agent != "":
		r.event(slog.LevelInfo, "watch.agents", 0, fmt.Sprintf("issues without a pipeline run %s (--agent)", agentName(r.Agent)), "picked", agentName(r.Agent))
	case err != nil:
		r.event(slog.LevelWarn, "watch.agents", 0, fmt.Sprintf("warning: %v; issues without a pipeline wait until one is available", err), "error", err)
	default:
		r.event(slog.LevelInfo, "watch.agents", 0, fmt.Sprintf("issues without a pipeline run %s", agentName(agent)), "picked", agentName(agent))
	}
	for cycle := 1; ; cycle++ {
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
			if err := r.Run(ctx, is.Number); err != nil {
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
		env = withoutEnv(env, "GITHUB_TOKEN", "OPENCODE_API_KEY", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN")
		return append(env, "CLAUDE_CODE_OAUTH_TOKEN="+r.ClaudeToken)
	case r.AnthropicKey != "":
		env = withoutEnv(env, "GITHUB_TOKEN", "OPENCODE_API_KEY", "ANTHROPIC_API_KEY")
		return append(env, "ANTHROPIC_API_KEY="+r.AnthropicKey)
	}
	return withoutEnv(env, "GITHUB_TOKEN", "OPENCODE_API_KEY")
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
