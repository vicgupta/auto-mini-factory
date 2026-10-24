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

// withGitUser gives git a name and email, as setupRepo does, for tests that
// don't need a repo; CI machines have none.
func withGitUser(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(cfg, []byte("[user]\n\tname = amf test\n\temail = amf@example.com\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
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
	t.Setenv("AMF_TRUSTED", "vic,teammate")

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
		`ok +trusted +only issues opened by @vic, @teammate$`,
		`ok +checkout +.*/work \(origin .*origin\.git\)$`,
		`ok +git push +your git credentials can push`,
		`ok +git identity +commits as amf test <amf@example\.com>$`,
		`ok +log folder +.*/work/\.amf/runs is writable$`,
		`ok +pipeline +origin/main: code \(claudecode, default model\)$`,
		`ok +claudecode +.*/claude, 1\.2\.3 \(Claude Code\)$`,
		`warn +codex +.*no-codex is not installed; only needed by pipelines that use codex$`,
		`ok +opencode +.*/opencode, opencode v2$`,
		`ok +claude login +neither CLAUDE_CODE_OAUTH_TOKEN nor ANTHROPIC_API_KEY is set; claude uses its own login, and amf runs it only for a pipeline or --agent claudecode$`,
		`ok +codex login +OPENAI_API_KEY is not set; codex uses its own login, and amf runs it only for a pipeline or --agent codex$`,
	)
	// Doctor changes nothing: no branch was pushed.
	if refs := mustGit(t, root, "--git-dir", origin, "branch", "--list", "amf/*"); refs != "" {
		t.Errorf("doctor pushed %q", refs)
	}
}

func TestDoctorAgentTimeout(t *testing.T) {
	withGitUser(t)
	_, srv := newFakeGitHub(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("AMF_API_URL", srv.URL)
	t.Setenv("AMF_RELEASES_URL", "http://127.0.0.1:1")
	for _, tc := range []struct{ env, want string }{
		{"", `ok +agent timeout +1h0m0s, the default`},
		{"AMF_AGENT_TIMEOUT=90m\n", `ok +agent timeout +1h30m0s, from .*/\.env$`},
		// A bad value fails only this check: the rest of .env still counts.
		{"AMF_AGENT_TIMEOUT=5x\n", `FAIL +agent timeout +AMF_AGENT_TIMEOUT="5x": not a duration`},
	} {
		os.WriteFile(filepath.Join(dir, ".env"), []byte("GITHUB_TOKEN=test-token\nGITHUB_REPO=vic/demo\n"+tc.env), 0o600)
		out, _ := doctorOutput(t)
		wantLines(t, out, `ok +\.env +`, `ok +GITHUB_REPO +vic/demo`, tc.want)
	}
}

func TestDoctorBrokenSetup(t *testing.T) {
	withGitUser(t)
	_, srv := newFakeGitHub(t)
	dir := t.TempDir()
	t.Chdir(dir)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("GITHUB_TOKEN=wrong\nGITHUB_REPO=vic/demo\nCLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat-x\n"), 0o600)
	t.Setenv("AMF_API_URL", srv.URL)
	t.Setenv("AMF_RELEASES_URL", "http://127.0.0.1:1")
	t.Setenv("AMF_CLAUDE", filepath.Join(t.TempDir(), "no-claude"))
	t.Setenv("AMF_CODEX", filepath.Join(t.TempDir(), "no-codex"))
	t.Setenv("AMF_OPENCODE", filepath.Join(t.TempDir(), "no-opencode"))

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
		`FAIL +claudecode +no coding agent is available \(.*no-claude is not installed; .*no-codex is not installed; .*no-opencode is not installed\)`,
		`warn +codex +.*no-codex is not installed either$`,
		`warn +opencode +.*no-opencode is not installed either$`,
		`ok +claude login +CLAUDE_CODE_OAUTH_TOKEN is set, from .*/\.env; claude runs with it$`,
	)
	if strings.Contains(out, "wrong") || strings.Contains(out, "sk-ant-oat-x") {
		t.Errorf("doctor printed a secret:\n%s", out)
	}

	// No git identity, outside a checkout.
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	os.WriteFile(cfg, []byte("[user]\n\tuseConfigOnly = true\n"), 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	out, _ = doctorOutput(t)
	wantLines(t, out, `FAIL +git identity +git has no name and email to commit with`)

	// A folder in the way of the clone.
	os.MkdirAll(filepath.Join(dir, "demo"), 0o755)
	out, _ = doctorOutput(t)
	wantLines(t, out, `FAIL +checkout +.*/demo exists but is not a clone of vic/demo`)
}

func TestDoctorConfigProblems(t *testing.T) {
	withGitUser(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("AMF_RELEASES_URL", "http://127.0.0.1:1")
	t.Setenv("AMF_CLAUDE", fakeAgent(t, "claude", "echo 1"))
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test") // so claude counts as an agent amf can pick

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
		`warn +trusted +AMF_TRUSTED is not set; amf works on issues opened by anyone`,
		`ok +pipeline +origin/main: code \(codex, default model\) -> eval \(codex, default model\)$`,
		`FAIL +skill eval +downloading skill https://127\.0\.0\.1:1/eval/SKILL\.md`,
		// claude isn't used by this pipeline.
		`warn +claudecode +.*no-claude is not installed; only needed by pipelines that use claudecode$`,
		`FAIL +codex +.*no-codex is not installed, and the pipeline's code and eval stages use it$`,
	)
	// The token's own account, which amf issue opens issues as, isn't trusted.
	t.Setenv("AMF_TRUSTED", "teammate")
	out, _ = doctorOutput(t)
	wantLines(t, out, `warn +trusted +only issues opened by @teammate; the token's account, @vic, isn't one`)

	// A broken table is a problem too.
	os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("## amf\n\n| Stage | Agent |\n|---|---|\n| code | gemini |\n"), 0o644)
	mustGit(t, root, "commit", "-qam", "break it")
	mustGit(t, root, "push", "-q", "origin", "main")
	out, _ = doctorOutput(t)
	wantLines(t, out, `FAIL +pipeline +.*unknown agent "gemini"`)
}

func TestDoctorFallbackAgent(t *testing.T) {
	root, origin := setupRepo(t) // no AGENTS.md, so no pipeline
	_, srv := newFakeGitHub(t)
	t.Chdir(root)
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)
	t.Setenv("AMF_CLONE_URL", origin)
	t.Setenv("AMF_RELEASES_URL", "http://127.0.0.1:1")
	t.Setenv("AMF_CLAUDE", filepath.Join(t.TempDir(), "no-claude"))
	t.Setenv("AMF_CODEX", fakeAgent(t, "codex", `echo "codex-cli 9"`))
	t.Setenv("AMF_OPENCODE", filepath.Join(t.TempDir(), "no-opencode"))
	t.Setenv("OPENAI_API_KEY", "sk-openai-test")

	out, _ := doctorOutput(t)
	wantLines(t, out,
		`ok +pipeline +none in AGENTS\.md on origin/main; amf runs codex once per issue$`,
		`warn +claudecode +.*no-claude is not installed; amf runs codex instead, unless --agent or a pipeline names claudecode$`,
		`ok +codex +.*/codex, codex-cli 9; runs issues without a pipeline$`,
		`warn +opencode +.*no-opencode is not installed; amf runs codex instead, unless --agent or a pipeline names opencode$`,
	)
	if strings.Contains(out, "FAIL  claudecode") {
		t.Errorf("a missing claude is fine when codex is installed:\n%s", out)
	}

	// amf pipeline says the same.
	var stdout bytes.Buffer
	if err := run(context.Background(), []string{"pipeline"}, nil, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "so amf runs codex with its built-in prompt (") || !strings.Contains(stdout.String(), "no-claude is not installed), unless --agent names another.") {
		t.Errorf("amf pipeline = %q", stdout.String())
	}
	t.Setenv("AMF_CODEX", filepath.Join(t.TempDir(), "no-codex"))
	stdout.Reset()
	if err := run(context.Background(), []string{"pipeline"}, nil, &stdout, io.Discard); err == nil || !strings.Contains(stdout.String(), "no coding agent is available") {
		t.Errorf("amf pipeline with no agent: %v, %q", err, stdout.String())
	}
}

func TestDoctorAgentWithoutKey(t *testing.T) {
	root, origin := setupRepo(t) // no pipeline
	_, srv := newFakeGitHub(t)
	t.Chdir(root)
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)
	t.Setenv("AMF_CLONE_URL", origin)
	t.Setenv("AMF_RELEASES_URL", "http://127.0.0.1:1")
	t.Setenv("AMF_CLAUDE", fakeAgent(t, "claude", `echo "1.2.3 (Claude Code)"`))
	t.Setenv("AMF_CODEX", fakeAgent(t, "codex", `echo "codex-cli 9"`))
	t.Setenv("AMF_OPENCODE", fakeAgent(t, "opencode", `echo "opencode v2"`))

	out, err := doctorOutput(t)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	wantLines(t, out,
		`ok +pipeline +none in AGENTS\.md on origin/main; amf runs opencode once per issue$`,
		`warn +claudecode +.*/claude, 1\.2\.3 \(Claude Code\); amf picks it only with ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN, so it runs opencode instead \(--agent claudecode runs it with its own login\)$`,
		`warn +codex +.*/codex, codex-cli 9; amf picks it only with OPENAI_API_KEY, so it runs opencode instead \(--agent codex runs it with its own login\)$`,
		`ok +opencode +.*/opencode, opencode v2; runs issues without a pipeline$`,
	)

	// With codex's key, codex is picked instead.
	t.Setenv("OPENAI_API_KEY", "sk-openai-test")
	out, _ = doctorOutput(t)
	wantLines(t, out,
		`ok +pipeline +none in AGENTS\.md on origin/main; amf runs codex once per issue$`,
		`ok +codex +.*/codex, codex-cli 9; runs issues without a pipeline$`,
		`ok +codex login +OPENAI_API_KEY is set, from the environment; codex runs with it$`,
	)
}

func TestDoctorLogFolder(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	root, _ := setupRepo(t)
	if got, err := writableFolder(filepath.Join(root, ".amf", "runs")); err != nil || got != root {
		t.Errorf("writableFolder before .amf exists = %q, %v; want %q, nil", got, err, root)
	}
	if _, err := os.Stat(filepath.Join(root, ".amf")); !os.IsNotExist(err) {
		t.Errorf("the check created .amf: %v", err)
	}
	os.MkdirAll(filepath.Join(root, ".amf", "runs"), 0o755)
	os.Chmod(filepath.Join(root, ".amf", "runs"), 0o555)
	t.Cleanup(func() { os.Chmod(filepath.Join(root, ".amf", "runs"), 0o755) })
	if _, err := writableFolder(filepath.Join(root, ".amf", "runs")); err == nil {
		t.Error("a read-only folder passed")
	}
	entries, _ := os.ReadDir(filepath.Join(root, ".amf", "runs"))
	if len(entries) != 0 {
		t.Errorf("the check left %v behind", entries)
	}
}
