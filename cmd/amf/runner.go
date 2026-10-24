package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	HTTP       *http.Client // for downloading skills; nil means a default client
	Log        io.Writer    // progress output
	// DB, when set, also gets every agent's output; Log already sends it
	// amf's own lines.
	DB *dbLog
	// Redact masks secrets and personal data in the agents' logs, the
	// comments and the PR body. newRunner also redacts Log with it. Nil
	// changes nothing.
	Redact *redactor
}

// Close sends the logs still queued for PostgreSQL.
func (r *Runner) Close() {
	r.DB.Close(15 * time.Second)
}

func (r *Runner) logf(format string, a ...any) {
	fmt.Fprintf(r.Log, "amf: "+format+"\n", a...)
}

// Run processes one issue end to end. On failure the issue is labeled
// amf:failed and the reason is commented. A run that timed out leaves no PR,
// no push and no branch: it removes the worktree, keeping the logs and the
// partial output; any other failure keeps the worktree for inspection.
func (r *Runner) Run(ctx context.Context, n int) (err error) {
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
		r.logf("#%d: ignoring --agent %s: AGENTS.md on origin/%s declares a pipeline, which names each stage's agent", n, r.Agent, base)
	}
	if _, err := gitIdentity(ctx, r.Root); err != nil {
		return err
	}

	r.logf("#%d: %s", n, issue.Title)
	if err := r.setStatus(ctx, n, labelInProgress); err != nil {
		return err
	}
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
			r.logf("#%d: could not set %s: %v", n, labelFailed, serr)
		}
		msg := fmt.Sprintf("amf could not turn this issue into a pull request:\n\n```\n%s\n```", err)
		var se *stageError
		if errors.As(err, &se) {
			msg = se.comment
		}
		var ate *agentTimeoutError
		if errors.As(err, &ate) {
			msg = ate.comment(r.Root)
			r.logf("issue.failed: issue=%d reason=agent_timeout", n)
			// The agent's process group is dead, so nothing still writes
			// into the worktree, and it has no branch holding partial work.
			r.removeWorktree(cctx, n)
		}
		if cerr := r.GH.Comment(cctx, n, r.Redact.Redact(msg)); cerr != nil {
			r.logf("#%d: could not comment: %v", n, cerr)
		}
	}()

	if err := r.checkAuthor(issue); err != nil {
		return err
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
	r.logf("#%d: %s %s", n, verb, pr.HTMLURL)
	if err := r.setStatus(ctx, n, labelDone); err != nil {
		r.logf("#%d: could not set %s: %v", n, labelDone, err)
	}
	if err := r.GH.Comment(ctx, n, r.Redact.Redact(fmt.Sprintf("amf %s %s for this issue.", verb, pr.HTMLURL))); err != nil {
		r.logf("#%d: could not comment: %v", n, err)
	}
	return nil
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

	r.logf("#%d: creating worktree %s on %s from origin/%s", n, wt, branch, base)
	if err := r.prepareWorktree(ctx, wt, branch, base); err != nil {
		return PullRequest{}, false, err
	}

	title := fmt.Sprintf("Fix #%d: %s", n, issue.Title)
	var summary, extra string
	if p == nil {
		model := r.agentModel(agent, "")
		if model != "" {
			r.logf("#%d: running %s (%s, timeout %s)", n, agentName(agent), model, r.Timeout)
		} else {
			r.logf("#%d: running %s (timeout %s)", n, agentName(agent), r.Timeout)
		}
		log := filepath.Join(r.Root, ".amf", "logs", fmt.Sprintf("issue-%d.log", n))
		out := filepath.Join(wt, ".amf", "code.md") // for agents that write their final message to a file
		if summary, err = r.runAgent(ctx, agent, "", model, wt, buildPrompt(r.Repo, branch, issue), log, out); err != nil {
			return PullRequest{}, false, err
		}
		if err := commitAll(ctx, wt, title, n, base); err != nil {
			return PullRequest{}, false, err
		}
	} else {
		r.logf("#%d: running the pipeline from AGENTS.md: %s", n, describePipeline(p))
		if summary, extra, err = r.runPipeline(ctx, wt, issue, branch, base, p); err != nil {
			return PullRequest{}, false, err
		}
	}

	r.logf("#%d: pushing %s", n, branch)
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
	pr, err = r.GH.CreatePR(ctx, title, branch, base, body)
	if isStatus(err, http.StatusUnprocessableEntity) {
		// A retry of an issue whose PR is still open: the push updated it,
		// so bring its description up to date too.
		if open, ok, ferr := r.GH.OpenPR(ctx, branch); ferr == nil && ok {
			r.logf("#%d: %s already has %s; updating it", n, branch, open.HTMLURL)
			pr, err = r.GH.UpdatePR(ctx, open.Number, title, body)
			updated = true
		}
	}
	if err != nil {
		return PullRequest{}, false, err
	}

	if _, err := git(ctx, r.Root, "worktree", "remove", wt); err != nil {
		r.logf("#%d: could not remove worktree: %v", n, err)
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
			r.logf("#%d: could not remove the worktree: %v", n, err)
			return
		}
	}
	if _, err := git(ctx, r.Root, "worktree", "prune"); err != nil {
		r.logf("#%d: could not prune the worktrees: %v", n, err)
	}
}

// prepareWorktree creates a fresh worktree on branch, starting at origin/base.
// A worktree left behind by an earlier failed run is discarded.
func (r *Runner) prepareWorktree(ctx context.Context, wt, branch, base string) error {
	if _, err := git(ctx, r.Root, "fetch", "origin", base); err != nil {
		return err
	}
	if _, err := os.Stat(wt); err == nil {
		r.logf("removing stale worktree %s", wt)
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

// commitAll commits whatever Claude changed and fails if the branch ends up
// changing nothing relative to base.
func commitAll(ctx context.Context, wt, title string, n int, base string) error {
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
	// Claude may have committed on its own despite the prompt, so compare
	// trees rather than look for amf's commit.
	changed, err := changedPaths(ctx, wt, base)
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		return errors.New("claude finished without making any changes")
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
// any push, so no branch or PR is created.
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
func needsWork(is Issue) bool {
	if is.PullRequest != nil || is.State != "open" || is.HasLabel(labelInProgress) {
		return false
	}
	if is.HasLabel(labelReady) {
		return true
	}
	for _, l := range statusLabels {
		if is.HasLabel(l) {
			return false
		}
	}
	return true
}

// Watch polls for open issues labeled label and runs them one at a time
// until ctx is cancelled.
func (r *Runner) Watch(ctx context.Context, label string, interval time.Duration) error {
	r.logf("watching %s for issues labeled %q every %s (Ctrl-C to stop)", r.Repo, label, interval)
	r.logf("agents for issues without a pipeline (claude and codex need a key):")
	for _, st := range r.agentStatuses() {
		r.logf("  %s", st.describe())
	}
	switch agent, _, err := r.pickAgent(); {
	case r.Agent != "":
		r.logf("issues without a pipeline run %s (--agent)", agentName(r.Agent))
	case err != nil:
		r.logf("warning: %v; issues without a pipeline wait until one is available", err)
	default:
		r.logf("issues without a pipeline run %s", agentName(agent))
	}
	for {
		issues, err := r.GH.ListIssues(ctx, "open", label, true)
		if err != nil && ctx.Err() == nil {
			r.logf("listing issues: %v", err)
		}
		for _, is := range issues {
			if ctx.Err() != nil {
				break
			}
			if !needsWork(is) {
				continue
			}
			if err := r.Run(ctx, is.Number); err != nil {
				r.logf("#%d failed: %v", is.Number, err)
			}
		}
		select {
		case <-ctx.Done():
			r.logf("stopped")
			return nil
		case <-time.After(interval):
		}
	}
}

// claudeEnv is the environment for Claude: never the GitHub token, and with a
// subscription token configured, no API key that would take precedence over it.
func (r *Runner) claudeEnv(env []string) []string {
	switch {
	case r.ClaudeToken != "":
		// The subscription token wins: an API key would take precedence over it.
		env = withoutEnv(env, "GITHUB_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN")
		return append(env, "CLAUDE_CODE_OAUTH_TOKEN="+r.ClaudeToken)
	case r.AnthropicKey != "":
		env = withoutEnv(env, "GITHUB_TOKEN", "ANTHROPIC_API_KEY")
		return append(env, "ANTHROPIC_API_KEY="+r.AnthropicKey)
	}
	return withoutEnv(env, "GITHUB_TOKEN")
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
