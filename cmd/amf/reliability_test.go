package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestGuardSkipsIssueWithOpenPR(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	gh.prs = append(gh.prs, map[string]string{"head": "amf/issue-1", "base": "main", "state": "open", "title": "Fix #1"})
	mustGit(t, root, "push", "origin", "main:refs/heads/amf/issue-1")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", "echo must not run >&2; exit 9"))
	err := r.Run(context.Background(), 1)
	var skip *skipError
	if !errors.As(err, &skip) || !strings.Contains(err.Error(), "already has an open pull request") {
		t.Fatalf("Run = %v, want a skip\nlog:\n%s", err, log)
	}
	if got := gh.labels(1); !slices.Equal(got, []string{"amf:pr-open"}) {
		t.Errorf("labels = %v, want amf:pr-open", got)
	}
	if len(gh.creates) != 0 || len(gh.comments[1]) != 0 {
		t.Errorf("creates = %v, comments = %v", gh.creates, gh.comments[1])
	}
}

func TestGuardSkipsExistingBranch(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	pushUnmergedBranch(t, root, "amf/issue-1")
	r, _ := newTestRunner(t, srv, root, fakeAgent(t, "claude", "echo must not run >&2; exit 9"))
	err := r.Run(context.Background(), 1)
	var skip *skipError
	if !errors.As(err, &skip) || !strings.Contains(err.Error(), "already exists on origin") {
		t.Fatalf("Run = %v, want a branch_exists skip", err)
	}
	if got := gh.labels(1); !slices.Equal(got, []string{"amf:ready"}) {
		t.Errorf("labels = %v, want them untouched", got)
	}
}

func TestGuardAllowsFailedRetryAndForce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []Label
		force  bool
	}{
		{"failed retry", []Label{{Name: "amf:ready"}, {Name: "amf:failed"}}, false},
		{"force", []Label{{Name: "amf:ready"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := setupRepo(t)
			gh, srv := newFakeGitHub(t)
			gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", State: "open", Labels: tc.labels}
			mustGit(t, root, "push", "origin", "main:refs/heads/amf/issue-1")
			r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", cleanClaude))
			r.Force = tc.force
			if err := r.Run(context.Background(), 1); err != nil {
				t.Fatalf("Run: %v\nlog:\n%s", err, log)
			}
			if got := gh.labels(1); !slices.Equal(got, []string{"amf:pr-open"}) {
				t.Errorf("labels = %v, want amf:pr-open", got)
			}
		})
	}
}

func TestDoneOnlyAfterMerge(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", cleanClaude))
	r.Merge = "squash"
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	if got := gh.labels(1); !slices.Equal(got, []string{"amf:done"}) {
		t.Errorf("labels = %v, want amf:done after the merge", got)
	}
}

func TestLockExcludesSecondAmf(t *testing.T) {
	root := t.TempDir()
	l, err := acquireLock(root, "watch")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLock(root, "run"); err == nil || !strings.Contains(err.Error(), "another amf (watch, pid") {
		t.Fatalf("second acquire = %v, want a clear error", err)
	}
	l.release()
	if _, err := os.Stat(lockPath(root)); !os.IsNotExist(err) {
		t.Fatalf("lock still there after release: %v", err)
	}
	l2, err := acquireLock(root, "run")
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	l2.release()
}

func TestLockReclaimsStale(t *testing.T) {
	host, _ := os.Hostname()
	t.Run("dead pid", func(t *testing.T) {
		root := t.TempDir()
		os.MkdirAll(filepath.Join(root, ".amf"), 0o755)
		// PID 1<<22 + 1 is above Linux's pid_max, so nothing runs as it.
		os.WriteFile(lockPath(root), []byte(`{"pid":4194305,"host":"`+host+`","command":"watch"}`), 0o644)
		l, err := acquireLock(root, "watch")
		if err != nil {
			t.Fatalf("acquire over a dead pid: %v", err)
		}
		l.release()
	})
	t.Run("old mtime", func(t *testing.T) {
		root := t.TempDir()
		os.MkdirAll(filepath.Join(root, ".amf"), 0o755)
		os.WriteFile(lockPath(root), []byte(`{"pid":1,"host":"elsewhere","command":"watch"}`), 0o644)
		if _, err := acquireLock(root, "watch"); err == nil {
			t.Fatal("a fresh lock from another host was taken over")
		}
		old := time.Now().Add(-lockStaleAfter - time.Minute)
		os.Chtimes(lockPath(root), old, old)
		l, err := acquireLock(root, "watch")
		if err != nil {
			t.Fatalf("acquire over an old lock: %v", err)
		}
		l.release()
	})
}

func TestParseStaleTimeout(t *testing.T) {
	for _, tc := range []struct {
		v     string
		agent time.Duration
		want  time.Duration
		bad   bool
	}{
		{"", time.Hour, 2 * time.Hour, false},
		{"", 3 * time.Hour, 6 * time.Hour, false},
		{"0", time.Hour, 0, false},
		{"off", time.Hour, 0, false},
		{"90m", time.Hour, 90 * time.Minute, false},
		{"10s", time.Hour, 0, true},
		{"soon", time.Hour, 0, true},
	} {
		got, err := parseStaleTimeout(tc.v, "AMF_STALE_TIMEOUT", tc.agent)
		if (err != nil) != tc.bad || got != tc.want {
			t.Errorf("%q = %v, %v; want %v, bad %v", tc.v, got, err, tc.want, tc.bad)
		}
	}
}

func TestSweepStale(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	old := time.Now().Add(-5 * time.Hour)
	inProgress := func() []Label { return []Label{{Name: "amf:in-progress"}} }
	gh.issues[1] = &Issue{Number: 1, Title: "old", State: "open", Labels: inProgress(), UpdatedAt: old}
	gh.issues[2] = &Issue{Number: 2, Title: "recently labeled", State: "open", Labels: inProgress(), UpdatedAt: old}
	gh.issues[3] = &Issue{Number: 3, Title: "has a PR", State: "open", Labels: inProgress(), UpdatedAt: old}
	gh.issues[4] = &Issue{Number: 4, Title: "being worked on", State: "open", Labels: inProgress(), UpdatedAt: old}
	gh.events = map[int][]IssueEvent{2: {{Event: "labeled", CreatedAt: time.Now().Add(-time.Minute)}}}
	gh.events[2][0].Label.Name = "amf:in-progress"
	gh.prs = append(gh.prs, map[string]string{"head": "amf/issue-3", "base": "main", "state": "open"})
	r, log := newTestRunner(t, srv, root, "claude")
	r.StaleTimeout = 2 * time.Hour
	r.working = 4
	r.sweepStale(context.Background())
	want := map[int]string{1: "amf:ready", 2: "amf:in-progress", 3: "amf:pr-open", 4: "amf:in-progress"}
	for n, l := range want {
		if got := gh.labels(n); !slices.Equal(got, []string{l}) {
			t.Errorf("#%d labels = %v, want %s\nlog:\n%s", n, got, l, log)
		}
	}
	if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "put it back to `amf:ready`") {
		t.Errorf("#1 comments = %q", c)
	}
	r.StaleTimeout = 0
	gh.issues[5] = &Issue{Number: 5, State: "open", Labels: inProgress(), UpdatedAt: old}
	r.sweepStale(context.Background())
	if got := gh.labels(5); !slices.Equal(got, []string{"amf:in-progress"}) {
		t.Errorf("disabled sweep changed #5: %v", got)
	}
}

func TestFailureCommentReportsAttempts(t *testing.T) {
	secret := "ghp_" + strings.Repeat("A1b2", 9)
	gh, _, err, _, _ := fallbackRun(t, "cat >/dev/null; echo codex broke; exit 3", "cat >/dev/null; echo token "+secret+"; echo '```'; exit 4")
	if err == nil {
		t.Fatal("Run succeeded")
	}
	c := gh.comments[1]
	if len(c) != 1 {
		t.Fatalf("comments = %q", c)
	}
	for _, want := range []string{"1. `codex`: exit code 3 after", "2. `claude`: exit code 4 after", "Fallback: ran", "issue-1-claude.log", "````text"} {
		if !strings.Contains(c[0], want) {
			t.Errorf("comment lacks %q:\n%s", want, c[0])
		}
	}
	if strings.Contains(c[0], secret) {
		t.Errorf("comment leaks the token:\n%s", c[0])
	}
	if got := gh.labels(1); !slices.Equal(got, []string{"amf:failed"}) {
		t.Errorf("labels = %v", got)
	}
}

func TestNoFallbackReasonWhenSecondaryUnusable(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	r, _ := newTestRunner(t, srv, root, fakeAgent(t, "claude", "exit 9"))
	r.Codex = fakeAgent(t, "codex", "cat >/dev/null; exit 3")
	r.OpenAIKey = "sk-openai-test"
	r.AnthropicKey = ""
	r.Primary, r.Secondary = agentCodex, agentClaude
	plan, err := r.agentPlan()
	if err != nil || strings.Join(plan.Agents, ",") != "codex" || !strings.Contains(plan.NoFallback, "secondary agent claude is not usable") {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	if err := r.Run(context.Background(), 1); err == nil {
		t.Fatal("Run succeeded")
	}
	if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "Fallback: did not run, because the secondary agent claude is not usable") {
		t.Errorf("comments = %q", c)
	}
}

func TestFailedRetryKeepsPROpen(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", State: "open", Labels: []Label{{Name: "amf:ready"}, {Name: "amf:pr-open"}}}
	gh.prs = append(gh.prs, map[string]string{"head": "amf/issue-1", "base": "main", "state": "open"})
	r, _ := newTestRunner(t, srv, root, fakeAgent(t, "claude", "cat >/dev/null; exit 3"))
	r.Force = true
	if err := r.Run(context.Background(), 1); err == nil {
		t.Fatal("Run succeeded")
	}
	if got := gh.labels(1); !slices.Equal(got, []string{"amf:pr-open"}) {
		t.Errorf("labels = %v, want amf:pr-open kept", got)
	}
}

func TestSweepFailed(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	failed := func(extra ...string) []Label {
		ls := []Label{{Name: labelFailed}}
		for _, e := range extra {
			ls = append(ls, Label{Name: e})
		}
		return ls
	}
	gh.issues[1] = &Issue{Number: 1, State: "open", Labels: failed()} // open, green PR
	gh.issues[2] = &Issue{Number: 2, State: "open", Labels: failed()} // no PR
	gh.issues[3] = &Issue{Number: 3, State: "open", Labels: failed(labelReady)}
	gh.prs = []map[string]string{
		{"head": "amf/issue-1", "base": "main", "state": "open"},
		{"head": "amf/issue-3", "base": "main", "state": "open"},
	}
	gh.checkRuns["abc123"] = []checkRun{{Name: "build", Status: "completed", Conclusion: "success"}}
	r, log := newTestRunner(t, srv, root, "claude")
	r.sweepFailed(context.Background())
	want := map[int][]string{1: {labelPROpen}, 2: {labelFailed}, 3: {labelFailed, labelReady}}
	for n, l := range want {
		if got := gh.labels(n); !slices.Equal(got, l) {
			t.Errorf("#%d labels = %v, want %v\nlog:\n%s", n, got, l, log)
		}
	}

	// Failing checks leave amf:failed in place.
	gh.issues[1].Labels = failed()
	gh.checkRuns["abc123"][0].Conclusion = "failure"
	r.sweepFailed(context.Background())
	if got := gh.labels(1); !slices.Equal(got, []string{labelFailed}) {
		t.Errorf("#1 with failing checks = %v", got)
	}
}

func TestFailureKeepsDoneOnlyWhenMerged(t *testing.T) {
	for _, merged := range []bool{true, false} {
		t.Run(fmt.Sprint("merged=", merged), func(t *testing.T) {
			root, _ := setupRepo(t)
			gh, srv := newFakeGitHub(t)
			gh.issues[7] = &Issue{Number: 7, Title: "again", State: "open", Labels: []Label{{Name: labelDone}, {Name: labelReady}}}
			if merged {
				gh.prs = []map[string]string{{"head": "amf/issue-7", "base": "main", "state": "merged"}}
			}
			r, _ := newTestRunner(t, srv, root, fakeClaude(t, "exit 1\n"))
			r.Force = true
			if err := r.Run(context.Background(), 7); err == nil {
				t.Fatal("want a failure")
			}
			want := labelFailed
			if merged {
				want = labelDone
			}
			if got := gh.labels(7); !slices.Equal(got, []string{want}) {
				t.Errorf("labels = %v, want %s", got, want)
			}
		})
	}
}
