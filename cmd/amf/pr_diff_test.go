package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestPRDiff(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	// No trailing newline, on purpose: the diff must print byte for byte.
	diff := "diff --git a/a.txt b/a.txt\nindex e69de29..d95f3f1 100644\n--- a/a.txt\n+++ b/a.txt\n@@ -0,0 +1 @@\n+hello"
	gh.diffs[101] = diff
	listJSONEnv(t, srv)
	for _, n := range []string{"101", "#101"} {
		var out bytes.Buffer
		if err := run(context.Background(), []string{"pr", "diff", n}, nil, &out, io.Discard); err != nil {
			t.Fatalf("pr diff %s: %v", n, err)
		}
		if out.String() != diff {
			t.Fatalf("pr diff %s =\n%q\nwant\n%q", n, out.String(), diff)
		}
	}
	if len(gh.diffAccepts) != 2 || gh.diffAccepts[0] != "application/vnd.github.diff" || gh.diffAccepts[1] != "application/vnd.github.diff" {
		t.Fatalf("diff Accept headers = %v", gh.diffAccepts)
	}
}

func TestPRDiffNameOnly(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	var files []prFile
	var want []string
	for i := range 205 {
		name := fmt.Sprintf("pkg/file%03d.go", i)
		files = append(files, prFile{Filename: name})
		want = append(want, name)
	}
	// A rename reports its new name (filename), not the old one.
	files = append(files, prFile{Filename: "pkg/renamed.go"})
	want = append(want, "pkg/renamed.go")
	gh.files[101] = files
	listJSONEnv(t, srv)

	var out bytes.Buffer
	if err := run(context.Background(), []string{"pr", "diff", "101", "--name-only"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if !slices.Equal(got, want) {
		t.Fatalf("got %d lines, want %d (first mismatch %q vs %q)", len(got), len(want), got, want)
	}
	if len(gh.fileAccepts) != 3 {
		t.Fatalf("expected 3 files pages, got %d", len(gh.fileAccepts))
	}
	for _, a := range gh.fileAccepts {
		if a != "application/vnd.github+json" {
			t.Fatalf("files request Accept = %q, want the JSON media type", a)
		}
	}
}

func TestPRDiffTooLarge(t *testing.T) {
	for _, status := range []int{http.StatusNotAcceptable, http.StatusUnprocessableEntity} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/vic/demo" {
					writeJSON(w, 200, map[string]any{"default_branch": "main", "permissions": map[string]bool{"push": true}})
					return
				}
				if r.Header.Get("Accept") != "application/vnd.github.diff" {
					t.Errorf("Accept = %q, want the diff media type", r.Header.Get("Accept"))
				}
				writeJSON(w, status, map[string]string{"message": "The diff is too large"})
			}))
			t.Cleanup(srv.Close)
			listJSONEnv(t, srv)
			var out bytes.Buffer
			err := run(context.Background(), []string{"pr", "diff", "101"}, nil, &out, io.Discard)
			want := "the diff of #101 is too large for GitHub to send; try --name-only, or git fetch origin pull/101/head"
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if out.Len() != 0 {
				t.Fatalf("stdout = %q", &out)
			}
		})
	}
}

func TestPRDiffNotFound(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	gh.issues[7] = &Issue{Number: 7, Title: "Issue, not a PR", State: "open"}
	listJSONEnv(t, srv)
	for _, args := range [][]string{{"pr", "diff", "7"}, {"pr", "diff", "7", "--name-only"}} {
		var out, stderr bytes.Buffer
		err := run(context.Background(), args, nil, &out, &stderr)
		want := "#7 is not a pull request in vic/demo"
		if err == nil || err.Error() != want {
			t.Fatalf("%v: err=%v, want %q", args, err, want)
		}
		if out.Len() != 0 {
			t.Fatalf("%v: stdout %q", args, &out)
		}
	}
}

func TestPRDiffBadArgs(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{
		{"pr", "diff"},
		{"pr", "diff", "--name-only"},
		{"pr", "diff", "1", "2"},
		{"pr", "diff", "abc"},
		{"pr", "diff", "#abc"},
		{"pr", "diff", "0"},
		{"pr", "diff", "#0"},
	} {
		var out, stderr bytes.Buffer
		err := run(context.Background(), args, nil, &out, &stderr)
		if !errors.Is(err, errUsage) {
			t.Errorf("%v: %v", args, err)
		}
		if out.Len() != 0 {
			t.Errorf("%v: stdout %q", args, &out)
		}
		if !strings.Contains(stderr.String(), "usage: amf pr diff <N> [--name-only]") {
			t.Errorf("%v: stderr %q", args, &stderr)
		}
	}
	// An unknown flag is the flag package's own error, still exit 2.
	var out bytes.Buffer
	if err := run(context.Background(), []string{"pr", "diff", "1", "--unknown"}, nil, &out, io.Discard); !errors.Is(err, errUsage) || out.Len() != 0 {
		t.Errorf("pr diff 1 --unknown: err=%v stdout=%q", err, &out)
	}
}

func TestPRDiffWriteError(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	gh.diffs[101] = "diff body"
	gh.files[101] = []prFile{{Filename: "a.txt"}}
	listJSONEnv(t, srv)
	for _, args := range [][]string{{"pr", "diff", "101"}, {"pr", "diff", "101", "--name-only"}} {
		if err := run(context.Background(), args, nil, failWriter{}, io.Discard); err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Errorf("%v: err = %v, want disk full", args, err)
		}
	}
}
