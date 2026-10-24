package main

import (
	"bytes"
	"context"
	"log/slog"
	"reflect"
	"slices"
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
