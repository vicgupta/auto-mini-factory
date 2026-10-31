package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseDotEnv(t *testing.T) {
	in := `
# comment
GITHUB_TOKEN=ghp_abc
export GITHUB_REPO = "vic/demo"
SINGLE='a b'
TRAILING=x # note
EMPTY=
`
	got, err := parseDotEnv(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"GITHUB_TOKEN": "ghp_abc",
		"GITHUB_REPO":  "vic/demo",
		"SINGLE":       "a b",
		"TRAILING":     "x",
		"EMPTY":        "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if _, err := parseDotEnv(strings.NewReader("NOEQUALS")); err == nil {
		t.Fatal("expected error for line without =")
	}
}

func TestQuoteEnv(t *testing.T) {
	for _, v := range []string{
		"",
		"plain",
		"/opt/my claude",
		"tab\there",
		"a#b",
		"x # note",
		`say "hi"`,
		"it's",
		`both "double" and 'single'`,
	} {
		got, err := parseDotEnv(strings.NewReader("K=" + quoteEnv(v)))
		if err != nil {
			t.Fatalf("quoteEnv(%q): %v", v, err)
		}
		if got["K"] != v {
			t.Errorf("quoteEnv(%q) = %q, parsed back as %q", v, quoteEnv(v), got["K"])
		}
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	env := filepath.Join(dir, ".env")
	os.WriteFile(env, []byte("GITHUB_TOKEN=from-file\nGITHUB_REPO=https://github.com/vic/demo.git\nCLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat-x\n"), 0o600)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_REPO", "")
	t.Setenv("AMF_API_URL", "")
	t.Setenv("AMF_CLAUDE", "") // checks the default

	cfg, err := loadConfig(filepath.Join(dir, "missing"), env)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Token != "from-file" || cfg.Repo != "vic/demo" || cfg.Claude != "claude" || cfg.APIURL != "https://api.github.com" || cfg.ClaudeToken != "sk-ant-oat-x" {
		t.Fatalf("unexpected config %+v", cfg)
	}

	t.Setenv("GITHUB_TOKEN", "from-env")
	if cfg, _ := loadConfig(env); cfg.Token != "from-env" {
		t.Fatalf("environment should override .env, got %q", cfg.Token)
	}

	t.Setenv("GITHUB_REPO", "not-a-repo")
	if _, err := loadConfig(env); err == nil {
		t.Fatal("expected error for malformed GITHUB_REPO")
	}
}

func TestNormalizeRepo(t *testing.T) {
	for _, in := range []string{
		"vic/demo",
		"vic/demo/",
		"https://github.com/vic/demo",
		"https://github.com/vic/demo/",
		"https://github.com/vic/demo.git",
		"git@github.com:vic/demo.git",
	} {
		if got := normalizeRepo(in); got != "vic/demo" {
			t.Errorf("normalizeRepo(%q) = %q, want %q", in, got, "vic/demo")
		}
	}
}

func TestParseArgs(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	b := fs.String("b", "", "")
	pos, err := parseArgs(fs, []string{"Title here", "-b", "body"})
	if err != nil || *b != "body" || !reflect.DeepEqual(pos, []string{"Title here"}) {
		t.Fatalf("pos=%v b=%q err=%v", pos, *b, err)
	}
}

func TestParseClaudeResult(t *testing.T) {
	got, err := parseClaudeResult([]byte(`{"type":"result","subtype":"success","is_error":false,"result":"Did it"}`))
	if err != nil || got != "Did it" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := parseClaudeResult([]byte(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"x"}`)); err == nil {
		t.Fatal("expected error result to fail")
	}
	// Newer Claude Code versions print an array of messages ending in the result.
	got, err = parseClaudeResult([]byte(`[{"type":"system","subtype":"init"},{"type":"assistant"},{"type":"result","subtype":"success","is_error":false,"result":"From array"}]`))
	if err != nil || got != "From array" {
		t.Fatalf("array: got %q, %v", got, err)
	}
	if _, err := parseClaudeResult([]byte(`[{"type":"result","subtype":"error_during_execution","is_error":true,"result":"boom"}]`)); err == nil {
		t.Fatal("expected error result in array to fail")
	}
	if _, err := parseClaudeResult([]byte(`[{"type":"system"}]`)); err == nil {
		t.Fatal("expected array without a result to fail")
	}
	// stream-json: one message per line.
	got, err = parseClaudeResult([]byte("{\"type\":\"system\",\"subtype\":\"init\"}\n{\"type\":\"assistant\"}\n{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"From stream\"}\n"))
	if err != nil || got != "From stream" {
		t.Fatalf("stream-json: got %q, %v", got, err)
	}
	if _, err := parseClaudeResult([]byte("{\"type\":\"system\"}\n{\"type\":\"assistant\"}\n")); err == nil {
		t.Fatal("expected stream without a result to fail")
	}
	if got, _ := parseClaudeResult([]byte("plain text\n")); got != "plain text" {
		t.Fatalf("plain text: got %q", got)
	}
}

func TestNeedsWork(t *testing.T) {
	issue := func(labels ...string) Issue {
		is := Issue{State: "open"}
		for _, l := range labels {
			is.Labels = append(is.Labels, Label{Name: l})
		}
		return is
	}
	for _, tc := range []struct {
		labels []string
		want   bool
	}{
		{[]string{"amf:ready"}, true},
		{[]string{"amf:ready", "amf:failed"}, true}, // retry
		{[]string{"amf:ready", "amf:done"}, true},   // retry
		{[]string{"amf:ready", "amf:in-progress"}, false},
		{[]string{"custom"}, true}, // watch --label custom
		{[]string{"custom", "amf:done"}, false},
		{[]string{"custom", "amf:failed"}, false},
	} {
		if got := needsWork(issue(tc.labels...)); got != tc.want {
			t.Errorf("needsWork(%v) = %v, want %v", tc.labels, got, tc.want)
		}
	}
	closed := issue("amf:ready")
	closed.State = "closed"
	if needsWork(closed) {
		t.Error("closed issues should be skipped")
	}
}

// fakeGitHub is an in-memory stand-in for the parts of the GitHub API amf uses.
type fakeGitHub struct {
	t           *testing.T
	mu          sync.Mutex
	issues      map[int]*Issue
	comments    map[int][]string
	prs         []map[string]string    // each PR's string fields, and its state
	drafts      []bool                 // whether prs[i] is a draft
	views       map[int]prViewResponse // GET /pulls/{n} responses
	viewGets    int                    // how many GET /pulls/{n} requests arrived
	diffs       map[int]string         // GET /pulls/{n} diff bodies (Accept: application/vnd.github.diff)
	files       map[int][]prFile       // GET /pulls/{n}/files responses
	diffAccepts []string               // Accept header of each diff request, in order
	fileAccepts []string               // Accept header of each files request, in order
	creates     []map[string]any       // every POST /pulls body, as sent
	updates     []map[string]any       // every PATCH /pulls/{n} body, as sent
	deletes     []string               // every DELETE /git/refs/heads/{branch}, decoded
	repoLabels  map[string]LabelSpec   // the repository's labels
	// graphqlCalls are every POST /graphql body, as sent; graphqlErr, when
	// set, is the message POST /graphql returns in an errors array instead
	// of applying the mutation.
	graphqlCalls []map[string]any
	graphqlErr   string
	// mergeErrs are the errors the next merges get, one each, before
	// merges succeed again.
	mergeErrs []fakeError
	// writes is every request that isn't a GET, as "METHOD path".
	writes []string
}

type fakeError struct {
	status  int
	message string
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *httptest.Server) {
	f := &fakeGitHub{t: t, issues: map[int]*Issue{}, comments: map[int][]string{}, repoLabels: map[string]LabelSpec{}, views: map[int]prViewResponse{}, diffs: map[int]string{}, files: map[int][]prFile{}}
	mux := http.NewServeMux()
	const repo = "/repos/vic/demo"
	mux.HandleFunc("GET "+repo, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"default_branch": "main", "permissions": map[string]bool{"push": true}})
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"login": "vic"})
	})
	mux.HandleFunc("POST "+repo+"/issues", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Title, Body string
			Labels      []string
		}
		decode(t, r, &in)
		f.mu.Lock()
		defer f.mu.Unlock()
		n := len(f.issues) + 1
		is := &Issue{Number: n, Title: in.Title, Body: in.Body, State: "open", HTMLURL: fmt.Sprintf("https://github.com/vic/demo/issues/%d", n)}
		for _, l := range in.Labels {
			is.Labels = append(is.Labels, Label{Name: l})
		}
		f.issues[n] = is
		writeJSON(w, 201, is)
	})
	mux.HandleFunc("GET "+repo+"/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []Issue
		for n := 1; n <= len(f.issues); n++ {
			is := f.issues[n]
			if (q.Get("state") == "all" || is.State == q.Get("state")) && (q.Get("labels") == "" || is.HasLabel(q.Get("labels"))) {
				out = append(out, *is)
			}
		}
		if q.Get("direction") != "asc" {
			slices.Reverse(out)
		}
		// Paginate like GitHub: per_page items per page, a Link header to the next.
		perPage, _ := strconv.Atoi(q.Get("per_page"))
		page, _ := strconv.Atoi(q.Get("page"))
		page = max(page, 1)
		start, end := min((page-1)*perPage, len(out)), min(page*perPage, len(out))
		if end < len(out) {
			q.Set("page", strconv.Itoa(page+1))
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next", <http://%s/x>; rel="last"`, r.Host, r.URL.Path, q.Encode(), r.Host))
		}
		writeJSON(w, 200, out[start:end])
	})
	mux.HandleFunc("GET "+repo+"/labels/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if l, ok := f.repoLabels[r.PathValue("name")]; ok {
			writeJSON(w, 200, map[string]string{"name": l.Name, "color": l.Color})
			return
		}
		writeJSON(w, 404, map[string]string{"message": "Not Found"})
	})
	mux.HandleFunc("POST "+repo+"/labels", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Name, Color, Description string }
		decode(t, r, &in)
		f.mu.Lock()
		defer f.mu.Unlock()
		if _, ok := f.repoLabels[in.Name]; ok {
			writeJSON(w, 422, map[string]string{"message": "Validation Failed"})
			return
		}
		f.repoLabels[in.Name] = LabelSpec{in.Name, in.Color, in.Description}
		writeJSON(w, 201, in)
	})
	mux.HandleFunc("GET "+repo+"/issues/{n}", func(w http.ResponseWriter, r *http.Request) {
		f.withIssue(w, r, func(is *Issue) { writeJSON(w, 200, is) })
	})
	mux.HandleFunc("POST "+repo+"/issues/{n}/labels", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Labels []string }
		decode(t, r, &in)
		f.withIssue(w, r, func(is *Issue) {
			for _, l := range in.Labels {
				if !is.HasLabel(l) {
					is.Labels = append(is.Labels, Label{Name: l})
				}
			}
			writeJSON(w, 200, is.Labels)
		})
	})
	mux.HandleFunc("DELETE "+repo+"/issues/{n}/labels/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.withIssue(w, r, func(is *Issue) {
			name := r.PathValue("name")
			if !is.HasLabel(name) {
				writeJSON(w, 404, map[string]string{"message": "Label does not exist"})
				return
			}
			is.Labels = slices.DeleteFunc(is.Labels, func(l Label) bool { return l.Name == name })
			writeJSON(w, 200, is.Labels)
		})
	})
	mux.HandleFunc("POST "+repo+"/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Body string }
		decode(t, r, &in)
		f.withIssue(w, r, func(is *Issue) {
			f.comments[is.Number] = append(f.comments[is.Number], in.Body)
			writeJSON(w, 201, map[string]any{})
		})
	})
	mux.HandleFunc("POST "+repo+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]any
		decode(t, r, &raw)
		// The fields are strings, but for draft, which GitHub wants a boolean.
		in, draft := map[string]string{}, false
		for k, v := range raw {
			switch v := v.(type) {
			case string:
				in[k] = v
			case bool:
				if k != "draft" {
					t.Errorf("POST /pulls: %s = %v, want a string", k, v)
				}
				draft = v
			default:
				t.Errorf("POST /pulls: %s = %v (%T)", k, v, v)
			}
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.creates = append(f.creates, raw)
		// Like GitHub, refuse a second open PR from the same branch.
		for _, pr := range f.prs {
			if pr["head"] == in["head"] && pr["state"] == "open" {
				writeJSON(w, 422, map[string]any{"message": "Validation Failed", "errors": []map[string]string{{"message": "A pull request already exists for vic:" + in["head"] + "."}}})
				return
			}
		}
		in["state"] = "open"
		f.prs = append(f.prs, in)
		f.drafts = append(f.drafts, draft)
		n := 100 + len(f.prs)
		writeJSON(w, 201, PullRequest{Number: n, HTMLURL: fmt.Sprintf("https://github.com/vic/demo/pull/%d", n)})
	})
	mux.HandleFunc("GET "+repo+"/pulls", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		defer f.mu.Unlock()
		out := []map[string]any{}
		for i, pr := range f.prs {
			if (q.Get("head") == "" || "vic:"+pr["head"] == q.Get("head")) && (q.Get("state") == "all" || pr["state"] == q.Get("state")) {
				n := 101 + i
				repoName := pr["head_repo"]
				if repoName == "" {
					repoName = "vic/demo"
				}
				var mergedAt any
				if pr["merged_at"] != "" {
					mergedAt = pr["merged_at"]
				}
				draft := i < len(f.drafts) && f.drafts[i]
				out = append(out, map[string]any{"number": n, "html_url": fmt.Sprintf("https://github.com/vic/demo/pull/%d", n), "title": pr["title"], "state": pr["state"], "draft": draft, "merged_at": mergedAt, "head": map[string]any{"ref": pr["head"], "repo": map[string]string{"full_name": repoName}}, "base": map[string]string{"ref": pr["base"]}})
			}
		}
		if q.Get("direction") == "desc" {
			slices.Reverse(out)
		}
		perPage, _ := strconv.Atoi(q.Get("per_page"))
		if perPage == 0 {
			perPage = 30
		}
		page, _ := strconv.Atoi(q.Get("page"))
		page = max(page, 1)
		start, end := min((page-1)*perPage, len(out)), min(page*perPage, len(out))
		if end < len(out) {
			q.Set("page", strconv.Itoa(page+1))
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, r.Host, r.URL.Path, q.Encode()))
		}
		writeJSON(w, 200, out[start:end])
	})
	mux.HandleFunc("GET "+repo+"/pulls/{n}", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		if r.Header.Get("Accept") == "application/vnd.github.diff" {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.diffAccepts = append(f.diffAccepts, r.Header.Get("Accept"))
			if diff, ok := f.diffs[n]; ok {
				w.Header().Set("Content-Type", "application/vnd.github.diff")
				w.WriteHeader(200)
				io.WriteString(w, diff)
				return
			}
			writeJSON(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		f.viewGets++
		if pr, ok := f.views[n]; ok {
			writeJSON(w, 200, pr)
			return
		}
		if n < 101 || n > 100+len(f.prs) {
			writeJSON(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		pr := f.prs[n-101]
		state, merged := pr["state"], pr["state"] == "merged"
		if merged {
			state = "closed" // GitHub reports a merged PR as closed
		}
		repoName := pr["head_repo"]
		if repoName == "" {
			repoName = "vic/demo"
		}
		var repo any
		if repoName == "deleted" {
			repo = nil
		} else {
			repo = map[string]string{"full_name": repoName}
		}
		writeJSON(w, 200, map[string]any{
			"number":   n,
			"html_url": fmt.Sprintf("https://github.com/vic/demo/pull/%d", n),
			"state":    state,
			"merged":   merged,
			"head":     map[string]any{"sha": "abc123", "ref": pr["head"], "repo": repo},
		})
	})
	mux.HandleFunc("GET "+repo+"/pulls/{n}/files", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		f.mu.Lock()
		defer f.mu.Unlock()
		f.fileAccepts = append(f.fileAccepts, r.Header.Get("Accept"))
		files, ok := f.files[n]
		if !ok {
			// A PR without recorded files lists nothing; a non-PR is 404.
			if _, inViews := f.views[n]; !inViews && (n < 101 || n > 100+len(f.prs)) {
				writeJSON(w, 404, map[string]string{"message": "Not Found"})
				return
			}
			writeJSON(w, 200, []prFile{})
			return
		}
		q := r.URL.Query()
		perPage, _ := strconv.Atoi(q.Get("per_page"))
		if perPage == 0 {
			perPage = 30
		}
		page, _ := strconv.Atoi(q.Get("page"))
		page = max(page, 1)
		start, end := min((page-1)*perPage, len(files)), min(page*perPage, len(files))
		if end < len(files) {
			q.Set("page", strconv.Itoa(page+1))
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, r.Host, r.URL.Path, q.Encode()))
		}
		writeJSON(w, 200, files[start:end])
	})
	mux.HandleFunc("DELETE "+repo+"/git/refs/heads/{branch}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.deletes = append(f.deletes, r.PathValue("branch"))
		w.WriteHeader(204)
	})
	mux.HandleFunc("PATCH "+repo+"/pulls/{n}", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		decode(t, r, &in)
		n, _ := strconv.Atoi(r.PathValue("n"))
		f.mu.Lock()
		defer f.mu.Unlock()
		f.updates = append(f.updates, in)
		if n < 101 || n > 100+len(f.prs) {
			writeJSON(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		for k, v := range in {
			switch v := v.(type) {
			case string:
				f.prs[n-101][k] = v
			case bool:
				if k == "draft" {
					f.drafts[n-101] = v
					continue
				}
				t.Errorf("PATCH /pulls/%d: %s = %v, want a string", n, k, v)
			default:
				t.Errorf("PATCH /pulls/%d: %s = %v (%T)", n, k, v, v)
			}
		}
		writeJSON(w, 200, PullRequest{Number: n, HTMLURL: fmt.Sprintf("https://github.com/vic/demo/pull/%d", n)})
	})

	mux.HandleFunc("PUT "+repo+"/pulls/{n}/merge", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		decode(t, r, &in)
		n, _ := strconv.Atoi(r.PathValue("n"))
		f.mu.Lock()
		defer f.mu.Unlock()
		if n < 101 || n > 100+len(f.prs) {
			writeJSON(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		pr := f.prs[n-101]
		pr["merge_attempts"] += "x"
		if len(f.mergeErrs) > 0 {
			e := f.mergeErrs[0]
			f.mergeErrs = f.mergeErrs[1:]
			writeJSON(w, e.status, map[string]string{"message": e.message})
			return
		}
		if pr["state"] != "open" {
			writeJSON(w, 405, map[string]string{"message": "Pull Request is not mergeable"})
			return
		}
		pr["state"], pr["merge_method"], pr["merge_sha"] = "merged", in["merge_method"], in["sha"]
		writeJSON(w, 200, map[string]any{"sha": "f00dfeed", "merged": true, "message": "Pull Request successfully merged"})
	})

	mux.HandleFunc("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		decode(t, r, &in)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.graphqlCalls = append(f.graphqlCalls, in)
		if f.graphqlErr != "" {
			writeJSON(w, 200, map[string]any{"errors": []map[string]string{{"message": f.graphqlErr}}})
			return
		}
		vars, _ := in["variables"].(map[string]any)
		id, _ := vars["id"].(string)
		for n, v := range f.views {
			if v.NodeID == id {
				query, _ := in["query"].(string)
				v.Draft = !strings.Contains(query, "convertPullRequestToDraft")
				f.views[n] = v
				writeJSON(w, 200, map[string]any{"data": map[string]any{"pullRequest": map[string]any{"isDraft": v.Draft}}})
				return
			}
		}
		writeJSON(w, 200, map[string]any{"errors": []map[string]string{{"message": "Could not resolve to a PullRequest with the id of '" + id + "'"}}})
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			writeJSON(w, 401, map[string]string{"message": "Bad credentials"})
			return
		}
		if r.Method != "GET" {
			f.mu.Lock()
			f.writes = append(f.writes, r.Method+" "+r.URL.Path)
			f.mu.Unlock()
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeGitHub) withIssue(w http.ResponseWriter, r *http.Request, fn func(*Issue)) {
	n, _ := strconv.Atoi(r.PathValue("n"))
	f.mu.Lock()
	defer f.mu.Unlock()
	is, ok := f.issues[n]
	if !ok {
		writeJSON(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	fn(is)
}

func (f *fakeGitHub) labels(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, l := range f.issues[n].Labels {
		out = append(out, l.Name)
	}
	slices.Sort(out)
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func decode(t *testing.T, r *http.Request, v any) {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Errorf("decode request body: %v", err)
	}
}

// setupRepo creates a bare "origin" with one commit on main and a clone of it.
func setupRepo(t *testing.T) (root, origin string) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(cfg, []byte("[user]\n\tname = amf test\n\temail = amf@example.com\n[init]\n\tdefaultBranch = main\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	dir := t.TempDir()
	origin = filepath.Join(dir, "origin.git")
	root = filepath.Join(dir, "work")
	mustGit(t, dir, "init", "--bare", "-b", "main", origin)
	mustGit(t, dir, "clone", origin, root)
	os.WriteFile(filepath.Join(root, "README.md"), []byte("demo\n"), 0o644)
	mustGit(t, root, "add", ".")
	mustGit(t, root, "commit", "-m", "initial")
	mustGit(t, root, "push", "-u", "origin", "main")
	return root, origin
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// fakeClaude writes a shell script standing in for the claude CLI.
func fakeClaude(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestRunner(t *testing.T, srv *httptest.Server, root, claude string) (*Runner, *bytes.Buffer) {
	var log bytes.Buffer
	return &Runner{
		GH:     newGitHub(Config{Token: "test-token", Repo: "vic/demo", APIURL: srv.URL}),
		Repo:   "vic/demo",
		Root:   root,
		Claude: claude,
		// A key makes claude the agent amf picks for issues without a pipeline.
		AnthropicKey: "sk-ant-test",
		Timeout:      time.Minute,
		Log:          &log,
	}, &log
}

func TestRunOpensPR(t *testing.T) {
	root, origin := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", Body: "Please add feature.txt", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	t.Setenv("GITHUB_TOKEN", "must-not-leak")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api-should-not-win")

	claude := fakeClaude(t, `
prompt=$(cat)
case "$prompt" in *"Add feature file"*"Please add feature.txt"*) ;; *) echo "bad prompt: $prompt" >&2; exit 4;; esac
case "$*" in *--dangerously-skip-permissions*"--output-format stream-json --verbose"*) ;; *) echo "missing flag: $*" >&2; exit 5;; esac
if [ -n "$GITHUB_TOKEN" ]; then echo "token leaked" >&2; exit 3; fi
if [ "$CLAUDE_CODE_OAUTH_TOKEN" != "sk-ant-oat-test" ]; then echo "oauth token missing" >&2; exit 6; fi
if [ -n "$ANTHROPIC_API_KEY" ]; then echo "api key should be dropped" >&2; exit 7; fi
echo '{"type":"system","subtype":"init"}'
echo hello > feature.txt
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"Added feature.txt.\n\nVerified with ls.\n\nCloses #1\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)"}'
`)
	r, log := newTestRunner(t, srv, root, claude)
	r.ClaudeToken = "sk-ant-oat-test"

	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}

	if len(gh.prs) != 1 {
		t.Fatalf("expected one PR, got %v", gh.prs)
	}
	pr := gh.prs[0]
	if pr["head"] != "amf/issue-1" || pr["base"] != "main" || pr["title"] != "Fix #1: Add feature file" {
		t.Fatalf("unexpected PR %v", pr)
	}
	if !strings.Contains(pr["body"], "Added feature.txt.") || strings.Count(pr["body"], "Closes #1") != 1 {
		t.Fatalf("PR body should have the summary and exactly one Closes: %q", pr["body"])
	}
	if strings.Contains(strings.ToLower(pr["body"]), "claude") {
		t.Fatalf("PR body should carry no Claude attribution: %q", pr["body"])
	}
	if got := mustGit(t, root, "--git-dir", origin, "show", "amf/issue-1:feature.txt"); got != "hello" {
		t.Fatalf("pushed branch has feature.txt = %q", got)
	}
	if got, want := gh.labels(1), []string{"amf:done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "/pull/101") {
		t.Fatalf("comments = %v", c)
	}
	if _, err := os.Stat(filepath.Join(root, ".amf", "worktrees", "issue-1")); !os.IsNotExist(err) {
		t.Fatalf("worktree should be removed after success, stat err = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".amf", "logs", "issue-1.log")); !bytes.Contains(b, []byte("Added feature.txt")) {
		t.Fatalf("log missing claude output: %q", b)
	}
}

func TestRunFailsWithoutChanges(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Do nothing", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	claude := fakeClaude(t, `cat >/dev/null; echo '{"type":"result","subtype":"success","is_error":false,"result":"nothing"}'`)
	r, _ := newTestRunner(t, srv, root, claude)

	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "without making any changes") {
		t.Fatalf("expected no-changes error, got %v", err)
	}
	if got, want := gh.labels(1), []string{"amf:failed"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "without making any changes") {
		t.Fatalf("comments = %v", c)
	}
	if len(gh.prs) != 0 {
		t.Fatalf("no PR expected, got %v", gh.prs)
	}
	if _, err := os.Stat(filepath.Join(root, ".amf", "worktrees", "issue-1")); err != nil {
		t.Fatalf("worktree should be kept after failure: %v", err)
	}

	// A re-run starts over from a fresh worktree.
	r.Claude = fakeClaude(t, `cat >/dev/null; echo fixed > fix.txt; echo done`)
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if got, want := gh.labels(1), []string{"amf:done"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("labels after re-run = %v, want %v", got, want)
	}
}

func TestRunClaudeTimeout(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Slow", State: "open"}
	r, _ := newTestRunner(t, srv, root, fakeClaude(t, "exec sleep 30\n"))
	r.Timeout = 200 * time.Millisecond

	err := r.Run(context.Background(), 1)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func TestWatchProcessesLabeledIssues(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "One", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	gh.issues[2] = &Issue{Number: 2, Title: "Already done", State: "open", Labels: []Label{{Name: "amf:done"}}}
	gh.issues[3] = &Issue{Number: 3, Title: "Not for amf", State: "open"}
	gh.issues[4] = &Issue{Number: 4, Title: "Retry", State: "open", Labels: []Label{{Name: "amf:ready"}, {Name: "amf:failed"}}}
	claude := fakeClaude(t, `cat >/dev/null; date > out.txt; echo ok`)
	r, log := newTestRunner(t, srv, root, claude)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- r.Watch(ctx, "amf:ready", 50*time.Millisecond) }()
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Contains(gh.labels(1), "amf:done") || !slices.Contains(gh.labels(4), "amf:done") {
		if time.Now().After(deadline) {
			t.Fatalf("issue 1 never processed; log:\n%s", log)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(gh.prs) != 2 || gh.prs[0]["head"] != "amf/issue-1" || gh.prs[1]["head"] != "amf/issue-4" {
		t.Fatalf("expected PRs for #1 and #4, got %v", gh.prs)
	}
	if got := gh.labels(4); !reflect.DeepEqual(got, []string{"amf:done"}) {
		t.Fatalf("retried issue labels = %v", got)
	}
}

func TestIssueCommand(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	t.Chdir(t.TempDir())
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"issue", "Add dark mode", "-b", "Use CSS variables"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "#1 https://github.com/vic/demo/issues/1") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	is := gh.issues[1]
	if is.Title != "Add dark mode" || is.Body != "Use CSS variables" || !is.HasLabel("amf:ready") {
		t.Fatalf("unexpected issue %+v", is)
	}

	// Body from stdin, no label.
	stdout.Reset()
	err = run(context.Background(), []string{"issue", "--no-label", "Second"}, strings.NewReader("from stdin\n"), &stdout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if is := gh.issues[2]; is.Body != "from stdin\n" || len(is.Labels) != 0 {
		t.Fatalf("unexpected issue %+v", is)
	}

	// Explicit file selection can also read stdin.
	err = run(context.Background(), []string{"issue", "Third", "-f", "-"}, strings.NewReader("from file stdin\n"), &stdout, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if is := gh.issues[3]; is.Body != "from file stdin\n" {
		t.Fatalf("unexpected issue body %q", is.Body)
	}
}

func TestIssueRejectsLabelWithNoLabel(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	t.Chdir(t.TempDir())
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)

	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"issue", "T", "--label", "amf:ready", "--no-label"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil {
		t.Fatalf("--label with --no-label should fail; stdout = %q", stdout.String())
	}
	if !strings.Contains(err.Error(), "--label") || !strings.Contains(err.Error(), "--no-label") {
		t.Fatalf("error %q should name both --label and --no-label", err)
	}
	if len(gh.issues) != 0 {
		t.Fatalf("rejected combination still created issues: %+v", gh.issues)
	}
}

func TestIssueRejectsEmptyLabel(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	t.Chdir(t.TempDir())
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)

	for _, label := range []string{"", "   "} {
		var stdout, stderr bytes.Buffer
		err := run(context.Background(), []string{"issue", "T", "--label", label}, strings.NewReader(""), &stdout, &stderr)
		if err == nil {
			t.Fatalf("--label %q should fail; stdout = %q", label, stdout.String())
		}
		if !strings.Contains(err.Error(), "--label can't be empty") {
			t.Fatalf("error %q should name --label", err)
		}
	}
	if len(gh.issues) != 0 {
		t.Fatalf("rejected empty label still created issues: %+v", gh.issues)
	}
}

func TestExcludeAMFDir(t *testing.T) {
	root, _ := setupRepo(t)
	ctx := context.Background()
	for range 2 {
		if err := excludeFromGit(ctx, root, "amf worktrees and logs", "/.amf/"); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(root, ".git", "info", "exclude"))
	if strings.Count(string(b), "/.amf/") != 1 {
		t.Fatalf("exclude file:\n%s", b)
	}
	os.MkdirAll(filepath.Join(root, ".amf", "logs"), 0o755)
	os.WriteFile(filepath.Join(root, ".amf", "logs", "x.log"), []byte("x"), 0o644)
	if st := mustGit(t, root, "status", "--porcelain"); st != "" {
		t.Fatalf(".amf should be ignored, status = %q", st)
	}
}

func TestMain(m *testing.M) {
	for _, k := range []string{"GITHUB_TOKEN", "GITHUB_REPO", "CLAUDE_CODE_OAUTH_TOKEN", "AMF_API_URL", "AMF_CLONE_URL", "AMF_CLAUDE", "AMF_CODEX", "AMF_OPENCODE", "AMF_RELEASES_URL", "AMF_TRUSTED", "AMF_POSTGRES_URL", "AMF_AGENT_TIMEOUT", "AMF_LOG_LEVEL"} {
		os.Unsetenv(k)
	}
	os.Setenv("AMF_API_URL", "http://127.0.0.1:1") // unreachable unless a test sets a fake
	// Hide any real agents on this machine, as CI has none: a test that needs
	// one sets a fake. The live tests run the real ones.
	if os.Getenv("AMF_LIVE_DIR") == "" {
		for _, k := range []string{"AMF_CLAUDE", "AMF_CODEX", "AMF_OPENCODE"} {
			os.Setenv(k, "amf-test-no-such-agent")
		}
		// Agent keys decide which agent amf picks, so none leak in either.
		os.Unsetenv("ANTHROPIC_API_KEY")
		os.Unsetenv("OPENAI_API_KEY")
		os.Unsetenv("OPENCODE_API_KEY")
		os.Unsetenv("OPENCODE_MODEL")
	}
	if _, err := exec.LookPath("git"); err != nil {
		fmt.Println("skipping: git not installed")
		return
	}
	os.Exit(m.Run())
}

func TestInitWritesTemplate(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	var out bytes.Buffer
	if err := run(context.Background(), []string{"init"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(".env")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"GITHUB_TOKEN=github_pat_...   # Contents, Issues, Pull requests: read & write\n",
		"GITHUB_REPO=owner/name\n",
		"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-...",
		"AMF_TRUSTED=login1,login2...   # optional: only work on issues opened by these GitHub users\n",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf(".env missing %q:\n%s", want, b)
		}
	}
	if st, _ := os.Stat(".env"); st.Mode().Perm() != 0o600 {
		t.Errorf(".env mode = %v, want 0600", st.Mode().Perm())
	}
	if m, _ := filepath.Glob(".env.*"); len(m) != 0 {
		t.Errorf("no backup expected without an existing .env, got %v", m)
	}
	if !strings.Contains(out.String(), "fill in GITHUB_TOKEN, GITHUB_REPO, CLAUDE_CODE_OAUTH_TOKEN") ||
		!strings.Contains(out.String(), "labels not checked yet") {
		t.Errorf("output = %q", out.String())
	}

	// Placeholders count as unset.
	for _, k := range []string{"GITHUB_TOKEN", "GITHUB_REPO", "CLAUDE_CODE_OAUTH_TOKEN"} {
		t.Setenv(k, "")
	}
	if _, err := loadConfig(".env"); err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN is not set") {
		t.Fatalf("loadConfig with placeholders: %v", err)
	}
	os.WriteFile(".env", []byte("GITHUB_TOKEN=real\nGITHUB_REPO=vic/demo\nCLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-...\n"), 0o600)
	cfg, err := loadConfig(".env")
	if err != nil || cfg.ClaudeToken != "" {
		t.Fatalf("placeholder OAuth token should be ignored: %+v, %v", cfg, err)
	}
}

func TestInitBacksUpAndKeepsValues(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	orig := "GITHUB_TOKEN=ghp_real\nAMF_CLAUDE=/opt/my claude\nGITHUB_REPO=owner/name\n"
	os.WriteFile(".env", []byte(orig), 0o600)

	var out bytes.Buffer
	if err := run(context.Background(), []string{"init"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(".env.0001"); string(b) != orig {
		t.Fatalf(".env.0001 = %q, want the original .env", b)
	}
	if !strings.Contains(out.String(), "backed up .env to .env.0001") {
		t.Errorf("output = %q", out.String())
	}
	vars, err := parseDotEnv(strings.NewReader(mustRead(t, ".env")))
	if err != nil {
		t.Fatal(err)
	}
	if vars["GITHUB_TOKEN"] != "ghp_real" || vars["AMF_CLAUDE"] != "/opt/my claude" || vars["GITHUB_REPO"] != "owner/name" {
		t.Fatalf("new .env lost values: %v", vars)
	}

	// Each change takes the next number, even after a gap.
	os.WriteFile(".env.0007", []byte("old"), 0o600)
	os.WriteFile(".env", []byte("GITHUB_TOKEN=ghp_other\n"), 0o600)
	if err := run(context.Background(), []string{"init"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(".env.0008"); err != nil {
		t.Fatalf("expected .env.0008: %v", err)
	}
}

func TestInitInRepoFillsRepoAndExcludes(t *testing.T) {
	root, _ := setupRepo(t)
	mustGit(t, root, "remote", "set-url", "origin", "git@github.com:vic/demo.git")
	t.Chdir(root)
	if err := run(context.Background(), []string{"init"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mustRead(t, ".env"), "GITHUB_REPO=vic/demo\n") {
		t.Fatalf("GITHUB_REPO not taken from origin:\n%s", mustRead(t, ".env"))
	}
	run(context.Background(), []string{"init"}, nil, io.Discard, io.Discard) // creates .env.0001
	if st := mustGit(t, root, "status", "--porcelain"); st != "" {
		t.Fatalf(".env and backups should be ignored, status = %q", st)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNewRunnerClonesOutsideCheckout(t *testing.T) {
	_, origin := setupRepo(t)
	_, srv := newFakeGitHub(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)
	t.Setenv("AMF_CLONE_URL", origin)
	ctx := context.Background()

	var log bytes.Buffer
	r, err := newRunner(ctx, time.Minute, 2, "", logOptions{}, &log)
	if err != nil {
		t.Fatalf("newRunner: %v\n%s", err, log.String())
	}
	if want, _ := filepath.EvalSymlinks(filepath.Join(dir, "demo")); mustEval(t, r.Root) != want {
		t.Fatalf("Root = %s, want %s", r.Root, want)
	}
	if !strings.Contains(log.String(), "cloning vic/demo into") {
		t.Fatalf("log = %q", log.String())
	}
	if got := mustGit(t, r.Root, "log", "--format=%s", "-1"); got != "initial" {
		t.Fatalf("clone HEAD = %q", got)
	}

	// The next run reuses the clone.
	log.Reset()
	if _, err := newRunner(ctx, time.Minute, 2, "", logOptions{}, &log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "using the clone at") {
		t.Fatalf("log = %q", log.String())
	}

	// A folder with the repo's name that isn't its clone is left alone.
	t.Chdir(t.TempDir())
	os.Mkdir("demo", 0o755)
	if _, err := newRunner(ctx, time.Minute, 2, "", logOptions{}, io.Discard); err == nil || !strings.Contains(err.Error(), "is not a clone of vic/demo") {
		t.Fatalf("expected not-a-clone error, got %v", err)
	}
}

func TestNewRunnerUnknownRepo(t *testing.T) {
	_, srv := newFakeGitHub(t)
	t.Chdir(t.TempDir())
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vicc/demo")
	t.Setenv("AMF_API_URL", srv.URL)
	_, err := newRunner(context.Background(), time.Minute, 2, "", logOptions{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "GITHUB_REPO vicc/demo was not found") {
		t.Fatalf("expected not-found error, got %v", err)
	}
	if m, _ := filepath.Glob("*"); len(m) != 0 {
		t.Fatalf("nothing should be cloned for an unknown repo, got %v", m)
	}

	t.Setenv("GITHUB_TOKEN", "wrong-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	if _, err := newRunner(context.Background(), time.Minute, 2, "", logOptions{}, io.Discard); err == nil || !strings.Contains(err.Error(), "rejected GITHUB_TOKEN") {
		t.Fatalf("expected bad-token error, got %v", err)
	}
}

func mustEval(t *testing.T, p string) string {
	t.Helper()
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestInitCreatesLabels(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	gh.repoLabels["amf:done"] = LabelSpec{"amf:done", "000000", "already here"}
	t.Chdir(t.TempDir())
	t.Setenv("AMF_API_URL", srv.URL)
	os.WriteFile(".env", []byte("GITHUB_TOKEN=test-token\nGITHUB_REPO=vic/demo\n"), 0o600)

	var out bytes.Buffer
	if err := run(context.Background(), []string{"init"}, nil, &out, io.Discard); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for _, l := range amfLabels {
		if _, ok := gh.repoLabels[l.Name]; !ok {
			t.Errorf("label %s not created", l.Name)
		}
	}
	if gh.repoLabels["amf:done"].Color != "000000" {
		t.Error("existing label should be left alone")
	}
	for _, want := range []string{"amf:ready        created", "amf:done         exists", "amf:failed       created"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}

	// Running init again changes nothing: no new backup, every label exists.
	out.Reset()
	if err := run(context.Background(), []string{"init"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), ".env is up to date") || strings.Contains(out.String(), "created") {
		t.Errorf("second init output:\n%s", out.String())
	}
	if m, _ := filepath.Glob(".env.0*"); len(m) != 1 {
		t.Errorf("expected only the first run's backup, got %v", m)
	}

	// A bad token fails clearly.
	os.WriteFile(".env", []byte("GITHUB_TOKEN=nope\nGITHUB_REPO=vic/demo\n"), 0o600)
	if err := run(context.Background(), []string{"init"}, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "rejected GITHUB_TOKEN") {
		t.Fatalf("expected bad-token error, got %v", err)
	}
}

func TestListIssuesPaginates(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	for n := 1; n <= 250; n++ {
		gh.issues[n] = &Issue{Number: n, Title: fmt.Sprint("issue ", n), State: "open"}
	}
	gh.issues[7].PullRequest = &struct{}{}
	c := newGitHub(Config{Token: "test-token", Repo: "vic/demo", APIURL: srv.URL})

	issues, err := c.ListIssues(context.Background(), "open", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 249 || issues[0].Number != 1 || issues[248].Number != 250 {
		t.Fatalf("got %d issues (first #%d), want 249 oldest first without the PR", len(issues), issues[0].Number)
	}
	if slices.ContainsFunc(issues, func(is Issue) bool { return is.Number == 7 }) {
		t.Fatal("pull requests should be left out")
	}
}

func TestListCommand(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Old done", State: "closed", Labels: []Label{{Name: "amf:done"}, {Name: "bug"}}}
	gh.issues[2] = &Issue{Number: 2, Title: "Waiting", State: "open", Labels: []Label{{Name: "amf:ready"}, {Name: "team backend"}}}
	gh.issues[3] = &Issue{Number: 3, Title: "Plain\tissue", State: "open"}
	gh.issues[4] = &Issue{Number: 4, Title: "A PR", State: "open", PullRequest: &struct{}{}}
	t.Chdir(t.TempDir())
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)
	list := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := run(context.Background(), append([]string{"list"}, args...), nil, &out, io.Discard); err != nil {
			t.Fatalf("amf list %v: %v", args, err)
		}
		return out.String()
	}

	if got, want := list(), "ISSUE  AMF        TITLE\n#3     -          Plain issue\n#2     amf:ready  Waiting\n"; got != want {
		t.Errorf("amf list =\n%s\nwant\n%s", got, want)
	}
	if got := list("--state", "all"); !strings.Contains(got, "STATE") || !strings.Contains(got, "#1     closed  amf:done") {
		t.Errorf("amf list --state all =\n%s", got)
	}
	if got := list("--state", " open "); got != list() {
		t.Errorf("amf list --state with surrounding whitespace =\n%s\nwant same as default state", got)
	}
	if got := list("--label", "amf:ready"); strings.Contains(got, "#3") || !strings.Contains(got, "#2") {
		t.Errorf("amf list --label amf:ready =\n%s", got)
	}
	if got := list("--label", " amf:ready "); got != list("--label", "amf:ready") {
		t.Errorf("padded label output =\n%s\nwant same as trimmed label output", got)
	}
	if got := list("--label", "team backend"); !strings.Contains(got, "#2") || strings.Contains(got, "#3") {
		t.Errorf("internally spaced label output =\n%s", got)
	}
	if got := list("--label", " \t "); got != list() {
		t.Errorf("whitespace-only label output =\n%s\nwant unfiltered output", got)
	}
	if got, want := list("--limit", "1"), "ISSUE  AMF  TITLE\n#3     -    Plain issue\n"; got != want {
		t.Errorf("amf list --limit 1 =\n%s\nwant\n%s", got, want)
	}
	if got := list("--limit", "0"); got != list() {
		t.Errorf("amf list --limit 0 =\n%s\nwant the unflagged output", got)
	}
	if got := list("--state", "all", "--limit", "2"); strings.Contains(got, "#1") || !strings.Contains(got, "#3") || !strings.Contains(got, "#2") {
		t.Errorf("amf list --state all --limit 2 =\n%s", got)
	}
	if got := list("--label", "amf:ready", "--limit", "1"); strings.Contains(got, "#3") || !strings.Contains(got, "#2") {
		t.Errorf("amf list --label amf:ready --limit 1 =\n%s", got)
	}
	if err := run(context.Background(), []string{"list", "--limit", "-1"}, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "--limit") {
		t.Errorf("amf list --limit -1: err = %v, want an error mentioning --limit", err)
	}
	if got := list("--state", "closed", "--label", "amf:failed"); got != "No closed issues labeled amf:failed in vic/demo.\n" {
		t.Errorf("empty list = %q", got)
	}
	gh.issues[1].State = "open"
	if got := list("--state", "closed", "--label", " \t "); got != "No closed issues in vic/demo.\n" {
		t.Errorf("empty list with whitespace-only label = %q", got)
	}
	if err := run(context.Background(), []string{"list", "--state", "bogus"}, nil, io.Discard, io.Discard); err == nil {
		t.Error("expected an error for --state bogus")
	}
}

func TestListCommandJSON(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	url := func(n int) string { return fmt.Sprintf("https://github.com/vic/demo/issues/%d", n) }
	gh.issues[1] = &Issue{Number: 1, Title: "Old done", State: "closed", HTMLURL: url(1), Labels: []Label{{Name: "amf:done"}, {Name: "bug"}}}
	gh.issues[2] = &Issue{Number: 2, Title: "Waiting", State: "open", HTMLURL: url(2), Labels: []Label{{Name: "enhancement"}, {Name: "amf:ready"}, {Name: "team backend"}}}
	gh.issues[3] = &Issue{Number: 3, Title: "Plain\tissue", State: "open", HTMLURL: url(3)}
	gh.issues[4] = &Issue{Number: 4, Title: "A PR", State: "open", PullRequest: &struct{}{}}
	t.Chdir(t.TempDir())
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)
	list := func(args ...string) (string, []listedIssue) {
		t.Helper()
		var out bytes.Buffer
		if err := run(context.Background(), append([]string{"list", "--json"}, args...), nil, &out, io.Discard); err != nil {
			t.Fatalf("amf list --json %v: %v", args, err)
		}
		var got []listedIssue
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("amf list --json %v printed %q: %v", args, out.String(), err)
		}
		return out.String(), got
	}
	numbers := func(is []listedIssue) []int {
		var ns []int
		for _, i := range is {
			ns = append(ns, i.Number)
		}
		return ns
	}

	raw, got := list()
	want := []listedIssue{
		{Number: 3, Title: "Plain\tissue", State: "open", URL: url(3), Labels: []string{}},
		{Number: 2, Title: "Waiting", State: "open", URL: url(2), Labels: []string{"enhancement", "amf:ready", "team backend"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("amf list --json = %+v, want %+v", got, want)
	}
	if !strings.HasPrefix(raw, "[") || !strings.HasSuffix(raw, "]\n") || strings.Count(raw, "\n") != 1 {
		t.Errorf("amf list --json should print one array and a newline, got %q", raw)
	}
	// Every field is there, with its JSON type, and nothing else.
	var generic []map[string]any
	if err := json.Unmarshal([]byte(raw), &generic); err != nil {
		t.Fatal(err)
	}
	if len(generic[0]) != 5 {
		t.Errorf("entry has fields %v, want exactly number, title, state, url and labels", generic[0])
	}
	if _, ok := generic[0]["number"].(float64); !ok {
		t.Errorf("number = %#v, want a JSON number", generic[0]["number"])
	}
	if labels, ok := generic[0]["labels"].([]any); !ok || len(labels) != 0 {
		t.Errorf("labels of an unlabeled issue = %#v, want []", generic[0]["labels"])
	}

	if _, got := list("--state", "all"); !slices.Equal(numbers(got), []int{3, 2, 1}) || got[2].State != "closed" || !slices.Equal(got[2].Labels, []string{"amf:done", "bug"}) {
		t.Errorf("amf list --json --state all = %+v", got)
	}
	if _, got := list("--label", "amf:ready"); !slices.Equal(numbers(got), []int{2}) {
		t.Errorf("amf list --json --label amf:ready = %+v", got)
	}
	if _, got := list("--label", " amf:ready "); !slices.Equal(numbers(got), []int{2}) {
		t.Errorf("amf list --json --label padded amf:ready = %+v", got)
	}
	if _, got := list("--label", "team backend"); !slices.Equal(numbers(got), []int{2}) {
		t.Errorf("amf list --json --label internally spaced = %+v", got)
	}
	if _, got := list("--label", " \t "); !slices.Equal(numbers(got), []int{3, 2}) {
		t.Errorf("amf list --json --label whitespace-only = %+v", got)
	}
	if _, got := list("--state", "all", "--limit", "2"); !slices.Equal(numbers(got), []int{3, 2}) {
		t.Errorf("amf list --json --state all --limit 2 = %+v", got)
	}
	if _, got := list("--limit", "0"); !slices.Equal(numbers(got), []int{3, 2}) {
		t.Errorf("amf list --json --limit 0 = %+v", got)
	}
	if raw, _ := list("--state", "closed", "--label", "amf:failed"); raw != "[]\n" {
		t.Errorf("empty amf list --json = %q, want %q", raw, "[]\n")
	}
	for _, args := range [][]string{{"--limit", "-1"}, {"--state", "bogus"}} {
		var out bytes.Buffer
		if err := run(context.Background(), append([]string{"list", "--json"}, args...), nil, &out, io.Discard); err == nil || out.Len() != 0 {
			t.Errorf("amf list --json %v: err = %v, stdout %q; want an error and no output", args, err, out.String())
		}
	}

	// --json=false keeps the table.
	var out bytes.Buffer
	if err := run(context.Background(), []string{"list", "--json=false"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if want := "ISSUE  AMF        TITLE\n#3     -          Plain issue\n#2     amf:ready  Waiting\n"; out.String() != want {
		t.Errorf("amf list --json=false =\n%s\nwant\n%s", out.String(), want)
	}
}

// fakeReleases serves /releases/latest and one release's archive and checksums
// the way GitHub does, for this platform. The archive's amf is a script that
// prints reports.
func fakeReleases(t *testing.T, tag, reports string, badSum bool) *httptest.Server {
	return fakeReleasesFor(t, tag, reports, badSum, runtime.GOOS, runtime.GOARCH)
}

// fakeReleasesFor is fakeReleases for another platform: a .zip holding
// amf.exe for windows, else a .tar.gz holding amf.
func fakeReleasesFor(t *testing.T, tag, reports string, badSum bool, goos, goarch string) *httptest.Server {
	t.Helper()
	folder, file, bin := releaseArchive(tag, goos, goarch)
	script := []byte("#!/bin/sh\necho '" + reports + "'\n")
	var buf bytes.Buffer
	if goos == "windows" {
		zw := zip.NewWriter(&buf)
		zw.Create(folder + "/")
		w, _ := zw.CreateHeader(&zip.FileHeader{Name: folder + "/" + bin, Method: zip.Deflate})
		w.Write(script)
		zw.Close()
	} else {
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		tw.WriteHeader(&tar.Header{Name: folder + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		tw.WriteHeader(&tar.Header{Name: folder + "/" + bin, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(script))})
		tw.Write(script)
		tw.Close()
		gz.Close()
	}
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)
	sums := fmt.Sprintf("%x  %s\n", sum, file)
	if badSum {
		sums = strings.Repeat("0", 64) + "  " + file + "\n"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/"+tag, http.StatusFound)
	})
	mux.HandleFunc("GET /releases/download/"+tag+"/"+file, func(w http.ResponseWriter, r *http.Request) { w.Write(archive) })
	mux.HandleFunc("GET /releases/download/"+tag+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, sums) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestUpdater(t *testing.T, srv *httptest.Server, current string) (*updater, *bytes.Buffer) {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "amf")
	os.WriteFile(exe, []byte("old binary"), 0o755)
	var out bytes.Buffer
	return &updater{base: srv.URL, current: current, exe: exe, out: &out, goos: runtime.GOOS, goarch: runtime.GOARCH}, &out
}

func TestUpdateReplacesBinary(t *testing.T) {
	srv := fakeReleases(t, "v9.9.9", "amf v9.9.9", false)
	u, out := newTestUpdater(t, srv, "v0.0.7")
	if err := u.run(context.Background(), "", false); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if b := mustRead(t, u.exe); !strings.Contains(b, "amf v9.9.9") {
		t.Fatalf("binary not replaced: %q", b)
	}
	if st, _ := os.Stat(u.exe); st.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
	if !strings.Contains(out.String(), "Updated amf v0.0.7 -> v9.9.9") {
		t.Errorf("output = %q", out)
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(u.exe), ".amf-update-*")); len(m) != 0 {
		t.Errorf("temp files left behind: %v", m)
	}
}

func TestUpdateNoChange(t *testing.T) {
	srv := fakeReleases(t, "v9.9.9", "amf v9.9.9", false)
	for _, tc := range []struct {
		name, current, target string
		check                 bool
		want                  string
	}{
		{"up to date", "v9.9.9", "", false, "amf v9.9.9 is up to date"},
		{"pinned, no v", "v9.9.9", "9.9.9", false, "is up to date"},
		{"check only", "v0.0.7", "", true, "amf v9.9.9 is available (you have v0.0.7)"},
		{"check newer pinned", "v0.0.7", "v9.9.9", true, "amf v9.9.9 is available (you have v0.0.7)"},
		{"check same pinned", "v9.9.9", "v9.9.9", true, "amf v9.9.9 is up to date"},
		{"check older pinned", "v0.10.0", "v0.9.12", true, "amf v0.9.12 is older than this build (v0.10.0); amf update --version v0.9.12 would downgrade"},
		{"check pinned on dev", "dev", "v0.0.1", true, "amf v0.0.1 is available (you have dev)"},
	} {
		u, out := newTestUpdater(t, srv, tc.current)
		if err := u.run(context.Background(), tc.target, tc.check); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("%s: output = %q, want %q", tc.name, out, tc.want)
		}
		if mustRead(t, u.exe) != "old binary" {
			t.Errorf("%s: binary should be unchanged", tc.name)
		}
	}
}

func TestUpdateTrimsVersionFlag(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"update", "--check", "--version", " v0.0.7 "}, nil, &out, io.Discard); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got := out.String(); !strings.Contains(got, "amf v0.0.7 is available") || strings.Contains(got, "v v0.0.7") {
		t.Errorf("output = %q, want it to reference the trimmed v0.0.7", got)
	}
}

func TestUpdateRefusesBadDownloads(t *testing.T) {
	for _, tc := range []struct {
		name, reports string
		badSum        bool
		target, want  string
	}{
		{"checksum", "amf v9.9.9", true, "", "checksum mismatch"},
		{"wrong version", "amf v1.0.0", false, "", `reports "amf v1.0.0"`},
		{"missing release", "amf v9.9.9", false, "v8.8.8", "no amf v8.8.8 for " + runtime.GOOS + "/" + runtime.GOARCH},
	} {
		u, _ := newTestUpdater(t, fakeReleases(t, "v9.9.9", tc.reports, tc.badSum), "v0.0.7")
		err := u.run(context.Background(), tc.target, false)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
		if mustRead(t, u.exe) != "old binary" {
			t.Errorf("%s: binary should be unchanged", tc.name)
		}
	}
}

func TestCleanSummary(t *testing.T) {
	in := "## Add tests\n\nCloses #3.\n\n### Summary\nAdds TestX.\n\nCloses #3\n\nSee #4, and closes #3 elsewhere.\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\n\nCo-Authored-By: Claude <noreply@anthropic.com>\n"
	want := "## Add tests\n\n### Summary\nAdds TestX.\n\nSee #4, and closes #3 elsewhere."
	if got := cleanSummary(in, 3); got != want {
		t.Fatalf("cleanSummary =\n%q\nwant\n%q", got, want)
	}
	if got := cleanSummary("Closes #4\nReal text", 3); got != "Closes #4\nReal text" {
		t.Fatalf("other issue numbers must stay: %q", got)
	}
}

func TestUpdateWindows(t *testing.T) {
	// The Windows path, run here: a .zip holding amf.exe, a temp file ending
	// in .exe, and the running binary moved aside rather than overwritten.
	srv := fakeReleasesFor(t, "v9.9.9", "amf v9.9.9", false, "windows", "amd64")
	dir := t.TempDir()
	exe := filepath.Join(dir, "amf.exe")
	os.WriteFile(exe, []byte("old binary"), 0o755)
	os.WriteFile(exe+".old", []byte("from the update before"), 0o755)
	var out bytes.Buffer
	u := &updater{base: srv.URL, current: "v0.4.1", exe: exe, out: &out, goos: "windows", goarch: "amd64"}
	if err := u.run(context.Background(), "", false); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "Downloading amf v9.9.9 for windows/amd64") {
		t.Errorf("output = %q", out.String())
	}
	if b := mustRead(t, exe); !strings.Contains(b, "amf v9.9.9") {
		t.Fatalf("amf.exe not replaced: %q", b)
	}
	if b := mustRead(t, exe+".old"); b != "old binary" {
		t.Errorf("amf.exe.old = %q, want the binary that was running", b)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, ".amf-update-*")); len(m) != 0 {
		t.Errorf("temp files left behind: %v", m)
	}

	// A zip without amf.exe in the expected folder is refused.
	bad := fakeReleasesFor(t, "v9.9.9", "amf v9.9.9", false, "windows", "arm64")
	u2 := &updater{base: bad.URL, current: "v0.4.1", exe: exe, out: io.Discard, goos: "windows", goarch: "amd64"}
	if err := u2.run(context.Background(), "", false); err == nil || !strings.Contains(err.Error(), "no amf v9.9.9 for windows/amd64") {
		t.Errorf("error = %v", err)
	}
}

func TestReleaseArchive(t *testing.T) {
	for _, tc := range []struct{ goos, file, bin string }{
		{"windows", "amf_v1.2.3_windows_amd64.zip", "amf.exe"},
		{"linux", "amf_v1.2.3_linux_amd64.tar.gz", "amf"},
		{"darwin", "amf_v1.2.3_darwin_amd64.tar.gz", "amf"},
	} {
		folder, file, bin := releaseArchive("v1.2.3", tc.goos, "amd64")
		if folder != "amf_v1.2.3_"+tc.goos+"_amd64" || file != tc.file || bin != tc.bin {
			t.Errorf("releaseArchive(%s) = %s, %s, %s", tc.goos, folder, file, bin)
		}
	}
}
