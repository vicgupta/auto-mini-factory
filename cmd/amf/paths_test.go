package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// wd is the current directory as os.Getwd reports it (symlinks resolved the
// same way the code under test sees them).
func wd(t *testing.T) string {
	t.Helper()
	d, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func resetPaths(t *testing.T) { t.Cleanup(func() { paths = pathOptions{} }) }

func runPaths(t *testing.T, args ...string) {
	t.Helper()
	var out bytes.Buffer
	if err := run(context.Background(), append(args, "version"), nil, &out, &out); err != nil {
		t.Fatalf("run %v: %v\n%s", args, err, out.String())
	}
}

func TestPathDefaultsAreCwd(t *testing.T) {
	resetPaths(t)
	t.Chdir(t.TempDir())
	// Neither $PWD nor $HOME decides a default: only os.Getwd does.
	t.Setenv("PWD", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	runPaths(t)
	cwd := wd(t)
	if got := envPaths(context.Background()); got[0] != filepath.Join(cwd, ".env") {
		t.Errorf("envPaths = %v, want %s first", got, filepath.Join(cwd, ".env"))
	}
	if got, _ := cloneDirFor("o/repo"); got != filepath.Join(cwd, "repo") {
		t.Errorf("clone dir = %s, want %s", got, filepath.Join(cwd, "repo"))
	}
	if got, _ := absFromCwd(".amf/runs/x/events.jsonl"); got != filepath.Join(cwd, ".amf", "runs", "x", "events.jsonl") {
		t.Errorf("log file = %s", got)
	}
}

func TestPathFlagsOverrideDefaults(t *testing.T) {
	resetPaths(t)
	t.Chdir(t.TempDir())
	other := t.TempDir()
	env := filepath.Join(other, "amf.env")
	clone := filepath.Join(other, "clone")
	runPaths(t, "--env-file", env, "--clone-dir="+clone)
	if got := envPaths(context.Background()); !slices.Equal(got, []string{env}) {
		t.Errorf("envPaths = %v, want only %s", got, env)
	}
	if got, _ := cloneDirFor("o/repo"); got != clone {
		t.Errorf("clone dir = %s, want %s", got, clone)
	}
}

func TestPathFlagsAfterCommand(t *testing.T) {
	rest, err := extractPathFlags([]string{"run", "42", "-dir", "a", "--env-file=b", "--agent", "codex", "--", "--clone-dir", "x"}, &pathOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"run", "42", "--agent", "codex", "--", "--clone-dir", "x"}; !slices.Equal(rest, want) {
		t.Errorf("rest = %v, want %v", rest, want)
	}
	if _, err := extractPathFlags([]string{"run", "--dir"}, &pathOptions{}); err == nil {
		t.Error("--dir without a value: no error")
	}
	if _, err := extractPathFlags([]string{"--dir="}, &pathOptions{}); err == nil {
		t.Error("empty --dir: no error")
	}
}

func TestRelativePathFlagsResolveAgainstCwd(t *testing.T) {
	resetPaths(t)
	t.Chdir(t.TempDir())
	start := wd(t)
	if err := os.Mkdir("work", 0o755); err != nil {
		t.Fatal(err)
	}
	runPaths(t, "--env-file", "conf/.env", "--clone-dir", "clones/r", "--dir", "work")
	if paths.EnvFile != filepath.Join(start, "conf", ".env") || paths.CloneDir != filepath.Join(start, "clones", "r") {
		t.Errorf("paths = %+v, want them under %s", paths, start)
	}
	if got := wd(t); got != filepath.Join(start, "work") {
		t.Errorf("--dir: cwd = %s, want %s", got, filepath.Join(start, "work"))
	}
	// Like --log-file and --memory-dir, which resolve after --dir.
	if got, _ := absFlag("events.jsonl"); got != filepath.Join(start, "events.jsonl") {
		t.Errorf("--log-file events.jsonl = %s, want it in %s", got, start)
	}
	if got, _ := (memoryOptions{Dir: "mem"}).baseDir(); got != filepath.Join(start, "mem") {
		t.Errorf("--memory-dir mem = %s, want it in %s", got, start)
	}
	if got, _ := (memoryOptions{}).baseDir(); got != filepath.Join(start, "work") {
		t.Errorf("default memory dir after --dir = %s", got)
	}
	// After --dir, the defaults follow it.
	if got, _ := cloneDirFor("o/x"); got != paths.CloneDir {
		t.Errorf("clone dir = %s", got)
	}
	paths.CloneDir = ""
	if got, _ := cloneDirFor("o/x"); got != filepath.Join(start, "work", "x") {
		t.Errorf("default clone dir after --dir = %s", got)
	}
}

func TestDirFlagBeatsEnv(t *testing.T) {
	resetPaths(t)
	t.Chdir(t.TempDir())
	target := t.TempDir()
	t.Setenv("PWD", t.TempDir())
	runPaths(t, "--dir", target)
	got, _ := filepath.EvalSymlinks(wd(t))
	want, _ := filepath.EvalSymlinks(target)
	if got != want {
		t.Errorf("cwd = %s, want %s", got, want)
	}
	if err := run(context.Background(), []string{"--dir", filepath.Join(target, "missing"), "version"}, nil, &bytes.Buffer{}, &bytes.Buffer{}); err == nil {
		t.Error("missing --dir: no error")
	}
}

func TestCloneDirFlagBeatsEnclosingCheckout(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	resetPaths(t)
	base := t.TempDir()
	gitInit := func(dir string) {
		for _, args := range [][]string{{"init", "-q", dir}, {"-C", dir, "remote", "add", "origin", "https://github.com/o/repo.git"}} {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
	}
	here, clone := filepath.Join(base, "here"), filepath.Join(base, "clone")
	gitInit(here)
	gitInit(clone)
	t.Chdir(here)
	cfg := Config{Repo: "o/repo", CloneURL: "https://github.com/o/repo.git"}
	root, how, err := checkoutRoot(context.Background(), cfg, &bytes.Buffer{})
	if err != nil || how != "checkout" {
		t.Fatalf("default: %s %s %v, want the enclosing checkout", root, how, err)
	}
	paths.CloneDir = clone
	root, how, err = checkoutRoot(context.Background(), cfg, &bytes.Buffer{})
	if err != nil || root != clone || how != "clone" {
		t.Fatalf("--clone-dir: %s %s %v, want %s (clone)", root, how, err, clone)
	}
}
