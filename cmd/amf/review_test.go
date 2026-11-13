package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseReviewGate(t *testing.T) {
	for _, tc := range []struct {
		value string
		on    bool
		err   string
	}{
		{"", false, ""},
		{"off", false, ""},
		{"on", true, ""},
		{" ON ", true, ""},
		{"yes", false, `AMF_REVIEW_GATE must be on or off, got "yes"`},
	} {
		on, err := parseReviewGate(tc.value)
		if on != tc.on || (tc.err == "" && err != nil) || (tc.err != "" && (err == nil || err.Error() != tc.err)) {
			t.Errorf("parseReviewGate(%q) = %v, %v; want %v, %q", tc.value, on, err, tc.on, tc.err)
		}
	}
}

func TestReviewCommandIsNotAPlaceholder(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	os.WriteFile(env, []byte("GITHUB_TOKEN=test-token\nGITHUB_REPO=vic/demo\nAMF_REVIEW_COMMAND=go test ./...\n"), 0o600)
	t.Setenv("AMF_REVIEW_COMMAND", "")
	cfg, err := loadConfig(env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReviewCommand != "go test ./..." {
		t.Fatalf("AMF_REVIEW_COMMAND = %q, want go test ./...", cfg.ReviewCommand)
	}
}

// reviewGateClaude fakes claude making the change, then reviewing it with
// review as its final message. When edit is true, the reviewer changes a file.
func reviewGateClaude(t *testing.T, review string, edit bool) string {
	t.Helper()
	reviewStep := "result '" + review + "'\n"
	if edit {
		reviewStep = "echo changed >> feature.txt\n" + reviewStep
	}
	return fakeAgent(t, "claude", shResult+`prompt=$(cat)
case "$prompt" in
*"amf-review: pass"*)
`+reviewStep+`;;
*)
echo hello > feature.txt
result 'Added feature.txt.'
;;
esac
`)
}

func TestReviewGateOffIsUnchanged(t *testing.T) {
	root, origin := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", Body: "Please add feature.txt", State: "open", Labels: []Label{{Name: "amf:ready"}}}

	// The review prompt must never reach this agent: the gate is off.
	claude := fakeAgent(t, "claude", shResult+`prompt=$(cat)
case "$prompt" in
*"amf-review: pass"*)
echo "review agent ran with the gate off" >&2
exit 9
;;
esac
echo hello > feature.txt
result 'Added feature.txt.'
`)
	r, log := newTestRunner(t, srv, root, claude)
	if r.ReviewGate {
		t.Fatal("the review gate should default to off")
	}

	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	if len(gh.prs) != 1 {
		t.Fatalf("expected one PR, got %v", gh.prs)
	}
	body := gh.prs[0]["body"]
	if strings.Contains(body, "## Review gate") || strings.Contains(body, "AMF_REVIEW_COMMAND") {
		t.Fatalf("gate off added a review section: %q", body)
	}
	if got := mustGit(t, root, "--git-dir", origin, "show", "amf/issue-1:feature.txt"); got != "hello" {
		t.Fatalf("pushed branch has feature.txt = %q", got)
	}
}

func TestReviewGatePass(t *testing.T) {
	root, origin := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", Body: "Please add feature.txt", State: "open", Labels: []Label{{Name: "amf:ready"}}}

	r, log := newTestRunner(t, srv, root, reviewGateClaude(t, "Looks good.\\n\\namf-review: pass", false))
	r.ReviewGate = true
	r.ReviewCommand = "test -f feature.txt && printf 'ok\\n'"

	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	if len(gh.prs) != 1 {
		t.Fatalf("expected one PR, got %v", gh.prs)
	}
	body := gh.prs[0]["body"]
	for _, want := range []string{
		"## Review gate",
		"_amf ran:_ `test -f feature.txt && printf 'ok\\n'` (exit 0 after ",
		"_Reviewer's notes (written by the agent; amf has not verified them):_",
		"Looks good.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("PR body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "amf-review: pass") {
		t.Errorf("PR body kept the verdict line:\n%s", body)
	}
	if got := mustGit(t, root, "--git-dir", origin, "show", "amf/issue-1:feature.txt"); got != "hello" {
		t.Fatalf("pushed branch has feature.txt = %q", got)
	}
	if b, err := os.ReadFile(filepath.Join(root, ".amf", "logs", "issue-1-review.log")); err != nil || !strings.Contains(string(b), "Looks good.") {
		t.Fatalf("review log = %q, %v", b, err)
	}
}

func TestReviewGateBlocks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		review  string
		edit    bool
		want    string
	}{
		{"verdict block", "true", `Security issue.\n\namf-review: block`, false, "amf-review: block"},
		{"missing verdict", "true", `Looks fine.`, false, "gave no amf-review verdict"},
		{"failing command", "exit 7", `Looks good.\n\namf-review: pass`, false, "test command failed"},
		{"reviewer edits a file", "true", `Looks good.\n\namf-review: pass`, true, "worktree changed during review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, origin := setupRepo(t)
			gh, srv := newFakeGitHub(t)
			gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", Body: "Please add feature.txt", State: "open", Labels: []Label{{Name: "amf:ready"}}}

			r, log := newTestRunner(t, srv, root, reviewGateClaude(t, tc.review, tc.edit))
			r.ReviewGate = true
			r.ReviewCommand = tc.command

			err := r.Run(context.Background(), 1)
			if err == nil || !strings.Contains(err.Error(), "review gate blocked") {
				t.Fatalf("Run = %v, want a review gate block\nlog:\n%s", err, log)
			}
			if len(gh.prs) != 0 || len(gh.creates) != 0 {
				t.Fatalf("gate block created a PR: %v %v", gh.prs, gh.creates)
			}
			if _, err := git(context.Background(), root, "--git-dir", origin, "rev-parse", "--verify", "refs/heads/amf/issue-1"); err == nil {
				t.Fatal("gate block pushed amf/issue-1")
			}
			if got, want := gh.labels(1), []string{"amf:failed"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("labels = %v, want %v", got, want)
			}
			comments := gh.comments[1]
			if len(comments) != 1 || !strings.Contains(comments[0], "review gate blocked") || !strings.Contains(comments[0], tc.want) ||
				!strings.Contains(comments[0], "amf/issue-1") || !strings.Contains(comments[0], ".amf/worktrees/issue-1") {
				t.Fatalf("comments = %v", comments)
			}
			if got := mustGit(t, root, "show", "amf/issue-1:feature.txt"); got != "hello" {
				t.Fatalf("local branch lost the commit: feature.txt = %q", got)
			}
			if _, err := os.Stat(filepath.Join(root, ".amf", "worktrees", "issue-1")); err != nil {
				t.Fatalf("worktree should be kept after a block: %v", err)
			}
		})
	}
}

func TestDoctorReviewGate(t *testing.T) {
	withGitUser(t)
	_, srv := newFakeGitHub(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("AMF_API_URL", srv.URL)
	t.Setenv("AMF_RELEASES_URL", "http://127.0.0.1:1")
	for _, tc := range []struct{ env, want string }{
		{"", `ok +review gate +off, the default`},
		{"AMF_REVIEW_GATE=on\nAMF_REVIEW_COMMAND=go test ./cmd/amf\n", `ok +review gate +on; AMF_REVIEW_COMMAND is set`},
		{"AMF_REVIEW_GATE=maybe\n", `FAIL +review gate +AMF_REVIEW_GATE must be on or off`},
	} {
		os.WriteFile(filepath.Join(dir, ".env"), []byte("GITHUB_TOKEN=test-token\nGITHUB_REPO=vic/demo\n"+tc.env), 0o600)
		out, _ := doctorOutput(t)
		wantLines(t, out, `ok +\.env +`, tc.want)
	}
}
