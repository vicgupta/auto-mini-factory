package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestForceBacksUpReplacedBranch(t *testing.T) {
	root, origin := setupRepo(t)
	// Someone else's commit on amf/issue-1, not on main.
	mustGit(t, root, "checkout", "-q", "-b", "amf/issue-1")
	if err := os.WriteFile(filepath.Join(root, "manual.txt"), []byte("by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, root, "add", "manual.txt")
	mustGit(t, root, "commit", "-qm", "manual work")
	old := mustGit(t, root, "rev-parse", "HEAD")
	mustGit(t, root, "push", "-q", "origin", "amf/issue-1")
	mustGit(t, root, "checkout", "-q", "main")
	mustGit(t, root, "branch", "-q", "-D", "amf/issue-1")

	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	r, log := newTestRunner(t, srv, root, fakeClaude(t, `cat >/dev/null; echo hello > feature.txt; echo done`))
	r.Force = true
	var events bytes.Buffer
	r.Events = newEventLogger(r.Log, slog.LevelInfo, &events, slog.LevelDebug, nil, "run")
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	ev := eventsByName(eventLines(t, events.String()))
	wantEvent(t, ev, "git.push", map[string]any{"branch": "amf/issue-1", "forced": true, "exit_code": float64(0)})
	if cmd := pushCommand(t, ev, "amf/issue-1"); !strings.Contains(cmd, "--force-with-lease=amf/issue-1:"+old) {
		t.Errorf("push command = %q, want a lease on %s", cmd, old)
	}
	heads := mustGit(t, root, "--git-dir", origin, "for-each-ref", "--format=%(refname:short) %(objectname)", "refs/heads/amf/backup/")
	f := strings.Fields(heads)
	if len(f) != 2 || !strings.HasPrefix(f[0], "amf/backup/issue-1-"+shortSHA(old)+"-") || f[1] != old {
		t.Fatalf("backup refs = %q, want amf/backup/issue-1-%s-* at %s", heads, shortSHA(old), old)
	}
	if !strings.Contains(log.String(), "backed up amf/issue-1") {
		t.Fatalf("log doesn't report the backup:\n%s", log)
	}
	if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], f[0]) {
		t.Fatalf("comments = %q, want the backup branch named", c)
	}
}

func TestForceAbortsWhenBackupFails(t *testing.T) {
	root, origin := setupRepo(t)
	mustGit(t, root, "checkout", "-q", "-b", "amf/issue-1")
	if err := os.WriteFile(filepath.Join(root, "manual.txt"), []byte("by hand\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, root, "add", "manual.txt")
	mustGit(t, root, "commit", "-qm", "manual work")
	old := mustGit(t, root, "rev-parse", "HEAD")
	mustGit(t, root, "push", "-q", "origin", "amf/issue-1")
	mustGit(t, root, "checkout", "-q", "main")
	mustGit(t, root, "branch", "-q", "-D", "amf/issue-1")
	// The remote refuses backup branches.
	hook := filepath.Join(origin, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nwhile read o n ref; do case $ref in refs/heads/amf/backup/*) echo no backups >&2; exit 1;; esac; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	r, log := newTestRunner(t, srv, root, fakeClaude(t, `cat >/dev/null; echo hello > feature.txt; echo done`))
	r.Force = true
	if err := r.Run(context.Background(), 1); err == nil {
		t.Fatalf("Run succeeded, want a backup failure\nlog:\n%s", log)
	}
	if got := mustGit(t, root, "--git-dir", origin, "rev-parse", "amf/issue-1"); got != old {
		t.Fatalf("amf/issue-1 = %s, want it left at %s", got, old)
	}
	if len(gh.prs) != 0 {
		t.Fatalf("PRs = %v, want none", gh.prs)
	}
}
