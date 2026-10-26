package main

// Edge cases: odd input, failing agents and GitHub, re-runs, and the stages
// of a pipeline misbehaving. Fake agents are shell scripts; GitHub is the
// in-memory fake from amf_test.go.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
	t.Setenv("AMF_CODEX", "") // from the file, not TestMain's placeholder
	t.Setenv("AMF_OPENCODE", "")
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
		{[]string{"run", "1", "--timeout", "0"}, "--timeout: agent timeout must be positive"},
		{[]string{"run", "1", "--timeout", "-5m"}, "--timeout: agent timeout must be positive"},
		{[]string{"run", "1", "--eval-rounds", "-1"}, "can't be negative"},
		{[]string{"run", "1", "--log-level", "loud"}, "--log-level: log level must be"},
		{[]string{"watch", "--log-level", "loud"}, "--log-level: log level must be"},
		{[]string{"run", "#3"}, "GITHUB_TOKEN is not set"},
		{[]string{"watch", "extra"}, "usage"},
		{[]string{"watch", "--interval", "0"}, "--interval must be positive"},
		{[]string{"watch", "--timeout", "0"}, "--timeout: agent timeout must be positive"},
		{[]string{"watch", "--eval-rounds", "-2"}, "can't be negative"},
		{[]string{"watch", "--exclude-label", ""}, "--exclude-label can't be empty"},
		{[]string{"watch", "--exclude-label", "blocked", "--exclude-label", " \t"}, "--exclude-label can't be empty"},
		{[]string{"watch", "--label", ""}, "--label must not be empty"},
		{[]string{"run", "1", "--exclude-label", "blocked"}, "usage"},
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
	t.Setenv("AMF_CLAUDE", fakeAgent(t, "claude", "exit 0"))
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := setupRepo(t)
			gh, srv := newFakeGitHub(t)
			gh.issues[1] = readyIssue(1, "Anything")
			r, _ := newTestRunner(t, srv, root, fakeAgent(t, "claude", tc.script))
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

func TestProtectedPaths(t *testing.T) {
	changed := []string{".github/workflows/ci.yml", ".github/CODEOWNERS", "github/x.go", "docs/.github.md", ".github"}
	got := protectedPaths(changed)
	want := []string{".github/workflows/ci.yml", ".github/CODEOWNERS"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("protectedPaths = %q, want %q", got, want)
	}
	if bad := protectedPaths(nil); bad != nil {
		t.Errorf("protectedPaths(nil) = %q, want none", bad)
	}
}

func TestRunRefusesGitHubChanges(t *testing.T) {
	root, origin := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Tamper with workflows")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", shResult+`cat >/dev/null
mkdir -p .github/workflows
echo evil > .github/workflows/evil.yml
echo x > x.txt
result 'Done.'
`))
	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "refuses to push") || !strings.Contains(err.Error(), ".github/workflows/evil.yml") {
		t.Fatalf("expected the refusal, got %v\nlog:\n%s", err, log)
	}
	if len(gh.prs) != 0 {
		t.Errorf("PRs = %v", gh.prs)
	}
	if _, gerr := git(context.Background(), root, "--git-dir", origin, "rev-parse", "--verify", "amf/issue-1"); gerr == nil {
		t.Error("the branch was pushed to origin")
	}
	if got, want := gh.labels(1), []string{"amf:failed"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
	if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], ".github/workflows/evil.yml") {
		t.Errorf("comments = %q", c)
	}
}

// git diff quotes paths with unusual characters and lists only the new side
// of a rename, so either would hide a .github/ change from a check that
// reads its plain output.
func TestRunRefusesHiddenGitHubChanges(t *testing.T) {
	for _, tc := range []struct{ name, script, path string }{
		{"non-ASCII name", `echo evil > .github/workflows/é.yml`, ".github/workflows/é.yml"},
		{"moved out", `git mv .github/workflows/ci.yml ci.yml`, ".github/workflows/ci.yml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, origin := setupRepo(t)
			os.MkdirAll(filepath.Join(root, ".github", "workflows"), 0o755)
			os.WriteFile(filepath.Join(root, ".github", "workflows", "ci.yml"), []byte("name: ci\n"), 0o644)
			mustGit(t, root, "add", ".")
			mustGit(t, root, "commit", "-m", "add ci")
			mustGit(t, root, "push", "origin", "main")
			gh, srv := newFakeGitHub(t)
			gh.issues[1] = readyIssue(1, "Tamper with workflows")
			r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", shResult+"cat >/dev/null\n"+tc.script+"\nresult 'Done.'\n"))
			err := r.Run(context.Background(), 1)
			if err == nil || !strings.Contains(err.Error(), "refuses to push") || !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("expected the refusal naming %s, got %v\nlog:\n%s", tc.path, err, log)
			}
			if _, gerr := git(context.Background(), root, "--git-dir", origin, "rev-parse", "--verify", "amf/issue-1"); gerr == nil {
				t.Error("the branch was pushed to origin")
			}
		})
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

// The no-changes error names the agent that ran, not always claude.
func TestRunWithoutChangesNamesTheAgent(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Do nothing")
	r, _ := newTestRunner(t, srv, root, "")
	r.Agent = agentOpencode
	r.Opencode = fakeAgent(t, "opencode", "cat >/dev/null; echo 'Nothing to do.'\n")
	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "opencode finished without making any changes") {
		t.Fatalf("error = %v", err)
	}
	if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "opencode finished without making any changes") {
		t.Errorf("comments = %v", c)
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

// --exclude-label holds issues in the queue: watch leaves them as they are
// until the label goes, and works on the others meanwhile.
func TestWatchExcludeLabels(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	withLabels := func(is *Issue, labels ...string) *Issue {
		for _, l := range labels {
			is.Labels = append(is.Labels, Label{Name: l})
		}
		return is
	}
	gh.issues[1] = withLabels(readyIssue(1, "Blocked"), "blocked")
	gh.issues[2] = readyIssue(2, "Eligible")
	gh.issues[3] = withLabels(readyIssue(3, "Held twice"), "needs-human", "blocked")
	gh.issues[4] = withLabels(readyIssue(4, "Busy"), "amf:in-progress", "blocked")
	ran := filepath.Join(t.TempDir(), "ran")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", shResult+`prompt=$(cat)
printf '%s\n' "$prompt" | grep '^# #' >> `+ran+`
echo x > x.txt
result 'ok'`))
	r.ExcludeLabels = []string{"blocked", "needs-human"}
	summaryRunner(t, r)
	var file syncBuffer
	r.Events = newEventLogger(r.Log, slog.LevelInfo, &file, slog.LevelDebug, nil, "run", r.stats)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(20*time.Second, cancel)
	done := make(chan error, 1)
	go func() { done <- r.Watch(ctx, labelReady, 20*time.Millisecond) }()
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for !ok() {
			if ctx.Err() != nil {
				t.Fatalf("%s; log:\n%s", what, log)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	waitFor("#2 not processed", func() bool {
		return slices.Contains(gh.labels(2), "amf:done") && strings.Count(file.String(), `"reason":"excluded_label"`) >= 4
	})
	for n, want := range map[int][]string{1: {"amf:ready", "blocked"}, 3: {"amf:ready", "blocked", "needs-human"}} {
		if got := gh.labels(n); !reflect.DeepEqual(got, want) {
			t.Errorf("#%d labels = %v, want %v", n, got, want)
		}
	}

	// Without its hold label, #1 goes through on a later poll; #3 still has one.
	gh.mu.Lock()
	gh.issues[1].Labels = []Label{{Name: "amf:ready"}}
	gh.mu.Unlock()
	waitFor("#1 not processed after its hold label went", func() bool { return slices.Contains(gh.labels(1), "amf:done") })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	if got := gh.labels(3); !reflect.DeepEqual(got, []string{"amf:ready", "blocked", "needs-human"}) {
		t.Errorf("#3 labels = %v", got)
	}
	if got := gh.labels(4); !reflect.DeepEqual(got, []string{"amf:in-progress", "amf:ready", "blocked"}) {
		t.Errorf("#4 labels = %v", got)
	}
	for _, n := range []int{3, 4} {
		if c := gh.comments[n]; len(c) != 0 {
			t.Errorf("#%d comments = %q", n, c)
		}
		if _, err := os.Stat(filepath.Join(root, ".amf", "worktrees", fmt.Sprintf("issue-%d", n))); !os.IsNotExist(err) {
			t.Errorf("#%d got a worktree", n)
		}
	}
	if b := mustRead(t, ran); b != "# #2: Eligible\n# #1: Blocked\n" {
		t.Errorf("claude ran for:\n%s", b)
	}
	if len(gh.prs) != 2 || gh.prs[0]["head"] != "amf/issue-2" || gh.prs[1]["head"] != "amf/issue-1" {
		t.Errorf("PRs = %v", gh.prs)
	}

	ev := eventsByName(eventLines(t, file.String()))
	wantEvent(t, ev, "watch.start", map[string]any{"label": "amf:ready", "exclude_labels": []any{"blocked", "needs-human"}})
	wantEvent(t, ev, "watch.skip", map[string]any{"issue": float64(1), "reason": "excluded_label", "label": "blocked"})
	wantEvent(t, ev, "watch.skip", map[string]any{"issue": float64(3), "reason": "excluded_label", "label": "blocked"})
	wantEvent(t, ev, "watch.skip", map[string]any{"issue": float64(4), "reason": "in_progress"})
	for _, e := range ev["watch.skip"] {
		if e["issue"] == float64(4) && e["reason"] != "in_progress" {
			t.Errorf("#4 skip = %v, want the lifecycle reason", e)
		}
	}
	// Skipped on every poll, each counts once.
	if s := r.stats.snapshot(); s.Issues.Done != 2 || s.Issues.Skipped != 3 {
		t.Errorf("summary = %+v", s)
	}
}

// Exclusions go with a custom --label, reach candidates on later pages, and
// leave amf run N alone.
func TestWatchExcludeLabelAcrossPages(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	for n := 1; n <= 100; n++ {
		gh.issues[n] = &Issue{Number: n, Title: "Held", State: "open", Labels: []Label{{Name: "team:backend"}, {Name: "on-hold"}}}
	}
	gh.issues[101] = &Issue{Number: 101, Title: "Eligible", State: "open", Labels: []Label{{Name: "team:backend"}}}
	gh.issues[102] = &Issue{Number: 102, Title: "Other queue", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", shResult+"cat >/dev/null; echo x > x.txt; result 'ok'"))
	r.ExcludeLabels = []string{"on-hold", "no-such-label"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(20*time.Second, cancel)
	done := make(chan error, 1)
	go func() { done <- r.Watch(ctx, "team:backend", 20*time.Millisecond) }()
	for !slices.Contains(gh.labels(101), "amf:done") {
		if ctx.Err() != nil {
			t.Fatalf("#101 not processed; log:\n%s", log)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(gh.prs) != 1 || gh.prs[0]["head"] != "amf/issue-101" {
		t.Errorf("PRs = %v", gh.prs)
	}
	for _, n := range []int{1, 100, 102} {
		if len(gh.comments[n]) != 0 || slices.ContainsFunc(gh.labels(n), func(l string) bool { return l != "team:backend" && l != "on-hold" && l != "amf:ready" }) {
			t.Errorf("#%d changed: labels %v, comments %q", n, gh.labels(n), gh.comments[n])
		}
	}

	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("amf run on a held issue: %v\nlog:\n%s", err, log)
	}
	if !slices.Contains(gh.labels(1), "amf:done") {
		t.Errorf("#1 labels after amf run = %v", gh.labels(1))
	}
}

func TestLabelsFlag(t *testing.T) {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	var f labelsFlag
	fs.Var(&f, "exclude-label", "")
	if err := fs.Parse([]string{"--exclude-label", " blocked ", "--exclude-label=on hold", "--exclude-label", "blocked"}); err != nil {
		t.Fatal(err)
	}
	if want := (labelsFlag{"blocked", "on hold"}); !reflect.DeepEqual(f, want) {
		t.Errorf("labels = %q, want %q", f, want)
	}
}

// --- the agent for issues without a pipeline ---

// fakeDefaultCodex is codex run without a pipeline: no model, its final
// message written to the -o file inside the worktree's .amf/.
const fakeDefaultCodex = `
case "$*" in "exec --dangerously-bypass-approvals-and-sandbox -o "*"/.amf/code.md -") ;; *) echo "bad args: $*" >&2; exit 5;; esac
if [ -n "$GITHUB_TOKEN$CLAUDE_CODE_OAUTH_TOKEN" ]; then echo "token leaked" >&2; exit 3; fi
[ -z "$OPENAI_API_KEY" ] || [ "$OPENAI_API_KEY" = sk-openai-test ] || { echo "wrong OPENAI_API_KEY" >&2; exit 6; }
case "$(cat)" in *"Add feature file"*) ;; *) echo "bad prompt" >&2; exit 4;; esac
echo hello > feature.txt
echo "Added feature.txt with codex (key: ${OPENAI_API_KEY:-none})." > "$4"
`

// fakeDefaultOpencode is opencode run without a pipeline, writing its final
// reply to the file the prompt names.
const fakeDefaultOpencode = `
[ "$*" = run ] || { echo "bad args: $*" >&2; exit 5; }
out=$(cat | sed -n 's/.*final reply, exactly as you give it, to the file \(.*\)\.$/\1/p')
echo hello > feature.txt
echo "Added feature.txt with opencode." > "$out"
`

func TestDefaultAgentFallsBack(t *testing.T) {
	missing := func(name string) func(t *testing.T) string {
		return func(t *testing.T) string { return filepath.Join(t.TempDir(), "no-"+name) }
	}
	mustNotRun := func(name string) func(t *testing.T) string {
		return func(t *testing.T) string { return fakeAgent(t, name, "echo "+name+" must not run >&2; exit 9") }
	}
	claude := func(t *testing.T) string {
		return fakeAgent(t, "claude", `cat >/dev/null; echo hello > feature.txt; echo '{"type":"result","subtype":"success","is_error":false,"result":"Added feature.txt with claude (key: '"$ANTHROPIC_API_KEY"')."}'`)
	}
	codex := func(t *testing.T) string { return fakeAgent(t, "codex", fakeDefaultCodex) }
	opencode := func(t *testing.T) string { return fakeAgent(t, "opencode", fakeDefaultOpencode) }
	for _, tc := range []struct {
		name                    string
		claude, codex, opencode func(t *testing.T) string
		anthropicKey, openAIKey string
		agent                   string // --agent
		want                    string // in the PR body
	}{
		{"claude, installed with a key", claude, mustNotRun("codex"), mustNotRun("opencode"),
			"sk-ant-test", "sk-openai-test", "", "with claude (key: sk-ant-test)."},
		{"codex when claude has no key", mustNotRun("claude"), codex, mustNotRun("opencode"),
			"", "sk-openai-test", "", "with codex (key: sk-openai-test)."},
		{"codex when claude isn't installed", missing("claude"), codex, mustNotRun("opencode"),
			"sk-ant-test", "sk-openai-test", "", "with codex (key: sk-openai-test)."},
		{"opencode when claude and codex have no key", mustNotRun("claude"), mustNotRun("codex"), opencode,
			"", "", "", "with opencode."},
		{"opencode when neither is installed", missing("claude"), missing("codex"), opencode,
			"sk-ant-test", "sk-openai-test", "", "with opencode."},
		{"--agent codex over a keyed claude, without a key of its own", mustNotRun("claude"), codex, missing("opencode"),
			"sk-ant-test", "", agentCodex, "with codex (key: none)."},
		{"--agent claudecode without a key", claude, mustNotRun("codex"), mustNotRun("opencode"),
			"", "", agentClaude, "with claude (key: )."},
		{"--agent opencode", mustNotRun("claude"), mustNotRun("codex"), opencode,
			"sk-ant-test", "sk-openai-test", agentOpencode, "with opencode."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := setupRepo(t)
			gh, srv := newFakeGitHub(t)
			gh.issues[1] = readyIssue(1, "Add feature file")
			t.Setenv("GITHUB_TOKEN", "must-not-leak")
			r, log := newTestRunner(t, srv, root, tc.claude(t))
			r.Codex, r.Opencode, r.Agent = tc.codex(t), tc.opencode(t), tc.agent
			r.AnthropicKey, r.OpenAIKey = tc.anthropicKey, tc.openAIKey
			if err := r.Run(context.Background(), 1); err != nil {
				t.Fatalf("Run: %v\nlog:\n%s", err, log)
			}
			if len(gh.prs) != 1 || !strings.Contains(gh.prs[0]["body"], tc.want) {
				t.Fatalf("PRs = %v, want a body with %q", gh.prs, tc.want)
			}
			if got := gh.labels(1); !reflect.DeepEqual(got, []string{"amf:done"}) {
				t.Errorf("labels = %v", got)
			}
		})
	}
}

func TestWatchShowsAgents(t *testing.T) {
	root, _ := setupRepo(t)
	_, srv := newFakeGitHub(t)
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", "exit 0"))
	r.AnthropicKey, r.ClaudeToken = "", "sk-ant-oat-test"
	r.Codex = fakeAgent(t, "codex", "exit 0")
	r.Opencode = filepath.Join(t.TempDir(), "no-opencode")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // print the startup lines, then stop
	if err := r.Watch(ctx, labelReady, time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"amf: agents for issues without a pipeline (claude and codex need a key, opencode one for a paid model):",
		"claude   installed at " + r.Claude + "; CLAUDE_CODE_OAUTH_TOKEN is set: usable",
		"codex    installed at " + r.Codex + "; OPENAI_API_KEY not set: skipped",
		"opencode not installed; needs no key: skipped",
		"amf: issues without a pipeline run claude\n",
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}

	// Without claude's token it falls through codex (no key) to nothing.
	log.Reset()
	r.ClaudeToken = ""
	if err := r.Watch(ctx, labelReady, time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"claude   installed at " + r.Claude + "; ANTHROPIC_API_KEY and CLAUDE_CODE_OAUTH_TOKEN not set: skipped",
		"warning: no coding agent is available (claude has no ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN; codex has no OPENAI_API_KEY; ",
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
}

func TestAgentKeysReachAgents(t *testing.T) {
	r := &Runner{AnthropicKey: "sk-ant-file", OpenAIKey: "sk-openai-file"}
	env := []string{"PATH=/bin", "ANTHROPIC_API_KEY=from-env", "OPENAI_API_KEY=from-env"}
	has := func(env []string, kv string) bool { return slices.Contains(env, kv) }
	if c := r.agentEnv(slices.Clone(env), agentClaude, "/wt"); !has(c, "ANTHROPIC_API_KEY=sk-ant-file") || has(c, "ANTHROPIC_API_KEY=from-env") {
		t.Errorf("claude env = %v", c)
	}
	if c := r.agentEnv(slices.Clone(env), agentCodex, "/wt"); !has(c, "OPENAI_API_KEY=sk-openai-file") || has(c, "OPENAI_API_KEY=from-env") {
		t.Errorf("codex env = %v", c)
	}
	// The subscription token wins over an API key for claude.
	r.ClaudeToken = "sk-ant-oat"
	if c := r.agentEnv(slices.Clone(env), agentClaude, "/wt"); !has(c, "CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat") || slices.ContainsFunc(c, func(kv string) bool { return strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") }) {
		t.Errorf("claude env with a token = %v", c)
	}
}

func TestNoAgentInstalledLeavesIssueAlone(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Anything")
	r, _ := newTestRunner(t, srv, root, filepath.Join(t.TempDir(), "no-claude"))
	r.Codex = filepath.Join(t.TempDir(), "no-codex")
	r.Opencode = filepath.Join(t.TempDir(), "no-opencode")

	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "no coding agent is available (") ||
		!strings.Contains(err.Error(), "no-claude is not installed; ") || !strings.Contains(err.Error(), "no-opencode is not installed)") {
		t.Fatalf("error = %v", err)
	}
	// Found before Claim: the issue keeps amf:ready, so watch tries again later.
	if got := gh.labels(1); !reflect.DeepEqual(got, []string{"amf:ready"}) {
		t.Errorf("labels = %v", got)
	}
	if len(gh.comments[1]) != 0 {
		t.Errorf("comments = %q", gh.comments[1])
	}
	if _, err := os.Stat(filepath.Join(root, ".amf", "worktrees", "issue-1")); !os.IsNotExist(err) {
		t.Errorf("no worktree expected: %v", err)
	}

	// A named agent that isn't installed doesn't fall back to another one.
	r.Claude = fakeAgent(t, "claude", "echo claude must not run >&2; exit 9")
	r.Agent = agentCodex
	if err := r.Run(context.Background(), 1); err == nil || !strings.Contains(err.Error(), "--agent codex: ") {
		t.Fatalf("error = %v", err)
	}
	if got := gh.labels(1); !reflect.DeepEqual(got, []string{"amf:ready"}) {
		t.Errorf("labels = %v", got)
	}
}

func TestAgentFlag(t *testing.T) {
	for in, want := range map[string]string{"": "", "claude": agentClaude, "ClaudeCode": agentClaude, " codex ": agentCodex, "opencode": agentOpencode} {
		if got, err := parseAgent(in); err != nil || got != want {
			t.Errorf("parseAgent(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := parseAgent("gemini"); err == nil || !strings.Contains(err.Error(), "--agent must be claudecode, codex or opencode") {
		t.Errorf("parseAgent(gemini) = %v", err)
	}

	// run and watch reject a bad or missing agent before talking to GitHub.
	t.Chdir(t.TempDir())
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_CODEX", filepath.Join(t.TempDir(), "no-codex"))
	for _, args := range [][]string{{"run", "1", "--agent", "gemini"}, {"watch", "--agent=gemini"}} {
		if err := run(context.Background(), args, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "--agent must be") {
			t.Errorf("amf %v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"run", "1", "--agent", "codex"}, {"watch", "--agent", "codex"}} {
		if err := run(context.Background(), args, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "--agent codex: ") {
			t.Errorf("amf %v: %v", args, err)
		}
	}
}

func TestAgentFlagIgnoredWithPipeline(t *testing.T) {
	root, _ := setupPipelineRepo(t, "## amf\n\n| Stage | Agent |\n|---|---|\n| code | codex |\n")
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", "echo claude must not run >&2; exit 9"))
	r.Codex = fakeAgent(t, "codex", `
while [ "$1" != "-o" ]; do shift; done
cat >/dev/null; echo hello > feature.txt; echo "Pipeline codex." > "$2"
`)
	r.Agent = agentClaude
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	if !strings.Contains(log.String(), "ignoring --agent claudecode: AGENTS.md on origin/main declares a pipeline") {
		t.Errorf("log = %s", log)
	}
	if len(gh.prs) != 1 || !strings.Contains(gh.prs[0]["body"], "Pipeline codex.") {
		t.Fatalf("PRs = %v", gh.prs)
	}
}

func TestListIssuesOrdersByNumber(t *testing.T) {
	// GitHub's created_at has one-second resolution, so two issues opened in
	// the same second can come back in either order. watch must still take
	// the older one first.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, []Issue{{Number: 79, State: "open"}, {Number: 78, State: "open"}, {Number: 80, State: "open"}})
	}))
	t.Cleanup(srv.Close)
	c := newGitHub(Config{Token: "t", Repo: "vic/demo", APIURL: srv.URL})
	for _, tc := range []struct {
		asc  bool
		want []int
	}{{true, []int{78, 79, 80}}, {false, []int{80, 79, 78}}} {
		issues, err := c.ListIssues(context.Background(), "open", "", tc.asc)
		if err != nil {
			t.Fatal(err)
		}
		var got []int
		for _, is := range issues {
			got = append(got, is.Number)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("asc=%v: got %v, want %v", tc.asc, got, tc.want)
		}
	}
}

// --- OPENCODE_MODEL ---

func TestOpencodeModel(t *testing.T) {
	t.Setenv("OPENCODE_MODEL", "")
	cfg, err := loadConfig(writeEnv(t, "GITHUB_TOKEN=t\nGITHUB_REPO=vic/demo\n"))
	if err != nil || cfg.OpencodeModel != "opencode/big-pickle" {
		t.Fatalf("default OpencodeModel = %q, %v", cfg.OpencodeModel, err)
	}
	cfg, _ = loadConfig(writeEnv(t, "GITHUB_TOKEN=t\nGITHUB_REPO=vic/demo\n# OPENCODE_MODEL=opencode/other\n"))
	if cfg.OpencodeModel != "opencode/big-pickle" {
		t.Errorf("a commented-out OPENCODE_MODEL should leave the default, got %q", cfg.OpencodeModel)
	}
	cfg, _ = loadConfig(writeEnv(t, "GITHUB_TOKEN=t\nGITHUB_REPO=vic/demo\nOPENCODE_MODEL=opencode/from-file\n"))
	if cfg.OpencodeModel != "opencode/from-file" {
		t.Errorf("OpencodeModel = %q", cfg.OpencodeModel)
	}

	// Without a pipeline, opencode always gets the model.
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	r, log := newTestRunner(t, srv, root, filepath.Join(t.TempDir(), "no-claude"))
	r.OpencodeModel = "opencode/test-model-free"
	r.Opencode = fakeAgent(t, "opencode", `
[ "$*" = "run --model opencode/test-model-free" ] || { echo "bad args: $*" >&2; exit 5; }
out=$(cat | sed -n 's/.*final reply, exactly as you give it, to the file \(.*\)\.$/\1/p')
echo hello > feature.txt
echo "Added with the model." > "$out"
`)
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	if !strings.Contains(log.String(), "#1: running opencode (opencode/test-model-free, timeout ") {
		t.Errorf("log = %s", log)
	}
}

func TestOpencodeModelInPipeline(t *testing.T) {
	// A stage that names no model gets OPENCODE_MODEL; one that names a model keeps it.
	root, _ := setupPipelineRepo(t, "## amf\n\n| Stage | Agent | Model |\n|---|---|---|\n| code | opencode | default |\n| eval | opencode | opencode/eval-model |\n")
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	r, log := newTestRunner(t, srv, root, filepath.Join(t.TempDir(), "no-claude"))
	r.OpencodeModel = "opencode/env-model"
	r.Opencode = fakeAgent(t, "opencode", `
prompt=$(cat)
out=$(printf '%s\n' "$prompt" | sed -n 's/.*final reply, exactly as you give it, to the file \(.*\)\.$/\1/p')
case "$prompt" in
*"VERDICT: PASS"*) [ "$*" = "run --model opencode/eval-model" ] || { echo "eval args: $*" >&2; exit 5; }; printf 'ok\n\nVERDICT: PASS\n' > "$out" ;;
*) [ "$*" = "run --model opencode/env-model" ] || { echo "code args: $*" >&2; exit 5; }; echo hello > feature.txt; echo "Coded." > "$out" ;;
esac
`)
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	for _, want := range []string{"code-1: running opencode (opencode/env-model,", "eval-1: running opencode (opencode/eval-model,"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
}

func TestInitOpencodeModelLine(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("OPENCODE_MODEL", "")
	var out bytes.Buffer
	if err := run(context.Background(), []string{"init"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	env := mustRead(t, ".env")
	if !strings.Contains(env, "\n# OPENCODE_MODEL=opencode/big-pickle   # opencode/<id> for free and pay-as-you-go models, opencode-go/<id> for Go; 'opencode models' lists them\n") {
		t.Fatalf(".env:\n%s", env)
	}
	if !strings.Contains(env, "\n# AMF_POSTGRES_URL=postgres://user:password@localhost:5432/postgres   # optional: also save the logs to database amflogs on this PostgreSQL server\n") {
		t.Fatalf(".env should have AMF_POSTGRES_URL commented out:\n%s", env)
	}
	if strings.Contains(out.String(), "fill in") && strings.Contains(out.String(), "OPENCODE_MODEL") {
		t.Errorf("OPENCODE_MODEL is optional, not something to fill in:\n%s", out.String())
	}
	// Re-running changes nothing, and a real value is kept, uncommented.
	out.Reset()
	run(context.Background(), []string{"init"}, nil, &out, io.Discard)
	if !strings.Contains(out.String(), ".env is up to date") {
		t.Errorf("second init:\n%s", out.String())
	}
	os.WriteFile(".env", []byte(strings.Replace(env, "# OPENCODE_MODEL=opencode/big-pickle", "OPENCODE_MODEL=opencode/mine", 1)), 0o600)
	run(context.Background(), []string{"init"}, nil, io.Discard, io.Discard)
	if env := mustRead(t, ".env"); !strings.Contains(env, "\nOPENCODE_MODEL=opencode/mine   # opencode/<id> for free") || strings.Contains(env, "Kept from the previous") {
		t.Errorf(".env after a real value:\n%s", env)
	}
}

func TestDoctorOpencodeModel(t *testing.T) {
	withGitUser(t)
	t.Chdir(t.TempDir())
	t.Setenv("AMF_RELEASES_URL", "http://127.0.0.1:1")
	t.Setenv("AMF_OPENCODE", fakeAgent(t, "opencode", `case "$1" in models) printf 'opencode/a\nopencode/listed\n' ;; *) echo "opencode v2" ;; esac`))
	for model, want := range map[string]string{
		"opencode/listed": `ok +opencode model +opencode/listed; opencode lists it$`,
		"":                `warn +opencode model +opencode/big-pickle \(the default\) isn't in .opencode models., so opencode runs would fail`,
	} {
		t.Setenv("OPENCODE_MODEL", model)
		out, _ := doctorOutput(t)
		wantLines(t, out, want)
	}
}

func TestWatchShowsOpencodeModel(t *testing.T) {
	r := &Runner{Opencode: fakeAgent(t, "opencode", "exit 0"), OpencodeModel: "opencode/m-free"}
	sts := r.agentStatuses()
	if len(sts) != 1 || !strings.HasSuffix(sts[0].describe(), "; needs no key, model opencode/m-free: usable") {
		t.Fatalf("statuses = %+v", sts)
	}
}

// --- OPENCODE_API_KEY ---

// fakeOpencodeAuth is an opencode whose `auth list --format json` lists a
// saved credential for each of providers.
func fakeOpencodeAuth(t *testing.T, providers ...string) string {
	t.Helper()
	var list []string
	for _, p := range providers {
		list = append(list, `{"id":"`+p+`","name":"x","connections":[{"type":"credential","id":"cred_1","label":"API key","method":"key"}]}`)
	}
	// An environment variable is a connection too, but not a saved login.
	list = append(list, `{"id":"opencode","name":"OpenCode Console","connections":[{"type":"env","name":"OPENCODE_API_KEY"}]}`)
	return fakeAgent(t, "opencode", `case "$*" in
"auth list --format json") echo '[`+strings.Join(list, ",")+`]' ;;
*) echo "opencode v2" ;;
esac`)
}

func TestOpencodeProvider(t *testing.T) {
	for model, want := range map[string]string{
		"opencode/big-pickle":               "",
		"opencode/deepseek-v4.1-flash-free": "",
		"opencode/gpt-6.1-sol":              "opencode",
		"opencode-go/kimi-k3":               "opencode-go",
		"anthropic/claude-sonnet-5-5":       "",
		"":                                  "",
	} {
		if got := opencodeProvider(model); got != want {
			t.Errorf("opencodeProvider(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestOpencodeKeyPicksOpencode(t *testing.T) {
	// The model × key matrix: a free model needs nothing; a paid or Go one
	// needs OPENCODE_API_KEY or a saved login for its provider.
	for _, tc := range []struct {
		model, key string
		saved      []string
		want       string
	}{
		{"opencode/big-pickle", "", nil, "needs no key, model opencode/big-pickle: usable"},
		{"opencode/big-pickle", "oc_sk_test", nil, "OPENCODE_API_KEY is set, model opencode/big-pickle: usable"},
		{"opencode/gpt-6.1-sol", "oc_sk_test", nil, "OPENCODE_API_KEY is set, model opencode/gpt-6.1-sol: usable"},
		{"opencode/gpt-6.1-sol", "", nil, "OPENCODE_API_KEY not set and no saved login, model opencode/gpt-6.1-sol: skipped"},
		{"opencode/gpt-6.1-sol", "", []string{"opencode"}, "uses its saved login, model opencode/gpt-6.1-sol: usable"},
		{"opencode-go/kimi-k3", "oc_sk_test", nil, "OPENCODE_API_KEY is set, model opencode-go/kimi-k3: usable"},
		{"opencode-go/kimi-k3", "", []string{"opencode"}, "OPENCODE_API_KEY not set and no saved login, model opencode-go/kimi-k3: skipped"},
		{"opencode-go/kimi-k3", "", []string{"opencode-go"}, "uses its saved login, model opencode-go/kimi-k3: usable"},
	} {
		r := &Runner{Opencode: fakeOpencodeAuth(t, tc.saved...), OpencodeModel: tc.model, OpencodeKey: tc.key}
		sts := r.agentStatuses()
		if len(sts) != 1 || !strings.HasSuffix(sts[0].describe(), "; "+tc.want) {
			t.Errorf("%s, key %q, saved %v: %+v, want %q", tc.model, tc.key, tc.saved, sts, tc.want)
			continue
		}
		_, passed, err := r.pickAgent()
		if sts[0].usable() != (err == nil) {
			t.Errorf("%s: pickAgent = %v", tc.model, err)
		}
		if err != nil && !strings.Contains(err.Error(), "opencode has no OPENCODE_API_KEY or saved login for "+tc.model) {
			t.Errorf("%s: pickAgent = %v, passed %v", tc.model, err, passed)
		}
	}
}

func TestOpencodeKeyReachesOpencode(t *testing.T) {
	r := &Runner{OpencodeKey: "oc_sk_file"}
	env := []string{"PATH=/bin", "OPENCODE_API_KEY=from-env"}
	if c := r.agentEnv(slices.Clone(env), agentOpencode, "/wt"); !slices.Contains(c, "OPENCODE_API_KEY=oc_sk_file") || slices.Contains(c, "OPENCODE_API_KEY=from-env") {
		t.Errorf("opencode env = %v", c)
	}
	for _, agent := range []string{agentClaude, agentCodex} {
		if c := r.agentEnv(slices.Clone(env), agent, "/wt"); slices.Contains(c, "OPENCODE_API_KEY=oc_sk_file") {
			t.Errorf("%s got opencode's key: %v", agent, c)
		}
	}
	// Without the key, opencode keeps what it inherited.
	r.OpencodeKey = ""
	if c := r.agentEnv(slices.Clone(env), agentOpencode, "/wt"); !slices.Contains(c, "OPENCODE_API_KEY=from-env") {
		t.Errorf("opencode env without a key = %v", c)
	}

	cfg, err := loadConfig(writeEnv(t, "GITHUB_TOKEN=t\nGITHUB_REPO=vic/demo\nOPENCODE_API_KEY=oc_sk_0123456789abcdef\n"))
	if err != nil || cfg.OpencodeKey != "oc_sk_0123456789abcdef" {
		t.Fatalf("OpencodeKey = %q, %v", cfg.OpencodeKey, err)
	}
	if got := configRedactor(cfg).Redact("key=oc_sk_0123456789abcdef"); strings.Contains(got, "0123456789abcdef") {
		t.Errorf("redacted = %q", got)
	}
	cfg, _ = loadConfig(writeEnv(t, "GITHUB_TOKEN=t\nGITHUB_REPO=vic/demo\n# OPENCODE_API_KEY=oc_sk_...\n"))
	if cfg.OpencodeKey != "" {
		t.Errorf("the init placeholder should leave OPENCODE_API_KEY unset, got %q", cfg.OpencodeKey)
	}
}

// fakeOpencodeAPI serves the Console's model lists: /v1/models to any key
// but "bad", and /go/v1/models only to "go".
func fakeOpencodeAPI(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch {
		case key == "oc_sk_bad":
			http.Error(w, `{"error":{"message":"Invalid credential"}}`, http.StatusUnauthorized)
		case r.URL.Path == "/v1/models":
			io.WriteString(w, `{"object":"list","data":[{"id":"big-pickle"},{"id":"gpt-6.1-sol"}]}`)
		case r.URL.Path == "/go/v1/models" && key == "oc_sk_go":
			io.WriteString(w, `{"object":"list","data":[{"id":"kimi-k3"}]}`)
		case r.URL.Path == "/go/v1/models":
			http.Error(w, "not subscribed", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := opencodeAPI
	opencodeAPI = srv.URL
	t.Cleanup(func() { opencodeAPI = old })
}

func TestDoctorOpencodeAuth(t *testing.T) {
	withGitUser(t)
	t.Chdir(t.TempDir())
	t.Setenv("AMF_RELEASES_URL", "http://127.0.0.1:1")
	fakeOpencodeAPI(t)
	noLogin, login := fakeOpencodeAuth(t), fakeOpencodeAuth(t, "opencode", "opencode-go")
	for _, tc := range []struct{ model, key, bin, want string }{
		{"", "", noLogin, `ok +opencode auth +OPENCODE_API_KEY is not set; opencode/big-pickle needs none$`},
		{"opencode/gpt-6.1-sol", "", noLogin, `FAIL +opencode auth +OPENCODE_API_KEY is not set and opencode has no saved login, so it can't run opencode/gpt-6.1-sol; set OPENCODE_API_KEY`},
		{"opencode-go/kimi-k3", "", login, `ok +opencode auth +OPENCODE_API_KEY is not set; opencode runs opencode-go/kimi-k3 with its saved login$`},
		{"opencode-go/kimi-k3", "", "no-opencode", `skip +opencode auth +OPENCODE_API_KEY is not set; opencode is not installed$`},
		{"opencode-go/kimi-k3", "oc_sk_go", noLogin, `ok +opencode auth +OPENCODE_API_KEY, from the environment, grants pay-as-you-go and Go; opencode runs opencode-go/kimi-k3 with it$`},
		{"opencode-go/kimi-k3", "oc_sk_paid", noLogin, `FAIL +opencode auth +OPENCODE_API_KEY, from the environment, grants pay-as-you-go, not Go \(its workspace isn't subscribed to OpenCode Go\), so opencode can't run opencode-go/kimi-k3$`},
		{"opencode-go/nope", "oc_sk_go", noLogin, `warn +opencode auth +OPENCODE_API_KEY, from the environment, grants pay-as-you-go and Go, but Go has no model nope$`},
		{"opencode/gpt-6.1-sol", "oc_sk_paid", noLogin, `ok +opencode auth +OPENCODE_API_KEY, from the environment, grants pay-as-you-go, not Go .*; opencode runs opencode/gpt-6.1-sol with it$`},
		{"opencode/nope", "oc_sk_paid", noLogin, `warn +opencode auth +.*but the Console has no model nope$`},
		{"opencode/gpt-6.1-sol", "oc_sk_bad", noLogin, `FAIL +opencode auth +OpenCode rejected OPENCODE_API_KEY, from the environment`},
	} {
		t.Setenv("OPENCODE_MODEL", tc.model)
		t.Setenv("OPENCODE_API_KEY", tc.key)
		t.Setenv("AMF_OPENCODE", tc.bin)
		out, _ := doctorOutput(t)
		wantLines(t, out, tc.want)
		if tc.key != "" && strings.Contains(out, tc.key) {
			t.Errorf("doctor printed the key:\n%s", out)
		}
	}
}

// writeEnv writes a .env with body in a temp dir and returns its path.
func writeEnv(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPipelineCommandShowsOpencodeModel(t *testing.T) {
	root, _ := setupRepo(t)
	t.Chdir(root)
	os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("## amf\n\n| Stage | Agent | Model |\n|---|---|---|\n| code | opencode | |\n| eval | opencode | opencode/named |\n"), 0o644)
	t.Setenv("AMF_OPENCODE", fakeAgent(t, "opencode", "exit 0"))
	t.Setenv("OPENCODE_MODEL", "opencode/env-model")
	var out bytes.Buffer
	if err := run(context.Background(), []string{"pipeline"}, nil, &out, io.Discard); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, want := range []string{"code   opencode  opencode/env-model (OPENCODE_MODEL)", "eval   opencode  opencode/named "} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

// A PostgreSQL server that can't be reached never stops an issue: amf warns
// and keeps logging locally.
func TestPostgresDownKeepsLocalLogs(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close() // nothing listens there now
	pg, err := parsePostgresURL("postgres://amf:pw@" + addr + "/?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}

	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	r, log := newTestRunner(t, srv, root, fakeClaude(t, `
cat >/dev/null; echo hello > feature.txt
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"Added it."}'
`))
	r.DB = startDBLog(context.Background(), pg, r.Repo, log, nil)
	r.Log = io.MultiWriter(log, r.DB.amfWriter())
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	r.Close()
	if len(gh.prs) != 1 {
		t.Fatalf("PRs = %v\nlog:\n%s", gh.prs, log)
	}
	if !strings.Contains(log.String(), "amf: warning: can't log to PostgreSQL at amf@"+addr+": ") ||
		!strings.Contains(log.String(), "logging locally only, and retrying") ||
		!strings.Contains(log.String(), "amf: #1: opened") {
		t.Errorf("log:\n%s", log)
	}
	if !strings.Contains(log.String(), "couldn't send") {
		t.Errorf("Close should say the queued lines weren't sent:\n%s", log)
	}
	if strings.Contains(log.String(), "pw") {
		t.Errorf("the password leaked into the log:\n%s", log)
	}
	if b, err := os.ReadFile(filepath.Join(root, ".amf", "logs", "issue-1.log")); err != nil || !strings.Contains(string(b), "Added it.") {
		t.Errorf("local agent log = %q, %v", b, err)
	}
}

func TestPostgresURLInvalid(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_POSTGRES_URL", "mysql://u:p@h/db")
	for _, args := range [][]string{{"run", "1"}, {"watch"}} {
		if err := run(context.Background(), args, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "AMF_POSTGRES_URL: must start with postgres://") {
			t.Errorf("amf %v: %v", args, err)
		}
	}
}

func TestDoctorPostgres(t *testing.T) {
	withGitUser(t)
	t.Chdir(t.TempDir())
	t.Setenv("AMF_RELEASES_URL", "http://127.0.0.1:1")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	for url, want := range map[string]string{
		"":                                    `ok +postgres +AMF_POSTGRES_URL is not set; logs are only local$`,
		"mysql://u@h/db":                      `FAIL +postgres +AMF_POSTGRES_URL: must start with postgres://`,
		"postgres://amf:secret@" + addr + "/": `warn +postgres +can't log to amf@` + regexp.QuoteMeta(addr) + `: .*; amf run and watch log locally only`,
	} {
		t.Setenv("AMF_POSTGRES_URL", url)
		out, _ := doctorOutput(t)
		wantLines(t, out, want)
		if strings.Contains(out, "secret") {
			t.Errorf("doctor printed the password:\n%s", out)
		}
	}
}

// --- draft PRs ---

// commented reports whether issue n has a comment yet.
func (f *fakeGitHub) commented(n int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.comments[n]) > 0
}

// draftCLI runs amf with args, as a user would, against the fake GitHub and
// a fake claude, from a checkout whose issues 1 and 2 are labeled amf:ready.
// A watch runs until both issues are done.
func draftCLI(t *testing.T, args ...string) (*fakeGitHub, string) {
	t.Helper()
	root, origin := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "One")
	gh.issues[2] = readyIssue(2, "Two")
	t.Chdir(root)
	for k, v := range map[string]string{
		"GITHUB_TOKEN": "test-token", "GITHUB_REPO": "vic/demo", "AMF_API_URL": srv.URL, "AMF_CLONE_URL": origin,
		"AMF_CLAUDE":        fakeAgent(t, "claude", shResult+"cat >/dev/null\necho x > x.txt\nresult 'Done.'\n"),
		"ANTHROPIC_API_KEY": "sk-ant-test", "CLAUDE_CODE_OAUTH_TOKEN": "", "OPENAI_API_KEY": "",
		"AMF_TRUSTED": "", "AMF_POSTGRES_URL": "", "AMF_AGENT_TIMEOUT": "", "AMF_LOG_LEVEL": "",
	} {
		t.Setenv(k, v)
	}
	var stderr syncBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, strings.NewReader(""), io.Discard, &stderr) }()
	if args[0] == "watch" {
		// The comment is amf's last write for an issue, after its label, so
		// wait for it too: cancelling sooner can leave that request still
		// being handled while the test reads the fake.
		for !gh.commented(1) || !gh.commented(2) || !slices.Contains(gh.labels(1), labelDone) || !slices.Contains(gh.labels(2), labelDone) {
			if ctx.Err() != nil {
				t.Fatalf("amf %q: issues not processed:\n%s", args, stderr.String())
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
	}
	if err := <-done; err != nil {
		t.Fatalf("amf %q: %v\n%s", args, err, stderr.String())
	}
	return gh, root
}

func TestDraftFlag(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		draft bool
	}{
		{[]string{"run", "1"}, false},
		{[]string{"run", "1", "--draft"}, true},
		{[]string{"run", "--draft", "1"}, true},
		{[]string{"run", "1", "--draft=true"}, true},
		{[]string{"run", "1", "--draft=false"}, false},
		{[]string{"watch", "--interval", "50ms"}, false},
		{[]string{"watch", "--draft", "--interval", "50ms"}, true},
		{[]string{"watch", "--draft=false", "--interval", "50ms"}, false},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			gh, root := draftCLI(t, append(tc.args, "--log-file", "off")...)
			want := 1
			if tc.args[0] == "watch" {
				want = 2
			}
			if len(gh.creates) != want || len(gh.prs) != want {
				t.Fatalf("creates = %v, PRs = %v, want %d", gh.creates, gh.prs, want)
			}
			for i, in := range gh.creates {
				// The JSON boolean true for a draft; omitted otherwise, as before.
				if d, ok := in["draft"]; tc.draft && d != true || !tc.draft && ok {
					t.Errorf("create %d: draft = %#v (sent: %t), want %t", i, d, ok, tc.draft)
				}
				if gh.drafts[i] != tc.draft {
					t.Errorf("PR %d: draft = %t, want %t", i, gh.drafts[i], tc.draft)
				}
				// A draft PR is done like any other: linked, labeled and cleaned up.
				n := i + 1
				if pr := gh.prs[i]; pr["title"] != fmt.Sprintf("Fix #%d: %s", n, gh.issues[n].Title) || !strings.HasPrefix(pr["body"], fmt.Sprintf("Done.\n\nCloses #%d", n)) {
					t.Errorf("PR %d = %v", i, pr)
				}
				if c := gh.comments[n]; len(c) != 1 || c[0] != fmt.Sprintf("amf opened https://github.com/vic/demo/pull/%d for this issue.", 100+n) {
					t.Errorf("#%d comments = %q", n, c)
				}
				if got := gh.labels(n); !reflect.DeepEqual(got, []string{labelDone}) {
					t.Errorf("#%d labels = %v", n, got)
				}
				if _, err := os.Stat(filepath.Join(root, ".amf", "worktrees", fmt.Sprintf("issue-%d", n))); !os.IsNotExist(err) {
					t.Errorf("#%d worktree still there: %v", n, err)
				}
			}
			if len(gh.updates) != 0 {
				t.Errorf("updates = %v", gh.updates)
			}
		})
	}
}

func TestDraftFlagHelp(t *testing.T) {
	for _, cmd := range []string{"run", "watch"} {
		var stderr bytes.Buffer
		if err := run(context.Background(), []string{cmd, "-h"}, nil, io.Discard, &stderr); !errors.Is(err, errUsage) {
			t.Errorf("amf %s -h: %v", cmd, err)
		}
		if !strings.Contains(stderr.String(), "-draft") || !strings.Contains(stderr.String(), "open new pull requests as drafts") {
			t.Errorf("amf %s -h doesn't describe --draft:\n%s", cmd, stderr.String())
		}
	}
	if !strings.Contains(usage, "also takes --draft") {
		t.Error("usage doesn't mention --draft")
	}
}

func TestPipelineDraftPR(t *testing.T) {
	skills := skillServer(t, testSkills)
	root, _ := setupPipelineRepo(t, pipelineTable(skills.URL))
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	r, log := newPipelineRunner(t, srv, skills, root)
	r.Draft = true
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	if len(gh.creates) != 1 || gh.creates[0]["draft"] != true || !gh.drafts[0] {
		t.Fatalf("creates = %v", gh.creates)
	}
	if body := gh.prs[0]["body"]; !strings.Contains(body, "## Evaluation") || !strings.Contains(body, "Closes #1") {
		t.Errorf("PR body = %q", body)
	}
	if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "amf opened https://github.com/vic/demo/pull/101") {
		t.Errorf("comments = %q", c)
	}
	if got := gh.labels(1); !reflect.DeepEqual(got, []string{labelDone}) {
		t.Errorf("labels = %v", got)
	}
}

// TestRetryKeepsDraftState checks that a retry updates the branch's open PR
// and leaves it a draft or ready for review, whatever --draft says.
func TestRetryKeepsDraftState(t *testing.T) {
	for _, tc := range []struct{ existing, flag bool }{{true, false}, {true, true}, {false, true}, {false, false}} {
		t.Run(fmt.Sprintf("draft=%t/--draft=%t", tc.existing, tc.flag), func(t *testing.T) {
			root, _ := setupRepo(t)
			gh, srv := newFakeGitHub(t)
			gh.issues[1] = readyIssue(1, "Add a file")
			gh.prs = []map[string]string{{"head": "amf/issue-1", "base": "main", "title": "old", "body": "old", "state": "open"}}
			gh.drafts = []bool{tc.existing}
			r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", shResult+"cat >/dev/null\necho two > f.txt\nresult 'Second try.'\n"))
			r.Draft = tc.flag
			if err := r.Run(context.Background(), 1); err != nil {
				t.Fatalf("Run: %v\nlog:\n%s", err, log)
			}
			if len(gh.prs) != 1 || gh.drafts[0] != tc.existing {
				t.Fatalf("PRs = %v, drafts = %v, want the one PR with draft %t", gh.prs, gh.drafts, tc.existing)
			}
			if pr := gh.prs[0]; pr["title"] != "Fix #1: Add a file" || !strings.HasPrefix(pr["body"], "Second try.\n\nCloses #1") {
				t.Errorf("PR = %v", pr)
			}
			// One update, of the title and body only: no conversion either way.
			if len(gh.updates) != 1 {
				t.Fatalf("updates = %v", gh.updates)
			}
			if keys := slices.Sorted(maps.Keys(gh.updates[0])); !reflect.DeepEqual(keys, []string{"body", "title"}) {
				t.Errorf("update sent %v", keys)
			}
			if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "amf updated https://github.com/vic/demo/pull/101") {
				t.Errorf("comments = %q", c)
			}
			if got := gh.labels(1); !reflect.DeepEqual(got, []string{labelDone}) {
				t.Errorf("labels = %v", got)
			}
		})
	}
}

// listJSONEnv points amf list at srv from an empty directory.
func listJSONEnv(t *testing.T, srv *httptest.Server) {
	t.Chdir(t.TempDir())
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)
}

// amf list --json leaves out PRs across pages, and --limit counts issues only.
func TestListJSONPaginatesAndSkipsPRs(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	for n := 1; n <= 250; n++ {
		gh.issues[n] = &Issue{Number: n, Title: fmt.Sprint("issue ", n), State: "open"}
	}
	gh.issues[249].PullRequest = &struct{}{}
	gh.issues[150].PullRequest = &struct{}{}
	listJSONEnv(t, srv)
	list := func(args ...string) []listedIssue {
		t.Helper()
		var out bytes.Buffer
		if err := run(context.Background(), append([]string{"list", "--json"}, args...), nil, &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		var got []listedIssue
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	all := list()
	if len(all) != 248 || all[0].Number != 250 || all[1].Number != 248 || all[247].Number != 1 {
		t.Fatalf("got %d issues, want 248 newest first without the PRs", len(all))
	}
	if slices.ContainsFunc(all, func(is listedIssue) bool { return is.Number == 150 || is.Number == 249 }) {
		t.Error("pull requests should be left out")
	}
	if got := list("--limit", "3"); len(got) != 3 || got[0].Number != 250 || got[1].Number != 248 || got[2].Number != 247 {
		t.Errorf("amf list --json --limit 3 = %+v, want #250, #248, #247", got)
	}
}

// Titles survive JSON as GitHub sent them, tabs included.
func TestListJSONKeepsTitles(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	titles := []string{"Say \"hi\" \\ back", "Tab\there", "Two\nlines", "Ünïcödé ✓ 日本 <&>"}
	for i, title := range titles {
		gh.issues[i+1] = &Issue{Number: i + 1, Title: title, State: "open"}
	}
	listJSONEnv(t, srv)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"list", "--json"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var got []listedIssue
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("%q: %v", out.String(), err)
	}
	if len(got) != len(titles) {
		t.Fatalf("got %d issues, want %d", len(got), len(titles))
	}
	for i, is := range got {
		if want := titles[len(titles)-1-i]; is.Title != want {
			t.Errorf("#%d title = %q, want %q", is.Number, is.Title, want)
		}
	}
	// Titles stay readable: no HTML escaping of <, > or &.
	if raw := out.String(); !strings.Contains(raw, "<&>") || strings.Contains(raw, `\u003c`) {
		t.Errorf("raw output escapes HTML characters: %s", raw)
	}
}

// A GitHub error fails amf list --json without printing an array.
func TestListJSONAPIError(t *testing.T) {
	_, srv := newFakeGitHub(t)
	target, _ := url.Parse(srv.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/issues") {
			writeJSON(w, 502, map[string]string{"message": "Bad Gateway"})
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(failing.Close)
	listJSONEnv(t, failing)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"list", "--json"}, nil, &out, io.Discard); err == nil || !strings.Contains(err.Error(), "502") {
		t.Errorf("err = %v, want the 502", err)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", out.String())
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// amf list --json returns the error when stdout can't be written.
func TestListJSONWriteError(t *testing.T) {
	_, srv := newFakeGitHub(t)
	listJSONEnv(t, srv)
	if err := run(context.Background(), []string{"list", "--json"}, nil, failWriter{}, io.Discard); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("err = %v, want disk full", err)
	}
}

// A merge GitHub refuses, such as for a required check, leaves the PR open
// and the issue done, and run fails with why.
func TestMergeRefusedLeavesPROpen(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	gh.mergeErrs = []fakeError{{405, `Required status check "ci" is expected.`}}
	r, _ := newTestRunner(t, srv, root, fakeClaude(t, `cat >/dev/null; echo hello > feature.txt; echo done`))
	r.Merge = "merge"

	err := r.Run(context.Background(), 1)
	var me *mergeError
	if !errors.As(err, &me) || !strings.Contains(err.Error(), "could not merge it") || !strings.Contains(err.Error(), "Required status check") {
		t.Fatalf("Run = %v, want a merge error naming the check", err)
	}
	if pr := gh.prs[0]; pr["state"] != "open" || pr["merge_attempts"] != "x" {
		t.Fatalf("PR = %v, want it open after one attempt (a required check isn't retried)", pr)
	}
	if got, want := gh.labels(1), []string{"amf:done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "but could not merge it") || !strings.Contains(c[0], `Required status check "ci" is expected.`) {
		t.Fatalf("comments = %q", c)
	}
	if _, err := os.Stat(filepath.Join(root, ".amf", "worktrees", "issue-1")); !os.IsNotExist(err) {
		t.Fatalf("worktree should be removed once the PR exists, stat err = %v", err)
	}
}

// Right after a push GitHub can call a PR not mergeable while it works out
// whether it is; amf asks again, up to mergeAttempts times.
func TestMergeRetriesNotMergeable(t *testing.T) {
	for _, tc := range []struct {
		fails    int
		merged   bool
		attempts string
	}{
		{fails: 1, merged: true, attempts: "xx"},
		{fails: mergeAttempts, merged: false, attempts: strings.Repeat("x", mergeAttempts)},
	} {
		root, _ := setupRepo(t)
		gh, srv := newFakeGitHub(t)
		gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", State: "open"}
		for range tc.fails {
			gh.mergeErrs = append(gh.mergeErrs, fakeError{405, "Pull Request is not mergeable"})
		}
		r, _ := newTestRunner(t, srv, root, fakeClaude(t, `cat >/dev/null; echo hello > feature.txt; echo done`))
		r.Merge, r.MergeRetry = "squash", time.Millisecond

		err := r.Run(context.Background(), 1)
		if pr := gh.prs[0]; (pr["state"] == "merged") != tc.merged || pr["merge_attempts"] != tc.attempts {
			t.Fatalf("%d failures: PR = %v, want merged %v after %d attempts", tc.fails, pr, tc.merged, len(tc.attempts))
		}
		if (err == nil) != tc.merged {
			t.Fatalf("%d failures: Run = %v", tc.fails, err)
		}
	}
}

// --merge never merges a change to AGENTS.md: it declares the pipeline, so
// one issue's agent could otherwise pick what runs every later issue.
func TestMergeRefusesAgentsMD(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Change the pipeline", State: "open"}
	r, _ := newTestRunner(t, srv, root, fakeClaude(t, `cat >/dev/null; echo '# agents' > AGENTS.md; echo done`))
	r.Merge = "merge"

	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "edits AGENTS.md") {
		t.Fatalf("Run = %v, want the AGENTS.md refusal", err)
	}
	if pr := gh.prs[0]; pr["state"] != "open" || pr["merge_attempts"] != "" {
		t.Fatalf("PR = %v, want it open with no merge attempted", pr)
	}
	if got, want := gh.labels(1), []string{"amf:done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}

	// An AGENTS.md below the top level declares no pipeline, so it merges.
	gh.issues[2] = &Issue{Number: 2, Title: "Docs", State: "open"}
	r.Claude = fakeClaude(t, `cat >/dev/null; mkdir -p docs; echo '# agents' > docs/AGENTS.md; echo done`)
	if err := r.Run(context.Background(), 2); err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	if pr := gh.prs[1]; pr["state"] != "merged" {
		t.Fatalf("PR #2 = %v, want it merged", pr)
	}
}

// watch merges each PR it opens with --merge, and says so when it starts.
func TestWatchMerges(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "One", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	r, log := newTestRunner(t, srv, root, fakeClaude(t, `cat >/dev/null; date > out.txt; echo ok`))
	r.Merge = "rebase"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- r.Watch(ctx, "amf:ready", 50*time.Millisecond) }()
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Contains(gh.labels(1), "amf:done") {
		if time.Now().After(deadline) {
			t.Fatalf("issue 1 never processed; log:\n%s", log)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	gh.mu.Lock()
	defer gh.mu.Unlock()
	if pr := gh.prs[0]; pr["state"] != "merged" || pr["merge_method"] != "rebase" {
		t.Fatalf("PR = %v, want it rebased and merged", pr)
	}
	if !strings.Contains(log.String(), ", merging each PR without review (rebase)") {
		t.Fatalf("watch didn't say it merges:\n%s", log)
	}
}

// Only canonical amf branches from this repository identify an issue.
func TestPRListIssueBranches(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	for _, head := range []string{"amf/issue-0", "amf/issue--1", "amf/issue-01", "amf/issue-+1", "amf/issue-1/extra", "amf/issue-", "feature"} {
		gh.prs = append(gh.prs, map[string]string{"head": head, "base": "main", "state": "open"})
	}
	listJSONEnv(t, srv)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"pr", "list", "--json"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(gh.prs) {
		t.Fatalf("got %d rows", len(rows))
	}
	for _, row := range rows {
		if row["issue"] != nil {
			t.Errorf("unexpected issue: %v", row)
		}
	}
}

// GitHub can return creation-time ties out of number order and a deleted
// head repository as null. Neither should corrupt the public listing.
func TestPRListUnorderedAndDeletedHeads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/vic/demo" {
			writeJSON(w, 200, map[string]string{"default_branch": "main"})
			return
		}
		if q := r.URL.Query(); q.Get("state") != "open" || q.Get("sort") != "created" || q.Get("direction") != "desc" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		writeJSON(w, 200, []map[string]any{
			{"number": 1, "state": "open", "head": map[string]any{"ref": "amf/issue-12", "repo": nil}},
			{"number": 2, "state": "open", "head": map[string]any{"ref": "amf/issue-13", "repo": map[string]string{"full_name": "VIC/DEMO"}}},
		})
	}))
	t.Cleanup(srv.Close)
	listJSONEnv(t, srv)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"pr", "list", "--json"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Number int
		Issue  *int
	}
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Number != 2 || rows[0].Issue == nil || *rows[0].Issue != 13 || rows[1].Number != 1 || rows[1].Issue != nil {
		t.Fatalf("rows = %+v", rows)
	}
}
