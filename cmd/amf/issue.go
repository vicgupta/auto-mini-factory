package main

import (
	"context"
	"fmt"
	"io"
)

var issueUsage = `usage: amf issue <command>

Commands:
  create "Title" [--body text | --file path] [--label name] [--no-label]  create an issue (the body may also come from stdin)
  list [--state open|closed|all] [--label name] [--search text] [--limit n] [--json]  list issues, newest first, with their amf labels
  label <N> [--add name]... [--remove name]...  add or remove an issue's labels
  run <N> [flags]  process one issue into a PR: see amf help run
  watch [flags]  process amf:ready issues as they arrive: see amf help watch

amf issue "Title" … is a shortcut for amf issue create "Title" …
Run amf help issue <command> or amf issue <command> --help for its flags.
` + "\nExamples:\n" + helpExamples["issue"]

var issueSubcommands = []string{"create", "list", "label", "run", "watch", "help"}

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
	case "label":
		return cmdIssueLabel(ctx, args[1:], stdout, stderr)
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
