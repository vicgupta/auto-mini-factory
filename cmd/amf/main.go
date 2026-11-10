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

// defaultAgentIdleTimeout is how long an agent may produce no output before
// amf stops it, without AMF_AGENT_IDLE_TIMEOUT saying otherwise.
const defaultAgentIdleTimeout = 15 * time.Minute

const troubleshootingURL = "https://github.com/vicgupta/auto-mini-factory/blob/main/docs/TROUBLESHOOTING.md"

var errUsage = errors.New("usage")

const (
	exitOK      = 0
	exitFailed  = 1
	exitUsage   = 2
	exitSkipped = 3
)

// exitCode maps a run error to amf's documented exit status.
func exitCode(err error) int {
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errUsage):
		return exitUsage
	case errors.As(err, new(*skipError)):
		return exitSkipped
	default:
		return exitFailed
	}
}

// execute runs amf and prints the error main used to print, except usage and
// skip errors: the command already printed those.
func execute(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	err := run(ctx, args, stdin, stdout, stderr)
	code := exitCode(err)
	if err != nil && code != exitUsage && code != exitSkipped {
		fmt.Fprintln(stderr, "amf:", err)
	}
	return code
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(execute(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	paths = pathOptions{}
	var output outputOptions
	args, err := extractOutputFlags(args, &output)
	if err != nil {
		fmt.Fprintln(stderr, "amf:", err)
		return errUsage
	}
	ctx = context.WithValue(ctx, outputKey{}, output)
	var po pathOptions
	args, err = extractPathFlags(args, &po)
	if err != nil {
		fmt.Fprintln(stderr, "amf:", err)
		return errUsage
	}
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errUsage
	}
	if paths, err = resolvePathOptions(po); err != nil {
		return err
	}
	switch cmd, rest := args[0], args[1:]; cmd {
	case "init":
		return cmdInit(ctx, rest, stdout, stderr)
	case "auth":
		return cmdAuth(ctx, rest, stdout, stderr)
	case "pr":
		return cmdPR(ctx, rest, stdin, stdout, stderr)
	case "list": // undocumented alias of amf issue list, kept for existing scripts
		return cmdList(ctx, rest, stdout, stderr)
	case "issue":
		return cmdIssueGroup(ctx, rest, stdin, stdout, stderr)
	case "tag":
		return cmdTag(ctx, rest, stdout, stderr)
	case "release":
		return cmdRelease(ctx, rest, stdout, stderr)
	case "run":
		return cmdRun(ctx, rest, stderr)
	case "watch":
		return cmdWatch(ctx, rest, stderr)
	case "auto":
		return cmdAuto(ctx, rest, stdin, stdout, stderr)
	case "memory":
		return cmdMemory(ctx, rest, stdout, stderr)
	case "pipeline":
		return cmdPipeline(ctx, rest, stdout, stderr)
	case "doctor":
		return cmdDoctor(ctx, rest, stdout, stderr)
	case "update", "--update", "-u":
		// --update and -u are compatibility aliases of amf update
		// (like amf --version for version). Root -u is free: pr merge's
		// -u/--update-branch is command-local only.
		return cmdUpdate(ctx, rest, stdout, stderr)
	case "completion":
		return cmdCompletion(rest, stdout, stderr)
	case "man":
		return cmdMan(ctx, rest, stdout, stderr)
	case "--version":
		fmt.Fprintln(stdout, "amf", buildVersion())
		return nil
	case "version":
		if _, err := parseArgs(newCommandFlags("version", stderr), rest); err != nil {
			return errUsage
		}
		fmt.Fprintln(stdout, "amf", buildVersion())
		return nil
	case "help", "-h", "--help":
		// amf help <command or topic> prints its details.
		if cmd == "help" && len(rest) > 0 {
			key := rest[0]
			if len(rest) > 1 && (key == "pr" || key == "issue" || key == "tag" || key == "release" || key == "memory" || key == "auth") {
				key += " " + rest[1]
			}
			if a, ok := helpArgs[key]; ok {
				// The command prints its own help, flags and all, so
				// amf help run matches amf run --help.
				if err := run(ctx, append(slices.Clone(a), "--help"), nil, stdout, stdout); err != nil && !errors.Is(err, errUsage) {
					return err
				}
				return nil
			}
			// amf help pr <typo> is an unknown topic too, not pr's overview.
			if subs, ok := helpSubcommands[rest[0]]; ok && len(rest) > 1 && !slices.Contains(subs, rest[1]) {
				fmt.Fprintf(stderr, "amf: unknown help topic %q\n", key)
				if s := suggestFrom(rest[1], subs); s != "" {
					fmt.Fprintf(stderr, "Did you mean %q?\n", rest[0]+" "+s)
				}
				fmt.Fprintf(stderr, "\n%s", helpTopics[rest[0]])
				return errUsage
			}
			if t, ok := helpTopics[rest[0]]; ok {
				_, err := fmt.Fprint(stdout, t)
				return err
			}
			// A word that names no command is a typo: say so, rather than
			// printing the usage as if the question had been answered.
			if !strings.HasPrefix(rest[0], "-") {
				fmt.Fprintf(stderr, "amf: unknown help topic %q\n", rest[0])
				if s := suggestFrom(rest[0], topicNames()); s != "" {
					fmt.Fprintf(stderr, "Did you mean %q?\n", s)
				}
				fmt.Fprintf(stderr, "\n%s", usage)
				return errUsage
			}
		}
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprintf(stderr, "amf: unknown command %q\n", cmd)
		if s := suggestCommand(cmd); s != "" {
			fmt.Fprintf(stderr, "Did you mean %q?\n", s)
		}
		fmt.Fprintf(stderr, "\n%s", usage)
		return errUsage
	}
}

type issueCreateFlags struct {
	body    *string
	file    *string
	label   *string
	noLabel *bool
}

func addIssueCreateFlags(fs *flag.FlagSet) issueCreateFlags {
	return issueCreateFlags{
		body:    fs.String("body", "", "`text` of the issue body"),
		file:    fs.String("file", "", "read the issue body from the file at `path` (- for stdin)"),
		label:   fs.String("label", labelReady, "label that marks the issue for amf"),
		noLabel: fs.Bool("no-label", false, "don't label the issue for amf"),
	}
}

func cmdIssueCreate(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newCommandFlags("issue create", stderr)
	f := addIssueCreateFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return errUsage
	}
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
		fmt.Fprintln(stderr, `usage: amf issue create "Title" [--body text | --file path] [--label name] [--no-label]`)
		return errUsage
	}

	// --no-label wins over an explicit --label today, silently dropping the
	// label. Reject the pair, so a script or typo gets a clear error instead
	// of an unlabeled issue with no hint. fs.Visit tells an explicit --label
	// apart from its amf:ready default, so plain --no-label keeps working.
	var bodySet, labelSet bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "body":
			bodySet = true
		case "label":
			labelSet = true
		}
	})
	if *f.noLabel && labelSet {
		return errors.New("--label and --no-label can't be used together")
	}
	if !*f.noLabel && strings.TrimSpace(*f.label) == "" {
		return errors.New("--label can't be empty")
	}

	text := *f.body
	switch {
	case bodySet && *f.file != "":
		return errors.New("use --body or --file, not both")
	case *f.file == "-":
		text, err = readAll(stdin)
	case *f.file != "":
		var b []byte
		b, err = os.ReadFile(*f.file)
		text = string(b)
	case !bodySet && !isTerminal(stdin):
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
	if !*f.noLabel {
		labels = []string{strings.TrimSpace(*f.label)}
	}
	is, err := newGitHub(cfg).CreateIssue(ctx, strings.TrimSpace(pos[0]), text, labels)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "#%d %s\n", is.Number, is.HTMLURL)
	return nil
}

type runFlags struct {
	timeout    *time.Duration
	alias      *time.Duration
	evalRounds *int
	agent      *string
	draft      *bool
	merge      *mergeFlag
	waitChecks *bool
	dryRun     *bool
	force      *bool
	log        *logOptions
	memory     *memoryOptions
}

func addRunFlags(fs *flag.FlagSet) runFlags {
	var f runFlags
	f.timeout, f.alias = addTimeoutFlags(fs)
	f.evalRounds = fs.Int("eval-rounds", 2, "max eval rounds before code rerun")
	f.agent = fs.String("agent", "", agentFlagHelp)
	f.draft = fs.Bool("draft", false, draftFlagHelp)
	f.merge = addMergeFlag(fs)
	f.waitChecks = fs.Bool("wait-for-checks", true, waitChecksFlagHelp)
	f.dryRun = fs.Bool("dry-run", false, "make the change locally only; no push or PR")
	f.force = fs.Bool("force", false, "run anyway; save replaced commits as backup")
	f.log = addLogFlags(fs)
	f.memory = addMemoryFlags(fs)
	return f
}

func cmdRun(ctx context.Context, args []string, stderr io.Writer) (err error) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	withHelp(fs, stderr, "run")
	f := addRunFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if err := f.merge.check(pos); err != nil {
		return err
	}
	if *f.draft && f.merge.value != "" {
		return errDraftMerge
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: amf run <issue-number> [flags] (amf run --help lists them)")
		return errUsage
	}
	timeout, err := timeoutFlag(fs, f.timeout, f.alias)
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(pos[0]), "#"))
	if err != nil || n <= 0 {
		return fmt.Errorf("invalid issue number %q; use a positive number such as 42 or #42", pos[0])
	}
	if err := f.log.check(); err != nil {
		return err
	}
	if err := f.memory.check(); err != nil {
		return err
	}
	defer func() {
		if err != nil && !errors.As(err, new(*skipError)) {
			fmt.Fprintln(stderr, "amf: troubleshooting:", troubleshootingURL)
		}
	}()
	r, err := newRunner(ctx, timeout, *f.evalRounds, *f.agent, *f.log, stderr)
	if err != nil {
		return err
	}
	defer r.Close()
	r.Draft = *f.draft
	r.Merge = f.merge.value
	r.MergeSkipChecks = !*f.waitChecks
	r.DryRun = *f.dryRun
	r.Force = *f.force
	if err := r.lockRepo("run"); err != nil {
		return err
	}
	r.Command = "run"
	if !*f.dryRun {
		r.openMemory(*f.memory)
	}
	err = redactErr(r.Redact, r.Run(ctx, n))
	r.DB.logError(err)
	return err
}

type watchFlags struct {
	interval   *time.Duration
	label      *string
	exclude    labelsFlag
	timeout    *time.Duration
	alias      *time.Duration
	evalRounds *int
	agent      *string
	draft      *bool
	merge      *mergeFlag
	waitChecks *bool
	stale      *string
	log        *logOptions
	memory     *memoryOptions
}

func addWatchFlags(fs *flag.FlagSet) *watchFlags {
	f := &watchFlags{}
	f.interval = fs.Duration("interval", 60*time.Second, "poll interval as a Go duration")
	f.label = fs.String("label", labelReady, "issues with this label")
	fs.Var(&f.exclude, "exclude-label", "skip issues with this `label` (repeatable)")
	f.timeout, f.alias = addTimeoutFlags(fs)
	f.evalRounds = fs.Int("eval-rounds", 2, "max eval rounds before code rerun")
	f.agent = fs.String("agent", "", agentFlagHelp)
	f.draft = fs.Bool("draft", false, draftFlagHelp)
	f.merge = addMergeFlag(fs)
	f.waitChecks = fs.Bool("wait-for-checks", true, waitChecksFlagHelp)
	f.stale = fs.String("stale-timeout", "", "reset stale issues (default max(2x agent,2h))")
	f.log = addLogFlags(fs)
	f.memory = addMemoryFlags(fs)
	return f
}

func cmdWatch(ctx context.Context, args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	withHelp(fs, stderr, "watch")
	f := addWatchFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return errUsage
	}
	if err := f.merge.check(pos); err != nil {
		return err
	}
	if *f.draft && f.merge.value != "" {
		return errDraftMerge
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: amf watch [flags] (amf watch --help lists them)")
		return errUsage
	}
	if *f.interval <= 0 {
		return errors.New("--interval must be positive")
	}
	if slices.Contains(f.exclude, "") {
		return errors.New("--exclude-label can't be empty")
	}
	for _, x := range f.exclude {
		if strings.EqualFold(x, strings.TrimSpace(*f.label)) {
			return fmt.Errorf("--exclude-label %q is the label watch is processing, so every issue would be skipped", x)
		}
	}
	if *f.label == "" {
		return errors.New("--label must not be empty")
	}
	timeout, err := timeoutFlag(fs, f.timeout, f.alias)
	if err != nil {
		return err
	}
	if err := f.log.check(); err != nil {
		return err
	}
	if err := f.memory.check(); err != nil {
		return err
	}
	r, err := newRunner(ctx, timeout, *f.evalRounds, *f.agent, *f.log, stderr)
	if err != nil {
		return err
	}
	defer r.Close()
	r.Command = "watch"
	r.openMemory(*f.memory)
	r.Draft = *f.draft
	r.ExcludeLabels = f.exclude
	r.Merge = f.merge.value
	r.MergeSkipChecks = !*f.waitChecks
	if *f.stale != "" {
		if r.StaleTimeout, err = parseStaleTimeout(*f.stale, "--stale-timeout", r.Timeout); err != nil {
			return err
		}
	}
	if err := r.lockRepo("watch"); err != nil {
		return err
	}
	err = redactErr(r.Redact, r.Watch(ctx, *f.label, *f.interval))
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
const agentFlagHelp = "agent name (default: AMF_PRIMARY_AGENT)"

// draftFlagHelp describes --draft for run and watch.
const draftFlagHelp = "open new pull requests as drafts"

// addTimeoutFlags registers the agent timeout flags for run and watch:
// --agent-timeout, and the deprecated --timeout alias next to it.
func addTimeoutFlags(fs *flag.FlagSet) (at, alias *time.Duration) {
	at = fs.Duration("agent-timeout", defaultAgentTimeout, "max agent run time (Go duration)")
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
	fs.StringVar(&lo.Level, "log-level", "", "debug, info, warn or error (default: info)")
	fs.StringVar(&lo.File, "log-file", "", "event log path (default .amf/runs/<id>, off)")
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
	idleTimeout, err := parseAgentIdleTimeout(cfg.AgentIdleTimeout)
	if err != nil {
		return nil, err
	}
	consoleLevel, fileLevel, err := logLevels(lo.Level, cfg.LogLevel)
	if err != nil {
		return nil, err
	}
	output := commandOutput(ctx)
	if output.Quiet {
		consoleLevel = max(consoleLevel, slog.LevelWarn)
	}
	if output.colorEnabled(log) {
		log = colorPrefixWriter{w: log}
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
	r.Quiet = output.Quiet
	r.IdleTimeout = idleTimeout

	runID := newRunID()
	path := lo.File
	if path != "" && path != "off" {
		if path, err = absFlag(path); err != nil {
			r.Close()
			return nil, fmt.Errorf("--log-file: %v", err)
		}
	}
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
	primary, secondary, err := cfg.agents()
	if err != nil {
		return nil, err
	}
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
	stale, err := parseStaleTimeout(cfg.StaleTimeout, "AMF_STALE_TIMEOUT", timeout)
	if err != nil {
		return nil, err
	}
	return &Runner{
		StaleTimeout:  stale,
		GH:            gh,
		Repo:          cfg.Repo,
		Root:          root,
		Claude:        cfg.Claude,
		Codex:         cfg.Codex,
		Opencode:      cfg.Opencode,
		Agent:         agent,
		Primary:       primary,
		Secondary:     secondary,
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

// envPaths is <cwd>/.env, then .env at the top of the enclosing git checkout.
// --env-file replaces both.
func envPaths(ctx context.Context) []string {
	if paths.EnvFile != "" {
		return []string{paths.EnvFile}
	}
	ps := []string{".env"}
	if wd, err := os.Getwd(); err == nil {
		ps[0] = filepath.Join(wd, ".env")
	}
	if root, err := git(ctx, ".", "rev-parse", "--show-toplevel"); err == nil && filepath.Join(root, ".env") != ps[0] {
		ps = append(ps, filepath.Join(root, ".env"))
	}
	return ps
}

// parseArgs parses flags that may come before or after positional arguments,
// so `amf issue "Title" -b body` and `amf run 42 --agent-timeout 1h` both work.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	args = expandShortFlags(fs, args)
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

var topCommands = []string{"init", "auth", "issue", "pr", "tag", "release", "run", "watch", "auto", "memory", "pipeline", "doctor", "update", "completion", "man", "version", "help"}

// suggestCommand returns the command closest to a mistyped one, or "" when
// nothing is close enough to be a likely typo.
// The hidden top-level list alias is suggested as issue list.
func suggestCommand(cmd string) string {
	s := suggestFrom(cmd, append([]string{"list"}, topCommands...))
	if s == "list" {
		return "issue list"
	}
	return s
}

// suggestFrom returns the entry of names closest to cmd, or "" when none is
// within two edits.
func suggestFrom(cmd string, names []string) string {
	best, bestD := "", 3
	for _, c := range names {
		if d := editDistance(strings.ToLower(cmd), c); d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}
