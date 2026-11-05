package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"slices"
	"strings"
)

// sweepOptions say what amf pr sweep (and watch's sweep step) may do.
type sweepOptions struct {
	Method string // merge, squash or rebase; "" leaves PRs unmerged
	Wait   bool   // wait for pending checks instead of leaving the PR for later
	DryRun bool   // only read from GitHub
}

// sweptPR is what the sweep did with one pull request; it is also the JSON
// schema of amf pr sweep --json.
type sweptPR struct {
	Number int    `json:"number"`
	Issue  int    `json:"issue"`
	URL    string `json:"url"`
	// Action is merged, would-merge, updated, would-update, waiting,
	// ready (mergeable, but merging is off) or blocked.
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
	Commit string `json:"commit,omitempty"`
}

// sweepMarker starts the hidden key amf puts in each sweep comment, so the
// same status is not posted twice for the same head.
const sweepMarker = "<!-- amf:sweep "

// sweepPRs works through the open pull requests amf opened (an amf/issue-N
// head in this repository whose issue is labeled amf:pr-open), oldest
// first: it updates a branch that is behind its base, reads the head's
// checks and, with opt.Method set, merges a PR whose checks passed and
// labels its issue amf:done. A PR that can't merge gets one status comment
// per head and reason. Drafts are left alone. It costs two list calls plus
// a few reads per PR; writes happen only when there is something to do.
func sweepPRs(ctx context.Context, gh *GitHub, repo string, opt sweepOptions, stdout, stderr io.Writer) ([]sweptPR, error) {
	issues, err := gh.ListIssues(ctx, "open", labelPROpen, true)
	if err != nil {
		return nil, fmt.Errorf("listing %s issues: %w", labelPROpen, err)
	}
	prOpen := map[int]bool{}
	for _, is := range issues {
		if is.PullRequest == nil {
			prOpen[is.Number] = true
		}
	}
	if len(prOpen) == 0 {
		return []sweptPR{}, nil
	}
	prs, err := gh.ListPRs(ctx, "open")
	if err != nil {
		return nil, fmt.Errorf("listing pull requests: %w", err)
	}
	slices.Reverse(prs) // oldest first
	out := []sweptPR{}
	for _, p := range prs {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		issue := issueFromHead(p.Head.Ref)
		if issue == nil || !prOpen[*issue] || p.Draft || p.Head.Repo == nil || !strings.EqualFold(p.Head.Repo.FullName, repo) {
			continue
		}
		res := sweepOne(ctx, gh, repo, p.Number, *issue, opt, stdout, stderr)
		res.URL = cmp.Or(res.URL, p.HTMLURL)
		out = append(out, res)
	}
	return out, nil
}

func sweepOne(ctx context.Context, gh *GitHub, repo string, n, issue int, opt sweepOptions, stdout, stderr io.Writer) sweptPR {
	res := sweptPR{Number: n, Issue: issue}
	pr, err := gh.GetPR(ctx, n)
	if err != nil {
		res.Action, res.Reason = "blocked", fmt.Sprintf("could not read the pull request: %v", err)
		return res
	}
	res.URL = pr.HTMLURL
	if pr.Merged || pr.State != "open" || pr.Head.SHA == "" {
		res.Action, res.Reason = "blocked", "GitHub no longer reports it open"
		return res
	}
	block := func(reason string) sweptPR {
		res.Action, res.Reason = "blocked", reason
		if !opt.DryRun {
			sweepComment(ctx, gh, n, pr.Head.SHA, reason, stderr)
		}
		return res
	}
	_, behind, err := gh.Compare(ctx, pr.Base.Ref, pr.Head.SHA)
	if err != nil {
		res.Action, res.Reason = "blocked", fmt.Sprintf("could not compare with %s: %v", pr.Base.Ref, err)
		return res
	}
	if behind > 0 {
		if opt.DryRun {
			if pr.MergeableState == "dirty" || (pr.Mergeable != nil && !*pr.Mergeable) {
				res.Action, res.Reason = "blocked", fmt.Sprintf("conflict: %s is %d %s behind %s; run amf run %d --force to have an agent resolve the conflicts",
					issueBranch(issue), behind, plural(behind, "commit", "commits"), pr.Base.Ref, issue)
				return res
			}
			res.Action, res.Reason = "would-update", fmt.Sprintf("%d %s behind %s", behind, plural(behind, "commit", "commits"), pr.Base.Ref)
			return res
		}
		next, err := updatePRBranch(ctx, gh, n, pr, io.Discard)
		if err != nil {
			return block(fmt.Sprintf("%s is %d %s behind %s and GitHub could not merge it in (%v); run amf run %d --force to have an agent resolve the conflicts",
				issueBranch(issue), behind, plural(behind, "commit", "commits"), pr.Base.Ref, err, issue))
		}
		pr = next
		if !opt.Wait {
			res.Action, res.Reason = "updated", fmt.Sprintf("merged %s into it (%d %s behind); its checks run on the new head", pr.Base.Ref, behind, plural(behind, "commit", "commits"))
			return res
		}
		fmt.Fprintf(stdout, "Updated #%d with %s\n", n, pr.Base.Ref)
	}
	runs, err := gh.CheckRuns(ctx, pr.Head.SHA)
	if err != nil {
		res.Action, res.Reason = "blocked", fmt.Sprintf("could not read checks: %v", err)
		return res
	}
	state := checksState(runs)
	if state == "pending" && opt.Wait && !opt.DryRun {
		if err := waitPRChecks(ctx, gh, n, pr.Head.SHA, io.Discard); err != nil {
			if ctx.Err() != nil {
				res.Action, res.Reason = "waiting", "interrupted"
				return res
			}
			return block(err.Error())
		}
		state = "passed"
	}
	switch state {
	case "pending":
		res.Action, res.Reason = "waiting", "checks pending ("+summarizeCheckRuns(runs)+")"
		return res
	case "failed":
		return block("checks failed (" + summarizeCheckRuns(runs) + "); not merging")
	}
	if opt.Method == "" {
		res.Action, res.Reason = "ready", "up to date with passing checks; merging is off"
		return res
	}
	files, err := gh.PRFiles(ctx, n)
	if err != nil {
		res.Action, res.Reason = "blocked", fmt.Sprintf("could not list changed files: %v", err)
		return res
	}
	if slices.Contains(files, "AGENTS.md") {
		return block("the change edits AGENTS.md, which declares amf's pipeline, so amf leaves it for review")
	}
	if opt.DryRun {
		res.Action, res.Reason = "would-merge", opt.Method
		return res
	}
	commit, _, err := gh.mergePRWithRetry(ctx, n, opt.Method, pr.Head.SHA, mergeRetry, nil)
	if err != nil {
		return block(fmt.Sprintf("GitHub refused the merge (%s): %v", opt.Method, err))
	}
	res.Action, res.Commit = "merged", commit
	if after, err := gh.GetPR(ctx, n); err == nil && !after.Merged {
		res.Action, res.Reason = "blocked", "GitHub accepted the merge but does not report the pull request merged"
		return res
	}
	markMergedIssue(ctx, gh, repo, pr, io.Discard, stderr)
	return res
}

// sweepComment posts reason on pull request n unless a sweep comment with
// the same reason is already there for this head.
func sweepComment(ctx context.Context, gh *GitHub, n int, sha, reason string, stderr io.Writer) {
	sum := sha256.Sum256([]byte(sha + "\x00" + reason))
	key := sweepMarker + hex.EncodeToString(sum[:8]) + " -->"
	bodies, err := gh.CommentBodies(ctx, n)
	if err != nil {
		fmt.Fprintf(stderr, "amf: #%d: could not read comments: %v\n", n, err)
		return
	}
	for _, b := range bodies {
		if strings.Contains(b, key) {
			return
		}
	}
	msg := fmt.Sprintf("amf pr sweep could not merge this pull request at %s: %s.\n\n%s", shortSHA(sha), reason, key)
	if err := gh.Comment(ctx, n, msg); err != nil {
		fmt.Fprintf(stderr, "amf: #%d: could not comment: %v\n", n, err)
	}
}

// CommentBodies returns the bodies of every comment on issue or PR n.
func (g *GitHub) CommentBodies(ctx context.Context, n int) ([]string, error) {
	var bodies []string
	q := url.Values{"per_page": {"100"}}
	for path := g.repoPath("/issues/%d/comments?%s", n, q.Encode()); path != ""; {
		var page []struct {
			Body string `json:"body"`
		}
		header, err := g.send(ctx, "GET", path, nil, &page)
		if err != nil {
			return nil, err
		}
		for _, c := range page {
			bodies = append(bodies, c.Body)
		}
		if path, err = g.nextPage(header); err != nil {
			return nil, err
		}
	}
	return bodies, nil
}

type prSweepFlags struct {
	merge  *mergeFlag
	wait   *bool
	dryRun *bool
	asJSON *bool
}

func addPRSweepFlags(fs *flag.FlagSet) prSweepFlags {
	var f prSweepFlags
	f.merge = addSweepMergeFlag(fs)
	f.wait = fs.Bool("wait-for-checks", false, "wait for pending checks on each PR (up to 30 minutes) instead of leaving it for the next sweep")
	f.dryRun = fs.Bool("dry-run", false, "only read from GitHub and print what the sweep would do")
	f.asJSON = fs.Bool("json", false, "print a JSON array of number, issue, url, action, reason and commit")
	return f
}

func cmdPRSweep(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("pr sweep", stderr)
	f := addPRSweepFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if err := f.merge.check(pos); err != nil {
		return err
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: amf pr sweep [--merge[=method]] [--wait-for-checks] [--dry-run] [--json]")
		return errUsage
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	if !*f.dryRun {
		// In a checkout, hold the repo lock so a running watch, run or auto
		// and this sweep don't both work on one PR.
		if root, err := git(ctx, ".", "rev-parse", "--show-toplevel"); err == nil {
			l, err := acquireLock(root, "pr sweep")
			if err != nil {
				return err
			}
			defer l.release()
		}
	}
	res, err := sweepPRs(ctx, gh, cfg.Repo, sweepOptions{Method: f.merge.value, Wait: *f.wait, DryRun: *f.dryRun}, stdout, stderr)
	if err != nil {
		return err
	}
	if *f.asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return err
		}
	} else if len(res) == 0 {
		if _, err := fmt.Fprintln(stdout, "No open amf pull requests to sweep."); err != nil {
			return err
		}
	} else {
		for _, r := range res {
			line := fmt.Sprintf("#%d (issue #%d) %s", r.Number, r.Issue, r.Action)
			if r.Commit != "" {
				line += " " + shortSHA(r.Commit)
			}
			if r.Reason != "" {
				line += ": " + r.Reason
			}
			fmt.Fprintf(stdout, "%s %s\n", line, r.URL)
		}
	}
	return sweepBlockedError(res)
}

// sweepBlockedError reports a sweep that could only block: at least one PR is
// blocked and none was merged or updated. A dry run's would-merge and
// would-update count as progress so --dry-run keeps the same exit status.
func sweepBlockedError(res []sweptPR) error {
	blocked := 0
	for _, r := range res {
		switch r.Action {
		case "blocked":
			blocked++
		case "merged", "updated", "would-merge", "would-update":
			return nil
		}
	}
	if blocked == 0 {
		return nil
	}
	return fmt.Errorf("%d %s blocked; no pull request was merged or updated", blocked, plural(blocked, "pull request is", "pull requests are"))
}

// addSweepMergeFlag is --merge for amf pr sweep: the same values as on run
// and watch, but it merges the PRs the sweep finds ready.
func addSweepMergeFlag(fs *flag.FlagSet) *mergeFlag {
	m := &mergeFlag{}
	fs.Var(m, "merge", "merge each ready PR: --merge (or -m) alone for a merge commit, or --merge=squash, -m squash, -m rebase (default: don't merge)")
	return m
}

// sweepPRs is watch's sweep step: with --merge set, each poll updates,
// merges and labels the amf PRs that are ready, without waiting on checks.
func (r *Runner) sweepPRs(ctx context.Context) {
	if r.Merge == "" || ctx.Err() != nil {
		return
	}
	res, err := sweepPRs(ctx, r.GH, r.Repo, sweepOptions{Method: r.Merge}, io.Discard, io.Discard)
	if err != nil {
		if ctx.Err() == nil {
			r.event(slog.LevelDebug, "watch.sweep", 0, fmt.Sprintf("sweeping pull requests: %v", err), "error", err)
		}
		return
	}
	for _, s := range res {
		level := slog.LevelInfo
		switch s.Action {
		case "waiting":
			level = slog.LevelDebug
		case "blocked":
			level = slog.LevelWarn
		}
		text := fmt.Sprintf("#%d: PR #%d %s", s.Issue, s.Number, s.Action)
		if s.Reason != "" {
			text += ": " + s.Reason
		}
		r.event(level, "watch.sweep", s.Issue, text, "pr", s.Number, "action", s.Action, "reason", s.Reason, "commit", s.Commit)
	}
}
