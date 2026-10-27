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
  list [--state open|closed|merged|all] [--limit N] [--issue N] [--json]  list pull requests, newest first
  merge <N> [--squash | --rebase] [--delete-branch]  merge a pull request
  view <N> [--json]  show a pull request
  diff <N> [--name-only]  show a pull request's diff, or one changed path per line
  close <N> [--comment text] [--delete-branch]  close a pull request
  reopen <N>                                      reopen a pull request
  ready <N>                                       mark a draft pull request ready for review
  draft <N>                                       convert a pull request to a draft
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
	case "diff":
		return cmdPRDiff(ctx, args[1:], stdout, stderr)
	case "close":
		return cmdPRClose(ctx, args[1:], stdout, stderr)
	case "reopen":
		return cmdPRReopen(ctx, args[1:], stdout, stderr)
	case "ready":
		return cmdPRReady(ctx, args[1:], stdout, stderr)
	case "draft":
		return cmdPRDraft(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprint(stderr, prUsage)
		return errUsage
	}
}

// parsePRNumber accepts a pull request number as "42" or "#42".
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
	issue := fs.Int("issue", 0, "only pull requests opened for issue N, 0 means no filter")
	asJSON := fs.Bool("json", false, "print a JSON array of number, title, state, draft, url, head, base and issue")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: amf pr list [--state open|closed|merged|all] [--limit N] [--issue N] [--json]")
		return errUsage
	}
	if *limit < 0 {
		return fmt.Errorf("--limit must be 0 or more, got %d", *limit)
	}
	if *issue < 0 {
		return fmt.Errorf("--issue must be 0 or more, got %d", *issue)
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
	if *issue != 0 {
		kept := out[:0]
		for _, row := range out {
			if row.Issue != nil && *row.Issue == *issue {
				kept = append(kept, row)
			}
		}
		out = kept
	}
	if *limit > 0 && len(out) > *limit {
		out = out[:*limit]
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
		if *issue != 0 {
			_, err := fmt.Fprintf(stdout, "No %s for issue #%d in %s.\n", which, *issue, cfg.Repo)
			return err
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
		fmt.Fprintln(stderr, err)
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

// cmdPRDiff prints a pull request's unified diff, or one changed path per
// line with --name-only.
func cmdPRDiff(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pr diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	nameOnly := fs.Bool("name-only", false, "print one changed path per line instead of the diff")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: amf pr diff <N> [--name-only]")
		return errUsage
	}
	n, err := parsePRNumber(pos[0])
	if err != nil {
		fmt.Fprintln(stderr, "usage: amf pr diff <N> [--name-only]")
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
	if *nameOnly {
		files, err := gh.PRFiles(ctx, n)
		if err != nil {
			return err
		}
		for _, f := range files {
			if _, err := fmt.Fprintln(stdout, f); err != nil {
				return err
			}
		}
		return nil
	}
	diff, err := gh.PRDiff(ctx, n)
	if err != nil {
		return err
	}
	_, err = stdout.Write(diff)
	return err
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

// cmdPRClose closes pull request n, optionally commenting first and deleting
// its head branch after. It never touches the linked issue's labels.
func cmdPRClose(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pr close", flag.ContinueOnError)
	fs.SetOutput(stderr)
	comment := fs.String("comment", "", "post a comment on the issue before closing")
	deleteBranch := fs.Bool("delete-branch", false, "delete the head branch after closing, if it is in this repository")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: amf pr close <N> [--comment text] [--delete-branch]")
		return errUsage
	}
	n, err := parsePRNumber(pos[0])
	if err != nil {
		return err
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
	if pr.Merged {
		return fmt.Errorf("#%d is merged; it can't be closed", n)
	}
	if pr.State == "closed" {
		fmt.Fprintf(stdout, "#%d is already closed\n", n)
		return nil
	}
	if *comment != "" {
		if err := gh.Comment(ctx, n, configRedactor(cfg).Redact(*comment)); err != nil {
			return err
		}
	}
	if err := gh.SetPRState(ctx, n, "closed"); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Closed #%d %s\n", n, pr.HTMLURL)
	if *deleteBranch {
		if pr.Head.Repo == nil || !strings.EqualFold(pr.Head.Repo.FullName, cfg.Repo) {
			fmt.Fprintf(stderr, "Skipping branch deletion for #%d: head repository is a fork or is unavailable.\n", n)
			return nil
		}
		if err := gh.DeleteBranch(ctx, pr.Head.Ref); err != nil {
			return fmt.Errorf("closed #%d but could not delete %s: %w", n, pr.Head.Ref, err)
		}
	}
	return nil
}

// cmdPRReopen reopens a closed pull request n. It never touches the linked
// issue's labels.
func cmdPRReopen(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("pr reopen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: amf pr reopen <N>")
		return errUsage
	}
	n, err := parsePRNumber(pos[0])
	if err != nil {
		return err
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
	if pr.Merged {
		return fmt.Errorf("#%d is merged; it can't be reopened", n)
	}
	if pr.State == "open" {
		fmt.Fprintf(stdout, "#%d is already open\n", n)
		return nil
	}
	if err := gh.SetPRState(ctx, n, "open"); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Reopened #%d %s\n", n, pr.HTMLURL)
	return nil
}

// The GraphQL mutations that change a pull request's draft state. The REST
// API can't do this: PATCH /pulls ignores a "draft" field.
const (
	readyMutation = `mutation($id: ID!) { markPullRequestReadyForReview(input: {pullRequestId: $id}) { pullRequest { isDraft } } }`
	draftMutation = `mutation($id: ID!) { convertPullRequestToDraft(input: {pullRequestId: $id}) { pullRequest { isDraft } } }`
)

// cmdPRReady marks a draft pull request ready for review.
func cmdPRReady(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return cmdPRDraftState(ctx, args, stdout, stderr, true)
}

// cmdPRDraft converts a pull request that is ready for review into a draft.
func cmdPRDraft(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return cmdPRDraftState(ctx, args, stdout, stderr, false)
}

// cmdPRDraftState implements amf pr ready and amf pr draft. It reads the PR
// with GET /pulls/N for its node_id, draft and state, then applies the
// matching GraphQL mutation. The no-op and closed/merged cases send nothing.
func cmdPRDraftState(ctx context.Context, args []string, stdout, stderr io.Writer, ready bool) error {
	name := "draft"
	if ready {
		name = "ready"
	}
	fs := flag.NewFlagSet("pr "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 1 {
		fmt.Fprintf(stderr, "usage: amf pr %s <N>\n", name)
		return errUsage
	}
	n, err := parsePRNumber(pos[0])
	if err != nil {
		fmt.Fprintf(stderr, "usage: amf pr %s <N>\n", name)
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
	switch {
	case pr.MergedAt != nil:
		return fmt.Errorf("#%d is merged", n)
	case pr.State == "closed":
		return fmt.Errorf("#%d is closed", n)
	}
	if ready && !pr.Draft {
		fmt.Fprintf(stdout, "#%d is already ready for review\n", n)
		return nil
	}
	if !ready && pr.Draft {
		fmt.Fprintf(stdout, "#%d is already a draft\n", n)
		return nil
	}
	if pr.NodeID == "" {
		return fmt.Errorf("github did not report a node ID for #%d", n)
	}
	query := draftMutation
	if ready {
		query = readyMutation
	}
	if err := gh.graphql(ctx, query, map[string]any{"id": pr.NodeID}, nil); err != nil {
		return err
	}
	if ready {
		fmt.Fprintf(stdout, "#%d is ready for review %s\n", n, pr.HTMLURL)
	} else {
		fmt.Fprintf(stdout, "#%d is now a draft %s\n", n, pr.HTMLURL)
	}
	return nil
}
