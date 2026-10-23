//go:build live

package main

// Live tests: the real amf binary, real agents and a real GitHub repository.
// They create issues, branches and PRs in that repository, and close or
// delete them all at the end (set AMF_LIVE_KEEP=1 to keep them).
//
//	AMF_LIVE_DIR=/tmp/amf go test -tags live -run Live -v -timeout 90m ./cmd/amf
//
// AMF_LIVE_DIR holds the .env (GITHUB_TOKEN and GITHUB_REPO) and is where amf
// runs, so it clones the repo there or reuses that clone. Optional:
//
//	AMF_LIVE_GITHUB_TOKEN     use this token instead of the one in .env
//	AMF_LIVE_MODEL            Claude model for every claude run (default haiku)
//	AMF_LIVE_PIPELINE=1       also test pipelines; this commits an AGENTS.md
//	                          with an ## amf table to the default branch, and
//	                          removes it again with a second commit
//	AMF_LIVE_OPENCODE_MODEL   opencode model for the pipeline's code stage
//	                          (default opencode/deepseek-v4.1-flash-free)
//	AMF_LIVE_SKILLS           base URL of the skills (default: this repo's
//	                          skills/ at v0.2.0 on raw.githubusercontent.com)

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

type live struct {
	t      *testing.T
	dir    string // AMF_LIVE_DIR
	bin    string // the amf binary under test
	env    []string
	gh     *GitHub
	repo   string
	base   string // default branch
	id     string // marks everything this run creates
	model  string
	issues []int
	labels []string
}

func newLive(t *testing.T) *live {
	dir := os.Getenv("AMF_LIVE_DIR")
	if dir == "" {
		t.Skip("set AMF_LIVE_DIR to a directory with a .env to run the live tests")
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	vars, err := parseDotEnv(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	token := vars["GITHUB_TOKEN"]
	if v := os.Getenv("AMF_LIVE_GITHUB_TOKEN"); v != "" {
		token = v
	}
	repo := normalizeRepo(vars["GITHUB_REPO"])
	l := &live{
		t:     t,
		dir:   dir,
		bin:   filepath.Join(t.TempDir(), "amf"),
		gh:    newGitHub(Config{Token: token, Repo: repo, APIURL: "https://api.github.com"}),
		repo:  repo,
		id:    fmt.Sprintf("live-%d", time.Now().Unix()),
		model: envOr("AMF_LIVE_MODEL", "haiku"),
	}
	ctx := context.Background()
	if l.base, err = l.gh.DefaultBranch(ctx); err != nil {
		t.Fatalf("GitHub: %v (set AMF_LIVE_GITHUB_TOKEN if .env's token is stale)", err)
	}

	if out, err := exec.Command("go", "build", "-ldflags", "-X main.version=v0.0.0-live", "-o", l.bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	// Claude runs on a cheap model unless amf passes one (pipeline stages).
	claude := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(claude, []byte(`#!/bin/sh
case " $* " in *" --model "*) exec claude "$@";; esac
exec claude --model "$AMF_LIVE_MODEL" "$@"
`), 0o755)
	// TestMain points AMF_API_URL at nothing; amf must talk to GitHub here.
	l.env = append(withoutEnv(os.Environ(), "AMF_API_URL", "GITHUB_TOKEN", "GITHUB_REPO"), "AMF_CLAUDE="+claude, "AMF_LIVE_MODEL="+l.model)
	if v := os.Getenv("AMF_LIVE_GITHUB_TOKEN"); v != "" {
		l.env = append(l.env, "GITHUB_TOKEN="+v)
	}
	t.Cleanup(l.cleanup)
	return l
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// amf runs the binary in AMF_LIVE_DIR (or dir, if given) and returns its
// stdout, stderr and exit code.
func (l *live) amf(dir string, extraEnv []string, args ...string) (stdout, stderr string, code int) {
	l.t.Helper()
	if dir == "" {
		dir = l.dir
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if len(args) > 0 && args[0] == "run" {
		// The test's claude wrapper pins the model, and runs on claude's own
		// login, which amf doesn't pick without a key; name it.
		args = append(slices.Clone(args), "--agent", "claudecode")
	}
	cmd := exec.CommandContext(ctx, l.bin, args...)
	cmd.Dir = dir
	cmd.Env = append(append([]string{}, l.env...), extraEnv...)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	code = cmd.ProcessState.ExitCode()
	if err != nil && code < 0 {
		l.t.Fatalf("amf %v: %v", args, err)
	}
	l.t.Logf("amf %s -> exit %d\n%s%s", strings.Join(args, " "), code, o.String(), e.String())
	return o.String(), e.String(), code
}

// newIssue creates an issue with amf issue and returns its number.
func (l *live) newIssue(title, body string, flags ...string) int {
	l.t.Helper()
	out, errOut, code := l.amf("", nil, append([]string{"issue", "[" + l.id + "] " + title, "-b", body}, flags...)...)
	m := regexp.MustCompile(`^#(\d+) https://github\.com/\S+/issues/\d+\n$`).FindStringSubmatch(out)
	if code != 0 || m == nil {
		l.t.Fatalf("amf issue: exit %d, %q %q", code, out, errOut)
	}
	n, _ := strconv.Atoi(m[1])
	l.issues = append(l.issues, n)
	return n
}

func (l *live) get(path string, out any) {
	l.t.Helper()
	if err := l.gh.do(context.Background(), "GET", l.gh.repoPath("%s", path), nil, out); err != nil {
		l.t.Fatal(err)
	}
}

func (l *live) issue(n int) Issue {
	l.t.Helper()
	var is Issue
	l.get(fmt.Sprintf("/issues/%d", n), &is)
	return is
}

func (l *live) labelsOf(n int) []string {
	var names []string
	for _, lb := range l.issue(n).Labels {
		names = append(names, lb.Name)
	}
	return names
}

func (l *live) wantLabels(n int, want ...string) {
	l.t.Helper()
	got := l.labelsOf(n)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		l.t.Errorf("#%d labels = %v, want %v", n, got, want)
	}
}

func (l *live) comments(n int) []string {
	l.t.Helper()
	var cs []struct{ Body string }
	l.get(fmt.Sprintf("/issues/%d/comments?per_page=100", n), &cs)
	var out []string
	for _, c := range cs {
		out = append(out, c.Body)
	}
	return out
}

type livePR struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

// prsFor lists the PRs (any state) from issue n's branch.
func (l *live) prsFor(n int) []livePR {
	l.t.Helper()
	owner, _, _ := strings.Cut(l.repo, "/")
	q := url.Values{"head": {fmt.Sprintf("%s:amf/issue-%d", owner, n)}, "state": {"all"}}
	var prs []livePR
	l.get("/pulls?"+q.Encode(), &prs)
	return prs
}

type liveCommit struct {
	Commit struct {
		Message string `json:"message"`
		Author  struct{ Name, Email string }
	} `json:"commit"`
}

func (l *live) prCommits(pr int) []liveCommit {
	l.t.Helper()
	var cs []liveCommit
	l.get(fmt.Sprintf("/pulls/%d/commits", pr), &cs)
	return cs
}

// file is a file's content on a branch, or "" if it isn't there.
func (l *live) file(path, ref string) string {
	l.t.Helper()
	var f struct{ Content string }
	err := l.gh.do(context.Background(), "GET", l.gh.repoPath("/contents/%s?ref=%s", path, url.QueryEscape(ref)), nil, &f)
	if isStatus(err, 404) {
		return ""
	}
	if err != nil {
		l.t.Fatal(err)
	}
	b, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
	if err != nil {
		l.t.Fatal(err)
	}
	return string(b)
}

func (l *live) branchExists(branch string) bool {
	err := l.gh.do(context.Background(), "GET", l.gh.repoPath("/branches/%s", branch), nil, nil)
	return err == nil
}

func (l *live) addLabel(n int, label string) {
	l.t.Helper()
	if err := l.gh.AddLabels(context.Background(), n, label); err != nil {
		l.t.Fatal(err)
	}
}

func (l *live) cleanup() {
	if os.Getenv("AMF_LIVE_KEEP") != "" {
		l.t.Logf("AMF_LIVE_KEEP is set; leaving issues %v and their branches and PRs", l.issues)
		return
	}
	ctx := context.Background()
	closed := map[string]string{"state": "closed"}
	for _, n := range l.issues {
		for _, pr := range l.prsFor(n) {
			if pr.State == "open" {
				l.gh.do(ctx, "PATCH", l.gh.repoPath("/pulls/%d", pr.Number), closed, nil)
			}
		}
		if l.branchExists(fmt.Sprintf("amf/issue-%d", n)) {
			l.gh.do(ctx, "DELETE", l.gh.repoPath("/git/refs/heads/amf/issue-%d", n), nil, nil)
		}
		l.gh.do(ctx, "PATCH", l.gh.repoPath("/issues/%d", n), closed, nil)
	}
	for _, name := range l.labels {
		l.gh.do(ctx, "DELETE", l.gh.repoPath("/labels/%s", url.PathEscape(name)), nil, nil)
	}
}

// checkPR checks the PR amf opened for issue n and returns it.
func (l *live) checkPR(n int, title string) livePR {
	l.t.Helper()
	prs := l.prsFor(n)
	if len(prs) != 1 {
		l.t.Fatalf("#%d has %d PRs, want 1", n, len(prs))
	}
	pr := prs[0]
	if want := fmt.Sprintf("Fix #%d: [%s] %s", n, l.id, title); pr.Title != want || pr.Base.Ref != l.base || pr.State != "open" {
		l.t.Errorf("PR = %q on %s (%s), want %q on %s", pr.Title, pr.Base.Ref, pr.State, want, l.base)
	}
	if strings.Count(pr.Body, fmt.Sprintf("Closes #%d", n)) != 1 || !strings.Contains(pr.Body, "_Opened by amf._") {
		l.t.Errorf("PR body should close #%d once and end with the footer:\n%s", n, pr.Body)
	}
	for _, bad := range []string{"Generated with", "Co-Authored-By", "Co-authored-by", "VERDICT:"} {
		if strings.Contains(pr.Body, bad) {
			l.t.Errorf("PR body contains %q:\n%s", bad, pr.Body)
		}
	}
	// One commit, by the user, closing the issue.
	email, _ := git(context.Background(), filepath.Join(l.dir, repoName(l.repo)), "config", "user.email")
	commits := l.prCommits(pr.Number)
	if len(commits) != 1 {
		l.t.Fatalf("PR #%d has %d commits, want 1", pr.Number, len(commits))
	}
	c := commits[0].Commit
	if !strings.HasPrefix(c.Message, pr.Title+"\n\nCloses #"+strconv.Itoa(n)) || strings.Contains(c.Message, "Co-Authored-By") {
		l.t.Errorf("commit message = %q", c.Message)
	}
	if email != "" && c.Author.Email != email {
		l.t.Errorf("commit author = %s <%s>, want %s", c.Author.Name, c.Author.Email, email)
	}
	cs := l.comments(n)
	if len(cs) == 0 || !strings.Contains(cs[len(cs)-1], pr.HTMLURL) {
		l.t.Errorf("last comment on #%d should link %s: %q", n, pr.HTMLURL, cs)
	}
	return pr
}

func repoName(repo string) string {
	_, name, _ := strings.Cut(repo, "/")
	return name
}

func TestLive(t *testing.T) {
	l := newLive(t)
	clone := filepath.Join(l.dir, repoName(l.repo))
	t.Logf("run %s against %s (default branch %s), claude model %s", l.id, l.repo, l.base, l.model)

	t.Run("version", func(t *testing.T) {
		if out, _, code := l.amf("", nil, "version"); code != 0 || out != "amf v0.0.0-live\n" {
			t.Errorf("amf version = %q, exit %d", out, code)
		}
		if _, errOut, code := l.amf("", nil, "bogus"); code != 2 || !strings.Contains(errOut, `unknown command "bogus"`) {
			t.Errorf("amf bogus: exit %d, %q", code, errOut)
		}
	})

	t.Run("bad credentials", func(t *testing.T) {
		if _, errOut, code := l.amf("", []string{"GITHUB_TOKEN=github_pat_0000000000000000000000_invalid"}, "list"); code != 1 || !strings.Contains(errOut, "GitHub rejected GITHUB_TOKEN") {
			t.Errorf("bad token: exit %d, %q", code, errOut)
		}
		if _, errOut, code := l.amf("", []string{"GITHUB_REPO=" + l.repo + "-does-not-exist"}, "list"); code != 1 || !strings.Contains(errOut, "was not found on GitHub") {
			t.Errorf("bad repo: exit %d, %q", code, errOut)
		}
		if _, errOut, code := l.amf("", []string{"GITHUB_REPO=not-a-repo"}, "list"); code != 1 || !strings.Contains(errOut, "must look like owner/name") {
			t.Errorf("malformed repo: exit %d, %q", code, errOut)
		}
	})

	t.Run("init", func(t *testing.T) {
		// In a copy, so the real .env is never rewritten.
		dir := t.TempDir()
		raw, _ := os.ReadFile(filepath.Join(l.dir, ".env"))
		os.WriteFile(filepath.Join(dir, ".env"), raw, 0o600)
		out, _, code := l.amf(dir, nil, "init")
		if code != 0 {
			t.Fatalf("amf init: exit %d", code)
		}
		for _, name := range []string{labelReady, labelInProgress, labelDone, labelFailed} {
			if !regexp.MustCompile(`label ` + regexp.QuoteMeta(name) + ` +(exists|created) in ` + regexp.QuoteMeta(l.repo)).MatchString(out) {
				t.Errorf("init output missing label %s:\n%s", name, out)
			}
		}
		// A second run changes nothing.
		if out, _, _ := l.amf(dir, nil, "init"); strings.Contains(out, "backed up") || strings.Contains(out, "created in") {
			t.Errorf("second init:\n%s", out)
		}
	})

	t.Run("doctor", func(t *testing.T) {
		out, _, code := l.amf("", nil, "doctor")
		if code != 0 {
			t.Errorf("amf doctor: exit %d", code)
		}
		for _, want := range []string{"github", "repo", "labels", "checkout", "git push", "pipeline", "claudecode"} {
			if !regexp.MustCompile(`(?m)^ok +` + want + ` `).MatchString(out) {
				t.Errorf("doctor: %s isn't ok:\n%s", want, out)
			}
		}
	})

	t.Run("issue and list", func(t *testing.T) {
		n := l.newIssue("List check", "Not for amf to run.", "--no-label")
		if got := l.labelsOf(n); len(got) != 0 {
			t.Errorf("--no-label issue has labels %v", got)
		}
		// GitHub's issue list can lag a new issue by a few seconds.
		row := regexp.MustCompile(fmt.Sprintf(`(?m)^#%d +open +- +\[%s\] List check$`, n, l.id))
		for try := 0; ; try++ {
			out, _, code := l.amf("", nil, "list", "--state", "all")
			if code == 0 && row.MatchString(out) {
				break
			}
			if try == 5 {
				t.Errorf("amf list --state all doesn't show #%d:\n%s", n, out)
				break
			}
			time.Sleep(3 * time.Second)
		}
		if out, _, _ := l.amf("", nil, "list", "--label", labelReady); strings.Contains(out, fmt.Sprintf("#%d ", n)) {
			t.Errorf("--label amf:ready lists the unlabeled #%d:\n%s", n, out)
		}
		l.gh.do(context.Background(), "PATCH", l.gh.repoPath("/issues/%d", n), map[string]string{"state": "closed"}, nil)
		if _, errOut, code := l.amf("", nil, "run", strconv.Itoa(n)); code != 1 || !strings.Contains(errOut, "is closed") {
			t.Errorf("run on a closed issue: exit %d, %q", code, errOut)
		}
		if _, errOut, code := l.amf("", nil, "run", "999999"); code != 1 || !strings.Contains(errOut, "404") {
			t.Errorf("run on a missing issue: exit %d, %q", code, errOut)
		}
	})

	var first, firstPR int
	t.Run("run opens a PR", func(t *testing.T) {
		path := "live/" + l.id + "/hello.txt"
		first = l.newIssue("Add hello file", "Create the file `"+path+"` containing exactly one line: `hello from amf`. Change nothing else.")
		l.wantLabels(first, labelReady)
		_, errOut, code := l.amf("", nil, "run", strconv.Itoa(first), "--timeout", "15m")
		if code != 0 {
			t.Fatalf("amf run: exit %d\n%s", code, errOut)
		}
		resolved, _ := filepath.EvalSymlinks(clone) // /tmp is /private/tmp on macOS
		if !strings.Contains(errOut, "using the clone at "+resolved) && !strings.Contains(errOut, "cloning "+l.repo) {
			t.Errorf("amf didn't use or make the clone in %s:\n%s", l.dir, errOut)
		}
		l.wantLabels(first, labelDone)
		pr := l.checkPR(first, "Add hello file")
		firstPR = pr.Number
		if got := l.file(path, pr.Head.Ref); strings.TrimSpace(got) != "hello from amf" {
			t.Errorf("%s on the branch = %q", path, got)
		}
		if _, err := os.Stat(filepath.Join(clone, ".amf", "logs", fmt.Sprintf("issue-%d.log", first))); err != nil {
			t.Errorf("no log: %v", err)
		}
		if _, err := os.Stat(filepath.Join(clone, ".amf", "worktrees", fmt.Sprintf("issue-%d", first))); !os.IsNotExist(err) {
			t.Errorf("worktree left after success: %v", err)
		}
		if st, _ := git(context.Background(), clone, "status", "--porcelain"); st != "" {
			t.Errorf("the clone isn't clean:\n%s", st)
		}
	})

	t.Run("run on a PR", func(t *testing.T) {
		if firstPR == 0 {
			t.Skip("no PR")
		}
		if _, errOut, code := l.amf("", nil, "run", strconv.Itoa(firstPR)); code != 1 || !strings.Contains(errOut, "is a pull request") {
			t.Errorf("exit %d, %q", code, errOut)
		}
	})

	t.Run("retry updates the open PR", func(t *testing.T) {
		if firstPR == 0 {
			t.Skip("no PR")
		}
		l.addLabel(first, labelReady)
		_, errOut, code := l.amf("", nil, "run", "#"+strconv.Itoa(first), "--timeout", "15m")
		if code != 0 {
			t.Fatalf("amf run: exit %d\n%s", code, errOut)
		}
		l.wantLabels(first, labelDone)
		pr := l.checkPR(first, "Add hello file")
		if pr.Number != firstPR {
			t.Errorf("retry opened #%d instead of updating #%d", pr.Number, firstPR)
		}
		if cs := l.comments(first); !strings.Contains(cs[len(cs)-1], "amf updated "+pr.HTMLURL) {
			t.Errorf("last comment = %q", cs[len(cs)-1])
		}
	})

	t.Run("untrusted author", func(t *testing.T) {
		n := l.newIssue("Untrusted", "Create the file `live/"+l.id+"/untrusted.txt` containing exactly: no")
		_, errOut, code := l.amf("", []string{"AMF_TRUSTED=someone-else-" + l.id}, "run", strconv.Itoa(n))
		if code != 1 || !strings.Contains(errOut, "not in AMF_TRUSTED") || regexp.MustCompile(`: running (claude|codex|opencode) `).MatchString(errOut) {
			t.Errorf("exit %d:\n%s", code, errOut)
		}
		l.wantLabels(n, labelFailed)
		if cs := l.comments(n); len(cs) != 1 || !strings.Contains(cs[0], "AMF_TRUSTED") {
			t.Errorf("comments = %q", cs)
		}
		if l.branchExists(fmt.Sprintf("amf/issue-%d", n)) {
			t.Error("a branch was pushed")
		}
	})

	t.Run("no changes fails the issue", func(t *testing.T) {
		n := l.newIssue("Nothing to do", "This is a test of amf's failure handling. Do not create, edit or delete any files. Reply only with: No changes needed.")
		_, _, code := l.amf("", nil, "run", strconv.Itoa(n), "--timeout", "15m")
		if code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		l.wantLabels(n, labelFailed)
		if cs := l.comments(n); len(cs) != 1 || !strings.Contains(cs[0], "without making any changes") {
			t.Errorf("comments = %q", cs)
		}
		if len(l.prsFor(n)) != 0 {
			t.Error("a PR was opened")
		}
	})

	t.Run("watch", func(t *testing.T) {
		label := "amf-" + l.id
		l.labels = append(l.labels, label)
		a := l.newIssue("Watch A", "Create the file `live/"+l.id+"/a.txt` containing exactly: a", "--label", label)
		b := l.newIssue("Watch B", "Create the file `live/"+l.id+"/b.txt` containing exactly: b", "--label", label)

		cmd := exec.Command(l.bin, "watch", "--agent", "claudecode", "--interval", "5s", "--label", label, "--timeout", "15m")
		cmd.Dir, cmd.Env = l.dir, l.env
		var errOut bytes.Buffer
		cmd.Stdout, cmd.Stderr = &errOut, &errOut
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(30 * time.Minute)
		for !(contains(l.labelsOf(a), labelDone) && contains(l.labelsOf(b), labelDone)) {
			if time.Now().After(deadline) || contains(l.labelsOf(a), labelFailed) || contains(l.labelsOf(b), labelFailed) {
				cmd.Process.Kill()
				cmd.Wait()
				t.Fatalf("watch didn't finish both issues:\n%s", errOut.String())
			}
			time.Sleep(10 * time.Second)
		}
		cmd.Process.Signal(os.Interrupt)
		err := cmd.Wait()
		t.Logf("amf watch:\n%s", errOut.String())
		if err != nil || !strings.Contains(errOut.String(), "amf: stopped") {
			t.Errorf("watch after SIGINT: %v", err)
		}
		// Oldest first.
		if i, j := strings.Index(errOut.String(), fmt.Sprintf("#%d: [", a)), strings.Index(errOut.String(), fmt.Sprintf("#%d: [", b)); i < 0 || j < i {
			t.Errorf("watch didn't take #%d before #%d", a, b)
		}
		l.checkPR(a, "Watch A")
		l.checkPR(b, "Watch B")
	})

	if os.Getenv("AMF_LIVE_PIPELINE") == "" {
		t.Log("set AMF_LIVE_PIPELINE=1 to test pipelines")
		return
	}
	t.Run("pipeline", func(t *testing.T) { l.testPipeline(t, clone) })
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// testPipeline commits a pipeline to the default branch, runs issues through
// it, and removes it again.
func (l *live) testPipeline(t *testing.T, clone string) {
	ctx := context.Background()
	skills := envOr("AMF_LIVE_SKILLS", "https://raw.githubusercontent.com/vicgupta/auto-mini-factory/v0.2.0/skills")
	ocModel := envOr("AMF_LIVE_OPENCODE_MODEL", "opencode/deepseek-v4.1-flash-free")
	table := fmt.Sprintf("# Agents\n\n## amf\n\n| Stage | Skill | Agent | Model |\n|---|---|---|---|\n"+
		"| plan | %[1]s/plan/SKILL.md | claudecode | %[2]s |\n"+
		"| code | %[1]s/code/SKILL.md | opencode | %[3]s |\n"+
		"| eval | %[1]s/eval/SKILL.md | claudecode | %[2]s |\n", skills, l.model, ocModel)

	// Commit the table from a scratch clone of our own.
	work := filepath.Join(t.TempDir(), "work")
	cloneURL, _ := git(ctx, clone, "remote", "get-url", "origin")
	mustGitLive := func(dir string, args ...string) string {
		t.Helper()
		out, err := git(ctx, dir, args...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	mustGitLive(filepath.Dir(work), "clone", "-q", "--branch", l.base, cloneURL, work)
	if _, err := os.Stat(filepath.Join(work, "AGENTS.md")); err == nil {
		t.Fatalf("%s already has an AGENTS.md; not overwriting it", l.repo)
	}
	os.WriteFile(filepath.Join(work, "AGENTS.md"), []byte(table), 0o644)
	mustGitLive(work, "add", "AGENTS.md")
	mustGitLive(work, "commit", "-qm", "amf live test: add a pipeline")
	mustGitLive(work, "push", "-q", "origin", l.base)
	t.Cleanup(func() {
		if os.Getenv("AMF_LIVE_KEEP") != "" {
			return
		}
		git(ctx, work, "pull", "-q", "--rebase", "origin", l.base)
		git(ctx, work, "rm", "-q", "AGENTS.md")
		git(ctx, work, "commit", "-qm", "amf live test: remove the pipeline")
		if _, err := git(ctx, work, "push", "-q", "origin", l.base); err != nil {
			t.Errorf("could not remove AGENTS.md from %s: %v", l.base, err)
		}
	})

	t.Run("amf pipeline --fetch", func(t *testing.T) {
		out, _, code := l.amf(work, nil, "pipeline", "--fetch")
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
		for _, want := range []string{"agent claudecode  ok:", "agent opencode    ok:", "skill plan        ok:", "skill code        ok:", "skill eval        ok:"} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q", want)
			}
		}
	})

	t.Run("plan, code and eval", func(t *testing.T) {
		path := "live/" + l.id + "/pipeline.txt"
		n := l.newIssue("Pipeline file", "Create the file `"+path+"` containing exactly one line: `pipeline ok`. Change nothing else.")
		_, errOut, code := l.amf("", nil, "run", strconv.Itoa(n), "--timeout", "15m")
		if code != 0 {
			t.Fatalf("amf run: exit %d\n%s", code, errOut)
		}
		if !strings.Contains(errOut, "running the pipeline from AGENTS.md: plan (claudecode, "+l.model+") -> code (opencode, "+ocModel+") -> eval (claudecode, "+l.model+")") {
			t.Errorf("progress doesn't describe the pipeline:\n%s", errOut)
		}
		l.wantLabels(n, labelDone)
		pr := l.checkPR(n, "Pipeline file")
		for _, want := range []string{"## Evaluation", "**PASS** by claudecode (" + l.model + ")", "<summary>Plan (claudecode, " + l.model + ")</summary>"} {
			if !strings.Contains(pr.Body, want) {
				t.Errorf("PR body missing %q:\n%s", want, pr.Body)
			}
		}
		if got := l.file(path, pr.Head.Ref); strings.TrimSpace(got) != "pipeline ok" {
			t.Errorf("%s = %q", path, got)
		}
		for _, f := range []string{"runs/issue-%d/plan.md", "runs/issue-%d/eval-1.md", "logs/issue-%d-plan.log", "logs/issue-%d-code-1.log", "logs/issue-%d-eval-1.log", "opencode.json"} {
			if strings.Contains(f, "%d") {
				f = fmt.Sprintf(f, n)
			}
			if _, err := os.Stat(filepath.Join(clone, ".amf", f)); err != nil {
				t.Errorf("missing .amf/%s", f)
			}
		}
	})

	t.Run("blocked plan", func(t *testing.T) {
		n := l.newIssue("Blocked plan", "This issue tests amf's plan stage. Plan stage: do not plan. "+
			"Reply with exactly this one line and nothing else: BLOCKED: Which color should the button be?")
		_, _, code := l.amf("", nil, "run", strconv.Itoa(n), "--timeout", "15m")
		if code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		l.wantLabels(n, labelFailed)
		if cs := l.comments(n); len(cs) != 1 || !strings.Contains(cs[0], "needs answers") || !strings.Contains(strings.ToLower(cs[0]), "color") {
			t.Errorf("comments = %q", cs)
		}
		if l.branchExists(fmt.Sprintf("amf/issue-%d", n)) {
			t.Error("a blocked plan pushed a branch")
		}
	})

	t.Run("eval keeps failing", func(t *testing.T) {
		// A model won't reliably fail a good change on request, so eval is a
		// script here; plan and code are the real agents.
		claude := ""
		for _, kv := range l.env {
			if v, ok := strings.CutPrefix(kv, "AMF_CLAUDE="); ok {
				claude = v
			}
		}
		failing := filepath.Join(t.TempDir(), "claude")
		os.WriteFile(failing, []byte(`#!/bin/sh
prompt=$(cat)
case "$prompt" in *"You are the eval stage"*)
	printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"Test of the failure path.\n\nVERDICT: FAIL"}'
	exit 0;;
esac
printf '%s' "$prompt" | exec `+claude+` "$@"
`), 0o755)
		path := "live/" + l.id + "/fail.txt"
		n := l.newIssue("Eval failure", "Create the file `"+path+"` containing exactly: x")
		_, _, code := l.amf("", []string{"AMF_CLAUDE=" + failing}, "run", strconv.Itoa(n), "--timeout", "15m", "--eval-rounds", "0")
		if code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		l.wantLabels(n, labelFailed)
		branch := fmt.Sprintf("amf/issue-%d", n)
		if cs := l.comments(n); len(cs) != 1 || !strings.Contains(cs[0], "still found problems after 1 code round,") || !strings.Contains(cs[0], "`"+branch+"`") || !strings.Contains(cs[0], "Test of the failure path.") {
			t.Errorf("comments = %q", cs)
		}
		if !l.branchExists(branch) {
			t.Errorf("%s wasn't pushed for inspection", branch)
		} else if got := l.file(path, branch); strings.TrimSpace(got) != "x" {
			t.Errorf("%s on %s = %q", path, branch, got)
		}
		if len(l.prsFor(n)) != 0 {
			t.Error("a PR was opened")
		}
	})
}
