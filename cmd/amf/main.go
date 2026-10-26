// Command amf (autonomous mini factory) turns GitHub issues into pull
// requests with coding agents: Claude Code, Codex or opencode. See README.md.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
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

// defaultAgentTimeout is how long an agent run may take, without a flag or
// AMF_AGENT_TIMEOUT saying otherwise. amf runs agents unattended with every
// permission, so a run without a limit could watch forever.
const defaultAgentTimeout = 60 * time.Minute

const usage = `amf turns GitHub issues into pull requests with coding agents: Claude Code, Codex or opencode.

Usage:
  amf init                                                           write .env (backing up an existing one) and create amf's labels
  amf issue "Title" [-b body | -f file.md] [--label L] [--no-label]  create an issue (body may also come from stdin)
  amf pr <command>                                                   manage pull requests (list, merge, view, diff, close, reopen, ready, draft)
  amf list [--state open|closed|all] [--label L] [--limit N]         list issues, newest first, with their amf labels (also takes --json)
  amf run <issue-number> [--agent A] [--agent-timeout 60m]           process one issue into a PR (also takes --draft, --dry-run, --merge, --eval-rounds, --log-level, --log-file)
  amf watch [--agent A] [--interval 60s] [--label amf:ready]         process amf:ready issues as they arrive (also takes --exclude-label, --draft, --merge, --agent-timeout, --eval-rounds, --log-level, --log-file)
  amf pipeline [--fetch]                                             check the plan/code/eval pipeline in this checkout's AGENTS.md
  amf doctor                                                         check the setup here, one line per check, changing nothing
  amf update [--version vX.Y.Z] [--check]                            replace this amf binary with the latest (or given) release
  amf version

Configuration is read from .env (or the environment):
  GITHUB_TOKEN   token with contents, issues and pull-requests write access
  GITHUB_REPO    owner/name
  CLAUDE_CODE_OAUTH_TOKEN  optional subscription token for Claude Code (claude setup-token)
  ANTHROPIC_API_KEY        optional Claude API key; with it or the token above, amf picks claude
  OPENAI_API_KEY           optional OpenAI API key; with it, amf picks codex when claude can't run
  OPENCODE_API_KEY         optional OpenCode Console API key, for paid opencode/ and Go opencode-go/ models
  OPENCODE_MODEL           model for opencode (default opencode/big-pickle); see opencode models
  AMF_AGENT_TIMEOUT        how long each agent run may take, a Go duration (default 60m);
                           --agent-timeout, or its deprecated alias --timeout, overrides it
  AMF_LOG_LEVEL            debug, info, warn or error (default info, and debug in .amf/runs/); --log-level overrides it
  AMF_POSTGRES_URL         optional postgres:// URL; run and watch also log to its amflogs database
  AMF_CLAUDE     path to the claude binary (default "claude")
  AMF_CODEX      path to the codex binary (default "codex")
  AMF_OPENCODE   path to the opencode binary (default "opencode")
  AMF_TRUSTED    optional GitHub logins, comma-separated; amf only works on their issues
  AMF_API_URL    GitHub API base URL (default https://api.github.com)
  AMF_CLONE_URL  URL to clone GITHUB_REPO from (default https://github.com/GITHUB_REPO.git)

--merge makes run and watch merge each PR they open or update, without
review: a merge commit, or --merge=squash or --merge=rebase. A merge GitHub
refuses (a conflict, required checks or reviews) leaves the PR open.

amf run --dry-run makes the change on the local amf/issue-N branch and prints
the pull request it would open, without labeling, commenting, pushing,
opening a PR or merging.

run and watch work in the enclosing git checkout. Started anywhere else, they
clone GITHUB_REPO into ./<name> (or reuse that clone) and work there.

Without a pipeline, each issue gets one agent run: the agent --agent names
(claudecode, codex or opencode), or else claude if it's installed and has
ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN, else codex if it's installed and
has OPENAI_API_KEY, else opencode (which needs OPENCODE_API_KEY or its own
login for a paid OPENCODE_MODEL). amf watch shows what it found for each.

A repo can declare a pipeline in its AGENTS.md: a table under "## amf" with a
plan, code and eval stage, each naming a skill URL, an agent (claudecode,
codex or opencode) and a model. See docs/SPECS.md at
github.com/vicgupta/auto-mini-factory.
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
	case "pr":
		return cmdPR(ctx, rest, stdout, stderr)
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
	case "doctor":
		return cmdDoctor(ctx, rest, stdout, stderr)
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
		fmt.Fprintln(stderr, `usage: amf issue "Title" [-b body | -f file.md] [--label L] [--no-label]`)
		return errUsage
	}

	// --no-label wins over an explicit --label today, silently dropping the
	// label. Reject the pair, so a script or typo gets a clear error instead
	// of an unlabeled issue with no hint. fs.Visit tells an explicit --label
	// apart from its amf:ready default, so plain --no-label keeps working.
	var labelSet bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "label" {
			labelSet = true
		}
	})
	if *noLabel && labelSet {
		return errors.New("--label and --no-label can't be used together")
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
	at, alias := addTimeoutFlags(fs)
	evalRounds := fs.Int("eval-rounds", 2, "how many times a pipeline's eval stage may send the change back to code")
	agent := fs.String("agent", "", agentFlagHelp)
	draft := fs.Bool("draft", false, draftFlagHelp)
	merge := addMergeFlag(fs)
	dryRun := fs.Bool("dry-run", false, "make the change on the local branch only: no label, comment, push, PR or merge")
	lo := addLogFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if err := merge.check(pos); err != nil {
		return err
	}
	if *draft && merge.value != "" {
		return errDraftMerge
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: amf run <issue-number> [--agent claudecode|codex|opencode] [--agent-timeout 60m] [--eval-rounds 2] [--dry-run] [--draft] [--merge[=squash|rebase]]")
		return errUsage
	}
	timeout, err := timeoutFlag(fs, at, alias)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(strings.TrimPrefix(pos[0], "#"))
	if err != nil || n <= 0 {
		return fmt.Errorf("invalid issue number %q", pos[0])
	}
	if err := lo.check(); err != nil {
		return err
	}
	r, err := newRunner(ctx, timeout, *evalRounds, *agent, *lo, stderr)
	if err != nil {
		return err
	}
	defer r.Close()
	r.Draft = *draft
	r.Merge = merge.value
	r.DryRun = *dryRun
	err = redactErr(r.Redact, r.Run(ctx, n))
	r.DB.logError(err)
	return err
}

func cmdWatch(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	interval := fs.Duration("interval", 60*time.Second, "how often to poll GitHub")
	label := fs.String("label", labelReady, "process open issues with this label")
	var exclude labelsFlag
	fs.Var(&exclude, "exclude-label", "skip issues with this `label`, leaving them as they are (repeatable: any of them skips)")
	at, alias := addTimeoutFlags(fs)
	evalRounds := fs.Int("eval-rounds", 2, "how many times a pipeline's eval stage may send the change back to code")
	agent := fs.String("agent", "", agentFlagHelp)
	draft := fs.Bool("draft", false, draftFlagHelp)
	merge := addMergeFlag(fs)
	lo := addLogFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if err := merge.check(pos); err != nil {
		return err
	}
	if *draft && merge.value != "" {
		return errDraftMerge
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: amf watch [--agent claudecode|codex|opencode] [--interval 60s] [--label amf:ready] [--exclude-label L]... [--agent-timeout 60m] [--eval-rounds 2] [--draft] [--merge[=squash|rebase]]")
		return errUsage
	}
	if *interval <= 0 {
		return errors.New("--interval must be positive")
	}
	if slices.Contains(exclude, "") {
		return errors.New("--exclude-label can't be empty")
	}
	if *label == "" {
		return errors.New("--label must not be empty")
	}
	timeout, err := timeoutFlag(fs, at, alias)
	if err != nil {
		return err
	}
	if err := lo.check(); err != nil {
		return err
	}
	r, err := newRunner(ctx, timeout, *evalRounds, *agent, *lo, stderr)
	if err != nil {
		return err
	}
	defer r.Close()
	r.Draft = *draft
	r.ExcludeLabels = exclude
	r.Merge = merge.value
	err = redactErr(r.Redact, r.Watch(ctx, *label, *interval))
	r.DB.logError(err)
	return err
}

// labelsFlag is a repeatable label flag: each value trimmed, duplicates
// dropped. An empty value stays in, for the command to reject.
type labelsFlag []string

func (f *labelsFlag) String() string { return strings.Join(*f, ",") }

func (f *labelsFlag) Set(s string) error {
	if s = strings.TrimSpace(s); !slices.Contains(*f, s) {
		*f = append(*f, s)
	}
	return nil
}

// errDraftMerge rejects --draft with --merge: GitHub refuses to merge a draft.
var errDraftMerge = errors.New("--draft and --merge can't be used together: GitHub doesn't merge draft pull requests")

// agentFlagHelp describes --agent for run and watch.
const agentFlagHelp = "agent for issues without a pipeline: claudecode, codex or opencode (default: claude if installed with ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN, else codex if installed with OPENAI_API_KEY, else opencode)"

// draftFlagHelp describes --draft for run and watch.
const draftFlagHelp = "open new pull requests as drafts (a retry leaves an open PR a draft or ready for review as it was)"

// addTimeoutFlags registers the agent timeout flags for run and watch:
// --agent-timeout, and the deprecated --timeout alias next to it.
func addTimeoutFlags(fs *flag.FlagSet) (at, alias *time.Duration) {
	at = fs.Duration("agent-timeout", defaultAgentTimeout, "maximum time each agent run may take")
	alias = fs.Duration("timeout", defaultAgentTimeout, "deprecated alias of --agent-timeout")
	return at, alias
}

// timeoutFlag reports the timeout a run's flags gave: --agent-timeout wins
// over its deprecated --timeout alias. When neither was set it reports
// (0, ""), and newRunner resolves AMF_AGENT_TIMEOUT or the default.
func timeoutFlag(fs *flag.FlagSet, at, alias *time.Duration) (time.Duration, error) {
	var atSet, aliasSet bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "agent-timeout":
			atSet = true
		case "timeout":
			aliasSet = true
		}
	})
	switch {
	case atSet:
		return *at, validateAgentTimeout(*at, "--agent-timeout")
	case aliasSet:
		return *alias, validateAgentTimeout(*alias, "--timeout")
	}
	return 0, nil
}

// agentTimeout resolves the timeout for a run whose flags named none: the
// value from AMF_AGENT_TIMEOUT (.env or the environment), else the default.
func agentTimeout(env time.Duration) (time.Duration, string) {
	if env != 0 {
		return env, "AMF_AGENT_TIMEOUT"
	}
	return defaultAgentTimeout, "default"
}

// agentKillGrace is how long an agent gets to exit after SIGTERM before amf
// SIGKILLs its whole process group.
const agentKillGrace = 15 * time.Second

// The timeouts amf accepts for an agent run: 0 and negatives run agents
// unbounded (amf has none, and unbounded runs hold the amf:in-progress
// label), less than a minute rarely covers a real run, and above a day is a
// mistake. Tests go around the flags and set Runner.Timeout directly.
const (
	minAgentTimeout = time.Minute
	maxAgentTimeout = 24 * time.Hour
)

// validateAgentTimeout rejects agent timeouts amf won't honor, naming src
// (the flag or variable they came from) in the message.
func validateAgentTimeout(d time.Duration, src string) error {
	switch {
	case d <= 0:
		return fmt.Errorf("%s: agent timeout must be positive, got %s", src, d)
	case d < minAgentTimeout:
		return fmt.Errorf("%s: agent timeout must be at least %s, got %s", src, minAgentTimeout, d)
	case d > maxAgentTimeout:
		return fmt.Errorf("%s: agent timeout must be at most %s, got %s", src, maxAgentTimeout, d)
	}
	return nil
}

// addLogFlags registers --log-level and --log-file for run and watch.
func addLogFlags(fs *flag.FlagSet) *logOptions {
	lo := &logOptions{}
	fs.StringVar(&lo.Level, "log-level", "", "debug, info, warn or error, for the terminal and the event log (default info, and debug in the event log)")
	fs.StringVar(&lo.File, "log-file", "", `where the event log goes (default .amf/runs/<run_id>/events.jsonl; "off" for none)`)
	return lo
}

// check rejects a bad --log-level before anything else runs.
func (lo logOptions) check() error {
	if lo.Level == "" {
		return nil
	}
	_, err := parseLogLevel(lo.Level, "--log-level")
	return err
}

func newRunner(ctx context.Context, timeout time.Duration, evalRounds int, agentFlag string, lo logOptions, log io.Writer) (*Runner, error) {
	if evalRounds < 0 {
		return nil, errors.New("--eval-rounds can't be negative")
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return nil, err
	}
	if timeout == 0 { // no flag gave one, or run and watch would have failed on it already
		env, err := parseAgentTimeout(cfg.AgentTimeout)
		if err != nil {
			return nil, err
		}
		timeout, _ = agentTimeout(env)
	}
	consoleLevel, fileLevel, err := logLevels(lo.Level, cfg.LogLevel)
	if err != nil {
		return nil, err
	}
	agent, err := parseAgent(agentFlag)
	if err != nil {
		return nil, err
	}
	// Everything run and watch write from here on is redacted: the console
	// here, and the database, the agents' logs, comments and PR bodies by
	// the Runner and dbLog.
	red := configRedactor(cfg)
	log = &redactWriter{r: red, w: log}
	var db *dbLog
	if cfg.PostgresURL != "" {
		pg, err := parsePostgresURL(cfg.PostgresURL)
		if err != nil {
			return nil, fmt.Errorf("AMF_POSTGRES_URL: %v", red.Redact(err.Error()))
		}
		db = startDBLog(ctx, pg, cfg.Repo, log, red)
		log = io.MultiWriter(log, db.amfWriter())
	}
	r, err := newRunnerFrom(ctx, cfg, timeout, evalRounds, agent, log)
	if err != nil {
		err = redactErr(red, err)
		db.logError(err)
		db.Close(15 * time.Second)
		return nil, err
	}
	r.DB = db
	r.Redact = red

	runID := newRunID()
	path := lo.File
	if path == "" {
		path = filepath.Join(r.Root, ".amf", "runs", runID, "events.jsonl")
	}
	var file io.Writer
	shownLog := ""
	if path != "off" {
		ef, err := openEventFile(path)
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("--log-file: %v", err)
		}
		r.events, file, shownLog = ef, ef, path
		// The summary stays in the run's folder, wherever the event log goes.
		runDir := filepath.Join(r.Root, ".amf", "runs", runID)
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			r.Close()
			return nil, err
		}
		r.summaryPath = filepath.Join(runDir, "summary.json")
	}
	r.stats = newStatsHandler(runID, shownLog)
	r.Events = newEventLogger(r.Log, consoleLevel, file, fileLevel, red, runID, r.stats)
	if file != nil {
		r.event(slog.LevelInfo, "log.file", 0, fmt.Sprintf("logging to %s", shownPath(r.Root, path)), "path", path)
	}
	r.event(slog.LevelDebug, "config.load", 0, fmt.Sprintf("configuration from %s", cmp.Or(cfg.EnvFile, "the environment")),
		"paths", cfg.EnvPaths, "file", cfg.EnvFile, slog.Group("keys", cfg.configAttrs()...))
	r.DB.setEvents(r.Events)
	r.event(slog.LevelDebug, "checkout.resolve", 0, fmt.Sprintf("checkout: %s (%s)", r.Root, r.checkout), "how", r.checkout, "dir", r.Root)
	return r, nil
}

// shownPath is path relative to root when it is inside it, as one would
// type it from the checkout, else as given.
func shownPath(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

func newRunnerFrom(ctx context.Context, cfg Config, timeout time.Duration, evalRounds int, agent string, log io.Writer) (*Runner, error) {
	// A named agent must be there; the fallback order only applies without --agent.
	agents := &Runner{Claude: cfg.Claude, Codex: cfg.Codex, Opencode: cfg.Opencode, Agent: agent} // --agent needs no key
	if agent != "" {
		if _, err := agents.defaultAgent(); err != nil {
			return nil, err
		}
	}
	gh := newGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return nil, err
	}
	root, how, err := checkoutRoot(ctx, cfg, log)
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
		GH:            gh,
		Repo:          cfg.Repo,
		Root:          root,
		Claude:        cfg.Claude,
		Codex:         cfg.Codex,
		Opencode:      cfg.Opencode,
		Agent:         agent,
		ClaudeToken:   cfg.ClaudeToken,
		AnthropicKey:  cfg.AnthropicKey,
		OpenAIKey:     cfg.OpenAIKey,
		OpencodeKey:   cfg.OpencodeKey,
		OpencodeModel: cfg.OpencodeModel,
		Trusted:       cfg.Trusted,
		Timeout:       timeout,
		EvalRounds:    evalRounds,
		Log:           log,
		checkout:      how,
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
// so `amf issue "Title" -b body` and `amf run 42 --agent-timeout 1h` both work.
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
