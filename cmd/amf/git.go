package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// git runs a git command in dir and returns its trimmed stdout.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// gitIdentity is the name and email git commits with in dir, as
// "Name <email>". It fails when git has none, so amf can say so before an
// agent spends a run on a change it can't commit.
func gitIdentity(ctx context.Context, dir string) (string, error) {
	ident, err := git(ctx, dir, "var", "GIT_AUTHOR_IDENT")
	if err == nil {
		_, err = git(ctx, dir, "var", "GIT_COMMITTER_IDENT")
	}
	if err != nil {
		return "", errors.New(`git has no name and email to commit with; set them with git config --global user.name "Your Name" and git config --global user.email you@example.com`)
	}
	// Drop the timestamp and time zone at the end.
	if i := strings.LastIndex(ident, "> "); i >= 0 {
		ident = ident[:i+1]
	}
	return ident, nil
}

// excludeFromGit adds patterns to .git/info/exclude, so they stay out of
// `git status` without touching the repository's tracked .gitignore.
// Patterns already listed are skipped.
func excludeFromGit(ctx context.Context, root, comment string, patterns ...string) error {
	common, err := git(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil {
		return err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	path := filepath.Join(common, "info", "exclude")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	have := map[string]bool{}
	for _, line := range strings.Split(string(existing), "\n") {
		have[strings.TrimSpace(line)] = true
	}
	var add []string
	for _, p := range patterns {
		if !have[p] {
			add = append(add, p)
		}
	}
	if len(add) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	prefix := ""
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		prefix = "\n"
	}
	_, err = fmt.Fprintf(f, "%s# %s\n%s\n", prefix, comment, strings.Join(add, "\n"))
	return err
}
