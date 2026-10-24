package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// git runs a git command in dir and returns its trimmed stdout. It logs a
// git.exec event, and git.push for a push, to the logger ctx carries.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	begin := time.Now()
	err := cmd.Run()
	logGit(ctx, dir, args, cmd, err, time.Since(begin), stdout.Len(), stderr.String())
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// logGit logs one git command: git.exec, and git.push for a push.
func logGit(ctx context.Context, dir string, args []string, cmd *exec.Cmd, err error, took time.Duration, stdoutBytes int, stderr string) {
	sub := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			sub = a
			break
		}
	}
	shown := make([]string, len(args))
	for i, a := range args {
		// Commit messages can be long; the event keeps their start.
		if i > 0 && args[i-1] == "-m" && len(a) > 80 {
			a = a[:80] + "…"
		}
		shown[i] = a
	}
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	} else if err != nil {
		code = -1 // git didn't start
	}
	attrs := []any{"subcommand", sub, "args", shown, "dir", dir, "exit_code", code, "duration_ms", ms(took),
		"stdout_bytes", stdoutBytes, "stderr_bytes", len(stderr)}
	if err != nil {
		attrs = append(attrs, "stderr", excerpt(stderr))
	}
	callEvent(ctx, "git.exec", fmt.Sprintf("git %s: exit %d", sub, code), attrs...)
	if sub == "push" {
		callEvent(ctx, "git.push", fmt.Sprintf("git push %s: exit %d", args[len(args)-1], code),
			"branch", args[len(args)-1], "forced", slices.Contains(args, "--force"), "exit_code", code)
	}
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
