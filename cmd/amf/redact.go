package main

import (
	"cmp"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// redactor masks secrets and personal data in text amf is about to write:
// the secret values from its configuration, well-known token formats, auth
// headers, NAME=value secrets, credentials in URLs, emails, and the user's
// home directory. Masks read [REDACTED:<kind>], and Redact is idempotent,
// so text can pass through it more than once. It is safe for concurrent
// use, and a nil *redactor changes nothing.
type redactor struct {
	literals *strings.Replacer // nil without any
	homeDirs []string          // the home directory's spellings
	homes    []*regexp.Regexp  // homeDirs[i] as a whole path element
}

// minLiteral is the shortest secret value a redactor masks by value:
// anything shorter could mask ordinary words.
const minLiteral = 8

// newRedactor returns a redactor that also masks each of literals wherever
// it appears, ignoring empty and short ones.
func newRedactor(literals ...string) *redactor {
	r := &redactor{}
	var lits []string
	for _, l := range literals {
		if len(l) >= minLiteral && !slices.Contains(lits, l) {
			lits = append(lits, l)
		}
	}
	if len(lits) > 0 {
		// Longest first, so a literal containing another is masked whole.
		slices.SortFunc(lits, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
		var pairs []string
		for _, l := range lits {
			pairs = append(pairs, l, "[REDACTED:config]")
		}
		r.literals = strings.NewReplacer(pairs...)
	}
	r.homeDirs = homeDirs()
	for _, h := range r.homeDirs {
		r.homes = append(r.homes, regexp.MustCompile(regexp.QuoteMeta(h)+`\b`))
	}
	return r
}

// homeDirs is the current user's home directory as amf, macOS, Linux and
// Windows would spell it.
func homeDirs() []string {
	var names, dirs []string
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		dirs = append(dirs, h)
		names = append(names, filepath.Base(h))
	}
	if u, err := user.Current(); err == nil {
		names = append(names, filepath.Base(u.Username)) // DOMAIN\name on Windows
	}
	for _, n := range names {
		if n == "" || n == "." || n == "/" || n == `\` {
			continue
		}
		dirs = append(dirs, "/Users/"+n, "/home/"+n, `C:\Users\`+n)
	}
	var out []string
	for _, d := range dirs {
		if len(d) > 1 && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

var (
	githubTokenRE = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`)
	apiKeyRE      = regexp.MustCompile(`\bsk-(?:ant-|proj-)?[A-Za-z0-9_-]{16,}`)
	awsKeyRE      = regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)
	// authHeaderRE keeps the header and scheme, and masks the credential.
	authHeaderRE = regexp.MustCompile(`(?i)(authorization:\s*(?:bearer|token|basic)\s+)(\S+)`)
	// urlCredsRE is a URL's user:password@.
	urlCredsRE = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s:@]+:[^/\s@]+@`)
	emailRE    = regexp.MustCompile(`([A-Za-z0-9._%+-]+)@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)
	// secretRE is NAME=value or NAME: value, optionally quoted as in JSON,
	// where NAME ends in _TOKEN, _KEY, _SECRET or PASSWORD.
	secretRE = regexp.MustCompile(`(?i)(\b[A-Z0-9_]*(?:_TOKEN|_KEY|_SECRET|PASSWORD)"?\s*[=:]\s*"?)([^\s"']+)`)
)

// Redact returns s with secrets and personal data masked.
func (r *redactor) Redact(s string) string {
	if r == nil || s == "" {
		return s
	}
	if r.literals != nil {
		s = r.literals.Replace(s)
	}
	// Each pattern runs only when s holds a substring it needs: most log
	// lines hold none, and strings.Contains is far cheaper than a regexp.
	lower := strings.ToLower(s)
	if strings.Contains(s, "gh") && strings.Contains(s, "_") {
		s = githubTokenRE.ReplaceAllString(s, "[REDACTED:github_token]")
	}
	if strings.Contains(s, "sk-") {
		s = apiKeyRE.ReplaceAllString(s, "[REDACTED:api_key]")
	}
	if strings.Contains(s, "AKIA") || strings.Contains(s, "ASIA") {
		s = awsKeyRE.ReplaceAllString(s, "[REDACTED:aws_key]")
	}
	if strings.Contains(lower, "authorization") {
		s = maskGroup(authHeaderRE, s, "auth_header")
	}
	if strings.Contains(s, "@") {
		if strings.Contains(s, "://") {
			s = urlCredsRE.ReplaceAllString(s, "${1}[REDACTED]@")
		}
		s = emailRE.ReplaceAllStringFunc(s, func(m string) string {
			if strings.HasPrefix(m, "git@") { // ssh remotes: git@github.com:owner/repo
				return m
			}
			return "[REDACTED:email]"
		})
	}
	if strings.ContainsAny(s, "=:") && (strings.Contains(lower, "_token") || strings.Contains(lower, "_key") ||
		strings.Contains(lower, "_secret") || strings.Contains(lower, "password")) {
		s = maskGroup(secretRE, s, "secret")
	}
	for i, h := range r.homeDirs {
		if strings.Contains(s, h) {
			s = r.homes[i].ReplaceAllLiteralString(s, "~")
		}
	}
	return s
}

// maskGroup masks re's second group in s as kind, keeping its first, unless
// an earlier pattern already masked it.
func maskGroup(re *regexp.Regexp, s, kind string) string {
	return re.ReplaceAllStringFunc(s, func(m string) string {
		g := re.FindStringSubmatch(m)
		if strings.HasPrefix(g[2], "[REDACTED") {
			return m
		}
		return g[1] + "[REDACTED:" + kind + "]"
	})
}

// configRedactor masks the secret values in cfg: the GitHub token, the
// agents' keys, and the password in AMF_POSTGRES_URL.
func configRedactor(cfg Config) *redactor {
	lits := []string{cfg.Token, cfg.ClaudeToken, cfg.AnthropicKey, cfg.OpenAIKey, cfg.OpencodeKey}
	if pg, err := parsePostgresURL(cfg.PostgresURL); cfg.PostgresURL != "" && err == nil {
		lits = append(lits, pg.Password)
	}
	return newRedactor(lits...)
}

// redactWriter redacts each write to w. It is for amf's own output, which
// writes whole lines; an agent's output goes through a lineWriter instead,
// so a secret split across writes is still caught.
type redactWriter struct {
	r *redactor
	w io.Writer
}

func (rw *redactWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(rw.w, rw.r.Redact(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// redactedError is an error whose message is redacted. It still unwraps to
// the original, so errors.Is and errors.As see through it.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// redactErr returns err with its message redacted, or nil.
func redactErr(r *redactor, err error) error {
	if err == nil || r == nil {
		return err
	}
	return &redactedError{msg: r.Redact(err.Error()), err: err}
}
