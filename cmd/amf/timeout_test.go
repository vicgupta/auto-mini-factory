package main

// The agent timeout: how long a run may take, from --agent-timeout (and its
// deprecated --timeout alias) or AMF_AGENT_TIMEOUT. A run that hits it has
// its whole process group killed, keeps its partial output, and fails the
// issue with a clear comment and no push, PR or worktree; watch moves on.
// GitHub is the in-memory fake from amf_test.go; agents are shell scripts.

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a concurrent-safe buffer for a runner's log.
type timeoutSyncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *timeoutSyncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *timeoutSyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// sleepAgentScript is a fake agent that prints a greeting, spawns a child
// and a grandchild, records all three PIDs in pids, and ignores SIGTERM, so
// amf has to escalate to SIGKILL to end the process group.
func sleepAgentScript(pids string) string {
	return fmt.Sprintf(`echo pre-sleep out
echo pre-sleep err >&2
trap '' TERM
echo $$ > %[1]s
cat > agent-child <<'CHILD'
#!/bin/sh
echo $$ >> %[1]s
cat > agent-grand <<'GRAND'
#!/bin/sh
echo $$ >> %[1]s
trap '' TERM
while :; do sleep 1; done
GRAND
chmod u+x agent-grand
trap '' TERM
./agent-grand
while :; do sleep 1; done
CHILD
chmod u+x agent-child
./agent-child
while :; do sleep 1; done
`, pids)
}

// TestAgentTimeoutKillsTheGroup runs an agent that ignores SIGTERM and
// checks that the timeout ends the run, keeps the partial output, fails the
// issue without pushing or opening a PR, removes the worktree, and leaves no
// process behind.
func TestAgentTimeoutKillsTheGroup(t *testing.T) {
	root, origin := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Hangs")
	pids := filepath.Join(t.TempDir(), "pids")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", sleepAgentScript(pids)))
	// Long enough for the child and grandchild to start and record their
	// PIDs before the kill, even on a loaded machine.
	r.Timeout = 2 * time.Second
	r.KillGrace = 100 * time.Millisecond
	var events bytes.Buffer
	r.Events = newEventLogger(r.Log, slog.LevelInfo, &events, slog.LevelDebug, nil, "run")

	begin := time.Now()
	err := r.Run(context.Background(), 1)
	ev := eventsByName(eventLines(t, events.String()))
	wantEvent(t, ev, "agent.timeout", map[string]any{"issue": float64(1), "agent": "claude", "stage": "agent run",
		"limit": "2s", "elapsed": nil, "grace": "100ms", "pgid": nil, "bytes": nil, "log": nil})
	wantEvent(t, ev, "issue.failed", map[string]any{"issue": float64(1), "reason": "agent_timeout", "stage": "agent run", "error": nil})
	wantEvent(t, ev, "agent.exit", map[string]any{"issue": float64(1), "timed_out": true, "exit_code": float64(-1), "signal": nil, "error": nil})
	if time.Since(begin) > 10*time.Second {
		t.Errorf("the run took %s", time.Since(begin))
	}
	var ate *agentTimeoutError
	if !errors.As(err, &ate) {
		t.Fatalf("error = %v, want an agentTimeoutError\nlog:\n%s", err, log.String())
	}
	if ate.Agent != "claude" || ate.Limit != r.Timeout || ate.Partial != "" || !strings.Contains(ate.LogPath, "issue-1.log") {
		t.Errorf("agentTimeoutError = %+v", *ate)
	}
	sb := mustRead(t, ate.LogPath)
	for _, want := range []string{
		"pre-sleep out\n", "pre-sleep err\n",
		"amf: killed after ", "timeout (SIGTERM, then SIGKILL after 100ms)",
	} {
		if !strings.Contains(sb, want) {
			t.Errorf("log missing %q:\n%s", want, sb)
		}
	}

	// The issue is failed with the timeout comment; nothing was pushed and
	// no pull request was opened or updated.
	if got, want := gh.labels(1), []string{"amf:failed"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
	if len(gh.prs) != 0 {
		t.Errorf("PRs = %v", gh.prs)
	}
	if _, err := git(context.Background(), origin, "rev-parse", "--verify", "amf/issue-1"); err == nil {
		t.Error("the branch was pushed for inspection")
	}
	if c := gh.comments[1]; len(c) != 1 {
		t.Fatalf("comments = %q", c)
	}
	for _, want := range []string{
		"amf stopped **claude** after **",
		"opened no pull request",
		"Partial output: `.amf/logs/issue-1.log`",
		"set `AMF_AGENT_TIMEOUT` or `--agent-timeout`",
		"re-add the `amf:ready` label",
	} {
		if !strings.Contains(gh.comments[1][0], want) {
			t.Errorf("comment missing %q:\n%s", want, gh.comments[1][0])
		}
	}

	// The worktree is gone, the log stays.
	if _, err := os.Stat(filepath.Join(root, ".amf", "worktrees", "issue-1")); !os.IsNotExist(err) {
		t.Errorf("the worktree should be removed after a timeout, stat = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".amf", "logs", "issue-1.log")); err != nil {
		t.Errorf("the log should be kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".amf", "logs", "issue-1.partial.md")); !os.IsNotExist(err) {
		t.Errorf("a .partial.md is only for agents with a final-message file, stat = %v", err)
	}

	// Nothing of the agent's process tree outlives the run.
	if runtime.GOOS == "windows" {
		t.Skip("process groups are a unix mechanism")
	}
	var ps []int
	for _, s := range strings.Fields(mustRead(t, pids)) {
		if n, err := strconv.Atoi(s); err == nil {
			ps = append(ps, n)
		}
	}
	if len(ps) != 3 {
		t.Fatalf("pids = %v, want 3", ps)
	}
	for _, n := range ps {
		waitESRCH(t, n)
	}
}

// TestAgentTimeoutGraceful checks that an agent that exits on SIGTERM ends
// the run promptly, with whatever it printed still in the log.
func TestAgentTimeoutGraceful(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Slow")
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", `
echo started
trap 'echo bye; exit 0' TERM
sleep 600
`))
	r.Timeout = 2 * time.Second
	r.KillGrace = 100 * time.Millisecond

	begin := time.Now()
	err := r.Run(context.Background(), 1)
	var ate *agentTimeoutError
	if !errors.As(err, &ate) {
		t.Fatalf("error = %v, want an agentTimeoutError\nlog:\n%s", err, log.String())
	}
	if time.Since(begin) > 5*time.Second {
		t.Errorf("the graceful exit took %s", time.Since(begin))
	}
	if b := mustRead(t, ate.LogPath); !strings.Contains(b, "bye") {
		t.Errorf("the log should hold the agent's last words:\n%s", b)
	}
	if got, want := gh.labels(1), []string{"amf:failed"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
}

// TestAgentTimeoutKeepsPartialMessage checks that codex's final message, cut
// short by the timeout, is kept as <log>.partial.md next to the log.
func TestAgentTimeoutKeepsPartialMessage(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Slow codex")
	r, log := newTestRunner(t, srv, root, "no-claude")
	r.Agent = "codex"
	r.Codex = fakeAgent(t, "codex", `echo pre-sleep
shift 3
echo "a partial final message canary-secret-value" > "$1"
trap '' TERM
while :; do sleep 1; done`)
	// Long enough for the fake codex to write its partial message first.
	r.Timeout = 2 * time.Second
	r.KillGrace = 100 * time.Millisecond
	r.Redact = newRedactor("canary-secret-value")

	err := r.Run(context.Background(), 1)
	var ate *agentTimeoutError
	if !errors.As(err, &ate) {
		t.Fatalf("error = %v, want an agentTimeoutError\nlog:\n%s", err, log.String())
	}
	if want := filepath.Join(root, ".amf", "logs", "issue-1.partial.md"); ate.Partial != want {
		t.Errorf("Partial = %q, want %q", ate.Partial, want)
	}
	if got := mustRead(t, ate.Partial); got != "a partial final message [REDACTED:config]\n" {
		t.Errorf("partial file = %q", got)
	}
	if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "final message so far: `.amf/logs/issue-1.partial.md`") {
		t.Errorf("comments = %q", c)
	}
	if got := gh.labels(1); !reflect.DeepEqual(got, []string{"amf:failed"}) {
		t.Errorf("labels = %v", got)
	}
}

// TestPipelineStageTimeoutFailsWithoutPush checks a timeout in the plan
// stage: the issue fails, and nothing is pushed and no PR opened (in
// particular, the "push the last attempt for inspection" path isn't taken).
func TestPipelineStageTimeoutFailsWithoutPush(t *testing.T) {
	r, gh, root, origin := pipelineRun(t, claudeTable("plan", "code"), stageClaude(t,
		"trap '' TERM; while :; do sleep 1; done", "exit 9", "exit 9"))
	r.Timeout = 200 * time.Millisecond
	r.KillGrace = 50 * time.Millisecond

	err := r.Run(context.Background(), 1)
	var ate *agentTimeoutError
	if !errors.As(err, &ate) {
		t.Fatalf("error = %v, want an agentTimeoutError", err)
	}
	if ate.Stage != "plan" || ate.Agent != "claude" {
		t.Errorf("agentTimeoutError = %+v", *ate)
	}
	if c := gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "during the **plan** stage") {
		t.Errorf("comments = %q", c)
	}
	if got, want := gh.labels(1), []string{"amf:failed"}; !reflect.DeepEqual(got, want) {
		t.Errorf("labels = %v, want %v", got, want)
	}
	if len(gh.prs) != 0 {
		t.Errorf("PRs = %v", gh.prs)
	}
	if _, err := git(context.Background(), origin, "rev-parse", "--verify", "amf/issue-1"); err == nil {
		t.Error("the branch was pushed for inspection")
	}
	if _, err := os.Stat(filepath.Join(root, ".amf", "worktrees", "issue-1")); !os.IsNotExist(err) {
		t.Errorf("the worktree should be removed after a timeout, stat = %v", err)
	}
	// The local branch stays, as SPECS says; a retry resets it.
	if _, err := git(context.Background(), root, "rev-parse", "--verify", "amf/issue-1"); err != nil {
		t.Errorf("the local branch should stay after a timeout: %v", err)
	}
}

// TestWatchContinuesAfterTimeout checks that watch fails the hung issue and
// processes the next one in the same cycle.
func TestWatchContinuesAfterTimeout(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Hangs")
	gh.issues[2] = readyIssue(2, "Fast")
	// Issue #1's prompt hangs; #2's prompt succeeds.
	hangy := `p=$(cat)
case "$p" in *"# #1:"*) while :; do sleep 1; done;; *) echo x > x.txt; echo done;; esac
`
	r, _ := newTestRunner(t, srv, root, fakeAgent(t, "claude", hangy))
	buf := &timeoutSyncBuffer{}
	r.Log = buf
	r.Timeout = 2 * time.Second
	r.KillGrace = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(15*time.Second, cancel) // never leave a hung test running
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Watch(ctx, "amf:ready", 20*time.Millisecond) }()
	deadline := time.Now().Add(10 * time.Second)
	for !slices.Contains(gh.labels(1), "amf:failed") || !slices.Contains(gh.labels(2), "amf:done") {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("issues not processed; log:\n%s", buf.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(gh.prs) != 1 || gh.prs[0]["head"] != "amf/issue-2" {
		t.Errorf("PRs = %v", gh.prs)
	}
}

// --- how a run's flags, .env and the default decide the timeout ---

func TestAgentTimeoutPrecedence(t *testing.T) {
	// The resolution order: AMF_AGENT_TIMEOUT, else the default.
	for _, tc := range []struct {
		env     time.Duration
		want    time.Duration
		wantSrc string
	}{
		{0, defaultAgentTimeout, "default"},
		{90 * time.Minute, 90 * time.Minute, "AMF_AGENT_TIMEOUT"},
	} {
		if got, src := agentTimeout(tc.env); got != tc.want || src != tc.wantSrc {
			t.Errorf("agentTimeout(%v) = %v, %q, want %v, %q", tc.env, got, src, tc.want, tc.wantSrc)
		}
	}

	// The flags: --agent-timeout wins over its deprecated --timeout alias,
	// and an explicit value is validated when it's given.
	parse := func(args ...string) (time.Duration, error) {
		t.Helper()
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		at, alias := addTimeoutFlags(fs)
		if err := fs.Parse(args); err != nil {
			t.Fatalf("parse %v: %v", args, err)
		}
		return timeoutFlag(fs, at, alias)
	}
	for _, tc := range []struct {
		name    string
		args    []string
		want    time.Duration
		wantErr string // "" for no error
	}{
		{"flag beats alias", []string{"--agent-timeout", "3h", "--timeout", "2m"}, 3 * time.Hour, ""},
		{"alias alone", []string{"--timeout", "2m"}, 2 * time.Minute, ""},
		{"neither, resolved later", nil, 0, ""},
		{"zero", []string{"--agent-timeout", "0"}, 0, "--agent-timeout: agent timeout must be positive"},
		{"too small", []string{"--timeout", "500ms"}, 0, "--timeout: agent timeout must be at least 1m"},
		{"too big", []string{"--agent-timeout", "48h"}, 0, "--agent-timeout: agent timeout must be at most 24h"},
	} {
		got, err := parse(tc.args...)
		if (err == nil) != (tc.wantErr == "") || err != nil && !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("%s: timeoutFlag(%v) = %v, %v, want %v, %q", tc.name, tc.args, got, err, tc.want, tc.wantErr)
		}
		if err == nil && got != tc.want {
			t.Errorf("%s: timeoutFlag(%v) = %v, want %v", tc.name, tc.args, got, tc.want)
		}
	}

	// .env and the environment for loadConfig: env beats .env. loadConfig
	// keeps the value as written; newRunner parses it.
	dir := t.TempDir()
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_AGENT_TIMEOUT", "")
	t.Setenv("AMF_API_URL", "")
	os.WriteFile(filepath.Join(dir, ".env"), []byte("GITHUB_TOKEN=from-file\nGITHUB_REPO=vic/demo\nAMF_AGENT_TIMEOUT=90m\n"), 0o600)
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_REPO", "")
	cfg, err := loadConfig(filepath.Join(dir, ".env"))
	if err != nil || cfg.AgentTimeout != "90m" {
		t.Errorf(".env: %v, %v", cfg.AgentTimeout, err)
	}
	t.Setenv("AMF_AGENT_TIMEOUT", "2h")
	if cfg, err := loadConfig(filepath.Join(dir, ".env")); err != nil || cfg.AgentTimeout != "2h" {
		t.Errorf("env beats .env: %v, %v", cfg.AgentTimeout, err)
	}
}

func TestAgentTimeoutValidation(t *testing.T) {
	for _, tc := range []struct {
		val, src string
		want     string // "" for no error
	}{
		{val: "5x", src: "AMF_AGENT_TIMEOUT", want: `AMF_AGENT_TIMEOUT="5x": not a duration`},
		{val: "0s", src: "AMF_AGENT_TIMEOUT", want: "AMF_AGENT_TIMEOUT: agent timeout must be positive"},
		{val: "-1m", src: "AMF_AGENT_TIMEOUT", want: "AMF_AGENT_TIMEOUT: agent timeout must be positive"},
		{val: "500ms", src: "AMF_AGENT_TIMEOUT", want: "AMF_AGENT_TIMEOUT: agent timeout must be at least 1m"},
		{val: "48h", src: "AMF_AGENT_TIMEOUT", want: "AMF_AGENT_TIMEOUT: agent timeout must be at most 24h"},
		{val: "90m", src: "AMF_AGENT_TIMEOUT", want: ""},
		{val: "24h", src: "AMF_AGENT_TIMEOUT", want: ""},
	} {
		d, err := parseAgentTimeout(tc.val)
		if tc.want == "" {
			if err != nil {
				t.Errorf("parseAgentTimeout(%q) = %v, %v", tc.val, d, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("parseAgentTimeout(%q) error = %v, want %q", tc.val, err, tc.want)
		}
	}

	// A value that can't be used fails run and watch, but not the commands
	// that read .env for anything else: loadConfig still returns the rest.
	t.Chdir(t.TempDir())
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_AGENT_TIMEOUT", "5x")
	cfg, err := loadConfig(".env")
	if err != nil || cfg.Repo != "vic/demo" || cfg.Claude == "" {
		t.Errorf("loadConfig with a bad AMF_AGENT_TIMEOUT = %+v, %v", cfg, err)
	}
	for _, args := range [][]string{{"run", "1"}, {"watch"}} {
		err := run(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), `AMF_AGENT_TIMEOUT="5x": not a duration`) {
			t.Errorf("amf %q: error = %v", args, err)
		}
	}
	// A flag overrides it, so the bad value isn't in the way.
	if _, err := newRunner(context.Background(), time.Hour, 0, "", logOptions{}, io.Discard); err != nil && strings.Contains(err.Error(), "AMF_AGENT_TIMEOUT") {
		t.Errorf("newRunner with a flag: %v", err)
	}
}

// TestKillAfter checks the SIGKILL escalation: it fires once the grace
// period is over, and not at all when the agent exits first, as the group's
// id may by then belong to something else.
func TestKillAfter(t *testing.T) {
	killed := make(chan struct{}, 1)
	kill := func() { killed <- struct{}{} }

	killAfter(10*time.Millisecond, make(chan struct{}), kill)
	select {
	case <-killed:
	case <-time.After(5 * time.Second):
		t.Fatal("no kill after the grace period")
	}

	done := make(chan struct{})
	killAfter(50*time.Millisecond, done, kill)
	close(done)
	select {
	case <-killed:
		t.Fatal("killed after the agent had exited")
	case <-time.After(200 * time.Millisecond):
	}
}
