package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// A pipeline is declared in the target repo's AGENTS.md as a table under an
// "## amf" heading. Each row is a stage: plan, code or eval, with a skill URL,
// a coding agent and a model.
const (
	agentClaude   = "claudecode"
	agentCodex    = "codex"
	agentOpencode = "opencode"

	maxSkillSize = 256 << 10
	maxDiffSize  = 100 << 10
)

var agentCommand = map[string]string{agentClaude: "claude", agentCodex: "codex", agentOpencode: "opencode"}

// Stage is one row of the pipeline table.
type Stage struct {
	Name  string // plan, code or eval
	Skill string // https URL of a SKILL.md, or "" for none
	Agent string // claudecode, codex or opencode
	Model string // "" for the agent's default
	Row   int    // 1-based data row, for error messages

	skill string // the downloaded skill text
}

func (s *Stage) model() string {
	if s.Model == "" {
		return "default model"
	}
	return s.Model
}

// Pipeline is the stages a repo declares. Code is always set.
type Pipeline struct {
	Plan, Code, Eval *Stage
}

func (p *Pipeline) stages() []*Stage {
	var out []*Stage
	for _, s := range []*Stage{p.Plan, p.Code, p.Eval} {
		if s != nil {
			out = append(out, s)
		}
	}
	return out
}

var amfHeading = regexp.MustCompile(`(?i)^##\s+amf\s*$`)

// parsePipeline reads the "## amf" table from the contents of AGENTS.md. It
// returns nil when there is no such section.
func parsePipeline(md string) (*Pipeline, error) {
	var rows [][]string
	in, found, fenced := false, false, false
	// Split rather than scan: a bufio.Scanner stops at a 64 KB line.
	for _, line := range strings.Split(md, "\n") {
		line = strings.TrimSpace(line)
		// Fenced code blocks hold examples, not the pipeline.
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			fenced = !fenced
			if in && len(rows) > 0 {
				break
			}
			continue
		}
		if fenced {
			continue
		}
		switch {
		case amfHeading.MatchString(line):
			in, found = true, true
			continue
		case in && strings.HasPrefix(line, "#"):
			in = false
		}
		if !in {
			continue
		}
		if strings.HasPrefix(line, "|") {
			rows = append(rows, splitRow(line))
		} else if len(rows) > 0 {
			break // the first table has ended
		}
	}
	if !found {
		return nil, nil
	}
	if len(rows) < 2 {
		return nil, errors.New("AGENTS.md has an ## amf section but no table under it")
	}

	cols := map[string]int{}
	for i, h := range rows[0] {
		cols[strings.ToLower(h)] = i
	}
	for _, c := range []string{"stage", "agent"} {
		if _, ok := cols[c]; !ok {
			return nil, fmt.Errorf("the ## amf table in AGENTS.md has no %s column", c)
		}
	}
	cell := func(row []string, col string) string {
		if i, ok := cols[col]; ok && i < len(row) {
			return row[i]
		}
		return ""
	}

	p := &Pipeline{}
	data := 0
	for _, row := range rows[1:] {
		if isSeparator(row) {
			continue
		}
		data++
		s := &Stage{
			Name:  strings.ToLower(cell(row, "stage")),
			Skill: cell(row, "skill"),
			Agent: strings.ToLower(cell(row, "agent")),
			Model: cell(row, "model"),
			Row:   data,
		}
		if s.Skill == "-" {
			s.Skill = ""
		}
		if strings.EqualFold(s.Model, "default") || s.Model == "-" {
			s.Model = ""
		}
		rowErr := func(format string, a ...any) error {
			return fmt.Errorf("AGENTS.md ## amf table, row %d (%s): %s", s.Row, s.Name, fmt.Sprintf(format, a...))
		}
		var slot **Stage
		switch s.Name {
		case "plan":
			slot = &p.Plan
		case "code":
			slot = &p.Code
		case "eval":
			slot = &p.Eval
		default:
			return nil, rowErr("unknown stage %q; use plan, code or eval", s.Name)
		}
		if *slot != nil {
			return nil, rowErr("the %s stage is listed twice", s.Name)
		}
		if _, ok := agentCommand[s.Agent]; !ok {
			return nil, rowErr("unknown agent %q; use claudecode, codex or opencode", s.Agent)
		}
		if s.Skill != "" {
			if _, _, err := skillURL(s.Skill); err != nil {
				return nil, rowErr("%v", err)
			}
		}
		*slot = s
	}
	if p.Code == nil {
		return nil, errors.New("the ## amf table in AGENTS.md has no code stage")
	}
	return p, nil
}

// splitRow splits a Markdown table row into trimmed cells. A cell may wrap
// its value in backticks, angle brackets or a [text](url) link.
func splitRow(line string) []string {
	line = strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|")
	link := regexp.MustCompile(`^\[[^\]]*\]\(([^)]*)\)$`)
	var cells []string
	for _, c := range strings.Split(line, "|") {
		c = strings.Trim(strings.TrimSpace(c), "`")
		if m := link.FindStringSubmatch(c); m != nil {
			c = m[1]
		}
		cells = append(cells, strings.TrimSuffix(strings.TrimPrefix(c, "<"), ">"))
	}
	return cells
}

func isSeparator(row []string) bool {
	for _, c := range row {
		if strings.Trim(c, ":- ") != "" {
			return false
		}
	}
	return true
}

// skillURL validates a skill URL and splits off its optional #sha256= pin.
func skillURL(raw string) (fetch, pin string, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return "", "", fmt.Errorf("skill %q is not an https URL", raw)
	}
	if u.User != nil {
		return "", "", fmt.Errorf("skill %q must not contain credentials", u.Redacted())
	}
	if u.Fragment != "" {
		pin, ok := strings.CutPrefix(u.Fragment, "sha256=")
		if b, err := hex.DecodeString(pin); !ok || err != nil || len(b) != sha256.Size {
			return "", "", fmt.Errorf("skill %q: the only fragment allowed is #sha256=<64 hex digits>", raw)
		}
		u.Fragment = ""
		return u.String(), strings.ToLower(pin), nil
	}
	return u.String(), "", nil
}

// fetchSkill downloads a skill. It sends no credentials, follows only https
// redirects, and checks the size, encoding and any sha256 pin.
func fetchSkill(ctx context.Context, client *http.Client, raw string) (string, error) {
	target, pin, err := skillURL(raw)
	if err != nil {
		return "", err
	}
	c := http.Client{Timeout: 30 * time.Second}
	if client != nil {
		c = *client
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return fmt.Errorf("refusing to follow a redirect to %s", req.URL)
		}
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading skill %s: %w", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading skill %s: %s", target, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxSkillSize+1))
	if err != nil {
		return "", fmt.Errorf("downloading skill %s: %w", target, err)
	}
	if len(b) > maxSkillSize {
		return "", fmt.Errorf("skill %s is larger than %d KB", target, maxSkillSize>>10)
	}
	if !utf8.Valid(b) {
		return "", fmt.Errorf("skill %s is not UTF-8 text", target)
	}
	if pin != "" {
		if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != pin {
			return "", fmt.Errorf("skill %s doesn't match its sha256 pin; it has changed since it was pinned", target)
		}
	}
	return string(b), nil
}

// loadPipeline reads the pipeline from AGENTS.md on origin/base and checks
// that its agents are installed. It returns nil when the repo declares none.
func (r *Runner) loadPipeline(ctx context.Context, base string) (*Pipeline, error) {
	p, err := readPipeline(ctx, r.Root, base)
	if p == nil || err != nil {
		return p, err
	}
	for _, s := range p.stages() {
		if _, err := exec.LookPath(r.agentBin(s.Agent)); err != nil {
			return nil, fmt.Errorf("the %s stage uses %s, but %s is not installed (%v)", s.Name, s.Agent, r.agentBin(s.Agent), err)
		}
	}
	return p, nil
}

// readPipeline fetches base and parses the pipeline in AGENTS.md on
// origin/base. It returns nil when the repo declares none.
func readPipeline(ctx context.Context, root, base string) (*Pipeline, error) {
	if _, err := git(ctx, root, "fetch", "origin", base); err != nil {
		return nil, err
	}
	ref := "origin/" + base + ":AGENTS.md"
	if _, err := git(ctx, root, "cat-file", "-e", ref); err != nil {
		return nil, nil
	}
	md, err := git(ctx, root, "show", ref)
	if err != nil {
		return nil, err
	}
	return parsePipeline(md)
}

func (r *Runner) agentBin(agent string) string {
	switch agent {
	case agentCodex:
		return r.Codex
	case agentOpencode:
		return r.Opencode
	default:
		return r.Claude
	}
}

// stageError is a failure with its own comment for the issue, in place of
// the generic one.
type stageError struct {
	msg     string
	comment string
}

func (e *stageError) Error() string { return e.msg }

// runPipeline runs the plan, code and eval stages in the worktree and leaves
// the branch with one commit. It returns the PR summary and the sections
// that follow it in the PR body.
func (r *Runner) runPipeline(ctx context.Context, wt string, issue Issue, branch, base string, p *Pipeline) (summary, extra string, err error) {
	n := issue.Number
	runDir := filepath.Join(r.Root, ".amf", "runs", fmt.Sprintf("issue-%d", n))
	if err := os.RemoveAll(runDir); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return "", "", err
	}
	// Agents write their final message under the worktree's .amf/, which git ignores.
	if err := excludeFromGit(ctx, r.Root, "amf worktrees and logs", "/.amf/"); err != nil {
		return "", "", err
	}
	for _, s := range p.stages() {
		if s.Skill == "" {
			continue
		}
		r.logf("#%d: downloading the %s skill from %s", n, s.Name, s.Skill)
		if s.skill, err = fetchSkill(ctx, r.HTTP, s.Skill); err != nil {
			return "", "", err
		}
	}

	var plan string
	if p.Plan != nil {
		out, err := r.runReadOnly(ctx, p.Plan, n, "plan", wt, planPrompt(r.Repo, branch, issue, p.Plan))
		if err != nil {
			return "", "", err
		}
		plan = cleanSummary(out, n)
		if err := os.WriteFile(filepath.Join(runDir, "plan.md"), []byte(plan+"\n"), 0o644); err != nil {
			return "", "", err
		}
		if first, ok := blocked(plan); ok {
			return "", "", &stageError{
				msg: fmt.Sprintf("the plan stage needs answers first: %s", first),
				comment: fmt.Sprintf("amf's plan stage (%s, %s) needs answers before it can work on this issue. "+
					"Edit the issue to answer them, then add the `%s` label to retry.\n\n%s", p.Plan.Agent, p.Plan.model(), labelReady, plan),
			}
		}
	}

	var review string
	rounds := 0
	for k := 1; ; k++ {
		rounds = k
		name := fmt.Sprintf("code-%d", k)
		out, err := r.runStage(ctx, p.Code, n, name, wt, codePrompt(r.Repo, branch, issue, p.Code, plan, review))
		if err != nil {
			return "", "", err
		}
		summary = out
		if err := commitRound(ctx, wt, k, base, p.Code); err != nil {
			return "", "", err
		}
		if p.Eval == nil {
			break
		}

		diff, err := git(ctx, wt, "diff", "origin/"+base, "HEAD")
		if err != nil {
			return "", "", err
		}
		name = fmt.Sprintf("eval-%d", k)
		out, err = r.runReadOnly(ctx, p.Eval, n, name, wt, evalPrompt(r.Repo, branch, base, issue, p.Eval, plan, summary, diff))
		if err != nil {
			return "", "", err
		}
		out = cleanSummary(out, n)
		if err := os.WriteFile(filepath.Join(runDir, name+".md"), []byte(out+"\n"), 0o644); err != nil {
			return "", "", err
		}
		pass, ok := parseVerdict(out)
		review = stripVerdict(out)
		if !ok {
			review = "eval gave no verdict.\n\n" + review
		}
		if pass {
			r.logf("#%d: eval passed in round %d", n, k)
			extra = fmt.Sprintf("## Evaluation\n\n**PASS** by %s (%s) after %d code %s.\n\n<details><summary>Review</summary>\n\n%s\n\n</details>",
				p.Eval.Agent, p.Eval.model(), k, plural(k, "round", "rounds"), review)
			break
		}
		if k > r.EvalRounds {
			tried := fmt.Sprintf("%d code %s", k, plural(k, "round", "rounds"))
			r.logf("#%d: eval still failing after %s; pushing %s for inspection", n, tried, branch)
			if _, err := git(ctx, wt, "push", "--force", "-u", "origin", branch); err != nil {
				return "", "", err
			}
			return "", "", &stageError{
				msg: fmt.Sprintf("the eval stage still found problems after %s; the last attempt is on %s", tried, branch),
				comment: fmt.Sprintf("amf's eval stage (%s, %s) still found problems after %s, so no pull request was opened. "+
					"The last attempt is on the branch `%s`.\n\nTo retry, edit the issue if needed and add the `%s` label.\n\n"+
					"<details><summary>Last review</summary>\n\n%s\n\n</details>", p.Eval.Agent, p.Eval.model(), tried, branch, labelReady, review),
			}
		}
		r.logf("#%d: eval found problems; starting code round %d", n, k+1)
	}

	// One commit per PR, however many rounds it took.
	if _, err := git(ctx, wt, "reset", "--soft", "origin/"+base); err != nil {
		return "", "", err
	}
	title := fmt.Sprintf("Fix #%d: %s", n, issue.Title)
	if _, err := git(ctx, wt, "commit", "-m", title, "-m", fmt.Sprintf("Closes #%d", n)); err != nil {
		return "", "", err
	}
	if p.Eval == nil {
		r.logf("#%d: code finished in %d %s (no eval stage)", n, rounds, plural(rounds, "round", "rounds"))
	}
	if plan != "" {
		if extra != "" {
			extra += "\n\n"
		}
		extra += fmt.Sprintf("<details><summary>Plan (%s, %s)</summary>\n\n%s\n\n</details>", p.Plan.Agent, p.Plan.model(), plan)
	}
	return summary, extra, nil
}

// blocked reports whether a plan starts with "BLOCKED:", ignoring case and
// Markdown emphasis, and returns its first line.
func blocked(plan string) (string, bool) {
	first, _, _ := strings.Cut(plan, "\n")
	first = strings.TrimSpace(first)
	t := strings.TrimLeft(first, "*_`#> ")
	return first, len(t) >= 8 && strings.EqualFold(t[:8], "blocked:")
}

// runReadOnly runs a stage that must not change the branch, then discards
// whatever it changed anyway.
func (r *Runner) runReadOnly(ctx context.Context, s *Stage, n int, name, wt, prompt string) (string, error) {
	head, err := git(ctx, wt, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	out, runErr := r.runStage(ctx, s, n, name, wt, prompt)
	if _, err := git(ctx, wt, "reset", "--hard", head); err != nil {
		return "", err
	}
	if _, err := git(ctx, wt, "clean", "-fd"); err != nil {
		return "", err
	}
	return out, runErr
}

func (r *Runner) runStage(ctx context.Context, s *Stage, n int, name, wt, prompt string) (string, error) {
	model := r.agentModel(s.Agent, s.Model)
	shown := model
	if shown == "" {
		shown = "default model"
	}
	r.logf("#%d: %s: running %s (%s, timeout %s)", n, name, s.Agent, shown, r.Timeout)
	log := filepath.Join(r.Root, ".amf", "logs", fmt.Sprintf("issue-%d-%s.log", n, name))
	return r.runAgent(ctx, s.Agent, model, wt, prompt, log, filepath.Join(wt, ".amf", name+".md"))
}

// logIssue is the issue number in a log file's name, issue-N.log or
// issue-N-stage.log.
func logIssue(logPath string) int {
	name := strings.TrimPrefix(filepath.Base(logPath), "issue-")
	end := strings.IndexFunc(name, func(r rune) bool { return r < '0' || r > '9' })
	if end < 0 {
		end = len(name)
	}
	n, _ := strconv.Atoi(name[:end])
	return n
}

// commitRound commits a code round and fails if the branch still doesn't
// change anything relative to base.
func commitRound(ctx context.Context, wt string, k int, base string, s *Stage) error {
	if _, err := git(ctx, wt, "add", "-A"); err != nil {
		return err
	}
	status, err := git(ctx, wt, "status", "--porcelain")
	if err != nil {
		return err
	}
	if status != "" {
		if _, err := git(ctx, wt, "commit", "-m", fmt.Sprintf("amf: code round %d", k)); err != nil {
			return err
		}
	}
	changed, err := git(ctx, wt, "diff", "--name-only", "origin/"+base, "HEAD")
	if err != nil {
		return err
	}
	if changed == "" {
		return fmt.Errorf("the code stage (%s) finished without making any changes", s.Agent)
	}
	return nil
}

// runAgent runs a coding agent headless in wt and returns its final message.
// Its output streams to logPath. Agents that can't print just their final
// message write it to outPath instead. Claude doesn't need outPath.
func (r *Runner) runAgent(ctx context.Context, agent, model, wt, prompt, logPath, outPath string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return "", err
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		return "", err
	}
	defer logFile.Close()
	if outPath != "" {
		if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
			return "", err
		}
		os.Remove(outPath)
	}

	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	var cmd *exec.Cmd
	switch agent {
	case agentCodex:
		args := []string{"exec", "--dangerously-bypass-approvals-and-sandbox"}
		if model != "" {
			args = append(args, "-m", model)
		}
		cmd = exec.CommandContext(ctx, r.Codex, append(args, "-o", outPath, "-")...)
		cmd.Stdin = strings.NewReader(prompt)
	case agentOpencode:
		cfg, err := r.opencodeConfig()
		if err != nil {
			return "", err
		}
		args := []string{"run"}
		if model != "" {
			args = append(args, "--model", model)
		}
		// The prompt goes on stdin: as an argument, a long one (with a big
		// diff or skill) would exceed Linux's 128 KB limit per argument.
		prompt += fmt.Sprintf("\nWhen you are done, also write your final reply, exactly as you give it, to the file %s.\n", outPath)
		cmd = exec.CommandContext(ctx, r.Opencode, args...)
		cmd.Stdin = strings.NewReader(prompt)
		cmd.Env = append(r.agentEnv(os.Environ(), agent, wt), "OPENCODE_CONFIG="+cfg)
	default:
		// stream-json writes one JSON message per line as Claude works, so the
		// log fills in live; it requires --verbose in print mode.
		args := []string{"-p", "--dangerously-skip-permissions", "--output-format", "stream-json", "--verbose"}
		if model != "" {
			args = append(args, "--model", model)
		}
		cmd = exec.CommandContext(ctx, r.Claude, args...)
		cmd.Stdin = strings.NewReader(prompt)
	}
	if cmd.Env == nil {
		cmd.Env = r.agentEnv(os.Environ(), agent, wt)
	}
	cmd.Dir = wt
	var stdout bytes.Buffer
	var logOut io.Writer = logFile
	if r.DB != nil {
		dbOut := r.DB.agentWriter(logIssue(logPath), agentName(agent), filepath.Base(logPath))
		defer dbOut.Flush()
		logOut = io.MultiWriter(logFile, dbOut)
	}
	cmd.Stdout = io.MultiWriter(&stdout, logOut)
	cmd.Stderr = logOut
	cmd.WaitDelay = 10 * time.Second

	name := agentCommand[agent]
	if name == "" {
		name = "claude"
	}
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("%s timed out after %s (log: %s)", name, r.Timeout, logPath)
		}
		return "", fmt.Errorf("%s: %v (log: %s)", name, err, logPath)
	}
	switch agent {
	case agentCodex, agentOpencode:
		b, err := os.ReadFile(outPath)
		if err == nil && strings.TrimSpace(string(b)) != "" {
			return strings.TrimSpace(string(b)), nil
		}
		if agent == agentCodex {
			return "", fmt.Errorf("codex wrote no final message (log: %s)", logPath)
		}
		// opencode didn't write the file; fall back to what it printed.
		return strings.TrimSpace(stdout.String()), nil
	default:
		return parseClaudeResult(stdout.Bytes())
	}
}

// agentEnv is the environment for an agent running in wt. No agent gets
// GITHUB_TOKEN, and only Claude gets CLAUDE_CODE_OAUTH_TOKEN. PWD is set to
// wt because some agents (opencode) resolve relative paths from it rather
// than from their working directory.
func (r *Runner) agentEnv(env []string, agent, wt string) []string {
	if agent == agentClaude {
		env = r.claudeEnv(env)
	} else {
		env = withoutEnv(env, "GITHUB_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "OPENCODE_CONFIG")
	}
	if agent == agentCodex && r.OpenAIKey != "" {
		env = append(withoutEnv(env, "OPENAI_API_KEY"), "OPENAI_API_KEY="+r.OpenAIKey)
	}
	return append(withoutEnv(env, "PWD"), "PWD="+wt)
}

// opencodeConfig writes the opencode config amf runs it with: every tool
// allowed, since nobody is there to approve one. It lives under .amf/, so it
// never reaches the worktree.
func (r *Runner) opencodeConfig() (string, error) {
	path := filepath.Join(r.Root, ".amf", "opencode.json")
	const cfg = `{
  "$schema": "https://opencode.ai/config.json",
  "permission": {"edit": "allow", "bash": "allow", "webfetch": "allow", "external_directory": "allow"}
}
`
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	return path, os.WriteFile(path, []byte(cfg), 0o644)
}

var verdictLine = regexp.MustCompile(`(?i)^verdict:\s*(pass|fail)\.?$`)

// parseVerdict reads eval's verdict from its last non-blank line. ok is
// false when there isn't one.
func parseVerdict(out string) (pass, ok bool) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := strings.Trim(strings.TrimSpace(lines[len(lines)-1]), "*_`#> ")
	m := verdictLine.FindStringSubmatch(last)
	if m == nil {
		return false, false
	}
	return strings.EqualFold(m[1], "pass"), true
}

// stripVerdict removes the verdict line from eval's review.
func stripVerdict(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if _, ok := parseVerdict(out); ok {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// describePipeline is a one-line summary of the stages, for progress output.
func describePipeline(p *Pipeline) string {
	var parts []string
	for _, s := range p.stages() {
		parts = append(parts, fmt.Sprintf("%s (%s, %s)", s.Name, s.Agent, s.model()))
	}
	return strings.Join(parts, " -> ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func issueText(issue Issue) string {
	body := strings.TrimSpace(issue.Body)
	if body == "" {
		body = "(no description)"
	}
	return fmt.Sprintf("<issue>\n# #%d: %s\n\n%s\n</issue>", issue.Number, issue.Title, body)
}

func skillText(s *Stage) string {
	if s.skill == "" {
		return ""
	}
	return fmt.Sprintf("\nFollow this skill (from %s). Where it conflicts with the instructions above, the instructions above win.\n\n<skill>\n%s\n</skill>\n", s.Skill, strings.TrimSpace(s.skill))
}

func planPrompt(repo, branch string, issue Issue, s *Stage) string {
	return fmt.Sprintf(`You are the plan stage of amf, working in a git worktree of the GitHub repository %s, on branch %s.
Plan how to resolve this GitHub issue:

%s

Instructions:
- Read the code, but do not create, edit or delete files. amf discards any file changes made while planning.
- Do not commit, push, create branches or open pull requests.
- Reply with only the plan in Markdown.
- If the issue can't be resolved as written, make the first line of your reply start with "BLOCKED:", followed by the questions that need answers.
%s`, repo, branch, issueText(issue), skillText(s))
}

func codePrompt(repo, branch string, issue Issue, s *Stage, plan, review string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are working in a git worktree of the GitHub repository %s, on branch %s.\nResolve this GitHub issue:\n\n%s\n", repo, branch, issueText(issue))
	if plan != "" {
		fmt.Fprintf(&b, "\nFollow this plan from amf's plan stage:\n\n<plan>\n%s\n</plan>\n", plan)
	}
	if review != "" {
		fmt.Fprintf(&b, "\nYour previous attempt is already in the working tree. amf's eval stage reviewed it and found problems. Fix them:\n\n<review>\n%s\n</review>\n", review)
	}
	b.WriteString(`
Instructions:
- Make the smallest complete change that resolves the issue, following the existing code style.
- Find and run the project's tests, build and linters. Fix any failures before you finish.
- Do not commit, push, create branches or open pull requests. amf handles git and GitHub.
- When you are done, reply with only a pull request description in Markdown:
  a short summary of what changed and why, and how you verified it.
`)
	b.WriteString(skillText(s))
	return b.String()
}

func evalPrompt(repo, branch, base string, issue Issue, s *Stage, plan, summary, diff string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the eval stage of amf, working in a git worktree of the GitHub repository %s, on branch %s.\nReview the change on this branch that is meant to resolve this GitHub issue:\n\n%s\n", repo, branch, issueText(issue))
	if plan != "" {
		fmt.Fprintf(&b, "\nThe plan it was meant to follow:\n\n<plan>\n%s\n</plan>\n", plan)
	}
	fmt.Fprintf(&b, "\nThe author's description of the change:\n\n<description>\n%s\n</description>\n", strings.TrimSpace(summary))
	note := ""
	if len(diff) > maxDiffSize {
		// Drop a character split by the cut.
		diff, note = strings.ToValidUTF8(diff[:maxDiffSize], ""), " It is cut short here; run the git diff command to see the rest."
	}
	fmt.Fprintf(&b, "\nThe change, as `git diff origin/%s HEAD`.%s\n\n<diff>\n%s\n</diff>\n", base, note, diff)
	b.WriteString(`
Instructions:
- Run the project's tests, build and linters to check the change.
- Do not create, edit or delete files, and do not change anything with git. amf discards any file changes made during evaluation.
- Reply with your review in Markdown. If the change isn't ready, say specifically what to fix.
- End your reply with a line that is exactly "VERDICT: PASS" if the change is ready for a pull request, or "VERDICT: FAIL" if it isn't.
`)
	b.WriteString(skillText(s))
	return b.String()
}
