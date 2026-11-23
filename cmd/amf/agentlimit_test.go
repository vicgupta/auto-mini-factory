package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDetectAgentLimit(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, kind, msg string
		ok            bool
	}{
		{"Free usage limit reached. Free models work in OpenCode", "usage", "Free usage limit reached. Free models work in OpenCode", true},
		{`timestamp=2026-11-23T00:20:19.090Z level=ERROR message="stream error" error.error="AI_APICallError: Free usage limit reached. Free models work in OpenCode (https://opencode.ai); subscribe to OpenCode Go to keep going: https://opencode.ai/go"`,
			"usage", "Free usage limit reached. Free models work in OpenCode (https://opencode.ai); subscribe to OpenCode Go to keep going: https://opencode.ai/go", true},
		{`error.error="AI_RetryError: Failed after 3 attempts. Last error: Free usage limit reached. Free models work in OpenCode"`,
			"usage", "Free usage limit reached. Free models work in OpenCode", true},
		{"HTTP 429 Too Many Requests", "rate_limit", "HTTP 429 Too Many Requests", true},
		{`{"status": 429, "error": "slow down"}`, "rate_limit", `{"status": 429, "error": "slow down"}`, true},
		{"the provider hit a rate limit; try later", "rate_limit", "the provider hit a rate limit; try later", true},
		{"Quota exceeded for this API key", "quota", "Quota exceeded for this API key", true},
		{"You are out of quota", "quota", "You are out of quota", true},
		{"Error: invalid API key", "auth", "Error: invalid API key", true},
		{"401 Unauthorized", "auth", "401 Unauthorized", true},
		{"authentication failed for this request", "auth", "authentication failed for this request", true},
		{"\x1b[0mFree usage limit reached\x1b[0m", "usage", "Free usage limit reached", true},
		// A mere mention of a quota, or an HTTP 200, must not fail the run.
		{"document the disk quota in the README", "", "", false},
		{"the function computes a quota of 12", "", "", false},
		{"status 200 OK", "", "", false},
		{"issue 429 is about model origin", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		kind, msg, ok := detectAgentLimit(tc.in)
		if ok != tc.ok || kind != tc.kind || (tc.ok && msg != tc.msg) {
			t.Errorf("detectAgentLimit(%q) = %q, %q, %v, want %q, %q, %v",
				tc.in, kind, msg, ok, tc.kind, tc.msg, tc.ok)
		}
	}
}

func limitRunner(t *testing.T, script string) (*Runner, *timeoutSyncBuffer, *fakeGitHub) {
	t.Helper()
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Limit")
	r, _ := newTestRunner(t, srv, root, fakeAgent(t, "claude", script))
	var log timeoutSyncBuffer
	r.Log = &log
	r.KillGrace = 100 * time.Millisecond
	r.Timeout = 10 * time.Second
	r.IdleTimeout = 5 * time.Second
	return r, &log, gh
}

// TestAgentLimitFailsFast stops an agent that reports a usage limit and then
// hangs, and checks that the issue fails clearly with no PR.
func TestAgentLimitFailsFast(t *testing.T) {
	r, log, gh := limitRunner(t, `
echo 'Free usage limit reached. Free models work in OpenCode' >&2
echo 'more after the error' >&2
exec sleep 600
`)
	var events bytes.Buffer
	r.Events = newEventLogger(r.Log, slog.LevelInfo, &events, slog.LevelDebug, nil, "run")

	begin := time.Now()
	err := r.Run(context.Background(), 1)
	if time.Since(begin) > 3*time.Second {
		t.Errorf("the run took %s, want fail-fast", time.Since(begin))
	}
	var ale *agentLimitError
	if !errors.As(err, &ale) {
		t.Fatalf("error = %v, want an agentLimitError\nlog:\n%s", err, log.String())
	}
	if ale.Agent != "claude" || ale.Kind != "usage" || !strings.Contains(ale.Message, "Free usage limit reached") {
		t.Errorf("agentLimitError = %+v", *ale)
	}
	if !strings.Contains(err.Error(), "claude: Free usage limit reached") {
		t.Errorf("error = %v", err)
	}
	ev := eventsByName(eventLines(t, events.String()))
	wantEvent(t, ev, "agent.limit", map[string]any{"issue": float64(1), "agent": "claude", "kind": "usage", "message": nil})
	wantEvent(t, ev, "issue.failed", map[string]any{"issue": float64(1), "reason": "agent_limit", "error": nil})
	if got, want := gh.labels(1), []string{"amf:failed"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
	if len(gh.prs) != 0 {
		t.Errorf("PRs = %v", gh.prs)
	}
	if c := gh.comments[1]; len(c) != 1 {
		t.Fatalf("comments = %q", c)
	} else {
		for _, want := range []string{
			"amf stopped **claude** after it reported a usage limit",
			"Free usage limit reached",
			"opened no pull request",
			"Partial output: `.amf/logs/issue-1.log`",
			"re-add the `amf:ready` label",
		} {
			if !strings.Contains(c[0], want) {
				t.Errorf("comment missing %q:\n%s", want, c[0])
			}
		}
	}
	if _, err := os.Stat(filepath.Join(r.Root, ".amf", "worktrees", "issue-1")); !os.IsNotExist(err) {
		t.Errorf("the worktree should be removed, stat = %v", err)
	}
	// Stderr was surfaced on amf's own log.
	if !strings.Contains(log.String(), "amf: claude: Free usage limit reached") {
		t.Errorf("log missing surfaced stderr:\n%s", log.String())
	}
	if b := mustRead(t, ale.LogPath); !strings.Contains(b, "Free usage limit reached") || !strings.Contains(b, "amf: stopped claude:") {
		t.Errorf("agent log missing the error or the stop line:\n%s", b)
	}
}

func TestAgentLimitPatternsFailFast(t *testing.T) {
	cases := []struct {
		line, kind, snippet string
	}{
		{"HTTP 429 Too Many Requests", "rate_limit", "HTTP 429"},
		{"provider rate limit exceeded", "rate_limit", "rate limit"},
		{"Quota exceeded for this project", "quota", "Quota exceeded"},
		{"Error: invalid API key", "auth", "invalid API key"},
	}
	for _, tc := range cases {
		t.Run(tc.kind+"/"+tc.snippet, func(t *testing.T) {
			r, log, gh := limitRunner(t, fmt.Sprintf("echo '%s' >&2\nexec sleep 600\n", tc.line))
			begin := time.Now()
			err := r.Run(context.Background(), 1)
			if time.Since(begin) > 3*time.Second {
				t.Errorf("the run took %s", time.Since(begin))
			}
			var ale *agentLimitError
			if !errors.As(err, &ale) || ale.Kind != tc.kind || !strings.Contains(ale.Message, tc.snippet) {
				t.Fatalf("error = %v (%+v)\nlog:\n%s", err, ale, log.String())
			}
			if got, want := gh.labels(1), []string{"amf:failed"}; !reflect.DeepEqual(got, want) {
				t.Errorf("labels = %v, want %v", got, want)
			}
		})
	}
}

func TestAgentLimitQuietHidesStream(t *testing.T) {
	r, log, _ := limitRunner(t, `
echo 'Free usage limit reached' >&2
exec sleep 600
`)
	r.Quiet = true
	err := r.Run(context.Background(), 1)
	var ale *agentLimitError
	if !errors.As(err, &ale) {
		t.Fatalf("error = %v, want an agentLimitError\nlog:\n%s", err, log.String())
	}
	if strings.Contains(log.String(), "amf: claude: Free usage limit reached") {
		t.Errorf("Quiet should hide streamed stderr:\n%s", log.String())
	}
}

func TestAgentLimitDryRunDoesNotLabel(t *testing.T) {
	r, log, gh := limitRunner(t, `
echo 'Free usage limit reached' >&2
exec sleep 600
`)
	r.DryRun = true
	var events bytes.Buffer
	r.Events = newEventLogger(r.Log, slog.LevelInfo, &events, slog.LevelDebug, nil, "run")
	err := r.Run(context.Background(), 1)
	var ale *agentLimitError
	if !errors.As(err, &ale) {
		t.Fatalf("error = %v, want an agentLimitError\nlog:\n%s", err, log.String())
	}
	if got, want := gh.labels(1), []string{"amf:ready"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want unchanged", got)
	}
	if len(gh.comments[1]) != 0 || len(gh.prs) != 0 {
		t.Errorf("dry-run posted comments or PRs: comments=%v prs=%v", gh.comments[1], gh.prs)
	}
	ev := eventsByName(eventLines(t, events.String()))
	wantEvent(t, ev, "issue.failed", map[string]any{"reason": "agent_limit", "dry_run": true})
}

func TestAgentLimitFallsBack(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	r, _ := newTestRunner(t, srv, root, fakeAgent(t, "claude", `
echo 'Free usage limit reached' >&2
exec sleep 600
`))
	var log timeoutSyncBuffer
	r.Log = &log
	r.KillGrace = 100 * time.Millisecond
	r.Timeout = 10 * time.Second
	r.Primary, r.Secondary = agentClaude, agentCodex
	r.OpenAIKey = "sk-test"
	r.Codex = fakeAgent(t, "codex", `
echo added > feature.txt
# write the final message to the -o file
while [ "$1" != "-o" ]; do shift; done
echo "Added feature.txt." > "$2"
`)
	var events bytes.Buffer
	r.Events = newEventLogger(r.Log, slog.LevelInfo, &events, slog.LevelDebug, nil, "run")
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log.String())
	}
	if len(gh.prs) != 1 {
		t.Fatalf("PRs = %v, want 1", gh.prs)
	}
	ev := eventsByName(eventLines(t, events.String()))
	wantEvent(t, ev, "agent.fallback", map[string]any{"from": "claude", "to": "codex", "reason": "agent_limit"})
}

func TestAgentProgressHeartbeat(t *testing.T) {
	r, log, _ := limitRunner(t, `
# stay quiet, then hit a limit so the test does not wait for the idle timeout
sleep 0.4
echo 'Free usage limit reached' >&2
exec sleep 600
`)
	r.ProgressEvery = 80 * time.Millisecond
	r.IdleTimeout = 5 * time.Second
	err := r.Run(context.Background(), 1)
	var ale *agentLimitError
	if !errors.As(err, &ale) {
		t.Fatalf("error = %v, want an agentLimitError\nlog:\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "claude still running") || !strings.Contains(log.String(), "bytes of output") {
		t.Errorf("log missing heartbeat:\n%s", log.String())
	}
}

func TestOpencodePrintLogsSurfacesLimit(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Limit")
	script := `
case " $* " in
*" --print-logs "*" --log-level ERROR "*|*" --print-logs --log-level ERROR "*) ;;
*) echo "missing print-logs flags: $*" >&2; exec sleep 600;;
esac
echo 'timestamp=2026-11-23T00:20:19.090Z level=ERROR message="stream error" error.error="AI_APICallError: Free usage limit reached. Free models work in OpenCode"' >&2
exec sleep 600
`
	r, _ := newTestRunner(t, srv, root, fakeAgent(t, "claude", "exit 1"))
	var log timeoutSyncBuffer
	r.Log = &log
	r.Claude = ""
	r.AnthropicKey = ""
	r.Agent = agentOpencode
	r.Opencode = fakeAgent(t, "opencode", script)
	r.OpencodeModel = defaultOpencodeModel
	r.KillGrace = 100 * time.Millisecond
	r.Timeout = 10 * time.Second
	begin := time.Now()
	err := r.Run(context.Background(), 1)
	if time.Since(begin) > 3*time.Second {
		t.Errorf("the run took %s", time.Since(begin))
	}
	var ale *agentLimitError
	if !errors.As(err, &ale) || ale.Agent != "opencode" || ale.Kind != "usage" {
		t.Fatalf("error = %v (%+v)\nlog:\n%s", err, ale, log.String())
	}
	if !strings.Contains(ale.Message, "Free usage limit reached") {
		t.Errorf("message = %q", ale.Message)
	}
	if got, want := gh.labels(1), []string{"amf:failed"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
}

func TestAgentLimitIgnoredOnOrdinaryOutput(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", `
echo 'document the disk quota in the README'
echo hello > feature.txt
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"Added feature.txt."}'
`))
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	if len(gh.prs) != 1 {
		t.Fatalf("PRs = %v, want 1", gh.prs)
	}
	if _, _, ok := detectAgentLimit("document the disk quota in the README"); ok {
		t.Fatal("ordinary quota mention should not match")
	}
}

func TestExitOfAgentLimit(t *testing.T) {
	if got := exitOf(&agentLimitError{Kind: "usage"}); got != "usage" {
		t.Errorf("exitOf usage = %q", got)
	}
	if got := exitOf(&agentLimitError{Kind: "rate_limit"}); got != "rate_limit" {
		t.Errorf("exitOf rate_limit = %q", got)
	}
}
