package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// sweepFake is a GitHub with a handful of open PRs in different states.
type sweepFake struct {
	mu       sync.Mutex
	t        *testing.T
	writes   []string
	comments map[int][]string
	heads    map[int]string
	merged   map[int]bool
}

func newSweepFake(t *testing.T) (*sweepFake, *httptest.Server) {
	f := &sweepFake{t: t, comments: map[int][]string{}, merged: map[int]bool{},
		heads: map[int]string{201: "sha201", 202: "sha202", 203: "sha203", 204: "sha204", 208: "sha208"}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

var sweepPRPath = regexp.MustCompile(`^/repos/vic/demo/pulls/(\d+)(/.*)?$`)

func (f *sweepFake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	if r.Method != "GET" {
		f.writes = append(f.writes, r.Method+" "+p)
	}
	pr := func(n int, ref string, draft bool) map[string]any {
		return map[string]any{"number": n, "html_url": fmt.Sprintf("https://github.com/vic/demo/pull/%d", n), "state": "open", "draft": draft,
			"merged": f.merged[n], "base": map[string]any{"ref": "main"},
			"head": map[string]any{"ref": ref, "sha": f.heads[n], "repo": map[string]string{"full_name": "vic/demo"}}}
	}
	refs := map[int]string{201: "amf/issue-1", 202: "amf/issue-2", 203: "amf/issue-3", 204: "amf/issue-4", 205: "feature/x", 206: "amf/issue-6", 207: "amf/issue-7", 208: "amf/issue-8"}
	switch {
	case p == "/repos/vic/demo":
		writeJSON(w, 200, map[string]any{"default_branch": "main", "permissions": map[string]bool{"push": true}})
	case p == "/repos/vic/demo/issues" && r.Method == "GET":
		if r.URL.Query().Get("labels") != labelPROpen {
			f.t.Errorf("issues query %v", r.URL.RawQuery)
		}
		var out []map[string]any
		for _, n := range []int{1, 2, 3, 4, 6, 8} {
			out = append(out, map[string]any{"number": n, "state": "open", "labels": []map[string]string{{"name": labelPROpen}}})
		}
		writeJSON(w, 200, out)
	case p == "/repos/vic/demo/pulls" && r.Method == "GET":
		var out []map[string]any
		for n := 208; n >= 201; n-- {
			out = append(out, pr(n, refs[n], n == 206))
		}
		writeJSON(w, 200, out)
	case strings.HasPrefix(p, "/repos/vic/demo/compare/"):
		behind := 0
		if strings.HasSuffix(p, "...sha202") {
			behind = 3
		}
		writeJSON(w, 200, map[string]int{"ahead_by": 1, "behind_by": behind})
	case strings.HasPrefix(p, "/repos/vic/demo/commits/"):
		run := map[string]string{"name": "test", "status": "completed", "conclusion": "success"}
		switch {
		case strings.Contains(p, "sha203"):
			run["conclusion"] = "failure"
		case strings.Contains(p, "sha204"):
			run = map[string]string{"name": "test", "status": "in_progress"}
		}
		writeJSON(w, 200, map[string]any{"check_runs": []any{run}})
	case strings.HasPrefix(p, "/repos/vic/demo/issues/") && strings.HasSuffix(p, "/comments"):
		var n int
		fmt.Sscanf(p, "/repos/vic/demo/issues/%d/comments", &n)
		if r.Method == "POST" {
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			f.comments[n] = append(f.comments[n], body["body"])
			writeJSON(w, 201, map[string]any{})
			return
		}
		var out []map[string]string
		for _, b := range f.comments[n] {
			out = append(out, map[string]string{"body": b})
		}
		writeJSON(w, 200, out)
	case strings.HasPrefix(p, "/repos/vic/demo/issues/"):
		if r.Method == "GET" {
			var n int
			fmt.Sscanf(p, "/repos/vic/demo/issues/%d", &n)
			writeJSON(w, 200, map[string]any{"number": n, "state": "open", "labels": []map[string]string{{"name": labelPROpen}}})
			return
		}
		writeJSON(w, 200, []any{})
	case sweepPRPath.MatchString(p):
		m := sweepPRPath.FindStringSubmatch(p)
		var n int
		fmt.Sscan(m[1], &n)
		switch m[2] {
		case "":
			writeJSON(w, 200, pr(n, refs[n], n == 206))
		case "/files":
			files := []map[string]string{{"filename": "main.go"}}
			if n == 208 {
				files = append(files, map[string]string{"filename": "AGENTS.md"})
			}
			writeJSON(w, 200, files)
		case "/update-branch":
			f.heads[n] = "sha202b"
			writeJSON(w, 202, map[string]string{"message": "Updating pull request branch."})
		case "/merge":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["sha"] != f.heads[n] || body["merge_method"] != "squash" {
				f.t.Errorf("merge body %v", body)
			}
			f.merged[n] = true
			writeJSON(w, 200, map[string]any{"merged": true, "sha": "mergedsha"})
		default:
			http.NotFound(w, r)
		}
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}
}

func TestPRSweep(t *testing.T) {
	f, srv := newSweepFake(t)
	listJSONEnv(t, srv)
	var out, stderr bytes.Buffer
	if err := run(context.Background(), []string{"pr", "sweep", "-m", "squash", "-j"}, strings.NewReader(""), &out, &stderr); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	var res []sweptPR
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	got := map[int]string{}
	for _, s := range res {
		got[s.Number] = s.Action
	}
	want := map[int]string{201: "merged", 202: "updated", 203: "blocked", 204: "waiting", 208: "blocked"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("actions = %v, want %v (%s)", got, want, out.String())
	}
	if res[0].Number != 201 || res[0].Commit != "mergedsha" || res[0].Issue != 1 {
		t.Errorf("first = %+v", res[0])
	}
	if len(f.comments[203]) != 1 || !strings.Contains(f.comments[203][0], "checks failed") || len(f.comments[208]) != 1 {
		t.Errorf("comments = %v", f.comments)
	}
	if len(f.comments[201])+len(f.comments[202])+len(f.comments[204]) != 0 {
		t.Errorf("unexpected comments = %v", f.comments)
	}
	// Issue 1 is labeled done.
	if !strings.Contains(strings.Join(f.writes, "\n"), "POST /repos/vic/demo/issues/1/labels") {
		t.Errorf("issue 1 not labeled: %v", f.writes)
	}
	// A second sweep doesn't repeat a comment for the same head and reason.
	out.Reset()
	if err := run(context.Background(), []string{"pr", "sweep", "--merge=squash"}, strings.NewReader(""), &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if len(f.comments[203]) != 1 || len(f.comments[208]) != 1 {
		t.Errorf("comments repeated: %v", f.comments)
	}
	if !strings.Contains(out.String(), "#204 (issue #4) waiting: checks pending") {
		t.Errorf("text output = %s", out.String())
	}
}

func TestPRSweepDryRunWrites(t *testing.T) {
	f, srv := newSweepFake(t)
	listJSONEnv(t, srv)
	var out, stderr bytes.Buffer
	if err := run(context.Background(), []string{"pr", "sweep", "-n", "-m", "squash"}, strings.NewReader(""), &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 0 {
		t.Fatalf("dry run wrote: %v", f.writes)
	}
	for _, s := range []string{"#201 (issue #1) would-merge", "#202 (issue #2) would-update: 3 commits behind main", "#203 (issue #3) blocked: checks failed"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("missing %q in %s", s, out.String())
		}
	}
}

func TestPRSweepWithoutMerge(t *testing.T) {
	f, srv := newSweepFake(t)
	listJSONEnv(t, srv)
	var out, stderr bytes.Buffer
	if err := run(context.Background(), []string{"pr", "sweep"}, strings.NewReader(""), &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if f.merged[201] || !strings.Contains(out.String(), "#201 (issue #1) ready") {
		t.Fatalf("merged without --merge: %s", out.String())
	}
}

func TestPRSweepRespectsLock(t *testing.T) {
	_, srv := newSweepFake(t)
	listJSONEnv(t, srv)
	dir, _ := os.Getwd()
	if _, err := git(context.Background(), dir, "init", "-q"); err != nil {
		t.Skip(err)
	}
	root, err := git(context.Background(), dir, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	l, err := acquireLock(root, "watch")
	if err != nil {
		t.Fatal(err)
	}
	defer l.release()
	var out, stderr bytes.Buffer
	err = run(context.Background(), []string{"pr", "sweep", "-m"}, strings.NewReader(""), &out, &stderr)
	if err == nil {
		t.Fatalf("sweep ran under a held lock: %s", out.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".amf", "watch.lock")); err != nil {
		t.Fatal(err)
	}
}

func TestPRSweepBadArgs(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"pr", "sweep", "1"}, {"pr", "sweep", "--merge=fast"}, {"pr", "sweep", "--unknown"}} {
		var out, stderr bytes.Buffer
		if err := run(context.Background(), args, strings.NewReader(""), &out, &stderr); err == nil || strings.Contains(err.Error(), "GITHUB_TOKEN") {
			t.Errorf("%v: %v", args, err)
		}
	}
}
