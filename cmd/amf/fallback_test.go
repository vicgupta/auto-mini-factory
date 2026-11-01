package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAgentSetting(t *testing.T) {
	for _, tc := range []struct {
		v         string
		allowNone bool
		want      string
		bad       bool
	}{
		{"codex", false, agentCodex, false},
		{"Codex", false, agentCodex, false},
		{"claudecode", false, agentClaude, false},
		{"CLAUDE", false, agentClaude, false},
		{" opencode ", false, agentOpencode, false},
		{"none", true, "", false},
		{"None", true, "", false},
		{"none", false, "", true},
		{"gemini", true, "", true},
	} {
		got, err := parseAgentSetting("AMF_X_AGENT", tc.v, tc.allowNone)
		if tc.bad {
			if err == nil || !strings.Contains(err.Error(), "AMF_X_AGENT must be claudecode, codex") {
				t.Errorf("%q: err = %v", tc.v, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q = %q, %v; want %q", tc.v, got, err, tc.want)
		}
	}
}

func TestAgentSettingsDefaultsAndEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_PRIMARY_AGENT", "")
	t.Setenv("AMF_SECONDARY_AGENT", "")
	os.Unsetenv("AMF_PRIMARY_AGENT")
	os.Unsetenv("AMF_SECONDARY_AGENT")
	cfg, err := loadConfig(filepath.Join(dir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if p, s, err := cfg.agents(); err != nil || p != agentCodex || s != agentClaude {
		t.Errorf("defaults = %q, %q, %v; want codex, claude", p, s, err)
	}
	env := filepath.Join(dir, ".env")
	if err := os.WriteFile(env, []byte("AMF_PRIMARY_AGENT=ClaudeCode\nAMF_SECONDARY_AGENT=opencode\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfig(env)
	if err != nil {
		t.Fatal(err)
	}
	if p, s, err := cfg.agents(); err != nil || p != agentClaude || s != agentOpencode {
		t.Errorf(".env = %q, %q, %v; want claude, opencode", p, s, err)
	}
	for _, c := range []struct{ p, s, wantS string }{{"codex", "none", ""}, {"codex", "codex", ""}} {
		cfg := Config{PrimaryAgent: c.p, SecondaryAgent: c.s}
		if _, s, err := cfg.agents(); err != nil || s != c.wantS {
			t.Errorf("%s/%s: secondary = %q, %v", c.p, c.s, s, err)
		}
	}
	if _, _, err := (Config{PrimaryAgent: "gpt", SecondaryAgent: "claude"}).agents(); err == nil || !strings.Contains(err.Error(), "AMF_PRIMARY_AGENT") {
		t.Errorf("bad primary: %v", err)
	}
	if _, _, err := (Config{PrimaryAgent: "codex", SecondaryAgent: "gpt"}).agents(); err == nil || !strings.Contains(err.Error(), "AMF_SECONDARY_AGENT") {
		t.Errorf("bad secondary: %v", err)
	}
}

func TestAgentPlanOrder(t *testing.T) {
	root, _ := setupRepo(t)
	_, srv := newFakeGitHub(t)
	r, _ := newTestRunner(t, srv, root, fakeAgent(t, "claude", "exit 9"))
	r.Codex = fakeAgent(t, "codex", "exit 9")
	r.Opencode = fakeAgent(t, "opencode", "exit 9")
	r.OpenAIKey = "sk-openai-test"
	r.Primary, r.Secondary = agentCodex, agentClaude
	plan, err := r.agentPlan()
	if err != nil || strings.Join(plan.Agents, ",") != "codex,claudecode" || len(plan.Skipped) != 0 {
		t.Fatalf("default plan = %+v, %v", plan, err)
	}
	if d := plan.describe(); d != "codex, then claude if it fails" {
		t.Errorf("describe = %q", d)
	}
	r.OpenAIKey = ""
	if plan, _ = r.agentPlan(); strings.Join(plan.Agents, ",") != "claudecode" || len(plan.Skipped) != 1 || plan.Skipped[0].Agent != agentCodex {
		t.Errorf("unkeyed codex plan = %+v", plan)
	}
	r.AnthropicKey = ""
	if plan, _ = r.agentPlan(); strings.Join(plan.Agents, ",") != "opencode" {
		t.Errorf("neither usable: plan = %+v, want the old automatic pick", plan)
	}
	r.Agent = agentCodex
	if plan, _ = r.agentPlan(); strings.Join(plan.Agents, ",") != "codex" {
		t.Errorf("--agent plan = %+v", plan)
	}
}

// fallbackRun runs issue #1 with codex as primary and claude as secondary.
func fallbackRun(t *testing.T, codex, claude string) (*fakeGitHub, string, error, *bytes.Buffer, string) {
	t.Helper()
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	t.Setenv("GITHUB_TOKEN", "must-not-leak")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", claude))
	r.Codex = fakeAgent(t, "codex", codex)
	r.OpenAIKey = "sk-openai-test"
	r.Primary, r.Secondary = agentCodex, agentClaude
	var file bytes.Buffer
	r.Events = newEventLogger(r.Log, slog.LevelInfo, &file, slog.LevelDebug, nil, "run")
	err := r.Run(context.Background(), 1)
	return gh, file.String(), err, log, root
}

const cleanClaude = shResult + `cat >/dev/null
if [ -e partial.txt ]; then echo "worktree not clean" >&2; exit 8; fi
echo hello > feature.txt; result 'Added feature.txt with claude.'
`

func TestFallbackToSecondary(t *testing.T) {
	for _, tc := range []struct{ name, codex, detail string }{
		{"non-zero exit", "cat >/dev/null; echo half > partial.txt; echo boom >&2; exit 3", "exit status 3"},
		{"no changes", `cat >/dev/null; echo .amf/ >> "$(git rev-parse --git-common-dir)/info/exclude"; echo "Nothing to do." > "$4"`, "without making any changes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh, events, err, log, root := fallbackRun(t, tc.codex, cleanClaude)
			if err != nil {
				t.Fatalf("Run: %v\nlog:\n%s", err, log)
			}
			if len(gh.prs) != 1 || !strings.Contains(gh.prs[0]["body"], "claude made the change, after amf fell back from codex") {
				t.Fatalf("prs = %v\nlog:\n%s", gh.prs, log)
			}
			if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "claude made the change, after amf fell back from codex") {
				t.Errorf("comments = %q", c)
			}
			e := wantEvent(t, eventsByName(eventLines(t, events)), "agent.fallback", map[string]any{"from": "codex", "to": "claude", "reason": "error"})
			if e != nil && !strings.Contains(e["error"].(string), tc.detail) {
				t.Errorf("fallback error = %q, want %q", e["error"], tc.detail)
			}
			if _, err := os.Stat(filepath.Join(root, ".amf", "logs", "issue-1-claude.log")); err != nil {
				t.Errorf("secondary log: %v", err)
			}
		})
	}
}

func TestFallbackBothFail(t *testing.T) {
	gh, _, err, _, _ := fallbackRun(t, "cat >/dev/null; exit 3", "cat >/dev/null; exit 4")
	if err == nil || !strings.Contains(err.Error(), "every agent failed") || !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "claude") {
		t.Fatalf("err = %v, want both agents named", err)
	}
	if len(gh.prs) != 0 {
		t.Errorf("prs = %v", gh.prs)
	}
}

func TestPrimarySuccessNoFallback(t *testing.T) {
	gh, events, err, log, _ := fallbackRun(t, fakeDefaultCodex, "echo claude must not run >&2; exit 9")
	if err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	if body := gh.prs[0]["body"]; !strings.Contains(body, "with codex") || strings.Contains(body, "fell back") {
		t.Errorf("body = %q", body)
	}
	if strings.Contains(events, "agent.fallback") {
		t.Errorf("unexpected fallback event: %s", events)
	}
}

func TestAgentFlagBypassesFallback(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	r, _ := newTestRunner(t, srv, root, fakeAgent(t, "claude", "echo claude must not run >&2; exit 9"))
	r.Codex = fakeAgent(t, "codex", "cat >/dev/null; exit 3")
	r.OpenAIKey = "sk-openai-test"
	r.Primary, r.Secondary, r.Agent = agentCodex, agentClaude, agentCodex
	if err := r.Run(context.Background(), 1); err == nil || strings.Contains(err.Error(), "every agent failed") {
		t.Fatalf("err = %v, want codex's failure alone", err)
	}
	if len(gh.prs) != 0 {
		t.Errorf("prs = %v", gh.prs)
	}
}

func TestPipelineIgnoresPrimary(t *testing.T) {
	root, _ := setupPipelineRepo(t, "## amf\n\n| Stage | Agent |\n|---|---|\n| code | codex |\n")
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", "echo claude must not run >&2; exit 9"))
	r.Codex = fakeAgent(t, "codex", `cat >/dev/null; echo hi > feature.txt; echo done > "$4"`)
	r.Primary, r.Secondary = agentClaude, agentOpencode
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	if len(gh.prs) != 1 || strings.Contains(gh.prs[0]["body"], "fell back") {
		t.Errorf("prs = %v", gh.prs)
	}
}

func TestDoctorAgentSettings(t *testing.T) {
	root, origin := setupRepo(t)
	_, srv := newFakeGitHub(t)
	t.Chdir(root)
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)
	t.Setenv("AMF_CLONE_URL", origin)
	t.Setenv("AMF_RELEASES_URL", "http://127.0.0.1:1")
	t.Setenv("AMF_CLAUDE", fakeAgent(t, "claude", `echo "2.0 (Claude Code)"`))
	t.Setenv("AMF_CODEX", fakeAgent(t, "codex", `echo "codex-cli 9"`))
	t.Setenv("AMF_OPENCODE", filepath.Join(t.TempDir(), "no-opencode"))
	t.Setenv("OPENAI_API_KEY", "sk-openai-test")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")

	out, _ := doctorOutput(t)
	wantLines(t, out,
		`ok +primary agent +codex, the default; .*codex-cli 9; OPENAI_API_KEY is set$`,
		`ok +secondary agent +claude, the default; .*ANTHROPIC_API_KEY is set$`,
		`ok +agent order +issues without a pipeline run codex, then claude if it fails`,
	)

	t.Setenv("AMF_PRIMARY_AGENT", "claudecode")
	t.Setenv("AMF_SECONDARY_AGENT", "none")
	out, _ = doctorOutput(t)
	wantLines(t, out,
		`ok +primary agent +claude, from the environment; `,
		`ok +secondary agent +none, from the environment; amf doesn.t fall back`,
	)

	t.Setenv("AMF_SECONDARY_AGENT", "gemini")
	out, err := doctorOutput(t)
	wantLines(t, out, `FAIL +agent settings +AMF_SECONDARY_AGENT must be`)
	if err == nil {
		t.Error("doctor passed with an invalid AMF_SECONDARY_AGENT")
	}
	// Every command that runs agents refuses the bad value before any work.
	if err := run(context.Background(), []string{"run", "1"}, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "AMF_SECONDARY_AGENT must be") {
		t.Errorf("amf run with a bad setting: %v", err)
	}
}
