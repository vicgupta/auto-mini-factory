package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// listedIssue is one entry of amf list --json, a schema kept apart from Issue.
type listedIssue struct {
	Number int      `json:"number"`
	Title  string   `json:"title"`
	State  string   `json:"state"`
	URL    string   `json:"url"`
	Labels []string `json:"labels"`
}

type listFlags struct {
	state  *string
	label  *string
	search *string
	limit  *int
	asJSON *bool
}

func addListFlags(fs *flag.FlagSet) listFlags {
	return listFlags{
		state:  fs.String("state", "open", "open, closed or all"),
		label:  fs.String("label", "", "only issues with this label, e.g. amf:ready"),
		search: fs.String("search", "", "only issues whose title contains this text, ignoring case"),
		limit:  fs.Int("limit", 0, "print at most N issues, 0 means no limit"),
		asJSON: fs.Bool("json", false, "print a JSON array of number, title, state, url, labels"),
	}
}

// cmdList prints the repository's issues, newest first, with their amf labels.
func cmdList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	withHelp(fs, stderr, "list")
	f := addListFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return errUsage
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: amf issue list [--state open|closed|all] [--label name] [--search text] [--limit n] [--json]")
		return errUsage
	}
	if *f.limit < 0 {
		return fmt.Errorf("--limit must be 0 or more, got %d", *f.limit)
	}
	*f.state = strings.ToLower(strings.TrimSpace(*f.state))
	switch *f.state {
	case "open", "closed", "all":
	default:
		return fmt.Errorf("--state must be open, closed or all, got %q", *f.state)
	}
	*f.label = strings.TrimSpace(*f.label)
	*f.search = strings.TrimSpace(*f.search)

	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	issues, err := gh.ListIssues(ctx, *f.state, *f.label, false)
	if err != nil {
		return err
	}
	if *f.search != "" {
		needle := strings.ToLower(*f.search)
		kept := issues[:0]
		for _, is := range issues {
			if strings.Contains(strings.ToLower(is.Title), needle) {
				kept = append(kept, is)
			}
		}
		issues = kept
	}
	matched := len(issues)
	if *f.limit > 0 && len(issues) > *f.limit {
		issues = issues[:*f.limit]
	}

	if *f.asJSON {
		out := make([]listedIssue, 0, len(issues))
		for _, is := range issues {
			labels := make([]string, 0, len(is.Labels))
			for _, l := range is.Labels {
				labels = append(labels, l.Name)
			}
			out = append(out, listedIssue{Number: is.Number, Title: is.Title, State: is.State, URL: is.HTMLURL, Labels: labels})
		}
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(out)
	}

	if len(issues) == 0 {
		which := *f.state + " issues"
		if *f.state == "all" {
			which = "issues"
		}
		if *f.label != "" {
			which += " labeled " + *f.label
		}
		if *f.search != "" {
			which += fmt.Sprintf(" with %q in the title", *f.search)
		}
		_, err := fmt.Fprintf(stdout, "No %s in %s.\n", which, cfg.Repo)
		return err
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	if *f.state == "all" {
		fmt.Fprintln(tw, "ISSUE\tSTATE\tAMF\tTITLE")
	} else {
		fmt.Fprintln(tw, "ISSUE\tAMF\tTITLE")
	}
	for _, is := range issues {
		title := strings.ReplaceAll(is.Title, "\t", " ")
		if *f.state == "all" {
			fmt.Fprintf(tw, "#%d\t%s\t%s\t%s\n", is.Number, is.State, amfLabelNames(is), title)
		} else {
			fmt.Fprintf(tw, "#%d\t%s\t%s\n", is.Number, amfLabelNames(is), title)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(issues) < matched {
		fmt.Fprintf(stderr, "Showing %d of %d issues; use --limit 0 to list them all.\n", len(issues), matched)
	}
	return nil
}

// amfLabelNames lists an issue's amf labels (amf:* and the pre-v0.0.5 "amf"), or "-".
func amfLabelNames(is Issue) string {
	var names []string
	for _, l := range is.Labels {
		if l.Name == "amf" || strings.HasPrefix(l.Name, "amf:") {
			names = append(names, l.Name)
		}
	}
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, ",")
}
