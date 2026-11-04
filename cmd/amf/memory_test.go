package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const keyA, keyB = "github.com/vic/alpha", "github.com/vic/beta"

// openBackend opens backend's store for key under dir.
func openBackend(t *testing.T, backend, dir, key string) MemoryStore {
	t.Helper()
	s, err := openMemory(memoryOptions{Backend: backend, Dir: dir}, key)
	if err != nil {
		t.Fatalf("open %s: %v", backend, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedMemory(t *testing.T, s MemoryStore) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.UpsertIssue(MemIssue{Number: 1, Title: "Login page crashes on empty password", Body: "Submitting the login form with no password panics", State: "closed"}))
	must(s.UpsertIssue(MemIssue{Number: 2, Title: "Add dark mode toggle", Body: "A settings switch for the dark palette", State: "open"}))
	must(s.UpsertIssue(MemIssue{Number: 3, Title: "Speed up CSV export", Body: "Exports of big tables are slow", State: "open"}))
	must(s.UpsertPR(MemPR{Number: 10, Title: "Fix #1: Login page crashes on empty password", State: "merged", Merged: true, HeadBranch: "amf/issue-1", Issue: 1}))
	must(s.RecordRun(MemRun{ID: "r1", Issue: 1, Command: "run", StartedAt: "2026-01-01T00:00:00Z", Outcome: "failed", Agents: []string{"codex"}, ExitCodes: []int{2}, Error: "tests failed"}))
	must(s.RecordRun(MemRun{ID: "r2", Issue: 1, Command: "watch", StartedAt: "2026-01-02T00:00:00Z", Outcome: "success", PR: 10}))
	// A later write replaces the earlier one.
	must(s.UpsertIssue(MemIssue{Number: 3, Title: "Speed up CSV export", Body: "Exports of big tables are slow", State: "closed"}))
}

// TestMemoryConformance runs the same checks against every backend.
func TestMemoryConformance(t *testing.T) {
	for _, backend := range memoryBackends {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			s := openBackend(t, backend, dir, keyA)
			if s.Key() != keyA {
				t.Fatalf("key = %q", s.Key())
			}
			seedMemory(t, s)

			h, err := s.History(1)
			if err != nil || h.Issue == nil || h.Issue.Title != "Login page crashes on empty password" || h.Issue.Repo != keyA {
				t.Fatalf("history = %+v, %v", h, err)
			}
			if len(h.PRs) != 1 || h.PRs[0].Number != 10 || h.PRs[0].Issue != 1 || !h.PRs[0].Merged || h.PRs[0].Repo != keyA {
				t.Fatalf("PRs = %+v", h.PRs)
			}
			if len(h.Runs) != 2 || h.Runs[0].ID != "r1" || h.Runs[1].ID != "r2" || h.Runs[1].PR != 10 ||
				!slices.Equal(h.Runs[0].ExitCodes, []int{2}) || h.Runs[0].Agents[0] != "codex" || h.Runs[0].Repo != keyA {
				t.Fatalf("runs = %+v", h.Runs)
			}
			if h3, _ := s.History(3); h3.Issue == nil || h3.Issue.State != "closed" {
				t.Fatalf("issue 3 should be updated: %+v", h3.Issue)
			}
			if h9, err := s.History(9); err != nil || h9.Issue != nil || len(h9.Runs) != 0 {
				t.Fatalf("unknown issue = %+v, %v", h9, err)
			}

			hits, err := similar(s, "login crashes when the password is empty", 0, 5)
			if err != nil || len(hits) == 0 {
				t.Fatalf("similar = %v, %v", hits, err)
			}
			if top := hits[0]; !(top.Issue == 1 || top.PR == 10) {
				t.Fatalf("most relevant first, got %+v", hits)
			}
			for _, h := range hits {
				if h.Issue == 2 || h.Issue == 3 {
					t.Fatalf("unrelated issue matched: %+v", hits)
				}
			}
			if hits, _ := similar(s, "login crashes on empty password", 1, 5); slices.ContainsFunc(hits, func(h Hit) bool { return h.Kind == "issue" && h.Issue == 1 }) {
				t.Fatalf("exclude should drop issue 1: %+v", hits)
			}
			if d := duplicateOf(hits, "Login page crashes on empty password"); d == nil || !d.Merged {
				t.Fatalf("duplicate of merged work = %+v", d)
			}
			if hits, err := similar(s, "", 0, 5); err != nil || len(hits) != 0 {
				t.Fatalf("empty query = %v, %v", hits, err)
			}

			if c, _ := s.SyncCursor(); c != "" {
				t.Fatalf("cursor = %q", c)
			}
			if err := s.SetSyncCursor("2026-02-01T00:00:00Z"); err != nil {
				t.Fatal(err)
			}

			// Everything survives a reopen.
			s.Close()
			s2 := openBackend(t, backend, dir, keyA)
			if c, _ := s2.SyncCursor(); c != "2026-02-01T00:00:00Z" {
				t.Fatalf("cursor after reopen = %q", c)
			}
			if h, _ := s2.History(1); len(h.PRs) != 1 || len(h.Runs) != 2 {
				t.Fatalf("history after reopen = %+v", h)
			}
		})
	}
}

func TestMemoryRepoIsolation(t *testing.T) {
	for _, backend := range memoryBackends {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			a := openBackend(t, backend, dir, keyA)
			seedMemory(t, a)
			b := openBackend(t, backend, dir, keyB)
			if err := b.UpsertIssue(MemIssue{Number: 7, Title: "Beta only issue", State: "open"}); err != nil {
				t.Fatal(err)
			}
			if keyPath(dir, keyA) == keyPath(dir, keyB) {
				t.Fatal("repos share a folder")
			}
			if h, _ := b.History(1); h.Issue != nil || len(h.PRs) != 0 || len(h.Runs) != 0 {
				t.Fatalf("repo B sees repo A's issue 1: %+v", h)
			}
			if hits, _ := similar(b, "login page crashes on empty password", 0, 5); len(hits) != 0 {
				t.Fatalf("repo B finds repo A's work: %+v", hits)
			}
			if hits, _ := similar(a, "beta only issue", 0, 5); len(hits) != 0 {
				t.Fatalf("repo A finds repo B's work: %+v", hits)
			}
			a.Close()
			b.Close()

			// A store moved under another repo's key refuses to open.
			if err := os.RemoveAll(keyPath(dir, keyB)); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(keyPath(dir, keyA), keyPath(dir, keyB)); err != nil {
				t.Fatal(err)
			}
			_, err := openMemory(memoryOptions{Backend: backend, Dir: dir}, keyB)
			if err == nil || !strings.Contains(err.Error(), "belongs to "+keyA) {
				t.Fatalf("mismatch err = %v", err)
			}
		})
	}
}

func TestMemoryFilesOnlyForChosenBackend(t *testing.T) {
	for _, backend := range memoryBackends {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			s := openBackend(t, backend, dir, keyA)
			seedMemory(t, s)
			s.Close()
			var files []string
			filepath.WalkDir(filepath.Join(dir, memoryDirName), func(p string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					rel, _ := filepath.Rel(keyPath(dir, keyA), p)
					files = append(files, filepath.ToSlash(rel))
				}
				return nil
			})
			has := func(name string) bool { return slices.Contains(files, name) }
			md := has("index.md") || has("index.json") || has("issues/1.md")
			logs := has("issues.jsonl") || has("runs.jsonl")
			db := has("memory.db")
			switch backend {
			case backendSQLite:
				if !db || md || logs {
					t.Fatalf("sqlite wrote %v", files)
				}
			case backendMarkdown:
				if !md || db || logs || !has("prs/10.md") || !has("runs/r1.md") {
					t.Fatalf("markdown wrote %v", files)
				}
			case backendLogfiles:
				if !logs || db || md || !has("prs.jsonl") {
					t.Fatalf("logfiles wrote %v", files)
				}
			}
		})
	}
}

func TestMarkdownLayout(t *testing.T) {
	dir := t.TempDir()
	s := openBackend(t, backendMarkdown, dir, keyA)
	seedMemory(t, s)
	base := keyPath(dir, keyA)
	b, _ := os.ReadFile(filepath.Join(base, "issues", "1.md"))
	if !bytes.HasPrefix(b, []byte("---\n")) || !bytes.Contains(b, []byte(`repo: "`+keyA+`"`)) || !bytes.Contains(b, []byte("Login page crashes")) {
		t.Fatalf("issues/1.md = %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(base, "prs", "10.md")); !bytes.Contains(b, []byte("\nissue: 1\n")) {
		t.Fatalf("prs/10.md = %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(base, "index.md")); !bytes.Contains(b, []byte(`repo: "`+keyA+`"`)) {
		t.Fatalf("index.md = %s", b)
	}
	var idx map[string]any
	if b, _ := os.ReadFile(filepath.Join(base, "index.json")); json.Unmarshal(b, &idx) != nil || idx["repo"] != keyA {
		t.Fatalf("index.json = %s", b)
	}
}

func TestLogfilesHeader(t *testing.T) {
	dir := t.TempDir()
	s := openBackend(t, backendLogfiles, dir, keyA)
	seedMemory(t, s)
	b, _ := os.ReadFile(filepath.Join(keyPath(dir, keyA), "issues.jsonl"))
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 5 || !strings.Contains(lines[0], `"type":"header"`) || !strings.Contains(lines[0], `"repo":"`+keyA+`"`) {
		t.Fatalf("issues.jsonl = %s", b)
	}
	for _, l := range lines[1:] {
		if !strings.Contains(l, `"repo":"`+keyA+`"`) {
			t.Fatalf("record without repo: %s", l)
		}
	}
}

func TestSQLiteSchema(t *testing.T) {
	dir := t.TempDir()
	s := openBackend(t, backendSQLite, dir, keyA)
	s.Close()
	path := filepath.Join(keyPath(dir, keyA), "memory.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil || n != len(sqliteMigrations) {
		t.Fatalf("migrations = %d, %v", n, err)
	}
	var repo string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'repo'`).Scan(&repo); err != nil || repo != keyA {
		t.Fatalf("meta repo = %q, %v", repo, err)
	}
	for _, table := range []string{"issues", "pull_requests", "issue_prs", "runs", "memory_fts"} {
		if err := db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("table %s: %v", table, err)
		}
	}
	db.Close()
	// Reopening applies nothing twice.
	s = openBackend(t, backendSQLite, dir, keyA)
	s.Close()
}

func TestMemoryOptions(t *testing.T) {
	paths = pathOptions{} // a prior run() leaves Start set
	if err := (memoryOptions{Backend: "log"}).check(); err == nil || !strings.Contains(err.Error(), "sqlite, markdown or logfiles") {
		t.Fatalf("bad backend err = %v", err)
	}
	wd := t.TempDir()
	t.Chdir(wd)
	wd, _ = os.Getwd()
	if d, _ := (memoryOptions{}).baseDir(); d != wd {
		t.Fatalf("default dir = %q, want %q", d, wd)
	}
	other := t.TempDir()
	if d, _ := (memoryOptions{Dir: other}).baseDir(); d != other {
		t.Fatalf("explicit dir = %q", d)
	}
	if d, _ := (memoryOptions{Dir: "notes/mem"}).baseDir(); d != filepath.Join(wd, "notes", "mem") {
		t.Fatalf("relative dir = %q", d)
	}
	s := openBackend(t, backendSQLite, "notes/mem", keyA)
	s.Close()
	if _, err := os.Stat(filepath.Join(wd, "notes", "mem", memoryDirName, "github.com", "vic", "alpha", "memory.db")); err != nil {
		t.Fatal(err)
	}
}

func TestRepoKey(t *testing.T) {
	for _, c := range []struct{ api, repo, want string }{
		{"https://api.github.com", "Vic/Demo", "github.com/vic/demo"},
		{"", "vic/demo", "github.com/vic/demo"},
		{"https://ghe.example.com/api/v3", "Team/App", "ghe.example.com/team/app"},
	} {
		if got := repoKey(c.api, c.repo, ""); got != c.want {
			t.Errorf("repoKey(%q, %q) = %q, want %q", c.api, c.repo, got, c.want)
		}
	}
	root := t.TempDir()
	if k := repoKey("", "", root); !strings.HasPrefix(k, "local/") || k != repoKey("", "", root) || k == repoKey("", "", t.TempDir()) {
		t.Fatalf("local key = %q", k)
	}
	base := "/m"
	if got := keyPath(base, "github.com/vic/demo"); got != filepath.Join(base, memoryDirName, "github.com", "vic", "demo") {
		t.Fatalf("keyPath = %q", got)
	}
	for _, odd := range []string{"github.com/../x", "github.com/a b/c", "a/b/c/d", "github.com/vic/.."} {
		if got := keyPath(base, odd); !strings.HasPrefix(got, filepath.Join(base, memoryDirName, "_hash")+string(filepath.Separator)) {
			t.Errorf("keyPath(%q) = %q, want a hash folder", odd, got)
		}
	}
}

func TestLinkedIssue(t *testing.T) {
	for _, c := range []struct {
		head, body string
		want       int
	}{
		{"amf/issue-12", "", 12},
		{"feature", "This Closes #7.", 7},
		{"feature", "fixes: #8", 8},
		{"feature", "see #9", 0},
	} {
		if got := linkedIssue(c.head, c.body); got != c.want {
			t.Errorf("linkedIssue(%q, %q) = %d, want %d", c.head, c.body, got, c.want)
		}
	}
}

// fakeSyncGitHub serves two pages of issues and of pulls, and records the
// since each issues request asked for.
func fakeSyncGitHub(t *testing.T) (*httptest.Server, *[]string) {
	var sinces []string
	var srv *httptest.Server
	page := func(w http.ResponseWriter, r *http.Request, pages [][]map[string]any) {
		p := 0
		fmt.Sscan(r.URL.Query().Get("page"), &p)
		if p+1 < len(pages) {
			q := r.URL.Query()
			q.Set("page", fmt.Sprint(p+1))
			w.Header().Set("Link", fmt.Sprintf("<%s%s?%s>; rel=\"next\"", srv.URL, r.URL.Path, q.Encode()))
		}
		writeJSON(w, 200, pages[p])
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/vic/demo/issues", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			sinces = append(sinces, r.URL.Query().Get("since"))
		}
		page(w, r, [][]map[string]any{
			{{"number": 1, "title": "Login page crashes", "body": "empty password", "state": "closed", "updated_at": "2026-01-01T00:00:00Z"},
				{"number": 2, "title": "Dark mode", "state": "open", "updated_at": "2026-01-02T00:00:00Z"}},
			{{"number": 3, "title": "CSV export is slow", "state": "open", "updated_at": "2026-01-03T00:00:00Z"},
				{"number": 11, "title": "a PR", "state": "open", "pull_request": map[string]any{}, "updated_at": "2026-01-03T00:00:00Z"}},
		})
	})
	merged := "2026-01-05T00:00:00Z"
	mux.HandleFunc("GET /repos/vic/demo/pulls", func(w http.ResponseWriter, r *http.Request) {
		page(w, r, [][]map[string]any{
			{{"number": 10, "title": "Fix #1: Login page crashes", "state": "closed", "merged_at": merged, "updated_at": "2026-01-05T00:00:00Z", "head": map[string]any{"ref": "amf/issue-1"}}},
			{{"number": 11, "title": "Faster CSV", "body": "Fixes #3", "state": "open", "updated_at": "2026-01-04T00:00:00Z", "head": map[string]any{"ref": "csv"}},
				{"number": 12, "title": "Unlinked", "state": "open", "updated_at": "2026-01-03T00:00:00Z", "head": map[string]any{"ref": "misc"}}},
		})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &sinces
}

func TestMemorySync(t *testing.T) {
	for _, backend := range memoryBackends {
		t.Run(backend, func(t *testing.T) {
			srv, sinces := fakeSyncGitHub(t)
			gh := newGitHub(Config{Token: "t", Repo: "vic/demo", APIURL: srv.URL})
			s := openBackend(t, backend, t.TempDir(), keyA)
			res, err := syncMemory(context.Background(), gh, s, false)
			if err != nil {
				t.Fatal(err)
			}
			if res.Issues != 3 || res.PRs != 3 || res.Linked != 2 {
				t.Fatalf("sync = %+v", res)
			}
			h1, _ := s.History(1)
			if len(h1.PRs) != 1 || h1.PRs[0].Number != 10 || !h1.PRs[0].Merged {
				t.Fatalf("issue 1 PRs = %+v", h1.PRs)
			}
			if h3, _ := s.History(3); len(h3.PRs) != 1 || h3.PRs[0].Number != 11 {
				t.Fatalf("issue 3 PRs = %+v", h3.PRs)
			}
			if h11, _ := s.History(11); h11.Issue != nil {
				t.Fatal("a PR was stored as an issue")
			}
			if _, err := syncMemory(context.Background(), gh, s, false); err != nil {
				t.Fatal(err)
			}
			if len(*sinces) != 2 || (*sinces)[0] != "" || (*sinces)[1] != res.Cursor {
				t.Fatalf("since = %q, want incremental from %q", *sinces, res.Cursor)
			}
		})
	}
}

// memoryCLI runs amf in a fresh repo wired to a sync fake.
func memoryCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	srv, _ := fakeSyncGitHub(t)
	return memoryCLIAt(t, srv, args...)
}

func memoryCLIAt(t *testing.T, srv *httptest.Server, args ...string) (string, error) {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)
	var out, errb bytes.Buffer
	err := run(context.Background(), args, strings.NewReader(""), &out, &errb)
	if err != nil {
		return out.String() + errb.String(), err
	}
	return out.String(), nil
}

func TestMemoryCommands(t *testing.T) {
	root, _ := setupRepo(t)
	t.Chdir(root)
	srv, _ := fakeSyncGitHub(t)
	if out, err := memoryCLIAt(t, srv, "memory", "sync"); err != nil || !strings.Contains(out, "synced 3 issues and 3 pull requests (2 linked") {
		t.Fatalf("sync: %q, %v", out, err)
	}
	out, err := memoryCLIAt(t, srv, "memory", "similar", "login page crashes with an empty password", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var hits []Hit
	if err := json.Unmarshal([]byte(out), &hits); err != nil || len(hits) == 0 || hits[0].Issue != 1 {
		t.Fatalf("similar --json = %s (%v)", out, err)
	}
	out, err = memoryCLIAt(t, srv, "memory", "show", "1", "--json")
	var h IssueHistory
	if err != nil || json.Unmarshal([]byte(out), &h) != nil || h.Issue == nil || len(h.PRs) != 1 {
		t.Fatalf("show --json = %s (%v)", out, err)
	}
	if out, err := memoryCLIAt(t, srv, "memory", "show", "1"); err != nil || !strings.Contains(out, "#10") {
		t.Fatalf("show = %q, %v", out, err)
	}
	if out, err := memoryCLIAt(t, srv, "memory", "similar", "3"); err != nil || strings.Contains(out, "issue #3 ") {
		t.Fatalf("similar 3 = %q, %v", out, err)
	}
	// Only the default store was written, and git ignores it.
	// (The fake's host has a port, so its key gets a hash folder.)
	dbs, _ := filepath.Glob(filepath.Join(root, memoryDirName, "_hash", "*", "*"))
	if len(dbs) != 1 || filepath.Base(dbs[0]) != "memory.db" {
		t.Fatalf("memory files = %v", dbs)
	}
	if st := mustGit(t, root, "status", "--porcelain"); st != "" {
		t.Fatalf("memory shows in git status: %q", st)
	}
	if _, err := memoryCLIAt(t, srv, "memory", "sync", "--memory-backend", "log"); err == nil || !strings.Contains(err.Error(), "sqlite, markdown or logfiles") {
		t.Fatalf("bad backend: %v", err)
	}
	if _, err := memoryCLIAt(t, srv, "memory", "nope"); err == nil {
		t.Fatal("unknown subcommand should fail")
	}
}

func TestRunRecordsMemory(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", Body: "Please add feature.txt", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	claude := fakeClaude(t, `
prompt=$(cat)
case "$prompt" in *"Similar past work"*"#7"*) ;; *) echo "no memory note: $prompt" >&2; exit 4;; esac
echo hello > feature.txt
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"Added feature.txt."}'
`)
	r, log := newTestRunner(t, srv, root, claude)
	mdir := t.TempDir()
	mo := memoryOptions{Backend: backendSQLite, Dir: mdir}
	pre, err := openMemory(mo, repoKey(r.GH.base, r.Repo, root))
	if err != nil {
		t.Fatal(err)
	}
	pre.UpsertIssue(MemIssue{Number: 7, Title: "Add feature file", Body: "add feature.txt to the repo", State: "closed"})
	pre.UpsertPR(MemPR{Number: 70, Title: "Fix #7: Add feature file", State: "merged", Merged: true, Issue: 7})
	pre.Close()

	r.Command = "watch"
	r.openMemory(mo)
	if r.Memory == nil {
		t.Fatalf("memory not opened:\n%s", log)
	}
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\n%s", err, log)
	}
	if !strings.Contains(log.String(), "similar past work: issue #7") || !strings.Contains(log.String(), "may duplicate already-merged work") {
		t.Fatalf("no triage events:\n%s", log)
	}
	h, err := r.Memory.History(1)
	if err != nil || h.Issue == nil || len(h.Runs) != 1 || len(h.PRs) != 1 {
		t.Fatalf("history = %+v, %v", h, err)
	}
	run := h.Runs[0]
	if run.Command != "watch" || run.Outcome != "success" || run.PR != 101 || run.FinishedAt == "" || len(run.Agents) != 1 {
		t.Fatalf("run = %+v", run)
	}
	if h.PRs[0].Number != 101 || h.PRs[0].Issue != 1 {
		t.Fatalf("PR = %+v", h.PRs[0])
	}
	r.Close()
	// The PR's commit carries no memory files.
	if files := mustGit(t, root, "ls-tree", "-r", "--name-only", "amf/issue-1"); strings.Contains(files, memoryDirName) {
		t.Fatalf("memory committed: %s", files)
	}
}

func TestRunRecordsFailure(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Do nothing", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	claude := fakeClaude(t, `cat >/dev/null; echo '{"type":"result","subtype":"success","is_error":false,"result":"nothing"}'`)
	r, _ := newTestRunner(t, srv, root, claude)
	r.openMemory(memoryOptions{Backend: backendLogfiles, Dir: t.TempDir()})
	if err := r.Run(context.Background(), 1); err == nil {
		t.Fatal("expected failure")
	}
	h, _ := r.Memory.History(1)
	if len(h.Runs) != 1 || h.Runs[0].Outcome != "failed" || h.Runs[0].Error == "" || h.Runs[0].Command != "run" {
		t.Fatalf("runs = %+v", h.Runs)
	}
	r.Close()
}

// A broken memory never breaks a run.
func TestMemoryFailureTolerance(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", State: "open", Labels: []Label{{Name: "amf:ready"}}}
	claude := fakeClaude(t, `cat >/dev/null; echo hi > f.txt; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'`)
	r, log := newTestRunner(t, srv, root, claude)
	// A file where the memory dir should be: opening fails.
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o644)
	r.openMemory(memoryOptions{Backend: backendSQLite, Dir: blocker})
	if r.Memory != nil || !strings.Contains(log.String(), "memory is off") {
		t.Fatalf("memory should be off with a warning:\n%s", log)
	}
	// A store whose every write fails.
	r.Memory = brokenStore{}
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run with broken memory: %v\n%s", err, log)
	}
	if !strings.Contains(log.String(), "could not record the run") {
		t.Fatalf("no warning:\n%s", log)
	}
}

type brokenStore struct{}

var errBroken = errors.New("disk on fire")

func (brokenStore) Key() string                       { return keyA }
func (brokenStore) UpsertIssue(MemIssue) error        { return errBroken }
func (brokenStore) UpsertPR(MemPR) error              { return errBroken }
func (brokenStore) RecordRun(MemRun) error            { return errBroken }
func (brokenStore) History(int) (IssueHistory, error) { return IssueHistory{}, errBroken }
func (brokenStore) Search(string, int) ([]Hit, error) { return nil, errBroken }
func (brokenStore) SyncCursor() (string, error)       { return "", errBroken }
func (brokenStore) SetSyncCursor(string) error        { return errBroken }
func (brokenStore) Close() error                      { return nil }

// The default run writes only memory.db, in the current directory; the
// markdown and logfiles backends write their folders only when chosen.
func TestRunMemoryBackendFlag(t *testing.T) {
	for _, backend := range []string{"", backendMarkdown, backendLogfiles} {
		t.Run(cmpOr(backend, "default"), func(t *testing.T) {
			args := []string{"run", "1"}
			if backend != "" {
				args = append(args, "--memory-backend", backend)
			}
			_, root := draftCLI(t, args...)
			var files []string
			filepath.WalkDir(filepath.Join(root, memoryDirName), func(p string, d os.DirEntry, err error) error {
				if err == nil && !d.IsDir() {
					files = append(files, filepath.Base(p))
				}
				return nil
			})
			has := func(n string) bool { return slices.Contains(files, n) }
			switch backend {
			case "":
				if !has("memory.db") || has("index.json") || has("runs.jsonl") {
					t.Fatalf("default wrote %v", files)
				}
			case backendMarkdown:
				if !has("index.json") || has("memory.db") || has("runs.jsonl") {
					t.Fatalf("markdown wrote %v", files)
				}
			case backendLogfiles:
				if !has("runs.jsonl") || has("memory.db") || has("index.json") {
					t.Fatalf("logfiles wrote %v", files)
				}
			}
			if st := mustGit(t, root, "status", "--porcelain"); strings.Contains(st, memoryDirName) {
				t.Fatalf("memory shows in git status: %q", st)
			}
		})
	}
}

// The checkout may be reached through a symlink (macOS's /var), and memory
// inside it must still be excluded from git.
func TestExcludeMemoryThroughSymlink(t *testing.T) {
	root, _ := setupRepo(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skip(err)
	}
	if err := os.MkdirAll(filepath.Join(root, memoryDirName, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, memoryDirName, "x", "memory.db"), []byte("x"), 0o644)
	if err := excludeMemory(root, memoryOptions{Dir: link}); err != nil {
		t.Fatal(err)
	}
	if st := mustGit(t, root, "status", "--porcelain"); st != "" {
		t.Fatalf("git status = %q", st)
	}
}
