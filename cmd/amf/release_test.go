package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type fakeTagObject struct {
	Type string `json:"type"`
	SHA  string `json:"sha"`
}

type fakeReleaseAPI struct {
	t   *testing.T
	mu  sync.Mutex
	url string

	tags    map[string]fakeTagObject
	objects map[string]fakeTagObject

	releases map[int64]*ghRelease
	assets   map[int64][]releaseAsset
	contents map[int64][]byte

	checkRuns    map[string][]checkRunFull
	statuses     map[string][]commitStatus
	required     []string
	requiredCode int

	nextReleaseID   int64
	nextAssetID     int64
	tagPageSize     int
	releasePageSize int

	creates, uploads, deletes, publishes int
	requests                             []string
	raceUploadContent                    []byte
	raceMoveTagSHA                       string
}

func newReleaseFake(t *testing.T) (*fakeReleaseAPI, *httptest.Server) {
	t.Helper()
	f := &fakeReleaseAPI{
		t: t, tags: map[string]fakeTagObject{}, objects: map[string]fakeTagObject{},
		releases: map[int64]*ghRelease{}, assets: map[int64][]releaseAsset{}, contents: map[int64][]byte{},
		checkRuns: map[string][]checkRunFull{}, statuses: map[string][]commitStatus{},
		nextReleaseID: 100, nextAssetID: 1000,
	}
	srv := httptest.NewServer(f)
	f.url = srv.URL
	t.Cleanup(srv.Close)
	t.Setenv("GITHUB_TOKEN", "tok")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)
	return f, srv
}

func (f *fakeReleaseAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())

	const repo = "/repos/vic/demo"
	switch {
	case r.Method == "GET" && r.URL.Path == repo:
		writeJSON(w, 200, map[string]any{"default_branch": "main"})
	case r.Method == "GET" && r.URL.Path == repo+"/tags":
		type listedTag struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		var tags []listedTag
		for name, obj := range f.tags {
			tags = append(tags, listedTag{Name: name, Commit: struct {
				SHA string `json:"sha"`
			}{obj.SHA}})
		}
		for i := 1; i < len(tags); i++ {
			for j := i; j > 0 && strings.Compare(tags[j].Name, tags[j-1].Name) < 0; j-- {
				tags[j], tags[j-1] = tags[j-1], tags[j]
			}
		}
		f.writeTagPage(w, r, tags)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, repo+"/git/ref/tags/"):
		name := strings.TrimPrefix(r.URL.Path, repo+"/git/ref/tags/")
		obj, ok := f.tags[name]
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		writeJSON(w, 200, map[string]any{"ref": "refs/tags/" + name, "object": map[string]string{"sha": obj.SHA, "type": obj.Type}})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, repo+"/git/tags/"):
		sha := strings.TrimPrefix(r.URL.Path, repo+"/git/tags/")
		obj, ok := f.objects[sha]
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		writeJSON(w, 200, map[string]any{"sha": sha, "object": map[string]string{"sha": obj.SHA, "type": obj.Type}})
	case r.Method == "POST" && r.URL.Path == repo+"/git/refs":
		var in struct{ Ref, SHA string }
		decode(f.t, r, &in)
		name := strings.TrimPrefix(in.Ref, "refs/tags/")
		if _, ok := f.tags[name]; ok {
			writeJSON(w, 422, map[string]string{"message": "Reference already exists"})
			return
		}
		f.tags[name] = fakeTagObject{Type: "commit", SHA: in.SHA}
		f.creates++
		writeJSON(w, 201, map[string]any{"ref": in.Ref, "object": map[string]string{"sha": in.SHA, "type": "commit"}})
	case r.Method == "GET" && r.URL.Path == repo+"/releases/latest":
		var latest *ghRelease
		for _, rel := range f.releases {
			if !rel.Draft && !rel.Prerelease && (latest == nil || rel.CreatedAt > latest.CreatedAt) {
				latest = rel
			}
		}
		if latest == nil {
			writeJSON(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		f.writeRelease(w, latest)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, repo+"/releases/tags/"):
		tag := strings.TrimPrefix(r.URL.Path, repo+"/releases/tags/")
		for _, rel := range f.releases {
			if rel.TagName == tag {
				f.writeRelease(w, rel)
				return
			}
		}
		writeJSON(w, 404, map[string]string{"message": "Not Found"})
	case r.Method == "GET" && r.URL.Path == repo+"/releases":
		var releases []*ghRelease
		for _, rel := range f.releases {
			releases = append(releases, rel)
		}
		slicesSortReleases(releases)
		f.writeReleasePage(w, r, releases)
	case r.Method == "POST" && r.URL.Path == repo+"/releases":
		var in ghRelease
		decode(f.t, r, &in)
		for _, rel := range f.releases {
			if rel.TagName == in.TagName {
				writeJSON(w, 422, map[string]string{"message": "already_exists"})
				return
			}
		}
		f.nextReleaseID++
		rel := &ghRelease{ID: f.nextReleaseID, TagName: in.TagName, TargetCommitish: in.TargetCommitish, Name: in.Name, Body: in.Body, Draft: true, Prerelease: in.Prerelease, CreatedAt: fmt.Sprintf("2026-01-%02dT00:00:00Z", len(f.releases)+1), HTMLURL: fmt.Sprintf("https://github.com/vic/demo/releases/%d", f.nextReleaseID)}
		f.releases[rel.ID] = rel
		f.writeRelease(w, rel)
	case r.Method == "PATCH" && strings.HasPrefix(r.URL.Path, repo+"/releases/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, repo+"/releases/"), 10, 64)
		rel, ok := f.releases[id]
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		var in struct {
			Draft bool `json:"draft"`
		}
		decode(f.t, r, &in)
		rel.Draft = in.Draft
		if !in.Draft {
			rel.PublishedAt = "2026-02-01T00:00:00Z"
			f.publishes++
		}
		f.writeRelease(w, rel)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, repo+"/releases/") && strings.HasSuffix(r.URL.Path, "/assets"):
		id := releaseIDFromAssetsPath(r.URL.Path, repo)
		assets := append([]releaseAsset(nil), f.assets[id]...)
		writeJSON(w, 200, assets)
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, repo+"/releases/") && strings.HasSuffix(r.URL.Path, "/assets"):
		id := releaseIDFromAssetsPath(r.URL.Path, repo)
		rel, ok := f.releases[id]
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		name := r.URL.Query().Get("name")
		data, _ := io.ReadAll(r.Body)
		if f.raceUploadContent != nil {
			f.nextAssetID++
			f.assets[id] = append(f.assets[id], releaseAsset{ID: f.nextAssetID, Name: name, Size: int64(len(f.raceUploadContent)), State: "uploaded", URL: fmt.Sprintf("%s/repos/vic/demo/releases/assets/%d", f.url, f.nextAssetID), BrowserDownloadURL: fmt.Sprintf("%s/repos/vic/demo/releases/assets/%d", f.url, f.nextAssetID)})
			f.contents[f.nextAssetID] = f.raceUploadContent
			f.raceUploadContent = nil
		}
		if f.raceMoveTagSHA != "" {
			for _, rel := range f.releases {
				if rel.ID == id {
					f.tags[rel.TagName] = fakeTagObject{Type: "commit", SHA: f.raceMoveTagSHA}
				}
			}
			f.raceMoveTagSHA = ""
		}
		for _, a := range f.assets[id] {
			if a.Name == name {
				writeJSON(w, 422, map[string]string{"message": "already_exists"})
				return
			}
		}
		f.nextAssetID++
		asset := releaseAsset{ID: f.nextAssetID, Name: name, Size: int64(len(data)), State: "uploaded", URL: fmt.Sprintf("%s/repos/vic/demo/releases/assets/%d", f.url, f.nextAssetID), BrowserDownloadURL: fmt.Sprintf("%s/repos/vic/demo/releases/assets/%d", f.url, f.nextAssetID)}
		f.assets[id] = append(f.assets[id], asset)
		f.contents[asset.ID] = data
		f.uploads++
		_ = rel
		writeJSON(w, 201, asset)
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, repo+"/releases/assets/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, repo+"/releases/assets/"), 10, 64)
		data, ok := f.contents[id]
		if !ok {
			writeJSON(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(200)
		_, _ = w.Write(data)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, repo+"/releases/assets/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, repo+"/releases/assets/"), 10, 64)
		for rid, assets := range f.assets {
			for i, a := range assets {
				if a.ID == id {
					f.assets[rid] = append(assets[:i], assets[i+1:]...)
					delete(f.contents, id)
					f.deletes++
					w.WriteHeader(204)
					return
				}
			}
		}
		writeJSON(w, 404, map[string]string{"message": "Not Found"})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, repo+"/commits/") && strings.HasSuffix(r.URL.Path, "/check-runs"):
		sha := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, repo+"/commits/"), "/check-runs")
		writeJSON(w, 200, map[string]any{"total_count": len(f.checkRuns[sha]), "check_runs": f.checkRuns[sha]})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, repo+"/commits/") && strings.HasSuffix(r.URL.Path, "/statuses"):
		sha := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, repo+"/commits/"), "/statuses")
		writeJSON(w, 200, f.statuses[sha])
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, repo+"/branches/") && strings.HasSuffix(r.URL.Path, "/protection/required_status_checks"):
		if f.requiredCode != 0 {
			writeJSON(w, f.requiredCode, map[string]string{"message": "nope"})
			return
		}
		writeJSON(w, 200, map[string]any{"contexts": f.required})
	default:
		writeJSON(w, 404, map[string]string{"message": "unexpected " + r.Method + " " + r.URL.Path})
	}
}

func releaseIDFromAssetsPath(path, repo string) int64 {
	rest := strings.TrimSuffix(strings.TrimPrefix(path, repo+"/releases/"), "/assets")
	id, _ := strconv.ParseInt(rest, 10, 64)
	return id
}

func (f *fakeReleaseAPI) writeRelease(w http.ResponseWriter, rel *ghRelease) {
	out := *rel
	out.Assets = append([]releaseAsset(nil), f.assets[rel.ID]...)
	out.UploadURL = fmt.Sprintf("%s/repos/vic/demo/releases/%d/assets{?name,label}", f.url, rel.ID)
	writeJSON(w, 200, out)
}

func (f *fakeReleaseAPI) writeTagPage(w http.ResponseWriter, r *http.Request, tags any) {
	per := f.tagPageSize
	if per <= 0 {
		per = 100
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	rv := reflect.ValueOf(tags)
	total := rv.Len()
	start, end := min((page-1)*per, total), min(page*per, total)
	if end < total {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page+1))
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, f.url, r.URL.Path, q.Encode()))
	}
	if total == 0 {
		writeJSON(w, 200, tags)
		return
	}
	writeJSON(w, 200, rv.Slice(start, end).Interface())
}

func (f *fakeReleaseAPI) writeReleasePage(w http.ResponseWriter, r *http.Request, releases []*ghRelease) {
	per := f.releasePageSize
	if per <= 0 {
		per = 100
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	start, end := min((page-1)*per, len(releases)), min(page*per, len(releases))
	if end < len(releases) {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page+1))
		w.Header().Set("Link", fmt.Sprintf(`<%s%s?%s>; rel="next"`, f.url, r.URL.Path, q.Encode()))
	}
	out := make([]ghRelease, 0, end-start)
	for _, rel := range releases[start:end] {
		cp := *rel
		cp.UploadURL = fmt.Sprintf("%s/repos/vic/demo/releases/%d/assets{?name,label}", f.url, rel.ID)
		out = append(out, cp)
	}
	writeJSON(w, 200, out)
}

func slicesSortReleases(releases []*ghRelease) {
	for i := 1; i < len(releases); i++ {
		for j := i; j > 0 && releases[j].CreatedAt > releases[j-1].CreatedAt; j-- {
			releases[j], releases[j-1] = releases[j-1], releases[j]
		}
	}
}

func (f *fakeReleaseAPI) addTag(tag, commit string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags[tag] = fakeTagObject{Type: "commit", SHA: commit}
}

func (f *fakeReleaseAPI) addDraft(tag, commit string) *ghRelease {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tags[tag] = fakeTagObject{Type: "commit", SHA: commit}
	rel := &ghRelease{ID: f.nextReleaseID, TagName: tag, TargetCommitish: commit, Name: tag, Draft: true, CreatedAt: "2026-01-01T00:00:00Z", HTMLURL: "https://github.com/vic/demo/releases/1"}
	f.nextReleaseID++
	f.releases[rel.ID] = rel
	return rel
}

func (f *fakeReleaseAPI) addAsset(releaseID int64, name string, data []byte, state string) releaseAsset {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextAssetID++
	a := releaseAsset{ID: f.nextAssetID, Name: name, Size: int64(len(data)), State: state, URL: fmt.Sprintf("%s/repos/vic/demo/releases/assets/%d", f.url, f.nextAssetID), BrowserDownloadURL: fmt.Sprintf("%s/repos/vic/demo/releases/assets/%d", f.url, f.nextAssetID)}
	f.assets[releaseID] = append(f.assets[releaseID], a)
	f.contents[a.ID] = data
	return a
}

func (f *fakeReleaseAPI) passCI(sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkRuns[sha] = []checkRunFull{{ID: 1, Name: "test", HeadSHA: sha, Status: "completed", Conclusion: "success"}}
}

func releaseRun(t *testing.T, args []string, stdin string) (string, string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	err := run(context.Background(), args, strings.NewReader(stdin), &out, &errb)
	return out.String(), errb.String(), err
}

func writeAssetFile(t *testing.T, dir, name string, data []byte) (string, string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return path, hex.EncodeToString(sum[:])
}

func writeManifest(t *testing.T, dir, tag, commit string, assets map[string][]byte) string {
	t.Helper()
	type entry struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
		Size   *int64 `json:"size,omitempty"`
	}
	m := struct {
		Tag    string  `json:"tag"`
		Commit string  `json:"commit"`
		Assets []entry `json:"assets"`
	}{Tag: tag, Commit: commit}
	for name, data := range assets {
		sum := sha256.Sum256(data)
		size := int64(len(data))
		m.Assets = append(m.Assets, entry{Name: name, SHA256: hex.EncodeToString(sum[:]), Size: &size})
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTagListAndView(t *testing.T) {
	f, _ := newReleaseFake(t)
	const commit = "0123456789abcdef0123456789abcdef01234567"
	tagObj := strings.Repeat("a", 40)
	f.addTag("z", commit)
	f.tags["a"] = fakeTagObject{Type: "tag", SHA: tagObj}
	f.objects[tagObj] = fakeTagObject{Type: "commit", SHA: commit}
	f.tagPageSize = 1

	out, _, err := releaseRun(t, []string{"tag", "list", "--json"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var listed []map[string]string
	if err := json.Unmarshal([]byte(out), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0]["name"] != "a" || listed[1]["name"] != "z" {
		t.Fatalf("tags = %s", out)
	}

	out, _, err = releaseRun(t, []string{"tag", "view", "a", "--json"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Type   string `json:"type"`
		SHA    string `json:"sha"`
		Commit string `json:"commit"`
		Depth  int    `json:"depth"`
	}
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatal(err)
	}
	if view.Type != "annotated" || view.SHA != tagObj || view.Commit != commit || view.Depth != 1 {
		t.Fatalf("view = %+v", view)
	}

	// A tag-object cycle and a tag pointing at a tree both fail instead of looping.
	f.tags["cycle"] = fakeTagObject{Type: "tag", SHA: tagObj}
	f.objects[tagObj] = fakeTagObject{Type: "tag", SHA: tagObj}
	if _, _, err := releaseRun(t, []string{"tag", "view", "cycle"}, ""); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("cycle error = %v", err)
	}
	tree := strings.Repeat("b", 40)
	f.tags["tree"] = fakeTagObject{Type: "tag", SHA: tree}
	f.objects[tree] = fakeTagObject{Type: "tree", SHA: strings.Repeat("c", 40)}
	if _, _, err := releaseRun(t, []string{"tag", "view", "tree"}, ""); err == nil || !strings.Contains(err.Error(), "not a commit") {
		t.Fatalf("tree error = %v", err)
	}
}

func TestReleaseListViewLatest(t *testing.T) {
	f, _ := newReleaseFake(t)
	commit := strings.Repeat("1", 40)
	f.addTag("v1", commit)
	f.releases[1] = &ghRelease{ID: 1, TagName: "v1", Name: "one", Draft: false, CreatedAt: "2026-01-01T00:00:00Z", HTMLURL: "u1"}
	f.releases[2] = &ghRelease{ID: 2, TagName: "v2", Name: "two", Draft: true, CreatedAt: "2026-02-01T00:00:00Z", HTMLURL: "u2"}
	f.releases[3] = &ghRelease{ID: 3, TagName: "v3", Name: "three", Prerelease: true, CreatedAt: "2026-03-01T00:00:00Z", HTMLURL: "u3"}
	f.releasePageSize = 1

	out, _, err := releaseRun(t, []string{"release", "list", "--json"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var all []listedRelease
	if err := json.Unmarshal([]byte(out), &all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Tag != "v3" || all[1].Tag != "v2" || all[2].Tag != "v1" {
		t.Fatalf("all = %+v", all)
	}

	out, _, err = releaseRun(t, []string{"release", "list", "--state", "published", "--json"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var published []listedRelease
	if err := json.Unmarshal([]byte(out), &published); err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 || published[0].Tag != "v1" {
		t.Fatalf("published = %+v", published)
	}

	out, _, err = releaseRun(t, []string{"release", "latest", "--json"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var latest listedRelease
	if err := json.Unmarshal([]byte(out), &latest); err != nil {
		t.Fatal(err)
	}
	if latest.Tag != "v1" {
		t.Fatalf("latest = %+v", latest)
	}
}

func TestReleaseCreatePinsExplicitCommit(t *testing.T) {
	f, _ := newReleaseFake(t)
	sha := strings.Repeat("2", 40)
	f.passCI(sha)

	out, _, err := releaseRun(t, []string{"release", "create", "v1", "--commit", sha, "--name", "Version 1", "--body", "notes"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Created draft release v1") {
		t.Fatalf("out = %q", out)
	}
	if f.creates != 1 || len(f.releases) != 1 {
		t.Fatalf("creates = %d, releases = %d", f.creates, len(f.releases))
	}
	for _, rel := range f.releases {
		if !rel.Draft || rel.TagName != "v1" || rel.TargetCommitish != sha || rel.Name != "Version 1" || rel.Body != "notes" {
			t.Fatalf("release = %+v", rel)
		}
	}

	// Running it again reuses the same tag and draft, without another create.
	beforeCreates, beforeReleases := f.creates, len(f.releases)
	out, _, err = releaseRun(t, []string{"release", "create", "v1", "--commit", SHAUpper(sha)}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Draft release v1 already exists") {
		t.Fatalf("out = %q", out)
	}
	if f.creates != beforeCreates || len(f.releases) != beforeReleases {
		t.Fatalf("retry changed state: creates %d, releases %d", f.creates, len(f.releases))
	}

	// A tag pointing elsewhere is never moved, and a branch name is rejected early.
	if _, _, err := releaseRun(t, []string{"release", "create", "v1", "--commit", strings.Repeat("3", 40)}, ""); err == nil || !strings.Contains(err.Error(), "refusing to move") {
		t.Fatalf("conflict error = %v", err)
	}
	if _, _, err := releaseRun(t, []string{"release", "create", "v2", "--commit", "main"}, ""); err == nil || !strings.Contains(err.Error(), "full 40- or 64-character") {
		t.Fatalf("branch error = %v", err)
	}
}

func TestReleaseCreateRequiresPassingCI(t *testing.T) {
	f, _ := newReleaseFake(t)
	sha := strings.Repeat("4", 40)
	f.checkRuns[sha] = []checkRunFull{{Name: "test", HeadSHA: sha, Status: "completed", Conclusion: "failure"}}

	if _, _, err := releaseRun(t, []string{"release", "create", "v1", "--commit", sha}, ""); err == nil || !strings.Contains(err.Error(), "CI on") {
		t.Fatalf("error = %v", err)
	}
	if f.creates != 0 || len(f.releases) != 0 {
		t.Fatalf("CI failure changed state: creates %d, releases %d", f.creates, len(f.releases))
	}
}

func TestReleaseStageUploadsSkipsAndRefusesCollisions(t *testing.T) {
	f, _ := newReleaseFake(t)
	sha := strings.Repeat("5", 40)
	rel := f.addDraft("v1", sha)
	f.passCI(sha)
	dir := t.TempDir()
	a := []byte("alpha")
	b := []byte("beta")
	ap, _ := writeAssetFile(t, dir, "a.txt", a)
	bp, _ := writeAssetFile(t, dir, "b.txt", b)
	f.addAsset(rel.ID, "a.txt", a, "uploaded")
	manifest := writeManifest(t, dir, "v1", strings.ToUpper(sha), map[string][]byte{"a.txt": a, "b.txt": b})

	out, _, err := releaseRun(t, []string{"release", "stage", "v1", "--manifest", manifest, "--asset", ap, "--asset", bp}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Staged 1 and skipped 1 identical asset") || !strings.Contains(out, "not published") {
		t.Fatalf("out = %q", out)
	}
	if f.uploads != 1 || f.publishes != 0 {
		t.Fatalf("uploads = %d, publishes = %d", f.uploads, f.publishes)
	}

	// A same-name asset with different bytes is a collision: never delete or overwrite.
	f.mu.Lock()
	for i := range f.assets[rel.ID] {
		if f.assets[rel.ID][i].Name == "b.txt" {
			f.contents[f.assets[rel.ID][i].ID] = []byte("different")
			f.assets[rel.ID][i].Size = int64(len("different"))
		}
	}
	f.mu.Unlock()
	beforeUploads, beforeDeletes := f.uploads, f.deletes
	_, _, err = releaseRun(t, []string{"release", "stage", "v1", "--manifest", manifest, "--asset", ap, "--asset", bp}, "")
	if err == nil || !strings.Contains(err.Error(), "asset b.txt") {
		t.Fatalf("collision error = %v", err)
	}
	if f.uploads != beforeUploads || f.deletes != beforeDeletes {
		t.Fatalf("collision changed state: uploads %d, deletes %d", f.uploads, f.deletes)
	}

	// A local symlink is rejected.
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	sym := filepath.Join(sub, "b.txt")
	if err := os.Symlink(bp, sym); err != nil {
		t.Fatal(err)
	}
	if _, _, err := releaseRun(t, []string{"release", "stage", "v1", "--manifest", manifest, "--asset", ap, "--asset", sym}, ""); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink error = %v", err)
	}
}

func TestStageRefusesUnknownRemoteAssetBeforeUploading(t *testing.T) {
	f, _ := newReleaseFake(t)
	sha := strings.Repeat("6", 40)
	rel := f.addDraft("v1", sha)
	f.passCI(sha)
	dir := t.TempDir()
	data := []byte("payload")
	path, _ := writeAssetFile(t, dir, "ok.txt", data)
	manifest := writeManifest(t, dir, "v1", sha, map[string][]byte{"ok.txt": data})
	f.addAsset(rel.ID, "surprise.txt", []byte("x"), "uploaded")

	if _, _, err := releaseRun(t, []string{"release", "stage", "v1", "--manifest", manifest, "--asset", path}, ""); err == nil || !strings.Contains(err.Error(), "not in the manifest") {
		t.Fatalf("error = %v", err)
	}
	if f.uploads != 0 || f.deletes != 0 {
		t.Fatalf("extra asset changed state: uploads %d, deletes %d", f.uploads, f.deletes)
	}
}

func TestStageRecoversInterruptedUpload(t *testing.T) {
	f, _ := newReleaseFake(t)
	sha := strings.Repeat("7", 40)
	rel := f.addDraft("v1", sha)
	f.passCI(sha)
	dir := t.TempDir()
	data := []byte("retry-me")
	path, _ := writeAssetFile(t, dir, "a.txt", data)
	manifest := writeManifest(t, dir, "v1", sha, map[string][]byte{"a.txt": data})
	old := f.addAsset(rel.ID, "a.txt", []byte("partial"), "starter")

	if _, _, err := releaseRun(t, []string{"release", "stage", "v1", "--manifest", manifest, "--asset", path}, ""); err != nil {
		t.Fatal(err)
	}
	if f.deletes != 1 || f.uploads != 1 {
		t.Fatalf("deletes = %d, uploads = %d", f.deletes, f.uploads)
	}
	assets := f.assets[rel.ID]
	if len(assets) != 1 || assets[0].ID == old.ID || assets[0].Name != "a.txt" {
		t.Fatalf("assets = %+v", assets)
	}
}

func TestReleaseVerifyIsReadOnly(t *testing.T) {
	f, _ := newReleaseFake(t)
	sha := strings.Repeat("8", 40)
	rel := f.addDraft("v1", sha)
	dir := t.TempDir()
	data := []byte("verify")
	_, _ = writeAssetFile(t, dir, "a.txt", data)
	manifest := writeManifest(t, dir, "v1", sha, map[string][]byte{"a.txt": data})
	f.addAsset(rel.ID, "a.txt", data, "uploaded")

	out, _, err := releaseRun(t, []string{"release", "verify", "v1", "--manifest", manifest}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "nothing was changed or published") {
		t.Fatalf("out = %q", out)
	}
	if f.uploads != 0 || f.deletes != 0 || f.publishes != 0 {
		t.Fatalf("verify changed state: %d %d %d", f.uploads, f.deletes, f.publishes)
	}
}

func TestReleasePublishVerifiesThenPublishes(t *testing.T) {
	f, _ := newReleaseFake(t)
	sha := strings.Repeat("9", 40)
	rel := f.addDraft("v1", sha)
	f.passCI(sha)
	dir := t.TempDir()
	data := []byte("publish")
	_, _ = writeAssetFile(t, dir, "a.txt", data)
	manifest := writeManifest(t, dir, "v1", sha, map[string][]byte{"a.txt": data})
	f.addAsset(rel.ID, "a.txt", data, "uploaded")

	out, _, err := releaseRun(t, []string{"release", "publish", "v1", "--manifest", manifest}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Published release v1") || f.publishes != 1 || f.releases[rel.ID].Draft {
		t.Fatalf("out = %q, publishes = %d, release = %+v", out, f.publishes, f.releases[rel.ID])
	}

	// Already published: verify again, report it, and send no second PATCH.
	out, _, err = releaseRun(t, []string{"release", "publish", "v1", "--manifest", manifest}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "already published") || f.publishes != 1 {
		t.Fatalf("idempotent publish: out = %q, publishes = %d", out, f.publishes)
	}
}

func TestReleasePublishRefusesBadState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fakeReleaseAPI, int64, string)
		want   string
	}{
		{"missing asset", func(f *fakeReleaseAPI, id int64, sha string) {}, "missing asset"},
		{"tag moved", func(f *fakeReleaseAPI, id int64, sha string) {
			f.tags["v1"] = fakeTagObject{Type: "commit", SHA: strings.Repeat("a", 40)}
		}, "points to"},
		{"ci failure", func(f *fakeReleaseAPI, id int64, sha string) {
			f.checkRuns[sha] = []checkRunFull{{Name: "test", HeadSHA: sha, Status: "completed", Conclusion: "failure"}}
		}, "CI on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newReleaseFake(t)
			sha := strings.Repeat("b", 40)
			rel := f.addDraft("v1", sha)
			f.passCI(sha)
			dir := t.TempDir()
			data := []byte("x")
			_, _ = writeAssetFile(t, dir, "a.txt", data)
			manifest := writeManifest(t, dir, "v1", sha, map[string][]byte{"a.txt": data})
			if tc.name != "missing asset" {
				f.addAsset(rel.ID, "a.txt", data, "uploaded")
			}
			tc.mutate(f, rel.ID, sha)
			if _, _, err := releaseRun(t, []string{"release", "publish", "v1", "--manifest", manifest}, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if f.publishes != 0 {
				t.Fatalf("published despite error")
			}
		})
	}
}

func TestRequireCI(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name string
		set  func(*fakeReleaseAPI, string)
		want string
	}{
		{"success", func(f *fakeReleaseAPI, sha string) {
			f.checkRuns[sha] = []checkRunFull{{Name: "a", HeadSHA: sha, Status: "completed", Conclusion: "success"}}
		}, ""},
		{"pending", func(f *fakeReleaseAPI, sha string) {
			f.checkRuns[sha] = []checkRunFull{{Name: "a", HeadSHA: sha, Status: "queued"}}
		}, "pending"},
		{"neutral is inconclusive", func(f *fakeReleaseAPI, sha string) {
			f.checkRuns[sha] = []checkRunFull{{Name: "a", HeadSHA: sha, Status: "completed", Conclusion: "neutral"}}
		}, "inconclusive"},
		{"missing", func(f *fakeReleaseAPI, sha string) {}, "missing"},
		{"mismatched", func(f *fakeReleaseAPI, sha string) {
			f.checkRuns[sha] = []checkRunFull{{Name: "a", HeadSHA: strings.Repeat("f", 40), Status: "completed", Conclusion: "success"}}
		}, "not the target"},
		{"required missing", func(f *fakeReleaseAPI, sha string) {
			f.required = []string{"build"}
			f.checkRuns[sha] = []checkRunFull{{Name: "test", HeadSHA: sha, Status: "completed", Conclusion: "success"}}
		}, "missing required"},
		{"status pending", func(f *fakeReleaseAPI, sha string) {
			f.statuses[sha] = []commitStatus{{ID: 1, Context: "ci", SHA: sha, State: "pending"}}
		}, "pending"},
		{"latest status wins", func(f *fakeReleaseAPI, sha string) {
			f.statuses[sha] = []commitStatus{{ID: 1, Context: "ci", SHA: sha, State: "failure"}, {ID: 2, Context: "ci", SHA: sha, State: "success"}}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newReleaseFake(t)
			tc.set(f, sha)
			cfg := Config{APIURL: f.url, Token: "tok", Repo: "vic/demo"}
			err := newGitHub(cfg).requireCI(context.Background(), sha)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestUploadURLSafetyAndRedirects(t *testing.T) {
	f, srv := newReleaseFake(t)
	gh := newGitHub(Config{APIURL: srv.URL, Token: "secret", Repo: "vic/demo"})
	rel := ghRelease{ID: 7, TagName: "v1", UploadURL: srv.URL + "/repos/vic/demo/releases/7/assets{?name,label}"}
	u, err := gh.uploadURL(rel, "a b.txt")
	if err != nil || !strings.Contains(u.RawQuery, "name=a+b.txt") {
		t.Fatalf("uploadURL = %v, %v", u, err)
	}
	for _, bad := range []string{
		"http://evil.example/repos/vic/demo/releases/7/assets",
		"https://evil.example/repos/vic/demo/releases/7/assets",
		"https://uploads.github.com/repos/vic/demo/releases/8/assets",
		"https://uploads.github.com/repos/vic/demo/releases/7/other",
	} {
		rel.UploadURL = bad
		if _, err := gh.uploadURL(rel, "x"); err == nil {
			t.Errorf("accepted unsafe upload URL %q", bad)
		}
	}

	_ = f
}

func TestDownloadRedirectDoesNotLeakToken(t *testing.T) {
	var gotAuth string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("ok"))
	}))
	defer storage.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("API request lost Authorization")
		}
		http.Redirect(w, r, storage.URL+"/asset", http.StatusFound)
	}))
	defer api.Close()
	gh := newGitHub(Config{APIURL: api.URL, Token: "secret", Repo: "vic/demo"})
	asset := releaseAsset{Name: "a", URL: api.URL + "/repos/vic/demo/releases/assets/1"}
	sum, size, err := gh.DownloadReleaseAssetChecksum(context.Background(), asset)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Fatalf("redirect leaked Authorization: %q", gotAuth)
	}
	want := sha256.Sum256([]byte("ok"))
	if sum != hex.EncodeToString(want[:]) || size != 2 {
		t.Fatalf("sum = %s, size = %d", sum, size)
	}
}

func TestReleaseValidators(t *testing.T) {
	good := []string{"v1", "release/v1.2.3", "a_b-c.d"}
	for _, tag := range good {
		if err := validTagName(tag); err != nil {
			t.Errorf("validTagName(%q) = %v", tag, err)
		}
	}
	for _, tag := range []string{"", "-v1", "/v1", "v1/", "v1.", "a..b", "a//b", "a@{b", ".foo", "foo/.bar", "a.lock", "a.lock/b", "a b", "a~b", "a^b", "a:b", "a?b", "a*b", "a[b", "a\\b"} {
		if err := validTagName(tag); err == nil {
			t.Errorf("validTagName(%q) accepted", tag)
		}
	}
	if _, err := validCommitSHA(strings.Repeat("a", 39)); err == nil {
		t.Error("short SHA accepted")
	}
	if got, err := validCommitSHA(strings.Repeat("A", 40)); err != nil || got != strings.Repeat("a", 40) {
		t.Fatalf("validCommitSHA uppercase = %q, %v", got, err)
	}
	if _, err := validCommitSHA("main"); err == nil {
		t.Error("branch accepted")
	}
	for _, name := range []string{"..", "a/b", `a\b`, "-x", " x", "x "} {
		if err := validAssetName(name); err == nil {
			t.Errorf("validAssetName(%q) accepted", name)
		}
	}

	if _, err := readReleaseManifest(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing manifest accepted")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{"tag":"v1","commit":"`+strings.Repeat("a", 40)+`","extra":true}`), 0o644)
	if _, err := readReleaseManifest(bad); err == nil {
		t.Error("unknown manifest field accepted")
	}
}

func SHAUpper(s string) string { return strings.ToUpper(s) }

func TestStageAcceptsConcurrentIdenticalUpload(t *testing.T) {
	f, _ := newReleaseFake(t)
	sha := strings.Repeat("c", 40)
	rel := f.addDraft("v1", sha)
	f.passCI(sha)
	dir := t.TempDir()
	data := []byte("race")
	path, _ := writeAssetFile(t, dir, "a.txt", data)
	manifest := writeManifest(t, dir, "v1", sha, map[string][]byte{"a.txt": data})
	f.raceUploadContent = data
	if _, _, err := releaseRun(t, []string{"release", "stage", "v1", "--manifest", manifest, "--asset", path}, ""); err != nil {
		t.Fatal(err)
	}
	if f.uploads != 0 || len(f.assets[rel.ID]) != 1 || f.publishes != 0 {
		t.Fatalf("race result: uploads %d, assets %d, publishes %d", f.uploads, len(f.assets[rel.ID]), f.publishes)
	}
}

func TestStageRefusesConcurrentDifferentUpload(t *testing.T) {
	f, _ := newReleaseFake(t)
	sha := strings.Repeat("d", 40)
	f.addDraft("v1", sha)
	f.passCI(sha)
	dir := t.TempDir()
	data := []byte("mine")
	path, _ := writeAssetFile(t, dir, "a.txt", data)
	manifest := writeManifest(t, dir, "v1", sha, map[string][]byte{"a.txt": data})
	f.raceUploadContent = []byte("theirs")
	if _, _, err := releaseRun(t, []string{"release", "stage", "v1", "--manifest", manifest, "--asset", path}, ""); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("error = %v", err)
	}
	if f.uploads != 0 || f.publishes != 0 {
		t.Fatalf("race changed state: uploads %d, publishes %d", f.uploads, f.publishes)
	}
}

func TestStageRechecksTagAfterUpload(t *testing.T) {
	f, _ := newReleaseFake(t)
	sha := strings.Repeat("e", 40)
	f.addDraft("v1", sha)
	f.passCI(sha)
	dir := t.TempDir()
	data := []byte("x")
	path, _ := writeAssetFile(t, dir, "a.txt", data)
	manifest := writeManifest(t, dir, "v1", sha, map[string][]byte{"a.txt": data})
	f.raceMoveTagSHA = strings.Repeat("f", 40)
	if _, _, err := releaseRun(t, []string{"release", "stage", "v1", "--manifest", manifest, "--asset", path}, ""); err == nil || !strings.Contains(err.Error(), "moved") {
		t.Fatalf("error = %v", err)
	}
	if f.publishes != 0 {
		t.Fatal("tag drift published")
	}
}

func TestReleaseCommandsRequireToken(t *testing.T) {
	newReleaseFake(t)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	if _, _, err := releaseRun(t, []string{"tag", "list"}, ""); err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("error = %v", err)
	}
}
