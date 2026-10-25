package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestPRClose(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state       string
		flags       []string
		headRepo    string
		wantOut     string
		wantErr     string
		wantUpdates int
		wantDeletes []string
		wantSkip    bool
	}{
		{name: "close", state: "open", wantOut: "Closed #101 https://github.com/vic/demo/pull/101\n", wantUpdates: 1},
		{name: "already closed", state: "closed", wantOut: "#101 is already closed\n"},
		{name: "merged", state: "merged", wantErr: "#101 is merged; it can't be closed"},
		{name: "delete", state: "open", flags: []string{"--delete-branch"}, wantOut: "Closed #101 https://github.com/vic/demo/pull/101\n", wantUpdates: 1, wantDeletes: []string{"feature/topic"}},
		{name: "fork", state: "open", flags: []string{"--delete-branch"}, headRepo: "other/demo", wantOut: "Closed #101 https://github.com/vic/demo/pull/101\n", wantUpdates: 1, wantSkip: true},
		{name: "deleted head", state: "open", flags: []string{"--delete-branch"}, headRepo: "deleted", wantOut: "Closed #101 https://github.com/vic/demo/pull/101\n", wantUpdates: 1, wantSkip: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh, srv := newFakeGitHub(t)
			pr := map[string]string{"head": "feature/topic", "base": "main", "state": tc.state}
			if tc.headRepo != "" {
				pr["head_repo"] = tc.headRepo
			}
			gh.prs = []map[string]string{pr}
			listJSONEnv(t, srv)
			var out, stderr bytes.Buffer
			err := run(context.Background(), append([]string{"pr", "close", "#101"}, tc.flags...), nil, &out, &stderr)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(gh.updates) != tc.wantUpdates {
				t.Fatalf("updates = %v, want %d PATCH", gh.updates, tc.wantUpdates)
			}
			if tc.wantUpdates > 0 && !reflect.DeepEqual(gh.updates[0], map[string]any{"state": "closed"}) {
				t.Fatalf("update = %v, want state=closed", gh.updates[0])
			}
			if out.String() != tc.wantOut {
				t.Fatalf("out = %q, want %q", out.String(), tc.wantOut)
			}
			if !reflect.DeepEqual(gh.deletes, tc.wantDeletes) {
				t.Fatalf("deletes = %v, want %v", gh.deletes, tc.wantDeletes)
			}
			if tc.wantSkip && !strings.Contains(stderr.String(), "Skipping branch deletion") {
				t.Fatalf("missing skip note: %q", stderr.String())
			}
		})
	}
}

func TestPRReopen(t *testing.T) {
	for _, tc := range []struct {
		name        string
		state       string
		wantOut     string
		wantErr     string
		wantUpdates int
	}{
		{name: "reopen", state: "closed", wantOut: "Reopened #101 https://github.com/vic/demo/pull/101\n", wantUpdates: 1},
		{name: "already open", state: "open", wantOut: "#101 is already open\n"},
		{name: "merged", state: "merged", wantErr: "#101 is merged; it can't be reopened"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh, srv := newFakeGitHub(t)
			gh.prs = []map[string]string{{"head": "feature/topic", "base": "main", "state": tc.state}}
			listJSONEnv(t, srv)
			var out bytes.Buffer
			err := run(context.Background(), []string{"pr", "reopen", "101"}, nil, &out, io.Discard)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if len(gh.updates) != tc.wantUpdates {
				t.Fatalf("updates = %v, want %d PATCH", gh.updates, tc.wantUpdates)
			}
			if tc.wantUpdates > 0 && !reflect.DeepEqual(gh.updates[0], map[string]any{"state": "open"}) {
				t.Fatalf("update = %v, want state=open", gh.updates[0])
			}
			if out.String() != tc.wantOut {
				t.Fatalf("out = %q, want %q", out.String(), tc.wantOut)
			}
		})
	}
}

// The comment is posted before the closing PATCH, and goes through the
// config redactor like amf's other comments.
func TestPRCloseCommentOrderAndRedaction(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	gh.prs = []map[string]string{{"head": "feature/topic", "base": "main", "state": "open"}}
	gh.issues[101] = &Issue{Number: 101, State: "open"} // the PR's issue, for the comment
	listJSONEnv(t, srv)
	var out, stderr bytes.Buffer
	if err := run(context.Background(), []string{"pr", "close", "101", "--comment", "leaked test-token secret"}, nil, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if out.String() != "Closed #101 https://github.com/vic/demo/pull/101\n" {
		t.Fatalf("out = %q", out.String())
	}
	want := []string{"POST /repos/vic/demo/issues/101/comments", "PATCH /repos/vic/demo/pulls/101"}
	if !reflect.DeepEqual(gh.writes, want) {
		t.Fatalf("writes = %v, want %v", gh.writes, want)
	}
	if c := gh.comments[101]; !reflect.DeepEqual(c, []string{"leaked [REDACTED:config] secret"}) {
		t.Fatalf("comments = %q, want the token redacted", c)
	}
}

func TestPRCloseReopenBadArgs(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{
		{"pr", "close"}, {"pr", "close", "0"}, {"pr", "close", "-1"},
		{"pr", "close", "abc"}, {"pr", "close", "1", "2"}, {"pr", "close", "1", "--unknown"},
		{"pr", "reopen"}, {"pr", "reopen", "0"}, {"pr", "reopen", "-1"},
		{"pr", "reopen", "abc"}, {"pr", "reopen", "1", "2"}, {"pr", "reopen", "1", "--unknown"},
	} {
		var out, stderr bytes.Buffer
		if err := run(context.Background(), args, nil, &out, &stderr); err == nil || strings.Contains(err.Error(), "GITHUB_TOKEN") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestPRCloseReopenGroupUsage(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"pr"}, {"pr", "help"}, {"pr", "-h"}, {"pr", "--help"}, {"pr", "bogus"}} {
		var out, stderr bytes.Buffer
		err := run(context.Background(), args, nil, &out, &stderr)
		if len(args) == 2 && args[1] == "bogus" {
			if !errors.Is(err, errUsage) || out.Len() != 0 || !strings.Contains(stderr.String(), "usage: amf pr") {
				t.Fatalf("%v: %v out=%q stderr=%q", args, err, &out, &stderr)
			}
		} else if err != nil || !strings.Contains(out.String(), "close <N>") || stderr.Len() != 0 {
			t.Fatalf("%v: %v out=%q stderr=%q", args, err, &out, &stderr)
		}
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"help"}, nil, &out, io.Discard); err != nil || !strings.Contains(out.String(), "amf pr") {
		t.Fatalf("help: %v, %q", err, &out)
	}
}
