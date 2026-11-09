package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type tagReleaseRequestLog struct {
	mu     sync.Mutex
	writes []string
}

func (l *tagReleaseRequestLog) wrote() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.writes...)
}

// newTagReleaseServer is an offline GitHub stand-in. It answers the repo check
// and records any attempt to write; every test handler only serves reads.
func newTagReleaseServer(t *testing.T, handle http.HandlerFunc) (*httptest.Server, *tagReleaseRequestLog) {
	t.Helper()
	return newTagReleaseServerWithRepo(t, handle, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"default_branch": "main"})
	})
}

func newTagReleaseServerWithRepo(t *testing.T, handle http.HandlerFunc, repo http.HandlerFunc) (*httptest.Server, *tagReleaseRequestLog) {
	t.Helper()
	log := &tagReleaseRequestLog{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/vic/demo" {
			repo(w, r)
			return
		}
		if r.Method != http.MethodGet {
			log.mu.Lock()
			log.writes = append(log.writes, r.Method+" "+r.URL.Path)
			log.mu.Unlock()
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "read-only test server"})
			return
		}
		handle(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, log
}

func tagReleaseEnv(t *testing.T, srv *httptest.Server) {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)
}

func runTagRelease(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	err := run(context.Background(), args, nil, &out, &stderr)
	return out.String(), stderr.String(), err
}

func tagReleaseNextLink(r *http.Request, page int) string {
	q := r.URL.Query()
	q.Set("page", strconv.Itoa(page))
	return fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, r.Host, r.URL.Path, q.Encode())
}

func tagReleaseTag(name, commit string) Tag {
	var tag Tag
	tag.Name = name
	tag.Commit.SHA = commit
	return tag
}

func tagReleaseAsset(id int64, name string) ReleaseAsset {
	return ReleaseAsset{ID: id, Name: name, Size: 12, ContentType: "application/zip", State: "uploaded", DownloadCount: 3, BrowserDownloadURL: "https://github.com/vic/demo/releases/download/a/" + name}
}

func tagReleaseRelease(id int64, tag, name string, draft, prerelease bool, created time.Time) Release {
	rel := Release{ID: id, TagName: tag, Name: name, TargetCommitish: "main", Draft: draft, Prerelease: prerelease, CreatedAt: created, HTMLURL: fmt.Sprintf("https://github.com/vic/demo/releases/tag/%s", tag)}
	if !draft {
		published := created.Add(time.Hour)
		rel.PublishedAt = &published
	}
	return rel
}

func TestTagListPaginatesSortsAndLimits(t *testing.T) {
	now := time.Now()
	_ = now
	srv, log := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/vic/demo/tags" {
			t.Errorf("unexpected path %s", r.URL.Path)
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("Link", tagReleaseNextLink(r, 2))
			writeJSON(w, http.StatusOK, []Tag{tagReleaseTag("b", "bbb"), tagReleaseTag("d", "ddd")})
		case "2":
			writeJSON(w, http.StatusOK, []Tag{tagReleaseTag("c", "ccc"), tagReleaseTag("a", "aaa")})
		default:
			writeJSON(w, http.StatusOK, []Tag{})
		}
	})
	tagReleaseEnv(t, srv)

	out, _, err := runTagRelease(t, "tag", "list", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got []listedTag
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if want := []listedTag{{"a", "aaa"}, {"b", "bbb"}, {"c", "ccc"}, {"d", "ddd"}}; !tagReleaseEqualTags(got, want) {
		t.Fatalf("tags = %+v, want %+v", got, want)
	}

	out, stderr, err := runTagRelease(t, "tag", "list", "--limit", "2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a    aaa") || !strings.Contains(out, "b    bbb") || strings.Contains(out, "c    ccc") {
		t.Fatalf("limited tags = %q", out)
	}
	if !strings.Contains(stderr, "Showing 2 of 4 tags") {
		t.Fatalf("stderr = %q", stderr)
	}
	if wrote := log.wrote(); len(wrote) != 0 {
		t.Fatalf("tag list wrote %v", wrote)
	}
}

func tagReleaseEqualTags(a, b []listedTag) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTagReleasePaginationGuards(t *testing.T) {
	t.Run("tag cycle", func(t *testing.T) {
		srv, _ := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Link", tagReleaseNextLink(r, 1))
			writeJSON(w, http.StatusOK, []Tag{tagReleaseTag("v1", "abc")})
		})
		tagReleaseEnv(t, srv)
		_, _, err := runTagRelease(t, "tag", "list")
		if err == nil || !strings.Contains(err.Error(), "cycles back") {
			t.Fatalf("error = %v, want a pagination cycle", err)
		}
	})

	t.Run("foreign host", func(t *testing.T) {
		var hits int
		evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
		defer evil.Close()
		srv, _ := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/vic/demo/tags?page=2>; rel="next"`, evil.URL))
			writeJSON(w, http.StatusOK, []Tag{tagReleaseTag("v1", "abc")})
		})
		tagReleaseEnv(t, srv)
		_, _, err := runTagRelease(t, "tag", "list")
		if err == nil || !strings.Contains(err.Error(), "outside") {
			t.Fatalf("error = %v, want a foreign-host refusal", err)
		}
		if hits != 0 {
			t.Fatalf("foreign server was contacted %d times", hits)
		}
	})

	t.Run("other endpoint", func(t *testing.T) {
		srv, _ := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s/repos/vic/demo/issues?page=2>; rel="next"`, r.Host))
			writeJSON(w, http.StatusOK, []Tag{tagReleaseTag("v1", "abc")})
		})
		tagReleaseEnv(t, srv)
		_, _, err := runTagRelease(t, "tag", "list")
		if err == nil || !strings.Contains(err.Error(), "leaves") {
			t.Fatalf("error = %v, want an endpoint-scope refusal", err)
		}
	})

	t.Run("asset cycle", func(t *testing.T) {
		srv, _ := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/repos/vic/demo/releases/tags/v1":
				writeJSON(w, http.StatusOK, tagReleaseRelease(7, "v1", "one", false, false, time.Now()))
			case r.URL.Path == "/repos/vic/demo/releases/7/assets":
				w.Header().Set("Link", tagReleaseNextLink(r, 1))
				writeJSON(w, http.StatusOK, []ReleaseAsset{tagReleaseAsset(1, "a.zip")})
			case r.URL.Path == "/repos/vic/demo/git/ref/tags/v1":
				writeJSON(w, http.StatusOK, tagRef{Ref: "refs/tags/v1", Object: struct {
					Type string `json:"type"`
					SHA  string `json:"sha"`
				}{Type: "commit", SHA: "abc"}})
			default:
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			}
		})
		tagReleaseEnv(t, srv)
		_, _, err := runTagRelease(t, "release", "view", "v1")
		if err == nil || !strings.Contains(err.Error(), "cycles back") {
			t.Fatalf("error = %v, want an asset pagination cycle", err)
		}
	})
}

func TestTagViewPeelingAndRejections(t *testing.T) {
	makeObj := func(sha, name, typ, target string) tagObject {
		var obj tagObject
		obj.SHA, obj.Tag = sha, name
		obj.Object.Type, obj.Object.SHA = typ, target
		return obj
	}
	makeRef := func(name, typ, sha string) tagRef {
		var ref tagRef
		ref.Ref = "refs/tags/" + name
		ref.Object.Type, ref.Object.SHA = typ, sha
		return ref
	}
	run := func(t *testing.T, ref tagRef, objects map[string]tagObject) (string, string, error) {
		t.Helper()
		srv, _ := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/repos/vic/demo/git/ref/tags/v1":
				writeJSON(w, http.StatusOK, ref)
			case strings.HasPrefix(r.URL.Path, "/repos/vic/demo/git/tags/"):
				sha := strings.TrimPrefix(r.URL.Path, "/repos/vic/demo/git/tags/")
				obj, ok := objects[sha]
				if !ok {
					writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
					return
				}
				writeJSON(w, http.StatusOK, obj)
			default:
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			}
		})
		tagReleaseEnv(t, srv)
		return runTagRelease(t, "tag", "view", "v1", "--json")
	}

	t.Run("lightweight", func(t *testing.T) {
		out, _, err := run(t, makeRef("v1", "commit", "c0ffee"), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, `"commit":"c0ffee"`) {
			t.Fatalf("output = %s", out)
		}
	})
	t.Run("nested annotated", func(t *testing.T) {
		objs := map[string]tagObject{
			"t1": makeObj("t1", "v1", "tag", "t2"),
			"t2": makeObj("t2", "inner", "commit", "c0ffee"),
		}
		out, _, err := run(t, makeRef("v1", "tag", "t1"), objs)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, `"commit":"c0ffee"`) {
			t.Fatalf("output = %s", out)
		}
	})
	t.Run("cycle", func(t *testing.T) {
		objs := map[string]tagObject{"t1": makeObj("t1", "v1", "tag", "t1")}
		_, _, err := run(t, makeRef("v1", "tag", "t1"), objs)
		if err == nil || !strings.Contains(err.Error(), "cycle") {
			t.Fatalf("error = %v, want a cycle", err)
		}
	})
	t.Run("depth", func(t *testing.T) {
		objs := map[string]tagObject{}
		for i := 1; i <= maxTagDepth+1; i++ {
			target := fmt.Sprintf("t%d", i+1)
			if i == maxTagDepth+1 {
				target = "c0ffee"
			}
			typ := "commit"
			if i < maxTagDepth+1 {
				typ = "tag"
			}
			name := fmt.Sprintf("inner%d", i)
			if i == 1 {
				name = "v1"
			}
			objs[fmt.Sprintf("t%d", i)] = makeObj(fmt.Sprintf("t%d", i), name, typ, target)
		}
		_, _, err := run(t, makeRef("v1", "tag", "t1"), objs)
		if err == nil || !strings.Contains(err.Error(), "nested more than") {
			t.Fatalf("error = %v, want a depth error", err)
		}
	})
	t.Run("non-commit", func(t *testing.T) {
		_, _, err := run(t, makeRef("v1", "tree", "abc"), nil)
		if err == nil || !strings.Contains(err.Error(), "tree object") {
			t.Fatalf("error = %v, want a non-commit error", err)
		}
	})
	t.Run("nested non-commit", func(t *testing.T) {
		objs := map[string]tagObject{"t1": makeObj("t1", "v1", "blob", "abc")}
		_, _, err := run(t, makeRef("v1", "tag", "t1"), objs)
		if err == nil || !strings.Contains(err.Error(), "blob object") {
			t.Fatalf("error = %v, want a non-commit error", err)
		}
	})
	t.Run("incomplete", func(t *testing.T) {
		_, _, err := run(t, makeRef("v1", "commit", ""), nil)
		if err == nil || !strings.Contains(err.Error(), "incomplete") {
			t.Fatalf("error = %v, want an incomplete error", err)
		}
	})
	t.Run("wrong ref", func(t *testing.T) {
		ref := makeRef("v1", "commit", "abc")
		ref.Ref = "refs/tags/other"
		_, _, err := run(t, ref, nil)
		if err == nil || !strings.Contains(err.Error(), "returned tag ref") {
			t.Fatalf("error = %v, want a ref identity error", err)
		}
	})
	t.Run("wrong object sha", func(t *testing.T) {
		obj := makeObj("other", "v1", "commit", "abc")
		_, _, err := run(t, makeRef("v1", "tag", "t1"), map[string]tagObject{"t1": obj})
		if err == nil || !strings.Contains(err.Error(), "returned tag object") {
			t.Fatalf("error = %v, want an object identity error", err)
		}
	})
	t.Run("annotated object alias", func(t *testing.T) {
		obj := makeObj("t1", "historical-name", "commit", "abc")
		out, _, err := run(t, makeRef("v1", "tag", "t1"), map[string]tagObject{"t1": obj})
		if err != nil || !strings.Contains(out, `"commit":"abc"`) {
			t.Fatalf("aliased annotated tag: output=%s error=%v", out, err)
		}
	})
	t.Run("missing tag", func(t *testing.T) {
		srv, _ := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		})
		tagReleaseEnv(t, srv)
		_, _, err := runTagRelease(t, "tag", "view", "missing")
		if err == nil || !strings.Contains(err.Error(), `tag "missing" was not found`) {
			t.Fatalf("error = %v, want a missing-tag message", err)
		}
	})
}

func TestReleaseListFiltersSortsLimitsAndAssets(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	releases := []Release{
		tagReleaseRelease(1, "v1", "one", false, false, base.Add(1*time.Hour)),
		tagReleaseRelease(2, "v2-pre", "two", false, true, base.Add(2*time.Hour)),
		tagReleaseRelease(3, "v3-draft", "three", true, false, base.Add(3*time.Hour)),
		tagReleaseRelease(4, "v4", "four", false, false, base.Add(4*time.Hour)),
		tagReleaseRelease(5, "v5-pre", "five", false, true, base.Add(5*time.Hour)),
	}
	releases[0].Assets = []ReleaseAsset{tagReleaseAsset(99, "inline-partial.zip")}
	srv, log := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/vic/demo/releases":
			switch r.URL.Query().Get("page") {
			case "", "1":
				w.Header().Set("Link", tagReleaseNextLink(r, 2))
				writeJSON(w, http.StatusOK, releases[:3])
			case "2":
				writeJSON(w, http.StatusOK, releases[3:])
			default:
				writeJSON(w, http.StatusOK, []Release{})
			}
		case strings.HasPrefix(r.URL.Path, "/repos/vic/demo/releases/") && strings.HasSuffix(r.URL.Path, "/assets"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/vic/demo/releases/"), "/assets")
			n, _ := strconv.ParseInt(id, 10, 64)
			if n == 1 {
				switch r.URL.Query().Get("page") {
				case "", "1":
					w.Header().Set("Link", tagReleaseNextLink(r, 2))
					writeJSON(w, http.StatusOK, []ReleaseAsset{tagReleaseAsset(1, "one.zip")})
				case "2":
					writeJSON(w, http.StatusOK, []ReleaseAsset{tagReleaseAsset(2, "two.zip")})
				}
				return
			}
			writeJSON(w, http.StatusOK, []ReleaseAsset{tagReleaseAsset(n, fmt.Sprintf("asset-%d.zip", n))})
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		}
	})
	tagReleaseEnv(t, srv)

	list := func(args ...string) []listedRelease {
		t.Helper()
		out, _, err := runTagRelease(t, append([]string{"release", "list", "--json"}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		var got []listedRelease
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	all := list()
	if len(all) != 5 || all[0].ID != 5 || all[4].ID != 1 {
		t.Fatalf("all releases = %+v, want newest first", all)
	}
	if len(all[4].Assets) != 2 || all[4].Assets[0].Name != "one.zip" || all[4].Assets[1].Name != "two.zip" {
		t.Fatalf("assets = %+v, want both pages and not the inline array", all[4].Assets)
	}
	if got := list("--state", "published"); len(got) != 2 || got[0].ID != 4 || got[1].ID != 1 {
		t.Fatalf("published = %+v", got)
	}
	if got := list("--state", "draft"); len(got) != 1 || got[0].ID != 3 {
		t.Fatalf("draft = %+v", got)
	}
	if got := list("--state", "prerelease"); len(got) != 2 || got[0].ID != 5 || got[1].ID != 2 {
		t.Fatalf("prerelease = %+v", got)
	}
	if got := list("--state", "published", "--limit", "1"); len(got) != 1 || got[0].ID != 4 {
		t.Fatalf("limited = %+v", got)
	}
	out, stderr, err := runTagRelease(t, "release", "list", "--state", "published", "--limit", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "v4") || strings.Contains(out, "v1") || !strings.Contains(stderr, "Showing 1 of 2 releases") {
		t.Fatalf("output = %q, stderr = %q", out, stderr)
	}
	if wrote := log.wrote(); len(wrote) != 0 {
		t.Fatalf("release list wrote %v", wrote)
	}
}

func TestReleaseViewUsesPeeledCommitAndAllAssets(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rel := tagReleaseRelease(10, "v1.2.3", "Release 1.2.3", false, false, created)
	rel.TargetCommitish = "main"
	rel.Assets = []ReleaseAsset{tagReleaseAsset(98, "inline-partial.zip")}
	srv, _ := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/vic/demo/releases/tags/v1.2.3":
			writeJSON(w, http.StatusOK, rel)
		case r.URL.Path == "/repos/vic/demo/releases/10/assets":
			switch r.URL.Query().Get("page") {
			case "", "1":
				w.Header().Set("Link", tagReleaseNextLink(r, 2))
				writeJSON(w, http.StatusOK, []ReleaseAsset{tagReleaseAsset(1, "one.zip"), tagReleaseAsset(2, "two.zip")})
			case "2":
				writeJSON(w, http.StatusOK, []ReleaseAsset{tagReleaseAsset(3, "three.zip")})
			}
		case r.URL.Path == "/repos/vic/demo/git/ref/tags/v1.2.3":
			var ref tagRef
			ref.Ref = "refs/tags/v1.2.3"
			ref.Object.Type, ref.Object.SHA = "tag", "t1"
			writeJSON(w, http.StatusOK, ref)
		case r.URL.Path == "/repos/vic/demo/git/tags/t1":
			var obj tagObject
			obj.SHA, obj.Tag = "t1", "v1.2.3"
			obj.Object.Type, obj.Object.SHA = "tag", "t2"
			writeJSON(w, http.StatusOK, obj)
		case r.URL.Path == "/repos/vic/demo/git/tags/t2":
			var obj tagObject
			obj.SHA, obj.Tag = "t2", "inner"
			obj.Object.Type, obj.Object.SHA = "commit", "feedface"
			writeJSON(w, http.StatusOK, obj)
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		}
	})
	tagReleaseEnv(t, srv)

	out, _, err := runTagRelease(t, "release", "view", "v1.2.3", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got viewedRelease
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Commit == nil || *got.Commit != "feedface" {
		t.Fatalf("commit = %v, want the peeled commit", got.Commit)
	}
	if got.TargetCommitish != "main" {
		t.Fatalf("target_commitish = %q, want main", got.TargetCommitish)
	}
	if len(got.Assets) != 3 || got.Assets[0].Name != "one.zip" || got.Assets[2].Name != "three.zip" {
		t.Fatalf("assets = %+v, want every page", got.Assets)
	}
	for _, a := range got.Assets {
		if a.Name == "inline-partial.zip" {
			t.Fatalf("inline partial assets were trusted: %+v", got.Assets)
		}
	}

	out, _, err = runTagRelease(t, "release", "view", "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"feedface", "main", "assets:", "one.zip", "three.zip"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, "inline-partial.zip") {
		t.Fatalf("output contains an inline asset: %q", out)
	}
}

func TestReleaseDraftMissingTagIsUnknown(t *testing.T) {
	rel := tagReleaseRelease(7, "draft-tag", "Draft", true, false, time.Now())
	rel.TargetCommitish = "main"
	srv, _ := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/vic/demo/releases/tags/draft-tag":
			writeJSON(w, http.StatusOK, rel)
		case "/repos/vic/demo/releases/7/assets":
			writeJSON(w, http.StatusOK, []ReleaseAsset{})
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		}
	})
	tagReleaseEnv(t, srv)

	out, _, err := runTagRelease(t, "release", "view", "draft-tag", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got viewedRelease
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Commit != nil {
		t.Fatalf("commit = %v, want null for a draft without a tag", got.Commit)
	}
	if got.TargetCommitish != "main" {
		t.Fatalf("target_commitish = %q", got.TargetCommitish)
	}
	out, _, err = runTagRelease(t, "release", "view", "draft-tag")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "unknown") || !strings.Contains(out, "main") {
		t.Fatalf("output = %q, want an unknown commit and the target separately", out)
	}

	rel.Draft = false
	srv2, _ := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/vic/demo/releases/tags/draft-tag":
			writeJSON(w, http.StatusOK, rel)
		case "/repos/vic/demo/releases/7/assets":
			writeJSON(w, http.StatusOK, []ReleaseAsset{})
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		}
	})
	tagReleaseEnv(t, srv2)
	if _, _, err := runTagRelease(t, "release", "view", "draft-tag"); err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("error = %v, want a missing tag error for a published release", err)
	}
}

func TestReleaseLatest(t *testing.T) {
	created := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	rel := tagReleaseRelease(11, "v2.0.0", "Two", false, false, created)
	srv, _ := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/vic/demo/releases/latest":
			writeJSON(w, http.StatusOK, rel)
		case "/repos/vic/demo/releases/11/assets":
			writeJSON(w, http.StatusOK, []ReleaseAsset{tagReleaseAsset(1, "amf.zip")})
		case "/repos/vic/demo/git/ref/tags/v2.0.0":
			var ref tagRef
			ref.Ref = "refs/tags/v2.0.0"
			ref.Object.Type, ref.Object.SHA = "commit", "c0ffee"
			writeJSON(w, http.StatusOK, ref)
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		}
	})
	tagReleaseEnv(t, srv)

	out, _, err := runTagRelease(t, "release", "latest", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got viewedRelease
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if got.Tag != "v2.0.0" || got.Commit == nil || *got.Commit != "c0ffee" || len(got.Assets) != 1 {
		t.Fatalf("latest = %+v", got)
	}

	absent, _ := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	})
	tagReleaseEnv(t, absent)
	out, _, err = runTagRelease(t, "release", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No published releases in vic/demo.") {
		t.Fatalf("output = %q", out)
	}
	out, _, err = runTagRelease(t, "release", "latest", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "null" {
		t.Fatalf("JSON = %q, want null", out)
	}
}

func TestReleaseViewEscapedTag(t *testing.T) {
	rel := tagReleaseRelease(12, "release/1.0", "Slash", false, false, time.Now())
	srv, _ := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/repos/vic/demo/releases/tags/release%2F1.0":
			writeJSON(w, http.StatusOK, rel)
		case "/repos/vic/demo/releases/12/assets":
			writeJSON(w, http.StatusOK, []ReleaseAsset{})
		case "/repos/vic/demo/git/ref/tags/release%2F1.0":
			var ref tagRef
			ref.Ref = "refs/tags/release/1.0"
			ref.Object.Type, ref.Object.SHA = "commit", "c0ffee"
			writeJSON(w, http.StatusOK, ref)
		default:
			t.Errorf("unexpected path %s", r.URL.EscapedPath())
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		}
	})
	tagReleaseEnv(t, srv)
	out, _, err := runTagRelease(t, "release", "view", "release/1.0", "--json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"tag":"release/1.0"`) || !strings.Contains(out, `"commit":"c0ffee"`) {
		t.Fatalf("output = %s", out)
	}
}

func TestTagReleaseEmptyErrorsAndNoAuth(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		srv, _ := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/repos/vic/demo/tags":
				writeJSON(w, http.StatusOK, []Tag{})
			case r.URL.Path == "/repos/vic/demo/releases":
				writeJSON(w, http.StatusOK, []Release{})
			default:
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			}
		})
		tagReleaseEnv(t, srv)
		out, _, err := runTagRelease(t, "tag", "list")
		if err != nil || !strings.Contains(out, "No tags in vic/demo.") {
			t.Fatalf("tag list: %q, %v", out, err)
		}
		out, _, err = runTagRelease(t, "tag", "list", "--json")
		if err != nil || strings.TrimSpace(out) != "[]" {
			t.Fatalf("tag JSON: %q, %v", out, err)
		}
		out, _, err = runTagRelease(t, "release", "list")
		if err != nil || !strings.Contains(out, "No releases in vic/demo.") {
			t.Fatalf("release list: %q, %v", out, err)
		}
		out, _, err = runTagRelease(t, "release", "list", "--json")
		if err != nil || strings.TrimSpace(out) != "[]" {
			t.Fatalf("release JSON: %q, %v", out, err)
		}
	})

	t.Run("api error", func(t *testing.T) {
		srv, _ := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "boom"})
		})
		tagReleaseEnv(t, srv)
		_, _, err := runTagRelease(t, "tag", "list")
		if err == nil || !strings.Contains(err.Error(), "500 boom") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("no token", func(t *testing.T) {
		srv, log := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("the API should not be called without a token")
		})
		tagReleaseEnv(t, srv)
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "")
		_, _, err := runTagRelease(t, "tag", "list")
		if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN is not set") {
			t.Fatalf("error = %v", err)
		}
		if wrote := log.wrote(); len(wrote) != 0 {
			t.Fatalf("wrote %v", wrote)
		}
	})

	t.Run("rejected token", func(t *testing.T) {
		srv, _ := newTagReleaseServerWithRepo(t, func(w http.ResponseWriter, r *http.Request) {
			t.Error("the token should be rejected at the repo check")
		}, func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		})
		tagReleaseEnv(t, srv)
		_, _, err := runTagRelease(t, "tag", "list")
		if err == nil || !strings.Contains(err.Error(), "rejected GITHUB_TOKEN") {
			t.Fatalf("error = %v", err)
		}
		if strings.Contains(err.Error(), "test-token") {
			t.Fatalf("error leaked the token: %v", err)
		}
	})
}

func TestTagReleaseReadOnlyCommands(t *testing.T) {
	unknown := [][]string{
		{"tag", "delete"}, {"tag", "update"},
		{"release", "delete"}, {"release", "edit"},
		{"release", "upload"}, {"release", "verify"},
	}
	srv, log := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []any{})
	})
	tagReleaseEnv(t, srv)
	for _, args := range unknown {
		if _, _, err := runTagRelease(t, args...); !errors.Is(err, errUsage) {
			t.Errorf("amf %v: error = %v, want a usage error", args, err)
		}
	}
	for _, word := range []string{"delete", "update", "edit", "upload", "verify"} {
		if tagReleaseContains(tagSubcommands, word) || tagReleaseContains(releaseSubcommands, word) {
			t.Errorf("unknown subcommand %q is exposed", word)
		}
	}
	for _, word := range []string{"create", "stage", "publish"} {
		if !tagReleaseContains(releaseSubcommands, word) {
			t.Errorf("release subcommand %q is missing", word)
		}
	}
	if wrote := log.wrote(); len(wrote) != 0 {
		t.Fatalf("unknown commands wrote %v", wrote)
	}

	// Every advertised subcommand only reads. Give it enough data to finish.
	reads := [][]string{
		{"tag", "list"},
		{"tag", "view", "v1"},
		{"release", "list"},
		{"release", "view", "v1"},
		{"release", "latest"},
	}
	readSrv, readLog := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/vic/demo/tags":
			writeJSON(w, http.StatusOK, []Tag{tagReleaseTag("v1", "abc")})
		case r.URL.Path == "/repos/vic/demo/releases":
			writeJSON(w, http.StatusOK, []Release{tagReleaseRelease(1, "v1", "one", false, false, time.Now())})
		case r.URL.Path == "/repos/vic/demo/releases/tags/v1":
			writeJSON(w, http.StatusOK, tagReleaseRelease(1, "v1", "one", false, false, time.Now()))
		case r.URL.Path == "/repos/vic/demo/releases/latest":
			writeJSON(w, http.StatusOK, tagReleaseRelease(1, "v1", "one", false, false, time.Now()))
		case strings.HasPrefix(r.URL.Path, "/repos/vic/demo/releases/") && strings.HasSuffix(r.URL.Path, "/assets"):
			writeJSON(w, http.StatusOK, []ReleaseAsset{})
		case r.URL.Path == "/repos/vic/demo/git/ref/tags/v1":
			var ref tagRef
			ref.Ref = "refs/tags/v1"
			ref.Object.Type, ref.Object.SHA = "commit", "abc"
			writeJSON(w, http.StatusOK, ref)
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		}
	})
	tagReleaseEnv(t, readSrv)
	for _, args := range reads {
		if _, _, err := runTagRelease(t, args...); err != nil {
			t.Errorf("amf %v: %v", args, err)
		}
	}
	if wrote := readLog.wrote(); len(wrote) != 0 {
		t.Fatalf("read commands wrote %v", wrote)
	}
}

func tagReleaseContains(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

func TestTagReleaseRefusesHTTPRedirects(t *testing.T) {
	for _, args := range [][]string{{"tag", "list"}, {"tag", "view", "v1"}, {"release", "list"}, {"release", "view", "v1"}, {"release", "latest"}} {
		for _, preliminary := range []bool{true, false} {
			t.Run(fmt.Sprint(args, preliminary), func(t *testing.T) {
				received := make(chan string, 10)
				evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					received <- r.Header.Get("Authorization")
					writeJSON(w, 200, []any{})
				}))
				defer evil.Close()
				redirect := func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, evil.URL, http.StatusFound) }
				repo := func(w http.ResponseWriter, r *http.Request) {
					writeJSON(w, 200, map[string]string{"default_branch": "main"})
				}
				if preliminary {
					repo = redirect
				}
				srv, log := newTagReleaseServerWithRepo(t, redirect, repo)
				tagReleaseEnv(t, srv)
				_, _, err := runTagRelease(t, args...)
				if err == nil {
					t.Fatal("redirect unexpectedly accepted")
				}
				select {
				case auth := <-received:
					t.Fatalf("redirect target was contacted (Authorization=%q)", auth)
				default:
				}
				if got := log.wrote(); len(got) != 0 {
					t.Fatalf("writes: %v", got)
				}
			})
		}
	}
}

func TestReleaseViewRejectsMismatchedResponseTag(t *testing.T) {
	srv, _ := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, tagReleaseRelease(1, "other", "other", false, false, time.Now()))
	})
	tagReleaseEnv(t, srv)
	_, _, err := runTagRelease(t, "release", "view", "v1")
	if err == nil || !strings.Contains(err.Error(), "returned release tag") {
		t.Fatalf("error=%v", err)
	}
}

func TestReleaseViewFindsDraftWhenByTagOnlyReturnsPublished(t *testing.T) {
	for _, duplicates := range []bool{false, true} {
		t.Run(fmt.Sprint(duplicates), func(t *testing.T) {
			srv, log := newTagReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/repos/vic/demo/releases/tags/draft":
					writeJSON(w, 404, map[string]string{"message": "Not Found"})
				case "/repos/vic/demo/releases":
					rel := tagReleaseRelease(2, "draft", "Draft", true, false, time.Now())
					if r.URL.Query().Get("page") == "2" {
						writeJSON(w, 200, []Release{rel})
					} else {
						w.Header().Set("Link", tagReleaseNextLink(r, 2))
						first := tagReleaseRelease(1, "other", "Other", true, false, time.Now())
						if duplicates {
							first.TagName = "draft"
						}
						writeJSON(w, 200, []Release{first})
					}
				case "/repos/vic/demo/releases/2/assets":
					writeJSON(w, 200, []ReleaseAsset{})
				case "/repos/vic/demo/git/ref/tags/draft":
					writeJSON(w, 404, map[string]string{"message": "Not Found"})
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					writeJSON(w, 404, map[string]string{})
				}
			})
			tagReleaseEnv(t, srv)
			out, _, err := runTagRelease(t, "release", "view", "draft", "--json")
			if duplicates {
				if err == nil || !strings.Contains(err.Error(), "multiple visible releases") {
					t.Fatalf("error=%v", err)
				}
			} else if err != nil || !strings.Contains(out, `"commit":null`) || !strings.Contains(out, `"draft":true`) {
				t.Fatalf("draft output=%s error=%v", out, err)
			}
			if got := log.wrote(); len(got) != 0 {
				t.Fatalf("writes=%v", got)
			}
		})
	}
}
