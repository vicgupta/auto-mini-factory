package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParsePlan(t *testing.T) {
	good := `{"issues": [{"title": "Add a", "scope": "Do a", "acceptance_criteria": ["a works"]}, {"title": " B ", "scope": "b", "acceptance_criteria": ["", "b"]}]}`
	for _, tc := range []struct {
		name, out, wantErr string
		want               int
	}{
		{name: "bare", out: good, want: 2},
		{name: "fenced with prose", out: "Here is the plan:\n```json\n" + good + "\n```\nDone.", want: 2},
		{name: "last fence wins", out: "```json\n{\"issues\": []}\n```\n```json\n" + good + "\n```", want: 2},
		{name: "array", out: `[{"title": "A", "scope": "s", "acceptance_criteria": ["c"]}]`, want: 1},
		{name: "embedded", out: "plan: " + good + " ok", want: 2},
		{name: "not json", out: "I could not do it", wantErr: "no JSON"},
		{name: "empty", out: `{"issues": []}`, wantErr: "no issues"},
		{name: "too many", out: `{"issues": [` + strings.Repeat(`{"title": "A", "scope": "s", "acceptance_criteria": ["c"]},`, 5) + `{"title": "A", "scope": "s", "acceptance_criteria": ["c"]}]}`, wantErr: "more than the 5"},
		{name: "no title", out: `{"issues": [{"title": "", "scope": "s", "acceptance_criteria": ["c"]}]}`, wantErr: "no title"},
		{name: "no scope", out: `{"issues": [{"title": "A", "acceptance_criteria": ["c"]}]}`, wantErr: "no scope"},
		{name: "no criteria", out: `{"issues": [{"title": "A", "scope": "s", "acceptance_criteria": [" "]}]}`, wantErr: "no acceptance criteria"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePlan(tc.out, 5)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || len(got) != tc.want {
				t.Fatalf("got %v, %v; want %d issues", got, err, tc.want)
			}
			if tc.want == 2 && (got[1].Title != "B" || !slices.Equal(got[1].Acceptance, []string{"b"})) {
				t.Fatalf("not trimmed: %+v", got[1])
			}
		})
	}
}

// autoClaude plans three issues, then makes a distinct change for each.
const autoClaude = `prompt=$(cat)
case "$prompt" in
*"You are planning"*)
	printf '%s\n' 'Plan:' '` + "```json" + `' '{"issues": [{"title": "One", "scope": "s1", "acceptance_criteria": ["c1"]}, {"title": "Two", "scope": "s2", "acceptance_criteria": ["c2"]}, {"title": "Three", "scope": "s3", "acceptance_criteria": ["c3"]}]}' '` + "```" + `'
	;;
*) echo "$$" > "change-$$.txt"; echo done ;;
esac
`

func merges(gh *fakeGitHub) int {
	gh.mu.Lock()
	defer gh.mu.Unlock()
	n := 0
	for _, pr := range gh.prs {
		if pr["state"] == "merged" {
			n++
		}
	}
	return n
}

func newAutoRunner(t *testing.T, script string) (*Runner, *fakeGitHub, *bytes.Buffer) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	r, log := newTestRunner(t, srv, root, fakeClaude(t, script))
	return r, gh, log
}

func TestAutoStopsAtCheckpointWithoutTerminal(t *testing.T) {
	r, gh, log := newAutoRunner(t, autoClaude)
	var events bytes.Buffer
	r.Events = newEventLogger(log, slog.LevelInfo, &events, slog.LevelDebug, nil, "auto")
	var out bytes.Buffer
	noTTY := func(string) (bool, bool) { return false, false }
	opts := autoOptions{MaxIssues: 5, Merge: "merge"}
	if err := r.Auto(context.Background(), "do three things", opts, noTTY, &out); err != nil {
		t.Fatalf("Auto: %v\nout:\n%s\nlog:\n%s", err, &out, log)
	}
	if n := merges(gh); n != 1 {
		t.Fatalf("%d PRs merged before approval, want 1", n)
	}
	for n := 1; n <= 3; n++ {
		if !gh.issues[n].HasLabel(labelAuto) {
			t.Fatalf("#%d has labels %v, want %s", n, gh.labels(n), labelAuto)
		}
	}
	for _, n := range []int{2, 3} {
		if gh.issues[n].HasLabel(labelReady) {
			t.Fatalf("#%d labels %v, want it held without %s", n, gh.labels(n), labelReady)
		}
	}
	if !strings.Contains(out.String(), "amf auto --continue") || !strings.Contains(out.String(), "checkpoint") {
		t.Fatalf("output doesn't explain how to resume:\n%s", &out)
	}
	st, err := loadAutoState(autoStatePath(r.Root))
	if err != nil || st.Approved || st.merged() != 1 || len(st.Issues) != 3 {
		t.Fatalf("state = %+v, %v", st, err)
	}
	if st.Issues[0].PR == "" || st.Issues[0].PRNumber != 101 || st.Issues[0].Commit == "" || st.Issues[0].Agent == "" {
		t.Fatalf("first issue not recorded: %+v", st.Issues[0])
	}

	// A new sentence is refused while this run waits.
	if err := r.Auto(context.Background(), "something else", opts, noTTY, &out); err == nil || !strings.Contains(err.Error(), "--continue") {
		t.Fatalf("second run err = %v", err)
	}

	// --continue approves and finishes.
	out.Reset()
	if err := r.Auto(context.Background(), "", autoOptions{Continue: true}, noTTY, &out); err != nil {
		t.Fatalf("continue: %v\nlog:\n%s", err, log)
	}
	if n := merges(gh); n != 3 {
		t.Fatalf("%d merged after --continue, want 3", n)
	}
	if _, err := os.Stat(autoStatePath(r.Root)); !os.IsNotExist(err) {
		t.Fatalf("state left behind: %v", err)
	}
	picked := eventsByName(eventLines(t, events.String()))["issue.picked"]
	if len(picked) != 3 {
		t.Fatalf("picked %d issues, want 3", len(picked))
	}
	for _, ev := range picked {
		if !slices.Contains(ev["labels"].([]any), any(labelReady)) {
			t.Errorf("issue started without %s: %v", labelReady, ev)
		}
	}
	for _, ev := range []string{"approved with --continue", "amf auto is done"} {
		if !strings.Contains(log.String()+out.String(), ev) {
			t.Fatalf("missing %q:\n%s\n%s", ev, log, &out)
		}
	}
}

func TestAutoPromptDeclinedAndApproved(t *testing.T) {
	for _, yes := range []bool{false, true} {
		r, gh, log := newAutoRunner(t, autoClaude)
		var out bytes.Buffer
		asked := 0
		approve := func(string) (bool, bool) { asked++; return true, yes }
		if err := r.Auto(context.Background(), "do three things", autoOptions{MaxIssues: 5, Merge: "squash"}, approve, &out); err != nil {
			t.Fatalf("Auto: %v\nlog:\n%s", err, log)
		}
		want := 1
		if yes {
			want = 3
		}
		if n := merges(gh); n != want || asked != 1 {
			t.Fatalf("yes=%v: %d merged, asked %d times; want %d, once", yes, n, asked, want)
		}
		if gh.prs[0]["merge_method"] != "squash" {
			t.Fatalf("merge method %q", gh.prs[0]["merge_method"])
		}
		if !yes && !strings.Contains(out.String(), "Stopped. The remaining issues stay labeled amf:auto only, unprocessed.") {
			t.Fatalf("decline not reported:\n%s", &out)
		}
	}
}

func TestAutoStopsWhenFirstMergeFails(t *testing.T) {
	// The first issue edits AGENTS.md, which amf never merges.
	script := strings.Replace(autoClaude, `*) echo "$$"`, `*) echo x >> AGENTS.md; echo "$$"`, 1)
	r, gh, _ := newAutoRunner(t, script)
	var out bytes.Buffer
	approve := func(string) (bool, bool) { t.Fatal("asked for approval after a failed merge"); return true, true }
	err := r.Auto(context.Background(), "do three things", autoOptions{MaxIssues: 5, Merge: "merge"}, approve, &out)
	if err == nil || !strings.Contains(out.String(), "stopped at #1") {
		t.Fatalf("err = %v, out:\n%s", err, &out)
	}
	if n := merges(gh); n != 0 || len(gh.prs) != 1 {
		t.Fatalf("merged %d of %d PRs; want only the first opened, unmerged", n, len(gh.prs))
	}
}

func TestAutoBadPlanCreatesNothing(t *testing.T) {
	r, gh, _ := newAutoRunner(t, `cat >/dev/null; echo "sorry, no plan"`)
	err := r.Auto(context.Background(), "do it", autoOptions{MaxIssues: 5, Merge: "merge"}, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "could not plan") {
		t.Fatalf("err = %v", err)
	}
	if len(gh.issues) != 0 {
		t.Fatalf("created %d issues from a bad plan", len(gh.issues))
	}
}

func TestAutoDryRunCreatesNothing(t *testing.T) {
	r, gh, _ := newAutoRunner(t, autoClaude)
	var out bytes.Buffer
	if err := r.Auto(context.Background(), "do three things", autoOptions{DryRun: true, MaxIssues: 5, Merge: "merge"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if len(gh.issues) != 0 || !strings.Contains(out.String(), "3. Three") {
		t.Fatalf("issues %d, out:\n%s", len(gh.issues), &out)
	}
}

func TestAutoFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--continue", "x"}, "takes no sentence"},
		{[]string{"x", "--merge=false"}, "always merges"},
		{[]string{"x", "--max-issues", "0"}, "between 1 and 10"},
		{[]string{"x", "--checkpoint-after", "0"}, "--checkpoint-after must be at least 1"},
		{[]string{"x", "--checkpoint-after", "-1"}, "--checkpoint-after must be at least 1"},
		{[]string{"--continue", "--dry-run"}, "can't be used together"},
		{[]string{"--abort", "x"}, "takes no sentence"},
		{[]string{"--abort", "--continue"}, "can't be used together"},
		{[]string{"--abort", "--dry-run"}, "can't be used together"},
		{[]string{"--close-issues"}, "requires --abort"},
		{[]string{"status", "x"}, "takes no sentence"},
		{[]string{"status", "--abort"}, "can't be used"},
		{[]string{"status", "--continue"}, "can't be used"},
		{[]string{"status", "--dry-run"}, "can't be used"},
		{[]string{"status", "--agent", "codex"}, "can't be used"},
	} {
		err := cmdAuto(context.Background(), tc.args, nil, &bytes.Buffer{}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
	if err := cmdAuto(context.Background(), nil, nil, &bytes.Buffer{}, &bytes.Buffer{}); err != errUsage {
		t.Fatalf("no args: %v", err)
	}
}

func TestAutoCheckpointHelp(t *testing.T) {
	var out bytes.Buffer
	cmdAuto(context.Background(), []string{"--help"}, nil, &bytes.Buffer{}, &out)
	for _, flag := range []string{"--no-checkpoint", "--checkpoint-after", "--approve", "--wait-for-checks"} {
		if !strings.Contains(out.String(), flag) {
			t.Errorf("help missing %s:\n%s", flag, &out)
		}
	}
}

func TestAutoCheckpointControls(t *testing.T) {
	for _, tc := range []struct {
		name            string
		opts            autoOptions
		merges, prompts int
	}{
		{"disabled", autoOptions{NoCheckpoint: true, WaitForChecks: true}, 3, 0},
		{"approved without terminal", autoOptions{Approve: true}, 3, 0},
		{"after two", autoOptions{CheckpointAfter: 2}, 2, 1},
		{"after two approved", autoOptions{CheckpointAfter: 2, Approve: true}, 3, 0},
		{"after all", autoOptions{CheckpointAfter: 3}, 3, 0},
		{"beyond plan", autoOptions{CheckpointAfter: 4}, 3, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, gh, log := newAutoRunner(t, autoClaude)
			tc.opts.MaxIssues, tc.opts.Merge = 5, "merge"
			var out bytes.Buffer
			prompts := 0
			wantApproval := tc.opts.Approve
			approve := func(string) (bool, bool) {
				prompts++
				if n := merges(gh); n != tc.merges {
					t.Errorf("checkpoint after %d merges, want %d", n, tc.merges)
				}
				return terminalApprover(strings.NewReader("y\n"), &out)("")
			}
			// Inspect the last recovery state before successful completion removes it.
			writer := writerFunc(func(p []byte) (int, error) {
				if bytes.Contains(p, []byte("amf auto is done")) {
					st, err := loadAutoState(autoStatePath(r.Root))
					if err != nil {
						t.Fatal(err)
					}
					if st.Approved != wantApproval {
						t.Errorf("saved approval = %v, want %v", st.Approved, wantApproval)
					}
				}
				return out.Write(p)
			})
			if err := r.Auto(context.Background(), "do three things", tc.opts, approve, writer); err != nil {
				t.Fatalf("Auto: %v\n%s", err, log)
			}
			if n := merges(gh); n != tc.merges || prompts != tc.prompts {
				t.Fatalf("merged %d, prompts %d; want %d, %d", n, prompts, tc.merges, tc.prompts)
			}
			if tc.opts.NoCheckpoint && strings.Contains(out.String(), "checkpoint") {
				t.Fatalf("checkpoint printed with --no-checkpoint:\n%s", &out)
			}
			if tc.merges == 2 {
				st, err := loadAutoState(autoStatePath(r.Root))
				if err != nil || st.CheckpointAfter != 2 || st.Approved {
					t.Fatalf("state = %+v, %v", st, err)
				}
				if !strings.Contains(out.String(), "issue:   #2") {
					t.Fatalf("checkpoint did not show latest merge:\n%s", &out)
				}
				wantApproval = true
				if err := r.Auto(context.Background(), "", autoOptions{Continue: true}, nil, writer); err != nil {
					t.Fatal(err)
				}
				if merges(gh) != 3 {
					t.Fatal("continue did not finish delayed checkpoint")
				}
			}
		})
	}
}

func TestAutoContinueBeforeDelayedCheckpoint(t *testing.T) {
	r, gh, log := newAutoRunner(t, autoClaude)
	path := autoStatePath(r.Root)
	ctx := context.Background()
	// Stop after one merge and model an interrupted run whose checkpoint is later.
	noTTY := terminalApprover(strings.NewReader(""), &bytes.Buffer{})
	if err := r.Auto(ctx, "do three things", autoOptions{MaxIssues: 5, Merge: "merge"}, noTTY, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	st, err := loadAutoState(path)
	if err != nil {
		t.Fatal(err)
	}
	st.CheckpointAfter = 2
	if err := saveAutoState(path, st); err != nil {
		t.Fatal(err)
	}
	if err := r.Auto(ctx, "", autoOptions{Continue: true}, noTTY, &bytes.Buffer{}); err != nil {
		t.Fatalf("continue: %v\n%s", err, log)
	}
	st, err = loadAutoState(path)
	if err != nil || st.Approved || st.CheckpointAfter != 2 || merges(gh) != 2 {
		t.Fatalf("state = %+v, %v; merged %d", st, err, merges(gh))
	}
	// Disabling a reached checkpoint also leaves approval unset.
	if err := r.Auto(ctx, "", autoOptions{Continue: true, NoCheckpoint: true}, nil, writerFunc(func(p []byte) (int, error) {
		if bytes.Contains(p, []byte("amf auto is done")) {
			st, err := loadAutoState(path)
			if err != nil || st.Approved || !st.NoCheckpoint {
				t.Fatalf("disabled state = %+v, %v", st, err)
			}
		}
		return len(p), nil
	})); err != nil {
		t.Fatal(err)
	}
}

func TestAutoCheckSummary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		responses []string
		wait      time.Duration
		want      string
	}{
		{"default does not wait", []string{`{"status":"in_progress"}`}, 0, "1 check: 1 in_progress"},
		{"default empty", []string{""}, 0, "none reported yet"},
		{"wait for appearance and completion", []string{"", `{"status":"queued"}`, `{"status":"in_progress"}`, `{"status":"completed","conclusion":"success"}`}, time.Second, "passed: 1 check: 1 success"},
		{"failed", []string{`{"status":"completed","conclusion":"failure"}`}, time.Second, "failed: 1 check: 1 failure"},
		{"mixed completion", []string{`{"status":"completed","conclusion":"failure"},{"status":"queued"}`, `{"status":"completed","conclusion":"failure"},{"status":"completed","conclusion":"success"}`}, time.Second, "failed: 2 checks: 1 failure, 1 success"},
		{"neutral and skipped", []string{`{"status":"completed","conclusion":"neutral"},{"status":"completed","conclusion":"skipped"}`}, time.Second, "passed:"},
		{"timeout pending", []string{`{"status":"in_progress"}`}, 30 * time.Millisecond, "timed out"},
		{"timeout absent", []string{""}, 30 * time.Millisecond, "timed out"},
		{"request timeout", []string{"block"}, 30 * time.Millisecond, "timed out"},
		{"API error", []string{"error"}, time.Second, "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/repos/vic/demo/commits/merge-sha/check-runs" {
					t.Errorf("checks requested for %s", req.URL.Path)
				}
				response := tc.responses[min(calls, len(tc.responses)-1)]
				calls++
				if response == "block" {
					<-req.Context().Done()
					return
				}
				if response == "error" {
					writeJSON(w, http.StatusForbidden, map[string]string{"message": "denied"})
					return
				}
				fmt.Fprintf(w, `{"check_runs":[%s]}`, response)
			}))
			defer srv.Close()
			r := &Runner{GH: newGitHub(Config{Repo: "vic/demo", APIURL: srv.URL})}
			got := r.checkSummary(context.Background(), "merge-sha", tc.wait, time.Millisecond)
			srv.Close()
			if !strings.Contains(got, tc.want) {
				t.Fatalf("summary = %q, want %q", got, tc.want)
			}
			if calls < len(tc.responses) || (tc.wait == 0 && calls != 1) {
				t.Fatalf("called CheckRuns %d times", calls)
			}
		})
	}
}

func TestAutoCheckpointWaitBeforeApproval(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("canceled=%v", cancel), func(t *testing.T) {
			r, _, _ := newAutoRunner(t, autoClaude)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if cancel {
					<-req.Context().Done()
					return
				}
				writeJSON(w, 200, map[string]any{"check_runs": []checkRun{{Status: "completed", Conclusion: "failure"}}})
			}))
			defer srv.Close()
			r.GH.base = srv.URL
			st := &autoState{Version: autoStateVersion, WaitForChecks: true, Issues: []autoIssue{
				{Number: 1, Status: "merged", Commit: "merge-sha"}, {Number: 2, Status: "pending"},
			}}
			ctx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if cancel {
				ctx, stop = context.WithTimeout(ctx, 30*time.Millisecond)
				defer stop()
			}
			var out bytes.Buffer
			asked := false
			approve := func(string) (bool, bool) {
				asked = true
				if !strings.Contains(out.String(), "checks:  failed: 1 check: 1 failure") {
					t.Fatalf("checks not reported before approval:\n%s", &out)
				}
				return false, true
			}
			if got, err := r.autoCheckpoint(ctx, st, autoStatePath(r.Root), approve, &out, "prompt"); got == cancel || asked == cancel || st.Approved == cancel || (err != nil) != cancel {
				t.Fatalf("approval = %v, error = %v, asked = %v, saved approval = %v, canceled = %v", got, err, asked, st.Approved, cancel)
			}
		})
	}
}

func TestAutoStatus(t *testing.T) {
	root, _ := setupRepo(t)
	t.Chdir(root)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_REPO", "")
	var out bytes.Buffer
	if err := cmdAuto(context.Background(), []string{"status"}, nil, &out, &bytes.Buffer{}); err != nil || !strings.Contains(out.String(), "No amf auto run") {
		t.Fatalf("no run: %v, output: %s", err, &out)
	}
	st := &autoState{Version: autoStateVersion, Sentence: "do three things", Merge: "squash", Issues: []autoIssue{
		{Number: 1, Title: "One", Status: "merged", PR: "https://github.com/vic/demo/pull/101", Commit: "abc123"},
		{Number: 2, Title: "Two", Status: "failed", PR: "https://github.com/vic/demo/pull/102"},
		{Title: "Three", Status: "pending"},
	}}
	path := autoStatePath(root)
	if err := saveAutoState(path, st); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := cmdAuto(context.Background(), []string{"status"}, nil, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{st.Sentence, "squash", "#1", "One", "merged", st.Issues[0].PR, "abc123", "#2", "failed", st.Issues[1].PR, "Three", "pending", "not created"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %s", want, &out)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("status changed state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".amf", "runs")); !os.IsNotExist(err) {
		t.Fatalf("status created logs: %v", err)
	}
}

func TestAutoAbortThenNewRun(t *testing.T) {
	r, gh, _ := newAutoRunner(t, autoClaude)
	t.Chdir(r.Root)
	ctx := context.Background()
	noTTY := func(string) (bool, bool) { return false, false }
	opts := autoOptions{MaxIssues: 5, Merge: "merge"}
	var out bytes.Buffer
	if err := r.Auto(ctx, "first run", opts, noTTY, &out); err != nil {
		t.Fatal(err)
	}
	before := len(gh.writes)
	if err := cmdAuto(ctx, []string{"--abort"}, nil, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(autoStatePath(r.Root)); !os.IsNotExist(err) {
		t.Fatalf("state left after abort: %v", err)
	}
	if len(gh.writes) != before || gh.issues[2].State != "open" {
		t.Fatal("plain abort changed GitHub issues")
	}
	if err := r.Auto(ctx, "second run", opts, noTTY, &out); err != nil {
		t.Fatal(err)
	}
	st, err := loadAutoState(autoStatePath(r.Root))
	if err != nil || st.Sentence != "second run" || len(gh.issues) != 6 {
		t.Fatalf("new run state = %+v, error = %v, issues = %d", st, err, len(gh.issues))
	}
}
