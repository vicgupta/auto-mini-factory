package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestIssueCloseReopen(t *testing.T) {
	for _, tc := range []struct {
		name, command, state, wantState, wantOut string
		flags                                    []string
		writes                                   []string
	}{
		{"close", "close", "open", "closed", "Closed #7 https://github.com/vic/demo/issues/7\n", nil, []string{"PATCH /repos/vic/demo/issues/7"}},
		{"reopen", "reopen", "closed", "open", "Reopened #7 https://github.com/vic/demo/issues/7\n", nil, []string{"PATCH /repos/vic/demo/issues/7"}},
		{"already closed", "close", "closed", "closed", "#7 is already closed\n", []string{"--comment", "unused", "--delete-branch"}, nil},
		{"already open", "reopen", "open", "open", "#7 is already open\n", nil, nil},
		{"comment and delete", "close", "open", "closed", "Closed #7 https://github.com/vic/demo/issues/7\n", []string{"-c", "leaked test-token secret", "-d"}, []string{"POST /repos/vic/demo/issues/7/comments", "PATCH /repos/vic/demo/issues/7", "DELETE /repos/vic/demo/git/refs/heads/amf/issue-7"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh, srv := newFakeGitHub(t)
			listJSONEnv(t, srv)
			labels := []Label{{Name: labelFailed}, {Name: "bug"}}
			gh.issues[7] = &Issue{Number: 7, State: tc.state, HTMLURL: "https://github.com/vic/demo/issues/7", Labels: labels}
			var out, stderr bytes.Buffer
			err := run(context.Background(), append([]string{"issue", tc.command, " #7 "}, tc.flags...), nil, &out, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.wantOut || stderr.Len() != 0 {
				t.Fatalf("out=%q stderr=%q", &out, &stderr)
			}
			if gh.issues[7].State != tc.wantState || !reflect.DeepEqual(gh.issues[7].Labels, labels) {
				t.Fatalf("issue = %+v", gh.issues[7])
			}
			if !reflect.DeepEqual(gh.writes, tc.writes) {
				t.Fatalf("writes = %v, want %v", gh.writes, tc.writes)
			}
			if len(tc.flags) > 0 && tc.state == "open" {
				if !reflect.DeepEqual(gh.comments[7], []string{"leaked [REDACTED:config] secret"}) || !reflect.DeepEqual(gh.deletes, []string{"amf/issue-7"}) {
					t.Fatalf("comments=%v deletes=%v", gh.comments[7], gh.deletes)
				}
			}
		})
	}
}

func TestIssueView(t *testing.T) {
	for _, state := range []string{"open", "closed"} {
		for _, empty := range []bool{false, true} {
			gh, srv := newFakeGitHub(t)
			listJSONEnv(t, srv)
			is := &Issue{Number: 7, Title: "Fix <login> & logout", State: state, HTMLURL: "https://github.com/vic/demo/issues/7", Body: "Details.\n\n", Labels: []Label{{Name: "bug"}, {Name: labelFailed}}}
			is.User.Login = "vic"
			labels, body := "bug, amf:failed", "\nDetails.\n"
			if empty {
				is.Labels, is.Body, labels, body = nil, "", "-", ""
			}
			gh.issues[7] = is
			var out bytes.Buffer
			if err := run(context.Background(), []string{"issue", "view", "#7"}, nil, &out, io.Discard); err != nil {
				t.Fatal(err)
			}
			want := "#7 Fix <login> & logout\nstate:     " + state + "\nauthor:    vic\nlabels:    " + labels + "\nurl:       https://github.com/vic/demo/issues/7\n" + body
			if out.String() != want {
				t.Fatalf("out=%q, want %q", &out, want)
			}
			out.Reset()
			if err := run(context.Background(), []string{"issue", "view", "7", "-j"}, nil, &out, io.Discard); err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			wantLabels := []any{"bug", labelFailed}
			if empty {
				wantLabels = []any{}
			}
			wantJSON := map[string]any{"number": float64(7), "title": is.Title, "state": state, "url": is.HTMLURL, "author": "vic", "labels": wantLabels, "body": is.Body}
			if !reflect.DeepEqual(got, wantJSON) || !strings.HasSuffix(out.String(), "\n") || strings.Contains(out.String(), `\u003c`) || len(gh.writes) != 0 {
				t.Fatalf("JSON=%s writes=%v", &out, gh.writes)
			}
		}
	}
}

func TestIssueActionsRejectPRsAndMissingIssues(t *testing.T) {
	for _, command := range []string{"close", "reopen", "view"} {
		for _, pr := range []bool{false, true} {
			gh, srv := newFakeGitHub(t)
			listJSONEnv(t, srv)
			wantErr := "404"
			if pr {
				gh.issues[7] = &Issue{Number: 7, State: "open", PullRequest: &struct{}{}}
				wantErr = "#7 is a pull request, not an issue"
			}
			var out bytes.Buffer
			err := run(context.Background(), []string{"issue", command, "7"}, nil, &out, io.Discard)
			if err == nil || !strings.Contains(err.Error(), wantErr) || out.Len() != 0 || len(gh.writes) != 0 {
				t.Fatalf("%s pr=%t: err=%v out=%q writes=%v", command, pr, err, &out, gh.writes)
			}
		}
	}
}

func TestIssueActionsArgsAndHelp(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, command := range []string{"close", "reopen", "view"} {
		for _, args := range [][]string{nil, {"0"}, {"-1"}, {"abc"}, {"#"}, {"1", "2"}, {"1", "--unknown"}} {
			var out bytes.Buffer
			err := run(context.Background(), append([]string{"issue", command}, args...), nil, &out, io.Discard)
			if err == nil || strings.Contains(err.Error(), "GITHUB_TOKEN") || out.Len() != 0 {
				t.Errorf("%s %v: err=%v out=%q", command, args, err, &out)
			}
		}
		for _, args := range [][]string{{"issue", command, "--help"}, {"issue", command, "7", "-h"}, {"help", "issue", command}} {
			var out, stderr bytes.Buffer
			if err := run(context.Background(), args, nil, &out, &stderr); err != nil || !strings.Contains(out.String()+stderr.String(), "amf issue "+command) {
				t.Errorf("%v: err=%v out=%q stderr=%q", args, err, &out, &stderr)
			}
		}
	}
}
