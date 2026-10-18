package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	GH     *GitHub
	Repo   string // owner/name, for the prompt
	Root   string // top level of the local checkout
	Claude string // claude binary
	// ClaudeToken, when set, is passed to Claude as CLAUDE_CODE_OAUTH_TOKEN.
	ClaudeToken string
	Timeout     time.Duration
	Log         io.Writer // progress output
}

func (r *Runner) logf(format string, a ...any) {
	fmt.Fprintf(r.Log, "amf: "+format+"\n", a...)
}

// Run processes one issue end to end. On failure the issue is labeled
// amf:failed, the reason is commented, and the worktree is kept for inspection.
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
		if cerr := r.GH.Comment(cctx, n, msg); cerr != nil {
			r.logf("#%d: could not comment: %v", n, cerr)
		}
	}()

	pr, err := r.process(ctx, issue)
	if err != nil {
		return err
	}

	r.logf("#%d: opened %s", n, pr.HTMLURL)
	if err := r.GH.Comment(ctx, n, fmt.Sprintf("amf opened %s for this issue.", pr.HTMLURL)); err != nil {
		return err
	}
	return r.setStatus(ctx, n, labelDone)
}

func (r *Runner) process(ctx context.Context, issue Issue) (PullRequest, error) {
	n := issue.Number
	base, err := r.GH.DefaultBranch(ctx)
	if err != nil {
		return PullRequest{}, err
	}
	branch := fmt.Sprintf("amf/issue-%d", n)
	wt := filepath.Join(r.Root, ".amf", "worktrees", fmt.Sprintf("issue-%d", n))

	r.logf("#%d: creating worktree %s on %s from origin/%s", n, wt, branch, base)
	if err := r.prepareWorktree(ctx, wt, branch, base); err != nil {
		return PullRequest{}, err
	}

	r.logf("#%d: running claude (timeout %s)", n, r.Timeout)
	summary, err := r.runClaude(ctx, wt, issue, branch)
	if err != nil {
		return PullRequest{}, err
	}

	title := fmt.Sprintf("Fix #%d: %s", n, issue.Title)
	if err := commitAll(ctx, wt, title, n, base); err != nil {
		return PullRequest{}, err
	}

	r.logf("#%d: pushing %s", n, branch)
	// amf owns amf/issue-* branches, so a re-run may replace an earlier push.
	if _, err := git(ctx, wt, "push", "--force", "-u", "origin", branch); err != nil {
		return PullRequest{}, err
	}

	if strings.TrimSpace(summary) == "" {
		summary = fmt.Sprintf("Resolves #%d.", n)
	}
	body := fmt.Sprintf("%s\n\nCloses #%d\n\n---\n_Opened by amf._", strings.TrimSpace(summary), n)
	pr, err := r.GH.CreatePR(ctx, title, branch, base, body)
	if err != nil {
		return PullRequest{}, err
	}

	if _, err := git(ctx, r.Root, "worktree", "remove", wt); err != nil {
		r.logf("#%d: could not remove worktree: %v", n, err)
	}
	return pr, nil
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
			return err
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
// with nothing new relative to base.
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
	// Claude may have committed on its own despite the prompt; count commits.
	ahead, err := git(ctx, wt, "rev-list", "--count", "origin/"+base+"..HEAD")
	if err != nil {
		return err
	}
	if ahead == "0" {
		return errors.New("claude finished without making any changes")
	}
	return nil
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
- Do not commit, push, create branches or open pull requests. amf handles git and GitHub.
- When you are done, reply with only a pull request description in Markdown:
  a short summary of what changed and why, and how you verified it.
`, repo, branch, issue.Number, issue.Title, body)
}

// runClaude runs Claude Code headless in wt and returns its final message.
// Output is saved to .amf/logs/issue-N.log.
func (r *Runner) runClaude(ctx context.Context, wt string, issue Issue, branch string) (string, error) {
	logPath := filepath.Join(r.Root, ".amf", "logs", fmt.Sprintf("issue-%d.log", issue.Number))
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return "", err
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return "", err
	}
	defer logFile.Close()

	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	// stream-json writes one JSON message per line as Claude works, so the log
	// fills in live; it requires --verbose in print mode.
	cmd := exec.CommandContext(ctx, r.Claude, "-p", "--dangerously-skip-permissions", "--output-format", "stream-json", "--verbose")
	cmd.Dir = wt
	cmd.Env = r.claudeEnv(os.Environ())
	cmd.Stdin = strings.NewReader(buildPrompt(r.Repo, branch, issue))
	var stdout bytes.Buffer
	cmd.Stdout = io.MultiWriter(&stdout, logFile)
	cmd.Stderr = logFile
	cmd.WaitDelay = 10 * time.Second

	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("claude timed out after %s (log: %s)", r.Timeout, logPath)
		}
		return "", fmt.Errorf("claude: %v (log: %s)", err, logPath)
	}
	return parseClaudeResult(stdout.Bytes())
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
	for {
		issues, err := r.GH.ListOpenIssues(ctx, label)
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
	if r.ClaudeToken == "" {
		return withoutEnv(env, "GITHUB_TOKEN")
	}
	env = withoutEnv(env, "GITHUB_TOKEN", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN")
	return append(env, "CLAUDE_CODE_OAUTH_TOKEN="+r.ClaudeToken)
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
