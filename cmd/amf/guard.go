package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"
)

// skipError is returned by Run when the pre-start guard (see guard) passes
// over an issue: watch logs it and moves on, amf run reports it.
type skipError struct {
	n      int
	reason string // pr_open or branch_exists
	msg    string
}

func (e *skipError) Error() string { return e.msg }

// guard keeps amf from starting an issue it already turned into a pull
// request, which would force-push over the branch and post a second "amf
// opened" comment. It runs before the issue is labeled amf:in-progress:
//   - with an open PR from amf/issue-N, the issue is relabeled amf:pr-open
//     (and loses amf:ready) and skipped;
//   - with an amf/issue-N branch on origin but no open PR, it is skipped and
//     left as it is: delete the branch, or run it with --force, to retry;
//     an amf:failed issue labeled amf:ready again is retried, though.
//
// --force (Force) skips the guard, and amf then updates the open PR.
func (r *Runner) guard(ctx context.Context, issue Issue) error {
	if r.Force {
		return nil
	}
	n, branch := issue.Number, issueBranch(issue.Number)
	// ls-remote costs no API call, and without the branch no PR can be open
	// from it (GitHub closes a PR whose head branch is deleted).
	out, err := git(ctx, r.Root, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	pr, open, err := r.GH.OpenPR(ctx, branch)
	if err != nil {
		return err
	}
	if open {
		if statusOf(issue) != labelPROpen || issue.HasLabel(labelReady) {
			if err := r.setStatus(ctx, n, labelPROpen); err != nil {
				return err
			}
			r.statusEvent(n, statusOf(issue), labelPROpen)
			if issue.HasLabel(labelReady) {
				if err := r.GH.RemoveLabel(ctx, n, labelReady); err != nil {
					return err
				}
			}
		}
		msg := fmt.Sprintf("#%d already has an open pull request, %s; not starting it again (amf run %d --force updates it)", n, pr.HTMLURL, n)
		r.event(slog.LevelInfo, "issue.skip", n, msg, "reason", "pr_open", "pr", pr.Number, "url", pr.HTMLURL)
		return &skipError{n, "pr_open", msg}
	}
	// A failed issue labeled amf:ready again is an explicit retry, which
	// resets the branch its failed run left.
	if issue.HasLabel(labelFailed) {
		return nil
	}
	msg := fmt.Sprintf("#%d: branch %s already exists on origin, without an open pull request; delete it, or use amf run %d --force, to retry", n, branch, n)
	r.event(slog.LevelInfo, "issue.skip", n, msg, "reason", "branch_exists", "branch", branch)
	return &skipError{n, "branch_exists", msg}
}

// defaultStaleTimeout is how long an issue may stay amf:in-progress with no
// amf working on it when neither AMF_STALE_TIMEOUT nor --stale-timeout says:
// twice the agent timeout, and at least two hours.
func defaultStaleTimeout(agentTimeout time.Duration) time.Duration {
	return max(2*agentTimeout, 2*time.Hour)
}

// parseStaleTimeout reads AMF_STALE_TIMEOUT or --stale-timeout (named src):
// "" means the default, "0" or "off" turns the sweep off.
func parseStaleTimeout(v, src string, agentTimeout time.Duration) (time.Duration, error) {
	switch v {
	case "":
		return defaultStaleTimeout(agentTimeout), nil
	case "0", "off", "none":
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s=%q: not a duration (use e.g. 3h, or off)", src, v)
	}
	if d > 0 && d < time.Minute {
		return 0, fmt.Errorf("%s: the stale timeout must be at least 1m, got %s", src, d)
	}
	return d, nil
}

// sweepStale puts back to amf:ready each amf:in-progress issue that this
// process isn't working on and that has been in progress longer than
// StaleTimeout, as after an amf that crashed or was killed. The time is
// when the label was added, from the issue's events, else the issue's
// updated_at; without either, the issue is left alone. An issue whose PR is
// open becomes amf:pr-open instead.
func (r *Runner) sweepStale(ctx context.Context) {
	if r.StaleTimeout <= 0 || ctx.Err() != nil {
		return
	}
	issues, err := r.GH.ListIssues(ctx, "open", labelInProgress, true)
	if err != nil {
		if ctx.Err() == nil {
			r.event(slog.LevelWarn, "watch.stale", 0, fmt.Sprintf("listing %s issues: %v", labelInProgress, err), "error", err)
		}
		return
	}
	for _, is := range issues {
		n := is.Number
		if n == r.working || ctx.Err() != nil {
			continue
		}
		since, err := r.GH.LabeledAt(ctx, n, labelInProgress)
		if err != nil || since.IsZero() {
			since = is.UpdatedAt
		}
		if since.IsZero() || time.Since(since) < r.StaleTimeout {
			continue
		}
		age := time.Since(since).Round(time.Minute)
		if _, open, err := r.GH.OpenPR(ctx, issueBranch(n)); err == nil && open {
			if err := r.setStatus(ctx, n, labelPROpen); err != nil {
				r.event(slog.LevelWarn, "watch.stale", n, fmt.Sprintf("#%d: could not set %s: %v", n, labelPROpen, err), "error", err)
				continue
			}
			r.event(slog.LevelInfo, "watch.stale", n, fmt.Sprintf("#%d: %s for %s with an open pull request; now %s", n, labelInProgress, age, labelPROpen),
				"to", labelPROpen, "age", age.String())
			continue
		}
		if err := r.GH.RemoveLabel(ctx, n, labelInProgress); err != nil {
			r.event(slog.LevelWarn, "watch.stale", n, fmt.Sprintf("#%d: could not remove %s: %v", n, labelInProgress, err), "error", err)
			continue
		}
		if err := r.GH.AddLabels(ctx, n, labelReady); err != nil {
			r.event(slog.LevelWarn, "watch.stale", n, fmt.Sprintf("#%d: could not add %s: %v", n, labelReady, err), "error", err)
			continue
		}
		r.event(slog.LevelInfo, "watch.stale", n, fmt.Sprintf("#%d: %s for %s with no amf working on it; back to %s", n, labelInProgress, age, labelReady),
			"to", labelReady, "age", age.String())
		r.comment(ctx, n, fmt.Sprintf("This issue was `%s` for %s with no amf working on it (the run was probably interrupted), so amf put it back to `%s` to retry it.",
			labelInProgress, age, labelReady))
	}
}

// agentAttempt is one agent run of a build without a pipeline.
type agentAttempt struct {
	Agent    string
	Exit     string // see buildOne
	Duration time.Duration
	Log      string
	Err      error
}

// exitOf is how an agent's run ended, from runAgent's error.
func exitOf(err error) string {
	var ee *exec.ExitError
	switch {
	case errors.As(err, new(*agentTimeoutError)):
		return "timed out"
	case errors.As(err, &ee):
		return fmt.Sprint(ee.ExitCode())
	}
	return ""
}

// tailLines is how many lines of the last agent's log the failure comment
// quotes, and tailBytes the most it quotes.
const (
	tailLines = 40
	tailBytes = 6000
)

// attemptReport is the failure comment's account of the agents amf ran:
// each in order with how it exited and how long it took, whether the
// fallback ran and if not why, and the redacted end of the last log.
func (r *Runner) attemptReport() string {
	if len(r.attempts) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n**Agents tried:**\n\n")
	for i, a := range r.attempts {
		exit := a.Exit
		switch exit {
		case "":
			exit = "did not finish"
		case "timed out":
		default:
			exit = "exit code " + exit
		}
		fmt.Fprintf(&b, "%d. `%s`: %s after %s (log: `%s`)\n", i+1, a.Agent, exit, a.Duration.Round(time.Second), relLog(r.Root, a.Log))
	}
	if len(r.attempts) > 1 {
		fmt.Fprintf(&b, "\nFallback: ran; amf fell back to `%s`.\n", r.attempts[len(r.attempts)-1].Agent)
	} else {
		why := cmpOr(r.noFallback, "no fallback agent is configured")
		fmt.Fprintf(&b, "\nFallback: did not run, because %s.\n", why)
	}
	last := r.attempts[len(r.attempts)-1]
	if tail := r.logTail(last.Log); tail != "" {
		fence := "```"
		for strings.Contains(tail, fence) {
			fence += "`"
		}
		fmt.Fprintf(&b, "\nLast lines of `%s`:\n\n%stext\n%s\n%s\n", relLog(r.Root, last.Log), fence, tail, fence)
	}
	return b.String()
}

// logTail is the last tailLines lines of the log at path, at most
// tailBytes, redacted: with the configured secrets, or at least the
// well-known token formats.
func (r *Runner) logTail(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	red := r.Redact
	if red == nil {
		red = newRedactor()
	}
	lines := strings.Split(strings.TrimRight(red.Redact(string(data)), "\n"), "\n")
	if len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
	}
	tail := strings.Join(lines, "\n")
	if len(tail) > tailBytes {
		tail = tail[len(tail)-tailBytes:]
		if i := strings.IndexByte(tail, '\n'); i >= 0 {
			tail = tail[i+1:]
		}
	}
	return strings.TrimSpace(tail)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// maxFailedSweep caps how many amf:failed issues one watch poll checks, to
// keep the API calls each poll makes modest.
const maxFailedSweep = 5

// sweepFailed relabels amf:pr-open each open amf:failed issue whose
// amf/issue-N branch has an open pull request with passing checks: a later
// attempt (or a person) fixed what the failed run left, so amf:failed is
// stale. Issues also labeled amf:ready are left to the retry.
func (r *Runner) sweepFailed(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	issues, err := r.GH.ListIssues(ctx, "open", labelFailed, true)
	if err != nil {
		if ctx.Err() == nil {
			r.event(slog.LevelDebug, "watch.failed", 0, fmt.Sprintf("listing %s issues: %v", labelFailed, err), "error", err)
		}
		return
	}
	checked := 0
	for _, is := range issues {
		if ctx.Err() != nil || checked >= maxFailedSweep {
			return
		}
		n := is.Number
		if n == r.working || is.PullRequest != nil || is.HasLabel(labelReady) || is.HasLabel(labelInProgress) {
			continue
		}
		checked++
		pr, open, err := r.GH.OpenPR(ctx, issueBranch(n))
		if err != nil || !open {
			continue
		}
		d, err := r.GH.GetPR(ctx, pr.Number)
		if err != nil || d.Head.SHA == "" || d.Draft {
			continue
		}
		runs, err := r.GH.CheckRuns(ctx, d.Head.SHA)
		if err != nil || checksState(runs) != "passed" {
			continue
		}
		if err := r.setStatus(ctx, n, labelPROpen); err != nil {
			r.event(slog.LevelWarn, "watch.failed", n, fmt.Sprintf("#%d: could not set %s: %v", n, labelPROpen, err), "error", err)
			continue
		}
		r.statusEvent(n, labelFailed, labelPROpen)
		r.event(slog.LevelInfo, "watch.failed", n, fmt.Sprintf("#%d: %s, but %s is open with passing checks; now %s", n, labelFailed, pr.HTMLURL, labelPROpen),
			"to", labelPROpen, "pr", pr.Number)
	}
}
