package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
)

// mergeMethods are the ways --merge can merge a PR, as GitHub names them.
var mergeMethods = []string{"merge", "squash", "rebase"}

// mergedVerb says what each method did, as GitHub's merge button does.
var mergedVerb = map[string]string{"merge": "merged", "squash": "squashed and merged", "rebase": "rebased and merged"}

// mergeRetry is how long amf waits before asking GitHub again to merge a PR
// it reports as not mergeable, and mergeAttempts how often it asks: right
// after a push GitHub may still be working out whether the PR merges.
const (
	mergeRetry    = 5 * time.Second
	mergeAttempts = 3
)

// mergeFlag is --merge for run and watch. Bare, it means a merge commit;
// --merge=squash and --merge=rebase pick another method, and --merge=false
// turns it off. check rejects anything else after parsing, so the error
// isn't the flag package's "invalid boolean value".
type mergeFlag struct{ value string }

func (m *mergeFlag) String() string   { return m.value }
func (m *mergeFlag) IsBoolFlag() bool { return true }

func (m *mergeFlag) Set(s string) error {
	switch s {
	case "true":
		m.value = "merge"
	case "false":
		m.value = ""
	default:
		m.value = s
	}
	return nil
}

func addMergeFlag(fs *flag.FlagSet) *mergeFlag {
	m := &mergeFlag{}
	fs.Var(m, "merge", "merge each PR: commit, squash or rebase")
	return m
}

// check rejects an unknown method. `--merge squash` and `-m squash` reach
// here as --merge=squash: expandShortFlags joins a following method word.
func (m *mergeFlag) check(_ []string) error {
	if m.value != "" && !slices.Contains(mergeMethods, m.value) {
		return fmt.Errorf("--merge: want merge, squash or rebase, got %q", m.value)
	}
	return nil
}

// mergeError is a PR amf opened or updated but couldn't merge. The PR stays
// open and the issue amf:pr-open, but run exits 1.
type mergeError struct {
	url string
	err error
}

func (e *mergeError) Error() string {
	return fmt.Sprintf("opened %s but could not merge it: %v", e.url, e.err)
}
func (e *mergeError) Unwrap() error { return e.err }

// mergePR merges issue n's PR with r.Merge, pinned to the commit amf pushed.
// It refuses a change to AGENTS.md: that is where the pipeline lives, and
// merging it unreviewed would let one issue's agent choose the agents and
// skills that run every later issue.
func (r *Runner) mergePR(ctx context.Context, n int, base string, pr PullRequest) error {
	r.event(slog.LevelInfo, "issue.merge", n, fmt.Sprintf("#%d: merging %s (%s)", n, pr.HTMLURL, r.Merge),
		"action", "merging", "method", r.Merge, "number", pr.Number, "sha", pr.Head)
	err := r.tryMerge(ctx, n, base, pr)
	if err != nil {
		r.event(slog.LevelWarn, "issue.merge", n, fmt.Sprintf("#%d: could not merge %s: %v", n, pr.HTMLURL, err),
			"action", "failed", "method", r.Merge, "number", pr.Number, "error", err)
	}
	return err
}

func (r *Runner) tryMerge(ctx context.Context, n int, base string, pr PullRequest) error {
	changed, err := changedPaths(ctx, r.Root, base, pr.Head)
	if err != nil {
		return err
	}
	if slices.Contains(changed, "AGENTS.md") {
		return errors.New("the change edits AGENTS.md, which declares amf's pipeline, so amf leaves it for review")
	}
	if !r.MergeSkipChecks {
		if err := r.waitMergeChecks(ctx, n, pr); err != nil {
			return err
		}
	}
	wait := cmp.Or(r.MergeRetry, mergeRetry)
	commit, attempts, err := r.GH.mergePRWithRetry(ctx, pr.Number, r.Merge, pr.Head, wait, func(attempt int, err error) {
		r.event(slog.LevelDebug, "issue.merge", n, fmt.Sprintf("#%d: %s is not mergeable yet; asking again in %s", n, pr.HTMLURL, wait),
			"action", "retry", "method", r.Merge, "number", pr.Number, "attempt", attempt, "error", err)
	})
	if err == nil {
		r.lastCommit = commit
		r.event(slog.LevelInfo, "issue.merge", n, fmt.Sprintf("#%d: %s %s", n, mergedVerb[r.Merge], pr.HTMLURL),
			"action", "merged", "method", r.Merge, "number", pr.Number, "commit", commit, "attempts", attempts)
	}
	return err
}

// mergePRWithRetry retries only GitHub's temporary mergeability refusal.
// The original head SHA is used for every attempt, including after a wait.
func (g *GitHub) mergePRWithRetry(ctx context.Context, n int, method, sha string, wait time.Duration, retry func(int, error)) (string, int, error) {
	for attempt := 1; ; attempt++ {
		commit, err := g.MergePR(ctx, n, method, sha)
		if err == nil {
			return commit, attempt, nil
		}
		var apiErr *APIError
		if attempt == mergeAttempts || !errors.As(err, &apiErr) || apiErr.Status != http.StatusMethodNotAllowed ||
			!strings.Contains(strings.ToLower(apiErr.Message), "not mergeable") {
			return "", attempt, err
		}
		if retry != nil {
			retry(attempt, err)
		}
		select {
		case <-ctx.Done():
			return "", attempt, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// waitMergeChecks waits for the check runs on the head amf pushed to pass
// before run, watch or auto merge it, as amf pr merge --wait-for-checks
// does: up to prChecksTimeout (and never past ctx, the run's own deadline).
// A head that reports no checks within prNoChecksGrace counts as a repo
// without CI and is merged; failed checks or a timeout leave the PR open.
func (r *Runner) waitMergeChecks(ctx context.Context, n int, pr PullRequest) error {
	timeout := cmp.Or(r.ChecksTimeout, prChecksTimeout)
	r.event(slog.LevelInfo, "issue.checks", n, fmt.Sprintf("#%d: waiting up to %s for checks on %s (%s)", n, timeout, pr.HTMLURL, shortSHA(pr.Head)),
		"action", "waiting", "number", pr.Number, "sha", pr.Head)
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	state, summary, err := r.GH.waitChecks(wctx, pr.Head, true, cmp.Or(r.ChecksPoll, prChecksPoll), cmp.Or(r.NoChecksGrace, prNoChecksGrace))
	switch {
	case state == "timeout" && ctx.Err() != nil:
		return ctx.Err()
	case state == "timeout":
		err = fmt.Errorf("timed out after %s waiting for checks on %s (%s); not merging", timeout, shortSHA(pr.Head), summary)
	case err != nil:
		err = fmt.Errorf("could not read checks on %s: %w; not merging", shortSHA(pr.Head), err)
	case state == "failed":
		err = fmt.Errorf("checks failed on %s (%s); not merging", shortSHA(pr.Head), summary)
	}
	if err != nil {
		r.event(slog.LevelWarn, "issue.checks", n, fmt.Sprintf("#%d: %v", n, err), "action", "failed", "number", pr.Number, "state", state, "summary", summary)
		return err
	}
	msg := fmt.Sprintf("#%d: checks passed on %s (%s)", n, shortSHA(pr.Head), summary)
	if state == "none" {
		msg = fmt.Sprintf("#%d: no checks reported on %s after %s; merging", n, shortSHA(pr.Head), cmp.Or(r.NoChecksGrace, prNoChecksGrace))
	}
	r.event(slog.LevelInfo, "issue.checks", n, msg, "action", state, "number", pr.Number, "summary", summary)
	return nil
}

const waitChecksFlagHelp = "wait up to 30m for PR checks"
