package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// testRedactor masks the literal "literal-secret-value" and treats
// /home/canaryuser as the home directory.
func testRedactor(t testing.TB) *redactor {
	t.Helper()
	t.Setenv("HOME", "/home/canaryuser")
	return newRedactor("literal-secret-value", "short", "")
}

func TestRedact(t *testing.T) {
	r := testRedactor(t)
	for _, tc := range []struct{ name, in, want string }{
		// Secrets.
		{"classic token", "token ghp_abcdefghijklmnopqrstuvwxyz0123456789 here", "token [REDACTED:github_token] here"},
		{"app token", "ghs_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345", "[REDACTED:github_token]"},
		{"fine-grained token", "github_pat_11ABCDEFG0123456789_abcdefghijklmnopqrstuvwxyz", "[REDACTED:github_token]"},
		{"anthropic key", "key sk-ant-api03-abcdefghijklmnopqrstuvwxyz-0123", "key [REDACTED:api_key]"},
		{"claude oauth token", "sk-ant-oat01-abcdefghijklmnopqrstuvwxyz_0123", "[REDACTED:api_key]"},
		{"openai key", "sk-proj-abcdefghijklmnopqrstuvwxyz0123", "[REDACTED:api_key]"},
		{"plain sk key", "sk-abcdefghijklmnopqrstuvwx", "[REDACTED:api_key]"},
		{"opencode key", "key oc_sk_abcdefghijklmnopqrstuvwx0123", "key [REDACTED:api_key]"},
		{"bearer header", "Authorization: Bearer abc.def.ghi", "Authorization: Bearer [REDACTED:auth_header]"},
		{"token header", "authorization: token 0123456789abcdef", "authorization: token [REDACTED:auth_header]"},
		{"basic header", "Authorization: Basic dXNlcjpwYXNz", "Authorization: Basic [REDACTED:auth_header]"},
		{"env secret", "MY_SERVICE_TOKEN=hunter2hunter2", "MY_SERVICE_TOKEN=[REDACTED:secret]"},
		{"lower-case key", "api_key: abc123", "api_key: [REDACTED:secret]"},
		{"quoted json secret", `{"DB_SECRET": "abc123"}`, `{"DB_SECRET": "[REDACTED:secret]"}`},
		{"password", "PASSWORD=pa55word", "PASSWORD=[REDACTED:secret]"},
		{"secret keeps a specific kind", "GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789", "GITHUB_TOKEN=[REDACTED:github_token]"},
		{"aws key id", "id AKIAIOSFODNN7EXAMPLE end", "id [REDACTED:aws_key] end"},
		{"aws session key id", "ASIAIOSFODNN7EXAMPLE", "[REDACTED:aws_key]"},
		{"postgres url", "postgres://amf:s3cret@db.example.com:5432/x", "postgres://[REDACTED]@db.example.com:5432/x"},
		{"https url", "clone https://x-access-token:abc@github.com/a/b.git", "clone https://[REDACTED]@github.com/a/b.git"},
		{"literal", "the value literal-secret-value leaked", "the value [REDACTED:config] leaked"},
		// Personal data.
		{"email", "Vic Gupta <canary@example.com> 1700000000 -0400", "Vic Gupta <[REDACTED:email]> 1700000000 -0400"},
		{"home path", "open /home/canaryuser/code/x.go: no such file", "open ~/code/x.go: no such file"},
		{"home itself", "cd /home/canaryuser", "cd ~"},
		{"mac home", "/Users/canaryuser/x", "~/x"},
		{"windows home", `C:\Users\canaryuser\x`, `~\x`},
		// Left alone.
		{"git sha", "commit 3f2a9c1e0b4d5f6a7b8c9d0e1f2a3b4c5d6e7f8a", "commit 3f2a9c1e0b4d5f6a7b8c9d0e1f2a3b4c5d6e7f8a"},
		{"issue and pr", "#12: opened https://github.com/vic/demo/pull/101", "#12: opened https://github.com/vic/demo/pull/101"},
		{"labels", "amf:ready amf:in-progress amf:done amf:failed", "amf:ready amf:in-progress amf:done amf:failed"},
		{"models", "opencode/big-pickle claude-opus-5-5 gpt-5-codex", "opencode/big-pickle claude-opus-5-5 gpt-5-codex"},
		{"duration", "timeout 1h0m0s, elapsed 59m59.99s", "timeout 1h0m0s, elapsed 59m59.99s"},
		{"url with port", "https://api.github.com:443/repos/a/b", "https://api.github.com:443/repos/a/b"},
		{"ssh remote", "git@github.com:vic/demo.git", "git@github.com:vic/demo.git"},
		{"go module version", "golang.org/x/mod@v0.20.0", "golang.org/x/mod@v0.20.0"},
		{"other home", "/home/canaryuser2/x and /home/other/y", "/home/canaryuser2/x and /home/other/y"},
		{"config name without value", "set OPENAI_API_KEY to use codex", "set OPENAI_API_KEY to use codex"},
		{"short literal", "a short note", "a short note"},
	} {
		if got := r.Redact(tc.in); got != tc.want {
			t.Errorf("%s: Redact(%q)\n got %q\nwant %q", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestRedactIdempotent(t *testing.T) {
	r := testRedactor(t)
	in := "GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789 postgres://u:p@h/db canary@example.com /home/canaryuser/x literal-secret-value"
	once := r.Redact(in)
	if twice := r.Redact(once); twice != once {
		t.Errorf("Redact isn't idempotent:\nonce  %q\ntwice %q", once, twice)
	}
}

func TestRedactNil(t *testing.T) {
	var r *redactor
	if got := r.Redact("ghp_abcdefghijklmnopqrstuvwxyz0123456789"); got != "ghp_abcdefghijklmnopqrstuvwxyz0123456789" {
		t.Errorf("a nil redactor changed its input: %q", got)
	}
}

func FuzzRedact(f *testing.F) {
	for _, s := range []string{
		"", "plain text", "GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"Authorization: Bearer x", "postgres://u:p@h/db", "a@b.co", "/home/canaryuser/x",
		"xliteral-secret-valuex", "API_KEY=[REDACTED:email]",
	} {
		f.Add(s)
	}
	r := testRedactor(f)
	f.Fuzz(func(t *testing.T, s string) {
		once := r.Redact(s)
		if twice := r.Redact(once); twice != once {
			t.Errorf("Redact(%q) isn't idempotent: %q then %q", s, once, twice)
		}
		if strings.Contains(once, "literal-secret-value") {
			t.Errorf("Redact(%q) = %q kept the literal", s, once)
		}
	})
}

func BenchmarkRedact(b *testing.B) {
	r := testRedactor(b)
	line := strings.Repeat("amf: #12: code: running claude (claude-opus-5-5, timeout 1h0m0s) in /home/canaryuser/x ", 6)[:512]
	b.SetBytes(int64(len(line)))
	for b.Loop() {
		r.Redact(line)
	}
}

// TestRunRedactsOutputs plants secrets in the configuration, the agent's
// output and final message, and a GitHub error body, runs one issue to done
// and one to failed, and checks that none of them reaches the console, the
// agent logs, the comments, the PR body or the rows for PostgreSQL.
func TestRunRedactsOutputs(t *testing.T) {
	root, _ := setupRepo(t)
	t.Setenv("HOME", "/home/canaryuser")
	cfg := Config{
		Token:        "canary-github-token-value",
		ClaudeToken:  "canary-claude-oauth-value",
		AnthropicKey: "canary-anthropic-key-value",
		OpenAIKey:    "canary-openai-key-value",
		PostgresURL:  "postgres://amf:canary-pg-password@db.example.com/x",
	}
	canaries := []string{
		cfg.Token, cfg.ClaudeToken, cfg.AnthropicKey, cfg.OpenAIKey, "canary-pg-password",
		"AKIACANARY12345678AB", "canary@example.com", "/home/canaryuser",
	}
	leak := strings.Join(canaries, " ")

	gh, fake := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Goes through")
	gh.issues[2] = readyIssue(2, "Fails to open its PR")
	// Issue 2's PR is refused with an error body holding a secret.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "POST" && strings.HasSuffix(req.URL.Path, "/pulls") {
			body, _ := io.ReadAll(req.Body)
			if strings.Contains(string(body), "amf/issue-2") {
				writeJSON(w, 500, map[string]string{"message": "boom " + cfg.Token})
				return
			}
			req.Body = io.NopCloser(bytes.NewReader(body))
		}
		fake.Config.Handler.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)

	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", shResult+`cat >/dev/null
echo "out `+leak+`"
echo "err `+leak+`" >&2
printf 'partial line without a newline `+leak+`' >&2
echo x >> x.txt
result 'Done. `+leak+`'
`))
	red := configRedactor(cfg)
	r.Redact = red
	r.Log = &redactWriter{r: red, w: r.Log}
	r.DB = &dbLog{red: red, ch: make(chan logRow, dbLogQueue), done: make(chan struct{})}
	var events bytes.Buffer
	r.Events = newEventLogger(r.Log, slog.LevelInfo, &events, slog.LevelDebug, red, "test-run")

	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run(1): %v\nlog:\n%s", err, log)
	}
	if err := r.Run(context.Background(), 2); err == nil {
		t.Fatal("Run(2) should fail")
	}

	outputs := map[string]string{"console": log.String(), "events.jsonl": events.String()}
	logs, _ := filepath.Glob(filepath.Join(root, ".amf", "logs", "*"))
	for _, p := range logs {
		outputs[p] = mustRead(t, p)
	}
	for n, cs := range gh.comments {
		outputs[fmt.Sprintf("comments on #%d", n)] = strings.Join(cs, "\n")
	}
	for i, pr := range gh.prs {
		outputs[fmt.Sprintf("PR %d", i)] = pr["body"]
	}
	close(r.DB.ch)
	var rows []string
	for row := range r.DB.ch {
		rows = append(rows, row.line)
	}
	outputs["postgres rows"] = strings.Join(rows, "\n")

	if !strings.Contains(outputs["console"], "#2") || len(logs) == 0 || len(gh.prs) != 1 || len(rows) == 0 {
		t.Fatalf("too little output to check: logs %v, PRs %v, %d rows\nconsole:\n%s", logs, gh.prs, len(rows), outputs["console"])
	}
	if !strings.Contains(outputs["comments on #2"], "boom [REDACTED:config]") {
		t.Errorf("the failure comment should keep the redacted error: %q", outputs["comments on #2"])
	}
	if !strings.Contains(outputs["postgres rows"], "partial line without a newline") {
		t.Errorf("the agent's last line, without a newline, is missing from the rows:\n%s", outputs["postgres rows"])
	}
	for where, out := range outputs {
		for _, c := range canaries {
			if strings.Contains(out, c) {
				t.Errorf("%s leaks %q:\n%s", where, c, out)
			}
		}
	}
}

func TestRedactErr(t *testing.T) {
	red := newRedactor("canary-secret-value")
	base := fmt.Errorf("wrapped: %w", context.Canceled)
	err := redactErr(red, fmt.Errorf("token canary-secret-value: %w", base))
	if err.Error() != "token [REDACTED:config]: wrapped: context canceled" || !errors.Is(err, context.Canceled) {
		t.Errorf("redactErr = %q, Is(Canceled) = %v", err, errors.Is(err, context.Canceled))
	}
	if redactErr(red, nil) != nil {
		t.Error("redactErr(nil) should be nil")
	}
}
