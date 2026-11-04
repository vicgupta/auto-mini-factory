package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func viewFixture() prViewResponse {
	yes := true
	pr := prViewResponse{
		Number:         101,
		Title:          "Fix #56: Support OPENCODE_API_KEY",
		State:          "open",
		Mergeable:      &yes,
		MergeableState: "clean",
		HTMLURL:        "https://github.com/vic/demo/pull/101",
		Additions:      120,
		Deletions:      14,
		ChangedFiles:   5,
		Body:           "The agent's summary.",
	}
	pr.User.Login = "vic"
	pr.Head.Ref = "amf/issue-56"
	pr.Head.SHA = "abc123"
	pr.Base.Ref = "main"
	return pr
}

func TestPRViewOutput(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	gh.views[101] = viewFixture()
	listJSONEnv(t, srv)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"pr", "view", "101"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	want := `#101 Fix #56: Support OPENCODE_API_KEY
state:     open
branch:    amf/issue-56 → main
author:    vic
issue:     #56
mergeable: yes
behind:    unknown
checks:    none
changes:   +120 −14 in 5 files
url:       https://github.com/vic/demo/pull/101

The agent's summary.
`
	if out.String() != want {
		t.Fatalf("output:\n%s\nwant:\n%s", &out, want)
	}

	pr := viewFixture()
	pr.ChangedFiles = 1
	pr.Body = ""
	gh.views[101] = pr
	out.Reset()
	if err := run(context.Background(), []string{"pr", "view", "#101"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "changes:   +120 −14 in 1 file\nurl:       https://github.com/vic/demo/pull/101\n") {
		t.Fatalf("singular changes or empty body: %q", &out)
	}
	if strings.HasSuffix(out.String(), "\n\n") {
		t.Fatalf("empty body left a blank line: %q", &out)
	}
}

func TestPRViewStates(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	listJSONEnv(t, srv)
	for _, tc := range []struct {
		name string
		edit func(*prViewResponse)
		want string
	}{
		{"open", func(*prViewResponse) {}, "open"},
		{"draft", func(pr *prViewResponse) { pr.Draft = true }, "open (draft)"},
		{"merged", func(pr *prViewResponse) {
			closed, merged := "closed", "2026-10-25T00:00:00Z"
			pr.State, pr.MergedAt = closed, &merged
		}, "merged"},
		{"closed", func(pr *prViewResponse) { pr.State = "closed" }, "closed"},
		{"closed draft", func(pr *prViewResponse) { pr.State, pr.Draft = "closed", true }, "closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := viewFixture()
			tc.edit(&pr)
			gh.views[101] = pr
			var out bytes.Buffer
			if err := run(context.Background(), []string{"pr", "view", "101"}, nil, &out, io.Discard); err != nil {
				t.Fatal(err)
			}
			if want := fmt.Sprintf("state:     %s\n", tc.want); !strings.Contains(out.String(), want) {
				t.Fatalf("output %q, want %q", &out, want)
			}
		})
	}
}

func TestPRViewMergeable(t *testing.T) {
	noMergeablePause(t)
	gh, srv := newFakeGitHub(t)
	listJSONEnv(t, srv)
	no, yes := false, true
	for _, tc := range []struct {
		name           string
		mergeable      *bool
		mergeableState string
		want           string
	}{
		{"clean", &yes, "clean", "yes"},
		{"no state", &yes, "", "yes"},
		{"conflicts", &no, "dirty", "no (conflicts)"},
		{"false and blocked", &no, "blocked", "no (conflicts)"},
		{"blocked", &yes, "blocked", "blocked (blocked)"},
		{"unstable", &yes, "unstable", "blocked (unstable)"},
		{"behind", &yes, "behind", "blocked (behind)"},
		{"checking", nil, "unknown", "unknown"},
		{"null with no state", nil, "", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := viewFixture()
			pr.Mergeable, pr.MergeableState = tc.mergeable, tc.mergeableState
			gh.views[101] = pr
			gh.viewGets = 0
			var out bytes.Buffer
			if err := run(context.Background(), []string{"pr", "view", "101"}, nil, &out, io.Discard); err != nil {
				t.Fatal(err)
			}
			if want := fmt.Sprintf("mergeable: %s\n", tc.want); !strings.Contains(out.String(), want) {
				t.Fatalf("output %q, want %q", &out, want)
			}
			// An unknown mergeability is re-read a few times; a known
			// one is read once.
			want := 1
			if tc.want == "unknown" {
				want += len(mergeablePolls)
			}
			if gh.viewGets != want {
				t.Fatalf("fetched the PR %d times, want %d", gh.viewGets, want)
			}
		})
	}
}

func TestPRViewIssueFromHead(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	listJSONEnv(t, srv)
	for _, tc := range []struct{ head, want string }{
		{"amf/issue-56", "#56"},
		{"amf/issue-1", "#1"},
		{"feature/x", "-"},
		{"amf/issue-0", "-"},
		{"amf/issue-005", "-"},
		{"amf/issue-56x", "-"},
		{"amf/issue-", "-"},
		{"AMF/issue-56", "-"},
	} {
		t.Run(tc.head, func(t *testing.T) {
			pr := viewFixture()
			pr.Head.Ref = tc.head
			gh.views[101] = pr
			var out bytes.Buffer
			if err := run(context.Background(), []string{"pr", "view", "101"}, nil, &out, io.Discard); err != nil {
				t.Fatal(err)
			}
			if want := fmt.Sprintf("issue:     %s\n", tc.want); !strings.Contains(out.String(), want) {
				t.Fatalf("output %q, want %q", &out, want)
			}
		})
	}
}

func TestPRViewJSON(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	pr := viewFixture()
	pr.Title, pr.Draft, pr.Body = "Fix <&>", true, "<b>body</b>"
	gh.views[101] = pr
	listJSONEnv(t, srv)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"pr", "view", "101", "--json"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `\u003c`) {
		t.Fatalf("JSON escaped HTML: %q", &out)
	}
	var row map[string]any
	if err := json.Unmarshal(out.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if len(row) != 18 {
		t.Fatalf("got %d fields: %#v", len(row), row)
	}
	want := map[string]any{
		"number": float64(101), "title": "Fix <&>", "state": "open", "draft": true,
		"url": "https://github.com/vic/demo/pull/101", "head": "amf/issue-56", "base": "main",
		"author": "vic", "issue": float64(56), "mergeable": true, "mergeable_state": "clean",
		"additions": float64(120), "deletions": float64(14), "changed_files": float64(5),
		"body": "<b>body</b>", "behind_by": nil, "ahead_by": nil,
	}
	for k, v := range want {
		if got := row[k]; got != v {
			t.Errorf("%s = %#v, want %#v", k, got, v)
		}
	}

	merged := "2026-10-25T00:00:00Z"
	pr = viewFixture()
	pr.State, pr.MergedAt, pr.Mergeable = "closed", &merged, nil
	pr.Head.Ref = "feature/topic"
	gh.views[102] = pr
	out.Reset()
	if err := run(context.Background(), []string{"pr", "view", "102", "--json"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	row = nil
	if err := json.Unmarshal(out.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row["state"] != "merged" || row["mergeable"] != nil || row["issue"] != nil || row["draft"] != false {
		t.Fatalf("merged row = %#v", row)
	}
}

func TestPRViewNotFound(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	listJSONEnv(t, srv)
	gh.issues[7] = &Issue{Number: 7, Title: "Issue, not a PR", State: "open"}
	for _, tc := range []struct {
		args []string
		n    int
	}{
		{[]string{"pr", "view", "7"}, 7},
		{[]string{"pr", "view", "#101", "--json"}, 101},
	} {
		gh.viewGets = 0
		var out, stderr bytes.Buffer
		err := run(context.Background(), tc.args, nil, &out, &stderr)
		want := fmt.Sprintf("#%d is not a pull request in vic/demo", tc.n)
		if err == nil || err.Error() != want {
			t.Fatalf("%v: err=%v, want %q", tc.args, err, want)
		}
		if out.Len() != 0 {
			t.Fatalf("%v: stdout %q", tc.args, &out)
		}
		if gh.viewGets != 1 {
			t.Fatalf("%v: fetched %d times", tc.args, gh.viewGets)
		}
	}
}

func TestPRViewBadArgs(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		args   []string
		reason string // expected parse-error text; "" when the case must print usage only
	}{
		{[]string{"pr", "view"}, ""},
		{[]string{"pr", "view", "--json"}, ""},
		{[]string{"pr", "view", "1", "2"}, ""},
		{[]string{"pr", "view", "abc"}, `invalid pull request number "abc": want a positive number`},
		{[]string{"pr", "view", "#abc"}, `invalid pull request number "#abc": want a positive number`},
		{[]string{"pr", "view", "0"}, `invalid pull request number "0": want a positive number`},
		{[]string{"pr", "view", "#0"}, `invalid pull request number "#0": want a positive number`},
	} {
		var out, stderr bytes.Buffer
		err := run(context.Background(), tc.args, nil, &out, &stderr)
		if !errors.Is(err, errUsage) {
			t.Errorf("%v: %v", tc.args, err)
		}
		if out.Len() != 0 {
			t.Errorf("%v: stdout %q", tc.args, &out)
		}
		if !strings.Contains(stderr.String(), "usage: amf pr view <number> [--json]") {
			t.Errorf("%v: stderr %q", tc.args, &stderr)
		}
		if tc.reason == "" {
			if strings.Contains(stderr.String(), "invalid pull request number") {
				t.Errorf("%v: stderr %q, want usage only", tc.args, &stderr)
			}
			continue
		}
		if !strings.Contains(stderr.String(), tc.reason) {
			t.Errorf("%v: stderr %q, want it to contain %q", tc.args, &stderr, tc.reason)
		}
	}
}

func TestPRViewGroup(t *testing.T) {
	for _, args := range [][]string{{"pr"}, {"pr", "help"}, {"pr", "-h"}, {"pr", "--help"}} {
		var out, stderr bytes.Buffer
		if err := run(context.Background(), args, nil, &out, &stderr); err != nil || !strings.Contains(out.String(), "usage: amf pr") || stderr.Len() != 0 {
			t.Fatalf("%v: %v, stdout %q, stderr %q", args, err, &out, &stderr)
		}
	}
	var out, stderr bytes.Buffer
	if err := run(context.Background(), []string{"pr", "bogus"}, nil, &out, &stderr); !errors.Is(err, errUsage) || out.Len() != 0 || !strings.Contains(stderr.String(), "usage: amf pr") {
		t.Fatalf("bogus: %v, stdout %q, stderr %q", err, &out, &stderr)
	}
	out.Reset()
	if err := run(context.Background(), []string{"help"}, nil, &out, io.Discard); err != nil || !strings.Contains(out.String(), "\n  pr ") {
		t.Fatalf("help: %v, %q", err, &out)
	}
}

func noMergeablePause(t *testing.T) {
	saved := mergeablePolls
	mergeablePolls = make([]time.Duration, len(saved))
	t.Cleanup(func() { mergeablePolls = saved })
}

func TestPRViewBehindAndChecks(t *testing.T) {
	noMergeablePause(t)
	gh, srv := newFakeGitHub(t)
	listJSONEnv(t, srv)
	gh.views[101] = viewFixture()
	gh.compares["main...abc123"] = [2]int{2, 3}
	gh.checkRuns["abc123"] = []checkRun{
		{Name: "build", Status: "completed", Conclusion: "success"},
		{Name: "test", Status: "in_progress"},
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"pr", "view", "101"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"behind:    3 commits behind main\n", "checks:    pending (2 checks: 1 success, 1 in_progress)\n"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q, want %q", &out, want)
		}
	}

	gh.compares["main...abc123"] = [2]int{2, 0}
	gh.checkRuns["abc123"][1] = checkRun{Name: "test", Status: "completed", Conclusion: "failure"}
	out.Reset()
	if err := run(context.Background(), []string{"pr", "view", "101", "--json"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var row struct {
		BehindBy *int        `json:"behind_by"`
		AheadBy  *int        `json:"ahead_by"`
		Checks   *viewChecks `json:"checks"`
	}
	if err := json.Unmarshal(out.Bytes(), &row); err != nil {
		t.Fatal(err)
	}
	if row.BehindBy == nil || *row.BehindBy != 0 || row.AheadBy == nil || *row.AheadBy != 2 || row.Checks == nil || row.Checks.State != "failed" {
		t.Fatalf("json: %s", &out)
	}

	// A closed pull request shows neither, and costs no extra calls.
	pr := viewFixture()
	pr.State = "closed"
	gh.views[101] = pr
	out.Reset()
	if err := run(context.Background(), []string{"pr", "view", "101"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "behind:") || strings.Contains(out.String(), "checks:") {
		t.Fatalf("closed PR output: %q", &out)
	}
	gh.compares["main...abc123"] = [2]int{0, 1}
	out.Reset()
	gh.views[101] = viewFixture()
	if err := run(context.Background(), []string{"pr", "view", "101"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "behind:    1 commit behind main\n") {
		t.Fatalf("singular: %q", &out)
	}
}
