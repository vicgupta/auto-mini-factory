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
	fs.Var(m, "merge", "merge each PR amf opens or updates: --merge for a merge commit, or --merge=squash or --merge=rebase")
	return m
}

// check rejects an unknown method, and `--merge squash`, where the flag
// package takes squash as a positional argument.
func (m *mergeFlag) check(pos []string) error {
	if m.value != "" && !slices.Contains(mergeMethods, m.value) {
		return fmt.Errorf("--merge: want merge, squash or rebase, got %q", m.value)
	}
	for _, p := range pos {
		if m.value != "" && slices.Contains(mergeMethods, p) {
			return fmt.Errorf("--merge takes its method after =: --merge=%s, not --merge %s", p, p)
		}
	}
	return nil
}

// mergeError is a PR amf opened or updated but couldn't merge. The PR stays
// open and the issue amf:done, but run exits 1.
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
	for attempt := 1; ; attempt++ {
		commit, err := r.GH.MergePR(ctx, pr.Number, r.Merge, pr.Head)
		if err == nil {
			r.event(slog.LevelInfo, "issue.merge", n, fmt.Sprintf("#%d: %s %s", n, mergedVerb[r.Merge], pr.HTMLURL),
				"action", "merged", "method", r.Merge, "number", pr.Number, "commit", commit, "attempts", attempt)
			return nil
		}
		var apiErr *APIError
		if attempt == mergeAttempts || !errors.As(err, &apiErr) || apiErr.Status != http.StatusMethodNotAllowed ||
			!strings.Contains(strings.ToLower(apiErr.Message), "not mergeable") {
			return err
		}
		wait := cmp.Or(r.MergeRetry, mergeRetry)
		r.event(slog.LevelDebug, "issue.merge", n, fmt.Sprintf("#%d: %s is not mergeable yet; asking again in %s", n, pr.HTMLURL, wait),
			"action", "retry", "method", r.Merge, "number", pr.Number, "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}
