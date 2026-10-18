// Command amf (autonomous mini factory) turns GitHub issues into pull
// requests with Claude Code. See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// version is set at release build time with -ldflags "-X main.version=...".
var version = "dev"

// buildVersion is version, or the module version when installed with
// `go install ...@vX.Y.Z`.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

const usage = `amf turns GitHub issues into pull requests with Claude Code.

Usage:
  amf init                                                        write .env (backing up an existing one) and create amf's labels
  amf issue "Title" [-b body | -f file.md] [--no-label]           create an issue (body may also come from stdin)
  amf list [--state open|closed|all] [--label L]                  list issues, newest first, with their amf labels
  amf run <issue-number> [--timeout 30m] [--eval-rounds 2]        process one issue into a PR
  amf watch [--interval 60s] [--label amf:ready] [--timeout 30m]  process amf:ready issues as they arrive (also takes --eval-rounds)
  amf pipeline [--fetch]                                          check the plan/code/eval pipeline in this checkout's AGENTS.md
  amf update [--version vX.Y.Z] [--check]                         replace this amf binary with the latest (or given) release
  amf version

Configuration is read from .env (or the environment):
  GITHUB_TOKEN   token with contents, issues and pull-requests write access
  GITHUB_REPO    owner/name
  CLAUDE_CODE_OAUTH_TOKEN  optional subscription token for Claude Code (claude setup-token)
  AMF_CLAUDE     path to the claude binary (default "claude")
  AMF_CODEX      path to the codex binary (default "codex"), for pipelines
  AMF_OPENCODE   path to the opencode binary (default "opencode"), for pipelines
  AMF_API_URL    GitHub API base URL (default https://api.github.com)
  AMF_CLONE_URL  URL to clone GITHUB_REPO from (default https://github.com/GITHUB_REPO.git)

run and watch work in the enclosing git checkout. Started anywhere else, they
clone GITHUB_REPO into ./<name> (or reuse that clone) and work there.

A repo can declare a pipeline in its AGENTS.md: a table under "## amf" with a
plan, code and eval stage, each naming a skill URL, an agent (claudecode,
codex or opencode) and a model. See SPECS.md.
`

var errUsage = errors.New("usage")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	if errors.Is(err, errUsage) {
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "amf:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errUsage
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "init":
		return cmdInit(ctx, rest, stdout, stderr)
	case "list":
		return cmdList(ctx, rest, stdout, stderr)
	case "issue":
		return cmdIssue(ctx, rest, stdin, stdout, stderr)
	case "run":
		return cmdRun(ctx, rest, stderr)
	case "watch":
		return cmdWatch(ctx, rest, stderr)
	case "pipeline":
		return cmdPipeline(ctx, rest, stdout, stderr)
	case "update":
		return cmdUpdate(ctx, rest, stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, "amf", buildVersion())
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprintf(stderr, "amf: unknown command %q\n\n%s", cmd, usage)
		return errUsage
	}
}

func cmdIssue(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("issue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	body := fs.String("b", "", "issue body")
	file := fs.String("f", "", "read the issue body from a file (- for stdin)")
	label := fs.String("label", labelReady, "label that marks the issue for amf")
	noLabel := fs.Bool("no-label", false, "don't label the issue for amf")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
		fmt.Fprintln(stderr, `usage: amf issue "Title" [-b body | -f file.md] [--no-label]`)
		return errUsage
	}

	text := *body
	switch {
	case *body != "" && *file != "":
		return errors.New("use -b or -f, not both")
	case *file == "-":
		text, err = readAll(stdin)
	case *file != "":
		var b []byte
		b, err = os.ReadFile(*file)
		text = string(b)
	case *body == "" && !isTerminal(stdin):
		text, err = readAll(stdin)
	}
	if err != nil {
		return err
	}

	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	var labels []string
	if !*noLabel {
		labels = []string{*label}
	}
	is, err := newGitHub(cfg).CreateIssue(ctx, pos[0], text, labels)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "#%d %s\n", is.Number, is.HTMLURL)
	return nil
}

func cmdRun(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", 30*time.Minute, "maximum time each agent run may take")
	evalRounds := fs.Int("eval-rounds", 2, "how many times a pipeline's eval stage may send the change back to code")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: amf run <issue-number> [--timeout 30m] [--eval-rounds 2]")
		return errUsage
	}
	n, err := strconv.Atoi(strings.TrimPrefix(pos[0], "#"))
	if err != nil || n <= 0 {
		return fmt.Errorf("invalid issue number %q", pos[0])
	}
	r, err := newRunner(ctx, *timeout, *evalRounds, stderr)
	if err != nil {
		return err
	}
	return r.Run(ctx, n)
}

func cmdWatch(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	interval := fs.Duration("interval", 60*time.Second, "how often to poll GitHub")
	label := fs.String("label", labelReady, "process open issues with this label")
	timeout := fs.Duration("timeout", 30*time.Minute, "maximum time each agent run may take")
	evalRounds := fs.Int("eval-rounds", 2, "how many times a pipeline's eval stage may send the change back to code")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: amf watch [--interval 60s] [--label amf:ready] [--timeout 30m] [--eval-rounds 2]")
		return errUsage
	}
	r, err := newRunner(ctx, *timeout, *evalRounds, stderr)
	if err != nil {
		return err
	}
	return r.Watch(ctx, *label, *interval)
}

func newRunner(ctx context.Context, timeout time.Duration, evalRounds int, log io.Writer) (*Runner, error) {
	if evalRounds < 0 {
		return nil, errors.New("--eval-rounds can't be negative")
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return nil, err
	}
	gh := newGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return nil, err
	}
	root, err := checkoutRoot(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	if origin, err := git(ctx, root, "remote", "get-url", "origin"); err != nil {
		return nil, fmt.Errorf("%s has no origin remote", root)
	} else if !strings.EqualFold(repoFromRemote(origin), cfg.Repo) && origin != cfg.CloneURL {
		fmt.Fprintf(log, "amf: warning: origin is %s but GITHUB_REPO is %s\n", origin, cfg.Repo)
	}
	if err := excludeFromGit(ctx, root, "amf worktrees and logs", "/.amf/"); err != nil {
		return nil, err
	}
	return &Runner{
		GH:          gh,
		Repo:        cfg.Repo,
		Root:        root,
		Claude:      cfg.Claude,
		Codex:       cfg.Codex,
		Opencode:    cfg.Opencode,
		ClaudeToken: cfg.ClaudeToken,
		Timeout:     timeout,
		EvalRounds:  evalRounds,
		Log:         log,
	}, nil
}

// envPaths is ./.env, then .env at the top of the enclosing git checkout.
func envPaths(ctx context.Context) []string {
	paths := []string{".env"}
	if root, err := git(ctx, ".", "rev-parse", "--show-toplevel"); err == nil {
		paths = append(paths, filepath.Join(root, ".env"))
	}
	return paths
}

// parseArgs parses flags that may come before or after positional arguments,
// so `amf issue "Title" -b body` and `amf run 42 --timeout 1h` both work.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func readAll(r io.Reader) (string, error) {
	b, err := io.ReadAll(r)
	return string(b), err
}

func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
