package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// eventsByName groups parsed events by their "event" field.
func eventsByName(ev []map[string]any) map[string][]map[string]any {
	out := map[string][]map[string]any{}
	for _, e := range ev {
		if name, ok := e["event"].(string); ok {
			out[name] = append(out[name], e)
		}
	}
	return out
}

// wantEvent fails unless an event called name has every field in fields,
// with the given value where it isn't nil, and returns it.
func wantEvent(t *testing.T, byName map[string][]map[string]any, name string, fields map[string]any) map[string]any {
	t.Helper()
next:
	for _, e := range byName[name] {
		for k, v := range fields {
			got, ok := e[k]
			if !ok || v != nil && !reflect.DeepEqual(got, v) {
				continue next
			}
		}
		return e
	}
	t.Errorf("no %s event with %v; got %v", name, fields, byName[name])
	return nil
}

// TestWatchEvents runs one watch cycle with an issue that gets done, one
// that fails and one that is skipped, and checks the events it logs.
func TestWatchEvents(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Goes through")
	gh.issues[2] = readyIssue(2, "Fails")
	gh.issues[3] = &Issue{Number: 3, Title: "Busy", State: "open", Labels: []Label{{Name: "amf:ready"}, {Name: "amf:in-progress"}}}
	r, log := newTestRunner(t, srv, root, fakeAgent(t, "claude", shResult+`p=$(cat)
case "$p" in *"# #2:"*) echo broken >&2; exit 3;; esac
echo x > x.txt; result 'Done.'
`))
	var file bytes.Buffer
	r.Events = newEventLogger(r.Log, slog.LevelInfo, &file, slog.LevelDebug, nil, "run")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(20*time.Second, cancel) // never leave a hung test running
	done := make(chan error, 1)
	go func() { done <- r.Watch(ctx, "amf:ready", 20*time.Millisecond) }()
	for !slices.Contains(gh.labels(1), "amf:done") || !slices.Contains(gh.labels(2), "amf:failed") {
		if ctx.Err() != nil {
			t.Fatalf("issues not processed; log:\n%s", log)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	ev := eventsByName(eventLines(t, file.String()))
	wantEvent(t, ev, "watch.start", map[string]any{"repo": "vic/demo", "label": "amf:ready", "interval": "20ms",
		"timeout": "1m0s", "eval_rounds": float64(0), "agent_flag": nil, "version": nil})
	wantEvent(t, ev, "watch.agents", map[string]any{"agent": nil, "path": nil, "usable": nil})
	wantEvent(t, ev, "watch.agents", map[string]any{"picked": "claude"})
	wantEvent(t, ev, "watch.tick", map[string]any{"cycle": float64(1), "issues": float64(3)})
	wantEvent(t, ev, "watch.skip", map[string]any{"issue": float64(3), "reason": "in_progress"})
	wantEvent(t, ev, "watch.stop", map[string]any{"reason": "context canceled", "cycles": nil})
	wantEvent(t, ev, "watch.failed", map[string]any{"issue": float64(2), "error": nil})

	wantEvent(t, ev, "issue.picked", map[string]any{"issue": float64(1), "title": "Goes through", "author": nil, "labels": nil})
	wantEvent(t, ev, "issue.author_check", map[string]any{"issue": float64(1), "trusted": true})
	wantEvent(t, ev, "issue.status", map[string]any{"issue": float64(1), "from": "amf:ready", "to": "amf:in-progress"})
	wantEvent(t, ev, "issue.status", map[string]any{"issue": float64(1), "from": "amf:in-progress", "to": "amf:done"})
	wantEvent(t, ev, "issue.status", map[string]any{"issue": float64(2), "from": "amf:in-progress", "to": "amf:failed"})
	wantEvent(t, ev, "issue.pipeline", map[string]any{"issue": float64(1), "declared": false, "agents": []any{"claude"}})
	wantEvent(t, ev, "issue.worktree", map[string]any{"issue": float64(1), "path": nil, "branch": "amf/issue-1", "base": "main"})
	wantEvent(t, ev, "issue.agent", map[string]any{"issue": float64(1), "agent": "claude", "timeout": "1m0s"})
	wantEvent(t, ev, "issue.push", map[string]any{"issue": float64(1), "branch": "amf/issue-1"})
	wantEvent(t, ev, "issue.pr", map[string]any{"issue": float64(1), "action": "opened", "number": float64(101), "url": "https://github.com/vic/demo/pull/101"})
	wantEvent(t, ev, "issue.comment", map[string]any{"issue": float64(1), "bytes": nil})
	wantEvent(t, ev, "issue.comment", map[string]any{"issue": float64(2), "bytes": nil})
	wantEvent(t, ev, "issue.done", map[string]any{"issue": float64(1), "pr": float64(101)})
	wantEvent(t, ev, "issue.failed", map[string]any{"issue": float64(2), "reason": "error", "error": nil})
	if e := ev["issue.picked"]; len(e) != 2 {
		t.Errorf("issue.picked = %v, want #1 and #2 only", e)
	}
}

// TestPipelineEvents runs plan, code and eval, with eval failing round 1.
func TestPipelineEvents(t *testing.T) {
	r, _, _, _ := pipelineRun(t, claudeTable("plan", "code", "eval"), stageClaude(t,
		"result 'The plan.'",
		`case "$prompt" in *"<review>"*) echo v2 > feature.txt ;; *) echo v1 > feature.txt ;; esac
result 'Wrote feature.txt.'`,
		`if [ "$(cat feature.txt)" = v1 ]; then result 'Wrong.\n\nVERDICT: FAIL'; else result 'Fine.\n\nVERDICT: PASS'; fi`))
	var file bytes.Buffer
	r.Events = newEventLogger(r.Log, slog.LevelInfo, &file, slog.LevelDebug, nil, "run")
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	ev := eventsByName(eventLines(t, file.String()))
	wantEvent(t, ev, "issue.pipeline", map[string]any{"declared": true, "stages": []any{"plan", "code", "eval"}, "agents": nil, "models": nil})
	wantEvent(t, ev, "issue.stage", map[string]any{"stage": "plan", "agent": "claudecode", "model": nil, "timeout": "1m0s"})
	wantEvent(t, ev, "issue.plan.done", map[string]any{"bytes": nil})
	wantEvent(t, ev, "issue.code.round", map[string]any{"round": float64(1)})
	wantEvent(t, ev, "issue.code.round", map[string]any{"round": float64(2)})
	wantEvent(t, ev, "issue.eval.verdict", map[string]any{"round": float64(1), "pass": false, "parsed": true, "next_round": float64(2)})
	wantEvent(t, ev, "issue.eval.verdict", map[string]any{"round": float64(2), "pass": true, "parsed": true})

	// A blocked plan, and code without eval.
	r, _, _, _ = pipelineRun(t, claudeTable("plan", "code"), stageClaude(t, "result 'BLOCKED: which page?'", "exit 9", "exit 9"))
	file.Reset()
	r.Events = newEventLogger(r.Log, slog.LevelInfo, &file, slog.LevelDebug, nil, "run")
	r.Run(context.Background(), 1)
	wantEvent(t, eventsByName(eventLines(t, file.String())), "issue.plan.blocked", map[string]any{"question": nil})

	r, _, _, _ = pipelineRun(t, claudeTable("code"), stageClaude(t, "exit 9", "echo x > feature.txt; result 'Added.'", "exit 9"))
	file.Reset()
	r.Events = newEventLogger(r.Log, slog.LevelInfo, &file, slog.LevelDebug, nil, "run")
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	wantEvent(t, eventsByName(eventLines(t, file.String())), "issue.code.done", map[string]any{"rounds": float64(1)})
}

// callLogger returns a context whose calls log, at debug, to the returned
// buffer, for issue 7, redacting with red.
func callLogger(red *redactor) (context.Context, *bytes.Buffer) {
	var buf bytes.Buffer
	log := newEventLogger(io.Discard, slog.LevelInfo, &buf, slog.LevelDebug, red, "run")
	return withEvents(context.Background(), log, 7), &buf
}

func TestTemplatePath(t *testing.T) {
	for in, want := range map[string]string{
		"/repos/vic/demo": "/repos/{repo}",
		"/repos/vic/demo/issues/12/labels/amf%3Aready": "/repos/{repo}/issues/{n}/labels/{label}",
		"/repos/vic/demo/issues/12/labels":             "/repos/{repo}/issues/{n}/labels",
		"/repos/vic/demo/issues?state=open&page=2":     "/repos/{repo}/issues",
		"/repos/vic/demo/pulls/101":                    "/repos/{repo}/pulls/{n}",
		"/repos/vic/demo-two/issues/1":                 "/repos/vic/demo-two/issues/{n}",
		"/user":                                        "/user",
		"/repos/vic/demo/labels/amf%3Aready":           "/repos/{repo}/labels/amf%3Aready",
	} {
		if got, _ := templatePath(in, "vic/demo"); got != want {
			t.Errorf("templatePath(%q) = %q, want %q", in, got, want)
		}
	}
	if _, q := templatePath("/repos/vic/demo/issues?state=open&page=2", "vic/demo"); q.Get("page") != "2" {
		t.Errorf("query = %v", q)
	}
}

func TestGitHubCallEvents(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "One")
	g := newGitHub(Config{Token: "test-token", Repo: "vic/demo", APIURL: srv.URL})
	ctx, buf := callLogger(nil)
	if _, err := g.GetIssue(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := g.AddLabels(ctx, 1, "amf:done"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.GetIssue(ctx, 99); err == nil {
		t.Fatal("GetIssue(99) should fail")
	}
	ev := eventsByName(eventLines(t, buf.String()))
	wantEvent(t, ev, "github.call", map[string]any{"issue": float64(7), "method": "GET", "path": "/repos/{repo}/issues/{n}",
		"status": float64(200), "duration_ms": nil, "bytes_out": float64(0), "bytes_in": nil,
		"ratelimit_remaining": nil, "ratelimit_reset": nil, "request_id": nil})
	add := wantEvent(t, ev, "github.call", map[string]any{"method": "POST", "path": "/repos/{repo}/issues/{n}/labels"})
	if add != nil && add["bytes_out"].(float64) == 0 {
		t.Errorf("a POST with a body sent no bytes: %v", add)
	}
	wantEvent(t, ev, "github.call", map[string]any{"path": "/repos/{repo}/issues/{n}", "status": float64(404), "error_body": nil})
	if strings.Contains(buf.String(), "test-token") || strings.Contains(buf.String(), "Bearer") {
		t.Errorf("an event holds the token:\n%s", buf)
	}

	// An error body with a secret is redacted.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 422, map[string]string{"message": "bad ghp_abcdefghijklmnopqrstuvwxyz0123456789"})
	}))
	t.Cleanup(srv2.Close)
	ctx, buf = callLogger(newRedactor())
	newGitHub(Config{Token: "test-token", Repo: "vic/demo", APIURL: srv2.URL}).GetIssue(ctx, 1)
	e := wantEvent(t, eventsByName(eventLines(t, buf.String())), "github.call", map[string]any{"status": float64(422)})
	if e != nil && !strings.Contains(e["error_body"].(string), "[REDACTED:github_token]") {
		t.Errorf("error_body = %v", e["error_body"])
	}
}

func TestGitCallEvents(t *testing.T) {
	root, _ := setupRepo(t)
	ctx, buf := callLogger(nil)
	long := strings.Repeat("a long commit message ", 10)
	os.WriteFile(filepath.Join(root, "f.txt"), []byte("x\n"), 0o644)
	mustGit(t, root, "add", ".")
	if _, err := git(ctx, root, "commit", "-m", long); err != nil {
		t.Fatal(err)
	}
	if _, err := git(ctx, root, "rev-parse", "--verify", "no-such-ref"); err == nil {
		t.Fatal("rev-parse should fail")
	}
	if _, err := git(ctx, root, "push", "--force", "-u", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	ev := eventsByName(eventLines(t, buf.String()))
	c := wantEvent(t, ev, "git.exec", map[string]any{"issue": float64(7), "subcommand": "commit", "dir": root, "exit_code": float64(0),
		"duration_ms": nil, "stdout_bytes": nil, "stderr_bytes": nil})
	if c != nil {
		if msg := c["args"].([]any)[2].(string); len(msg) > 84 || !strings.HasPrefix(msg, "a long commit message") {
			t.Errorf("commit message in args = %q", msg)
		}
		if _, ok := c["stderr"]; ok {
			t.Errorf("a successful command has stderr: %v", c)
		}
	}
	f := wantEvent(t, ev, "git.exec", map[string]any{"subcommand": "rev-parse", "exit_code": float64(128)})
	if f != nil && !strings.Contains(f["stderr"].(string), "Needed a single revision") {
		t.Errorf("failed git.exec = %v", f)
	}
	wantEvent(t, ev, "git.push", map[string]any{"branch": "main", "forced": true, "exit_code": float64(0)})

	// Without a logger in the context, nothing is logged and nothing breaks.
	if _, err := git(context.Background(), root, "status"); err != nil {
		t.Fatal(err)
	}
}

func TestFetchSkillEvent(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/SKILL.md":
			io.WriteString(w, "# Skill\n")
		case "/moved":
			http.Redirect(w, r, srv.URL+"/SKILL.md", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	ctx, buf := callLogger(nil)
	if _, err := fetchSkill(ctx, srv.Client(), srv.URL+"/moved"); err != nil {
		t.Fatal(err)
	}
	if _, err := fetchSkill(ctx, srv.Client(), srv.URL+"/missing"); err == nil {
		t.Fatal("a 404 should fail")
	}
	sum := sha256.Sum256([]byte("# Skill\n"))
	ev := eventsByName(eventLines(t, buf.String()))
	wantEvent(t, ev, "http.fetch", map[string]any{"issue": float64(7), "url": srv.URL + "/moved", "status": float64(200), "redirects": float64(1),
		"bytes": float64(8), "sha256": hex.EncodeToString(sum[:]), "duration_ms": nil})
	wantEvent(t, ev, "http.fetch", map[string]any{"url": srv.URL + "/missing", "status": float64(404), "error": nil})
}

func TestAgentEvents(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Goes through")
	gh.issues[2] = readyIssue(2, "Fails")
	gh.issues[3] = readyIssue(3, "Codex")
	t.Setenv("GITHUB_TOKEN", "must-not-leak")
	r, _ := newTestRunner(t, srv, root, fakeAgent(t, "claude", shResult+`p=$(cat)
echo working >&2
case "$p" in *"# #2:"*) exit 3;; esac
echo x > x.txt; result 'Done.'
`))
	var file bytes.Buffer
	r.Events = newEventLogger(r.Log, slog.LevelInfo, &file, slog.LevelDebug, nil, "run")
	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(context.Background(), 2); err == nil {
		t.Fatal("#2 should fail")
	}
	r.Agent = agentCodex
	r.Codex = fakeAgent(t, "codex", `while [ "$1" != "-o" ]; do shift; done
cat >/dev/null; echo y > y.txt; echo "Codex did it." > "$2"`)
	if err := r.Run(context.Background(), 3); err != nil {
		t.Fatal(err)
	}

	ev := eventsByName(eventLines(t, file.String()))
	start := wantEvent(t, ev, "agent.start", map[string]any{"issue": float64(1), "agent": "claude", "binary": r.Claude, "model": "",
		"stage": "agent run", "dir": nil, "timeout": "1m0s", "stdin_bytes": nil, "stdin_sha256": nil})
	if start != nil {
		if args := start["args"].([]any); !slices.Contains(args, any("--dangerously-skip-permissions")) {
			t.Errorf("args = %v", args)
		}
		if added := start["env_added"].([]any); !slices.Contains(added, any("ANTHROPIC_API_KEY")) || !slices.Contains(added, any("PWD")) {
			t.Errorf("env_added = %v", added)
		}
		if removed := start["env_removed"].([]any); !slices.Contains(removed, any("GITHUB_TOKEN")) {
			t.Errorf("env_removed = %v", removed)
		}
	}
	wantEvent(t, ev, "agent.exit", map[string]any{"issue": float64(1), "exit_code": float64(0), "timed_out": false,
		"duration_ms": nil, "stdout_bytes": nil, "stderr_bytes": float64(len("working\n")), "result_bytes": float64(5), "result": "Done."})
	wantEvent(t, ev, "agent.parse", map[string]any{"issue": float64(1), "result_found": true, "json": true})
	wantEvent(t, ev, "agent.exit", map[string]any{"issue": float64(2), "exit_code": float64(3), "timed_out": false, "error": nil})
	wantEvent(t, ev, "agent.exit", map[string]any{"issue": float64(3), "agent": "codex", "exit_code": float64(0), "out_bytes": nil, "result": "Codex did it."})
	wantEvent(t, ev, "agent.parse", map[string]any{"issue": float64(3), "agent": "codex", "out_file": true})
	for _, secret := range []string{"must-not-leak", "sk-ant-test"} {
		if strings.Contains(file.String(), secret) {
			t.Errorf("an event holds %q", secret)
		}
	}
}

func TestConfigLoadEvent(t *testing.T) {
	root, _ := setupRepo(t)
	_, srv := newFakeGitHub(t)
	t.Chdir(root)
	os.WriteFile(filepath.Join(root, ".env"), []byte("GITHUB_REPO=vic/demo\nOPENAI_API_KEY=sk-proj-from-the-file-0123456789\n"), 0o600)
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("AMF_API_URL", srv.URL)
	own := filepath.Join(t.TempDir(), "events.jsonl")
	r, err := newRunner(context.Background(), defaultAgentTimeout, 2, "", logOptions{File: own}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	out := mustRead(t, own)
	e := wantEvent(t, eventsByName(eventLines(t, out)), "config.load", map[string]any{"paths": nil, "file": ".env", "keys": nil})
	if e == nil {
		return
	}
	keys := e["keys"].(map[string]any)
	for k, want := range map[string]map[string]any{
		"GITHUB_TOKEN":      {"source": "env", "value": "set"},
		"GITHUB_REPO":       {"source": ".env", "value": "vic/demo"},
		"OPENAI_API_KEY":    {"source": ".env", "value": "set"},
		"ANTHROPIC_API_KEY": {"source": "default", "value": "unset"},
		"OPENCODE_MODEL":    {"source": "default", "value": defaultOpencodeModel},
		"AMF_API_URL":       {"source": "env", "value": srv.URL},
	} {
		if !reflect.DeepEqual(keys[k], any(want)) {
			t.Errorf("keys[%s] = %v, want %v", k, keys[k], want)
		}
	}
	if strings.Contains(out, "sk-proj-from-the-file") || strings.Contains(out, "test-token") {
		t.Errorf("config.load holds a secret:\n%s", out)
	}
}

func TestPostgresEvents(t *testing.T) {
	pg, err := parsePostgresURL("postgres://amf:pw@127.0.0.1:1/x?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	var mu sync.Mutex
	log := newEventLogger(io.Discard, slog.LevelInfo, writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), slog.LevelDebug, nil, "run")
	d := startDBLog(context.Background(), pg, "vic/demo", io.Discard, nil)
	d.setEvents(log)
	d.add(logRow{at: time.Now(), line: "a line"})
	deadline := time.Now().Add(10 * time.Second)
	for {
		mu.Lock()
		got := strings.Contains(buf.String(), `"pg.retry"`)
		mu.Unlock()
		if got || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.Close(10 * time.Second)
	mu.Lock()
	defer mu.Unlock()
	ev := eventsByName(eventLines(t, buf.String()))
	wantEvent(t, ev, "pg.connect", map[string]any{"host": "127.0.0.1", "port": "1", "database": "amflogs", "error": nil, "created": false})
	wantEvent(t, ev, "pg.retry", map[string]any{"attempt": float64(1), "backoff": "1s", "error": nil})
	wantEvent(t, ev, "pg.drop", map[string]any{"rows": float64(1), "error": nil})
	if strings.Contains(buf.String(), ":pw@") {
		t.Errorf("an event holds the password:\n%s", buf.String())
	}
}

// writerFunc is a function as an io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
