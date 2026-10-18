package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// envTemplate is what `amf init` writes to .env, in order.
var envTemplate = []struct{ key, placeholder, comment string }{
	{"GITHUB_TOKEN", "github_pat_...", "Contents, Issues, Pull requests: read & write"},
	{"GITHUB_REPO", "owner/name", ""},
	{"CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-...", "optional: Claude subscription token from `claude setup-token`"},
}

// isPlaceholder reports whether v is an unfilled value from the template.
func isPlaceholder(v string) bool {
	return strings.HasSuffix(v, "...") || v == "owner/name"
}

// cmdInit writes .env in the current directory with every variable amf uses.
// An existing .env is first copied to the next free .env.NNNN, and its real
// values are carried over into the new file.
func cmdInit(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) != 0 {
		fmt.Fprintln(stderr, "usage: amf init")
		return errUsage
	}
	const path = ".env"

	old := map[string]string{}
	var oldKeys []string // to keep extra variables in their original order
	if raw, err := os.ReadFile(path); err == nil {
		if old, err = parseDotEnv(strings.NewReader(string(raw))); err != nil {
			return fmt.Errorf("%s: %w (fix or move it, then re-run amf init)", path, err)
		}
		oldKeys = envKeys(string(raw))
		backup, err := backupFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "amf: backed up %s to %s\n", path, backup)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	if old["GITHUB_REPO"] == "" || isPlaceholder(old["GITHUB_REPO"]) {
		if repo := originRepo(ctx); repo != "" {
			old["GITHUB_REPO"] = repo
		}
	}

	var b strings.Builder
	b.WriteString("# amf configuration. Keep this file out of git.\n")
	var todo []string
	for _, t := range envTemplate {
		val := old[t.key]
		if val == "" || isPlaceholder(val) {
			val = t.placeholder
			todo = append(todo, t.key)
		}
		line := t.key + "=" + quoteEnv(val)
		if t.comment != "" {
			line += "   # " + t.comment
		}
		b.WriteString(line + "\n")
	}
	var extra []string
	for _, k := range oldKeys {
		if !slices.ContainsFunc(envTemplate, func(t struct{ key, placeholder, comment string }) bool { return t.key == k }) {
			extra = append(extra, k+"="+quoteEnv(old[k]))
		}
	}
	if len(extra) > 0 {
		b.WriteString("\n# Kept from the previous .env\n" + strings.Join(extra, "\n") + "\n")
	}

	if err := writeFileAtomic(path, []byte(b.String()), 0o600); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "amf: wrote %s\n", path)

	if root, err := git(ctx, ".", "rev-parse", "--show-toplevel"); err == nil {
		if err := excludeFromGit(ctx, root, "amf secrets", "/.env", "/.env.[0-9][0-9][0-9][0-9]"); err != nil {
			fmt.Fprintf(stderr, "amf: warning: could not update .git/info/exclude: %v\n", err)
		}
	}

	if len(todo) > 0 {
		fmt.Fprintf(stdout, "Next: edit %s and fill in %s.\n", path, strings.Join(todo, ", "))
		if slices.Contains(todo, "CLAUDE_CODE_OAUTH_TOKEN") {
			fmt.Fprintln(stdout, "CLAUDE_CODE_OAUTH_TOKEN is optional; leave the placeholder to use your existing Claude login.")
		}
	}
	return nil
}

// backupFile copies path to path.NNNN, one higher than the highest existing backup.
func backupFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	matches, _ := filepath.Glob(path + ".[0-9][0-9][0-9][0-9]")
	next := 1
	for _, m := range matches {
		if n, err := strconv.Atoi(m[len(path)+1:]); err == nil && n >= next {
			next = n + 1
		}
	}
	for ; next <= 9999; next++ {
		name := fmt.Sprintf("%s.%04d", path, next)
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(data)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			os.Remove(name)
			return "", werr
		}
		return name, nil
	}
	return "", fmt.Errorf("too many backups of %s; remove some %s.NNNN files", path, path)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// originRepo is owner/name of the current checkout's GitHub origin, or "".
func originRepo(ctx context.Context) string {
	url, err := git(ctx, ".", "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	repo := normalizeRepo(strings.TrimPrefix(url, "ssh://git@github.com/"))
	if owner, name, ok := strings.Cut(repo, "/"); !ok || owner == "" || name == "" || strings.ContainsAny(name, "/:") || strings.Contains(owner, ":") {
		return ""
	}
	return repo
}

// envKeys lists the keys in a .env file in the order they appear.
func envKeys(s string) []string {
	var keys []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "export ")
		if line == "" || line[0] == '#' {
			continue
		}
		if k, _, ok := strings.Cut(line, "="); ok && !slices.Contains(keys, strings.TrimSpace(k)) {
			keys = append(keys, strings.TrimSpace(k))
		}
	}
	return keys
}

// quoteEnv quotes a value if parseDotEnv would otherwise misread it.
func quoteEnv(v string) string {
	if !strings.ContainsAny(v, " \t#'\"") {
		return v
	}
	if !strings.Contains(v, `"`) {
		return `"` + v + `"`
	}
	return "'" + v + "'"
}
