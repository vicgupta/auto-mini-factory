package main

import (
	"bytes"
	"context"
	"os"
	"slices"
	"strings"
	"testing"
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
		if !gh.issues[n].HasLabel(labelReady) {
			t.Fatalf("#%d labels %v, want it left %s", n, gh.labels(n), labelReady)
		}
	}
	if !strings.Contains(out.String(), "amf auto --continue") || !strings.Contains(out.String(), "checkpoint") {
		t.Fatalf("output doesn't explain how to resume:\n%s", &out)
	}
	st, err := loadAutoState(autoStatePath(r.Root))
	if err != nil || st.Approved || st.merged() != 1 || len(st.Issues) != 3 {
		t.Fatalf("state = %+v, %v", st, err)
	}
	if st.Issues[0].PR == "" || st.Issues[0].Commit == "" || st.Issues[0].Agent == "" {
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
		if !yes && !strings.Contains(out.String(), "Stopped.") {
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
		{[]string{"--continue", "--dry-run"}, "can't be used together"},
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
