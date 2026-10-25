package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
)

// dryRun is Run with --dry-run: it makes the change on the issue's local
// branch, as a real run would, and reports the PR it would open, without
// changing anything on GitHub. There is no claim, label, comment, push, PR
// or merge; amf only reads the issue, the repo and the branch's open PR. A
// failure is returned without marking the issue failed, and a timed-out
// run's worktree is removed as usual.
func (r *Runner) dryRun(ctx context.Context, issue Issue, base string, p *Pipeline, agent string) (err error) {
	n := issue.Number
	defer func() {
		if err == nil {
			return
		}
		var ate *agentTimeoutError
		if errors.As(err, &ate) {
			r.event(slog.LevelInfo, "issue.failed", n, fmt.Sprintf("issue.failed: issue=%d reason=agent_timeout", n),
				"reason", "agent_timeout", "stage", stageName(ate.Stage), "error", err, "dry_run", true)
			r.removeWorktree(context.WithoutCancel(ctx), n)
			return
		}
		reason := "error"
		if errors.As(err, new(*stageError)) {
			reason = "stage"
		}
		r.event(slog.LevelDebug, "issue.failed", n, fmt.Sprintf("#%d: failed: %v", n, err), "reason", reason, "error", err, "dry_run", true)
	}()

	// The trust check still holds: a dry run runs the issue's text too.
	trusted := r.checkAuthor(issue)
	r.event(slog.LevelDebug, "issue.author_check", n, fmt.Sprintf("#%d: opened by @%s", n, issue.User.Login),
		"author", issue.User.Login, "trusted", trusted == nil)
	if trusted != nil {
		return trusted
	}

	summary, extra, err := r.build(ctx, issue, base, p, agent)
	if err != nil {
		return err
	}
	branch, wt := issueBranch(n), r.worktreePath(n)
	head, err := git(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	changed, err := changedPaths(ctx, wt, base, "HEAD")
	if err != nil {
		return err
	}
	stat, err := git(ctx, wt, "diff", "--stat", "origin/"+base, "HEAD")
	if err != nil {
		return err
	}
	open, hasPR, err := r.GH.OpenPR(ctx, branch)
	if err != nil {
		return err
	}

	action, url, would := "open", "", fmt.Sprintf("open a pull request into %s", base)
	switch {
	case hasPR:
		action, url, would = "update", open.HTMLURL, "update "+open.HTMLURL
	case r.Draft:
		would = fmt.Sprintf("open a draft pull request into %s", base)
	}
	merge, mergeErr := "off", ""
	if r.Merge != "" {
		merge = r.Merge
		if slices.Contains(changed, "AGENTS.md") {
			merge, mergeErr = "refused", "the change edits AGENTS.md, which declares amf's pipeline, so amf leaves it for review"
			would += ", but not merge it: " + mergeErr
		} else {
			would += fmt.Sprintf(", and merge it (%s) if GitHub allows", r.Merge)
		}
	}
	title, body := prTitle(issue), r.prBody(summary, extra, n)
	r.event(slog.LevelInfo, "issue.dry_run", n, fmt.Sprintf("#%d: dry run: committed %s on the local branch %s; amf would push it and %s", n, short(head), branch, would),
		"branch", branch, "base", base, "head", head, "files", changed, "action", action, "url", url,
		"draft", r.Draft && !hasPR, "merge", merge, "merge_error", mergeErr, "title", title, "body", body)

	if _, err := git(ctx, r.Root, "worktree", "remove", wt); err != nil {
		r.event(slog.LevelWarn, "issue.cleanup", n, fmt.Sprintf("#%d: could not remove worktree: %v", n, err), "error", err)
	}
	fmt.Fprintf(r.Log, "\n%s\n\nTitle: %s\n\n%s\n\nNothing was pushed, and the issue and GitHub are unchanged. To see the change: git diff origin/%s %s\n\n",
		stat, title, body, base, branch)
	r.event(slog.LevelDebug, "issue.done", n, fmt.Sprintf("#%d: dry run done", n), "dry_run", true)
	return nil
}

// short is a commit hash cut to the 7 characters git shows.
func short(hash string) string {
	if len(hash) > 7 {
		return hash[:7]
	}
	return hash
}
