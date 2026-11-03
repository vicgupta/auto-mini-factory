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
  amf help [command | topic]

Without an argument, prints the overview of commands and topics. With one,
prints that command's or topic's details.
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

const envHelp = `Configuration is read from .env (or the environment):
  GITHUB_TOKEN   token with contents, issues and pull-requests write access
  GITHUB_REPO    owner/name
  CLAUDE_CODE_OAUTH_TOKEN  optional subscription token for Claude Code (claude setup-token)
  ANTHROPIC_API_KEY        optional Claude API key; with it or the token above, claude is usable
  OPENAI_API_KEY           optional OpenAI API key; with it, codex is usable
  OPENCODE_API_KEY         optional OpenCode Console API key, for paid opencode/ and Go opencode-go/ models
  OPENCODE_MODEL           model for opencode (default opencode/big-pickle); see opencode models
  AMF_PRIMARY_AGENT        agent for issues without a pipeline: claudecode, codex or opencode (default codex)
  AMF_SECONDARY_AGENT      agent amf falls back to if the primary fails, or none (default claudecode)
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

amf init writes .env with these; amf doctor checks them.
`

// helpTopics maps each amf help <topic> to its text: every command, plus
// the agents and env topics.
var helpTopics = map[string]string{
	"issue": issueUsage, "pr": prUsage, "list": listHelp, "run": runHelp, "watch": watchHelp,
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
