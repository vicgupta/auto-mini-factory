package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParsePipeline(t *testing.T) {
	md := "# Agents\n\nSome text.\n\n## amf\n\nIntro line.\n\n" +
		"| Agent | Stage | Skill | Model |\n" +
		"|:--|---|---|---|\n" +
		"| `claudecode` | plan | [plan](https://example.com/plan/SKILL.md) | claude-opus-5-5 |\n" +
		"| codex | Code | <https://example.com/code/SKILL.md#sha256=" + strings.Repeat("ab", 32) + "> | default |\n" +
		"| opencode | eval | - | |\n" +
		"\n| not | part | of | it |\n\n## Other\n\n| Stage | Agent |\n|---|---|\n| bogus | bogus |\n"
	p, err := parsePipeline(md)
	if err != nil {
		t.Fatal(err)
	}
	want := []Stage{
		{Name: "plan", Skill: "https://example.com/plan/SKILL.md", Agent: "claudecode", Model: "claude-opus-5-5", Row: 1},
		{Name: "code", Skill: "https://example.com/code/SKILL.md#sha256=" + strings.Repeat("ab", 32), Agent: "codex", Row: 2},
		{Name: "eval", Agent: "opencode", Row: 3},
	}
	for i, s := range p.stages() {
		if !reflect.DeepEqual(*s, want[i]) {
			t.Errorf("stage %d = %+v, want %+v", i, *s, want[i])
		}
	}

	if p, err := parsePipeline("# Agents\n\nNo table here.\n"); p != nil || err != nil {
		t.Fatalf("no section: got %v, %v", p, err)
	}

	table := func(rows ...string) string {
		return "## amf\n\n| Stage | Skill | Agent | Model |\n|---|---|---|---|\n" + strings.Join(rows, "\n") + "\n"
	}
	for _, tc := range []struct{ md, want string }{
		{"## amf\n\nnothing\n", "no table"},
		{"## amf\n\n| Stage | Model |\n|---|---|\n| code | x |\n", "no agent column"},
		{table("| plan | - | claudecode | |"), "no code stage"},
		{table("| code | - | claudecode | |", "| test | - | codex | |"), `row 2 (test): unknown stage "test"`},
		{table("| code | - | gemini | |"), `row 1 (code): unknown agent "gemini"`},
		{table("| code | - | codex | |", "| code | - | codex | |"), "listed twice"},
		{table("| code | http://example.com/SKILL.md | codex | |"), "not an https URL"},
		{table("| code | https://example.com/SKILL.md#v1 | codex | |"), "#sha256="},
	} {
		if _, err := parsePipeline(tc.md); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parsePipeline(%q) error = %v, want %q", tc.md, err, tc.want)
		}
	}
}

func TestParseVerdict(t *testing.T) {
	for _, tc := range []struct {
		out      string
		pass, ok bool
	}{
		{"Looks good.\n\nVERDICT: PASS", true, true},
		{"Broken.\nVERDICT: FAIL\n\n", false, true},
		{"**Verdict: pass**", true, true},
		{"`VERDICT: FAIL.`", false, true},
		{"VERDICT: PASS\nBut actually more text", false, false},
		{"", false, false},
	} {
		pass, ok := parseVerdict(tc.out)
		if pass != tc.pass || ok != tc.ok {
			t.Errorf("parseVerdict(%q) = %v, %v; want %v, %v", tc.out, pass, ok, tc.pass, tc.ok)
		}
	}
	if got := stripVerdict("Fix x.\n\nVERDICT: FAIL"); got != "Fix x." {
		t.Errorf("stripVerdict = %q", got)
	}
}

// skillServer serves skills over TLS, the way raw.githubusercontent.com does.
func skillServer(t *testing.T, skills map[string]string) *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("skill request carried credentials")
		}
		switch r.URL.Path {
		case "/redirect-http":
			http.Redirect(w, r, "http://example.com/SKILL.md", http.StatusFound)
		case "/big":
			w.Write(bytes.Repeat([]byte("x"), maxSkillSize+1))
		default:
			s, ok := skills[r.URL.Path]
			if !ok {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, s)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchSkill(t *testing.T) {
	srv := skillServer(t, map[string]string{"/plan/SKILL.md": "# Plan\n"})
	ctx := context.Background()
	got, err := fetchSkill(ctx, srv.Client(), srv.URL+"/plan/SKILL.md")
	if err != nil || got != "# Plan\n" {
		t.Fatalf("fetchSkill = %q, %v", got, err)
	}
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte("# Plan\n")))
	if _, err := fetchSkill(ctx, srv.Client(), srv.URL+"/plan/SKILL.md#sha256="+sum); err != nil {
		t.Fatalf("matching pin: %v", err)
	}
	for path, want := range map[string]string{
		"/plan/SKILL.md#sha256=" + strings.Repeat("0", 64): "doesn't match its sha256 pin",
		"/missing":       "404",
		"/big":           "larger than",
		"/redirect-http": "refusing to follow a redirect",
	} {
		if _, err := fetchSkill(ctx, srv.Client(), srv.URL+path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("fetchSkill(%s) error = %v, want %q", path, err, want)
		}
	}
}

// fakeAgent writes a shell script standing in for an agent CLI.
func fakeAgent(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// setupPipelineRepo is setupRepo with agentsMD committed as AGENTS.md on main.
func setupPipelineRepo(t *testing.T, agentsMD string) (root, origin string) {
	t.Helper()
	root, origin = setupRepo(t)
	os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(agentsMD), 0o644)
	mustGit(t, root, "add", "AGENTS.md")
	mustGit(t, root, "commit", "-m", "add pipeline")
	mustGit(t, root, "push", "origin", "main")
	return root, origin
}

const (
	// The plan stage, as claude: it must get the plan skill, and its stray
	// file must not survive.
	fakePlanClaude = `
prompt=$(cat)
case "$prompt" in *"plan stage"*"PLAN-SKILL"*) ;; *) echo "bad plan prompt: $prompt" >&2; exit 4;; esac
case "$*" in *"--model claude-opus-5-5"*) ;; *) echo "missing model: $*" >&2; exit 5;; esac
echo scratch > plan-scratch.txt
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"1. Add feature.txt\n2. Test it"}'
`
	// The code stage, as codex: round 2 must see eval's review.
	fakeCodeCodex = `
case "$*" in "exec --dangerously-bypass-approvals-and-sandbox -m gpt-test -o "*" -") ;; *) echo "bad args: $*" >&2; exit 5;; esac
if [ -n "$GITHUB_TOKEN$CLAUDE_CODE_OAUTH_TOKEN" ]; then echo "token leaked" >&2; exit 3; fi
prompt=$(cat)
case "$prompt" in *"<plan>"*"Add feature.txt"*"CODE-SKILL"*) ;; *) echo "bad code prompt: $prompt" >&2; exit 4;; esac
while [ "$1" != "-o" ]; do shift; done
case "$prompt" in
*"<review>"*"needs a newline"*) printf 'hello\n' > feature.txt; echo "Fixed the newline." > "$2" ;;
*) printf hello > feature.txt; echo "Added feature.txt." > "$2" ;;
esac
`
	// The eval stage, as opencode: it fails round 1, passes round 2, and its
	// stray file must not survive.
	fakeEvalOpencode = `
[ "$1" = run ] && [ "$2" = --model ] && [ "$3" = opencode/test ] || { echo "bad args: $*" >&2; exit 5; }
[ -f "$OPENCODE_CONFIG" ] || { echo "no OPENCODE_CONFIG" >&2; exit 6; }
prompt=$4
case "$prompt" in *"eval stage"*"+hello"*"EVAL-SKILL"*) ;; *) echo "bad eval prompt: $prompt" >&2; exit 4;; esac
out=$(printf '%s\n' "$prompt" | sed -n 's/.*final reply, exactly as you give it, to the file \(.*\)\.$/\1/p')
echo scratch > eval-scratch.txt
if [ "$(cat feature.txt)" = hello ] && [ "$(wc -l < feature.txt | tr -d ' ')" = 1 ]; then
	printf 'All criteria met.\n\nVERDICT: PASS\n' > "$out"
else
	printf -- '- **blocking** feature.txt needs a newline\n\nVERDICT: FAIL\n' > "$out"
fi
echo "opencode transcript"
`
)

func pipelineTable(skills string) string {
	return "# Agents\n\n## amf\n\n| Stage | Skill | Agent | Model |\n|---|---|---|---|\n" +
		"| plan | " + skills + "/plan/SKILL.md | claudecode | claude-opus-5-5 |\n" +
		"| code | " + skills + "/code/SKILL.md | codex | gpt-test |\n" +
		"| eval | " + skills + "/eval/SKILL.md | opencode | opencode/test |\n"
}

func newPipelineRunner(t *testing.T, gh *httptest.Server, skills *httptest.Server, root string) (*Runner, *bytes.Buffer) {
	r, log := newTestRunner(t, gh, root, fakeAgent(t, "claude", fakePlanClaude))
	r.Codex = fakeAgent(t, "codex", fakeCodeCodex)
	r.Opencode = fakeAgent(t, "opencode", fakeEvalOpencode)
	r.HTTP = skills.Client()
	r.EvalRounds = 2
	return r, log
}

var testSkills = map[string]string{"/plan/SKILL.md": "PLAN-SKILL", "/code/SKILL.md": "CODE-SKILL", "/eval/SKILL.md": "EVAL-SKILL"}

func TestPipelineRetriesUntilEvalPasses(t *testing.T) {
	skills := skillServer(t, testSkills)
	root, origin := setupPipelineRepo(t, pipelineTable(skills.URL))
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", Body: "Please add feature.txt", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	t.Setenv("GITHUB_TOKEN", "must-not-leak")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "claude-only")
	r, log := newPipelineRunner(t, srv, skills, root)

	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	if len(gh.prs) != 1 {
		t.Fatalf("expected one PR, got %v", gh.prs)
	}
	body := gh.prs[0]["body"]
	for _, want := range []string{"Fixed the newline.", "## Evaluation", "**PASS** by opencode (opencode/test) after 2 code rounds", "All criteria met.", "<summary>Plan (claudecode, claude-opus-5-5)</summary>", "1. Add feature.txt", "Closes #1"} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "VERDICT") {
		t.Errorf("PR body should not carry the verdict line:\n%s", body)
	}

	// One commit, with only the code stage's change.
	if got := mustGit(t, root, "--git-dir", origin, "log", "--format=%s", "main..amf/issue-1"); got != "Fix #1: Add feature file" {
		t.Fatalf("branch commits = %q", got)
	}
	if got := mustGit(t, root, "--git-dir", origin, "diff", "--name-only", "main", "amf/issue-1"); got != "feature.txt" {
		t.Fatalf("branch changes = %q", got)
	}
	if got, want := gh.labels(1), []string{"amf:done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	for _, name := range []string{"logs/issue-1-plan.log", "logs/issue-1-code-1.log", "logs/issue-1-eval-1.log", "logs/issue-1-code-2.log", "logs/issue-1-eval-2.log", "runs/issue-1/plan.md", "runs/issue-1/eval-1.md", "runs/issue-1/eval-2.md"} {
		if _, err := os.Stat(filepath.Join(root, ".amf", name)); err != nil {
			t.Errorf("missing .amf/%s", name)
		}
	}
	if b := mustRead(t, filepath.Join(root, ".amf", "runs", "issue-1", "eval-1.md")); !strings.Contains(b, "needs a newline") {
		t.Errorf("eval-1.md = %q", b)
	}
}

func TestPipelineFailsWhenEvalNeverPasses(t *testing.T) {
	skills := skillServer(t, testSkills)
	root, origin := setupPipelineRepo(t, pipelineTable(skills.URL))
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	r, log := newPipelineRunner(t, srv, skills, root)
	r.Opencode = fakeAgent(t, "opencode", `
out=$(printf '%s\n' "$4" | sed -n 's/.*final reply, exactly as you give it, to the file \(.*\)\.$/\1/p')
printf -- '- **blocking** still wrong\n\nVERDICT: FAIL\n' > "$out"
`)
	r.EvalRounds = 1

	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "after 2 code rounds") {
		t.Fatalf("expected eval failure, got %v\nlog:\n%s", err, log)
	}
	if len(gh.prs) != 0 {
		t.Fatalf("no PR expected, got %v", gh.prs)
	}
	if got, want := gh.labels(1), []string{"amf:failed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	c := gh.comments[1]
	if len(c) != 1 || !strings.Contains(c[0], "still found problems after 2 code rounds") || !strings.Contains(c[0], "still wrong") || strings.Contains(c[0], "```") {
		t.Fatalf("comments = %q", c)
	}
	// The last attempt is pushed for inspection.
	if got := mustGit(t, root, "--git-dir", origin, "show", "amf/issue-1:feature.txt"); got != "hello" {
		t.Fatalf("pushed feature.txt = %q", got)
	}
}

func TestPipelinePlanBlocked(t *testing.T) {
	skills := skillServer(t, testSkills)
	root, _ := setupPipelineRepo(t, pipelineTable(skills.URL))
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Make it nicer", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	r, _ := newPipelineRunner(t, srv, skills, root)
	r.Claude = fakeAgent(t, "claude", `cat >/dev/null; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"BLOCKED: nicer how?\n\n- Which page?"}'`)
	r.Codex = fakeAgent(t, "codex", `echo "code must not run" >&2; exit 9`)

	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "BLOCKED: nicer how?") {
		t.Fatalf("expected blocked plan, got %v", err)
	}
	if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "needs answers") || !strings.Contains(c[0], "- Which page?") {
		t.Fatalf("comments = %q", c)
	}
}

func TestPipelineMissingAgentLeavesIssueAlone(t *testing.T) {
	root, _ := setupPipelineRepo(t, "## amf\n\n| Stage | Agent |\n|---|---|\n| code | codex |\n")
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Anything", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	r, _ := newTestRunner(t, srv, root, "claude")
	r.Codex = filepath.Join(t.TempDir(), "no-codex")

	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "the code stage uses codex, but") {
		t.Fatalf("expected missing agent, got %v", err)
	}
	if got, want := gh.labels(1), []string{"amf:ready"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	if len(gh.comments[1]) != 0 {
		t.Fatalf("comments = %q", gh.comments[1])
	}
}

func TestPipelineCodeOnly(t *testing.T) {
	root, _ := setupPipelineRepo(t, "## amf\n\n| Stage | Skill | Agent | Model |\n|---|---|---|---|\n| code | - | claudecode | claude-sonnet-5-5 |\n")
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add it", State: "open"}
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", `
case "$*" in *"--model claude-sonnet-5-5"*) ;; *) echo "missing model: $*" >&2; exit 5;; esac
cat >/dev/null; echo hi > it.txt
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"Added it."}'
`))
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	body := gh.prs[0]["body"]
	if !strings.HasPrefix(body, "Added it.\n\nCloses #1") || strings.Contains(body, "Evaluation") {
		t.Fatalf("PR body = %q", body)
	}
}

func TestPipelineCommand(t *testing.T) {
	root, _ := setupRepo(t)
	t.Chdir(root)
	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"pipeline"}, nil, &stdout, io.Discard); err != nil || !strings.Contains(stdout.String(), "No AGENTS.md") {
		t.Fatalf("no AGENTS.md: %v, %q", err, stdout.String())
	}

	os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("## amf\n\n| Stage | Agent | Model |\n|---|---|---|\n| code | claudecode | |\n| eval | codex | gpt-test |\n"), 0o644)
	t.Setenv("AMF_CLAUDE", fakeAgent(t, "claude", "exit 0"))
	t.Setenv("AMF_CODEX", filepath.Join(t.TempDir(), "no-codex"))
	stdout.Reset()
	err := run(context.Background(), []string{"pipeline"}, nil, &stdout, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "1 problem") {
		t.Fatalf("expected a missing codex, got %v", err)
	}
	out := stdout.String()
	for _, want := range []string{"STAGE", "code   claudecode  default model  -", "eval   codex       gpt-test", "agent claudecode  ok:", "agent codex       missing:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestAgentEnv(t *testing.T) {
	env := []string{"PATH=/bin", "PWD=/somewhere/else", "GITHUB_TOKEN=gh", "CLAUDE_CODE_OAUTH_TOKEN=old", "OPENCODE_CONFIG=/x", "ANTHROPIC_API_KEY=key"}
	r := &Runner{ClaudeToken: "oat"}
	for agent, want := range map[string][]string{
		agentClaude:   {"PATH=/bin", "OPENCODE_CONFIG=/x", "CLAUDE_CODE_OAUTH_TOKEN=oat", "PWD=/wt"},
		agentCodex:    {"PATH=/bin", "ANTHROPIC_API_KEY=key", "PWD=/wt"},
		agentOpencode: {"PATH=/bin", "ANTHROPIC_API_KEY=key", "PWD=/wt"},
	} {
		if got := r.agentEnv(env, agent, "/wt"); !reflect.DeepEqual(got, want) {
			t.Errorf("agentEnv(%s) = %v, want %v", agent, got, want)
		}
	}
}
