package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPRMerge(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		flags                 []string
		state                 string
		draft, merged         bool
		headRepo              string
		failures              []fakeError
		deleteError           bool
		want, wantErr, method string
		attempts, deletes     int
	}{
		{name: "retry", failures: []fakeError{{405, "Pull Request is not mergeable"}}, method: "merge", attempts: 2, want: "Merged"},
		{name: "merge", method: "merge", attempts: 1, want: "Merged"},
		{name: "squash", flags: []string{"--squash"}, method: "squash", attempts: 1, want: "Squashed and merged"},
		{name: "rebase", flags: []string{"--rebase"}, method: "rebase", attempts: 1, want: "Rebased and merged"},
		{name: "draft", draft: true, wantErr: "#101 is a draft; mark it ready for review first"},
		{name: "merged", state: "closed", merged: true, wantErr: "#101 is already merged"},
		{name: "closed", state: "closed", wantErr: "#101 is closed"},
		{name: "changed head", failures: []fakeError{{409, "Head branch was modified"}}, attempts: 1, wantErr: "Head branch was modified"},
		{name: "checks", failures: []fakeError{{405, "Required checks pending"}}, attempts: 1, wantErr: "Required checks pending"},
		{name: "validation", failures: []fakeError{{422, "Invalid merge method"}}, attempts: 1, wantErr: "Invalid merge method"},
		{name: "delete", flags: []string{"--delete-branch"}, method: "merge", attempts: 1, deletes: 1, want: "Merged"},
		{name: "fork", flags: []string{"--delete-branch"}, headRepo: "other/demo", method: "merge", attempts: 1, want: "Merged"},
		{name: "missing repo", flags: []string{"--delete-branch"}, headRepo: "deleted", method: "merge", attempts: 1, want: "Merged"},
		{name: "delete fails", flags: []string{"--delete-branch"}, deleteError: true, method: "merge", attempts: 1, deletes: 1, want: "Merged", wantErr: "merged #101 but could not delete feature/topic: github DELETE"},
		{name: "conflicting flags", flags: []string{"--squash", "--rebase"}, wantErr: "--squash and --rebase can't be used together"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempts, deletes, gets := 0, 0, 0
			state := tc.state
			if state == "" {
				state = "open"
			}
			headRepo := tc.headRepo
			if headRepo == "" {
				headRepo = "vic/demo"
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "GET" && r.URL.Path == "/repos/vic/demo":
					writeJSON(w, 200, map[string]any{"default_branch": "main", "permissions": map[string]bool{"push": true}})
				case r.Method == "GET" && r.URL.Path == "/repos/vic/demo/pulls/101":
					gets++
					var repo any = map[string]string{"full_name": headRepo}
					if headRepo == "deleted" {
						repo = nil
					}
					writeJSON(w, 200, map[string]any{"number": 101, "html_url": "https://github.com/vic/demo/pull/101", "state": state, "draft": tc.draft, "merged": tc.merged, "head": map[string]any{"sha": "abc123", "ref": "feature/topic", "repo": repo}})
				case r.Method == "PUT" && r.URL.Path == "/repos/vic/demo/pulls/101/merge":
					attempts++
					var body map[string]string
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body["sha"] != "abc123" {
						t.Errorf("unpinned merge: %v", body)
					}
					if tc.method != "" && body["merge_method"] != tc.method {
						t.Errorf("method: %v", body)
					}
					if attempts <= len(tc.failures) {
						e := tc.failures[attempts-1]
						writeJSON(w, e.status, map[string]string{"message": e.message})
						return
					}
					writeJSON(w, 200, map[string]any{"merged": true, "sha": "merged-sha"})
				case r.Method == "DELETE" && r.URL.Path == "/repos/vic/demo/git/refs/heads/feature/topic":
					deletes++
					if tc.deleteError {
						writeJSON(w, 422, map[string]string{"message": "protected branch"})
						return
					}
					w.WriteHeader(204)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			listJSONEnv(t, srv)
			var out, stderr bytes.Buffer
			err := run(context.Background(), append([]string{"pr", "merge", "#101"}, tc.flags...), strings.NewReader(""), &out, &stderr)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error=%v, want %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if attempts != tc.attempts || deletes != tc.deletes {
				t.Fatalf("attempts=%d deletes=%d", attempts, deletes)
			}
			if tc.want != "" && out.String() != tc.want+" #101 https://github.com/vic/demo/pull/101\n" {
				t.Fatalf("output=%q", out.String())
			}
			if tc.want == "" && out.Len() != 0 {
				t.Fatalf("output on failure: %q", out.String())
			}
			if tc.headRepo != "" && !strings.Contains(stderr.String(), "Skipping") {
				t.Fatalf("missing skip note: %s", stderr.String())
			}
			if tc.name == "conflicting flags" && gets != 0 {
				t.Fatalf("invalid flags fetched PR")
			}
		})
	}
}

func TestPRMergeBadArgs(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"pr", "merge"}, {"pr", "merge", "0"}, {"pr", "merge", "-1"}, {"pr", "merge", "abc"}, {"pr", "merge", "1", "2"}, {"pr", "merge", "1", "--unknown"}} {
		var out, stderr bytes.Buffer
		if err := run(context.Background(), args, strings.NewReader(""), &out, &stderr); err == nil || strings.Contains(err.Error(), "GITHUB_TOKEN") {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// The same retry policy serves the explicit command and run/watch. The short
// interval keeps exhaustive retry/cancellation tests independent of wall time.
func TestMergePRRetry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures []fakeError
		cancel   bool
		attempts int
		wantErr  string
	}{
		{"retry", []fakeError{{405, "Pull Request is not mergeable"}}, false, 2, ""},
		{"exhausted", []fakeError{{405, "not mergeable"}, {405, "not mergeable"}, {405, "not mergeable"}}, false, 3, "not mergeable"},
		{"cancel", []fakeError{{405, "not mergeable"}}, true, 1, "context canceled"},
		{"other 405", []fakeError{{405, "required review"}}, false, 1, "required review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			attempts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				if attempts <= len(tc.failures) {
					e := tc.failures[attempts-1]
					writeJSON(w, e.status, map[string]string{"message": e.message})
					if tc.cancel {
						cancel()
					}
					return
				}
				writeJSON(w, 200, map[string]string{"sha": "merged"})
			}))
			defer srv.Close()
			gh := newGitHub(Config{APIURL: srv.URL, Repo: "vic/demo"})
			var retries []int
			sha, n, err := gh.mergePRWithRetry(ctx, 101, "merge", "abc123", time.Millisecond, func(attempt int, err error) { retries = append(retries, attempt) })
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v", err)
				}
			} else if err != nil || sha != "merged" || n != 2 || !reflect.DeepEqual(retries, []int{1}) {
				t.Fatalf("sha=%s n=%d retries=%v err=%v", sha, n, retries, err)
			}
			if attempts != tc.attempts {
				t.Fatalf("attempts=%d", attempts)
			}
			if tc.cancel && !errors.Is(err, context.Canceled) {
				t.Fatal(fmt.Sprint(err))
			}
		})
	}
}

func TestPRGroupUsage(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"pr"}, {"pr", "help"}, {"pr", "-h"}, {"pr", "--help"}, {"pr", "bogus"}} {
		var out, stderr bytes.Buffer
		err := run(context.Background(), args, strings.NewReader(""), &out, &stderr)
		if len(args) == 2 && args[1] == "bogus" {
			if !errors.Is(err, errUsage) || out.Len() != 0 || !strings.Contains(stderr.String(), "usage: amf pr") {
				t.Fatalf("%v: %v out=%q stderr=%q", args, err, &out, &stderr)
			}
		} else if err != nil || !strings.Contains(out.String(), "merge <N>") || stderr.Len() != 0 {
			t.Fatalf("%v: %v out=%q stderr=%q", args, err, &out, &stderr)
		}
	}
}

func TestPRMergePreflightErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   any
		want   string
	}{
		{"missing SHA", 200, map[string]any{"state": "open", "head": map[string]string{"ref": "topic"}}, "github did not report a head SHA"},
		{"missing PR", 404, map[string]string{"message": "Not Found"}, "Not Found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					t.Errorf("unexpected write %s", r.Method)
				}
				if r.URL.Path == "/repos/vic/demo" {
					writeJSON(w, 200, map[string]string{"default_branch": "main"})
					return
				}
				writeJSON(w, tc.status, tc.body)
			}))
			defer srv.Close()
			listJSONEnv(t, srv)
			var out, stderr bytes.Buffer
			err := run(context.Background(), []string{"pr", "merge", "101"}, strings.NewReader(""), &out, &stderr)
			if err == nil || !strings.Contains(err.Error(), tc.want) || out.Len() != 0 {
				t.Fatalf("err=%v stdout=%q", err, &out)
			}
		})
	}
}

func TestDeleteBranchMissingRef(t *testing.T) {
	// A missing ref must never turn into DELETE .../refs/heads/.
	gh := newGitHub(Config{APIURL: "http://127.0.0.1:1", Repo: "vic/demo"})
	if err := gh.DeleteBranch(context.Background(), ""); err == nil || err.Error() != "github did not report a head branch" {
		t.Fatalf("err=%v", err)
	}
}

func TestPRMergeLabelsIssueDone(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	listJSONEnv(t, srv)
	gh.prs = []map[string]string{
		{"head": "amf/issue-1", "state": "open"},
		{"head": "amf/issue-2", "state": "open", "head_repo": "other/demo"},
		{"head": "feature/x", "state": "open"},
		{"head": "amf/issue-4", "state": "open"},
	}
	gh.drafts = make([]bool, len(gh.prs))
	gh.issues[1] = &Issue{Number: 1, State: "closed", Labels: []Label{{Name: labelFailed}, {Name: labelPROpen}, {Name: "bug"}}}
	gh.issues[2] = &Issue{Number: 2, State: "open", Labels: []Label{{Name: labelPROpen}}}
	gh.issues[4] = &Issue{Number: 4, State: "closed", Labels: []Label{{Name: labelDone}}}

	var out bytes.Buffer
	if err := run(context.Background(), []string{"pr", "merge", "101", "--squash"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Labeled #1 amf:done\n") {
		t.Fatalf("output %q", &out)
	}
	if got := labelNames(*gh.issues[1]); !reflect.DeepEqual(got, []string{"bug", labelDone}) {
		t.Fatalf("issue 1 labels %v", got)
	}

	// A fork's amf/issue-N branch and a non-amf branch leave issues alone.
	for _, n := range []string{"102", "103"} {
		gh.writes = nil
		if err := run(context.Background(), []string{"pr", "merge", n}, nil, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
		for _, w := range gh.writes {
			if strings.Contains(w, "/labels") {
				t.Fatalf("PR %s: unexpected label write %s", n, w)
			}
		}
	}
	if got := labelNames(*gh.issues[2]); !reflect.DeepEqual(got, []string{labelPROpen}) {
		t.Fatalf("issue 2 labels %v", got)
	}

	// An issue already amf:done costs no label writes and prints nothing.
	gh.writes, out = nil, bytes.Buffer{}
	if err := run(context.Background(), []string{"pr", "merge", "104"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Labeled") || slices.ContainsFunc(gh.writes, func(w string) bool { return strings.Contains(w, "/labels") }) {
		t.Fatalf("output %q, writes %v", &out, gh.writes)
	}
}

func shortPRWaits(t *testing.T) {
	t.Helper()
	old := []time.Duration{prUpdatePoll, prUpdateTimeout, prChecksTimeout, prChecksPoll, prNoChecksGrace}
	prUpdatePoll, prUpdateTimeout, prChecksTimeout, prChecksPoll, prNoChecksGrace = time.Millisecond, 50*time.Millisecond, 200*time.Millisecond, time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() {
		prUpdatePoll, prUpdateTimeout, prChecksTimeout, prChecksPoll, prNoChecksGrace = old[0], old[1], old[2], old[3], old[4]
	})
}

func TestPRMergeUpdateBranchAndWait(t *testing.T) {
	shortPRWaits(t)
	pass := []checkRun{{Name: "build", Status: "completed", Conclusion: "success"}}
	for _, tc := range []struct {
		name               string
		flags              []string
		behind             int
		conflict           bool
		runs               map[string][]checkRun
		want, wantErr, sha string
	}{
		{name: "behind", flags: []string{"-u"}, behind: 3, sha: "def456", want: "Updated #101 with main (was 3 commits behind)\n"},
		{name: "up to date", flags: []string{"--update-branch"}, sha: "abc123", want: "#101 is up to date with main\n"},
		{name: "conflict", flags: []string{"-u"}, behind: 1, conflict: true, wantErr: "could not update #101 with main (1 commit behind)"},
		{name: "update then checks", flags: []string{"-u", "-w"}, behind: 2, runs: map[string][]checkRun{"def456": pass}, sha: "def456", want: "Checks passed on #101 (1 check: 1 success)"},
		{name: "checks fail", flags: []string{"--wait-for-checks"}, runs: map[string][]checkRun{"abc123": {{Name: "test", Status: "completed", Conclusion: "failure"}}}, wantErr: "checks failed on #101"},
		{name: "checks pending", flags: []string{"-w"}, runs: map[string][]checkRun{"abc123": {{Name: "test", Status: "in_progress"}}}, wantErr: "timed out after"},
		{name: "no checks", flags: []string{"-w"}, sha: "abc123", want: "No checks reported on #101"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh, srv := newFakeGitHub(t)
			listJSONEnv(t, srv)
			pr := map[string]string{"head": "feature/x", "base": "main", "state": "open"}
			if tc.conflict {
				pr["update_conflict"] = "yes"
			}
			gh.prs = []map[string]string{pr}
			gh.drafts = []bool{false}
			gh.compares["main...abc123"] = [2]int{1, tc.behind}
			for k, v := range tc.runs {
				gh.checkRuns[k] = v
			}
			var out bytes.Buffer
			err := run(context.Background(), append([]string{"pr", "merge", "101"}, tc.flags...), nil, &out, io.Discard)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				if pr["state"] != "open" {
					t.Fatalf("PR state %s after a refusal", pr["state"])
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), tc.want) || pr["state"] != "merged" || pr["merge_sha"] != tc.sha {
				t.Fatalf("output %q, state %s, merged sha %q (want %s)", &out, pr["state"], pr["merge_sha"], tc.sha)
			}
		})
	}
}
