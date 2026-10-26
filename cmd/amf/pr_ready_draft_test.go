package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func draftFixture() prViewResponse {
	pr := prViewResponse{
		Number:  101,
		Title:   "Fix something",
		State:   "open",
		HTMLURL: "https://github.com/vic/demo/pull/101",
		NodeID:  "PR_101",
	}
	pr.Head.Ref = "amf/issue-56"
	pr.Base.Ref = "main"
	return pr
}

func TestPRReadyDraft(t *testing.T) {
	merged := "2026-10-25T00:00:00Z"
	for _, tc := range []struct {
		name     string
		cmd      string // "ready" or "draft"
		edit     func(*prViewResponse)
		want     string // exact stdout, or ""
		wantErr  string // error substring, or ""
		mutation string // expected mutation query fragment, or "" for none
	}{
		{"ready on draft", "ready", func(pr *prViewResponse) { pr.Draft = true }, "#101 is ready for review https://github.com/vic/demo/pull/101\n", "", "markPullRequestReadyForReview"},
		{"ready on ready", "ready", func(pr *prViewResponse) {}, "#101 is already ready for review\n", "", ""},
		{"ready on closed", "ready", func(pr *prViewResponse) { pr.State = "closed" }, "", "#101 is closed", ""},
		{"ready on merged", "ready", func(pr *prViewResponse) { pr.State, pr.MergedAt = "closed", &merged }, "", "#101 is merged", ""},
		{"draft on ready", "draft", func(pr *prViewResponse) {}, "#101 is now a draft https://github.com/vic/demo/pull/101\n", "", "convertPullRequestToDraft"},
		{"draft on draft", "draft", func(pr *prViewResponse) { pr.Draft = true }, "#101 is already a draft\n", "", ""},
		{"draft on closed", "draft", func(pr *prViewResponse) { pr.State = "closed" }, "", "#101 is closed", ""},
		{"draft on merged", "draft", func(pr *prViewResponse) { pr.State, pr.MergedAt = "closed", &merged }, "", "#101 is merged", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh, srv := newFakeGitHub(t)
			pr := draftFixture()
			tc.edit(&pr)
			gh.views[101] = pr
			listJSONEnv(t, srv)
			var out bytes.Buffer
			err := run(context.Background(), []string{"pr", tc.cmd, "101"}, nil, &out, io.Discard)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v, want %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.want {
				t.Fatalf("output=%q, want %q", out.String(), tc.want)
			}
			if tc.mutation == "" {
				if len(gh.graphqlCalls) != 0 {
					t.Fatalf("unexpected mutation: %v", gh.graphqlCalls)
				}
				return
			}
			if len(gh.graphqlCalls) != 1 {
				t.Fatalf("graphql calls = %v, want one", gh.graphqlCalls)
			}
			call := gh.graphqlCalls[0]
			if q, _ := call["query"].(string); !strings.Contains(q, tc.mutation) {
				t.Fatalf("query = %v, want %q", q, tc.mutation)
			}
			vars, _ := call["variables"].(map[string]any)
			if vars["id"] != "PR_101" {
				t.Fatalf("variables = %v, want id PR_101", vars)
			}
		})
	}
}

func TestPRReadyDraftGraphQLError(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	pr := draftFixture()
	pr.Draft = true
	gh.views[101] = pr
	gh.graphqlErr = "Resource not accessible by integration"
	listJSONEnv(t, srv)
	var out bytes.Buffer
	err := run(context.Background(), []string{"pr", "ready", "101"}, nil, &out, io.Discard)
	if err == nil || err.Error() != "Resource not accessible by integration" {
		t.Fatalf("err=%v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("stdout=%q", out.String())
	}
}

func TestPRReadyDraftMissingNodeID(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	pr := draftFixture()
	pr.Draft = true
	pr.NodeID = ""
	gh.views[101] = pr
	listJSONEnv(t, srv)
	var out bytes.Buffer
	err := run(context.Background(), []string{"pr", "ready", "101"}, nil, &out, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "github did not report a node ID") {
		t.Fatalf("err=%v", err)
	}
	if len(gh.graphqlCalls) != 0 {
		t.Fatalf("unexpected mutation: %v", gh.graphqlCalls)
	}
}

func TestPRReadyDraftBadArgs(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{
		{"pr", "ready"},
		{"pr", "ready", "0"},
		{"pr", "ready", "abc"},
		{"pr", "ready", "1", "2"},
		{"pr", "draft"},
		{"pr", "draft", "0"},
		{"pr", "draft", "abc"},
		{"pr", "draft", "1", "2"},
	} {
		var out, stderr bytes.Buffer
		err := run(context.Background(), args, nil, &out, &stderr)
		if !errors.Is(err, errUsage) {
			t.Errorf("%v: %v", args, err)
		}
		if out.Len() != 0 {
			t.Errorf("%v: stdout %q", args, &out)
		}
		if !strings.Contains(stderr.String(), "usage: amf pr "+args[1]+" <N>") {
			t.Errorf("%v: stderr %q", args, &stderr)
		}
	}
}

func TestGraphqlEndpoint(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"https://api.github.com", "https://api.github.com/graphql"},
		{"https://host/api/v3", "https://host/api/graphql"},
	} {
		if got := graphqlEndpoint(tc.base); got != tc.want {
			t.Errorf("graphqlEndpoint(%q) = %q, want %q", tc.base, got, tc.want)
		}
	}
}

func TestPRReadyDraftGHESEndpoint(t *testing.T) {
	// A GitHub Enterprise Server base (…/api/v3) must map to …/api/graphql.
	t.Chdir(t.TempDir())
	graphqlPath := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v3/repos/vic/demo":
			writeJSON(w, 200, map[string]any{"default_branch": "main", "permissions": map[string]bool{"push": true}})
		case r.Method == "GET" && r.URL.Path == "/api/v3/repos/vic/demo/pulls/101":
			writeJSON(w, 200, map[string]any{"number": 101, "html_url": "https://host/vic/demo/pull/101", "state": "open", "draft": true, "node_id": "PR_101", "head": map[string]any{"ref": "topic"}, "base": map[string]any{"ref": "main"}})
		case r.Method == "POST" && r.URL.Path == "/api/graphql":
			graphqlPath = r.URL.Path
			writeJSON(w, 200, map[string]any{"data": map[string]any{"pullRequest": map[string]any{"isDraft": false}}})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL+"/api/v3")
	var out bytes.Buffer
	if err := run(context.Background(), []string{"pr", "ready", "101"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if graphqlPath != "/api/graphql" {
		t.Fatalf("graphql path = %q, want /api/graphql", graphqlPath)
	}
	if out.String() != "#101 is ready for review https://host/vic/demo/pull/101\n" {
		t.Fatalf("output=%q", out.String())
	}
}
