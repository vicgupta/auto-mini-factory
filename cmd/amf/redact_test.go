package main

import (
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
