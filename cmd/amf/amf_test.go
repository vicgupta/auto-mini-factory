package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseDotEnv(t *testing.T) {
	in := `
# comment
GITHUB_TOKEN=ghp_abc
export GITHUB_REPO = "vic/demo"
SINGLE='a b'
TRAILING=x # note
EMPTY=
`
	got, err := parseDotEnv(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"GITHUB_TOKEN": "ghp_abc",
		"GITHUB_REPO":  "vic/demo",
		"SINGLE":       "a b",
		"TRAILING":     "x",
		"EMPTY":        "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if _, err := parseDotEnv(strings.NewReader("NOEQUALS")); err == nil {
		t.Fatal("expected error for line without =")
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	os.WriteFile(env, []byte("GITHUB_TOKEN=from-file\nGITHUB_REPO=https://github.com/vic/demo.git\nCLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat-x\n"), 0o600)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_REPO", "")

	cfg, err := loadConfig(filepath.Join(dir, "missing"), env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "from-file" || cfg.Repo != "vic/demo" || cfg.Claude != "claude" || cfg.APIURL != "https://api.github.com" || cfg.ClaudeToken != "sk-ant-oat-x" {
		t.Fatalf("unexpected config %+v", cfg)
	}

	t.Setenv("GITHUB_TOKEN", "from-env")
	if cfg, _ := loadConfig(env); cfg.Token != "from-env" {
		t.Fatalf("environment should override .env, got %q", cfg.Token)
	}

	t.Setenv("GITHUB_REPO", "not-a-repo")
	if _, err := loadConfig(env); err == nil {
		t.Fatal("expected error for malformed GITHUB_REPO")
	}
}

func TestNormalizeRepo(t *testing.T) {
	for _, in := range []string{
		"vic/demo",
		"vic/demo/",
		"https://github.com/vic/demo",
		"https://github.com/vic/demo/",
		"https://github.com/vic/demo.git",
		"git@github.com:vic/demo.git",
	} {
		if got := normalizeRepo(in); got != "vic/demo" {
			t.Errorf("normalizeRepo(%q) = %q, want %q", in, got, "vic/demo")
		}
	}
}

func TestParseArgs(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	b := fs.String("b", "", "")
	pos, err := parseArgs(fs, []string{"Title here", "-b", "body"})
	if err != nil || *b != "body" || !reflect.DeepEqual(pos, []string{"Title here"}) {
		t.Fatalf("pos=%v b=%q err=%v", pos, *b, err)
	}
}

func TestParseClaudeResult(t *testing.T) {
	got, err := parseClaudeResult([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"Did it"}`))
	if err != nil || got != "Did it" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := parseClaudeResult([]byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"x"}`)); err == nil {
		t.Fatal("expected error result to fail")
	}
	if got, _ := parseClaudeResult([]byte("plain text\n")); got != "plain text" {
		t.Fatalf("plain text: got %q", got)
	}
}

func TestNeedsWork(t *testing.T) {
	open := Issue{State: "open", Labels: []Label{{Name: "amf"}}}
	if !needsWork(open) {
		t.Fatal("labeled open issue should need work")
	}
	for _, l := range statusLabels {
		is := open
		is.Labels = append(slices.Clone(open.Labels), Label{Name: l})
		if needsWork(is) {
			t.Fatalf("issue with %s should be skipped", l)
		}
	}
}

// fakeGitHub is an in-memory stand-in for the parts of the GitHub API amf uses.
type fakeGitHub struct {
	t        *testing.T
	mu       sync.Mutex
	issues   map[int]*Issue
	comments map[int][]string
	prs      []map[string]string
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *httptest.Server) {
	f := &fakeGitHub{t: t, issues: map[int]*Issue{}, comments: map[int][]string{}}
	mux := http.NewServeMux()
	const repo = "/repos/vic/demo"
	mux.HandleFunc("GET "+repo, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"default_branch": "main"})
	})
	mux.HandleFunc("POST "+repo+"/issues", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Title, Body string
			Labels      []string
		}
		decode(t, r, &in)
		f.mu.Lock()
		defer f.mu.Unlock()
		n := len(f.issues) + 1
		is := &Issue{Number: n, Title: in.Title, Body: in.Body, State: "open", HTMLURL: fmt.Sprintf("https://github.com/vic/demo/issues/%d", n)}
		for _, l := range in.Labels {
			is.Labels = append(is.Labels, Label{Name: l})
		}
		f.issues[n] = is
		writeJSON(w, 201, is)
	})
	mux.HandleFunc("GET "+repo+"/issues", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []Issue
		for n := 1; n <= len(f.issues); n++ {
			if is := f.issues[n]; is.State == "open" && is.HasLabel(r.URL.Query().Get("labels")) {
				out = append(out, *is)
			}
		}
		writeJSON(w, 200, out)
	})
	mux.HandleFunc("GET "+repo+"/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		f.withIssue(w, r, func(is *Issue) { writeJSON(w, 200, is) })
	})
	mux.HandleFunc("POST "+repo+"/issues/{n}/labels", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Labels []string }
		decode(t, r, &in)
		f.withIssue(w, r, func(is *Issue) {
			for _, l := range in.Labels {
				if !is.HasLabel(l) {
					is.Labels = append(is.Labels, Label{Name: l})
				}
			}
			writeJSON(w, 200, is.Labels)
		})
	})
	mux.HandleFunc("DELETE "+repo+"/issues/{n}/labels/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.withIssue(w, r, func(is *Issue) {
			name := r.PathValue("name")
			if !is.HasLabel(name) {
				writeJSON(w, 404, map[string]string{"message": "Label does not exist"})
				return
			}
			is.Labels = slices.DeleteFunc(is.Labels, func(l Label) bool { return l.Name == name })
			writeJSON(w, 200, is.Labels)
		})
	})
	mux.HandleFunc("POST "+repo+"/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Body string }
		decode(t, r, &in)
		f.withIssue(w, r, func(is *Issue) {
			f.comments[is.Number] = append(f.comments[is.Number], in.Body)
			writeJSON(w, 201, map[string]any{})
		})
	})
	mux.HandleFunc("POST "+repo+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		decode(t, r, &in)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.prs = append(f.prs, in)
		n := 100 + len(f.prs)
		writeJSON(w, 201, PullRequest{Number: n, HTMLURL: fmt.Sprintf("https://github.com/vic/demo/pull/%d", n)})
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			writeJSON(w, 401, map[string]string{"message": "Bad credentials"})
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeGitHub) withIssue(w http.ResponseWriter, r *http.Request, fn func(*Issue)) {
	n, _ := strconv.Atoi(r.PathValue("n"))
	f.mu.Lock()
	defer f.mu.Unlock()
	is, ok := f.issues[n]
	if !ok {
		writeJSON(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	fn(is)
}

func (f *fakeGitHub) labels(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, l := range f.issues[n].Labels {
		out = append(out, l.Name)
	}
	slices.Sort(out)
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func decode(t *testing.T, r *http.Request, v any) {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Errorf("decode request body: %v", err)
	}
}

// setupRepo creates a bare "origin" with one commit on main and a clone of it.
func setupRepo(t *testing.T) (root, origin string) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(cfg, []byte("[user]\n\tname = amf test\n\temail = amf@example.com\n[init]\n\tdefaultBranch = main\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	dir := t.TempDir()
	origin = filepath.Join(dir, "origin.git")
	root = filepath.Join(dir, "work")
	mustGit(t, dir, "init", "--bare", "-b", "main", origin)
	mustGit(t, dir, "clone", origin, root)
	os.WriteFile(filepath.Join(root, "README.md"), []byte("demo\n"), 0o644)
	mustGit(t, root, "add", ".")
	mustGit(t, root, "commit", "-m", "initial")
	mustGit(t, root, "push", "-u", "origin", "main")
	return root, origin
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// fakeClaude writes a shell script standing in for the claude CLI.
func fakeClaude(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestRunner(t *testing.T, srv *httptest.Server, root, claude string) (*Runner, *bytes.Buffer) {
	var log bytes.Buffer
	return &Runner{
		GH:      newGitHub(Config{Token: "test-token", Repo: "vic/demo", APIURL: srv.URL}),
		Repo:    "vic/demo",
		Root:    root,
		Claude:  claude,
		Timeout: time.Minute,
		Log:     &log,
	}, &log
}

func TestRunOpensPR(t *testing.T) {
	root, origin := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", Body: "Please add feature.txt", State: "open", Labels: []Label{{Name: "amf"}}}
	t.Setenv("GITHUB_TOKEN", "must-not-leak")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api-should-not-win")

	claude := fakeClaude(t, `
prompt=$(cat)
case "$prompt" in *"Add feature file"*"Please add feature.txt"*) ;; *) echo "bad prompt: $prompt" >&2; exit 4;; esac
case "$*" in *--dangerously-skip-permissions*) ;; *) echo "missing flag: $*" >&2; exit 5;; esac
if [ -n "$GITHUB_TOKEN" ]; then echo "token leaked" >&2; exit 3; fi
if [ "$CLAUDE_CODE_OAUTH_TOKEN" != "sk-ant-oat-test" ]; then echo "oauth token missing" >&2; exit 6; fi
if [ -n "$ANTHROPIC_API_KEY" ]; then echo "api key should be dropped" >&2; exit 7; fi
echo hello > feature.txt
echo '{"type":"result","subtype":"success","is_error":false,"result":"Added feature.txt.\n\nVerified with ls."}'
`)
	r, log := newTestRunner(t, srv, root, claude)
	r.ClaudeToken = "sk-ant-oat-test"

	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}

	if len(gh.prs) != 1 {
		t.Fatalf("expected one PR, got %v", gh.prs)
	}
	pr := gh.prs[0]
	if pr["head"] != "amf/issue-1" || pr["base"] != "main" || pr["title"] != "Fix #1: Add feature file" {
		t.Fatalf("unexpected PR %v", pr)
	}
	if !strings.Contains(pr["body"], "Added feature.txt.") || !strings.Contains(pr["body"], "Closes #1") {
		t.Fatalf("PR body missing summary or Closes: %q", pr["body"])
	}
	if got := mustGit(t, root, "--git-dir", origin, "show", "amf/issue-1:feature.txt"); got != "hello" {
		t.Fatalf("pushed branch has feature.txt = %q", got)
	}
	if got, want := gh.labels(1), []string{"amf", "amf:done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "/pull/101") {
		t.Fatalf("comments = %v", c)
	}
	if _, err := os.Stat(filepath.Join(root, ".amf", "worktrees", "issue-1")); !os.IsNotExist(err) {
		t.Fatalf("worktree should be removed after success, stat err = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".amf", "logs", "issue-1.log")); !bytes.Contains(b, []byte("Added feature.txt")) {
		t.Fatalf("log missing claude output: %q", b)
	}
}

func TestRunFailsWithoutChanges(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Do nothing", State: "open", Labels: []Label{{Name: "amf"}}}
	claude := fakeClaude(t, `cat >/dev/null; echo '{"type":"result","subtype":"success","is_error":false,"result":"nothing"}'`)
	r, _ := newTestRunner(t, srv, root, claude)

	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "without making any changes") {
		t.Fatalf("expected no-changes error, got %v", err)
	}
	if got, want := gh.labels(1), []string{"amf", "amf:failed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "without making any changes") {
		t.Fatalf("comments = %v", c)
	}
	if len(gh.prs) != 0 {
		t.Fatalf("no PR expected, got %v", gh.prs)
	}
	if _, err := os.Stat(filepath.Join(root, ".amf", "worktrees", "issue-1")); err != nil {
		t.Fatalf("worktree should be kept after failure: %v", err)
	}

	// A re-run starts over from a fresh worktree.
	r.Claude = fakeClaude(t, `cat >/dev/null; echo fixed > fix.txt; echo done`)
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if got, want := gh.labels(1), []string{"amf", "amf:done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labels after re-run = %v, want %v", got, want)
	}
}

func TestRunClaudeTimeout(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Slow", State: "open"}
	r, _ := newTestRunner(t, srv, root, fakeClaude(t, "exec sleep 30\n"))
	r.Timeout = 200 * time.Millisecond

	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func TestWatchProcessesLabeledIssues(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "One", State: "open", Labels: []Label{{Name: "amf"}}}
	gh.issues[2] = &Issue{Number: 2, Title: "Already done", State: "open", Labels: []Label{{Name: "amf"}, {Name: "amf:done"}}}
	gh.issues[3] = &Issue{Number: 3, Title: "Not for amf", State: "open"}
	claude := fakeClaude(t, `cat >/dev/null; date > out.txt; echo ok`)
	r, log := newTestRunner(t, srv, root, claude)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- r.Watch(ctx, "amf", 50*time.Millisecond) }()
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
	if len(gh.prs) != 1 || gh.prs[0]["head"] != "amf/issue-1" {
		t.Fatalf("expected exactly one PR for #1, got %v", gh.prs)
	}
}

func TestIssueCommand(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	t.Chdir(t.TempDir())
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"issue", "Add dark mode", "-b", "Use CSS variables"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "#1 https://github.com/vic/demo/issues/1") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	is := gh.issues[1]
	if is.Title != "Add dark mode" || is.Body != "Use CSS variables" || !is.HasLabel("amf") {
		t.Fatalf("unexpected issue %+v", is)
	}

	// Body from stdin, no label.
	stdout.Reset()
	err = run(context.Background(), []string{"issue", "--no-label", "Second"}, strings.NewReader("from stdin\n"), &stdout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if is := gh.issues[2]; is.Body != "from stdin\n" || len(is.Labels) != 0 {
		t.Fatalf("unexpected issue %+v", is)
	}
}

func TestExcludeAMFDir(t *testing.T) {
	root, _ := setupRepo(t)
	ctx := context.Background()
	for range 2 {
		if err := excludeAMFDir(ctx, root); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(root, ".git", "info", "exclude"))
	if strings.Count(string(b), "/.amf/") != 1 {
		t.Fatalf("exclude file:\n%s", b)
	}
	os.MkdirAll(filepath.Join(root, ".amf", "logs"), 0o755)
	os.WriteFile(filepath.Join(root, ".amf", "logs", "x.log"), []byte("x"), 0o644)
	if st := mustGit(t, root, "status", "--porcelain"); st != "" {
		t.Fatalf(".amf should be ignored, status = %q", st)
	}
}

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Println("skipping: git not installed")
		return
	}
	os.Exit(m.Run())
}
