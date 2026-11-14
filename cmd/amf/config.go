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
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// defaultOpencodeModel is the model amf runs opencode with when
// OPENCODE_MODEL is unset.
const defaultOpencodeModel = "opencode/big-pickle"

// Config is everything amf needs to talk to GitHub and run the agents.
type Config struct {
	Token string // GITHUB_TOKEN, or another source: see resolveToken
	// TokenSource says where Token came from, for doctor, amf auth status
	// and the debug log; never the token itself.
	TokenSource string
	Repo        string // GITHUB_REPO, as owner/name
	APIURL      string // AMF_API_URL, defaults to https://api.github.com
	Claude      string // AMF_CLAUDE, path to the claude binary, defaults to "claude"
	Codex       string // AMF_CODEX, path to the codex binary, defaults to "codex"
	Opencode    string // AMF_OPENCODE, path to the opencode binary, defaults to "opencode"
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
	// OpencodeKey is OPENCODE_API_KEY, an OpenCode Console API key. amf
	// passes it to opencode, and with it opencode can run paid opencode/
	// and Go opencode-go/ models.
	OpencodeKey string
	// OpencodeModel is OPENCODE_MODEL, the model amf runs opencode with,
	// defaulting to defaultOpencodeModel.
	OpencodeModel string

	// PrimaryAgent and SecondaryAgent are AMF_PRIMARY_AGENT and
	// AMF_SECONDARY_AGENT as written: the agent amf runs first for an issue
	// without a pipeline, and the one it falls back to when that fails.
	// Config.agents parses them, so a bad value fails only run, watch and
	// doctor, which names it.
	PrimaryAgent, SecondaryAgent string

	// PostgresURL is AMF_POSTGRES_URL, a PostgreSQL server amf also writes
	// the logs of amf run and amf watch to, in the amflogs database.
	// Optional.
	PostgresURL string

	// AgentTimeout is AMF_AGENT_TIMEOUT as written, how long each agent run
	// may take. "" means unset: the flags or defaultAgentTimeout decide.
	// newRunner and doctor parse it, so a bad value fails only run and
	// watch, and doctor names it, instead of every command reading .env.
	AgentTimeout string

	// AgentIdleTimeout is AMF_AGENT_IDLE_TIMEOUT as written: how long an
	// agent may produce no output and change no worktree file before amf
	// stops it. "" means the default, "off" disables it. newRunner parses
	// it, like AgentTimeout.
	AgentIdleTimeout string

	// LogLevel is AMF_LOG_LEVEL as written; newRunner parses it, like
	// AgentTimeout. "" means the defaults.
	LogLevel string

	// StaleTimeout is AMF_STALE_TIMEOUT as written: how long an issue may
	// stay amf:in-progress, with no amf working on it, before watch puts it
	// back to amf:ready. "" means the default, "off" no sweep.
	StaleTimeout string

	// PRSections is AMF_PR_SECTIONS=on: the PR body and the agents' prompts
	// include the Testing, Docs updated, Decisions and README sections.
	PRSections bool
	// PRSectionsRaw is AMF_PR_SECTIONS as written, for a command to report a
	// bad value instead of every command that reads .env failing on it.
	PRSectionsRaw string

	// Trusted is AMF_TRUSTED, the lower-case GitHub logins whose issues amf
	// works on. Nil means anyone's.
	Trusted []string

	// EnvPaths are the .env files loadConfig looked for, and EnvFile the
	// one it read, or "".
	EnvPaths []string
	EnvFile  string
	// RepoConfigFile is the per-repo settings file loadConfig read, or "".
	RepoConfigFile string
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
	first := func(key, def string) string {
		if v := os.Getenv(key); v != "" && !isPlaceholder(v) {
			return v
		}
		if v := file[key]; v != "" && !isPlaceholder(v) {
			return v
		}
		return def
	}
	repoFile := repoSettingsPath(strings.TrimSuffix(first("AMF_API_URL", "https://api.github.com"), "/"), normalizeRepo(first("GITHUB_REPO", "")))
	repoVars := map[string]string{}
	if repoFile != "" {
		f, err := os.Open(repoFile)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			repoFile = ""
		case err != nil:
			return Config{}, err
		default:
			repoVars, err = parseDotEnv(f)
			f.Close()
			if err != nil {
				return Config{}, fmt.Errorf("%s: %w", repoFile, err)
			}
			for k := range repoVars {
				if !strings.HasPrefix(k, "AMF_PR_") && !strings.HasPrefix(k, "AMF_REVIEW_") {
					return Config{}, fmt.Errorf("%s: %s is not allowed in a repo settings file; only AMF_PR_* and AMF_REVIEW_* are read", repoFile, k)
				}
			}
		}
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
		if v := repoVars[key]; v != "" && !isPlaceholder(v) {
			seen[key] = configValue{"repo", v}
			return v
		}
		seen[key] = configValue{"default", def}
		return def
	}

	cfg := Config{
		Repo:     normalizeRepo(get("GITHUB_REPO", "")),
		APIURL:   strings.TrimSuffix(get("AMF_API_URL", "https://api.github.com"), "/"),
		Claude:   get("AMF_CLAUDE", "claude"),
		Codex:    get("AMF_CODEX", "codex"),
		Opencode: get("AMF_OPENCODE", "opencode"),

		ClaudeToken:      get("CLAUDE_CODE_OAUTH_TOKEN", ""),
		AnthropicKey:     get("ANTHROPIC_API_KEY", ""),
		OpenAIKey:        get("OPENAI_API_KEY", ""),
		OpencodeKey:      get("OPENCODE_API_KEY", ""),
		OpencodeModel:    get("OPENCODE_MODEL", defaultOpencodeModel),
		PostgresURL:      get("AMF_POSTGRES_URL", ""),
		PrimaryAgent:     get("AMF_PRIMARY_AGENT", defaultPrimaryAgent),
		SecondaryAgent:   get("AMF_SECONDARY_AGENT", defaultSecondaryAgent),
		Trusted:          parseTrusted(get("AMF_TRUSTED", "")),
		AgentTimeout:     get("AMF_AGENT_TIMEOUT", ""),
		AgentIdleTimeout: get("AMF_AGENT_IDLE_TIMEOUT", ""),
		LogLevel:         get("AMF_LOG_LEVEL", ""),
		StaleTimeout:     get("AMF_STALE_TIMEOUT", ""),
		PRSectionsRaw:    get("AMF_PR_SECTIONS", ""),
		EnvPaths:         paths,
		EnvFile:          envFile,
		RepoConfigFile:   repoFile,
		seen:             seen,
	}
	cfg.PRSections = cfg.PRSectionsRaw == "on"
	cfg.Token, cfg.TokenSource = resolveToken(get, seen, envFile, authHost(cfg.APIURL))
	if cfg.Token == "" {
		return cfg, errNoToken
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

// repoSettingsPath is the box-local settings file for a repo, or "" when
// the repo or the config folder can't name one.
func repoSettingsPath(apiURL, repo string) string {
	if validRepo(repo) != nil {
		return ""
	}
	d, err := configDir()
	if err != nil {
		return ""
	}
	owner, name, _ := strings.Cut(strings.ToLower(repo), "/")
	return filepath.Join(d, "repos", authHost(apiURL), owner, name+".env")
}

// parsePRSections reads AMF_PR_SECTIONS: on, off, or empty (off).
func parsePRSections(v string) (bool, error) {
	switch v {
	case "", "off":
		return false, nil
	case "on":
		return true, nil
	}
	return false, fmt.Errorf("AMF_PR_SECTIONS=%q: want on or off", v)
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

// parseAgentIdleTimeout reads AMF_AGENT_IDLE_TIMEOUT. An empty value means
// the default; "off" disables the idle timeout.
func parseAgentIdleTimeout(v string) (time.Duration, error) {
	switch v {
	case "":
		return defaultAgentIdleTimeout, nil
	case "off":
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("AMF_AGENT_IDLE_TIMEOUT=%q: not a duration (use e.g. 30m, or off)", v)
	}
	if d <= 0 {
		return 0, fmt.Errorf("AMF_AGENT_IDLE_TIMEOUT: the idle timeout must be positive, got %s (use off to disable it)", d)
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
var shownConfig = []string{"GITHUB_REPO", "AMF_API_URL", "OPENCODE_MODEL", "AMF_AGENT_TIMEOUT", "AMF_AGENT_IDLE_TIMEOUT", "AMF_STALE_TIMEOUT", "AMF_PR_SECTIONS", "AMF_LOG_LEVEL", "AMF_CLAUDE", "AMF_CODEX", "AMF_OPENCODE", "AMF_PRIMARY_AGENT", "AMF_SECONDARY_AGENT"}

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
