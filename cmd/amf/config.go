package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// Config is everything amf needs to talk to GitHub and Claude Code.
type Config struct {
	Token  string // GITHUB_TOKEN
	Repo   string // GITHUB_REPO, as owner/name
	APIURL string // AMF_API_URL, defaults to https://api.github.com
	Claude string // AMF_CLAUDE, path to the claude binary, defaults to "claude"

	// ClaudeToken is CLAUDE_CODE_OAUTH_TOKEN, a subscription token from
	// `claude setup-token`. Optional: without it Claude uses its own login.
	ClaudeToken string
}

// loadConfig reads the first .env file in paths that exists. Variables set in
// the process environment take precedence over the file.
func loadConfig(paths ...string) (Config, error) {
	file := map[string]string{}
	for _, p := range paths {
		f, err := os.Open(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Config{}, err
		}
		file, err = parseDotEnv(f)
		f.Close()
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", p, err)
		}
		break
	}
	get := func(key, def string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		if v := file[key]; v != "" {
			return v
		}
		return def
	}

	cfg := Config{
		Token:  get("GITHUB_TOKEN", ""),
		Repo:   normalizeRepo(get("GITHUB_REPO", "")),
		APIURL: strings.TrimSuffix(get("AMF_API_URL", "https://api.github.com"), "/"),
		Claude: get("AMF_CLAUDE", "claude"),

		ClaudeToken: get("CLAUDE_CODE_OAUTH_TOKEN", ""),
	}
	if cfg.Token == "" {
		return cfg, errors.New("GITHUB_TOKEN is not set (add it to .env)")
	}
	if cfg.Repo == "" {
		return cfg, errors.New("GITHUB_REPO is not set (add GITHUB_REPO=owner/name to .env)")
	}
	if owner, name, ok := strings.Cut(cfg.Repo, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return cfg, fmt.Errorf("GITHUB_REPO must look like owner/name, got %q", cfg.Repo)
	}
	return cfg, nil
}

// normalizeRepo accepts owner/name or a github.com URL for it.
func normalizeRepo(s string) string {
	s = strings.TrimPrefix(s, "https://github.com/")
	s = strings.TrimPrefix(s, "git@github.com:")
	s = strings.TrimSuffix(s, ".git")
	return strings.Trim(s, "/")
}

// parseDotEnv parses KEY=VALUE lines. Blank lines and # comments are skipped,
// an "export " prefix is allowed, and values may be single or double quoted.
func parseDotEnv(r io.Reader) (map[string]string, error) {
	vars := map[string]string{}
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		s := strings.TrimSpace(sc.Text())
		if s == "" || s[0] == '#' {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		key, val, ok := strings.Cut(s, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=VALUE", line)
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if n := len(val); n >= 2 && (val[0] == '"' && val[n-1] == '"' || val[0] == '\'' && val[n-1] == '\'') {
			val = val[1 : n-1]
		} else if i := strings.Index(val, " #"); i >= 0 {
			val = strings.TrimSpace(val[:i])
		}
		vars[key] = val
	}
	return vars, sc.Err()
}
