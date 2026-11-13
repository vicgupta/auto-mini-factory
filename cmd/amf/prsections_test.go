package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPRSectionsOffPromptAndBodyUnchanged(t *testing.T) {
	issue := Issue{Number: 7, Title: "Add a thing", Body: "Please."}
	wantPrompt := `You are working in a git worktree of the GitHub repository vic/demo, on branch amf/issue-7.
Resolve this GitHub issue:

# #7: Add a thing

Please.

Instructions:
- Make the smallest complete change that resolves the issue, following the existing code style.
- Find and run the project's tests, build and linters. Fix any failures before you finish.
- Do not create, edit or delete anything under .github/: amf refuses to push such changes.
- Do not commit, push, create branches or open pull requests. amf handles git and GitHub.
- When you are done, reply with only a pull request description in Markdown:
  a short summary of what changed and why, and how you verified it.
- End the final message with exactly one line: amf-status: complete if the
  change finishes the issue, or amf-status: partial if it is only one slice
  of it.
`
	if got := buildPrompt("vic/demo", "amf/issue-7", issue); got != wantPrompt {
		t.Errorf("off prompt changed:\n%s", got)
	}
	r := &Runner{}
	f := changeFacts{Diffstat: " a.txt | 1 +\n 1 file changed, 1 insertion(+)", Files: []string{"a.txt"}}
	wantBody := "## Changes\n\n_What amf observed in git, not the agent._\n\n```\n a.txt | 1 +\n 1 file changed, 1 insertion(+)\n```\n\n" +
		"Changed files:\n- `a.txt`\n\n## Agent summary\n\n_Written by the agent; amf has not verified it._\n\nDid it.\n\nCloses #7\n\n---\n_Opened by amf._"
	if got := r.prBody("Did it.", "", 7, f); got != wantBody {
		t.Errorf("off body changed:\n%s", got)
	}
	stage := &Stage{Name: "code", Agent: agentClaude}
	if got := codePrompt("vic/demo", "amf/issue-7", issue, stage, "", ""); strings.Contains(got, "## Testing") {
		t.Errorf("off code prompt has PR sections:\n%s", got)
	}
}

func TestPRSectionsOnBody(t *testing.T) {
	summary := "## Summary\nDid it.\n\n## Testing\nRan `go test ./...`: pass.\n\n#### Details\nAll good.\n\n" +
		"## Decisions on open questions\nChose the cautious option.\n\n## Docs updated\n- `ignored.md`\n"
	f := changeFacts{
		Diffstat: " a.txt | 1 +\n 2 files changed, 2 insertions(+)",
		Files:    []string{"a.txt", "docs/guide.md", "README.md"},
	}
	var log bytes.Buffer
	r := &Runner{PRSections: true, Log: &log}
	body := r.prBody(summary, "", 7, f)
	order := []string{"## Summary", "## Changes", "## Testing", "## Docs updated", "## Decisions on open questions"}
	last := -1
	for _, want := range order {
		i := strings.Index(body, want)
		if i < 0 || i < last {
			t.Fatalf("body missing %q in order:\n%s", want, body)
		}
		last = i
	}
	for _, want := range []string{
		"_Agent summary: written by the agent; amf has not verified it._\n\nDid it.\n",
		"Ran `go test ./...`: pass.\n\n#### Details\nAll good.",
		"- `docs/guide.md`\n- `README.md`",
		"Chose the cautious option.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	for _, bad := range []string{"## Agent summary", "ignored.md", "## README feature list"} {
		if strings.Contains(body, bad) {
			t.Errorf("body has %q:\n%s", bad, body)
		}
	}
	if log.Len() != 0 {
		t.Errorf("unexpected warning: %s", log.String())
	}
	if !strings.HasSuffix(body, "Closes #7\n\n---\n_Opened by amf._") {
		t.Errorf("body ending = %q", body)
	}
}

func TestPRSectionsMissingAndReadme(t *testing.T) {
	var log bytes.Buffer
	r := &Runner{PRSections: true, Log: &log}
	body := r.prBody("Just did it.", "", 7, changeFacts{Diffstat: " a.txt | 1 +", Files: []string{"a.txt"}})
	for _, want := range []string{
		prTestingMissing,
		prDecisionsMissing,
		"## README feature list\n\n" + prReadmeMissing,
		"## Docs updated\n\n_From git, not the agent._\n\nNone.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if want := "PR sections missing: testing, decisions, readme"; !strings.Contains(log.String(), want) {
		t.Errorf("log lacks %q: %s", want, log.String())
	}

	log.Reset()
	body = r.prBody("Did it.", "", 7, changeFacts{Diffstat: " a.txt | 1 +", Files: []string{"a.txt", "README.md", "doc/guide.rst", "notes.mdx", "plain.txt"}})
	if strings.Contains(body, "## README feature list") || strings.Contains(log.String(), "readme") {
		t.Errorf("README section or warning present:\n%s\n%s", body, log.String())
	}
	for _, want := range []string{"- `doc/guide.rst`", "- `notes.mdx`", "- `README.md`"} {
		if !strings.Contains(body, want) {
			t.Errorf("docs list lacks %q:\n%s", want, body)
		}
	}
	docs := body[strings.Index(body, "## Docs updated"):strings.Index(body, "## Decisions on open questions")]
	if strings.Contains(docs, "plain.txt") {
		t.Errorf("docs list has plain.txt:\n%s", docs)
	}
}

func TestPRSectionsPartialRewritesSections(t *testing.T) {
	summary := "## Testing\nThis fixes #7 in the safe way.\n\nFixes #7\n\n## Decisions on open questions\nCloses #7 only after checking.\n"
	r := &Runner{PRSections: true}
	body := r.prBody(summary, "", 7, changeFacts{Files: []string{"README.md"}, Part: true})
	if strings.Contains(body, "fixes #7") || strings.Contains(body, "Closes #7 only") || strings.Contains(body, "\nFixes #7\n") {
		t.Errorf("section kept a closing keyword:\n%s", body)
	}
	if !strings.Contains(body, "Part of #7") || !strings.HasSuffix(body, "Part of #7\n\n---\n_Opened by amf._") {
		t.Errorf("partial body = %q", body)
	}
}

func TestPRSectionsPromptOn(t *testing.T) {
	issue := Issue{Number: 7, Title: "Add a thing"}
	prompt := buildPrompt("vic/demo", "amf/issue-7", issue, true)
	for _, want := range []string{"## Testing", "## Decisions on open questions", "README.md", "never invent numbers"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	stage := &Stage{Name: "code", Agent: agentClaude}
	code := codePrompt("vic/demo", "amf/issue-7", issue, stage, "", "", true)
	for _, want := range []string{"## Testing", "## Decisions on open questions", "README.md"} {
		if !strings.Contains(code, want) {
			t.Errorf("code prompt lacks %q:\n%s", want, code)
		}
	}
}

func TestPRSectionsRepoFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AMF_CONFIG_DIR", dir)
	t.Setenv("AMF_API_URL", "")
	t.Setenv("AMF_PR_SECTIONS", "")
	env := writeEnv(t, "GITHUB_TOKEN=t\nGITHUB_REPO=Vic/Demo\n")
	repoFile := filepath.Join(dir, "repos", "github.com", "vic", "demo.env")
	if err := os.MkdirAll(filepath.Dir(repoFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(repoFile, []byte("AMF_PR_SECTIONS=on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(env)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.PRSections || cfg.RepoConfigFile != repoFile || cfg.seen["AMF_PR_SECTIONS"] != (configValue{"repo", "on"}) {
		t.Fatalf("config = %+v", cfg)
	}

	t.Setenv("AMF_PR_SECTIONS", "off")
	if cfg, err := loadConfig(env); err != nil || cfg.PRSections {
		t.Fatalf("environment should win: %+v, %v", cfg, err)
	}

	os.WriteFile(env, []byte("GITHUB_TOKEN=t\nGITHUB_REPO=Vic/Demo\nAMF_PR_SECTIONS=off\n"), 0o600)
	t.Setenv("AMF_PR_SECTIONS", "")
	if cfg, err := loadConfig(env); err != nil || cfg.PRSections {
		t.Fatalf(".env should win: %+v, %v", cfg, err)
	}

	os.WriteFile(repoFile, []byte("GITHUB_TOKEN=secret\n"), 0o600)
	if _, err := loadConfig(env); err == nil || !strings.Contains(err.Error(), repoFile) || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("disallowed key error = %v", err)
	}
}

func TestPRSectionsBadValue(t *testing.T) {
	for _, v := range []string{"yes", "ON", "true"} {
		if _, err := parsePRSections(v); err == nil || !strings.Contains(err.Error(), "AMF_PR_SECTIONS") {
			t.Errorf("parsePRSections(%q) error = %v", v, err)
		}
	}
	for _, tc := range []struct {
		v    string
		want bool
	}{{"", false}, {"off", false}, {"on", true}} {
		if got, err := parsePRSections(tc.v); err != nil || got != tc.want {
			t.Errorf("parsePRSections(%q) = %v, %v", tc.v, got, err)
		}
	}
}
