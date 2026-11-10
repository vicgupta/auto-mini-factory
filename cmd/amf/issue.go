package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
)

var issueUsage = `usage: amf issue <command>

Commands:
  create "Title" [flags]  create an issue (body from --body, --file or stdin)
  list [flags]  list issues, newest first, with their amf labels
  label [flags]  add or remove labels on an issue or filtered set
  unlabel [flags]  remove labels from an issue or filtered set
  close <N> [flags]  close an issue
  reopen <N>  reopen an issue
  view <N> [flags]  show an issue
  run <N> [flags]  process one issue into a PR: see amf help run
  watch [flags]  process amf:ready issues as they arrive: see amf help watch

For label/unlabel, selectors are --state open|closed|all, --label name,
and --unlabeled. Supply an issue number or selectors, not both.

amf issue "Title" … is a shortcut for amf issue create "Title" …
Run amf help issue <command> or amf issue <command> --help for its flags.
` + "\nExamples:\n" + helpExamples["issue"] + "\nSee also: amf help run, amf help watch, amf help pr\n"

var issueSubcommands = []string{"create", "list", "label", "unlabel", "close", "reopen", "view", "run", "watch", "help"}

// cmdIssueGroup dispatches `amf issue <command>`, keeping the old
// `amf issue "Title"` form working as a shortcut for `create`.
func cmdIssueGroup(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, err := fmt.Fprint(stdout, issueUsage)
		return err
	}
	switch args[0] {
	case "create":
		return cmdIssueCreate(ctx, args[1:], stdin, stdout, stderr)
	case "list":
		return cmdList(ctx, args[1:], stdout, stderr)
	case "label", "unlabel":
		return cmdIssueLabel(ctx, args[0], args[1:], stdout, stderr)
	case "close", "reopen":
		return cmdIssueState(ctx, args[0], args[1:], stdout, stderr)
	case "view":
		return cmdIssueView(ctx, args[1:], stdout, stderr)
	case "run":
		return cmdRun(ctx, args[1:], stderr)
	case "watch":
		return cmdWatch(ctx, args[1:], stderr)
	case "help", "-h", "--help":
		_, err := fmt.Fprint(stdout, issueUsage)
		return err
	default:
		// A lone word that is one typo from a subcommand is almost surely a
		// mistyped subcommand, not a title; don't create an issue from it.
		if len(args) == 1 {
			if sug := suggestFrom(args[0], issueSubcommands); sug != "" {
				fmt.Fprintf(stderr, "amf: unknown issue command %q\nDid you mean %q? To create an issue with that title, use: amf issue create %q\n\n", args[0], sug, args[0])
				fmt.Fprint(stderr, issueUsage)
				return errUsage
			}
		}
		// Backward compatibility: `amf issue "Title" …`.
		return cmdIssueCreate(ctx, args, stdin, stdout, stderr)
	}
}

type issueStateFlags struct {
	comment      *string
	deleteBranch *bool
}

func addIssueStateFlags(fs *flag.FlagSet, command string) issueStateFlags {
	var f issueStateFlags
	if command == "close" {
		f.comment = fs.String("comment", "", "post a comment on the issue before closing")
		f.deleteBranch = fs.Bool("delete-branch", false, "delete this repo's amf/issue-N branch after closing")
	}
	return f
}

// cmdIssueState closes or reopens an issue without changing its labels.
func cmdIssueState(ctx context.Context, command string, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("issue "+command, stderr)
	f := addIssueStateFlags(fs, command)
	pos, err := parseArgs(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return errUsage
	}
	comment, deleteBranch := "", false
	if f.comment != nil {
		comment = *f.comment
	}
	if f.deleteBranch != nil {
		deleteBranch = *f.deleteBranch
	}
	if len(pos) != 1 {
		fs.Usage()
		return errUsage
	}
	n, err := issueArg(pos)
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
	is, err := gh.GetIssue(ctx, n)
	if err != nil {
		return err
	}
	if is.PullRequest != nil {
		return fmt.Errorf("#%d is a pull request, not an issue", n)
	}
	state, verb := "closed", "Closed"
	if command == "reopen" {
		state, verb = "open", "Reopened"
	}
	if is.State == state {
		_, err := fmt.Fprintf(stdout, "#%d is already %s\n", n, state)
		return err
	}
	if comment != "" {
		if err := gh.Comment(ctx, n, configRedactor(cfg).Redact(comment)); err != nil {
			return err
		}
	}
	if command == "close" {
		err = gh.CloseIssue(ctx, n)
	} else {
		err = gh.ReopenIssue(ctx, n)
	}
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%s #%d %s\n", verb, n, is.HTMLURL); err != nil {
		return err
	}
	if deleteBranch {
		branch := issueBranch(n)
		if err := gh.DeleteBranch(ctx, branch); err != nil {
			return fmt.Errorf("closed #%d but could not delete %s: %w", n, branch, err)
		}
	}
	return nil
}

// viewedIssue extends the list schema with the issue's author and full body.
type viewedIssue struct {
	listedIssue
	Author string `json:"author"`
	Body   string `json:"body"`
}

func addIssueViewFlags(fs *flag.FlagSet) *bool {
	return fs.Bool("json", false, "print one JSON object")
}

func cmdIssueView(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("issue view", stderr)
	asJSON := addIssueViewFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return errUsage
	}
	if len(pos) != 1 {
		fs.Usage()
		return errUsage
	}
	n, err := issueArg(pos)
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
	is, err := gh.GetIssue(ctx, n)
	if err != nil {
		return err
	}
	if is.PullRequest != nil {
		return fmt.Errorf("#%d is a pull request, not an issue", n)
	}
	labels := append([]string{}, labelNames(is)...)
	if *asJSON {
		out := viewedIssue{listedIssue: listedIssue{Number: n, Title: is.Title, State: is.State, URL: is.HTMLURL, Labels: labels}, Author: is.User.Login, Body: is.Body}
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(out)
	}
	labelText := "-"
	if len(labels) > 0 {
		labelText = strings.Join(labels, ", ")
	}
	if _, err := fmt.Fprintf(stdout, "#%d %s\n%-10s %s\n%-10s %s\n%-10s %s\n%-10s %s\n", n, is.Title, "state:", is.State, "author:", is.User.Login, "labels:", labelText, "url:", is.HTMLURL); err != nil {
		return err
	}
	if body := strings.TrimRight(is.Body, "\n"); body != "" {
		_, err := fmt.Fprintf(stdout, "\n%s\n", body)
		return err
	}
	return nil
}
