package main

import (
	"context"
	"fmt"
	"io"
)

const issueUsage = `usage: amf issue <command>

Commands:
  create "Title" [-b body | -f file.md] [--label L] [--no-label]  create an issue (body may also come from stdin)
  list [--state open|closed|all] [--label L] [--limit N]           list issues, newest first, with their amf labels (also takes --json)
  run <N> [--agent A] [--agent-timeout 60m]                       process one issue into a PR (also takes --draft, --dry-run, --merge, --eval-rounds, --log-level, --log-file)
  watch [--agent A] [--interval 60s] [--label amf:ready]          process amf:ready issues as they arrive (also takes --exclude-label, --draft, --merge, --agent-timeout, --eval-rounds, --log-level, --log-file)

amf issue "Title" … is a shortcut for amf issue create "Title" …
`

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
	case "run":
		return cmdRun(ctx, args[1:], stderr)
	case "watch":
		return cmdWatch(ctx, args[1:], stderr)
	case "help", "-h", "--help":
		_, err := fmt.Fprint(stdout, issueUsage)
		return err
	default:
		// Backward compatibility: `amf issue "Title" …`.
		return cmdIssueCreate(ctx, args, stdin, stdout, stderr)
	}
}
