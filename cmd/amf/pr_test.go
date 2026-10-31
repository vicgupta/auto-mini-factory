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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestPRGroup(t *testing.T) {
	for _, args := range [][]string{{"pr"}, {"pr", "help"}, {"pr", "-h"}, {"pr", "--help"}, {"pr", "bogus"}} {
		var out, stderr bytes.Buffer
		err := run(context.Background(), args, nil, &out, &stderr)
		if args[len(args)-1] == "bogus" {
			if !errors.Is(err, errUsage) || out.Len() != 0 || !strings.Contains(stderr.String(), "usage: amf pr") {
				t.Fatalf("%v: %v, stdout %q, stderr %q", args, err, &out, &stderr)
			}
		} else if err != nil || !strings.Contains(out.String(), "usage: amf pr") || stderr.Len() != 0 {
			t.Fatalf("%v: %v, stdout %q, stderr %q", args, err, &out, &stderr)
		}
	}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"help"}, nil, &out, io.Discard); err != nil || !strings.Contains(out.String(), "amf pr") {
		t.Fatalf("help: %v, %q", err, &out)
	}
}

func TestPRList(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	gh.prs = []map[string]string{
		{"head": "amf/issue-12", "base": "main", "state": "closed", "title": "Merged", "merged_at": "2026-10-25T00:00:00Z"},
		{"head": "other", "base": "main", "state": "closed", "title": "Closed"},
		{"head": "amf/issue-12", "base": "main", "state": "open", "title": "Open\t<&>"},
		{"head": "amf/issue-13", "base": "main", "state": "open", "title": "Fork", "head_repo": "other/demo"},
	}
	gh.drafts = []bool{false, true, false, true}
	listJSONEnv(t, srv)
	for _, tc := range []struct {
		name    string
		state   string
		numbers []int
		states  []string
	}{
		{"open", "open", []int{104, 103}, []string{"draft", "open"}},
		{"padded open", " open ", []int{104, 103}, []string{"draft", "open"}},
		{"closed", "closed", []int{102}, []string{"closed"}},
		{"merged", "merged", []int{101}, []string{"merged"}},
		{"all", "all", []int{104, 103, 102, 101}, []string{"draft", "open", "closed", "merged"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(context.Background(), []string{"pr", "list", "--json", "--state", tc.state}, nil, &out, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			var rows []map[string]any
			if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
				t.Fatal(err)
			}
			if len(rows) != len(tc.numbers) {
				t.Fatalf("rows = %s", &out)
			}
			for i, row := range rows {
				n := tc.numbers[i]
				if len(row) != 8 || row["number"] != float64(n) || row["state"] != tc.states[i] || row["draft"] != gh.drafts[n-101] || row["url"] != fmt.Sprintf("https://github.com/vic/demo/pull/%d", n) || row["head"] != gh.prs[n-101]["head"] || row["base"] != "main" || row["title"] != gh.prs[n-101]["title"] {
					t.Errorf("row = %#v", row)
				}
				var issue any
				if n == 101 || n == 103 {
					issue = float64(12)
				}
				if row["issue"] != issue {
					t.Errorf("issue = %#v, want %#v", row["issue"], issue)
				}
			}
		})
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "PR    ISSUE  STATE  TITLE\n#104  -      draft  Fork\n#103  #12    open   Open <&>\n"},
		{[]string{"--limit", "1"}, "PR    ISSUE  STATE  TITLE\n#104  -      draft  Fork\n"},
		{[]string{"--limit", "0", "--json=false"}, "PR    ISSUE  STATE  TITLE\n#104  -      draft  Fork\n#103  #12    open   Open <&>\n"},
	} {
		var out bytes.Buffer
		if err := run(context.Background(), append([]string{"pr", "list"}, tc.args...), nil, &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		if out.String() != tc.want {
			t.Errorf("%v: got %q, want %q", tc.args, out.String(), tc.want)
		}
	}
}

func TestPRListIssueFilter(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	gh.prs = []map[string]string{
		{"head": "amf/issue-12", "base": "main", "state": "closed", "title": "Merged", "merged_at": "2026-10-25T00:00:00Z"},
		{"head": "feature", "base": "main", "state": "open", "title": "Feature"},
		{"head": "amf/issue-12", "base": "main", "state": "open", "title": "Open fix"},
		{"head": "amf/issue-12", "base": "main", "state": "open", "title": "Fork fix", "head_repo": "other/demo"},
		{"head": "amf/issue-13", "base": "main", "state": "open", "title": "Other issue"},
	}
	listJSONEnv(t, srv)

	// --issue keeps only the amf/issue-N branches in this repository (the
	// fork branch's issue is nil), newest first, before --limit applies.
	for _, tc := range []struct {
		name    string
		args    []string
		numbers []int
	}{
		{"issue 12 all", []string{"--issue", "12", "--state", "all"}, []int{103, 101}},
		{"issue 12 open", []string{"--issue", "12"}, []int{103}},
		{"issue 12 limit 1", []string{"--issue", "12", "--state", "all", "--limit", "1"}, []int{103}},
		{"issue 13 all", []string{"--issue", "13", "--state", "all"}, []int{105}},
		{"no match", []string{"--issue", "99", "--state", "all"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(context.Background(), append([]string{"pr", "list", "--json"}, tc.args...), nil, &out, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			var rows []struct {
				Number int
				Issue  *int
			}
			if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
				t.Fatal(err)
			}
			if len(rows) != len(tc.numbers) {
				t.Fatalf("rows = %s", &out)
			}
			for i, row := range rows {
				if row.Number != tc.numbers[i] {
					t.Errorf("row %d number = %d, want %d", i, row.Number, tc.numbers[i])
				}
				if row.Issue == nil {
					t.Errorf("row %d issue = nil", i)
				}
			}
		})
	}

	// Filtered table output.
	var out bytes.Buffer
	if err := run(context.Background(), []string{"pr", "list", "--issue", "12"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if want := "PR    ISSUE  STATE  TITLE\n#103  #12    open   Open fix\n"; out.String() != want {
		t.Errorf("table got %q, want %q", out.String(), want)
	}

	// Empty-match messages drop the state word for --state all, as the
	// unfiltered empty message does; --json prints [].
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--issue", "99"}, "No open pull requests for issue #99 in vic/demo.\n"},
		{[]string{"--issue", "99", "--state", "all"}, "No pull requests for issue #99 in vic/demo.\n"},
		{[]string{"--issue", "99", "--json"}, "[]\n"},
	} {
		var out bytes.Buffer
		if err := run(context.Background(), append([]string{"pr", "list"}, tc.args...), nil, &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		if out.String() != tc.want {
			t.Errorf("%v: got %q, want %q", tc.args, out.String(), tc.want)
		}
	}
}

func TestPRListIssueNegativeMakesNoGitHubCall(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	t.Cleanup(srv.Close)
	listJSONEnv(t, srv)
	var out bytes.Buffer
	err := run(context.Background(), []string{"pr", "list", "--issue", "-1"}, nil, &out, io.Discard)
	if err == nil || err.Error() != "--issue must be 0 or more, got -1" || out.Len() != 0 {
		t.Fatalf("err %v, stdout %q", err, &out)
	}
	if calls.Load() != 0 {
		t.Fatalf("made %d GitHub calls", calls.Load())
	}
}

func TestPRListPaginationAndLimit(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	for i := 0; i < 205; i++ {
		gh.prs = append(gh.prs, map[string]string{"head": "feature", "base": "main", "title": fmt.Sprint(i), "state": "closed"})
		gh.drafts = append(gh.drafts, false)
	}
	gh.prs[0]["merged_at"] = "2026-10-25T00:00:00Z"
	listJSONEnv(t, srv)
	for _, tc := range []struct {
		args []string
		want []int
	}{
		{[]string{"--state", "merged", "--limit", "1"}, []int{101}},
		{[]string{"--state", "all", "--limit", "2"}, []int{305, 304}},
		{[]string{"--state", "all"}, nil},
	} {
		var out bytes.Buffer
		if err := run(context.Background(), append([]string{"pr", "list", "--json"}, tc.args...), nil, &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		var rows []struct{ Number int }
		if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
			t.Fatal(err)
		}
		var numbers []int
		for _, row := range rows {
			numbers = append(numbers, row.Number)
		}
		if tc.want != nil {
			if !slices.Equal(numbers, tc.want) {
				t.Fatalf("numbers = %v, want %v", numbers, tc.want)
			}
		} else {
			if len(numbers) != 205 {
				t.Fatalf("got %d rows", len(numbers))
			}
			for i, n := range numbers {
				if n != 305-i {
					t.Fatalf("numbers[%d] = %d", i, n)
				}
			}
		}
	}
}

func TestPRListEmptyAndErrors(t *testing.T) {
	_, srv := newFakeGitHub(t)
	listJSONEnv(t, srv)
	for _, state := range []string{"open", "closed", "merged", "all"} {
		for _, asJSON := range []bool{false, true} {
			var out bytes.Buffer
			if err := run(context.Background(), []string{"pr", "list", "--state", state, fmt.Sprintf("--json=%t", asJSON)}, nil, &out, io.Discard); err != nil {
				t.Fatal(err)
			}
			want := "No " + state + " pull requests in vic/demo.\n"
			if state == "all" {
				want = "No pull requests in vic/demo.\n"
			}
			if asJSON {
				want = "[]\n"
			}
			if out.String() != want {
				t.Errorf("got %q, want %q", out.String(), want)
			}
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--state", "bad"}, `--state must be open, closed, merged or all, got "bad"`},
		{[]string{"--limit", "-1"}, "--limit must be 0 or more, got -1"},
		{[]string{"--issue", "-1"}, "--issue must be 0 or more, got -1"},
		{[]string{"--limit", "bad"}, "usage"},
		{[]string{"--unknown"}, "usage"},
		{[]string{"extra"}, "usage"},
	} {
		var out bytes.Buffer
		err := run(context.Background(), append([]string{"pr", "list", "--json"}, tc.args...), nil, &out, io.Discard)
		if err == nil || err.Error() != tc.want || out.Len() != 0 {
			t.Errorf("%v: err %v, stdout %q", tc.args, err, &out)
		}
	}
	if err := run(context.Background(), []string{"pr", "list", "--json"}, nil, failWriter{}, io.Discard); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("write error = %v", err)
	}
}

func TestPRListAPIError(t *testing.T) {
	for _, failure := range []string{"repo", "second page"} {
		t.Run(failure, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/repos/vic/demo" && failure != "repo" {
					writeJSON(w, 200, map[string]any{"default_branch": "main", "permissions": map[string]bool{"push": true}})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/pulls") && r.URL.Query().Get("page") == "" {
					w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/vic/demo/pulls?page=2>; rel="next"`, r.Host))
					writeJSON(w, 200, []map[string]any{{"number": 1, "state": "open"}})
					return
				}
				writeJSON(w, 502, map[string]string{"message": "Bad Gateway"})
			}))
			t.Cleanup(srv.Close)
			listJSONEnv(t, srv)
			var out bytes.Buffer
			err := run(context.Background(), []string{"pr", "list", "--json"}, nil, &out, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "502") || out.Len() != 0 {
				t.Fatalf("err %v, stdout %q", err, &out)
			}
		})
	}
}

func TestParsePRNumber(t *testing.T) {
	for _, s := range []string{"42", "#42", " 42", "42 ", "\t42\t", " #42", "#42 "} {
		if n, err := parsePRNumber(s); err != nil || n != 42 {
			t.Errorf("%q: %d, %v", s, n, err)
		}
	}
	for _, s := range []string{"", "#", "0", "-1", "##42", "abc", "999999999999999999999999999999999"} {
		if _, err := parsePRNumber(s); err == nil {
			t.Errorf("%q should fail", s)
		}
	}
}

func TestPRListOpenPagesIncludeDrafts(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	for i := 0; i < 205; i++ {
		gh.prs = append(gh.prs, map[string]string{"head": "feature", "base": "main", "state": "open"})
		gh.drafts = append(gh.drafts, i%2 == 0)
	}
	listJSONEnv(t, srv)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"pr", "list", "--json"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Number int
		State  string
		Draft  bool
	}
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 205 {
		t.Fatalf("got %d PRs, want 205", len(rows))
	}
	for i, row := range rows {
		n := 305 - i
		state := "open"
		draft := (n-101)%2 == 0
		if draft {
			state = "draft"
		}
		if row.Number != n || row.State != state || row.Draft != draft {
			t.Errorf("row %d = %+v", i, row)
		}
	}
}
