package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// staleBranch pushes amf/issue-1 adding a.txt, then moves main on: with
// conflict, main adds a different a.txt; without, it adds b.txt.
func staleBranch(t *testing.T, conflict bool) (root, origin string) {
	t.Helper()
	root, origin = setupRepo(t)
	mustGit(t, root, "checkout", "-q", "-b", "amf/issue-1")
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("branch\n"), 0o644)
	mustGit(t, root, "add", ".")
	mustGit(t, root, "commit", "-q", "-m", "branch change")
	mustGit(t, root, "push", "-q", "origin", "amf/issue-1")
	mustGit(t, root, "checkout", "-q", "main")
	name := "b.txt"
	if conflict {
		name = "a.txt"
	}
	os.WriteFile(filepath.Join(root, name), []byte("main\n"), 0o644)
	mustGit(t, root, "add", ".")
	mustGit(t, root, "commit", "-q", "-m", "main change")
	mustGit(t, root, "push", "-q", "origin", "main")
	return root, origin
}

func originFile(t *testing.T, origin, rev, path string) string {
	t.Helper()
	out, err := git(context.Background(), origin, "show", rev+":"+path)
	if err != nil {
		return ""
	}
	return out
}

func TestRetryMergesBaseIntoStalePR(t *testing.T) {
	root, origin := staleBranch(t, false)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add a file")
	gh.prs = []map[string]string{{"head": "amf/issue-1", "base": "main", "title": "old", "body": "old", "state": "open"}}
	gh.drafts = []bool{false}
	gh.compares["main...amf/issue-1"] = [2]int{1, 1}
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", "echo agent ran >&2; exit 1\n"))
	r.Force = true
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	if originFile(t, origin, "amf/issue-1", "a.txt") != "branch" || originFile(t, origin, "amf/issue-1", "b.txt") != "main" {
		t.Fatalf("branch not merged; log:\n%s", log)
	}
	// The merge only added to the branch: the old commit is an ancestor.
	if _, err := git(context.Background(), origin, "merge-base", "--is-ancestor", "main", "amf/issue-1"); err != nil {
		t.Fatalf("main is not in the branch: %v", err)
	}
	if got := gh.labels(1); !reflect.DeepEqual(got, []string{labelPROpen}) {
		t.Errorf("labels = %v", got)
	}
	if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "amf updated") {
		t.Errorf("comments = %q", c)
	}
	if strings.Contains(log.String(), "running claude") {
		t.Errorf("an agent ran for a clean merge:\n%s", log)
	}
}

func TestRetryResolvesConflictsWithAgent(t *testing.T) {
	root, origin := staleBranch(t, true)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add a file")
	gh.prs = []map[string]string{{"head": "amf/issue-1", "base": "main", "state": "open"}}
	gh.drafts = []bool{false}
	gh.compares["main...amf/issue-1"] = [2]int{1, 1}
	prompt := filepath.Join(t.TempDir(), "prompt")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", shResult+"cat > "+prompt+"\nprintf 'branch\\nmain\\n' > a.txt\nresult 'Kept both.'\n"))
	r.Force = true
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	if got := originFile(t, origin, "amf/issue-1", "a.txt"); got != "branch\nmain" {
		t.Fatalf("a.txt = %q\nlog:\n%s", got, log)
	}
	p, _ := os.ReadFile(prompt)
	if !strings.Contains(string(p), "- `a.txt`") || strings.Contains(string(p), "README") {
		t.Errorf("prompt should list only the conflicted file:\n%s", p)
	}
}

func TestStaleConflictWithoutAgentCommentsOnPR(t *testing.T) {
	root, origin := staleBranch(t, true)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add a file")
	gh.prs = []map[string]string{{"head": "amf/issue-1", "base": "main", "state": "open"}}
	gh.drafts = []bool{false}
	gh.compares["main...amf/issue-1"] = [2]int{1, 1}
	gh.issues[101] = &Issue{Number: 101, State: "open"} // the PR's conversation
	before := originFile(t, origin, "amf/issue-1", "a.txt")
	r, _ := newTestRunner(t, srv, root, "")
	_, handled, err := r.refreshStale(context.Background(), *gh.issues[1], "main", nil, runPlan{})
	if !handled || err == nil || !strings.Contains(err.Error(), "no usable agent") {
		t.Fatalf("handled=%t err=%v", handled, err)
	}
	if c := gh.comments[101]; len(c) != 1 || !strings.Contains(c[0], "`a.txt`") {
		t.Fatalf("PR comments = %q", c)
	}
	if originFile(t, origin, "amf/issue-1", "a.txt") != before {
		t.Fatal("branch changed")
	}
}

func TestUpToDatePRIsRebuilt(t *testing.T) {
	root, _ := staleBranch(t, false)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add a file")
	gh.prs = []map[string]string{{"head": "amf/issue-1", "base": "main", "state": "open"}}
	gh.drafts = []bool{false}
	gh.compares["main...amf/issue-1"] = [2]int{1, 0}
	r, _ := newTestRunner(t, srv, root, "")
	if _, handled, err := r.refreshStale(context.Background(), *gh.issues[1], "main", nil, runPlan{}); handled || err != nil {
		t.Fatalf("handled=%t err=%v", handled, err)
	}
}
