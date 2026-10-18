package main

// Edge cases: odd input, failing agents and GitHub, re-runs, and the stages
// of a pipeline misbehaving. Fake agents are shell scripts; GitHub is the
// in-memory fake from amf_test.go.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// shResult defines result(), which prints a successful stream-json result
// message. Its argument must not contain quotes; \n is a newline.
const shResult = `result() { printf '{"type":"result","subtype":"success","is_error":false,"result":"%s"}\n' "$1"; }
`

// stageClaude is a fake claude that runs plan, code or eval, depending on
// which stage's prompt it gets on stdin, as $prompt.
func stageClaude(t *testing.T, plan, code, eval string) string {
	t.Helper()
	return fakeAgent(t, "claude", shResult+`prompt=$(cat)
case "$prompt" in
*"You are the plan stage"*)
`+plan+`
;;
*"You are the eval stage"*)
`+eval+`
;;
*)
`+code+`
;;
esac
`)
}

// claudeTable is an ## amf table with the given stages, all run by claude.
func claudeTable(stages ...string) string {
	md := "## amf\n\n| Stage | Agent |\n|---|---|\n"
	for _, s := range stages {
		md += "| " + s + " | claudecode |\n"
	}
	return md
}

func readyIssue(n int, title string) *Issue {
	return &Issue{Number: n, Title: title, State: "open", Labels: []Label{{Name: "amf:ready"}}}
}

// --- AGENTS.md, skills and verdicts ---

func TestParsePipelineEdgeCases(t *testing.T) {
	parse := func(md string) *Pipeline {
		t.Helper()
		p, err := parsePipeline(md)
		if err != nil || p == nil {
			t.Fatalf("parsePipeline(%q) = %v, %v", md, p, err)
		}
		return p
	}

	// Heading in any case with trailing spaces, CRLF line endings, an extra
	// column, upper-case values, and no Skill or Model column.
	p := parse("## AMF  \r\n\r\n| STAGE | Notes | AGENT |\r\n|---|---|---|\r\n| CODE | whatever | Codex |\r\n")
	if want := (Stage{Name: "code", Agent: "codex", Row: 1}); !reflect.DeepEqual(*p.Code, want) {
		t.Errorf("code = %+v, want %+v", *p.Code, want)
	}
	// A row with fewer cells than the header leaves the rest empty.
	if p := parse("## amf\n\n| Stage | Agent | Model |\n|--|--|--|\n| code | opencode |\n"); p.Code.Model != "" || p.Code.Agent != "opencode" {
		t.Errorf("short row = %+v", *p.Code)
	}
	// An upper-case pin is accepted.
	pin := strings.Repeat("AB", 32)
	if p := parse("## amf\n\n| Stage | Skill | Agent |\n|---|---|---|\n| code | https://example.com/s/SKILL.md?ref=v1#sha256=" + pin + " | codex |\n"); !strings.HasSuffix(p.Code.Skill, pin) {
		t.Errorf("pinned skill = %q", p.Code.Skill)
	}

	// Examples in fenced code blocks are documentation, not the pipeline.
	fenced := "# Agents\n\n```markdown\n## amf\n\n| Stage | Agent |\n|---|---|\n| code | codex |\n```\n\n~~~\n## amf\n| Stage | Agent |\n|---|---|\n| code | codex |\n~~~\n"
	if p, err := parsePipeline(fenced); p != nil || err != nil {
		t.Errorf("fenced example only: got %+v, %v; want no pipeline", p, err)
	}
	if p := parse(fenced + "\n## amf\n\n| Stage | Agent |\n|---|---|\n| code | opencode |\n"); p.Code.Agent != "opencode" {
		t.Errorf("real table after a fenced example: code = %+v", *p.Code)
	}
	// A fence inside the section, before the table, doesn't end the section.
	if p := parse("## amf\n\n```sh\n# not a heading\n```\n\n| Stage | Agent |\n|---|---|\n| code | codex |\n"); p.Code.Agent != "codex" {
		t.Errorf("fence before the table: code = %+v", *p.Code)
	}

	for _, tc := range []struct{ md, want string }{
		// The table must come before the next heading, even a subheading.
		{"## amf\n\n### Stages\n\n| Stage | Agent |\n|---|---|\n| code | codex |\n", "no table"},
		{"## amf\n\n| Stage | Agent |\n|---|---|\n", "no code stage"},
		{"## amf\n\n| Stage | Agent |\n|---|---|\n|  | codex |\n", `unknown stage ""`},
		{"## amf\n\n| Stage | Agent |\n|---|---|\n| code |  |\n", `unknown agent ""`},
		{"## amf\n\n| Stage | Skill | Agent |\n|---|---|---|\n| code | https://user:secret@example.com/SKILL.md | codex |\n", "credentials"},
		{"## amf\n\n| Stage | Skill | Agent |\n|---|---|---|\n| code | https:///SKILL.md | codex |\n", "not an https URL"},
		{"## amf\n\n| Stage | Skill | Agent |\n|---|---|---|\n| code | ftp://example.com/SKILL.md | codex |\n", "not an https URL"},
		{"## amf\n\n| Stage | Skill | Agent |\n|---|---|---|\n| code | example.com/SKILL.md | codex |\n", "not an https URL"},
		{"## amf\n\n| Stage | Skill | Agent |\n|---|---|---|\n| code | https://example.com/SKILL.md#sha256=abc | codex |\n", "#sha256="},
		{"## amf\n\n| Stage | Skill | Agent |\n|---|---|---|\n| code | https://example.com/SKILL.md#SHA256=" + strings.Repeat("ab", 32) + " | codex |\n", "#sha256="},
	} {
		if _, err := parsePipeline(tc.md); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parsePipeline(%q) error = %v, want %q", tc.md, err, tc.want)
		}
	}
}

func TestSkillURL(t *testing.T) {
	sum := strings.Repeat("Ab", 32)
	fetch, pin, err := skillURL("https://example.com/a/SKILL.md?x=1#sha256=" + sum)
	if err != nil || fetch != "https://example.com/a/SKILL.md?x=1" || pin != strings.ToLower(sum) {
		t.Fatalf("skillURL = %q, %q, %v", fetch, pin, err)
	}
	if fetch, pin, err := skillURL("https://example.com/SKILL.md"); err != nil || fetch != "https://example.com/SKILL.md" || pin != "" {
		t.Fatalf("unpinned skillURL = %q, %q, %v", fetch, pin, err)
	}
}

func TestFetchSkillEdgeCases(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Errorf("skill request carried credentials: %v", r.Header)
		}
		switch r.URL.Path {
		case "/SKILL.md":
			io.WriteString(w, "# Skill\n")
		case "/redirect-https":
			http.Redirect(w, r, srv.URL+"/SKILL.md", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, srv.URL+"/loop", http.StatusFound)
		case "/exact":
			w.Write(bytes.Repeat([]byte("x"), maxSkillSize))
		case "/latin1":
			w.Write([]byte("caf\xe9\n"))
		case "/empty":
		case "/500":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/slow":
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()

	if got, err := fetchSkill(ctx, srv.Client(), srv.URL+"/redirect-https"); err != nil || got != "# Skill\n" {
		t.Errorf("https redirect: %q, %v", got, err)
	}
	if got, err := fetchSkill(ctx, srv.Client(), srv.URL+"/exact"); err != nil || len(got) != maxSkillSize {
		t.Errorf("skill of exactly %d bytes: %d bytes, %v", maxSkillSize, len(got), err)
	}
	if got, err := fetchSkill(ctx, srv.Client(), srv.URL+"/empty"); err != nil || got != "" {
		t.Errorf("empty skill: %q, %v", got, err)
	}
	for path, want := range map[string]string{
		"/loop":   "too many redirects",
		"/latin1": "not UTF-8",
		"/500":    "500",
	} {
		if _, err := fetchSkill(ctx, srv.Client(), srv.URL+path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("fetchSkill(%s) error = %v, want %q", path, err, want)
		}
	}

	// Credentials in the URL are refused before anything is sent.
	u, _ := url.Parse(srv.URL + "/SKILL.md")
	u.User = url.UserPassword("me", "secret")
	if _, err := fetchSkill(ctx, srv.Client(), u.String()); err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Errorf("URL with credentials: error = %v", err)
	}
	// Cancelling stops a slow download.
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := fetchSkill(cctx, srv.Client(), srv.URL+"/slow"); err == nil || time.Since(start) > 3*time.Second {
		t.Errorf("cancelled download: error = %v after %s", err, time.Since(start))
	}
}

func TestParseVerdictEdgeCases(t *testing.T) {
	for _, tc := range []struct {
		out      string
		pass, ok bool
	}{
		{"VERDICT:PASS", true, true},
		{"verdict: fail", false, true},
		{"Review.\r\n\r\nVERDICT: PASS\r\n", true, true},
		{"Review.\n\nVERDICT: PASS\n\n\n", true, true},
		{"## VERDICT: PASS", true, true},
		{"> VERDICT: FAIL", false, true},
		{"VERDICT: PASS - ship it", false, false},
		{"VERDICT: MAYBE", false, false},
		{"PASS", false, false},
		{"The verdict: pass is in the middle\nof the review.", false, false},
		{"   \n  ", false, false},
	} {
		pass, ok := parseVerdict(tc.out)
		if pass != tc.pass || ok != tc.ok {
			t.Errorf("parseVerdict(%q) = %v, %v; want %v, %v", tc.out, pass, ok, tc.pass, tc.ok)
		}
	}
	if got := stripVerdict("Fix x.\r\n\r\n**VERDICT: FAIL**\r\n"); got != "Fix x." {
		t.Errorf("stripVerdict with CRLF = %q", got)
	}
	if got := stripVerdict("No verdict here."); got != "No verdict here." {
		t.Errorf("stripVerdict without a verdict = %q", got)
	}
}

func TestEvalPromptTruncatesLargeDiffs(t *testing.T) {
	issue := Issue{Number: 1, Title: "t"}
	small := evalPrompt("vic/demo", "amf/issue-1", "main", issue, &Stage{}, "", "summary", "+small\n")
	if strings.Contains(small, "cut short") || !strings.Contains(small, "+small") {
		t.Errorf("small diff prompt:\n%s", small)
	}
	// An odd prefix puts the cut in the middle of a two-byte character.
	diff := "a" + strings.Repeat("é", maxDiffSize)
	p := evalPrompt("vic/demo", "amf/issue-1", "main", issue, &Stage{}, "", "summary", diff)
	if !strings.Contains(p, "It is cut short here") {
		t.Error("large diff prompt has no note that it was cut")
	}
	if !utf8.ValidString(p) {
		t.Error("the cut diff splits a UTF-8 character")
	}
	if len(p) > maxDiffSize+4<<10 {
		t.Errorf("prompt is %d bytes", len(p))
	}
}

func TestCleanSummaryEdgeCases(t *testing.T) {
	for in, want := range map[string]string{
		"A\nCloses #10\nFixes #1.":              "A\nCloses #10",
		"  \n\n":                                "",
		"Fixes #1":                              "",
		"A\n\n\n\n\nB":                          "A\n\nB",
		"A\nresolves #1\nCo-authored-by: x <y>": "A",
	} {
		if got := cleanSummary(in, 1); got != want {
			t.Errorf("cleanSummary(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- configuration and the command line ---

func TestParseDotEnvEdgeCases(t *testing.T) {
	in := "\ufeffGITHUB_TOKEN=tok\r\nGITHUB_REPO=a/b\r\nDUP=1\nDUP=2\nQ=\"has # hash\"\nE=a=b\nexport  SPACED = v \n"
	got, err := parseDotEnv(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"GITHUB_TOKEN": "tok", "GITHUB_REPO": "a/b", "DUP": "2", "Q": "has # hash", "E": "a=b", "SPACED": "v"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, err := parseDotEnv(strings.NewReader("OK=1\n\nnot a pair\n")); err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("bad line: error = %v", err)
	}
}

func TestLoadConfigEdgeCases(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, ".env")
		os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	t.Setenv("AMF_API_URL", "")

	for _, tc := range []struct{ file, want string }{
		{"GITHUB_TOKEN=github_pat_...\nGITHUB_REPO=vic/demo\n", "GITHUB_TOKEN is not set"},
		{"GITHUB_TOKEN=tok\nGITHUB_REPO=owner/name\n", "GITHUB_REPO is not set"},
		{"GITHUB_TOKEN=tok\nGITHUB_REPO=a/b/c\n", "must look like owner/name"},
		{"GITHUB_TOKEN=tok\nGITHUB_REPO=/b\n", "must look like owner/name"},
		{"", "GITHUB_TOKEN is not set"},
	} {
		if _, err := loadConfig(write(tc.file)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("loadConfig(%q) error = %v, want %q", tc.file, err, tc.want)
		}
	}
	if _, err := loadConfig(filepath.Join(dir, "missing")); err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN is not set") {
		t.Errorf("no .env: error = %v", err)
	}

	p := write("GITHUB_TOKEN=tok\nGITHUB_REPO=file/repo\nAMF_API_URL=http://ghe.example/api/v3/\nAMF_CODEX=/opt/codex\nAMF_OPENCODE=/opt/oc\n")
	t.Setenv("GITHUB_REPO", "env/repo")
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repo != "env/repo" || cfg.APIURL != "http://ghe.example/api/v3" || cfg.Codex != "/opt/codex" || cfg.Opencode != "/opt/oc" || cfg.CloneURL != "https://github.com/env/repo.git" {
		t.Fatalf("config = %+v", cfg)
	}
	// A placeholder in the environment doesn't hide the file's value.
	t.Setenv("GITHUB_REPO", "owner/name")
	if cfg, err := loadConfig(p); err != nil || cfg.Repo != "file/repo" {
		t.Fatalf("placeholder in env: %+v, %v", cfg, err)
	}
}

func TestCommandLineErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		args []string
		want string // "usage" for errUsage, "" for success, else part of the error
	}{
		{nil, "usage"},
		{[]string{"bogus"}, "usage"},
		{[]string{"help"}, ""},
		{[]string{"--help"}, ""},
		{[]string{"version"}, ""},
		{[]string{"run"}, "usage"},
		{[]string{"run", "1", "2"}, "usage"},
		{[]string{"run", "--bogus", "1"}, "usage"},
		{[]string{"run", "abc"}, "invalid issue number"},
		{[]string{"run", "0"}, "invalid issue number"},
		{[]string{"run", "#"}, "invalid issue number"},
		{[]string{"run", "1", "--timeout", "0"}, "--timeout must be positive"},
		{[]string{"run", "1", "--timeout", "-5m"}, "--timeout must be positive"},
		{[]string{"run", "1", "--eval-rounds", "-1"}, "can't be negative"},
		{[]string{"run", "#3"}, "GITHUB_TOKEN is not set"},
		{[]string{"watch", "extra"}, "usage"},
		{[]string{"watch", "--interval", "0"}, "--interval must be positive"},
		{[]string{"watch", "--timeout", "0"}, "--timeout must be positive"},
		{[]string{"watch", "--eval-rounds", "-2"}, "can't be negative"},
		{[]string{"list", "extra"}, "usage"},
		{[]string{"list", "--state", "bogus"}, "--state must be"},
		{[]string{"issue"}, "usage"},
		{[]string{"issue", "   "}, "usage"},
		{[]string{"issue", "A", "B"}, "usage"},
		{[]string{"issue", "T", "-b", "x", "-f", "y.md"}, "use -b or -f"},
		{[]string{"issue", "T", "-f", "/nonexistent/body.md"}, "no such file"},
		{[]string{"pipeline", "extra"}, "usage"},
		{[]string{"pipeline", "--bogus"}, "usage"},
		{[]string{"init", "extra"}, "usage"},
		{[]string{"update", "extra"}, "usage"},
	} {
		err := run(context.Background(), tc.args, strings.NewReader(""), io.Discard, io.Discard)
		switch {
		case tc.want == "usage":
			if !errors.Is(err, errUsage) {
				t.Errorf("amf %q: error = %v, want a usage error", tc.args, err)
			}
		case tc.want == "":
			if err != nil {
				t.Errorf("amf %q: %v", tc.args, err)
			}
		case err == nil || errors.Is(err, errUsage) || !strings.Contains(err.Error(), tc.want):
			t.Errorf("amf %q: error = %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestPipelineCommandEdgeCases(t *testing.T) {
	ctx := context.Background()
	t.Chdir(t.TempDir())
	if err := run(ctx, []string{"pipeline"}, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "inside a git checkout") {
		t.Fatalf("outside a checkout: %v", err)
	}

	root, _ := setupRepo(t)
	t.Chdir(filepath.Join(root))
	agents := filepath.Join(root, "AGENTS.md")
	var out bytes.Buffer

	os.WriteFile(agents, []byte("# Agents\n\nNo pipeline.\n"), 0o644)
	if err := run(ctx, []string{"pipeline"}, nil, &out, io.Discard); err != nil || !strings.Contains(out.String(), "No ## amf table") {
		t.Fatalf("no table: %v, %q", err, out.String())
	}

	os.WriteFile(agents, []byte("## amf\n\n| Stage | Agent |\n|---|---|\n| code | gemini |\n"), 0o644)
	if err := run(ctx, []string{"pipeline"}, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), `unknown agent "gemini"`) {
		t.Fatalf("bad table: %v", err)
	}

	// Run from a subdirectory: the checkout's top-level AGENTS.md is used.
	// Skills that can't be downloaded are each a problem; a shared agent is
	// checked once.
	os.WriteFile(agents, []byte("## amf\n\n| Stage | Skill | Agent |\n|---|---|---|\n"+
		"| code | https://127.0.0.1:1/code/SKILL.md | claudecode |\n"+
		"| eval | https://127.0.0.1:1/eval/SKILL.md | claudecode |\n"), 0o644)
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	t.Chdir(filepath.Join(root, "sub"))
	t.Setenv("AMF_CLAUDE", fakeAgent(t, "claude", "exit 0"))
	out.Reset()
	if err := run(ctx, []string{"pipeline"}, nil, &out, io.Discard); err != nil {
		t.Fatalf("without --fetch skills aren't downloaded: %v\n%s", err, out.String())
	}
	out.Reset()
	err := run(ctx, []string{"pipeline", "--fetch"}, nil, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "2 problems") {
		t.Fatalf("--fetch with unreachable skills: %v\n%s", err, out.String())
	}
	if s := out.String(); strings.Count(s, "agent claudecode") != 1 || !strings.Contains(s, "skill code        failed:") || !strings.Contains(s, "skill eval        failed:") {
		t.Errorf("output:\n%s", s)
	}
}

// --- GitHub ---

func TestGitHubEdgeCases(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "x", State: "open", Labels: []Label{{Name: "needs review"}}}
	ctx := context.Background()
	c := newGitHub(Config{Token: "test-token", Repo: "vic/demo", APIURL: srv.URL})

	// Labels with spaces are escaped; removing a missing label is fine.
	if err := c.RemoveLabel(ctx, 1, "needs review"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveLabel(ctx, 1, "not there"); err != nil {
		t.Fatalf("removing a missing label: %v", err)
	}
	if err := c.AddLabels(ctx, 1, "a b", "amf:ready"); err != nil {
		t.Fatal(err)
	}
	if got := gh.labels(1); !reflect.DeepEqual(got, []string{"a b", "amf:ready"}) {
		t.Fatalf("labels = %v", got)
	}
	if _, err := c.GetIssue(ctx, 99); !isStatus(err, 404) {
		t.Fatalf("missing issue: %v", err)
	}

	bad := newGitHub(Config{Token: "wrong", Repo: "vic/demo", APIURL: srv.URL})
	if err := checkRepo(ctx, bad, "vic/demo"); err == nil || !strings.Contains(err.Error(), "rejected GITHUB_TOKEN") {
		t.Errorf("bad token: %v", err)
	}
	missing := newGitHub(Config{Token: "test-token", Repo: "vic/missing", APIURL: srv.URL})
	if err := checkRepo(ctx, missing, "vic/missing"); err == nil || !strings.Contains(err.Error(), "vic/missing was not found") {
		t.Errorf("missing repo: %v", err)
	}
	down := newGitHub(Config{Token: "test-token", Repo: "vic/demo", APIURL: "http://127.0.0.1:1"})
	if err := checkRepo(ctx, down, "vic/demo"); err == nil {
		t.Error("unreachable GitHub: no error")
	}

	// Pagination never follows a link to another host.
	g := &GitHub{base: "https://api.github.com"}
	h := http.Header{"Link": {`<https://evil.example/repos/x?page=2>; rel="next"`}}
	if _, err := g.nextPage(h); err == nil {
		t.Error("followed a pagination link to another host")
	}
	if p, err := g.nextPage(http.Header{}); p != "" || err != nil {
		t.Errorf("no Link header: %q, %v", p, err)
	}
}

// --- processing an issue without a pipeline ---

func TestRunRejectsNonIssues(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "A PR", State: "open", PullRequest: &struct{}{}, Labels: []Label{{Name: "amf:ready"}}}
	gh.issues[2] = &Issue{Number: 2, Title: "Closed", State: "closed", Labels: []Label{{Name: "amf:ready"}}}
	r, _ := newTestRunner(t, srv, root, fakeAgent(t, "claude", `echo "must not run" >&2; exit 9`))
	for n, want := range map[int]string{1: "is a pull request", 2: "#2 is closed", 3: "404"} {
		if err := r.Run(context.Background(), n); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Run(%d) error = %v, want %q", n, err, want)
		}
	}
	for n := 1; n <= 2; n++ {
		if got := gh.labels(n); !reflect.DeepEqual(got, []string{"amf:ready"}) {
			t.Errorf("#%d labels = %v", n, got)
		}
		if len(gh.comments[n]) != 0 {
			t.Errorf("#%d comments = %q", n, gh.comments[n])
		}
	}
}

func TestRunAgentFailures(t *testing.T) {
	for _, tc := range []struct {
		name, script, want string
	}{
		{"exit status", `cat >/dev/null; echo boom >&2; exit 2`, "claude: exit status 2 (log: "},
		{"error result", `cat >/dev/null; echo '{"type":"result","subtype":"error_max_turns","is_error":true,"result":"ran out"}'`, "claude reported an error (error_max_turns): ran out"},
		{"no result", `cat >/dev/null; echo hi > x.txt; echo '{"type":"system","subtype":"init"}'`, "no result message"},
		{"missing binary", "", "claude: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := setupRepo(t)
			gh, srv := newFakeGitHub(t)
			gh.issues[1] = readyIssue(1, "Anything")
			claude := filepath.Join(t.TempDir(), "no-claude")
			if tc.script != "" {
				claude = fakeAgent(t, "claude", tc.script)
			}
			r, _ := newTestRunner(t, srv, root, claude)
			err := r.Run(context.Background(), 1)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if got := gh.labels(1); !reflect.DeepEqual(got, []string{"amf:failed"}) {
				t.Errorf("labels = %v", got)
			}
			if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], tc.want) {
				t.Errorf("comments = %q", c)
			}
			if len(gh.prs) != 0 {
				t.Errorf("PRs = %v", gh.prs)
			}
			if _, err := os.Stat(filepath.Join(root, ".amf", "worktrees", "issue-1")); err != nil {
				t.Errorf("worktree should be kept: %v", err)
			}
		})
	}
}

func TestRunCancelledMarksFailed(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Slow")
	started := filepath.Join(t.TempDir(), "started")
	r, _ := newTestRunner(t, srv, root, fakeAgent(t, "claude", "touch "+started+"\nexec sleep 30\n"))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			if _, err := os.Stat(started); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	begin := time.Now()
	if err := r.Run(ctx, 1); err == nil {
		t.Fatal("expected an error")
	}
	if time.Since(begin) > 15*time.Second {
		t.Errorf("cancelling took %s", time.Since(begin))
	}
	if got := gh.labels(1); !reflect.DeepEqual(got, []string{"amf:failed"}) {
		t.Errorf("labels = %v", got)
	}
	if len(gh.comments[1]) != 1 {
		t.Errorf("comments = %q", gh.comments[1])
	}
}

func TestRunOddTitleAndEmptyBody(t *testing.T) {
	root, origin := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	title := "Fix \"quotes\" & $(touch pwned) `ticks` 'single' — ünïcode 🚀"
	gh.issues[1] = &Issue{Number: 1, Title: title, State: "open"}
	saved := filepath.Join(t.TempDir(), "prompt.txt")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", shResult+"cat > "+saved+"\necho x > x.txt\nresult ''\n"))
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	prompt := mustRead(t, saved)
	if !strings.Contains(prompt, "# #1: "+title) || !strings.Contains(prompt, "(no description)") {
		t.Errorf("prompt:\n%s", prompt)
	}
	if got := mustGit(t, root, "--git-dir", origin, "log", "-1", "--format=%s", "amf/issue-1"); got != "Fix #1: "+title {
		t.Errorf("commit subject = %q", got)
	}
	if pr := gh.prs[0]; pr["title"] != "Fix #1: "+title || !strings.HasPrefix(pr["body"], "Resolves #1.\n\nCloses #1") {
		t.Errorf("PR = %q", pr)
	}
	if _, err := os.Stat(filepath.Join(root, "pwned")); err == nil {
		t.Error("the title was run by a shell")
	}
}

func TestRunAgentCommitsItself(t *testing.T) {
	root, origin := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Self-committing agent")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", shResult+`cat >/dev/null
echo x > x.txt && git add x.txt && git commit -qm "agent's own commit"
echo y > y.txt
result 'Done.'
`))
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	if got := mustGit(t, root, "--git-dir", origin, "diff", "--name-only", "main", "amf/issue-1"); got != "x.txt\ny.txt" {
		t.Errorf("branch changes = %q", got)
	}
}

func TestRunNetZeroChangeFails(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Change and undo")
	r, _ := newTestRunner(t, srv, root, fakeAgent(t, "claude", shResult+`cat >/dev/null
echo x > x.txt && git add x.txt && git commit -qm add
git rm -q x.txt && git commit -qm remove
result 'Nothing really.'
`))
	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "without making any changes") {
		t.Fatalf("error = %v", err)
	}
	if len(gh.prs) != 0 {
		t.Errorf("PRs = %v", gh.prs)
	}
}

func TestRerunUpdatesOpenPR(t *testing.T) {
	root, origin := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add a file")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", shResult+"cat >/dev/null\necho one > f.txt\nresult 'First try.'\n"))
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}

	// The PR is still open when the issue is retried.
	gh.issues[1].Labels = append(gh.issues[1].Labels, Label{Name: "amf:ready"})
	r.Claude = fakeAgent(t, "claude", shResult+"cat >/dev/null\necho two > f.txt\nresult 'Second try.'\n")
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("re-run: %v\nlog:\n%s", err, log)
	}
	if len(gh.prs) != 1 {
		t.Fatalf("expected the one PR to be reused, got %v", gh.prs)
	}
	if body := gh.prs[0]["body"]; !strings.HasPrefix(body, "Second try.") {
		t.Errorf("PR body wasn't updated: %q", body)
	}
	if got := mustGit(t, root, "--git-dir", origin, "show", "amf/issue-1:f.txt"); got != "two" {
		t.Errorf("branch f.txt = %q", got)
	}
	if got := mustGit(t, root, "--git-dir", origin, "rev-list", "--count", "main..amf/issue-1"); got != "1" {
		t.Errorf("branch has %s commits, want 1", got)
	}
	if c := gh.comments[1]; len(c) != 2 || !strings.Contains(c[1], "amf updated https://github.com/vic/demo/pull/101") {
		t.Errorf("comments = %q", c)
	}
	if got := gh.labels(1); !reflect.DeepEqual(got, []string{"amf:done"}) {
		t.Errorf("labels = %v", got)
	}

	// Once that PR is closed, a retry opens a new one.
	gh.prs[0]["state"] = "closed"
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("re-run after close: %v", err)
	}
	if len(gh.prs) != 2 {
		t.Errorf("expected a second PR, got %v", gh.prs)
	}
}

func TestRunReplacesStrayWorktreeDir(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Stray dir")
	// A leftover folder that git doesn't know as a worktree.
	os.MkdirAll(filepath.Join(root, ".amf", "worktrees", "issue-1", "junk"), 0o755)
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", shResult+"cat >/dev/null\necho x > x.txt\nresult 'ok'\n"))
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
}

func TestWatchContinuesAfterFailures(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Broken one")
	gh.issues[2] = readyIssue(2, "Good one")

	// GitHub fails the first two listings.
	target, _ := url.Parse(srv.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var lists atomic.Int32
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/issues") && lists.Add(1) <= 2 {
			writeJSON(w, 502, map[string]string{"message": "Bad Gateway"})
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(flaky.Close)

	r, _ := newTestRunner(t, flaky, root, fakeAgent(t, "claude", shResult+`prompt=$(cat)
case "$prompt" in *"Broken one"*) exit 3;; esac
echo x > x.txt
result 'ok'
`))
	var mu syncBuffer
	r.Log = &mu
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- r.Watch(ctx, "amf:ready", 20*time.Millisecond) }()
	deadline := time.Now().Add(15 * time.Second)
	for !slices.Contains(gh.labels(1), "amf:failed") || !slices.Contains(gh.labels(2), "amf:done") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("issues not processed; log:\n%s", mu.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	out := mu.String()
	for _, want := range []string{"listing issues: github GET", "502", "#1 failed:", "stopped"} {
		if !strings.Contains(out, want) {
			t.Errorf("watch log missing %q:\n%s", want, out)
		}
	}
	if len(gh.prs) != 1 || gh.prs[0]["head"] != "amf/issue-2" {
		t.Errorf("PRs = %v", gh.prs)
	}
}

// --- pipelines ---

// pipelineRun sets up a repo with agentsMD on main, issue #1 and a runner
// whose claude is the given fake.
func pipelineRun(t *testing.T, agentsMD, claude string) (r *Runner, gh *fakeGitHub, root, origin string) {
	t.Helper()
	root, origin = setupPipelineRepo(t, agentsMD)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	r, _ = newTestRunner(t, srv, root, claude)
	r.EvalRounds = 2
	return r, gh, root, origin
}

func TestPipelineBlockedVariants(t *testing.T) {
	for first, blocked := range map[string]bool{
		"BLOCKED: which page?":       true,
		"**BLOCKED:** which page?":   true,
		"Blocked: which page?":       true,
		"`BLOCKED:` which page?":     true,
		"## BLOCKED: which page?":    true,
		"> BLOCKED: which page?":     true,
		"Not BLOCKED: we can do it":  false,
		"1. Add it\\n\\nBLOCKED: no": false,
	} {
		t.Run(first, func(t *testing.T) {
			r, gh, _, _ := pipelineRun(t, claudeTable("plan", "code"), stageClaude(t,
				"result '"+first+"'",
				"echo x > feature.txt; result 'Added.'",
				"exit 9"))
			err := r.Run(context.Background(), 1)
			if !blocked {
				if err != nil {
					t.Fatalf("not blocked, but: %v", err)
				}
				return
			}
			var se *stageError
			if !errors.As(err, &se) || !strings.Contains(err.Error(), "which page?") {
				t.Fatalf("error = %v, want a blocked plan", err)
			}
			if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "needs answers") {
				t.Errorf("comments = %q", c)
			}
			if len(gh.prs) != 0 {
				t.Errorf("PRs = %v", gh.prs)
			}
		})
	}
}

func TestPipelineEvalWithoutVerdict(t *testing.T) {
	r, gh, _, _ := pipelineRun(t, claudeTable("code", "eval"), stageClaude(t, "exit 9", `
case "$prompt" in
*"<review>"*)
	case "$prompt" in *"eval gave no verdict."*"Looks odd."*) ;; *) echo "review missing" >&2; exit 7;; esac
	echo v2 > feature.txt ;;
*) echo v1 > feature.txt ;;
esac
result 'Wrote feature.txt.'`, `
if [ "$(cat feature.txt)" = v1 ]; then result 'Looks odd.'; else result 'Fine.\n\nVERDICT: PASS'; fi`))
	r.EvalRounds = 1
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if body := gh.prs[0]["body"]; !strings.Contains(body, "after 2 code rounds") {
		t.Errorf("PR body:\n%s", body)
	}
}

func TestPipelineEvalRoundsZero(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	r, gh, _, origin := pipelineRun(t, claudeTable("code", "eval"), stageClaude(t, "exit 9",
		"echo x >> "+count+"; echo bad > feature.txt; result 'Tried.'",
		"result 'Wrong.\\n\\nVERDICT: FAIL'"))
	r.EvalRounds = 0
	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "after 1 code round;") {
		t.Fatalf("error = %v", err)
	}
	if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "after 1 code round, so no pull request") || !strings.Contains(c[0], "Wrong.") {
		t.Errorf("comments = %q", c)
	}
	if got := strings.Count(mustRead(t, count), "x"); got != 1 {
		t.Errorf("code ran %d times", got)
	}
	if got := mustGit(t, origin, "show", "amf/issue-1:feature.txt"); got != "bad" {
		t.Errorf("pushed feature.txt = %q", got)
	}
}

func TestPipelineReadOnlyStagesCantChangeBranch(t *testing.T) {
	const vandal = `rm README.md; echo junk > junk.txt; mkdir -p d/e; echo x > d/e/f; echo changed >> AGENTS.md
git add -A && git commit -qm sneaky`
	r, gh, root, origin := pipelineRun(t, claudeTable("plan", "code", "eval"), stageClaude(t,
		vandal+"\nresult 'The plan.'",
		`[ -f README.md ] && [ ! -e junk.txt ] && [ ! -e d ] || { echo "plan changes survived" >&2; exit 8; }
echo hello > feature.txt; result 'Added.'`,
		vandal+"\nresult 'Good.\\n\\nVERDICT: PASS'"))
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if got := mustGit(t, origin, "diff", "--name-only", "main", "amf/issue-1"); got != "feature.txt" {
		t.Errorf("branch changes = %q", got)
	}
	if got := mustGit(t, origin, "log", "--format=%s", "main..amf/issue-1"); got != "Fix #1: Add feature file" {
		t.Errorf("branch commits = %q", got)
	}
	if len(gh.prs) != 1 {
		t.Errorf("PRs = %v", gh.prs)
	}
	if _, err := os.Stat(filepath.Join(root, ".amf", "worktrees", "issue-1")); !os.IsNotExist(err) {
		t.Errorf("worktree should be removed after success: %v", err)
	}
}

func TestPipelineCodeCommitsItselfIsSquashed(t *testing.T) {
	r, _, _, origin := pipelineRun(t, claudeTable("code", "eval"), stageClaude(t, "exit 9",
		"echo a > a.txt; git add a.txt; git commit -qm wip; echo b > b.txt; result 'Two files.'",
		"result 'VERDICT: PASS'"))
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if got := mustGit(t, origin, "log", "--format=%s%n%b", "main..amf/issue-1"); strings.TrimSpace(got) != "Fix #1: Add feature file\nCloses #1" {
		t.Errorf("branch commits = %q", got)
	}
}

func TestPipelineCodeMakesNoChange(t *testing.T) {
	for name, code := range map[string]string{
		"nothing": "result 'Nothing to do.'",
		// Round 2 undoes round 1, leaving nothing to merge.
		"undone": `case "$prompt" in *"<review>"*) rm feature.txt;; *) echo x > feature.txt;; esac; result 'Changed.'`,
	} {
		t.Run(name, func(t *testing.T) {
			r, gh, _, _ := pipelineRun(t, claudeTable("code", "eval"), stageClaude(t, "exit 9", code,
				`if [ -f feature.txt ]; then result 'Remove it.\n\nVERDICT: FAIL'; else result 'VERDICT: PASS'; fi`))
			err := r.Run(context.Background(), 1)
			if err == nil || !strings.Contains(err.Error(), "the code stage (claudecode) finished without making any changes") {
				t.Fatalf("error = %v", err)
			}
			if len(gh.prs) != 0 {
				t.Errorf("PRs = %v", gh.prs)
			}
		})
	}
}

func TestPipelineCodexWritesNoMessage(t *testing.T) {
	r, gh, _, _ := pipelineRun(t, "## amf\n\n| Stage | Agent |\n|---|---|\n| code | codex |\n", "claude")
	r.Codex = fakeAgent(t, "codex", "cat >/dev/null; echo x > feature.txt\n")
	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "codex wrote no final message") {
		t.Fatalf("error = %v", err)
	}
	if got := gh.labels(1); !reflect.DeepEqual(got, []string{"amf:failed"}) {
		t.Errorf("labels = %v", got)
	}
}

func TestPipelineOpencodeReadsPromptFromStdin(t *testing.T) {
	r, gh, _, _ := pipelineRun(t, "## amf\n\n| Stage | Agent | Model |\n|---|---|---|\n| code | opencode | default |\n", "claude")
	r.Opencode = fakeAgent(t, "opencode", `
[ "$#" = 1 ] && [ "$1" = run ] || { echo "bad args: $*" >&2; exit 5; }
prompt=$(cat)
case "$prompt" in *"Add feature file"*"final reply"*) ;; *) echo "bad prompt: $prompt" >&2; exit 4;; esac
echo x > feature.txt
echo "Summary from stdout."
`)
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if body := gh.prs[0]["body"]; !strings.HasPrefix(body, "Summary from stdout.\n\nCloses #1") {
		t.Errorf("PR body = %q", body)
	}
}

func TestPipelineStageTimeout(t *testing.T) {
	r, gh, _, _ := pipelineRun(t, claudeTable("code", "eval"), stageClaude(t, "exit 9",
		"echo x > feature.txt; result 'ok'", "exec sleep 30"))
	r.Timeout = time.Second
	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "claude timed out after 1s") || !strings.Contains(err.Error(), "issue-1-eval-1.log") {
		t.Fatalf("error = %v", err)
	}
	if got := gh.labels(1); !reflect.DeepEqual(got, []string{"amf:failed"}) {
		t.Errorf("labels = %v", got)
	}
}

func TestPipelineSkillFailuresFailTheIssue(t *testing.T) {
	skills := skillServer(t, map[string]string{"/code/SKILL.md": "CODE-SKILL"})
	for name, tc := range map[string]struct{ skill, want string }{
		"missing": {"/nope/SKILL.md", "404"},
		"pinned":  {"/code/SKILL.md#sha256=" + strings.Repeat("0", 64), "sha256 pin"},
	} {
		t.Run(name, func(t *testing.T) {
			r, gh, _, _ := pipelineRun(t, "## amf\n\n| Stage | Skill | Agent |\n|---|---|---|\n| code | "+skills.URL+tc.skill+" | claudecode |\n",
				fakeAgent(t, "claude", `echo "must not run" >&2; exit 9`))
			r.HTTP = skills.Client()
			err := r.Run(context.Background(), 1)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], tc.want) {
				t.Errorf("comments = %q", c)
			}
		})
	}
	// A matching pin works.
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte("CODE-SKILL")))
	r, _, _, _ := pipelineRun(t, "## amf\n\n| Stage | Skill | Agent |\n|---|---|---|\n| code | "+skills.URL+"/code/SKILL.md#sha256="+sum+" | claudecode |\n",
		fakeAgent(t, "claude", shResult+`prompt=$(cat)
case "$prompt" in *"<skill>"*"CODE-SKILL"*"</skill>"*) ;; *) exit 4;; esac
echo x > feature.txt; result 'ok'`))
	r.HTTP = skills.Client()
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("pinned skill: %v", err)
	}
}

func TestPipelineIsReadFromOrigin(t *testing.T) {
	ok := shResult + "cat >/dev/null; echo x > feature.txt; result 'ok'\n"

	// An uncommitted table in the local checkout is ignored.
	root, _ := setupRepo(t)
	os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("## amf\n\n| Stage | Agent |\n|---|---|\n| code | codex |\n"), 0o644)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Local table")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", ok))
	r.Codex = filepath.Join(t.TempDir(), "no-codex")
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("local table was used: %v\nlog:\n%s", err, log)
	}

	// The table on origin is used even after the local checkout drops it.
	r2, _, root2, _ := pipelineRun(t, "## amf\n\n| Stage | Agent | Model |\n|---|---|---|\n| code | claudecode | m1 |\n",
		fakeAgent(t, "claude", shResult+`case "$*" in *"--model m1"*) ;; *) exit 5;; esac
cat >/dev/null; echo x > feature.txt; result 'ok'`))
	mustGit(t, root2, "rm", "-q", "AGENTS.md")
	mustGit(t, root2, "commit", "-qm", "drop the table locally")
	if err := r2.Run(context.Background(), 1); err != nil {
		t.Fatalf("origin's table wasn't used: %v", err)
	}
}

func TestPipelineInvalidTableLeavesIssueAlone(t *testing.T) {
	r, gh, _, _ := pipelineRun(t, "## amf\n\n| Stage | Agent |\n|---|---|\n| code | gemini |\n", fakeAgent(t, "claude", "exit 9"))
	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), `unknown agent "gemini"`) {
		t.Fatalf("error = %v", err)
	}
	if got := gh.labels(1); !reflect.DeepEqual(got, []string{"amf:ready"}) {
		t.Errorf("labels = %v", got)
	}
	if len(gh.comments[1]) != 0 {
		t.Errorf("comments = %q", gh.comments[1])
	}
}

func TestPipelineRerunAfterEvalFailureStartsFresh(t *testing.T) {
	r, gh, _, origin := pipelineRun(t, claudeTable("code", "eval"), stageClaude(t, "exit 9",
		"echo bad > feature.txt; result 'Tried.'", "result 'No.\\n\\nVERDICT: FAIL'"))
	r.EvalRounds = 0
	if err := r.Run(context.Background(), 1); err == nil {
		t.Fatal("expected the eval failure")
	}

	gh.issues[1].Labels = append(gh.issues[1].Labels, Label{Name: "amf:ready"})
	r.Claude = stageClaude(t, "exit 9",
		`[ ! -e feature.txt ] || { echo "the failed attempt leaked in" >&2; exit 9; }
echo good > feature.txt; result 'Fresh.'`, "result 'VERDICT: PASS'")
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if got := mustGit(t, origin, "show", "amf/issue-1:feature.txt"); got != "good" {
		t.Errorf("feature.txt = %q", got)
	}
	if got := mustGit(t, origin, "rev-list", "--count", "main..amf/issue-1"); got != "1" {
		t.Errorf("branch has %s commits", got)
	}
	if got := gh.labels(1); !reflect.DeepEqual(got, []string{"amf:done"}) {
		t.Errorf("labels = %v", got)
	}
}

func TestPipelinePRBodyHasNoAttribution(t *testing.T) {
	r, gh, _, _ := pipelineRun(t, claudeTable("plan", "code", "eval"), stageClaude(t,
		"result '1. Do it\\n\\nCo-Authored-By: Claude <noreply@anthropic.com>'",
		"echo x > feature.txt; result 'Did it.\\n\\nCloses #1'",
		"result 'Fine.\\n\\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\\n\\nVERDICT: PASS'"))
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	body := gh.prs[0]["body"]
	for _, bad := range []string{"Co-Authored-By", "Generated with", "VERDICT"} {
		if strings.Contains(body, bad) {
			t.Errorf("PR body contains %q:\n%s", bad, body)
		}
	}
	if strings.Count(body, "Closes #1") != 1 || !strings.Contains(body, "1. Do it") || !strings.Contains(body, "Fine.") {
		t.Errorf("PR body:\n%s", body)
	}
}

// syncBuffer is a bytes.Buffer safe to write from Watch while a test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// --- v0.3.1 ---

// failingGitHub proxies to srv, failing with 502 the requests fail picks.
func failingGitHub(t *testing.T, srv *httptest.Server, fail func(r *http.Request) bool) *httptest.Server {
	target, _ := url.Parse(srv.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail(r) {
			writeJSON(w, 502, map[string]string{"message": "Bad Gateway"})
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

func TestRunSucceedsOnceThePRExists(t *testing.T) {
	ok := shResult + "cat >/dev/null\necho x > x.txt\nresult 'Done.'\n"

	t.Run("comment fails", func(t *testing.T) {
		root, _ := setupRepo(t)
		gh, srv := newFakeGitHub(t)
		gh.issues[1] = readyIssue(1, "Comment fails")
		flaky := failingGitHub(t, srv, func(r *http.Request) bool {
			return r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/comments")
		})
		r, log := newTestRunner(t, flaky, root, fakeAgent(t, "claude", ok))
		if err := r.Run(context.Background(), 1); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if got := gh.labels(1); !reflect.DeepEqual(got, []string{"amf:done"}) {
			t.Errorf("labels = %v", got)
		}
		if !strings.Contains(log.String(), "#1: could not comment") {
			t.Errorf("log:\n%s", log)
		}
	})

	t.Run("label fails", func(t *testing.T) {
		root, _ := setupRepo(t)
		gh, srv := newFakeGitHub(t)
		gh.issues[1] = readyIssue(1, "Label fails")
		var prOpened atomic.Bool
		flaky := failingGitHub(t, srv, func(r *http.Request) bool {
			if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls") {
				prOpened.Store(true)
			}
			return prOpened.Load() && r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/labels")
		})
		r, log := newTestRunner(t, flaky, root, fakeAgent(t, "claude", ok))
		if err := r.Run(context.Background(), 1); err != nil {
			t.Fatalf("Run: %v", err)
		}
		if slices.Contains(gh.labels(1), "amf:failed") {
			t.Errorf("labels = %v", gh.labels(1))
		}
		if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "amf opened") {
			t.Errorf("comments = %q", c)
		}
		if !strings.Contains(log.String(), "#1: could not set amf:done") {
			t.Errorf("log:\n%s", log)
		}
	})
}

func TestRunNeedsAGitIdentity(t *testing.T) {
	root, _ := setupRepo(t)
	// A machine where git has no name or email.
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(cfg, []byte("[user]\n\tuseConfigOnly = true\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "No identity")
	r, _ := newTestRunner(t, srv, root, fakeAgent(t, "claude", `echo "must not run" >&2; exit 9`))

	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "git config --global user.email") {
		t.Fatalf("error = %v", err)
	}
	if got := gh.labels(1); !reflect.DeepEqual(got, []string{"amf:ready"}) {
		t.Errorf("labels = %v", got)
	}
	if len(gh.comments[1]) != 0 {
		t.Errorf("comments = %q", gh.comments[1])
	}
}

func TestInitInSubdirectoryExcludesEnv(t *testing.T) {
	root, _ := setupRepo(t)
	sub := filepath.Join(root, "sub dir", "a[1]")
	os.MkdirAll(sub, 0o755)
	t.Chdir(sub)
	for range 2 { // the second run makes a backup, .env.0001
		if err := run(context.Background(), []string{"init"}, nil, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(".env", []byte("GITHUB_TOKEN=github_pat_...\nEXTRA=secret\n"), 0o600)
	}
	if _, err := os.Stat(".env.0001"); err != nil {
		t.Fatal(err)
	}
	if st := mustGit(t, root, "status", "--porcelain", "--untracked-files=all"); st != "" {
		t.Fatalf(".env in a subdirectory should be ignored, status = %q", st)
	}
	// Only this folder's .env is excluded, not every .env in the repo.
	os.WriteFile(filepath.Join(root, "sub dir", ".env"), []byte("x\n"), 0o644)
	if st := mustGit(t, root, "status", "--porcelain", "--untracked-files=all"); st != "?? \"sub dir/.env\"" {
		t.Fatalf("status = %q", st)
	}
}

func TestParsePipelineAfterALongLine(t *testing.T) {
	md := "# Agents\n\n" + strings.Repeat("x", 100<<10) + "\n\n## amf\n\n| Stage | Agent |\n|---|---|\n| code | codex |\n"
	p, err := parsePipeline(md)
	if err != nil || p == nil || p.Code.Agent != "codex" {
		t.Fatalf("parsePipeline = %+v, %v", p, err)
	}
}

// --- AMF_TRUSTED ---

func TestParseTrusted(t *testing.T) {
	if got := parseTrusted(" @Vic, teammate  other,,"); !reflect.DeepEqual(got, []string{"vic", "teammate", "other"}) {
		t.Errorf("parseTrusted = %q", got)
	}
	if got := parseTrusted(""); got != nil {
		t.Errorf("parseTrusted(\"\") = %q", got)
	}
}

func TestLoadConfigTrusted(t *testing.T) {
	env := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(env, []byte("GITHUB_TOKEN=tok\nGITHUB_REPO=vic/demo\nAMF_TRUSTED=vic,Teammate\n"), 0o600)
	if cfg, err := loadConfig(env); err != nil || !reflect.DeepEqual(cfg.Trusted, []string{"vic", "teammate"}) {
		t.Fatalf("cfg.Trusted = %q, %v", cfg.Trusted, err)
	}
	os.WriteFile(env, []byte("GITHUB_TOKEN=tok\nGITHUB_REPO=vic/demo\nAMF_TRUSTED=login1,login2...\n"), 0o600)
	if cfg, _ := loadConfig(env); cfg.Trusted != nil {
		t.Fatalf("a placeholder should leave AMF_TRUSTED unset, got %q", cfg.Trusted)
	}
}

func TestRunOnlyTrustedAuthors(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	byAuthor := func(n int, login string) *Issue {
		is := readyIssue(n, "From "+login)
		is.User.Login = login
		return is
	}
	gh.issues[1] = byAuthor(1, "Teammate")
	gh.issues[2] = byAuthor(2, "stranger")
	gh.issues[3] = byAuthor(3, "")
	ran := filepath.Join(t.TempDir(), "ran")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", shResult+`prompt=$(cat)
printf '%s\n' "$prompt" | grep '^# #' >> `+ran+`
echo x > x.txt
result 'ok'`))
	r.Trusted = []string{"vic", "teammate"}

	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("trusted author: %v\nlog:\n%s", err, log)
	}
	for n, who := range map[int]string{2: "@stranger", 3: "an unknown author"} {
		err := r.Run(context.Background(), n)
		if err == nil || !strings.Contains(err.Error(), "not in AMF_TRUSTED") {
			t.Errorf("#%d: error = %v", n, err)
		}
		if got := gh.labels(n); !reflect.DeepEqual(got, []string{"amf:failed"}) {
			t.Errorf("#%d labels = %v", n, got)
		}
		if c := gh.comments[n]; len(c) != 1 || !strings.Contains(c[0], who) || !strings.Contains(c[0], "AMF_TRUSTED") || strings.Contains(c[0], "```") {
			t.Errorf("#%d comments = %q", n, c)
		}
		if _, err := os.Stat(filepath.Join(root, ".amf", "worktrees", fmt.Sprintf("issue-%d", n))); !os.IsNotExist(err) {
			t.Errorf("#%d got a worktree", n)
		}
	}
	if b := mustRead(t, ran); b != "# #1: From Teammate\n" {
		t.Errorf("claude ran for:\n%s", b)
	}
	if len(gh.prs) != 1 {
		t.Errorf("PRs = %v", gh.prs)
	}
}

func TestWatchDoesntRetryUntrustedIssues(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Untrusted")
	gh.issues[1].User.Login = "stranger"
	r, _ := newTestRunner(t, srv, root, fakeAgent(t, "claude", "exit 9"))
	r.Trusted = []string{"vic"}
	var out syncBuffer
	r.Log = &out
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	r.Watch(ctx, "amf:ready", 20*time.Millisecond)
	if c := gh.comments[1]; len(c) != 1 {
		t.Errorf("watch commented %d times: %q", len(c), c)
	}
}
