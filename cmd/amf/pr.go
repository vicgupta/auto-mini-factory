package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
)

const prUsage = `usage: amf pr <command>

Commands:
  list [--state open|closed|merged|all] [--limit N] [--json]  list pull requests, newest first
  merge <N> [--squash | --rebase] [--delete-branch]  merge a pull request
  view <N> [--json]  show a pull request
`

func cmdPR(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, err := fmt.Fprint(stdout, prUsage)
		return err
	}
	switch args[0] {
	case "help", "-h", "--help":
		_, err := fmt.Fprint(stdout, prUsage)
		return err
	case "list":
		return cmdPRList(ctx, args[1:], stdout, stderr)
	case "merge":
		return cmdPRMerge(ctx, args[1:], stdout, stderr)
	case "view":
		return cmdPRView(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprint(stderr, prUsage)
		return errUsage
	}
}

func parsePRNumber(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(s, "#"))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid pull request number %q: want a positive number", s)
	}
	return n, nil
}

// listedPR is the public JSON schema, separate from GitHub's response.
type listedPR struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Draft  bool   `json:"draft"`
	URL    string `json:"url"`
	Head   string `json:"head"`
	Base   string `json:"base"`
	Issue  *int   `json:"issue"`
}

func cmdPRList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pr list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	state := fs.String("state", "open", "open, closed, merged or all")
	limit := fs.Int("limit", 0, "print at most N pull requests, 0 means no limit")
	asJSON := fs.Bool("json", false, "print a JSON array of number, title, state, draft, url, head, base and issue")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: amf pr list [--state open|closed|merged|all] [--limit N] [--json]")
		return errUsage
	}
	if *limit < 0 {
		return fmt.Errorf("--limit must be 0 or more, got %d", *limit)
	}
	switch *state {
	case "open", "closed", "merged", "all":
	default:
		return fmt.Errorf("--state must be open, closed, merged or all, got %q", *state)
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	prs, err := gh.ListPRs(ctx, *state)
	if err != nil {
		return err
	}
	if *limit > 0 && len(prs) > *limit {
		prs = prs[:*limit]
	}
	out := make([]listedPR, 0, len(prs))
	for _, pr := range prs {
		row := listedPR{Number: pr.Number, Title: pr.Title, State: pr.displayState(), Draft: pr.Draft, URL: pr.HTMLURL, Head: pr.Head.Ref, Base: pr.Base.Ref}
		if pr.Head.Repo != nil && strings.EqualFold(pr.Head.Repo.FullName, cfg.Repo) {
			if suffix, ok := strings.CutPrefix(pr.Head.Ref, "amf/issue-"); ok {
				if n, err := strconv.Atoi(suffix); err == nil && n > 0 && strconv.Itoa(n) == suffix {
					row.Issue = &n
				}
			}
		}
		out = append(out, row)
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(out)
	}
	if len(out) == 0 {
		which := *state + " pull requests"
		if *state == "all" {
			which = "pull requests"
		}
		_, err := fmt.Fprintf(stdout, "No %s in %s.\n", which, cfg.Repo)
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PR\tISSUE\tSTATE\tTITLE")
	for _, pr := range out {
		issue := "-"
		if pr.Issue != nil {
			issue = fmt.Sprintf("#%d", *pr.Issue)
		}
		fmt.Fprintf(tw, "#%d\t%s\t%s\t%s\n", pr.Number, issue, pr.State, strings.ReplaceAll(pr.Title, "\t", " "))
	}
	return tw.Flush()
}

func cmdPRMerge(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pr merge", flag.ContinueOnError)
	fs.SetOutput(stderr)
	squash := fs.Bool("squash", false, "squash and merge")
	rebase := fs.Bool("rebase", false, "rebase and merge")
	deleteBranch := fs.Bool("delete-branch", false, "delete the head branch after merging, if it is in this repository")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if *squash && *rebase {
		return errors.New("--squash and --rebase can't be used together")
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: amf pr merge <N> [--squash | --rebase] [--delete-branch]")
		return errUsage
	}
	n, err := parsePRNumber(pos[0])
	if err != nil {
		return err
	}
	method := "merge"
	if *squash {
		method = "squash"
	}
	if *rebase {
		method = "rebase"
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	pr, err := gh.GetPR(ctx, n)
	if err != nil {
		return err
	}
	switch {
	case pr.Merged:
		return fmt.Errorf("#%d is already merged", n)
	case pr.State == "closed":
		return fmt.Errorf("#%d is closed", n)
	case pr.Draft:
		return fmt.Errorf("#%d is a draft; mark it ready for review first", n)
	}
	if pr.Head.SHA == "" {
		return fmt.Errorf("github did not report a head SHA for #%d", n)
	}
	if _, _, err := gh.mergePRWithRetry(ctx, n, method, pr.Head.SHA, mergeRetry, nil); err != nil {
		return err
	}
	verb := mergedVerb[method]
	fmt.Fprintf(stdout, "%s%s #%d %s\n", strings.ToUpper(verb[:1]), verb[1:], n, pr.HTMLURL)
	if *deleteBranch {
		if pr.Head.Repo == nil || !strings.EqualFold(pr.Head.Repo.FullName, cfg.Repo) {
			fmt.Fprintf(stderr, "Skipping branch deletion for #%d: head repository is a fork or is unavailable.\n", n)
			return nil
		}
		if err := gh.DeleteBranch(ctx, pr.Head.Ref); err != nil {
			return fmt.Errorf("merged #%d but could not delete %s: %w", n, pr.Head.Ref, err)
		}
	}
	return nil
}

// viewedPR is the public JSON schema of amf pr view, kept apart from
// GitHub's response.
type viewedPR struct {
	Number         int    `json:"number"`
	Title          string `json:"title"`
	State          string `json:"state"`
	Draft          bool   `json:"draft"`
	URL            string `json:"url"`
	Head           string `json:"head"`
	Base           string `json:"base"`
	Author         string `json:"author"`
	Issue          *int   `json:"issue"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
	Additions      int    `json:"additions"`
	Deletions      int    `json:"deletions"`
	ChangedFiles   int    `json:"changed_files"`
	Body           string `json:"body"`
}

func cmdPRView(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pr view", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print one JSON object")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: amf pr view <number> [--json]")
		return errUsage
	}
	n, err := parsePRNumber(pos[0])
	if err != nil {
		fmt.Fprintln(stderr, "usage: amf pr view <number> [--json]")
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
	pr, err := gh.GetPRView(ctx, n)
	if err != nil {
		return err
	}
	issue := issueFromHead(pr.Head.Ref)
	if *asJSON {
		out := viewedPR{
			Number:         n,
			Title:          pr.Title,
			State:          jsonState(pr),
			Draft:          pr.Draft,
			URL:            pr.HTMLURL,
			Head:           pr.Head.Ref,
			Base:           pr.Base.Ref,
			Author:         pr.User.Login,
			Issue:          issue,
			Mergeable:      pr.Mergeable,
			MergeableState: pr.MergeableState,
			Additions:      pr.Additions,
			Deletions:      pr.Deletions,
			ChangedFiles:   pr.ChangedFiles,
			Body:           pr.Body,
		}
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(out)
	}

	issueText := "-"
	if issue != nil {
		issueText = fmt.Sprintf("#%d", *issue)
	}
	files := "files"
	if pr.ChangedFiles == 1 {
		files = "file"
	}
	if _, err := fmt.Fprintf(stdout, "#%d %s\n", n, pr.Title); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%-10s %s\n", "state:", textState(pr)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%-10s %s → %s\n", "branch:", pr.Head.Ref, pr.Base.Ref); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%-10s %s\n", "author:", pr.User.Login); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%-10s %s\n", "issue:", issueText); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%-10s %s\n", "mergeable:", textMergeable(pr)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%-10s +%d −%d in %d %s\n", "changes:", pr.Additions, pr.Deletions, pr.ChangedFiles, files); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%-10s %s\n", "url:", pr.HTMLURL); err != nil {
		return err
	}
	if body := strings.TrimRight(pr.Body, "\n"); body != "" {
		if _, err := fmt.Fprintf(stdout, "\n%s\n", body); err != nil {
			return err
		}
	}
	return nil
}

// issueFromHead reads the issue number from a canonical amf/issue-N branch,
// rejecting leading zeroes and non-positive numbers.
func issueFromHead(ref string) *int {
	suffix, ok := strings.CutPrefix(ref, "amf/issue-")
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(suffix)
	if err != nil || n <= 0 || strconv.Itoa(n) != suffix {
		return nil
	}
	return &n
}

func textState(pr prViewResponse) string {
	if pr.MergedAt != nil {
		return "merged"
	}
	if pr.State == "closed" {
		return "closed"
	}
	if pr.Draft {
		return "open (draft)"
	}
	return "open"
}

// jsonState is the lifecycle state without the text-only draft marker; the
// draft field beside it says whether the pull request is a draft.
func jsonState(pr prViewResponse) string {
	if pr.MergedAt != nil {
		return "merged"
	}
	if pr.State == "closed" {
		return "closed"
	}
	return "open"
}

func textMergeable(pr prViewResponse) string {
	switch {
	case pr.Mergeable == nil || pr.MergeableState == "unknown":
		return "unknown"
	case pr.MergeableState == "dirty" || !*pr.Mergeable:
		return "no (conflicts)"
	case pr.MergeableState == "" || pr.MergeableState == "clean":
		return "yes"
	default:
		return "blocked (" + pr.MergeableState + ")"
	}
}
