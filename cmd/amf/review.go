package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// reviewTailLines is how many lines of the test command's combined output
// the review gate records.
const reviewTailLines = 60

var reviewVerdictLine = regexp.MustCompile(`(?i)^amf-review:\s*(pass|block)\.?$`)

// reviewOutcome is what the pre-PR review gate observed and decided.
type reviewOutcome struct {
	CommandRan      bool
	Command         string
	CommandExit     int
	CommandDuration time.Duration
	CommandOutput   string

	ReviewerAgent string
	ReviewerRan   bool
	VerdictFound  bool
	Passed        bool
	Findings      string

	reasons []string
}

// reviewGate runs AMF_REVIEW_GATE's steps in the worktree after build and
// changeFacts, before anything is pushed: the optional test command, then a
// review agent. It returns the outcome, and a stageError when the gate
// blocks the PR.
func (r *Runner) reviewGate(ctx context.Context, issue Issue, base string, p *Pipeline) (reviewOutcome, error) {
	n := issue.Number
	wt := r.worktreePath(n)
	out := reviewOutcome{}

	agent, err := r.reviewAgent(p)
	if err != nil {
		out.reasons = append(out.reasons, "no agent was available to review the change: "+err.Error())
		return out, r.blockReview(issue, out, wt)
	}
	out.ReviewerAgent = agentName(agent)

	beforeHead, err := git(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		return out, err
	}
	beforeStatus, err := git(ctx, wt, "status", "--porcelain")
	if err != nil {
		return out, err
	}

	if r.ReviewCommand != "" {
		out.CommandRan, out.Command = true, r.ReviewCommand
		out.CommandExit, out.CommandDuration, out.CommandOutput, err = r.runReviewCommand(ctx, wt, agent)
		r.event(slog.LevelDebug, "review.command", n,
			fmt.Sprintf("#%d: review command exited %d in %s", n, out.CommandExit, out.CommandDuration.Round(10*time.Millisecond)),
			"command", out.Command, "exit_code", out.CommandExit, "duration", out.CommandDuration.String(), "output", out.CommandOutput)
		if err != nil {
			out.reasons = append(out.reasons, fmt.Sprintf("the test command failed (exit %d after %s)", out.CommandExit, out.CommandDuration.Round(10*time.Millisecond)))
		}
	}

	logPath := filepath.Join(r.Root, ".amf", "logs", fmt.Sprintf("issue-%d-review.log", n))
	outPath := filepath.Join(wt, ".amf", "review.md")
	model := r.agentModel(agent, "")
	out.ReviewerRan = true
	summary, aerr := r.runAgent(ctx, agent, "review", model, wt, base, reviewPrompt(r.Repo, issue, base), logPath, outPath)
	pass, found := parseReviewVerdict(summary)
	out.Passed, out.VerdictFound, out.Findings = pass, found, stripReviewVerdict(summary)
	switch {
	case aerr != nil:
		out.reasons = append(out.reasons, fmt.Sprintf("the review agent %s failed: %v", agentName(agent), aerr))
	case !found:
		out.reasons = append(out.reasons, "the review agent gave no amf-review verdict")
	case !pass:
		out.reasons = append(out.reasons, "the review agent returned amf-review: block")
	}

	afterHead, herr := git(ctx, wt, "rev-parse", "HEAD")
	afterStatus, serr := git(ctx, wt, "status", "--porcelain")
	switch {
	case herr != nil:
		out.reasons = append(out.reasons, fmt.Sprintf("could not read HEAD after review: %v", herr))
	case afterHead != beforeHead:
		out.reasons = append(out.reasons, "the worktree's HEAD moved during review")
	}
	switch {
	case serr != nil:
		out.reasons = append(out.reasons, fmt.Sprintf("could not read the worktree status after review: %v", serr))
	case afterStatus != beforeStatus:
		out.reasons = append(out.reasons, "the worktree changed during review (git status is not empty)")
	}

	if len(out.reasons) > 0 {
		return out, r.blockReview(issue, out, wt)
	}
	r.event(slog.LevelInfo, "review.pass", n, fmt.Sprintf("#%d: the review gate passed", n),
		"agent", out.ReviewerAgent, "command", out.Command, "exit_code", out.CommandExit,
		"duration", out.CommandDuration.String(), "pass", true)
	return out, nil
}

// blockReview logs review.block and returns the stageError that fails the
// issue without pushing or touching a PR. The worktree and the local branch
// stay for inspection.
func (r *Runner) blockReview(issue Issue, out reviewOutcome, wt string) error {
	n := issue.Number
	r.event(slog.LevelWarn, "review.block", n,
		fmt.Sprintf("#%d: the review gate blocked the pull request: %s", n, strings.Join(out.reasons, "; ")),
		"reasons", out.reasons, "agent", out.ReviewerAgent, "command", out.Command,
		"exit_code", out.CommandExit, "duration", out.CommandDuration.String(), "worktree", wt)
	return &stageError{
		msg:     fmt.Sprintf("the review gate blocked the pull request: %s", strings.Join(out.reasons, "; ")),
		comment: out.comment(r, issue, wt),
	}
}

// reviewAgent picks the agent for the review: the one that made the change
// without a pipeline, or the primary agent for a pipeline.
func (r *Runner) reviewAgent(p *Pipeline) (string, error) {
	if p == nil {
		if a, err := parseAgent(r.lastAgent); err == nil && a != "" {
			return a, nil
		}
	}
	plan, err := r.agentPlan()
	if err != nil {
		return "", err
	}
	return plan.Agents[0], nil
}

// runReviewCommand runs AMF_REVIEW_COMMAND with sh -c in wt, with the agent
// environment rules and the agent timeout. It returns the exit code, the
// duration and the redacted last 60 lines of combined output.
func (r *Runner) runReviewCommand(ctx context.Context, wt, agent string) (code int, duration time.Duration, output string, err error) {
	begin := time.Now()
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", r.ReviewCommand)
	cmd.Dir = wt
	cmd.Env = r.agentEnv(os.Environ(), agent, wt)
	var combined strings.Builder
	cmd.Stdout = &combined
	cmd.Stderr = &combined

	grace := r.KillGrace
	if grace <= 0 {
		grace = agentKillGrace
	}
	procGroup(cmd)
	exited := make(chan struct{})
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = killGroup(cmd.Process, syscall.SIGTERM)
			if !groupOutlivesLeader {
				return nil
			}
			killAfter(grace, exited, func() { _ = killGroup(cmd.Process, syscall.SIGKILL) })
		}
		return nil
	}
	cmd.WaitDelay = grace + 5*time.Second

	err = cmd.Run()
	close(exited)
	if cmd.Process != nil && groupOutlivesLeader {
		_ = killGroup(cmd.Process, syscall.SIGKILL)
	}
	code = -1
	if ps := cmd.ProcessState; ps != nil {
		code = ps.ExitCode()
	}
	duration = time.Since(begin)
	output = r.Redact.Redact(reviewTail(combined.String(), reviewTailLines))
	return code, duration, output, err
}

// reviewTail is the last n lines of s, without trailing blank lines.
func reviewTail(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// reviewPrompt is the review agent's prompt: the issue, and instructions to
// inspect the diff itself and end with exactly one verdict line.
func reviewPrompt(repo string, issue Issue, base string) string {
	body := strings.TrimSpace(issue.Body)
	if body == "" {
		body = "(no description)"
	}
	return fmt.Sprintf(`You are the pre-PR review gate for a git worktree of the GitHub repository %s.
Review the change that is meant to resolve this GitHub issue:

# #%d: %s

%s

Instructions:
- Inspect the change yourself: run git diff origin/%s HEAD. Do not expect it in this prompt.
- Check every acceptance criterion in the issue, and look for regressions, security problems, missing tests and missing docs.
- Do not create, edit or delete any file, and do not commit, push, create branches or open pull requests.
- Reply with your review in Markdown. List any blocking findings above the last line.
- End your reply with exactly one line: amf-review: pass if the change is ready, or amf-review: block if it isn't.
`, repo, issue.Number, issue.Title, body, base)
}

// parseReviewVerdict reads the review verdict from the last non-blank line.
// ok is false when there isn't one.
func parseReviewVerdict(out string) (pass, ok bool) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := strings.Trim(strings.TrimSpace(lines[len(lines)-1]), "*_`#> ")
	m := reviewVerdictLine.FindStringSubmatch(last)
	if m == nil {
		return false, false
	}
	return strings.EqualFold(m[1], "pass"), true
}

// stripReviewVerdict removes the verdict line from the reviewer's notes.
func stripReviewVerdict(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if _, ok := parseReviewVerdict(out); ok {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// comment is the issue comment when the gate blocks: the reasons, the
// command and the reviewer's findings, and where the commit is kept.
func (o reviewOutcome) comment(r *Runner, issue Issue, wt string) string {
	var b strings.Builder
	b.WriteString("amf's pre-PR review gate blocked the pull request, so amf pushed nothing and opened or updated no PR.\n\n")
	b.WriteString("**Reasons:**\n")
	for _, reason := range o.reasons {
		b.WriteString("- " + reason + "\n")
	}
	fmt.Fprintf(&b, "\nThe commit is kept on the local branch `%s` and in the worktree `%s`.\n", issueBranch(issue.Number), shownPath(r.Root, wt))
	if o.CommandRan {
		fmt.Fprintf(&b, "\n**Test command:** %s (exit %d after %s).\n", markdownCode(o.Command), o.CommandExit, o.CommandDuration.Round(10*time.Millisecond))
		if o.CommandOutput != "" {
			fence := "```"
			for strings.Contains(o.CommandOutput, fence) {
				fence += "`"
			}
			fmt.Fprintf(&b, "\n%stext\n%s\n%s\n", fence, o.CommandOutput, fence)
		}
	}
	if o.ReviewerRan {
		b.WriteString("\n**Reviewer's findings** (written by the agent; amf has not verified them):\n\n")
		if o.Findings == "" {
			b.WriteString("(no findings)\n")
		} else {
			b.WriteString(o.Findings + "\n")
		}
	}
	return b.String()
}

// section is the PR body's Review gate section, added only when the gate
// passes.
func (o reviewOutcome) section() string {
	var b strings.Builder
	b.WriteString("## Review gate\n\n")
	if o.CommandRan {
		fmt.Fprintf(&b, "_amf ran:_ %s (exit %d after %s).\n\n", markdownCode(o.Command), o.CommandExit, o.CommandDuration.Round(10*time.Millisecond))
	} else {
		b.WriteString("_amf ran:_ no test command (`AMF_REVIEW_COMMAND` is unset).\n\n")
	}
	b.WriteString("_Reviewer's notes (written by the agent; amf has not verified them):_\n\n")
	if o.Findings != "" {
		b.WriteString(o.Findings + "\n")
	}
	return b.String()
}

// markdownCode wraps s in enough backticks to keep it one code span.
func markdownCode(s string) string {
	fence := "`"
	for strings.Contains(s, fence) {
		fence += "`"
	}
	return fence + s + fence
}
