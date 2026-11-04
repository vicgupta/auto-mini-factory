package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

// stringList is a flag that may be given more than once.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}

// cmdIssueLabel adds and removes labels on one issue, for fixing amf's
// status labels (or any other) by hand.
func cmdIssueLabel(ctx context.Context, command string, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("issue "+command, stderr)
	var add, remove stringList
	if command == "label" {
		fs.Var(&add, "add", "add this label (repeatable, or comma-separated)")
		fs.Var(&remove, "remove", "remove this label (repeatable, or comma-separated)")
	}
	// Keep every label after -- positional; parseArgs otherwise reparses
	// later names as flags. Skip flag values when looking for the separator.
	args = expandShortFlags(fs, args)
	var tail []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			tail, args = args[i+1:], args[:i]
			break
		}
		if args[i] == "--add" || args[i] == "--remove" {
			i++
		}
	}
	pos, err := parseArgs(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return errUsage
	}
	pos = append(pos, tail...)
	if len(pos) == 0 {
		fs.Usage()
		return errUsage
	}
	for _, l := range pos[1:] {
		if command == "unlabel" {
			_ = remove.Set(l)
		} else {
			_ = add.Set(l)
		}
	}
	if len(add)+len(remove) == 0 {
		fs.Usage()
		return errUsage
	}
	n, err := parsePRNumber(pos[0])
	if err != nil {
		return err
	}
	for _, l := range add {
		if slices.Contains(remove, l) {
			return fmt.Errorf("%s is both added and removed", l)
		}
	}
	var statuses []string
	for _, l := range add {
		if slices.Contains(statusLabels, l) && !slices.Contains(statuses, l) {
			statuses = append(statuses, l)
		}
	}
	if len(statuses) > 1 {
		return fmt.Errorf("an issue has one amf status label at a time; can't add %s", strings.Join(statuses, " and "))
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	is, err := gh.GetIssue(ctx, n)
	if err != nil {
		return err
	}
	if is.PullRequest != nil {
		return fmt.Errorf("#%d is a pull request, not an issue", n)
	}
	// Adding a status label replaces the issue's other status label.
	if len(statuses) == 1 {
		for _, l := range statusLabels {
			if l != statuses[0] && is.HasLabel(l) && !slices.Contains(remove, l) {
				remove = append(remove, l)
			}
		}
	}
	var added, removed []string
	for _, l := range remove {
		if !is.HasLabel(l) || slices.Contains(removed, l) {
			continue
		}
		if err := gh.RemoveLabel(ctx, n, l); err != nil {
			return err
		}
		removed = append(removed, l)
	}
	var toAdd []string
	for _, l := range add {
		if !is.HasLabel(l) && !slices.Contains(toAdd, l) {
			toAdd = append(toAdd, l)
		}
	}
	if len(toAdd) > 0 {
		if err := gh.AddLabels(ctx, n, toAdd...); err != nil {
			return err
		}
		added = toAdd
	}
	if len(added)+len(removed) == 0 {
		fmt.Fprintf(stdout, "#%d unchanged\n", n)
		return nil
	}
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added "+strings.Join(added, ", "))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+strings.Join(removed, ", "))
	}
	fmt.Fprintf(stdout, "#%d: %s\n", n, strings.Join(parts, "; "))
	return nil
}

// labelFix is one issue whose amf status label disagrees with its
// amf/issue-N pull requests, and the label it should have.
type labelFix struct {
	Number   int
	From, To string
	Why      string
}

// labelProblems compares each issue carrying an amf status label with the
// pull requests from its amf/issue-N branch in this repository:
//   - a merged PR makes it amf:done (from amf:failed, amf:pr-open, or two
//     status labels);
//   - amf:done without a merged PR is amf:pr-open with an open PR, else
//     amf:failed;
//   - amf:pr-open without an open or merged PR is amf:failed.
//
// amf:in-progress issues are left to watch's stale sweep, and amf:failed
// with an open PR to watch, which waits for its checks. It costs one paged
// pull request listing and one paged issue listing per status label.
func labelProblems(ctx context.Context, gh *GitHub) ([]labelFix, int, error) {
	prs, err := gh.ListPRs(ctx, "all")
	if err != nil {
		return nil, 0, err
	}
	open, merged := map[int]bool{}, map[int]bool{}
	for _, pr := range prs {
		n := issueFromHead(pr.Head.Ref)
		if n == nil || pr.Head.Repo == nil || !strings.EqualFold(pr.Head.Repo.FullName, gh.repo) {
			continue
		}
		switch {
		case pr.MergedAt != nil:
			merged[*n] = true
		case pr.State == "open":
			open[*n] = true
		}
	}
	seen := map[int]bool{}
	var fixes []labelFix
	for _, label := range []string{labelDone, labelFailed, labelPROpen} {
		issues, err := gh.ListIssues(ctx, "all", label, true)
		if err != nil {
			return nil, 0, err
		}
		for _, is := range issues {
			if seen[is.Number] {
				continue
			}
			seen[is.Number] = true
			if is.HasLabel(labelInProgress) {
				continue
			}
			var have []string
			for _, l := range statusLabels {
				if is.HasLabel(l) {
					have = append(have, l)
				}
			}
			n, from := is.Number, strings.Join(have, "+")
			branch := issueBranch(n)
			var to, why string
			switch {
			case merged[n]:
				to, why = labelDone, "its PR from "+branch+" is merged"
			case is.HasLabel(labelDone) && open[n]:
				to, why = labelPROpen, "its PR from "+branch+" is open, not merged"
			case is.HasLabel(labelDone):
				to, why = labelFailed, "no merged PR from "+branch
			case is.HasLabel(labelPROpen) && !open[n] && !is.HasLabel(labelFailed):
				to, why = labelFailed, "no open or merged PR from "+branch
			case len(have) > 1:
				to, why = statusOf(is), "it has more than one status label"
			}
			if to != "" && (len(have) != 1 || have[0] != to) {
				fixes = append(fixes, labelFix{n, from, to, why})
			}
		}
	}
	slices.SortFunc(fixes, func(a, b labelFix) int { return a.Number - b.Number })
	return fixes, len(seen), nil
}

// checkLabelState is doctor's "label state" check; with fix it relabels
// each issue labelProblems finds.
func (d *doctor) checkLabelState(ctx context.Context, gh *GitHub, fix bool) {
	fixes, checked, err := labelProblems(ctx, gh)
	if err != nil {
		d.line("warn", "label state", "could not check: %v", err)
		return
	}
	if len(fixes) == 0 {
		d.line("ok", "label state", "%d amf %s match their pull requests", checked, plural(checked, "issue's label", "issues' labels"))
		return
	}
	for _, f := range fixes {
		if !fix {
			d.line("warn", "label state", "#%d is %s but %s; should be %s (amf doctor --fix-labels fixes it)", f.Number, f.From, f.Why, f.To)
			continue
		}
		if _, err := gh.SetIssueStatus(ctx, f.Number, f.To); err != nil {
			d.line("FAIL", "label state", "#%d: could not relabel %s: %v", f.Number, f.To, err)
			continue
		}
		d.line("ok", "label state", "#%d relabeled %s → %s: %s", f.Number, f.From, f.To, f.Why)
	}
}
