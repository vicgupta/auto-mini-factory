package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"
)

// doctor prints one line per check: ok, warn, FAIL, or skip when an earlier
// check it depends on failed.
type doctor struct {
	w     *tabwriter.Writer
	fails int
}

func (d *doctor) line(status, check, format string, a ...any) {
	if status == "FAIL" {
		d.fails++
	}
	fmt.Fprintf(d.w, "%s\t%s\t%s\n", status, check, fmt.Sprintf(format, a...))
}

// cmdDoctor checks everything amf needs to run here, without changing
// anything: no clone, no labels, no push.
func cmdDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: amf doctor")
		return errUsage
	}
	d := &doctor{w: tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)}
	d.check(ctx)
	d.w.Flush()
	if d.fails > 0 {
		return fmt.Errorf("doctor found %d %s", d.fails, plural(d.fails, "problem", "problems"))
	}
	return nil
}

func (d *doctor) check(ctx context.Context) {
	d.checkVersion(ctx)
	if out, err := exec.CommandContext(ctx, "git", "--version").Output(); err != nil {
		d.line("FAIL", "git", "not installed; amf needs git on PATH")
	} else {
		d.line("ok", "git", "%s", strings.TrimSpace(string(out)))
	}

	// Configuration.
	paths := envPaths(ctx)
	envFile := ""
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			envFile, _ = filepath.Abs(p)
			break
		}
	}
	cfg, err := loadConfig(paths...)
	parsed := err == nil || cfg.Claude != "" // loadConfig fills cfg unless the file didn't parse
	switch {
	case !parsed:
		d.line("FAIL", ".env", "%v", err)
	case envFile == "":
		d.line("warn", ".env", "none in %s; using the environment only (amf init writes one)", strings.Join(paths, " or "))
	default:
		d.line("ok", ".env", "%s", envFile)
	}
	tokenOK, repoOK := cfg.Token != "", cfg.Repo != "" && validRepo(cfg.Repo) == nil
	switch {
	case !parsed:
		d.line("skip", "GITHUB_TOKEN", ".env didn't parse")
	case !tokenOK:
		d.line("FAIL", "GITHUB_TOKEN", "not set (run amf init, then edit .env)")
	default:
		d.line("ok", "GITHUB_TOKEN", "set, from %s", source("GITHUB_TOKEN", envFile))
	}
	switch {
	case !parsed:
		d.line("skip", "GITHUB_REPO", ".env didn't parse")
	case cfg.Repo == "":
		d.line("FAIL", "GITHUB_REPO", "not set (run amf init, then edit .env)")
	case !repoOK:
		d.line("FAIL", "GITHUB_REPO", "%v", validRepo(cfg.Repo))
	default:
		d.line("ok", "GITHUB_REPO", "%s, from %s", cfg.Repo, source("GITHUB_REPO", envFile))
	}

	// GitHub.
	base := ""
	if gh := newGitHub(cfg); tokenOK && repoOK {
		base = d.checkGitHub(ctx, gh, cfg)
	} else {
		for _, c := range []string{"github", "repo", "labels"} {
			d.line("skip", c, "needs GITHUB_TOKEN and GITHUB_REPO")
		}
	}

	// The checkout, and pushing from it.
	root := d.checkCheckout(ctx, cfg, repoOK)
	if root == "" {
		d.line("skip", "git push", "no checkout yet")
	} else {
		pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		cmd := exec.CommandContext(pctx, "git", "push", "--dry-run", "--porcelain", "origin", "HEAD:refs/heads/amf/doctor-check")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0") // fail rather than ask for a password
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		cancel()
		if err != nil {
			msg := strings.TrimSpace(stderr.String())
			if i := strings.LastIndex(msg, "\n"); i >= 0 {
				msg = msg[i+1:]
			}
			if msg == "" {
				msg = err.Error()
			}
			d.line("FAIL", "git push", "%s (amf pushes with your git credentials; for GitHub, run gh auth setup-git)", msg)
		} else {
			d.line("ok", "git push", "your git credentials can push to origin (checked with --dry-run)")
		}
	}

	// The pipeline, its skills, and the agents.
	var p *Pipeline
	pipelineKnown := false
	switch {
	case root == "" || base == "":
		d.line("skip", "pipeline", "needs a checkout and the default branch")
	default:
		var err error
		if p, err = readPipeline(ctx, root, base); err != nil {
			d.line("FAIL", "pipeline", "%v", err)
		} else if p == nil {
			pipelineKnown = true
			d.line("ok", "pipeline", "none in AGENTS.md on origin/%s; amf runs claude once per issue", base)
		} else {
			pipelineKnown = true
			d.line("ok", "pipeline", "origin/%s: %s", base, describePipeline(p))
		}
	}
	if p != nil {
		for _, s := range p.stages() {
			if s.Skill == "" {
				continue
			}
			text, err := fetchSkill(ctx, nil, s.Skill)
			if err != nil {
				d.line("FAIL", "skill "+s.Name, "%v", err)
				continue
			}
			d.line("ok", "skill "+s.Name, "%.1f KB, sha256 %x", float64(len(text))/1024, sha256.Sum256([]byte(text)))
		}
	}
	bins := map[string]string{agentClaude: cfg.Claude, agentCodex: cfg.Codex, agentOpencode: cfg.Opencode}
	for _, agent := range []string{agentClaude, agentCodex, agentOpencode} {
		bin := bins[agent]
		if bin == "" {
			bin = agentCommand[agent]
		}
		var users []string // why it's needed
		if p != nil {
			for _, s := range p.stages() {
				if s.Agent == agent {
					users = append(users, s.Name)
				}
			}
		} else if agent == agentClaude {
			users = []string{"every issue"}
		}
		path, err := exec.LookPath(bin)
		switch {
		case err == nil:
			d.line("ok", agent, "%s, %s", path, agentVersion(ctx, path))
		case len(users) > 0 && p != nil:
			n := len(users)
			d.line("FAIL", agent, "%s is not installed, and the pipeline's %s %s %s it", bin, strings.Join(users, " and "), plural(n, "stage", "stages"), plural(n, "uses", "use"))
		case len(users) > 0:
			d.line("FAIL", agent, "%s is not installed; amf runs it for every issue", bin)
		case pipelineKnown:
			d.line("warn", agent, "%s is not installed; only needed by pipelines that use %s", bin, agent)
		default:
			d.line("warn", agent, "%s is not installed; needed if the pipeline uses %s", bin, agent)
		}
	}
	if cfg.ClaudeToken != "" {
		d.line("ok", "claude login", "CLAUDE_CODE_OAUTH_TOKEN is set, from %s; claude runs with it", source("CLAUDE_CODE_OAUTH_TOKEN", envFile))
	} else {
		d.line("ok", "claude login", "CLAUDE_CODE_OAUTH_TOKEN is not set; claude uses its own login")
	}
}

func (d *doctor) checkVersion(ctx context.Context) {
	current := buildVersion()
	base := releasesURL
	if v := os.Getenv("AMF_RELEASES_URL"); v != "" {
		base = strings.TrimSuffix(v, "/")
	}
	latest, err := (&updater{base: base}).latest(ctx)
	switch {
	case err != nil:
		d.line("warn", "amf", "%s; could not check for a newer release: %v", current, err)
	case current == latest:
		d.line("ok", "amf", "%s, the latest release", current)
	case !releaseTag.MatchString(current):
		d.line("warn", "amf", "a development build (%s); the latest release is %s", current, latest)
	default:
		d.line("warn", "amf", "%s; %s is available (run amf update)", current, latest)
	}
}

// checkGitHub checks the token, the repo and the labels, and returns the
// default branch, or "" if the repo can't be read.
func (d *doctor) checkGitHub(ctx context.Context, gh *GitHub, cfg Config) string {
	var user struct {
		Login string `json:"login"`
	}
	err := gh.do(ctx, "GET", "/user", nil, &user)
	switch {
	case isStatus(err, http.StatusUnauthorized):
		d.line("FAIL", "github", "GitHub rejected GITHUB_TOKEN; check it in .env, or create a new one")
		d.line("skip", "repo", "needs a working GITHUB_TOKEN")
		d.line("skip", "labels", "needs a working GITHUB_TOKEN")
		return ""
	case err != nil && !isStatus(err, http.StatusForbidden): // some tokens may not read /user
		d.line("FAIL", "github", "can't reach %s: %v", cfg.APIURL, err)
		d.line("skip", "repo", "needs GitHub")
		d.line("skip", "labels", "needs GitHub")
		return ""
	case user.Login != "":
		d.line("ok", "github", "token accepted for @%s at %s", user.Login, cfg.APIURL)
	default:
		d.line("ok", "github", "%s is reachable", cfg.APIURL)
	}

	var repo struct {
		DefaultBranch string `json:"default_branch"`
		Permissions   *struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	err = gh.do(ctx, "GET", gh.repoPath(""), nil, &repo)
	switch {
	case isStatus(err, http.StatusNotFound):
		d.line("FAIL", "repo", "%s was not found on GitHub, or GITHUB_TOKEN can't access it; check the spelling in .env", cfg.Repo)
		d.line("skip", "labels", "needs the repo")
		return ""
	case err != nil:
		d.line("FAIL", "repo", "%v", err)
		d.line("skip", "labels", "needs the repo")
		return ""
	case repo.Permissions != nil && !repo.Permissions.Push:
		d.line("warn", "repo", "%s (default branch %s), but your account can't push to it", cfg.Repo, repo.DefaultBranch)
	default:
		d.line("ok", "repo", "%s, default branch %s", cfg.Repo, repo.DefaultBranch)
	}

	var missing []string
	for _, l := range amfLabels {
		err := gh.do(ctx, "GET", gh.repoPath("/labels/%s", url.PathEscape(l.Name)), nil, nil)
		if isStatus(err, http.StatusNotFound) {
			missing = append(missing, l.Name)
		} else if err != nil {
			d.line("FAIL", "labels", "%v", err)
			return repo.DefaultBranch
		}
	}
	if len(missing) > 0 {
		d.line("warn", "labels", "missing %s (amf init creates them; GitHub also creates them when amf first adds them)", strings.Join(missing, ", "))
	} else {
		d.line("ok", "labels", "all %d amf labels exist", len(amfLabels))
	}
	return repo.DefaultBranch
}

// checkCheckout finds the checkout run and watch would use, without cloning,
// and returns its top level, or "" if there isn't one yet.
func (d *doctor) checkCheckout(ctx context.Context, cfg Config, repoOK bool) string {
	matches := func(origin string) bool {
		return strings.EqualFold(repoFromRemote(origin), cfg.Repo) || origin == cfg.CloneURL
	}
	if root, err := git(ctx, ".", "rev-parse", "--show-toplevel"); err == nil {
		origin, err := git(ctx, root, "remote", "get-url", "origin")
		switch {
		case err != nil:
			d.line("FAIL", "checkout", "%s has no origin remote", root)
			return ""
		case repoOK && !matches(origin):
			d.line("warn", "checkout", "%s, but its origin is %s, not %s", root, origin, cfg.Repo)
		default:
			d.line("ok", "checkout", "%s (origin %s)", root, origin)
		}
		return root
	}
	if !repoOK {
		d.line("skip", "checkout", "not in a git checkout, and GITHUB_REPO isn't usable")
		return ""
	}
	_, name, _ := strings.Cut(cfg.Repo, "/")
	dir, _ := filepath.Abs(name)
	if _, err := os.Stat(dir); err != nil {
		d.line("warn", "checkout", "not in a git checkout; run and watch will clone %s into %s", cfg.Repo, dir)
		return ""
	}
	if origin, err := git(ctx, dir, "remote", "get-url", "origin"); err != nil || !matches(origin) {
		d.line("FAIL", "checkout", "%s exists but is not a clone of %s; move it, or run amf inside a clone", dir, cfg.Repo)
		return ""
	}
	d.line("ok", "checkout", "%s, the clone run and watch use here", dir)
	return dir
}

// releaseTag matches the versions of release builds.
var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// agentVersion is the first line an agent prints for --version.
func agentVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	if err != nil || first == "" {
		return "version unknown"
	}
	return first
}

// source says where a configuration value came from.
func source(key, envFile string) string {
	if v := os.Getenv(key); v != "" && !isPlaceholder(v) {
		return "the environment"
	}
	return envFile
}
