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

// cmdList prints the repository's issues, newest first, with their amf labels.
func cmdList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	state := fs.String("state", "open", "open, closed or all")
	label := fs.String("label", "", "only issues with this label, e.g. amf:ready")
	limit := fs.Int("limit", 0, "print at most N issues, 0 means no limit")
	asJSON := fs.Bool("json", false, "print a JSON array of number, title, state, url and labels")
	pos, err := parseArgs(fs, args)
	if err == flag.ErrHelp {
		return nil
	}
	if err != nil {
		return errUsage
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: amf list [--state open|closed|all] [--label L] [--limit N] [--json]")
		return errUsage
	}
	if *limit < 0 {
		return fmt.Errorf("--limit must be 0 or more, got %d", *limit)
	}
	*state = strings.TrimSpace(*state)
	switch *state {
	case "open", "closed", "all":
	default:
		return fmt.Errorf("--state must be open, closed or all, got %q", *state)
	}
	*label = strings.TrimSpace(*label)

	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	issues, err := gh.ListIssues(ctx, *state, *label, false)
	if err != nil {
		return err
	}
	if *limit > 0 && len(issues) > *limit {
		issues = issues[:*limit]
	}

	if *asJSON {
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
		which := *state + " issues"
		if *state == "all" {
			which = "issues"
		}
		if *label != "" {
			which += " labeled " + *label
		}
		_, err := fmt.Fprintf(stdout, "No %s in %s.\n", which, cfg.Repo)
		return err
	}

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	if *state == "all" {
		fmt.Fprintln(tw, "ISSUE\tSTATE\tAMF\tTITLE")
	} else {
		fmt.Fprintln(tw, "ISSUE\tAMF\tTITLE")
	}
	for _, is := range issues {
		title := strings.ReplaceAll(is.Title, "\t", " ")
		if *state == "all" {
			fmt.Fprintf(tw, "#%d\t%s\t%s\t%s\n", is.Number, is.State, amfLabelNames(is), title)
		} else {
			fmt.Fprintf(tw, "#%d\t%s\t%s\n", is.Number, amfLabelNames(is), title)
		}
	}
	return tw.Flush()
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
