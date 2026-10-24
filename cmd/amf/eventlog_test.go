package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// eventLines parses JSONL, failing on a line that isn't a JSON object.
func eventLines(t *testing.T, jsonl string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(jsonl), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("not JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestEventLogger(t *testing.T) {
	var console, file bytes.Buffer
	red := newRedactor("canary-secret-value")
	r := &Runner{Log: &console, Events: newEventLogger(&console, slog.LevelInfo, &file, slog.LevelDebug, red, "run-1")}
	r.event(slog.LevelInfo, "test.one", 3, "#3: hello canary-secret-value")
	r.event(slog.LevelInfo, "test.two", 0, "no issue here")
	r.Events.Debug("only in the file", "token", "canary-secret-value", "err", io.EOF, "list", []string{"canary-secret-value"})

	if got, want := console.String(), "amf: #3: hello [REDACTED:config]\namf: no issue here\n"; got != want {
		t.Errorf("console = %q, want %q", got, want)
	}
	if strings.Contains(file.String(), "canary-secret-value") {
		t.Errorf("the file leaks the secret:\n%s", file.String())
	}
	ev := eventLines(t, file.String())
	if len(ev) != 3 {
		t.Fatalf("events = %v", ev)
	}
	for _, e := range ev {
		for _, k := range []string{"time", "level", "msg", "run_id"} {
			if e[k] == nil || e[k] == "" {
				t.Errorf("event %v has no %s", e, k)
			}
		}
	}
	if ev[0]["issue"] != float64(3) || ev[0]["msg"] != "#3: hello [REDACTED:config]" || ev[0]["level"] != "INFO" || ev[0]["event"] != "test.one" {
		t.Errorf("first event = %v", ev[0])
	}
	if _, ok := ev[1]["issue"]; ok {
		t.Errorf("an event without #N has an issue: %v", ev[1])
	}
	if ev[2]["level"] != "DEBUG" || ev[2]["token"] != "[REDACTED:config]" || ev[2]["err"] != "EOF" {
		t.Errorf("debug event = %v", ev[2])
	}

	// warn hides info on both.
	console.Reset()
	file.Reset()
	r.Events = newEventLogger(&console, slog.LevelWarn, &file, slog.LevelWarn, red, "run-2")
	r.event(slog.LevelInfo, "test.quiet", 3, "#3: quiet")
	r.event(slog.LevelWarn, "test.loud", 0, "loud")
	if console.String() != "amf: loud\n" || len(eventLines(t, file.String())) != 1 {
		t.Errorf("at warn: console %q, file %q", console.String(), file.String())
	}
}

func TestLogLevels(t *testing.T) {
	for _, tc := range []struct {
		flag, env     string
		console, file slog.Level
		err           string
	}{
		{"", "", slog.LevelInfo, slog.LevelDebug, ""},
		{"warn", "debug", slog.LevelWarn, slog.LevelWarn, ""},
		{"", "ERROR", slog.LevelError, slog.LevelError, ""},
		{"loud", "", 0, 0, `--log-level: log level must be debug, info, warn or error, got "loud"`},
		{"", "loud", 0, 0, `AMF_LOG_LEVEL: log level must be debug, info, warn or error, got "loud"`},
	} {
		c, f, err := logLevels(tc.flag, tc.env)
		if tc.err != "" {
			if err == nil || err.Error() != tc.err {
				t.Errorf("logLevels(%q, %q) error = %v, want %q", tc.flag, tc.env, err, tc.err)
			}
			continue
		}
		if err != nil || c != tc.console || f != tc.file {
			t.Errorf("logLevels(%q, %q) = %v, %v, %v", tc.flag, tc.env, c, f, err)
		}
	}
}

func TestNewRunnerEventLog(t *testing.T) {
	root, _ := setupRepo(t)
	_, srv := newFakeGitHub(t)
	t.Chdir(root)
	t.Setenv("GITHUB_TOKEN", "test-token")
	t.Setenv("GITHUB_REPO", "vic/demo")
	t.Setenv("AMF_API_URL", srv.URL)
	ctx := context.Background()

	// The default: .amf/runs/<run_id>/events.jsonl, announced once.
	var log bytes.Buffer
	r, err := newRunner(ctx, defaultAgentTimeout, 2, "", logOptions{}, &log)
	if err != nil {
		t.Fatalf("newRunner: %v\n%s", err, log.String())
	}
	r.event(slog.LevelInfo, "test.hello", 3, "#3: hello test-token")
	r.Close()
	files, _ := filepath.Glob(filepath.Join(r.Root, ".amf", "runs", "*", "events.jsonl"))
	if len(files) != 1 {
		t.Fatalf("event files = %v", files)
	}
	rel, _ := filepath.Rel(r.Root, files[0])
	if !strings.Contains(log.String(), "amf: logging to "+rel+"\n") {
		t.Errorf("console = %q, want it to name %s", log.String(), rel)
	}
	ev := eventLines(t, mustRead(t, files[0]))
	byName := eventsByName(ev)
	if e := byName["log.file"]; len(e) != 1 || e[0]["path"] != files[0] {
		t.Errorf("log.file = %v", e)
	}
	if e := byName["checkout.resolve"]; len(e) != 1 || e[0]["how"] != "checkout" || e[0]["dir"] != r.Root {
		t.Errorf("checkout.resolve = %v", e)
	}
	last := wantEvent(t, byName, "test.hello", map[string]any{})
	if last == nil {
		return
	}
	if last["msg"] != "#3: hello [REDACTED:config]" || last["issue"] != float64(3) || !strings.Contains(files[0], last["run_id"].(string)) {
		t.Errorf("last event = %v (file %s)", last, files[0])
	}

	// A path of one's own.
	own := filepath.Join(t.TempDir(), "sub", "own.jsonl")
	log.Reset()
	r, err = newRunner(ctx, defaultAgentTimeout, 2, "", logOptions{File: own}, &log)
	if err != nil {
		t.Fatal(err)
	}
	r.event(slog.LevelInfo, "test.own", 0, "into my own file")
	r.Close()
	if !strings.Contains(log.String(), "amf: logging to "+own+"\n") || !strings.Contains(mustRead(t, own), "into my own file") {
		t.Errorf("console %q, file %q", log.String(), mustRead(t, own))
	}

	// off: no file, no announcement.
	os.RemoveAll(filepath.Join(r.Root, ".amf", "runs"))
	log.Reset()
	r, err = newRunner(ctx, defaultAgentTimeout, 2, "", logOptions{File: "off"}, &log)
	if err != nil {
		t.Fatal(err)
	}
	r.event(slog.LevelInfo, "test.console", 0, "only on the console")
	r.Close()
	if _, err := os.Stat(filepath.Join(r.Root, ".amf", "runs")); !os.IsNotExist(err) {
		t.Errorf("--log-file off created .amf/runs: %v", err)
	}
	if strings.Contains(log.String(), "logging to") || !strings.Contains(log.String(), "amf: only on the console\n") {
		t.Errorf("console = %q", log.String())
	}

	// AMF_LOG_LEVEL applies to both; a bad one fails naming itself.
	t.Setenv("AMF_LOG_LEVEL", "warn")
	log.Reset()
	r, err = newRunner(ctx, defaultAgentTimeout, 2, "", logOptions{File: own}, &log)
	if err != nil {
		t.Fatal(err)
	}
	r.event(slog.LevelInfo, "test.hidden", 0, "hidden at warn")
	r.Close()
	if strings.Contains(log.String(), "hidden at warn") || strings.Contains(mustRead(t, own), "hidden at warn") {
		t.Errorf("an info line got through at warn: console %q", log.String())
	}
	t.Setenv("AMF_LOG_LEVEL", "loud")
	if _, err := newRunner(ctx, defaultAgentTimeout, 2, "", logOptions{}, io.Discard); err == nil || !strings.Contains(err.Error(), "AMF_LOG_LEVEL: log level must be") {
		t.Errorf("bad AMF_LOG_LEVEL: %v", err)
	}
}
