package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// pathOptions are the path flags every command accepts, before or after the
// command name: --dir, --env-file and --clone-dir. Relative values resolve
// against the directory amf was started in (os.Getwd), never $PWD or $HOME.
// Without them every default is the current directory: the checkout it is
// in, ./.env, ./<repo name> for a clone, and ./.amf/ for amf's own state.
type pathOptions struct {
	Dir      string // --dir: work as if started here
	EnvFile  string // --env-file: the only .env read (and the one amf init writes)
	CloneDir string // --clone-dir: the checkout to use, cloning GITHUB_REPO there if missing

	// Start is the directory amf was started in, before --dir, which
	// relative flag values (--log-file, --memory-dir too) resolve against.
	Start string
}

// paths holds the resolved, absolute path flags of the running command.
var paths pathOptions

// extractPathFlags removes --dir, --env-file and --clone-dir (as --f v,
// --f=v, -f v or -f=v) from args, stopping at "--", and returns the rest.
func extractPathFlags(args []string, into *pathOptions) ([]string, error) {
	targets := map[string]*string{"dir": &into.Dir, "env-file": &into.EnvFile, "clone-dir": &into.CloneDir}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") || targets[name] == nil {
			rest = append(rest, a)
			continue
		}
		if !hasVal {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("flag needs an argument: --%s", name)
			}
			i++
			val = args[i]
		}
		if strings.TrimSpace(val) == "" {
			return nil, fmt.Errorf("--%s can't be empty", name)
		}
		*targets[name] = val
	}
	return rest, nil
}

// resolvePathOptions makes each set path absolute against the current
// directory, then changes into --dir, so every default after it is there.
func resolvePathOptions(o pathOptions) (pathOptions, error) {
	o.Start, _ = os.Getwd()
	for _, p := range []*string{&o.Dir, &o.EnvFile, &o.CloneDir} {
		if *p == "" {
			continue
		}
		abs, err := absFromCwd(*p)
		if err != nil {
			return o, err
		}
		*p = abs
	}
	if o.Dir != "" {
		st, err := os.Stat(o.Dir)
		if err != nil {
			return o, fmt.Errorf("--dir: %w", err)
		}
		if !st.IsDir() {
			return o, fmt.Errorf("--dir: %s is not a directory", o.Dir)
		}
		if err := os.Chdir(o.Dir); err != nil {
			return o, fmt.Errorf("--dir: %w", err)
		}
	}
	return o, nil
}

// absFlag is a path flag's value made absolute against the directory amf
// was started in (before --dir), or the current directory.
func absFlag(p string) (string, error) {
	if paths.Start != "" && !filepath.IsAbs(p) && !strings.HasPrefix(p, "~") {
		return filepath.Join(paths.Start, p), nil
	}
	return absFromCwd(p)
}

// absFromCwd is p made absolute against os.Getwd.
func absFromCwd(p string) (string, error) {
	if strings.HasPrefix(p, "~") {
		return "", errors.New("paths starting with ~ are not expanded; use an absolute or relative path")
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return filepath.Join(wd, p), nil
}

// cloneDirFor is where run, watch and auto clone GITHUB_REPO when amf isn't
// started inside a checkout: --clone-dir, else <cwd>/<repo name>.
func cloneDirFor(repo string) (string, error) {
	if paths.CloneDir != "" {
		return paths.CloneDir, nil
	}
	_, name, _ := strings.Cut(repo, "/")
	return absFromCwd(name)
}
