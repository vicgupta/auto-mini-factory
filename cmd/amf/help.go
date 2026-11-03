package main

import (
	"flag"
	"fmt"
	"io"
	"slices"
)

// usage is the short overview amf, amf help, -h and --help print: one line
// per command and help topic. The details live in helpTopics.
const usage = `amf turns GitHub issues into pull requests with coding agents: Claude Code, Codex or opencode.

Usage:
  amf <command> [arguments]

Commands:
  init       write .env and create amf's labels
  issue      manage issues: create, list, run, watch
  pr         manage pull requests: list, merge, view, diff, close, reopen, ready, draft
  run        process one issue into a PR (alias of issue run)
  watch      process amf:ready issues as they arrive (alias of issue watch)
  auto       plan one sentence into up to 5 issues and merge their PRs, pausing after the first by default
  pipeline   check the plan/code/eval pipeline in this checkout's AGENTS.md
  doctor     check the setup here, changing nothing
  update     replace this amf binary with the latest (or given) release
  version    print amf's version
  help       show help for a command or topic

Topics:
  agents     which coding agent amf runs, and when it falls back to another
  env        configuration: every .env setting, with its default

Run amf help <command> or amf <command> -h for details.
`

const runHelp = `Usage:
  amf run <issue-number> [--agent A] [--agent-timeout 60m] [--eval-rounds 2] [--dry-run] [--draft] [--merge[=squash|rebase]] [--log-level L] [--log-file P]

Processes one issue into a pull request (alias of amf issue run; also takes
--draft, --dry-run, --merge, --eval-rounds, --log-level, --log-file).

--merge makes run and watch merge each PR they open or update, without
review: a merge commit, or --merge=squash or --merge=rebase. A merge GitHub
refuses (a conflict, required checks or reviews) leaves the PR open.
See diagrams and tradeoffs for --merge at:
https://github.com/vicgupta/auto-mini-factory/blob/main/docs/COMMANDS.md#merge-methods

amf run --dry-run makes the change on the local amf/issue-N branch and prints
the pull request it would open, without labeling, commenting, pushing,
opening a PR or merging.

run and watch work in the enclosing git checkout. Started anywhere else, they
clone GITHUB_REPO into ./<name> (or reuse that clone) and work there.

amf help agents says which agent runs; amf help env lists the settings.
`

const watchHelp = `Usage:
  amf watch [--agent A] [--interval 60s] [--label amf:ready] [--exclude-label L]... [--agent-timeout 60m] [--eval-rounds 2] [--draft] [--merge[=squash|rebase]] [--log-level L] [--log-file P]

Processes amf:ready issues as they arrive, one at a time, until interrupted
(alias of amf issue watch; also takes --exclude-label, --draft, --merge,
--agent-timeout, --eval-rounds, --log-level, --log-file). --merge, the
checkout and the agent work as for amf run: see amf help run.
See merge methods at:
https://github.com/vicgupta/auto-mini-factory/blob/main/docs/COMMANDS.md#merge-methods

amf help agents says which agent runs; amf help env lists the settings.
`

const listHelp = `Usage:
  amf issue list [--state open|closed|all] [--label L] [--search TEXT] [--limit N] [--json]

Lists issues, newest first, with their amf labels.
`

const initHelp = `Usage:
  amf init

Writes .env (backing up an existing one) and creates amf's labels.
amf help env lists every setting .env can hold.
`

const pipelineHelp = `Usage:
  amf pipeline [--fetch]

Checks the plan/code/eval pipeline in this checkout's AGENTS.md.

A repo can declare a pipeline in its AGENTS.md: a table under "## amf" with a
plan, code and eval stage, each naming a skill URL, an agent (claudecode,
codex or opencode) and a model. See docs/SPECS.md at
github.com/vicgupta/auto-mini-factory.
`

const doctorHelp = `Usage:
  amf doctor

Checks the setup here, one line per check, changing nothing.
`

const updateHelp = `Usage:
  amf update [--version vX.Y.Z] [--check]

Replaces this amf binary with the latest (or given) release.
`

const versionHelp = `Usage:
  amf version

Prints amf's version (also amf --version).
`

const helpHelp = `Usage:
  amf help [command [subcommand] | topic]

Without an argument, prints the overview of commands and topics. With one,
prints that command's or topic's details. Use amf help pr merge or
amf help issue list for a subcommand.
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

  GITHUB_TOKEN (default: required)
    API access: Contents, Issues, Pull requests (read & write)
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
  AMF_LOG_LEVEL (default: info (terminal), debug (event log))
    debug, info, warn or error, for both the terminal and the event log of run and watch;
    --log-level overrides it
  AMF_CODEX (default: codex)
    Path to the Codex CLI
  AMF_OPENCODE (default: opencode)
    Path to the opencode CLI
  AMF_API_URL (default: https://api.github.com)
    GitHub API base URL. amf pr ready and amf pr draft derive their GraphQL endpoint from
    it: …/graphql appended, or a GitHub Enterprise Server …/api/v3 becomes …/api/graphql
  AMF_CLONE_URL (default: https://github.com/<repo>.git)
    Where to clone GITHUB_REPO from

amf init writes the common settings to .env and preserves existing values;
amf doctor checks the configuration.
`

// helpTopics maps each amf help <topic> to its text: every command, plus
// the agents and env topics.
var helpTopics = map[string]string{
	"issue": issueUsage, "pr": prUsage, "list": listHelp, "run": runHelp, "watch": watchHelp, "auto": autoHelp,
	"init": initHelp, "pipeline": pipelineHelp, "doctor": doctorHelp, "update": updateHelp,
	"version": versionHelp, "help": helpHelp, "agents": agentsHelp, "env": envHelp,
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

// withHelp makes -h and --help on fs print the command's help topic before
// the flag list.
func withHelp(fs *flag.FlagSet, w io.Writer, topic string) {
	fs.Usage = func() {
		fmt.Fprintf(w, "%s\nUsage of %s:\n", topic, fs.Name())
		fs.PrintDefaults()
	}
}

// isHelpArgs reports whether args ask for help: exactly -h or --help.
func isHelpArgs(args []string) bool {
	return len(args) == 1 && (args[0] == "-h" || args[0] == "--help")
}
