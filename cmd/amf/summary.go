package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"sync"
)

// runSummary is what a run or watch process did, from its events: written
// to summary.json after each issue and at exit, and logged as run.summary.
type runSummary struct {
	RunID  string `json:"run_id"`
	Issues struct {
		Done    int `json:"done"`
		Failed  int `json:"failed"`
		Skipped int `json:"skipped"`
	} `json:"issues"`
	GitHubCalls int `json:"github_calls"`
	// RateLimitLowest is the lowest X-RateLimit-Remaining GitHub reported,
	// or nil when it reported none.
	RateLimitLowest *int `json:"ratelimit_lowest"`
	GitCalls        int  `json:"git_calls"`
	// AgentMS is the agents' wall time in milliseconds per stage: plan,
	// code, eval, or "agent run" without a pipeline.
	AgentMS map[string]int64 `json:"agent_ms"`
	// Errors counts the warn and error events.
	Errors int    `json:"errors"`
	Log    string `json:"log"` // the event log, or "" with --log-file off
}

// statsHandler is a slog.Handler that counts the events a run logs into a
// runSummary, at every level, whatever the console and file show.
type statsHandler struct {
	mu      sync.Mutex
	sum     runSummary
	skipped map[int]bool
}

func newStatsHandler(runID, logPath string) *statsHandler {
	h := &statsHandler{skipped: map[int]bool{}}
	h.sum.RunID, h.sum.Log, h.sum.AgentMS = runID, logPath, map[string]int64{}
	return h
}

func (h *statsHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *statsHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *statsHandler) WithGroup(string) slog.Handler            { return h }

// roundSuffix is the -K of code-K and eval-K.
var roundSuffix = regexp.MustCompile(`-\d+$`)

func (h *statsHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]slog.Value{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Resolve()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Level >= slog.LevelWarn {
		h.sum.Errors++
	}
	switch attrs["event"].String() {
	case "issue.done":
		h.sum.Issues.Done++
	case "issue.failed":
		h.sum.Issues.Failed++
	case "watch.skip":
		// watch skips the same issue on every poll; count it once.
		if n := attrs["issue"]; n.Kind() == slog.KindInt64 && !h.skipped[int(n.Int64())] {
			h.skipped[int(n.Int64())] = true
			h.sum.Issues.Skipped++
		}
	case "github.call":
		h.sum.GitHubCalls++
		if left, err := strconv.Atoi(attrs["ratelimit_remaining"].String()); err == nil {
			if h.sum.RateLimitLowest == nil || left < *h.sum.RateLimitLowest {
				h.sum.RateLimitLowest = &left
			}
		}
	case "git.exec":
		h.sum.GitCalls++
	case "agent.exit":
		if d := attrs["duration_ms"]; d.Kind() == slog.KindInt64 {
			h.sum.AgentMS[roundSuffix.ReplaceAllString(attrs["stage"].String(), "")] += d.Int64()
		}
	}
	return nil
}

// snapshot is a copy of the summary so far.
func (h *statsHandler) snapshot() runSummary {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sum
	s.AgentMS = map[string]int64{}
	for k, v := range h.sum.AgentMS {
		s.AgentMS[k] = v
	}
	if h.sum.RateLimitLowest != nil {
		left := *h.sum.RateLimitLowest
		s.RateLimitLowest = &left
	}
	return s
}

// summarize writes summary.json and logs run.summary: at debug after an
// issue, and at exit (final) at info, which the terminal shows.
func (r *Runner) summarize(final bool) {
	if r.stats == nil {
		return
	}
	s := r.stats.snapshot()
	if r.summaryPath != "" {
		b, _ := json.MarshalIndent(s, "", "  ")
		if err := os.WriteFile(r.summaryPath, append(b, '\n'), 0o644); err != nil {
			r.event(slog.LevelWarn, "run.summary", 0, fmt.Sprintf("could not write %s: %v", r.summaryPath, err), "error", err)
		}
	}
	n := s.Issues.Done + s.Issues.Failed
	text := fmt.Sprintf("summary: %d %s (%d done, %d failed, %d skipped); %d GitHub %s",
		n, plural(n, "issue", "issues"), s.Issues.Done, s.Issues.Failed, s.Issues.Skipped,
		s.GitHubCalls, plural(s.GitHubCalls, "call", "calls"))
	if s.Log != "" {
		text += "; log " + shownPath(r.Root, s.Log)
	}
	level := slog.LevelDebug
	if final {
		level = slog.LevelInfo
	}
	lowest := any(nil)
	if s.RateLimitLowest != nil {
		lowest = *s.RateLimitLowest
	}
	r.event(level, "run.summary", 0, text, "final", final, "done", s.Issues.Done, "failed", s.Issues.Failed,
		"skipped", s.Issues.Skipped, "github_calls", s.GitHubCalls, "ratelimit_lowest", lowest,
		"git_calls", s.GitCalls, "agent_ms", s.AgentMS, "errors", s.Errors, "log", s.Log)
}
