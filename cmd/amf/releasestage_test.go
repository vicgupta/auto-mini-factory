package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"
const testCommit2 = "89abcdef0123456789abcdef0123456789abcdef"

type releaseFake struct {
	t  *testing.T
	mu sync.Mutex

	tagCommits map[string]string
	releases   []Release
	assets     map[int64][]ReleaseAsset
	assetData  map[int64][]byte
	nextRelID  int64
	nextAsset  int64
	policy     map[string][]byte
	policyRefs []string
	checks     []checkRunDetail
	statuses   []commitStatusDetail

	writes []string

	createLost      bool
	uploadLost      bool
	patchLost       bool // apply the PATCH, then lose the response
	patchFail       bool
	uploadRedir     bool
	downloadRedir   string
	checkPages      int
	statusPages     int
	tagRace         map[string]string
	moveTagOnCreate string
}

func newReleaseFake(t *testing.T) (*releaseFake, *httptest.Server) {
	t.Helper()
	f := &releaseFake{
		t: t, tagCommits: map[string]string{}, assets: map[int64][]ReleaseAsset{},
		assetData: map[int64][]byte{}, policy: map[string][]byte{},
		nextRelID: 10, nextAsset: 100,
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Chdir(t.TempDir())
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)
	return f, srv
}

func (f *releaseFake) wrote(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, w := range f.writes {
		if w == method+" "+path {
			n++
		}
	}
	return n
}

func (f *releaseFake) addDraft(id int64, rel Release) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rel.ID = id
	f.releases = append(f.releases, rel)
}

func (f *releaseFake) addAsset(releaseID int64, asset ReleaseAsset, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.assets[releaseID] = append(f.assets[releaseID], asset)
	f.assetData[asset.ID] = append([]byte{}, data...)
}

func (f *releaseFake) release(id int64) *Release {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.releases {
		if f.releases[i].ID == id {
			cp := f.releases[i]
			return &cp
		}
	}
	return nil
}

func (f *releaseFake) setPolicy(path, sha string, p any) {
	b, err := json.Marshal(p)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.policy[path+"@"+sha] = b
	f.mu.Unlock()
}

func (f *releaseFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/repos/vic/demo" {
		writeJSON(w, 200, map[string]any{"default_branch": "main"})
		return
	}
	if r.Method != http.MethodGet {
		f.mu.Lock()
		f.writes = append(f.writes, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/vic/demo"), "/")
	if len(parts) < 2 {
		writeJSON(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	switch {
	case parts[1] == "git" && len(parts) == 4 && parts[2] == "commits":
		writeJSON(w, 200, map[string]string{"sha": parts[3]})
	case parts[1] == "git" && len(parts) == 5 && parts[2] == "ref" && parts[3] == "tags":
		f.handleTag(w, parts[4])
	case parts[1] == "git" && len(parts) == 3 && parts[2] == "refs":
		f.handleCreateTag(w, r)
	case parts[1] == "contents":
		f.handleContents(w, r, strings.Join(parts[2:], "/"))
	case parts[1] == "releases" && len(parts) == 2:
		f.handleReleases(w, r)
	case parts[1] == "releases" && len(parts) == 3:
		id, _ := strconv.ParseInt(parts[2], 10, 64)
		f.handleRelease(w, r, id)
	case parts[1] == "releases" && len(parts) == 4 && parts[3] == "assets":
		id, _ := strconv.ParseInt(parts[2], 10, 64)
		f.handleAssets(w, r, id)
	case parts[1] == "releases" && len(parts) == 4 && parts[2] == "assets":
		id, _ := strconv.ParseInt(parts[3], 10, 64)
		f.handleDownload(w, r, id)
	case parts[1] == "commits" && len(parts) == 4 && parts[3] == "check-runs":
		f.handleChecks(w, r)
	case parts[1] == "commits" && len(parts) == 4 && parts[3] == "status":
		writeJSON(w, 200, map[string]string{"sha": parts[2]})
	case parts[1] == "commits" && len(parts) == 4 && parts[3] == "statuses":
		f.handleStatuses(w, r)
	default:
		writeJSON(w, 404, map[string]string{"message": "Not Found"})
	}
}

func (f *releaseFake) handleTag(w http.ResponseWriter, tag string) {
	name, _ := url.PathUnescape(tag)
	f.mu.Lock()
	sha, ok := f.tagCommits[name]
	f.mu.Unlock()
	if !ok {
		writeJSON(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	var ref tagRef
	ref.Ref = "refs/tags/" + name
	ref.Object.Type, ref.Object.SHA = "commit", sha
	writeJSON(w, 200, ref)
}

func (f *releaseFake) handleCreateTag(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	}
	decode2(f.t, r, &in)
	name := strings.TrimPrefix(in.Ref, "refs/tags/")
	f.mu.Lock()
	if race, ok := f.tagRace[name]; ok {
		f.tagCommits[name] = race
		delete(f.tagRace, name)
		f.mu.Unlock()
		writeJSON(w, 422, map[string]string{"message": "Reference already exists"})
		return
	}
	if _, ok := f.tagCommits[name]; ok {
		f.mu.Unlock()
		writeJSON(w, 422, map[string]string{"message": "Reference already exists"})
		return
	}
	f.tagCommits[name] = in.SHA
	f.mu.Unlock()
	writeJSON(w, 201, map[string]any{"ref": in.Ref})
}

func (f *releaseFake) handleContents(w http.ResponseWriter, r *http.Request, path string) {
	ref := r.URL.Query().Get("ref")
	f.mu.Lock()
	f.policyRefs = append(f.policyRefs, ref)
	b, ok := f.policy[path+"@"+ref]
	f.mu.Unlock()
	if !ok {
		writeJSON(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	writeJSON(w, 200, map[string]any{"type": "file", "encoding": "base64", "size": len(b), "content": base64.StdEncoding.EncodeToString(b)})
}

func (f *releaseFake) handleReleases(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		f.mu.Lock()
		out := append([]Release{}, f.releases...)
		f.mu.Unlock()
		writeJSON(w, 200, out)
		return
	}
	var in map[string]any
	decode2(f.t, r, &in)
	tag, _ := in["tag_name"].(string)
	f.mu.Lock()
	for _, rel := range f.releases {
		if rel.TagName == tag {
			f.mu.Unlock()
			writeJSON(w, 422, map[string]string{"message": "already_exists"})
			return
		}
	}
	f.nextRelID++
	rel := Release{ID: f.nextRelID, TagName: tag, Draft: true, CreatedAt: time.Now()}
	rel.TargetCommitish, _ = in["target_commitish"].(string)
	rel.Name, _ = in["name"].(string)
	rel.Body, _ = in["body"].(string)
	rel.HTMLURL = fmt.Sprintf("https://github.com/vic/demo/releases/tag/%s", tag)
	f.releases = append(f.releases, rel)
	f.assets[rel.ID] = nil
	if f.moveTagOnCreate != "" {
		f.tagCommits[tag] = f.moveTagOnCreate
	}
	lost := f.createLost
	f.mu.Unlock()
	if lost {
		writeJSON(w, 500, map[string]string{"message": "response lost"})
		return
	}
	writeJSON(w, 201, rel)
}

func (f *releaseFake) handleRelease(w http.ResponseWriter, r *http.Request, id int64) {
	if r.Method == http.MethodGet {
		if rel := f.release(id); rel != nil {
			writeJSON(w, 200, rel)
			return
		}
		writeJSON(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	var in map[string]any
	decode2(f.t, r, &in)
	f.mu.Lock()
	if f.patchFail {
		f.mu.Unlock()
		writeJSON(w, 500, map[string]string{"message": "patch failed"})
		return
	}
	var out *Release
	for i := range f.releases {
		if f.releases[i].ID != id {
			continue
		}
		if v, ok := in["draft"].(bool); ok {
			f.releases[i].Draft = v
		}
		cp := f.releases[i]
		out = &cp
	}
	lost := f.patchLost
	f.mu.Unlock()
	if out == nil {
		writeJSON(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	if lost {
		writeJSON(w, 500, map[string]string{"message": "response lost"})
		return
	}
	writeJSON(w, 200, out)
}

func (f *releaseFake) handleAssets(w http.ResponseWriter, r *http.Request, releaseID int64) {
	if r.Method == http.MethodGet {
		f.mu.Lock()
		out := append([]ReleaseAsset{}, f.assets[releaseID]...)
		f.mu.Unlock()
		writeJSON(w, 200, out)
		return
	}
	if f.uploadRedir {
		http.Redirect(w, r, "https://evil.example/upload", http.StatusFound)
		return
	}
	data, _ := ioReadAll(r.Body)
	name := r.URL.Query().Get("name")
	f.mu.Lock()
	for _, asset := range f.assets[releaseID] {
		if strings.EqualFold(asset.Name, name) {
			f.mu.Unlock()
			writeJSON(w, 422, map[string]string{"message": "already_exists"})
			return
		}
	}
	f.nextAsset++
	asset := ReleaseAsset{ID: f.nextAsset, Name: name, Size: int64(len(data)), State: "uploaded", ContentType: "application/octet-stream"}
	f.assets[releaseID] = append(f.assets[releaseID], asset)
	f.assetData[asset.ID] = data
	lost := f.uploadLost
	f.mu.Unlock()
	if lost {
		writeJSON(w, 500, map[string]string{"message": "response lost"})
		return
	}
	writeJSON(w, 201, asset)
}

func (f *releaseFake) handleDownload(w http.ResponseWriter, r *http.Request, id int64) {
	if f.downloadRedir != "" {
		http.Redirect(w, r, f.downloadRedir, http.StatusFound)
		return
	}
	f.mu.Lock()
	data, ok := f.assetData[id]
	f.mu.Unlock()
	if !ok {
		writeJSON(w, 404, map[string]string{"message": "Not Found"})
		return
	}
	w.Write(data)
}

func (f *releaseFake) handleChecks(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	runs := append([]checkRunDetail{}, f.checks...)
	pages := f.checkPages
	f.mu.Unlock()
	if pages <= 1 {
		writeJSON(w, 200, map[string]any{"check_runs": runs})
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page == 0 {
		page = 1
	}
	per := (len(runs) + pages - 1) / pages
	start := (page - 1) * per
	end := start + per
	if start > len(runs) {
		start = len(runs)
	}
	if end > len(runs) {
		end = len(runs)
	}
	if page < pages {
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=%d>; rel="next"`, r.Host, r.URL.Path, page+1))
	}
	writeJSON(w, 200, map[string]any{"check_runs": runs[start:end]})
}

func (f *releaseFake) handleStatuses(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	statuses := append([]commitStatusDetail{}, f.statuses...)
	pages := f.statusPages
	f.mu.Unlock()
	if pages <= 1 {
		writeJSON(w, 200, statuses)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page == 0 {
		page = 1
	}
	per := (len(statuses) + pages - 1) / pages
	start := (page - 1) * per
	end := start + per
	if start > len(statuses) {
		start = len(statuses)
	}
	if end > len(statuses) {
		end = len(statuses)
	}
	if page < pages {
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=%d>; rel="next"`, r.Host, r.URL.Path, page+1))
	}
	writeJSON(w, 200, statuses[start:end])
}

func ioReadAll(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(r)
	return b.Bytes(), err
}

func decode2(t *testing.T, r *http.Request, v any) {
	t.Helper()
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		t.Errorf("decode: %v", err)
	}
}

func writePlanFiles(t *testing.T, dir, tag string, files map[string][]byte) (planPath, manifestPath string) {
	t.Helper()
	arts := make([]planArtifact, 0, len(files))
	var manifest strings.Builder
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data := files[name]
		rel := "dist/" + name
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(&manifest, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		arts = append(arts, planArtifact{Path: rel, Name: name})
	}
	plan := releasePlan{
		Schema: releasePlanSchema, Tag: tag, Commit: testCommit, Name: "amf " + tag,
		Notes: "notes for " + tag, Policy: "amf-release-policy.json", Artifacts: arts,
	}
	planPath = filepath.Join(dir, "plan.json")
	manifestPath = filepath.Join(dir, "checksums.txt")
	b, _ := json.Marshal(plan)
	if err := os.WriteFile(planPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(manifest.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return planPath, manifestPath
}

func goodPolicy() releasePolicy {
	return releasePolicy{Version: 1,
		RequiredChecks:   []policyCheck{{Name: "test", AppID: 7}},
		RequiredStatuses: []policyStatus{{Context: "ci/build", CreatorID: 9}},
	}
}

func (f *releaseFake) allowRelease(tag string) {
	f.mu.Lock()
	f.tagCommits[tag] = testCommit
	f.mu.Unlock()
	f.setPolicy("amf-release-policy.json", testCommit, goodPolicy())
	f.mu.Lock()
	f.checks = []checkRunDetail{{ID: 1, Name: "test", HeadSHA: testCommit, Status: "completed", Conclusion: "success", App: struct {
		ID int64 `json:"id"`
	}{ID: 7}}}
	f.statuses = []commitStatusDetail{{ID: 1, Context: "ci/build", State: "success", Creator: struct {
		ID int64 `json:"id"`
	}{ID: 9}}}
	f.mu.Unlock()
}

func runReleaseCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, stderr bytes.Buffer
	err := run(context.Background(), args, nil, &out, &stderr)
	return out.String(), stderr.String(), err
}

func TestTagCreateImmutableTarget(t *testing.T) {
	f, _ := newReleaseFake(t)
	f.mu.Lock()
	f.tagCommits["v1"] = testCommit
	f.mu.Unlock()

	out, _, err := runReleaseCmd(t, "tag", "create", "v1", "--commit", testCommit)
	if err != nil || !strings.Contains(out, "already points") {
		t.Fatalf("same tag: out=%q err=%v", out, err)
	}
	if got := f.wrote("POST", "/repos/vic/demo/git/refs"); got != 0 {
		t.Fatalf("idempotent tag create posted %d times", got)
	}

	_, _, err = runReleaseCmd(t, "tag", "create", "v1", "--commit", testCommit2)
	if err == nil || !strings.Contains(err.Error(), "never moves") {
		t.Fatalf("conflicting tag error=%v", err)
	}
	if got := f.wrote("POST", "/repos/vic/demo/git/refs"); got != 0 {
		t.Fatalf("conflicting tag create posted %d times", got)
	}

	_, _, err = runReleaseCmd(t, "tag", "create", "v2", "--commit", "abc123")
	if err == nil || !strings.Contains(err.Error(), "full") {
		t.Fatalf("short SHA error=%v", err)
	}

	out, _, err = runReleaseCmd(t, "tag", "create", "v2", "--commit", testCommit)
	if err != nil || !strings.Contains(out, "Created tag v2") {
		t.Fatalf("create: out=%q err=%v", out, err)
	}
	if f.tagCommits["v2"] != testCommit {
		t.Fatalf("tag v2 = %q", f.tagCommits["v2"])
	}
}

func TestTagCreateConcurrentAndMissingCommit(t *testing.T) {
	f, _ := newReleaseFake(t)
	f.mu.Lock()
	f.tagRace = map[string]string{"v1": testCommit}
	f.mu.Unlock()
	out, _, err := runReleaseCmd(t, "tag", "create", "v1", "--commit", testCommit)
	if err != nil || !strings.Contains(out, "already points") {
		t.Fatalf("race same SHA: out=%q err=%v", out, err)
	}

	f2, _ := newReleaseFake(t)
	f2.mu.Lock()
	f2.tagRace = map[string]string{"v2": testCommit2}
	f2.mu.Unlock()
	_, _, err = runReleaseCmd(t, "tag", "create", "v2", "--commit", testCommit)
	if err == nil || !strings.Contains(err.Error(), "never moves") {
		t.Fatalf("race other SHA error=%v", err)
	}

	// A tree/blob (or missing) object is not a commit.
	f3, _ := newReleaseFake(t)
	f3.mu.Lock()
	f3.tagCommits["v3"] = testCommit
	f3.mu.Unlock()
	_, _, err = runReleaseCmd(t, "tag", "create", "v3", "--commit", testCommit)
	if err != nil {
		t.Fatalf("existing same tag with valid commit: %v", err)
	}
}

func TestReleaseCreatePinsPlanAndPolicy(t *testing.T) {
	f, _ := newReleaseFake(t)
	f.allowRelease("v1")
	dir, _ := os.Getwd()
	plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})

	out, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Created draft release") {
		t.Fatalf("out=%q", out)
	}
	if got := f.wrote("POST", "/repos/vic/demo/releases"); got != 1 {
		t.Fatalf("release POSTs = %d", got)
	}
	rel := f.release(11)
	if rel == nil || !rel.Draft || rel.TagName != "v1" || rel.TargetCommitish != testCommit {
		t.Fatalf("release = %+v", rel)
	}
	rec, err := parseRecordBody(rel.Body)
	if err != nil {
		t.Fatalf("record: %v\nbody=%s", err, rel.Body)
	}
	if rec.Commit != testCommit || rec.Tag != "v1" || rec.PolicyPath != "amf-release-policy.json" {
		t.Fatalf("record = %+v", rec)
	}
	if len(rec.Artifacts) != 1 || rec.Artifacts[0].Name != "one.zip" || rec.Artifacts[0].Size != 3 {
		t.Fatalf("record artifacts = %+v", rec.Artifacts)
	}

	// Same plan, no write.
	out, _, err = runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest)
	if err != nil || !strings.Contains(out, "already matches") {
		t.Fatalf("idempotent create: out=%q err=%v", out, err)
	}
	if got := f.wrote("POST", "/repos/vic/demo/releases"); got != 1 {
		t.Fatalf("idempotent create posted %d times", got)
	}

	// Changed artifact plan against the same draft fails without modification.
	plan2, manifest2 := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("two")})
	_, _, err = runReleaseCmd(t, "release", "create", "v1", "--plan", plan2, "--manifest", manifest2)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("changed plan error=%v", err)
	}
	if got := f.wrote("PATCH", "/repos/vic/demo/releases/11"); got != 0 {
		t.Fatalf("changed plan patched release")
	}
}

func TestReleaseCreateRechecksTagAfterDraftCreation(t *testing.T) {
	f, _ := newReleaseFake(t)
	f.allowRelease("v1")
	f.mu.Lock()
	f.moveTagOnCreate = testCommit2
	f.mu.Unlock()
	dir, _ := os.Getwd()
	plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
	_, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest)
	if err == nil || !strings.Contains(err.Error(), "points to commit") {
		t.Fatalf("error=%v", err)
	}
}

func TestReleaseCreateFailsClosed(t *testing.T) {
	t.Run("missing tag", func(t *testing.T) {
		f, _ := newReleaseFake(t)
		f.setPolicy("amf-release-policy.json", testCommit, goodPolicy())
		dir, _ := os.Getwd()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		_, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "create it first") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("missing policy", func(t *testing.T) {
		f, _ := newReleaseFake(t)
		f.mu.Lock()
		f.tagCommits["v1"] = testCommit
		f.mu.Unlock()
		dir, _ := os.Getwd()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		_, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "was not found") {
			t.Fatalf("error=%v", err)
		}
	})
	t.Run("empty policy", func(t *testing.T) {
		f, _ := newReleaseFake(t)
		f.mu.Lock()
		f.tagCommits["v1"] = testCommit
		f.mu.Unlock()
		f.setPolicy("amf-release-policy.json", testCommit, releasePolicy{Version: 1})
		dir, _ := os.Getwd()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		_, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "no required") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("wrong tag commit", func(t *testing.T) {
		f, _ := newReleaseFake(t)
		f.mu.Lock()
		f.tagCommits["v1"] = testCommit2
		f.mu.Unlock()
		f.setPolicy("amf-release-policy.json", testCommit, goodPolicy())
		dir, _ := os.Getwd()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		_, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "points to commit") {
			t.Fatalf("error=%v", err)
		}
		if got := f.wrote("POST", "/repos/vic/demo/releases"); got != 0 {
			t.Fatalf("wrong tag created a release")
		}
	})

	t.Run("unrelated published release", func(t *testing.T) {
		f, _ := newReleaseFake(t)
		f.allowRelease("v1")
		f.addDraft(5, Release{TagName: "v1", Body: "someone else's release", CreatedAt: time.Now()})
		dir, _ := os.Getwd()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		_, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "no amf ownership record") {
			t.Fatalf("error=%v", err)
		}
		if got := f.wrote("POST", "/repos/vic/demo/releases"); got != 0 {
			t.Fatalf("unrelated release was touched")
		}
	})
}

func TestReleaseStageUploadsVerifiedSnapshots(t *testing.T) {
	f, _ := newReleaseFake(t)
	f.allowRelease("v1")
	dir, _ := os.Getwd()
	files := map[string][]byte{"one.zip": []byte("one"), "two.zip": []byte("two")}
	plan, manifest := writePlanFiles(t, dir, "v1", files)
	if _, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest); err != nil {
		t.Fatal(err)
	}
	out, _, err := runReleaseCmd(t, "release", "stage", "v1", "--release", "11", "--plan", plan, "--manifest", manifest)
	if err != nil || !strings.Contains(out, "Staged 2") {
		t.Fatalf("stage: out=%q err=%v", out, err)
	}
	f.mu.Lock()
	assets := append([]ReleaseAsset{}, f.assets[11]...)
	f.mu.Unlock()
	if len(assets) != 2 {
		t.Fatalf("assets=%+v", assets)
	}
	for _, a := range assets {
		if a.State != "uploaded" || string(f.assetData[a.ID]) != string(files[a.Name]) {
			t.Fatalf("asset %q not verified: state=%q data=%q", a.Name, a.State, f.assetData[a.ID])
		}
	}
	before := f.wrote("POST", "/repos/vic/demo/releases/11/assets")
	out, _, err = runReleaseCmd(t, "release", "stage", "v1", "--release", "11", "--plan", plan, "--manifest", manifest)
	if err != nil || !strings.Contains(out, "already has all") {
		t.Fatalf("restage: out=%q err=%v", out, err)
	}
	if got := f.wrote("POST", "/repos/vic/demo/releases/11/assets"); got != before {
		t.Fatalf("restage uploaded again: %d", got-before)
	}
}

func TestReleaseStageRejectsUnsafeInputs(t *testing.T) {
	f, _ := newReleaseFake(t)
	f.allowRelease("v1")

	t.Run("traversal", func(t *testing.T) {
		dir := t.TempDir()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		b, _ := os.ReadFile(plan)
		b = bytes.Replace(b, []byte(`"dist/one.zip"`), []byte(`"../one.zip"`), 1)
		os.WriteFile(plan, b, 0o644)
		_, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		full := filepath.Join(dir, "dist", "one.zip")
		os.Remove(full)
		if err := os.Symlink(filepath.Join(dir, "real.zip"), full); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, "real.zip"), []byte("one"), 0o644)
		_, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("symlink parent", func(t *testing.T) {
		dir := t.TempDir()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		outside := t.TempDir()
		if err := os.Rename(filepath.Join(dir, "dist"), filepath.Join(outside, "dist")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(outside, "dist"), filepath.Join(dir, "dist")); err != nil {
			t.Fatal(err)
		}
		_, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "includes symlink") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("manifest missing entry", func(t *testing.T) {
		dir := t.TempDir()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		os.WriteFile(manifest, []byte{}, 0o644)
		_, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "no checksums found") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("duplicate manifest", func(t *testing.T) {
		dir := t.TempDir()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		line, _ := os.ReadFile(manifest)
		os.WriteFile(manifest, append(line, line...), 0o644)
		_, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "duplicates") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("unsafe asset name", func(t *testing.T) {
		dir := t.TempDir()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		b, _ := os.ReadFile(plan)
		b = bytes.Replace(b, []byte(`"name":"one.zip"`), []byte(`"name":"../one.zip"`), 1)
		os.WriteFile(plan, b, 0o644)
		_, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "one file name") {
			t.Fatalf("error=%v", err)
		}
	})

	t.Run("checksum mismatch", func(t *testing.T) {
		dir := t.TempDir()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		os.WriteFile(filepath.Join(dir, "dist", "one.zip"), []byte("changed"), 0o644)
		_, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "checksum manifest says") {
			t.Fatalf("error=%v", err)
		}
	})
}

func TestReleaseStageNeverDeletesOrOverwrites(t *testing.T) {
	f, _ := newReleaseFake(t)
	f.allowRelease("v1")
	dir, _ := os.Getwd()
	plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
	if _, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest); err != nil {
		t.Fatal(err)
	}
	f.addAsset(11, ReleaseAsset{ID: 900, Name: "one.zip", Size: 3, State: "open"}, []byte("one"))
	_, _, err := runReleaseCmd(t, "release", "stage", "v1", "--release", "11", "--plan", plan, "--manifest", manifest)
	if err == nil || !strings.Contains(err.Error(), "refusing to delete") {
		t.Fatalf("error=%v", err)
	}
	if got := f.wrote("DELETE", "/repos/vic/demo/releases/11/assets/900"); got != 0 {
		t.Fatalf("stage deleted an asset")
	}
	if got := f.wrote("POST", "/repos/vic/demo/releases/11/assets"); got != 0 {
		t.Fatalf("stage uploaded over an ambiguous asset")
	}
}

func TestReleaseStageLostUploadResponseAndContentMismatch(t *testing.T) {
	t.Run("lost response is re-read", func(t *testing.T) {
		f, _ := newReleaseFake(t)
		f.allowRelease("v1")
		dir, _ := os.Getwd()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		if _, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		f.uploadLost = true
		f.mu.Unlock()
		_, _, err := runReleaseCmd(t, "release", "stage", "v1", "--release", "11", "--plan", plan, "--manifest", manifest)
		if err != nil {
			t.Fatalf("lost upload response: %v", err)
		}
		f.mu.Lock()
		n := len(f.assets[11])
		f.mu.Unlock()
		if n != 1 {
			t.Fatalf("assets=%d", n)
		}
	})
	t.Run("different remote bytes fail", func(t *testing.T) {
		f, _ := newReleaseFake(t)
		f.allowRelease("v1")
		dir, _ := os.Getwd()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		if _, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest); err != nil {
			t.Fatal(err)
		}
		f.addAsset(11, ReleaseAsset{ID: 901, Name: "one.zip", Size: 3, State: "uploaded"}, []byte("bad"))
		_, _, err := runReleaseCmd(t, "release", "stage", "v1", "--release", "11", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "different remote bytes") {
			t.Fatalf("error=%v", err)
		}
		if got := f.wrote("DELETE", "/repos/vic/demo/releases/11/assets/901"); got != 0 {
			t.Fatalf("mismatched asset was deleted")
		}
	})
}

func TestReleasePublishExactCIGate(t *testing.T) {
	cases := []struct {
		name string
		mut  func(f *releaseFake)
		want string
	}{
		{"missing check", func(f *releaseFake) { f.checks = nil }, "missing"},
		{"pending check", func(f *releaseFake) { f.checks[0].Status = "in_progress" }, "in_progress"},
		{"failed check", func(f *releaseFake) { f.checks[0].Conclusion = "failure" }, "failure"},
		{"stale success superseded", func(f *releaseFake) {
			f.checks = append(f.checks, checkRunDetail{ID: 2, Name: "test", HeadSHA: testCommit, Status: "in_progress", App: struct {
				ID int64 `json:"id"`
			}{ID: 7}})
		}, "in_progress"},
		{"wrong app", func(f *releaseFake) { f.checks[0].App.ID = 8 }, "not required app"},
		{"wrong sha", func(f *releaseFake) { f.checks[0].HeadSHA = testCommit2 }, "not " + testCommit},
		{"missing status", func(f *releaseFake) { f.statuses = nil }, "missing"},
		{"wrong status creator", func(f *releaseFake) { f.statuses[0].Creator.ID = 8 }, "not required creator"},
		{"failed status", func(f *releaseFake) { f.statuses[0].State = "failure" }, "failure"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newReleaseFake(t)
			f.allowRelease("v1")
			dir, _ := os.Getwd()
			plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
			if _, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest); err != nil {
				t.Fatal(err)
			}
			if _, _, err := runReleaseCmd(t, "release", "stage", "v1", "--release", "11", "--plan", plan, "--manifest", manifest); err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			tc.mut(f)
			f.mu.Unlock()
			_, _, err := runReleaseCmd(t, "release", "publish", "v1", "--release", "11", "--plan", plan, "--manifest", manifest)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
			if got := f.wrote("PATCH", "/repos/vic/demo/releases/11"); got != 0 {
				t.Fatalf("published despite CI failure")
			}
			if got := f.wrote("POST", "/repos/vic/demo/releases/11/assets"); got != 1 { // stage only
				t.Fatalf("claim uploaded before CI passed: posts=%d", got)
			}
			if got := f.wrote("POST", "/repos/vic/demo/git/refs"); got != 0 {
				t.Fatalf("failed CI created a tag")
			}
		})
	}
}

func TestReleasePublishPaginatesCIAndPublishesOnce(t *testing.T) {
	f, _ := newReleaseFake(t)
	f.allowRelease("v1")
	f.mu.Lock()
	f.checkPages = 2
	f.statusPages = 2
	f.checks = append([]checkRunDetail{{ID: 1, Name: "other", HeadSHA: testCommit, Status: "completed", Conclusion: "success", App: struct {
		ID int64 `json:"id"`
	}{ID: 1}}}, f.checks...)
	f.statuses = append([]commitStatusDetail{{ID: 1, Context: "other", State: "success", Creator: struct {
		ID int64 `json:"id"`
	}{ID: 1}}}, f.statuses...)
	f.mu.Unlock()
	dir, _ := os.Getwd()
	plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
	if _, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runReleaseCmd(t, "release", "stage", "v1", "--release", "11", "--plan", plan, "--manifest", manifest); err != nil {
		t.Fatal(err)
	}
	out, _, err := runReleaseCmd(t, "release", "publish", "v1", "--release", "11", "--plan", plan, "--manifest", manifest)
	if err != nil || !strings.Contains(out, "Published release 11") {
		t.Fatalf("publish: out=%q err=%v", out, err)
	}
	if got := f.wrote("PATCH", "/repos/vic/demo/releases/11"); got != 1 {
		t.Fatalf("PATCHes=%d", got)
	}
	if got := f.wrote("POST", "/repos/vic/demo/releases/11/assets"); got != 2 { // artifact + claim
		t.Fatalf("uploads=%d", got)
	}
	out, _, err = runReleaseCmd(t, "release", "publish", "v1", "--release", "11", "--plan", plan, "--manifest", manifest)
	if err != nil || !strings.Contains(out, "already published") {
		t.Fatalf("retry: out=%q err=%v", out, err)
	}
	if got := f.wrote("PATCH", "/repos/vic/demo/releases/11"); got != 1 {
		t.Fatalf("retry PATCHed again: %d", got)
	}
	if got := f.wrote("POST", "/repos/vic/demo/releases/11/assets"); got != 2 {
		t.Fatalf("retry uploaded again: %d", got)
	}
}

func TestReleasePublishRequiresExactRemoteAssets(t *testing.T) {
	setup := func(t *testing.T) (*releaseFake, string, string) {
		f, _ := newReleaseFake(t)
		f.allowRelease("v1")
		dir, _ := os.Getwd()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		if _, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest); err != nil {
			t.Fatal(err)
		}
		if _, _, err := runReleaseCmd(t, "release", "stage", "v1", "--release", "11", "--plan", plan, "--manifest", manifest); err != nil {
			t.Fatal(err)
		}
		return f, plan, manifest
	}
	t.Run("missing artifact", func(t *testing.T) {
		f, plan, manifest := setup(t)
		f.mu.Lock()
		f.assets[11] = nil
		f.mu.Unlock()
		_, _, err := runReleaseCmd(t, "release", "publish", "v1", "--release", "11", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "missing artifact") {
			t.Fatalf("error=%v", err)
		}
		if got := f.wrote("PATCH", "/repos/vic/demo/releases/11"); got != 0 {
			t.Fatalf("published with a missing artifact")
		}
	})
	t.Run("extra artifact", func(t *testing.T) {
		f, plan, manifest := setup(t)
		f.addAsset(11, ReleaseAsset{ID: 960, Name: "surprise.zip", Size: 2, State: "uploaded"}, []byte("hi"))
		_, _, err := runReleaseCmd(t, "release", "publish", "v1", "--release", "11", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "unexpected asset") {
			t.Fatalf("error=%v", err)
		}
		if got := f.wrote("PATCH", "/repos/vic/demo/releases/11"); got != 0 {
			t.Fatalf("published with an extra artifact")
		}
		if got := f.wrote("DELETE", "/repos/vic/demo/releases/11/assets/960"); got != 0 {
			t.Fatalf("deleted an extra artifact")
		}
	})
}

func TestReleasePublishClaimAndUncertainOutcome(t *testing.T) {
	t.Run("existing claim fails closed", func(t *testing.T) {
		f, _ := newReleaseFake(t)
		f.allowRelease("v1")
		dir, _ := os.Getwd()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		if _, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest); err != nil {
			t.Fatal(err)
		}
		if _, _, err := runReleaseCmd(t, "release", "stage", "v1", "--release", "11", "--plan", plan, "--manifest", manifest); err != nil {
			t.Fatal(err)
		}
		f.addAsset(11, ReleaseAsset{ID: 950, Name: publishClaimAsset, Size: 2, State: "uploaded"}, []byte("{}"))
		_, _, err := runReleaseCmd(t, "release", "publish", "v1", "--release", "11", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "unexpected asset") {
			t.Fatalf("error=%v", err)
		}
		if got := f.wrote("PATCH", "/repos/vic/demo/releases/11"); got != 0 {
			t.Fatalf("published with a foreign claim")
		}
		if got := f.wrote("DELETE", "/repos/vic/demo/releases/11/assets/950"); got != 0 {
			t.Fatalf("deleted a claim")
		}
	})

	t.Run("lost patch response is re-read", func(t *testing.T) {
		f, _ := newReleaseFake(t)
		f.allowRelease("v1")
		dir, _ := os.Getwd()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		if _, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest); err != nil {
			t.Fatal(err)
		}
		if _, _, err := runReleaseCmd(t, "release", "stage", "v1", "--release", "11", "--plan", plan, "--manifest", manifest); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		f.patchLost = true
		f.mu.Unlock()
		out, _, err := runReleaseCmd(t, "release", "publish", "v1", "--release", "11", "--plan", plan, "--manifest", manifest)
		if err != nil || !strings.Contains(out, "Published release 11") {
			t.Fatalf("lost patch: out=%q err=%v", out, err)
		}
		if got := f.wrote("PATCH", "/repos/vic/demo/releases/11"); got != 1 {
			t.Fatalf("PATCHes=%d", got)
		}
	})

	t.Run("ambiguous patch refuses", func(t *testing.T) {
		f, _ := newReleaseFake(t)
		f.allowRelease("v1")
		dir, _ := os.Getwd()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		if _, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest); err != nil {
			t.Fatal(err)
		}
		if _, _, err := runReleaseCmd(t, "release", "stage", "v1", "--release", "11", "--plan", plan, "--manifest", manifest); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		f.patchFail = true
		f.mu.Unlock()
		_, _, err := runReleaseCmd(t, "release", "publish", "v1", "--release", "11", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "uncertain") {
			t.Fatalf("error=%v", err)
		}
		if got := f.wrote("PATCH", "/repos/vic/demo/releases/11"); got != 1 {
			t.Fatalf("PATCHes=%d", got)
		}
		if rel := f.release(11); rel == nil || !rel.Draft {
			t.Fatalf("release should still be a draft: %+v", rel)
		}
	})
}

func TestReleaseDownloadAndUploadRedirectSafety(t *testing.T) {
	t.Run("download redirect to foreign host refused", func(t *testing.T) {
		f, _ := newReleaseFake(t)
		f.allowRelease("v1")
		dir, _ := os.Getwd()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		if _, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest); err != nil {
			t.Fatal(err)
		}
		if _, _, err := runReleaseCmd(t, "release", "stage", "v1", "--release", "11", "--plan", plan, "--manifest", manifest); err != nil {
			t.Fatal(err)
		}
		var hits int
		evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			if r.Header.Get("Authorization") != "" {
				t.Errorf("Authorization forwarded to foreign host")
			}
		}))
		defer evil.Close()
		f.mu.Lock()
		f.downloadRedir = evil.URL
		f.mu.Unlock()
		_, _, err := runReleaseCmd(t, "release", "publish", "v1", "--release", "11", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "refusing asset download redirect") {
			t.Fatalf("error=%v", err)
		}
		if hits != 0 {
			t.Fatalf("foreign host was contacted %d times", hits)
		}
	})

	t.Run("upload redirect refused", func(t *testing.T) {
		f, _ := newReleaseFake(t)
		f.allowRelease("v1")
		dir, _ := os.Getwd()
		plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
		if _, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		f.uploadRedir = true
		f.mu.Unlock()
		_, _, err := runReleaseCmd(t, "release", "stage", "v1", "--release", "11", "--plan", plan, "--manifest", manifest)
		if err == nil || !strings.Contains(err.Error(), "302") {
			t.Fatalf("error=%v", err)
		}
		if got := f.wrote("POST", "/repos/vic/demo/releases/11/assets"); got != 1 {
			t.Fatalf("upload attempts=%d", got)
		}
	})
}

func TestReleaseRecordStrictness(t *testing.T) {
	rec := releaseRecord{
		Schema: releaseRecordSchema, Repo: "vic/demo", Tag: "v1", Commit: testCommit,
		Name: "n", PolicyPath: "amf-release-policy.json", Policy: goodPolicy(),
		Artifacts: []recordArtifact{{Name: "one.zip", SHA256: strings.Repeat("a", 64), Size: 1}},
	}
	body, err := encodeRecordBody(rec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseRecordBody(body); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, body, want string }{
		{"no marker", "hello", "no amf ownership"},
		{"duplicate marker", body + "\n" + releaseRecordMarker, "more than one"},
		{"changed notes", body + "x", "notes do not match"},
		{"duplicate JSON key", releaseRecordMarker + "\n{\"schema\":\"x\",\"schema\":\"y\"}\n-->", "duplicate JSON key"},
		{"unknown field", releaseRecordMarker + "\n{\"schema\":\"x\",\"surprise\":1}\n-->", "unknown field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseRecordBody(tc.body); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestReleasePlanRejectsDuplicateJSONKeys(t *testing.T) {
	f, _ := newReleaseFake(t)
	f.allowRelease("v1")
	dir, _ := os.Getwd()
	plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
	b, _ := os.ReadFile(plan)
	b = bytes.Replace(b, []byte(`"tag":"v1"`), []byte(`"tag":"v1","tag":"v2"`), 1)
	os.WriteFile(plan, b, 0o644)
	_, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest)
	if err == nil || !strings.Contains(err.Error(), "duplicate JSON key") {
		t.Fatalf("error=%v", err)
	}
}

func TestTagCreatePeelsAnnotatedTags(t *testing.T) {
	handler := func(peeled string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/repos/vic/demo/git/commits/" + testCommit:
				writeJSON(w, 200, map[string]string{"sha": testCommit})
			case "/repos/vic/demo/git/ref/tags/v1":
				var ref tagRef
				ref.Ref = "refs/tags/v1"
				ref.Object.Type, ref.Object.SHA = "tag", "tag-object-1"
				writeJSON(w, 200, ref)
			case "/repos/vic/demo/git/tags/tag-object-1":
				var obj tagObject
				obj.SHA, obj.Tag = "tag-object-1", "old-name"
				obj.Object.Type, obj.Object.SHA = "commit", peeled
				writeJSON(w, 200, obj)
			default:
				writeJSON(w, 404, map[string]string{"message": "Not Found"})
			}
		}
	}
	t.Run("same commit", func(t *testing.T) {
		srv, log := newTagReleaseServer(t, handler(testCommit))
		tagReleaseEnv(t, srv)
		out, _, err := runTagRelease(t, "tag", "create", "v1", "--commit", testCommit)
		if err != nil || !strings.Contains(out, "already points") {
			t.Fatalf("out=%q err=%v", out, err)
		}
		if wrote := log.wrote(); len(wrote) != 0 {
			t.Fatalf("annotated same tag wrote: %v", wrote)
		}
	})
	t.Run("different commit", func(t *testing.T) {
		srv, log := newTagReleaseServer(t, handler(testCommit2))
		tagReleaseEnv(t, srv)
		_, _, err := runTagRelease(t, "tag", "create", "v1", "--commit", testCommit)
		if err == nil || !strings.Contains(err.Error(), "never moves") {
			t.Fatalf("error=%v", err)
		}
		if wrote := log.wrote(); len(wrote) != 0 {
			t.Fatalf("annotated conflicting tag wrote: %v", wrote)
		}
	})
}

func TestReleasePlanChangesRejected(t *testing.T) {
	f, _ := newReleaseFake(t)
	f.allowRelease("v1")
	two, twoManifest := writePlanFiles(t, t.TempDir(), "v1", map[string][]byte{"one.zip": []byte("one"), "two.zip": []byte("two")})
	if _, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", two, "--manifest", twoManifest); err != nil {
		t.Fatal(err)
	}
	one, oneManifest := writePlanFiles(t, t.TempDir(), "v1", map[string][]byte{"one.zip": []byte("one")})
	_, _, err := runReleaseCmd(t, "release", "stage", "v1", "--release", "11", "--plan", one, "--manifest", oneManifest)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("subset plan error=%v", err)
	}
	if got := f.wrote("POST", "/repos/vic/demo/releases/11/assets"); got != 0 {
		t.Fatalf("subset plan uploaded assets")
	}

	// A draft created from one artifact rejects a plan that adds another.
	f2, _ := newReleaseFake(t)
	f2.allowRelease("v1")
	d2, _ := os.Getwd()
	p1, m1 := writePlanFiles(t, d2, "v1", map[string][]byte{"one.zip": []byte("one")})
	if _, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", p1, "--manifest", m1); err != nil {
		t.Fatal(err)
	}
	_, _, err = runReleaseCmd(t, "release", "stage", "v1", "--release", "11", "--plan", two, "--manifest", twoManifest)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("extra plan error=%v", err)
	}
	if got := f2.wrote("POST", "/repos/vic/demo/releases/11/assets"); got != 0 {
		t.Fatalf("extra plan uploaded assets")
	}
}

func TestReleasePolicyChangeRejectedOnStage(t *testing.T) {
	f, _ := newReleaseFake(t)
	f.allowRelease("v1")
	dir, _ := os.Getwd()
	plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
	if _, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest); err != nil {
		t.Fatal(err)
	}
	f.setPolicy("amf-release-policy.json", testCommit, releasePolicy{Version: 1,
		RequiredChecks: []policyCheck{{Name: "different", AppID: 7}}})
	_, _, err := runReleaseCmd(t, "release", "stage", "v1", "--release", "11", "--plan", plan, "--manifest", manifest)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("changed policy error=%v", err)
	}
	if got := f.wrote("POST", "/repos/vic/demo/releases/11/assets"); got != 0 {
		t.Fatalf("changed policy uploaded assets")
	}
}

func TestReleasePolicyReadAtExactSHA(t *testing.T) {
	f, _ := newReleaseFake(t)
	f.allowRelease("v1")
	dir, _ := os.Getwd()
	plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
	if _, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.policyRefs) == 0 {
		t.Fatal("policy was never fetched")
	}
	for _, ref := range f.policyRefs {
		if ref != testCommit {
			t.Fatalf("policy fetched at %q, want %s", ref, testCommit)
		}
	}
}

func TestReleaseCreateRejectsBoundedOversizeFile(t *testing.T) {
	f, _ := newReleaseFake(t)
	f.allowRelease("v1")
	dir, _ := os.Getwd()
	plan, manifest := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
	if err := os.Truncate(filepath.Join(dir, "dist", "one.zip"), maxArtifactBytes+1); err != nil {
		t.Fatal(err)
	}
	_, _, err := runReleaseCmd(t, "release", "create", "v1", "--plan", plan, "--manifest", manifest)
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("error=%v", err)
	}
}
