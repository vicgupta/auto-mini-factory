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
  amf issue "Title" [-b body | -f file.md] [--no-label]   create an issue (body may also come from stdin)
  amf run <issue-number> [--timeout 30m]                   process one issue into a PR
  amf watch [--interval 60s] [--label amf] [--timeout 30m] process labeled issues as they arrive
  amf version

Configuration is read from .env (or the environment):
  GITHUB_TOKEN   token with contents, issues and pull-requests write access
  GITHUB_REPO    owner/name
  CLAUDE_CODE_OAUTH_TOKEN  optional subscription token for Claude Code (claude setup-token)
  AMF_CLAUDE     path to the claude binary (default "claude")
  AMF_API_URL    GitHub API base URL (default https://api.github.com)

run and watch must be started inside a clone of GITHUB_REPO.
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
	case "issue":
		return cmdIssue(ctx, rest, stdin, stdout, stderr)
	case "run":
		return cmdRun(ctx, rest, stderr)
	case "watch":
		return cmdWatch(ctx, rest, stderr)
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
	label := fs.String("label", labelTrigger, "label that marks the issue for amf")
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
	timeout := fs.Duration("timeout", 30*time.Minute, "maximum time Claude may spend on the issue")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: amf run <issue-number> [--timeout 30m]")
		return errUsage
	}
	n, err := strconv.Atoi(strings.TrimPrefix(pos[0], "#"))
	if err != nil || n <= 0 {
		return fmt.Errorf("invalid issue number %q", pos[0])
	}
	r, err := newRunner(ctx, *timeout, stderr)
	if err != nil {
		return err
	}
	return r.Run(ctx, n)
}

func cmdWatch(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	interval := fs.Duration("interval", 60*time.Second, "how often to poll GitHub")
	label := fs.String("label", labelTrigger, "process open issues with this label")
	timeout := fs.Duration("timeout", 30*time.Minute, "maximum time Claude may spend per issue")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: amf watch [--interval 60s] [--label amf] [--timeout 30m]")
		return errUsage
	}
	r, err := newRunner(ctx, *timeout, stderr)
	if err != nil {
		return err
	}
	return r.Watch(ctx, *label, *interval)
}

func newRunner(ctx context.Context, timeout time.Duration, log io.Writer) (*Runner, error) {
	root, err := git(ctx, ".", "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, errors.New("run amf inside a git clone of GITHUB_REPO")
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return nil, err
	}
	if origin, err := git(ctx, root, "remote", "get-url", "origin"); err != nil {
		return nil, errors.New("this checkout has no origin remote")
	} else if !strings.EqualFold(normalizeRepo(strings.TrimPrefix(origin, "ssh://git@github.com/")), cfg.Repo) {
		fmt.Fprintf(log, "amf: warning: origin is %s but GITHUB_REPO is %s\n", origin, cfg.Repo)
	}
	if err := excludeAMFDir(ctx, root); err != nil {
		return nil, err
	}
	return &Runner{
		GH:          newGitHub(cfg),
		Repo:        cfg.Repo,
		Root:        root,
		Claude:      cfg.Claude,
		ClaudeToken: cfg.ClaudeToken,
		Timeout:     timeout,
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
