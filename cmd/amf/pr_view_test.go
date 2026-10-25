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
			if gh.viewGets != 1 {
				t.Fatalf("fetched the PR %d times, want once", gh.viewGets)
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
	if len(row) != 15 {
		t.Fatalf("got %d fields: %#v", len(row), row)
	}
	want := map[string]any{
		"number": float64(101), "title": "Fix <&>", "state": "open", "draft": true,
		"url": "https://github.com/vic/demo/pull/101", "head": "amf/issue-56", "base": "main",
		"author": "vic", "issue": float64(56), "mergeable": true, "mergeable_state": "clean",
		"additions": float64(120), "deletions": float64(14), "changed_files": float64(5),
		"body": "<b>body</b>",
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
	for _, args := range [][]string{
		{"pr", "view"},
		{"pr", "view", "--json"},
		{"pr", "view", "1", "2"},
		{"pr", "view", "abc"},
		{"pr", "view", "#abc"},
		{"pr", "view", "0"},
		{"pr", "view", "#0"},
	} {
		var out, stderr bytes.Buffer
		err := run(context.Background(), args, nil, &out, &stderr)
		if !errors.Is(err, errUsage) {
			t.Errorf("%v: %v", args, err)
		}
		if out.Len() != 0 {
			t.Errorf("%v: stdout %q", args, &out)
		}
		if !strings.Contains(stderr.String(), "usage: amf pr view <number> [--json]") {
			t.Errorf("%v: stderr %q", args, &stderr)
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
	if err := run(context.Background(), []string{"help"}, nil, &out, io.Discard); err != nil || !strings.Contains(out.String(), "amf pr") {
		t.Fatalf("help: %v, %q", err, &out)
	}
}
