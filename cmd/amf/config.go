package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"time"
)

// defaultOpencodeModel is the model amf runs opencode with when
// OPENCODE_MODEL is unset.
const defaultOpencodeModel = "opencode/big-pickle"

// Config is everything amf needs to talk to GitHub and Claude Code.
type Config struct {
	Token    string // GITHUB_TOKEN
	Repo     string // GITHUB_REPO, as owner/name
	APIURL   string // AMF_API_URL, defaults to https://api.github.com
	Claude   string // AMF_CLAUDE, path to the claude binary, defaults to "claude"
	Codex    string // AMF_CODEX, path to the codex binary, defaults to "codex"
	Opencode string // AMF_OPENCODE, path to the opencode binary, defaults to "opencode"
	// CloneURL is AMF_CLONE_URL, where to clone GITHUB_REPO from when amf
	// isn't started inside a checkout. Defaults to https://github.com/<repo>.git.
	CloneURL string

	// ClaudeToken is CLAUDE_CODE_OAUTH_TOKEN, a subscription token from
	// `claude setup-token`. Optional: without it Claude uses its own login.
	ClaudeToken string
	// AnthropicKey is ANTHROPIC_API_KEY. With it or ClaudeToken, amf picks
	// claude for issues without a pipeline, and passes it to Claude.
	AnthropicKey string
	// OpenAIKey is OPENAI_API_KEY. With it, amf picks codex for issues
	// without a pipeline (when claude can't run), and passes it to Codex.
	OpenAIKey string
	// OpencodeModel is OPENCODE_MODEL, the model amf runs opencode with,
	// defaulting to defaultOpencodeModel.
	OpencodeModel string

	// PostgresURL is AMF_POSTGRES_URL, a PostgreSQL server amf also writes
	// the logs of amf run and amf watch to, in the amflogs database.
	// Optional.
	PostgresURL string

	// AgentTimeout is AMF_AGENT_TIMEOUT as written, how long each agent run
	// may take. "" means unset: the flags or defaultAgentTimeout decide.
	// newRunner and doctor parse it, so a bad value fails only run and
	// watch, and doctor names it, instead of every command reading .env.
	AgentTimeout string

	// LogLevel is AMF_LOG_LEVEL as written; newRunner parses it, like
	// AgentTimeout. "" means the defaults.
	LogLevel string

	// Trusted is AMF_TRUSTED, the lower-case GitHub logins whose issues amf
	// works on. Nil means anyone's.
	Trusted []string

	// EnvPaths are the .env files loadConfig looked for, and EnvFile the
	// one it read, or "".
	EnvPaths []string
	EnvFile  string
	// seen is each variable loadConfig read: where from and its value.
	seen map[string]configValue
}

// configValue is where a variable's value came from, env, .env or
// default, and the value.
type configValue struct{ From, Value string }

// loadConfig reads the first .env file in paths that exists. Variables set in
// the process environment take precedence over the file.
func loadConfig(paths ...string) (Config, error) {
	file := map[string]string{}
	envFile := ""
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
		envFile = p
		break
	}
	seen := map[string]configValue{}
	// Unfilled placeholders from `amf init` count as unset.
	get := func(key, def string) string {
		if v := os.Getenv(key); v != "" && !isPlaceholder(v) {
			seen[key] = configValue{"env", v}
			return v
		}
		if v := file[key]; v != "" && !isPlaceholder(v) {
			seen[key] = configValue{".env", v}
			return v
		}
		seen[key] = configValue{"default", def}
		return def
	}

	cfg := Config{
		Token:    get("GITHUB_TOKEN", ""),
		Repo:     normalizeRepo(get("GITHUB_REPO", "")),
		APIURL:   strings.TrimSuffix(get("AMF_API_URL", "https://api.github.com"), "/"),
		Claude:   get("AMF_CLAUDE", "claude"),
		Codex:    get("AMF_CODEX", "codex"),
		Opencode: get("AMF_OPENCODE", "opencode"),

		ClaudeToken:   get("CLAUDE_CODE_OAUTH_TOKEN", ""),
		AnthropicKey:  get("ANTHROPIC_API_KEY", ""),
		OpenAIKey:     get("OPENAI_API_KEY", ""),
		OpencodeModel: get("OPENCODE_MODEL", defaultOpencodeModel),
		PostgresURL:   get("AMF_POSTGRES_URL", ""),
		Trusted:       parseTrusted(get("AMF_TRUSTED", "")),
		AgentTimeout:  get("AMF_AGENT_TIMEOUT", ""),
		LogLevel:      get("AMF_LOG_LEVEL", ""),
		EnvPaths:      paths,
		EnvFile:       envFile,
		seen:          seen,
	}
	if cfg.Token == "" {
		return cfg, errors.New("GITHUB_TOKEN is not set (run amf init, then edit .env)")
	}
	if cfg.Repo == "" {
		return cfg, errors.New("GITHUB_REPO is not set (run amf init, then edit .env)")
	}
	if err := validRepo(cfg.Repo); err != nil {
		return cfg, err
	}
	cfg.CloneURL = get("AMF_CLONE_URL", "https://github.com/"+cfg.Repo+".git")
	return cfg, nil
}

// parseAgentTimeout reads AMF_AGENT_TIMEOUT. An empty value means unset (0);
// anything else must be a duration amf will honor.
func parseAgentTimeout(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("AMF_AGENT_TIMEOUT=%q: not a duration (use e.g. 90m)", v)
	}
	if err := validateAgentTimeout(d, "AMF_AGENT_TIMEOUT"); err != nil {
		return 0, err
	}
	return d, nil
}

// parseTrusted splits AMF_TRUSTED, a list of GitHub logins separated by
// commas or spaces, each optionally starting with @. Logins are compared
// ignoring case, so they're lower-cased.
func parseTrusted(s string) []string {
	var logins []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
		if l := strings.ToLower(strings.TrimPrefix(f, "@")); l != "" {
			logins = append(logins, l)
		}
	}
	return logins
}

// validRepo checks that repo looks like owner/name.
func validRepo(repo string) error {
	if owner, name, ok := strings.Cut(repo, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("GITHUB_REPO must look like owner/name, got %q", repo)
	}
	return nil
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
		if line == 1 {
			s = strings.TrimPrefix(s, "\ufeff") // a byte order mark, from some Windows editors
		}
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

// shownConfig are the variables whose values config.load may log; every
// other one is logged only as set or unset.
var shownConfig = []string{"GITHUB_REPO", "AMF_API_URL", "OPENCODE_MODEL", "AMF_AGENT_TIMEOUT", "AMF_LOG_LEVEL", "AMF_CLAUDE", "AMF_CODEX", "AMF_OPENCODE"}

// configAttrs is, for config.load, each variable loadConfig read with where
// it came from and its value, or set or unset for those not in shownConfig.
func (cfg Config) configAttrs() []any {
	keys := slices.Sorted(maps.Keys(cfg.seen))
	var out []any
	for _, k := range keys {
		v := cfg.seen[k]
		shown := v.Value
		if !slices.Contains(shownConfig, k) {
			shown = "unset"
			if v.Value != "" {
				shown = "set"
			}
		}
		out = append(out, slog.Group(k, "source", v.From, "value", shown))
	}
	return out
}
