package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// checkoutRoot returns the top of the git checkout amf works in. Inside a git
// checkout that is the checkout itself. Anywhere else, such as a folder that
// only holds .env, amf clones GITHUB_REPO into ./<name>, or reuses the clone
// a previous run made there.
//
// how says which it was: checkout, clone (reused) or cloned.
func checkoutRoot(ctx context.Context, cfg Config, log io.Writer) (root, how string, err error) {
	if root, err := git(ctx, ".", "rev-parse", "--show-toplevel"); err == nil {
		return root, "checkout", nil
	}
	_, name, _ := strings.Cut(cfg.Repo, "/")
	dir, err := filepath.Abs(name)
	if err != nil {
		return "", "", err
	}

	if _, err := os.Stat(dir); err == nil {
		origin, err := git(ctx, dir, "remote", "get-url", "origin")
		if err != nil || !strings.EqualFold(repoFromRemote(origin), cfg.Repo) && origin != cfg.CloneURL {
			return "", "", fmt.Errorf("%s already exists but is not a clone of %s; move it, or run amf inside a clone", dir, cfg.Repo)
		}
		fmt.Fprintf(log, "amf: not inside a git checkout; using the clone at %s\n", dir)
		return dir, "clone", nil
	}

	fmt.Fprintf(log, "amf: not inside a git checkout; cloning %s into %s\n", cfg.Repo, dir)
	if _, err := git(ctx, ".", "clone", cfg.CloneURL, dir); err != nil {
		return "", "", fmt.Errorf("%w\namf clones with your own git credentials (e.g. run `gh auth setup-git`), or clone %s yourself and run amf inside it", err, cfg.Repo)
	}
	return dir, "cloned", nil
}

// checkRepo confirms GITHUB_REPO exists and GITHUB_TOKEN can see it, so a
// typo in .env fails with a clear message instead of a git or API error later.
func checkRepo(ctx context.Context, gh *GitHub, repo string) error {
	_, err := gh.DefaultBranch(ctx)
	switch {
	case isStatus(err, http.StatusNotFound):
		return fmt.Errorf("GITHUB_REPO %s was not found on GitHub, or GITHUB_TOKEN can't access it; check the spelling in .env", repo)
	case isStatus(err, http.StatusUnauthorized):
		return errors.New("GitHub rejected GITHUB_TOKEN; check it in .env, or create a new one")
	}
	return err
}

// repoFromRemote turns a GitHub remote URL into owner/name, or "" if it
// doesn't look like one.
func repoFromRemote(url string) string {
	repo := normalizeRepo(strings.TrimPrefix(url, "ssh://git@github.com/"))
	if owner, name, ok := strings.Cut(repo, "/"); !ok || owner == "" || name == "" || strings.ContainsAny(name, "/:") || strings.Contains(owner, ":") {
		return ""
	}
	return repo
}
