package main

import (
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

// usage is the short overview amf, amf help, -h and --help print: one line
// per command and help topic. The details live in helpTopics.
const usage = `amf turns GitHub issues into pull requests with coding agents: Claude Code, Codex or opencode.

Usage:
  amf <command> [arguments]

Commands:
  init       write .env and create amf's labels
  auth       get a GitHub token: login, status, logout, token
  issue      manage issues: create, list, label, run, watch
  pr         manage pull requests: list, merge, sweep, view, diff, close, reopen, ready, draft
  run        process one issue into a PR (alias of issue run)
  watch      process amf:ready issues as they arrive (alias of issue watch)
  auto       plan one sentence into up to 5 issues and merge their PRs, pausing after the first by default
  memory     the repo's memory of issues, PRs and runs: sync, similar, show
  pipeline   check the plan/code/eval pipeline in this checkout's AGENTS.md
  doctor     check the setup here, changing nothing
  update     replace this amf binary with the latest (or given) release
  version    print amf's version
  help       show help for a command or topic

Topics:
  agents     which coding agent amf runs, and when it falls back to another
  env        configuration: every .env setting, with its default
  flags      flag conventions: long and short forms, shared letters
  output     stdout data, stderr diagnostics and workflow status messages

Paths default to the current directory; amf help run lists the path flags
every command takes.

Run amf help <command> or amf <command> -h for details.
Every flag has a long form and common ones a short one: see amf help flags.

Exit status:
  0  success
  1  command failed (GitHub error, agent failure, refused merge, etc.)
  2  usage error (no command, unknown command or flag, missing arguments)
amf help <command> exits 0; unknown commands exit 2.
Some commands return 2 for -h; use amf help <command> in scripts.
Command-specific value validation can return 1 (for example, amf run 0).
`

const outputHelp = `Output

Command data (issue/PR numbers and URLs, tables, JSON and diffs) goes to
stdout; progress logs, warnings and errors go to stderr. Redirect or pipe
stdout to capture results while keeping diagnostics visible. Use --json
where available for machine-readable results. Workflow commands such as
init, auto, pr merge, pr sweep --wait-for-checks and update also
print status messages or prompts to stdout, so their output is not always
a data-only stream.
`

const runHelp = `Usage:
  amf run <issue-number> [flags]

Processes one issue into a pull request (alias of amf issue run).

Exit status: 0 success, 1 command failed, 2 usage error (also --help).
Use amf help run for help that exits 0.

--merge makes run and watch merge each PR they open or update, without
review: a merge commit, or --merge=squash or --merge=rebase (also written
--merge squash, -m squash or -m=squash). A merge GitHub
refuses (a conflict, required checks or reviews) leaves the PR open.
See diagrams and tradeoffs for --merge at:
https://github.com/vicgupta/auto-mini-factory/blob/main/docs/COMMANDS.md#merge-methods

amf run --dry-run makes the change on the local amf/issue-N branch and prints
the pull request it would open, without labeling, commenting, pushing,
opening a PR or merging.

An issue that already has an open amf pull request is labeled amf:pr-open
and skipped, and one whose amf/issue-N branch is on origin without an open
PR is skipped: --force runs it anyway, updating the open PR.
A --force run on an open PR that is behind the default branch first merges
the default branch into it: a clean merge is pushed without an agent; on
conflicts one agent resolves only the conflicted files; with no usable
agent amf comments on the PR naming them.
An issue is
amf:pr-open while its PR is open and amf:done once GitHub confirms it is
merged. watch moves an amf:failed issue whose PR is open with passing
checks to amf:pr-open.

run, watch and auto hold .amf/watch.lock while they work, so two of them
never share a checkout; a lock left by a dead amf is taken over.

run and watch work in the enclosing git checkout. Started anywhere else, they
clone GITHUB_REPO into ./<name> (or reuse that clone) and work there.

run, watch and auto record each attempt in the repo's memory and note
similar past work: --memory-backend picks sqlite (default), markdown or
logfiles, and --memory-dir where the amf-memory folder goes (default: the
current directory). See amf help memory.

Paths default to the current directory; flags override them, and relative
values resolve against it:
  --dir DIR         work as if started in DIR
  --env-file FILE   read only FILE instead of ./.env
  --clone-dir DIR   use (or clone into) DIR instead of the checkout or ./<name>
  --log-file FILE   event log (default .amf/runs/<run_id>/events.jsonl, or off)
amf keeps its state under <checkout>/.amf/, excluded from git.

amf help agents says which agent runs; amf help env lists the settings.
`

const watchHelp = `Usage:
  amf watch [flags]

Processes amf:ready issues as they arrive, one at a time, until interrupted
(alias of amf issue watch). --merge, the checkout and the agent work as for
amf run: see amf help run.

Each poll, watch also puts amf:in-progress issues that no amf is working on
back to amf:ready once they have been in progress longer than
--stale-timeout (AMF_STALE_TIMEOUT; default twice the agent timeout, at
least 2h; off disables it), or labels them amf:pr-open if their PR is open.
With --merge, each poll also sweeps the open amf pull requests as amf pr
sweep --merge does: it updates a branch that is behind, merges a PR whose
checks passed and labels its issue amf:done, and comments once on a PR
that can't merge. It doesn't wait for pending checks.
See merge methods at:
https://github.com/vicgupta/auto-mini-factory/blob/main/docs/COMMANDS.md#merge-methods

amf help agents says which agent runs; amf help env lists the settings.
`

const listHelp = `Usage:
  amf issue list [flags]

Lists issues, newest first, with their amf labels.
`

const initHelp = `Usage:
  amf init [flags]

Writes .env (backing up an existing one) and creates amf's labels.
amf help env lists every setting .env can hold.
`

const pipelineHelp = `Usage:
  amf pipeline [flags]

Checks the plan/code/eval pipeline in this checkout's AGENTS.md.

A repo can declare a pipeline in its AGENTS.md: a table under "## amf" with a
plan, code and eval stage, each naming a skill URL, an agent (claudecode,
codex or opencode) and a model. See docs/SPECS.md at
github.com/vicgupta/auto-mini-factory.
`

const doctorHelp = `Usage:
  amf doctor [flags]

Checks the setup here, one line per check, changing nothing.

The label state check compares each issue labeled amf:done, amf:failed or
amf:pr-open with the pull requests from its amf/issue-N branch, and warns
about any that disagree: amf:done without a merged PR, amf:failed or
amf:pr-open with a merged PR, amf:pr-open with no open PR, or two status
labels. --fix-labels relabels them instead (the only change doctor makes).
`

const updateHelp = `Usage:
  amf update [flags]

Replaces this amf binary with the latest (or given) release.
`

const versionHelp = `Usage:
  amf version [flags]

Prints amf's version (also amf --version).
`

const helpHelp = `Usage:
  amf help [command [subcommand] | topic]

Without an argument, prints the overview of commands and topics. With one,
prints that command's or topic's details: amf help run prints the same as
amf run --help, and amf help pr merge the same as amf pr merge --help.
`

const agentsHelp = `Choosing the agent

Without a pipeline, each issue gets one agent run: the agent --agent names
(claudecode, codex or opencode), with no fallback, or else AMF_PRIMARY_AGENT
(default codex); if it fails, amf runs AMF_SECONDARY_AGENT (default
claudecode) in a clean worktree. AMF_SECONDARY_AGENT=none turns the fallback
off. claude needs ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN, codex
OPENAI_API_KEY, and opencode OPENCODE_API_KEY or its own login for a paid
OPENCODE_MODEL. If neither configured agent can run, amf picks the first of
claude, codex and opencode that can. amf watch shows what it found for each.

A pipeline in AGENTS.md names each stage's agent, and wins over --agent and
both settings: see amf help pipeline. amf help env lists the settings.
`

const envHelp = `Configuration is read from .env (or the environment; environment variables win).
Placeholders ending in ..., and owner/name, count as unset.

  GITHUB_TOKEN (default: gh auth token, then amf auth login)
    API access: Contents, Issues, Pull requests (read & write). GH_TOKEN is read too. Unset,
    amf uses gh's token, then the one amf auth login saved (amf help auth)
  GITHUB_REPO (default: required)
    owner/name; github.com URLs are accepted
  CLAUDE_CODE_OAUTH_TOKEN (default: unset)
    Claude subscription token, passed to Claude. Makes claude usable without --agent
  ANTHROPIC_API_KEY (default: unset)
    Claude API key, passed to Claude when there's no CLAUDE_CODE_OAUTH_TOKEN. Makes claude
    usable without --agent
  OPENAI_API_KEY (default: unset)
    OpenAI API key, passed to Codex. Makes codex usable without --agent
  OPENCODE_API_KEY (default: unset)
    OpenCode Console API key (oc_sk_...), passed to opencode only. One key covers
    pay-as-you-go opencode/<id> models (with credits) and, for a workspace subscribed to
    OpenCode Go, opencode-go/<id> models. Makes opencode usable without --agent with a
    paid OPENCODE_MODEL. Passed through the environment, never written to an agent
    configuration file; amf init preserves it in .env
  OPENCODE_MODEL (default: opencode/big-pickle)
    The model opencode runs with: always without a pipeline, and for pipeline stages that
    name no model. opencode/<id> is a free or pay-as-you-go model, opencode-go/<id> an
    OpenCode Go one. opencode models lists the choices
  AMF_TRUSTED (default: unset)
    GitHub logins, separated by commas or spaces (a leading @ is allowed, case is
    ignored). When set, amf only works on issues they opened, except issues labeled
    amf:auto, which are always trusted. Unset means anyone's
  AMF_POSTGRES_URL (default: unset)
    A PostgreSQL server that run and watch also write their logs to, in the amflogs database
  AMF_CLAUDE (default: claude)
    Path to the Claude CLI
  AMF_PRIMARY_AGENT (default: codex)
    The agent for issues without a pipeline: claudecode (or claude), codex or opencode,
    ignoring case. --agent and a pipeline override it
  AMF_SECONDARY_AGENT (default: claudecode)
    The agent amf falls back to when the primary fails, in a fresh worktree; the same
    values, or none for no fallback
  AMF_AGENT_TIMEOUT (default: 60m)
    How long each agent run may take, as a Go duration; --agent-timeout (or its deprecated
    alias --timeout) overrides it. Must be at least 1m and at most 24h
  AMF_STALE_TIMEOUT (default: max(2x agent timeout, 2h))
    How long watch lets an issue stay amf:in-progress with no amf working on it
    before putting it back to amf:ready; off disables it. --stale-timeout overrides it
  AMF_LOG_LEVEL (default: info (terminal), debug (event log))
    debug, info, warn or error, for both the terminal and the event log of run and watch;
    --log-level overrides it
  AMF_CODEX (default: codex)
    Path to the Codex CLI
  AMF_OPENCODE (default: opencode)
    Path to the opencode CLI
  AMF_GITHUB_CLIENT_ID (default: unset)
    The GitHub OAuth app amf auth login uses, when --client-id isn't given
  AMF_CONFIG_DIR (default: the user config folder's amf)
    Where amf auth login saves tokens (hosts.json); environment only
  AMF_API_URL (default: https://api.github.com)
    GitHub API base URL. amf pr ready and amf pr draft derive their GraphQL endpoint from
    it: …/graphql appended, or a GitHub Enterprise Server …/api/v3 becomes …/api/graphql
  AMF_CLONE_URL (default: https://github.com/<repo>.git)
    Where to clone GITHUB_REPO from

amf init writes the common settings to .env and preserves existing values;
amf doctor checks the configuration.
`

// helpTopics maps each amf help <topic> to its text: every command, plus
// the agents, env, flags and output topics.
var helpTopics = map[string]string{
	"issue": issueUsage, "auth": authUsage, "pr": prUsage, "list": listHelp, "run": runHelp, "watch": watchHelp, "auto": autoHelp, "memory": memoryUsage,
	"init": initHelp, "pipeline": pipelineHelp, "doctor": doctorHelp, "update": updateHelp,
	"version": versionHelp, "help": helpHelp, "agents": agentsHelp, "env": envHelp, "flags": flagsHelp,
	"output": outputHelp,
}

// topicNames is every help topic, for typo suggestions.
func topicNames() []string {
	names := append([]string{}, topCommands...)
	for k := range helpTopics {
		if k != "list" && !slices.Contains(names, k) { // amf help list still works, unlisted
			names = append(names, k)
		}
	}
	slices.Sort(names)
	return names
}

// withHelp makes -h and --help on fs print the help for key: its topic
// text, its flags in one aligned column, and examples. amf help <key> prints
// the same, by running the command with --help.
func withHelp(fs *flag.FlagSet, w io.Writer, key string) {
	// run strips the path flags before the command parses its own, so these
	// are only listed here; see paths.go.
	if fs.Lookup("dir") == nil {
		fs.String("dir", "", "work as if amf were started in this `folder` (default: the current directory)")
		fs.String("env-file", "", "read only this .env `file` (default: ./.env, then the checkout's top-level .env)")
		fs.String("clone-dir", "", "use, or clone GITHUB_REPO into, this `folder` (default: the enclosing checkout, else ./<repo name>)")
	}
	fs.Usage = func() { fmt.Fprint(w, commandHelp(fs, key)) }
}

// commandHelp is the full help for key, with fs's flags.
func commandHelp(fs *flag.FlagSet, key string) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(helpText(key), "\n"))
	b.WriteString("\n\nFlags:\n")
	printFlags(&b, fs)
	b.WriteString("\nGlobal flags (any command, before --):\n")
	printGlobalFlags(&b)
	if ex := helpExamples[key]; ex != "" {
		b.WriteString("\nExamples:\n" + ex)
	}
	return b.String()
}

// helpText is the topic text for a command or a two-word subcommand.
func helpText(key string) string {
	if t, ok := subHelp[key]; ok {
		return t
	}
	return helpTopics[key]
}

// newCommandFlags starts the FlagSet for the command key, wired to print its
// help to w.
func newCommandFlags(key string, w io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(key, flag.ContinueOnError)
	fs.SetOutput(w)
	withHelp(fs, w, key)
	return fs
}

// helpSubcommands lists the subcommands amf help <group> <subcommand> knows.
var helpSubcommands = map[string][]string{
	"pr":     prSubcommands,
	"issue":  issueSubcommands,
	"memory": {"sync", "similar", "show", "help"},
	"auth":   authSubcommands,
}

// helpArgs maps each amf help <topic> that is a command with flags to the
// arguments that run it, so amf help run prints what amf run --help does.
var helpArgs = map[string][]string{
	"init": {"init"}, "run": {"run"}, "watch": {"watch"}, "auto": {"auto"}, "list": {"issue", "list"},
	"pipeline": {"pipeline"}, "doctor": {"doctor"}, "update": {"update"}, "version": {"version"},
	"issue create": {"issue", "create"}, "issue list": {"issue", "list"}, "issue label": {"issue", "label"}, "issue run": {"run"}, "issue watch": {"watch"},
	"pr list": {"pr", "list"}, "pr merge": {"pr", "merge"}, "pr sweep": {"pr", "sweep"}, "pr view": {"pr", "view"}, "pr diff": {"pr", "diff"},
	"auth login": {"auth", "login"}, "auth status": {"auth", "status"}, "auth logout": {"auth", "logout"}, "auth token": {"auth", "token"},
	"memory sync": {"memory", "sync"}, "memory similar": {"memory", "similar"}, "memory show": {"memory", "show"},
	"pr close": {"pr", "close"}, "pr reopen": {"pr", "reopen"}, "pr ready": {"pr", "ready"}, "pr draft": {"pr", "draft"},
}

// isHelpArgs reports whether args ask for help: exactly -h or --help.
func isHelpArgs(args []string) bool {
	return len(args) == 1 && (args[0] == "-h" || args[0] == "--help")
}
