package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// wantGitHubUnchanged fails if amf wrote anything to GitHub: a label,
// comment, PR or merge.
func wantGitHubUnchanged(t *testing.T, gh *fakeGitHub, n int, labels []string) {
	t.Helper()
	if len(gh.writes) != 0 {
		t.Errorf("dry run wrote to GitHub: %v", gh.writes)
	}
	if got := gh.labels(n); !reflect.DeepEqual(got, labels) {
		t.Errorf("labels = %v, want %v", got, labels)
	}
	if c := gh.comments[n]; len(c) != 0 {
		t.Errorf("comments = %q, want none", c)
	}
}

// wantNotPushed fails if branch reached origin.
func wantNotPushed(t *testing.T, root, origin, branch string) {
	t.Helper()
	if out, _ := git(context.Background(), root, "--git-dir", origin, "branch", "--list", branch); out != "" {
		t.Errorf("%s was pushed to origin", branch)
	}
}

func TestDryRunLeavesGitHubUnchanged(t *testing.T) {
	root, origin := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	r, log := newTestRunner(t, srv, root, fakeClaude(t, `cat >/dev/null; echo hello > feature.txt
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"Added feature.txt."}'`))
	r.DryRun, r.Merge = true, "squash"
	var events bytes.Buffer
	r.Events = newEventLogger(r.Log, slog.LevelInfo, &events, slog.LevelDebug, nil, "run")

	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	wantGitHubUnchanged(t, gh, 1, []string{"amf:ready"})
	if len(gh.prs) != 0 {
		t.Fatalf("PRs = %v, want none", gh.prs)
	}
	wantNotPushed(t, root, origin, "amf/issue-1")
	// The change is committed on the local branch, as a real run makes it.
	if got := mustGit(t, root, "show", "amf/issue-1:feature.txt"); got != "hello" {
		t.Fatalf("local amf/issue-1 has feature.txt = %q", got)
	}
	if got := mustGit(t, root, "log", "--format=%s", "origin/main..amf/issue-1"); got != "Fix #1: Add feature file" {
		t.Fatalf("local branch commits = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".amf", "worktrees", "issue-1")); !os.IsNotExist(err) {
		t.Fatalf("worktree should be removed after a dry run, stat err = %v", err)
	}
	for _, want := range []string{
		"#1: dry run: committed ",
		" on the local branch amf/issue-1; amf would push it and open a pull request into main, and merge it (squash) if GitHub allows",
		"feature.txt | 1 +",
		"Title: Fix #1: Add feature file",
		"Added feature.txt.\n\nCloses #1",
		"Nothing was pushed, and the issue and GitHub are unchanged. To see the change: git diff origin/main amf/issue-1",
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
	ev := eventsByName(eventLines(t, events.String()))
	wantEvent(t, ev, "issue.dry_run", map[string]any{"branch": "amf/issue-1", "base": "main", "files": []any{"feature.txt"},
		"action": "open", "url": "", "draft": false, "merge": "squash", "title": "Fix #1: Add feature file"})
	wantEvent(t, ev, "issue.done", map[string]any{"dry_run": true})
	for _, name := range []string{"issue.status", "issue.push", "issue.pr", "issue.merge", "issue.comment"} {
		if len(ev[name]) != 0 {
			t.Errorf("dry run logged %s: %v", name, ev[name])
		}
	}

	// A real run afterwards starts the branch over and opens the PR.
	r.DryRun = false
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("real run after the dry run: %v\nlog:\n%s", err, log)
	}
	if len(gh.prs) != 1 || gh.prs[0]["state"] != "merged" {
		t.Fatalf("PRs = %v, want one, merged", gh.prs)
	}
}

func TestDryRunWouldUpdateOpenPR(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	gh.prs = []map[string]string{{"head": "amf/issue-1", "state": "open", "title": "Fix #1: old"}}
	gh.drafts = []bool{false}
	r, log := newTestRunner(t, srv, root, fakeClaude(t, `cat >/dev/null; echo hello > feature.txt; echo done`))
	r.DryRun, r.Draft = true, true

	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	wantGitHubUnchanged(t, gh, 1, []string{"amf:ready"})
	if gh.prs[0]["title"] != "Fix #1: old" {
		t.Fatalf("the open PR changed: %v", gh.prs[0])
	}
	if want := "amf would push it and update https://github.com/vic/demo/pull/101\n"; !strings.Contains(log.String(), want) {
		t.Fatalf("log missing %q:\n%s", want, log)
	}
}

func TestDryRunWouldOpenDraft(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", State: "open"}
	r, log := newTestRunner(t, srv, root, fakeClaude(t, `cat >/dev/null; echo hello > feature.txt; echo done`))
	r.DryRun, r.Draft = true, true

	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	wantGitHubUnchanged(t, gh, 1, nil)
	if want := "amf would push it and open a draft pull request into main\n"; !strings.Contains(log.String(), want) {
		t.Fatalf("log missing %q:\n%s", want, log)
	}
}

// --merge in a dry run applies the AGENTS.md rule a real merge would.
func TestDryRunWouldNotMergeAgentsMD(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Edit the agent notes", State: "open"}
	r, log := newTestRunner(t, srv, root, fakeClaude(t, `cat >/dev/null; echo notes > AGENTS.md; echo done`))
	r.DryRun, r.Merge = true, "merge"

	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	wantGitHubUnchanged(t, gh, 1, nil)
	if want := "open a pull request into main, but not merge it: the change edits AGENTS.md"; !strings.Contains(log.String(), want) {
		t.Fatalf("log missing %q:\n%s", want, log)
	}
}

// A dry run that fails returns the error and leaves the issue as it was.
func TestDryRunFailureLeavesIssue(t *testing.T) {
	for name, tc := range map[string]struct {
		script, wantErr string
		trusted         []string
		timeout         time.Duration
	}{
		"no changes":     {script: `cat >/dev/null; echo nothing`, wantErr: "finished without making any changes"},
		".github change": {script: `cat >/dev/null; mkdir -p .github; echo x > .github/ci.yml; echo done`, wantErr: "which amf refuses to push"},
		"untrusted":      {script: `cat >/dev/null; echo hi > a.txt; echo done`, trusted: []string{"someone"}, wantErr: "not in AMF_TRUSTED"},
		"timeout":        {script: "exec sleep 30\n", timeout: 200 * time.Millisecond, wantErr: "timed out"},
	} {
		t.Run(name, func(t *testing.T) {
			root, origin := setupRepo(t)
			gh, srv := newFakeGitHub(t)
			gh.issues[1] = &Issue{Number: 1, Title: "Try it", State: "open", Labels: []Label{{Name: "amf:ready"}}}
			gh.issues[1].User.Login = "vic"
			r, log := newTestRunner(t, srv, root, fakeClaude(t, tc.script))
			r.DryRun, r.Trusted = true, tc.trusted
			r.KillGrace = 100 * time.Millisecond
			if tc.timeout > 0 {
				r.Timeout = tc.timeout
			}

			err := r.Run(context.Background(), 1)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %v, want %q\nlog:\n%s", err, tc.wantErr, log)
			}
			wantGitHubUnchanged(t, gh, 1, []string{"amf:ready"})
			wantNotPushed(t, root, origin, "amf/issue-1")
		})
	}
}

// A pipeline whose eval never passes pushes its last attempt for
// inspection, but not in a dry run.
func TestDryRunPipelineEvalNeverPasses(t *testing.T) {
	skills := skillServer(t, testSkills)
	root, origin := setupPipelineRepo(t, pipelineTable(skills.URL))
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	r, log := newPipelineRunner(t, srv, skills, root)
	r.Opencode = fakeAgent(t, "opencode", `
out=$(cat | sed -n 's/.*final reply, exactly as you give it, to the file \(.*\)\.$/\1/p')
printf -- '- **blocking** still wrong\n\nVERDICT: FAIL\n' > "$out"
`)
	r.EvalRounds, r.DryRun = 0, true

	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "after 1 code round") {
		t.Fatalf("expected eval failure, got %v\nlog:\n%s", err, log)
	}
	wantGitHubUnchanged(t, gh, 1, []string{"amf:ready"})
	wantNotPushed(t, root, origin, "amf/issue-1")
}

func TestDryRunPipelinePasses(t *testing.T) {
	skills := skillServer(t, testSkills)
	root, origin := setupPipelineRepo(t, pipelineTable(skills.URL))
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", Body: "Please add feature.txt", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	r, log := newPipelineRunner(t, srv, skills, root)
	r.DryRun = true

	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	wantGitHubUnchanged(t, gh, 1, []string{"amf:ready"})
	wantNotPushed(t, root, origin, "amf/issue-1")
	for _, want := range []string{"## Evaluation", "**PASS** by opencode (opencode/test) after 2 code rounds", "<summary>Plan (claudecode, claude-opus-5-5)</summary>"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("printed PR body missing %q:\n%s", want, log)
		}
	}
}

// --dry-run is only for run: watch would redo every queued issue each poll.
func TestDryRunFlag(t *testing.T) {
	t.Chdir(t.TempDir())
	var stderr bytes.Buffer
	if err := run(context.Background(), []string{"watch", "--dry-run"}, strings.NewReader(""), &bytes.Buffer{}, &stderr); err != errUsage {
		t.Errorf("watch --dry-run: %v, want a usage error", err)
	}
	// run takes it, and gets as far as reading the configuration.
	err := run(context.Background(), []string{"run", "1", "--dry-run"}, strings.NewReader(""), &bytes.Buffer{}, &stderr)
	if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("run 1 --dry-run: %v, want the missing GITHUB_TOKEN", err)
	}
}
