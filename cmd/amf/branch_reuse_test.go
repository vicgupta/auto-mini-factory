package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mergedBranch makes amf/issue-1 and merges it into main with a merge
// commit, as an earlier slice PR would leave it.
func mergedBranch(t *testing.T, root string) string {
	t.Helper()
	mustGit(t, root, "checkout", "-q", "-b", "amf/issue-1")
	if err := os.WriteFile(filepath.Join(root, "slice.txt"), []byte("first slice\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, root, "add", "slice.txt")
	mustGit(t, root, "commit", "-qm", "first slice")
	mustGit(t, root, "push", "-q", "origin", "amf/issue-1")
	mustGit(t, root, "checkout", "-q", "main")
	mustGit(t, root, "merge", "--no-ff", "-qm", "Merge slice 1", "amf/issue-1")
	mustGit(t, root, "push", "-q", "origin", "main")
	return mustGit(t, root, "rev-parse", "amf/issue-1")
}

// pushCommand is the git push event's command line for branch.
func pushCommand(t *testing.T, ev map[string][]map[string]any, branch string) string {
	t.Helper()
	for _, e := range ev["git.exec"] {
		if e["subcommand"] != "push" {
			continue
		}
		args, _ := e["args"].([]any)
		if len(args) > 0 && args[len(args)-1] == branch {
			return fmt.Sprint(args)
		}
	}
	t.Fatalf("no push of %s in %v", branch, ev["git.exec"])
	return ""
}

func TestGuardReusesBranchMergedByAncestry(t *testing.T) {
	root, origin := setupRepo(t)
	old := mergedBranch(t, root)
	mustGit(t, root, "branch", "-q", "-D", "amf/issue-1")

	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", cleanClaude))
	var events bytes.Buffer
	r.Events = newEventLogger(r.Log, slog.LevelInfo, &events, slog.LevelDebug, nil, "run")
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	ev := eventsByName(eventLines(t, events.String()))
	wantEvent(t, ev, "issue.reuse", map[string]any{"issue": float64(1), "branch": "amf/issue-1", "sha": old, "reason": "ancestor"})
	if len(ev["issue.skip"]) != 0 {
		t.Errorf("reuse logged issue.skip: %v", ev["issue.skip"])
	}
	wantEvent(t, ev, "git.push", map[string]any{"branch": "amf/issue-1", "forced": false, "exit_code": float64(0)})
	if cmd := pushCommand(t, ev, "amf/issue-1"); strings.Contains(cmd, "--force") {
		t.Errorf("push command = %q, want a plain push", cmd)
	}
	if got := mustGit(t, root, "--git-dir", origin, "for-each-ref", "--format=%(refname:short)", "refs/heads/amf/backup/"); got != "" {
		t.Errorf("backup refs = %q, want none", got)
	}
	if got := mustGit(t, root, "--git-dir", origin, "rev-parse", "amf/issue-1"); got == old {
		t.Errorf("amf/issue-1 still at the old tip %s", old)
	}
}

func TestGuardReusesBranchMergedByPullRequest(t *testing.T) {
	root, origin := setupRepo(t)
	old := pushUnmergedBranch(t, root, "amf/issue-1")

	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	gh.prs = append(gh.prs, map[string]string{"head": "amf/issue-1", "base": "main", "state": "merged", "head_sha": old})
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", cleanClaude))
	var events bytes.Buffer
	r.Events = newEventLogger(r.Log, slog.LevelInfo, &events, slog.LevelDebug, nil, "run")
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	ev := eventsByName(eventLines(t, events.String()))
	wantEvent(t, ev, "issue.reuse", map[string]any{"issue": float64(1), "branch": "amf/issue-1", "sha": old, "reason": "merged_pr"})
	if len(ev["issue.skip"]) != 0 {
		t.Errorf("reuse logged issue.skip: %v", ev["issue.skip"])
	}
	wantEvent(t, ev, "git.push", map[string]any{"branch": "amf/issue-1", "forced": true, "exit_code": float64(0)})
	if cmd := pushCommand(t, ev, "amf/issue-1"); !strings.Contains(cmd, "--force-with-lease=amf/issue-1:"+old) {
		t.Errorf("push command = %q, want a lease on %s", cmd, old)
	}
	if len(ev["issue.backup"]) != 0 {
		t.Errorf("merged branch was backed up: %v", ev["issue.backup"])
	}
	if got := mustGit(t, root, "--git-dir", origin, "for-each-ref", "--format=%(refname:short)", "refs/heads/amf/backup/"); got != "" {
		t.Errorf("backup refs = %q, want none", got)
	}
	if got := mustGit(t, root, "--git-dir", origin, "rev-parse", "amf/issue-1"); got == old {
		t.Errorf("amf/issue-1 still at the old tip %s", old)
	}
}

func TestPushNewBranchIsPlain(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", cleanClaude))
	var events bytes.Buffer
	r.Events = newEventLogger(r.Log, slog.LevelInfo, &events, slog.LevelDebug, nil, "run")
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	ev := eventsByName(eventLines(t, events.String()))
	wantEvent(t, ev, "git.push", map[string]any{"branch": "amf/issue-1", "forced": false, "exit_code": float64(0)})
	if cmd := pushCommand(t, ev, "amf/issue-1"); strings.Contains(cmd, "--force") {
		t.Errorf("push command = %q, want a plain push", cmd)
	}
}

func TestPushLeaseRefusesWhenRemoteMoved(t *testing.T) {
	root, origin := setupRepo(t)
	old := pushUnmergedBranch(t, root, "amf/issue-1")

	mustGit(t, root, "checkout", "-q", "-b", "moved", "main")
	if err := os.WriteFile(filepath.Join(root, "moved.txt"), []byte("moved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, root, "add", "moved.txt")
	mustGit(t, root, "commit", "-qm", "remote moved")
	moved := mustGit(t, root, "rev-parse", "HEAD")
	mustGit(t, root, "checkout", "-q", "main")
	mustGit(t, root, "branch", "-q", "-D", "moved")
	mustGit(t, root, "push", "-q", "origin", moved+":refs/heads/moved")

	hook := filepath.Join(root, ".git", "hooks", "pre-push")
	script := "#!/bin/sh\n" +
		"while read local_ref local_sha remote_ref remote_sha; do\n" +
		"  case \"$remote_ref\" in refs/heads/amf/issue-1)\n" +
		"    git --git-dir=\"$TEST_ORIGIN\" update-ref refs/heads/amf/issue-1 \"$TEST_MOVED\"\n" +
		"  esac\n" +
		"done\n"
	if err := os.WriteFile(hook, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_ORIGIN", origin)
	t.Setenv("TEST_MOVED", moved)

	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", cleanClaude))
	r.Force = true
	err := r.Run(context.Background(), 1)
	if err == nil || !(strings.Contains(err.Error(), "stale info") || strings.Contains(err.Error(), "but expected")) {
		t.Fatalf("Run = %v, want the lease to refuse\nlog:\n%s", err, log)
	}
	if got := mustGit(t, root, "--git-dir", origin, "rev-parse", "amf/issue-1"); got != moved {
		t.Errorf("amf/issue-1 = %s, want the remote's moved tip %s (old %s)", got, moved, old)
	}
	if len(gh.prs) != 0 {
		t.Errorf("PRs = %v, want none", gh.prs)
	}
}
