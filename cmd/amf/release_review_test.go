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
	"strings"
	"sync"
	"testing"
)

func reviewStagedRelease(t *testing.T) (*releaseFake, *httptest.Server, string, string) {
	t.Helper()
	f, s := newReleaseFake(t)
	f.allowRelease("v1")
	dir, _ := os.Getwd()
	p, m := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
	for _, args := range [][]string{{"release", "create", "v1", "--plan", p, "--manifest", m}, {"release", "stage", "v1", "--release", "11", "--plan", p, "--manifest", m}} {
		if _, _, err := runReleaseCmd(t, args...); err != nil {
			t.Fatal(err)
		}
	}
	return f, s, p, m
}
func TestReviewTagRequiresCIPolicy(t *testing.T) {
	f, _ := newReleaseFake(t)
	_, _, err := runReleaseCmd(t, "tag", "create", "v1", "--commit", testCommit)
	if err == nil || f.wrote("POST", "/repos/vic/demo/git/refs") != 0 {
		t.Fatalf("missing policy allowed tag creation: %v", err)
	}
}
func TestReviewCreateAndStageRequireCI(t *testing.T) {
	for _, stage := range []bool{false, true} {
		t.Run(fmt.Sprint(stage), func(t *testing.T) {
			f, _ := newReleaseFake(t)
			f.allowRelease("v1")
			dir, _ := os.Getwd()
			p, m := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one")})
			if stage {
				if _, _, e := runReleaseCmd(t, "release", "create", "v1", "--plan", p, "--manifest", m); e != nil {
					t.Fatal(e)
				}
			}
			f.mu.Lock()
			f.checks[0].Status = "in_progress"
			f.mu.Unlock()
			args := []string{"release", "create", "v1", "--plan", p, "--manifest", m}
			endpoint := "/repos/vic/demo/releases"
			if stage {
				args = []string{"release", "stage", "v1", "--release", "11", "--plan", p, "--manifest", m}
				endpoint += "/11/assets"
			}
			_, _, e := runReleaseCmd(t, args...)
			if e == nil || f.wrote("POST", endpoint) != 0 {
				t.Fatalf("pending CI allowed mutation: %v", e)
			}
		})
	}
}
func TestReviewPublishRevalidatesAfterClaim(t *testing.T) {
	for _, change := range []string{"ci", "record", "bytes", "extra", "claim", "missing claim", "tag", "duplicate draft"} {
		t.Run(change, func(t *testing.T) {
			f, s, p, m := reviewStagedRelease(t)
			s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				f.ServeHTTP(w, r)
				if r.Method == "POST" && r.URL.Query().Get("name") == publishClaimAsset {
					f.mu.Lock()
					defer f.mu.Unlock()
					switch change {
					case "missing claim":
						for i, a := range f.assets[11] {
							if a.Name == publishClaimAsset {
								f.assets[11] = append(f.assets[11][:i], f.assets[11][i+1:]...)
								break
							}
						}
					case "tag":
						f.tagCommits["v1"] = testCommit2
					case "duplicate draft":
						cp := f.releases[0]
						cp.ID = 99
						f.releases = append(f.releases, cp)
					case "ci":
						f.checks[0].Status = "in_progress"
					case "record":
						f.releases[0].Body = "unrelated draft"
					case "bytes":
						f.assetData[f.assets[11][0].ID] = []byte("bad")
					case "extra":
						f.assets[11] = append(f.assets[11], ReleaseAsset{ID: 999, Name: "extra.zip", State: "uploaded", Size: 3})
						f.assetData[999] = []byte("bad")
					case "claim":
						for _, a := range f.assets[11] {
							if a.Name == publishClaimAsset {
								f.assetData[a.ID] = []byte("bad")
							}
						}
					}
				}
			})
			_, _, e := runReleaseCmd(t, "release", "publish", "v1", "--release", "11", "--plan", p, "--manifest", m)
			if e == nil || f.wrote("PATCH", "/repos/vic/demo/releases/11") != 0 {
				t.Fatalf("%s drift reached publication: %v", change, e)
			}
		})
	}
}
func TestReviewAssetNamesMustMatchExactly(t *testing.T) {
	f, _, p, m := reviewStagedRelease(t)
	f.mu.Lock()
	f.assets[11][0].Name = "ONE.zip"
	f.mu.Unlock()
	_, _, e := runReleaseCmd(t, "release", "publish", "v1", "--release", "11", "--plan", p, "--manifest", m)
	if e == nil || f.wrote("PATCH", "/repos/vic/demo/releases/11") != 0 {
		t.Fatalf("case-renamed artifact published: %v", e)
	}
}
func TestReviewCaseAliasedJSONKeys(t *testing.T) {
	var p releasePlan
	if e := decodeStrictJSON([]byte(`{"schema":"a","SCHEMA":"b"}`), &p); e == nil {
		t.Fatal("ambiguous keys accepted")
	}
}
func TestReviewMissingCommitStatusProof(t *testing.T) {
	f, s, p, m := reviewStagedRelease(t)
	s.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/status") {
			json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		f.ServeHTTP(w, r)
	})
	_, _, e := runReleaseCmd(t, "release", "publish", "v1", "--release", "11", "--plan", p, "--manifest", m)
	if e == nil || f.wrote("PATCH", "/repos/vic/demo/releases/11") != 0 {
		t.Fatalf("missing exact-SHA proof accepted: %v", e)
	}
}
func TestReviewConcurrentPublishOnlyOnce(t *testing.T) {
	f, _, p, m := reviewStagedRelease(t)
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Real CLI publishers run in separate processes. Exercise concurrent
			// command handlers directly, avoiding run()'s process-global CLI setup.
			var out, stderr bytes.Buffer
			e := cmdReleasePublish(context.Background(), []string{"v1", "--release", "11", "--plan", p, "--manifest", m}, &out, &stderr)
			errs <- e
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	successful := 0
	for e := range errs {
		if e == nil {
			successful++
		}
	}
	if successful == 0 || f.wrote("PATCH", "/repos/vic/demo/releases/11") != 1 {
		t.Fatalf("successful=%d PATCHes=%d", successful, f.wrote("PATCH", "/repos/vic/demo/releases/11"))
	}
}

type reviewTransport func(*http.Request) (*http.Response, error)

func (f reviewTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestReviewAssetRedirectNeverDowngradesOrForwardsAuth(t *testing.T) {
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			calls := 0
			g := newGitHub(Config{APIURL: "https://api.github.com", Repo: "vic/demo", Token: "fake-token"})
			g.http.Transport = reviewTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{scheme + "://api.github.com/payload"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				}
				if r.Header.Get("Authorization") != "" {
					t.Fatal("Authorization forwarded on download redirect")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("one")), Request: r}, nil
			})
			_, e := g.DownloadReleaseAsset(t.Context(), 1)
			if scheme == "http" && (e == nil || calls != 1) {
				t.Fatalf("downgrade: calls=%d error=%v", calls, e)
			}
			if scheme == "https" && (e != nil || calls != 2) {
				t.Fatalf("safe redirect: calls=%d error=%v", calls, e)
			}
		})
	}
}
func TestReviewRemoteHashStreamsAndEnforcesLength(t *testing.T) {
	g := newGitHub(Config{APIURL: "https://api.github.com", Repo: "vic/demo", Token: "fake-token"})
	g.http.Transport = reviewTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("oneEXTRA")), Request: r}, nil
	})
	sum := sha256.Sum256([]byte("one"))
	if e := verifyRemotePayload(t.Context(), g, ReleaseAsset{ID: 1}, recordArtifact{Name: "one", Size: 3, SHA256: hex.EncodeToString(sum[:])}); e == nil {
		t.Fatal("extra bytes accepted")
	}
}
func TestReviewUnicodeCaseAliasedJSONKeys(t *testing.T) {
	var p releasePlan
	if e := decodeStrictJSON([]byte(`{"schema":"a","ſchema":"b"}`), &p); e == nil {
		t.Fatal("Unicode-aliased JSON keys accepted")
	}
}

func TestReviewChecksumManifestCanBeNamedVerifiedArtifact(t *testing.T) {
	f, _ := newReleaseFake(t)
	f.allowRelease("v1")
	dir, _ := os.Getwd()
	sum := sha256.Sum256([]byte("one"))
	publishedChecksums := []byte(fmt.Sprintf("%x  one.zip\n", sum))
	p, m := writePlanFiles(t, dir, "v1", map[string][]byte{"one.zip": []byte("one"), "checksums.txt": publishedChecksums})
	for _, args := range [][]string{{"release", "create", "v1", "--plan", p, "--manifest", m}, {"release", "stage", "v1", "--release", "11", "--plan", p, "--manifest", m}, {"release", "publish", "v1", "--release", "11", "--plan", p, "--manifest", m}} {
		if _, _, e := runReleaseCmd(t, args...); e != nil {
			t.Fatal(e)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	found := false
	for _, a := range f.assets[11] {
		if a.Name == "checksums.txt" {
			found = true
			if !bytes.Equal(f.assetData[a.ID], publishedChecksums) {
				t.Fatal("checksum asset content changed")
			}
		}
	}
	if !found {
		t.Fatal("named checksum asset was not uploaded")
	}
}
