package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// doctorOutput runs amf doctor and checks that every line is one check.
func doctorOutput(t *testing.T) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(context.Background(), []string{"doctor"}, nil, &out, io.Discard)
	line := regexp.MustCompile(`^(ok|warn|FAIL|skip) +\S+( \S+)? {2,}\S.*$`)
	for _, l := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if !line.MatchString(l) {
			t.Errorf("not a check line: %q", l)
		}
	}
	return out.String(), err
}

func wantLines(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !regexp.MustCompile(`(?m)^` + w).MatchString(out) {
			t.Errorf("output has no line matching %q:\n%s", w, out)
		}
	}
}

func TestDoctorAllGood(t *testing.T) {
	root, origin := setupPipelineRepo(t, "## amf\n\n| Stage | Agent |\n|---|---|\n| code | claudecode |\n")
	gh, srv := newFakeGitHub(t)
	for _, l := range amfLabels {
		gh.repoLabels[l.Name] = l
	}
	t.Chdir(root)
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)
	t.Setenv("AMF_CLONE_URL", origin)
	t.Setenv("AMF_RELEASES_URL", fakeReleases(t, "v9.9.9", "amf v9.9.9", false).URL)
	t.Setenv("AMF_CLAUDE", fakeAgent(t, "claude", `echo "1.2.3 (Claude Code)"`))
	t.Setenv("AMF_CODEX", filepath.Join(t.TempDir(), "no-codex"))
	t.Setenv("AMF_OPENCODE", fakeAgent(t, "opencode", `echo "opencode v2"`))

	out, err := doctorOutput(t)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	wantLines(t, out,
		`warn +amf +a development build \(dev\); the latest release is v9\.9\.9$`,
		`ok +git +git version`,
		`warn +\.env +none in `,
		`ok +GITHUB_TOKEN +set, from the environment$`,
		`ok +GITHUB_REPO +vic/demo, from the environment$`,
		`ok +github +token accepted for @vic at http`,
		`ok +repo +vic/demo, default branch main$`,
		`ok +labels +all 4 amf labels exist$`,
		`ok +checkout +.*/work \(origin .*origin\.git\)$`,
		`ok +git push +your git credentials can push`,
		`ok +pipeline +origin/main: code \(claudecode, default model\)$`,
		`ok +claudecode +.*/claude, 1\.2\.3 \(Claude Code\)$`,
		`warn +codex +.*no-codex is not installed; only needed by pipelines that use codex$`,
		`ok +opencode +.*/opencode, opencode v2$`,
		`ok +claude login +CLAUDE_CODE_OAUTH_TOKEN is not set; claude uses its own login$`,
	)
	// Doctor changes nothing: no branch was pushed.
	if refs := mustGit(t, root, "--git-dir", origin, "branch", "--list", "amf/*"); refs != "" {
		t.Errorf("doctor pushed %q", refs)
	}
}

func TestDoctorBrokenSetup(t *testing.T) {
	_, srv := newFakeGitHub(t)
	dir := t.TempDir()
	t.Chdir(dir)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("GITHUB_TOKEN=wrong\nGITHUB_REPO=vic/demo\nCLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat-x\n"), 0o600)
	t.Setenv("AMF_API_URL", srv.URL)
	t.Setenv("AMF_RELEASES_URL", "http://127.0.0.1:1")
	t.Setenv("AMF_CLAUDE", filepath.Join(t.TempDir(), "no-claude"))

	out, err := doctorOutput(t)
	if err == nil || err.Error() != "doctor found 2 problems" {
		t.Fatalf("error = %v\n%s", err, out)
	}
	wantLines(t, out,
		`warn +amf +dev; could not check for a newer release`,
		`ok +\.env +.*/\.env$`,
		`ok +GITHUB_TOKEN +set, from .*/\.env$`,
		`FAIL +github +GitHub rejected GITHUB_TOKEN`,
		`skip +repo `,
		`skip +labels `,
		`warn +checkout +not in a git checkout; run and watch will clone vic/demo into .*/demo$`,
		`skip +git push `,
		`skip +pipeline `,
		`FAIL +claudecode +.*no-claude is not installed; amf runs it for every issue$`,
		`ok +claude login +CLAUDE_CODE_OAUTH_TOKEN is set, from .*/\.env; claude runs with it$`,
	)
	if strings.Contains(out, "wrong") || strings.Contains(out, "sk-ant-oat-x") {
		t.Errorf("doctor printed a secret:\n%s", out)
	}

	// A folder in the way of the clone.
	os.MkdirAll(filepath.Join(dir, "demo"), 0o755)
	out, _ = doctorOutput(t)
	wantLines(t, out, `FAIL +checkout +.*/demo exists but is not a clone of vic/demo`)
}

func TestDoctorConfigProblems(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("AMF_RELEASES_URL", "http://127.0.0.1:1")
	t.Setenv("AMF_CLAUDE", fakeAgent(t, "claude", "echo 1"))

	os.WriteFile(filepath.Join(dir, ".env"), []byte("not a pair\n"), 0o600)
	out, _ := doctorOutput(t)
	wantLines(t, out, `FAIL +\.env +.*line 1: expected KEY=VALUE`, `skip +GITHUB_TOKEN `, `skip +GITHUB_REPO `, `skip +github `)

	os.WriteFile(filepath.Join(dir, ".env"), []byte("GITHUB_TOKEN=github_pat_...\nGITHUB_REPO=a/b/c\n"), 0o600)
	out, err := doctorOutput(t)
	wantLines(t, out, `FAIL +GITHUB_TOKEN +not set`, `FAIL +GITHUB_REPO +GITHUB_REPO must look like owner/name`, `skip +checkout `)
	if err == nil || !strings.Contains(err.Error(), "2 problems") {
		t.Errorf("error = %v", err)
	}
}

func TestDoctorPipelineProblems(t *testing.T) {
	root, origin := setupPipelineRepo(t, "## amf\n\n| Stage | Skill | Agent |\n|---|---|---|\n"+
		"| code | - | codex |\n| eval | https://127.0.0.1:1/eval/SKILL.md | codex |\n")
	_, srv := newFakeGitHub(t)
	t.Chdir(root)
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)
	t.Setenv("AMF_CLONE_URL", origin)
	t.Setenv("AMF_RELEASES_URL", "http://127.0.0.1:1")
	t.Setenv("AMF_CLAUDE", filepath.Join(t.TempDir(), "no-claude"))
	t.Setenv("AMF_CODEX", filepath.Join(t.TempDir(), "no-codex"))

	out, err := doctorOutput(t)
	if err == nil || err.Error() != "doctor found 2 problems" {
		t.Fatalf("error = %v\n%s", err, out)
	}
	wantLines(t, out,
		`warn +labels +missing amf:ready, amf:in-progress, amf:done, amf:failed`,
		`ok +pipeline +origin/main: code \(codex, default model\) -> eval \(codex, default model\)$`,
		`FAIL +skill eval +downloading skill https://127\.0\.0\.1:1/eval/SKILL\.md`,
		// claude isn't used by this pipeline.
		`warn +claudecode +.*no-claude is not installed; only needed by pipelines that use claudecode$`,
		`FAIL +codex +.*no-codex is not installed, and the pipeline's code and eval stages use it$`,
	)
	// A broken table is a problem too.
	os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("## amf\n\n| Stage | Agent |\n|---|---|\n| code | gemini |\n"), 0o644)
	mustGit(t, root, "commit", "-qam", "break it")
	mustGit(t, root, "push", "-q", "origin", "main")
	out, _ = doctorOutput(t)
	wantLines(t, out, `FAIL +pipeline +.*unknown agent "gemini"`)
}
