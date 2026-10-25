package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
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
