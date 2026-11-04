package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeOAuth serves the device flow, answering token polls from replies in
// order (the last one repeats), and GET /user.
func fakeOAuth(t *testing.T, replies ...map[string]any) (*httptest.Server, *int) {
	t.Helper()
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/login/device/code":
			if r.FormValue("client_id") != "cid" || r.FormValue("scope") != "repo workflow" {
				t.Errorf("device code form = %v", r.Form)
			}
			json.NewEncoder(w).Encode(map[string]any{"device_code": "dc", "user_code": "ABCD-1234",
				"verification_uri": "https://example.test/login/device", "expires_in": 900, "interval": 5})
		case "/login/oauth/access_token":
			if r.FormValue("device_code") != "dc" || r.FormValue("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
				t.Errorf("token form = %v", r.Form)
			}
			rep := replies[min(polls, len(replies)-1)]
			polls++
			json.NewEncoder(w).Encode(rep)
		case "/user":
			if r.Header.Get("Authorization") != "Bearer gho_new" {
				w.WriteHeader(401)
				return
			}
			w.Header().Set("X-OAuth-Scopes", "workflow, repo")
			json.NewEncoder(w).Encode(map[string]any{"login": "octo"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &polls
}

func setupAuth(t *testing.T, srv *httptest.Server) (*[]time.Duration, string) {
	t.Helper()
	dir := t.TempDir()
	if d, err := filepath.EvalSymlinks(dir); err == nil {
		dir = d // macOS: /var is /private/var
	}
	t.Setenv("AMF_CONFIG_DIR", filepath.Join(dir, "cfg"))
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	var waits []time.Duration
	oldBase, oldWait := oauthBaseURL, pollWait
	oauthBaseURL = func(string) string { return srv.URL }
	pollWait = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	t.Cleanup(func() { oauthBaseURL, pollWait = oldBase, oldWait })
	return &waits, dir
}

func TestAuthLoginDeviceFlow(t *testing.T) {
	srv, polls := fakeOAuth(t,
		map[string]any{"error": "authorization_pending"},
		map[string]any{"error": "slow_down", "interval": 7},
		map[string]any{"error": "slow_down"},
		map[string]any{"access_token": "gho_new", "scope": "repo,workflow"})
	waits, dir := setupAuth(t, srv)
	var out bytes.Buffer
	if err := authLogin(context.Background(), "github.com", srv.URL, "cid", "repo,workflow", false, &out); err != nil {
		t.Fatal(err)
	}
	if *polls != 4 {
		t.Errorf("polls = %d", *polls)
	}
	want := []time.Duration{5 * time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second}
	if len(*waits) != 4 || (*waits)[0] != want[0] || (*waits)[1] != want[1] || (*waits)[2] != want[2] || (*waits)[3] != want[3] {
		t.Errorf("waits = %v, want %v", *waits, want)
	}
	if !strings.Contains(out.String(), "ABCD-1234") || !strings.Contains(out.String(), "as @octo") {
		t.Errorf("output:\n%s", out.String())
	}
	if strings.Contains(out.String(), "gho_new") {
		t.Error("login printed the token")
	}
	path := filepath.Join(dir, "cfg", "hosts.json")
	h, p, err := savedToken("github.com")
	if err != nil || h.Token != "gho_new" || h.User != "octo" || p != path {
		t.Fatalf("saved = %+v %s %v", h, p, err)
	}
	if runtime.GOOS != "windows" {
		for f, mode := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
			if st, err := os.Stat(f); err != nil || st.Mode().Perm() != mode {
				t.Errorf("%s mode = %v, want %v (%v)", f, st.Mode().Perm(), mode, err)
			}
		}
	}

	// Status reports the saved token's source, account and scopes.
	tok, src := resolveToken(func(string, string) string { return "" }, nil, ".env", "github.com")
	if tok != "gho_new" || !strings.HasPrefix(src, "amf auth login") {
		t.Fatalf("resolve = %q %q", tok, src)
	}
	out.Reset()
	if err := authStatus(context.Background(), "github.com", srv.URL, tok, src, &out); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"@octo", "repo, workflow", "OAuth"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("status lacks %q:\n%s", s, out.String())
		}
	}

	// Logout removes it, and the empty file.
	out.Reset()
	if err := authLogout("github.com", &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("hosts.json still there: %v", err)
	}
	if h, _, _ := savedToken("github.com"); h.Token != "" {
		t.Error("token survived logout")
	}
	out.Reset()
	if err := authLogout("github.com", &out); err != nil || !strings.Contains(out.String(), "No saved") {
		t.Errorf("second logout: %v %s", err, out.String())
	}
}

func TestAuthLoginFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		reply map[string]any
		want  string
	}{
		"denied":  {map[string]any{"error": "access_denied"}, "denied"},
		"expired": {map[string]any{"error": "expired_token"}, "expired"},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := fakeOAuth(t, map[string]any{"error": "authorization_pending"}, tc.reply)
			setupAuth(t, srv)
			err := authLogin(context.Background(), "github.com", srv.URL, "cid", "repo workflow", false, &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if h, _, _ := savedToken("github.com"); h.Token != "" {
				t.Error("saved a token")
			}
		})
	}
	t.Run("deadline", func(t *testing.T) {
		srv, polls := fakeOAuth(t, map[string]any{"error": "authorization_pending"})
		setupAuth(t, srv)
		err := authLogin(context.Background(), "github.com", srv.URL, "cid", "repo workflow", false, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "expired") || *polls != 180 {
			t.Errorf("err = %v after %d polls", err, *polls)
		}
	})
	t.Run("no client id", func(t *testing.T) {
		srv, _ := fakeOAuth(t, map[string]any{})
		setupAuth(t, srv)
		t.Setenv("AMF_GITHUB_CLIENT_ID", "")
		err := authLogin(context.Background(), "github.com", srv.URL, "", "repo", false, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "Enable Device Flow") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestResolveTokenOrder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake gh is a shell script")
	}
	srv, _ := fakeOAuth(t, map[string]any{})
	_, dir := setupAuth(t, srv)
	if _, err := writeHosts(map[string]savedHost{"github.com": {User: "octo", Token: "gho_saved"}}); err != nil {
		t.Fatal(err)
	}
	gh := filepath.Join(dir, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\n[ \"$*\" = \"auth token --hostname github.com\" ] && echo gho_gh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := ghCommand
	t.Cleanup(func() { ghCommand = old })

	env := map[string]string{}
	seen := map[string]configValue{}
	get := func(k, def string) string { return cmpOr(env[k], def) }
	check := func(wantTok, wantSrc string) {
		t.Helper()
		tok, src := resolveToken(get, seen, ".env", "github.com")
		if tok != wantTok || !strings.HasPrefix(src, wantSrc) {
			t.Errorf("resolve = %q, %q; want %q, %q", tok, src, wantTok, wantSrc)
		}
	}
	ghCommand = gh
	env["GITHUB_TOKEN"], seen["GITHUB_TOKEN"] = "ghp_env", configValue{"env", "ghp_env"}
	check("ghp_env", "GITHUB_TOKEN from the environment")
	delete(env, "GITHUB_TOKEN")
	delete(seen, "GITHUB_TOKEN")
	env["GH_TOKEN"], seen["GH_TOKEN"] = "ghp_file", configValue{"file", "ghp_file"}
	check("ghp_file", "GH_TOKEN from .env")
	delete(env, "GH_TOKEN")
	check("gho_gh", "gh auth token")
	ghCommand = "amf-test-no-such-gh"
	check("gho_saved", "amf auth login (")
	if tok, _ := resolveToken(get, nil, ".env", "ghe.example.com"); tok != "" {
		t.Errorf("another host got %q", tok)
	}
	if err := authLogout("github.com", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	check("", "")
}

func TestAuthHost(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.github.com":          "github.com",
		"https://ghe.example.com/api/v3":  "ghe.example.com",
		"https://ghe.example.com/api/v3/": "ghe.example.com",
	} {
		if got := authHost(in); got != want {
			t.Errorf("authHost(%q) = %q, want %q", in, got, want)
		}
	}
	if got := apiURLForHost("ghe.example.com"); got != "https://ghe.example.com/api/v3" {
		t.Errorf("apiURLForHost = %q", got)
	}
}

func TestAuthUsageErrors(t *testing.T) {
	var errb bytes.Buffer
	if err := run(context.Background(), []string{"auth", "nope"}, nil, &bytes.Buffer{}, &errb); err == nil {
		t.Error("unknown subcommand accepted")
	}
	if err := run(context.Background(), []string{"auth", "login", "extra"}, nil, &bytes.Buffer{}, &errb); err == nil {
		t.Error("positional argument accepted")
	}
}
