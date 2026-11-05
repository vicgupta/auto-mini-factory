package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"slices"
	"strconv"
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

// cmdIssueLabel adds and removes labels on one issue or a filtered set.
func cmdIssueLabel(ctx context.Context, command string, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("issue "+command, stderr)
	state := fs.String("state", "open", "select issues by state: open, closed or all (bulk mode)")
	label := fs.String("label", "", "select issues with this label (bulk mode)")
	unlabeled := fs.Bool("unlabeled", false, "select only issues with no labels (bulk mode)")
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
		if args[i] == "--add" || args[i] == "--remove" || args[i] == "--state" || args[i] == "--label" {
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
	bulk := *unlabeled
	labelSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "state" || f.Name == "label" {
			bulk = true
		}
		if f.Name == "label" {
			labelSet = true
		}
	})
	*state = strings.ToLower(strings.TrimSpace(*state))
	switch *state {
	case "open", "closed", "all":
	default:
		return fmt.Errorf("--state must be open, closed or all, got %q", *state)
	}
	*label = strings.TrimSpace(*label)
	if labelSet && *label == "" {
		return fmt.Errorf("--label can't be empty")
	}
	if *unlabeled && *label != "" {
		return fmt.Errorf("use --label or --unlabeled, not both")
	}
	var n int
	if !bulk {
		if len(pos) == 0 {
			fs.Usage()
			return errUsage
		}
		n, err = parsePRNumber(pos[0])
		if err != nil {
			return err
		}
		pos = pos[1:]
	} else if len(pos) > 0 {
		if _, err := parsePRNumber(pos[0]); err == nil {
			return fmt.Errorf("use an issue number or bulk selectors, not both")
		}
	}
	for _, l := range pos {
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
	var issues []Issue
	if bulk {
		issues, err = gh.ListIssues(ctx, *state, *label, false)
		if err != nil {
			return err
		}
		if *unlabeled {
			issues = slices.DeleteFunc(issues, func(is Issue) bool { return len(is.Labels) != 0 })
		}
		if len(issues) == 0 {
			_, err := fmt.Fprintf(stdout, "No matching issues in %s.\n", cfg.Repo)
			return err
		}
	} else {
		is, err := gh.GetIssue(ctx, n)
		if err != nil {
			return err
		}
		if is.PullRequest != nil {
			return fmt.Errorf("#%d is a pull request, not an issue", n)
		}
		issues = []Issue{is}
	}
	for _, is := range issues {
		if err := updateIssueLabels(ctx, gh, is, add, remove, statuses, stdout); err != nil {
			return fmt.Errorf("#%d: %w", is.Number, err)
		}
	}
	return nil
}

func updateIssueLabels(ctx context.Context, gh *GitHub, is Issue, add, remove, statuses []string, stdout io.Writer) error {
	n := is.Number
	remove = slices.Clone(remove)
	// Adding a status label replaces the issue's other status label. Write
	// the additions first, so a failure can leave an extra status but never
	// none.
	if len(statuses) == 1 {
		for _, l := range statusLabels {
			if l != statuses[0] && is.HasLabel(l) && !slices.Contains(remove, l) {
				remove = append(remove, l)
			}
		}
	}
	var toAdd []string
	for _, l := range add {
		if !is.HasLabel(l) && !slices.Contains(toAdd, l) {
			toAdd = append(toAdd, l)
		}
	}
	var added, removed []string
	if len(toAdd) > 0 {
		if err := gh.AddLabels(ctx, n, toAdd...); err != nil {
			return err
		}
		added = toAdd
	}
	for _, l := range remove {
		if !is.HasLabel(l) || slices.Contains(removed, l) {
			continue
		}
		if err := gh.RemoveLabel(ctx, n, l); err != nil {
			return err
		}
		removed = append(removed, l)
	}
	if len(added)+len(removed) == 0 {
		_, err := fmt.Fprintf(stdout, "#%d unchanged\n", n)
		return err
	}
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added "+strings.Join(added, ", "))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+strings.Join(removed, ", "))
	}
	_, err := fmt.Fprintf(stdout, "#%d: %s\n", n, strings.Join(parts, "; "))
	return err
}

// labelFix is one issue whose amf status label disagrees with its
// amf/issue-N pull requests, and the label it should have.
type labelFix struct {
	Number   int
	From, To string
	Why      string
}

// labelProblems compares each issue carrying an amf status label with the
// pull requests in this repository that close it, from its amf/issue-N
// branch, a closing keyword in a PR body, or a timeline cross-reference:
//   - a merged PR makes it amf:done (from amf:failed, amf:pr-open, or two
//     status labels);
//   - amf:done without a merged PR is amf:pr-open with an open PR, else
//     amf:failed;
//   - amf:pr-open without an open or merged PR is amf:failed.
//
// amf:in-progress issues are left to watch's stale sweep, and amf:failed
// with an open PR to watch, which waits for its checks. It costs one paged
// pull request listing, one paged issue listing per status label, and a
// timeline read for a closed issue that listing doesn't link to a merged PR.
func labelProblems(ctx context.Context, gh *GitHub) ([]labelFix, int, error) {
	prs, err := gh.ListPRs(ctx, "all")
	if err != nil {
		return nil, 0, err
	}
	open, merged := map[int]bool{}, map[int]bool{}
	for _, pr := range prs {
		var linked []int
		if n := issueFromHead(pr.Head.Ref); n != nil && pr.Head.Repo != nil && strings.EqualFold(pr.Head.Repo.FullName, gh.repo) {
			linked = append(linked, *n)
		}
		for _, m := range closesRef.FindAllStringSubmatch(pr.Body, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				linked = append(linked, n)
			}
		}
		for _, n := range linked {
			switch {
			case pr.MergedAt != nil:
				merged[n] = true
			case pr.State == "open":
				open[n] = true
			}
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
			n := is.Number
			if !merged[n] && is.State == "closed" {
				m, err := gh.IssueMergedPR(ctx, n, is.ClosedAt)
				if err != nil {
					return nil, 0, err
				}
				merged[n] = merged[n] || m
			}
			var have []string
			for _, l := range statusLabels {
				if is.HasLabel(l) {
					have = append(have, l)
				}
			}
			from := strings.Join(have, "+")
			var to, why string
			switch {
			case merged[n]:
				to, why = labelDone, "a merged PR closes it"
			case is.HasLabel(labelDone) && open[n]:
				to, why = labelPROpen, "an open PR closes it, not merged"
			case is.HasLabel(labelDone):
				to, why = labelFailed, "no merged PR closes it"
			case is.HasLabel(labelPROpen) && !open[n] && !is.HasLabel(labelFailed):
				to, why = labelFailed, "no open or merged PR closes it"
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
