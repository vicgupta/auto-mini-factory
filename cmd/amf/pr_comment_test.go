package main

import (
	"bytes"
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestPRComment(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		stdin    string
		file     string
		wantBody []string
	}{
		{name: "body", args: []string{"101", "--body", "Looks good to me"}, wantBody: []string{"Looks good to me"}},
		{name: "body short", args: []string{"#101", "-b", "Ship it"}, wantBody: []string{"Ship it"}},
		{name: "file", args: []string{"101", "--file", "review.md"}, file: "Line one\nLine two\n", wantBody: []string{"Line one\nLine two\n"}},
		{name: "file short", args: []string{"101", "-f", "review.md"}, file: "Short file\n", wantBody: []string{"Short file\n"}},
		{name: "stdin", args: []string{"101", "--file", "-"}, stdin: "From stdin\n", wantBody: []string{"From stdin\n"}},
		{name: "redacted", args: []string{"101", "--body", "leaked test-token secret"}, wantBody: []string{"leaked [REDACTED:config] secret"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh, srv := newFakeGitHub(t)
			gh.prs = []map[string]string{{"head": "feature/topic", "base": "main", "state": "open"}}
			listJSONEnv(t, srv)
			if tc.file != "" {
				if err := os.WriteFile("review.md", []byte(tc.file), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var out, stderr bytes.Buffer
			err := run(context.Background(), append([]string{"pr", "comment"}, tc.args...), strings.NewReader(tc.stdin), &out, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			wantURL := "https://github.com/vic/demo/pull/101#issuecomment-1\n"
			if out.String() != wantURL {
				t.Fatalf("out = %q, want %q", out.String(), wantURL)
			}
			if !reflect.DeepEqual(gh.comments[101], tc.wantBody) {
				t.Fatalf("comments = %q, want %q", gh.comments[101], tc.wantBody)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestPRCommentRejectsBadBody(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	gh.prs = []map[string]string{{"head": "feature/topic", "base": "main", "state": "open"}}
	listJSONEnv(t, srv)
	if err := os.WriteFile("empty.md", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		args  []string
		stdin string
		want  string
	}{
		{name: "missing", args: []string{"101"}, want: "use --body or --file"},
		{name: "both", args: []string{"101", "--body", "text", "--file", "empty.md"}, want: "use --body or --file, not both"},
		{name: "empty body", args: []string{"101", "--body", ""}, want: "comment body can't be empty"},
		{name: "whitespace body", args: []string{"101", "--body", " \n\t"}, want: "comment body can't be empty"},
		{name: "empty file", args: []string{"101", "--file", "empty.md"}, want: "comment body can't be empty"},
		{name: "empty stdin", args: []string{"101", "--file", "-"}, want: "comment body can't be empty"},
		{name: "missing file", args: []string{"101", "--file", "missing.md"}, want: "missing.md"},
		{name: "invalid number", args: []string{"0", "--body", "text"}, want: "invalid pull request number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			err := run(context.Background(), append([]string{"pr", "comment"}, tc.args...), strings.NewReader(tc.stdin), &out, &stderr)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if out.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", &out)
			}
			if len(gh.comments[101]) != 0 {
				t.Fatalf("posted comment despite error: %q", gh.comments[101])
			}
		})
	}
}

func TestPRCommentUsage(t *testing.T) {
	t.Chdir(t.TempDir())
	var out, stderr bytes.Buffer
	err := run(context.Background(), []string{"pr", "comment"}, nil, &out, &stderr)
	if err == nil || !strings.Contains(err.Error(), "usage") || !strings.Contains(stderr.String(), "amf pr comment") {
		t.Fatalf("err = %v, stdout = %q, stderr = %q", err, &out, &stderr)
	}
}
