package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// defaultAgentProgress is how often amf prints a still-running line while an
// agent is working and --quiet is off.
const defaultAgentProgress = 30 * time.Second

// agentLimitError is a run amf stopped because the agent's output reported a
// usage, quota, rate-limit, or authentication error. Such a run pushes
// nothing and opens or updates no PR, and its worktree is removed; its
// changes are kept on a local-only WIP branch when there are any.
type agentLimitError struct {
	Agent, Stage, Kind, Message, LogPath string
	Elapsed                              time.Duration
	// Partial is where the agent's final message so far is kept, or "".
	Partial string
	// WIP is the local-only branch holding the agent's partial work, or "";
	// Base is the branch it was compared with, for how to inspect it.
	WIP, Base string
	// Before is how the agents amf tried first failed, when this one was
	// the fallback; "" otherwise.
	Before string
}

func (e *agentLimitError) Error() string {
	return fmt.Sprintf("%s: %s (log: %s)", e.Agent, e.Message, e.LogPath)
}

func (e *agentLimitError) phrase() string {
	switch e.Kind {
	case "quota":
		return "a quota limit"
	case "rate_limit":
		return "a rate limit"
	case "auth":
		return "an authentication error"
	default:
		return "a usage limit"
	}
}

// comment is what Run posts on the issue.
func (e *agentLimitError) comment(root string) string {
	when := fmt.Sprintf("after it reported %s", e.phrase())
	if e.Stage != "" {
		when = fmt.Sprintf("during the **%s** stage after it reported %s", stageName(e.Stage), e.phrase())
	}
	kept := fmt.Sprintf("Partial output: `%s`", relLog(root, e.LogPath))
	if e.Partial != "" {
		kept += fmt.Sprintf(" (its final message so far: `%s`)", relLog(root, e.Partial))
	}
	wip := ""
	if e.WIP != "" {
		wip = fmt.Sprintf(" Its partial work is on the local branch `%s`; inspect it with `git log -p %s..%s`.",
			e.WIP, e.Base, e.WIP)
	}
	before := ""
	if e.Before != "" {
		before = fmt.Sprintf("First %s; amf fell back to **%s**. ", e.Before, e.Agent)
	}
	retry := "To retry, fix the agent's credentials or wait until the limit resets, then re-add the `" + labelReady + "` label."
	return fmt.Sprintf("%samf stopped **%s** %s and opened no pull request: `%s: %s`. %s.%s %s",
		before, e.Agent, when, e.Agent, e.Message, kept, wip, retry)
}

// ansiSeq matches common terminal escapes so a progress line can be logged
// as plain text.
var ansiSeq = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07|\x1b[()].`)

func stripANSI(s string) string {
	return strings.TrimSpace(ansiSeq.ReplaceAllString(s, ""))
}

func containsAny(s string, phrases ...string) bool {
	for _, p := range phrases {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// detectAgentLimit reports whether text (one line, or a short chunk) is a
// usage, quota, rate-limit, or authentication error from a coding agent.
func detectAgentLimit(text string) (kind, message string, ok bool) {
	plain := stripANSI(text)
	if plain == "" {
		return "", "", false
	}
	lower := strings.ToLower(plain)
	switch {
	case containsAny(lower, "free usage limit", "usage limit reached", "usage limit exceeded"):
		kind = "usage"
	case containsAny(lower,
		"rate limit", "rate-limit", "ratelimit", "rate_limit",
		"too many requests",
		"http 429", "http/429",
		"status 429", "status: 429", "status code 429",
		`"status":429`, `"status": 429`,
		"429 too many requests"):
		kind = "rate_limit"
	case containsAny(lower,
		"quota exceeded", "quota limit", "quota reached", "quota exhausted",
		"exceeded quota", "exceeded the quota", "exceeded your quota",
		"out of quota"):
		kind = "quota"
	case containsAny(lower,
		"invalid api key", "invalid api_key", "api key is invalid", "api key invalid",
		"api key not set", "api key is missing", "missing api key",
		"401 unauthorized", "unauthorized: api", "unauthorized api",
		"authentication failed", "authentication required", "authentication error",
		"not authenticated", "not logged in"):
		kind = "auth"
	default:
		return "", "", false
	}
	return kind, limitMessage(plain), true
}

// limitMessage is the human-readable part of a detected agent error line.
func limitMessage(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, `error.error="`); i >= 0 {
		rest := s[i+len(`error.error="`):]
		if j := strings.LastIndex(rest, `"`); j >= 0 {
			s = rest[:j]
		} else {
			s = rest
		}
	}
	if i := strings.Index(s, "Last error: "); i >= 0 {
		s = s[i+len("Last error: "):]
	} else if i := strings.Index(s, "AI_APICallError: "); i >= 0 {
		s = s[i+len("AI_APICallError: "):]
	} else if i := strings.Index(s, "AI_RetryError: "); i >= 0 {
		s = s[i+len("AI_RetryError: "):]
	}
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[:297] + "..."
	}
	if s == "" {
		return "usage, quota, rate-limit, or authentication error"
	}
	return s
}

func (r *Runner) progressEvery() time.Duration {
	if r.ProgressEvery < 0 {
		return 0
	}
	if r.ProgressEvery > 0 {
		return r.ProgressEvery
	}
	return defaultAgentProgress
}

func (r *Runner) redactLine(s string) string {
	s = stripANSI(s)
	if r != nil && r.Redact != nil {
		s = r.Redact.Redact(s)
	}
	return s
}

// limitWatch scans an agent's stdout and stderr for usage/quota/rate-limit
// or authentication errors and cancels the run on the first match. It also
// prints redacted stderr lines to the user, unless Quiet is set.
type limitWatch struct {
	r      *Runner
	ctx    context.Context
	agent  string
	cancel context.CancelFunc

	mu        sync.Mutex
	kind, msg string
	hit       bool
	stopped   bool
}

func newLimitWatch(r *Runner, ctx context.Context, agent string, cancel context.CancelFunc) *limitWatch {
	return &limitWatch{r: r, ctx: ctx, agent: agent, cancel: cancel}
}

func (w *limitWatch) writer(stderr bool) io.Writer {
	return &lineWriter{emit: func(line string) { w.see(line, stderr) }}
}

func (w *limitWatch) see(line string, stderr bool) {
	if kind, msg, ok := detectAgentLimit(line); ok {
		w.mu.Lock()
		first := !w.hit
		if first {
			w.hit, w.kind, w.msg = true, kind, msg
		}
		w.mu.Unlock()
		if first && w.cancel != nil {
			w.cancel()
		}
	}
	if !stderr || w.r == nil || w.r.Quiet || w.r.Log == nil {
		return
	}
	text := w.r.redactLine(line)
	if text == "" {
		return
	}
	progress(w.ctx, w.r.Log, "amf: %s: %s\n", agentName(w.agent), text)
}

func (w *limitWatch) flush() {
	// lineWriter.Flush is called by the caller on the same writer; this
	// marks that no further cancel is needed after the process has exited.
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()
}

func (w *limitWatch) result() (kind, msg string, ok bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.kind, w.msg, w.hit
}

// byteCounter counts bytes written to it, safely from several goroutines.
type byteCounter struct{ n atomic.Int64 }

func (c *byteCounter) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return len(p), nil
}

func (c *byteCounter) N() int { return int(c.n.Load()) }

// startProgress prints a still-running line every interval until stop is
// called. Interval 0 or Quiet disables it. stop waits for the goroutine.
func (r *Runner) startProgress(interval time.Duration, issue int, agent string, begin time.Time, bytes *byteCounter) (stop func()) {
	if interval <= 0 || r.Quiet {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				elapsed := time.Since(begin).Round(time.Second)
				n := 0
				if bytes != nil {
					n = bytes.N()
				}
				r.event(slog.LevelInfo, "agent.progress", issue,
					fmt.Sprintf("#%d: %s still running (%s, %d bytes of output)", issue, agentName(agent), elapsed, n),
					"agent", agentName(agent), "elapsed", elapsed.String(), "bytes", n)
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

// finishLimit records a detected agent limit, saves any partial work, and
// returns the error Run should fail with.
func (r *Runner) finishLimit(ctx context.Context, agent, stage, kind, msg, wt, base, logPath, outPath string, logOut io.Writer, begin time.Time) *agentLimitError {
	elapsed := time.Since(begin).Round(10 * time.Millisecond)
	name := agentCommand[agent]
	if name == "" {
		name = agentName(agent)
	}
	fmt.Fprintf(logOut, "amf: stopped %s: %s\n", name, msg)
	r.event(slog.LevelWarn, "agent.limit", logIssue(logPath),
		fmt.Sprintf("agent.limit: agent=%s stage=%s issue=%d kind=%s", agentName(agent), stageName(stage), logIssue(logPath), kind),
		"agent", agentName(agent), "stage", stageName(stage), "kind", kind, "message", msg, "elapsed", elapsed.String(), "log", logPath)
	return &agentLimitError{
		Agent:   agentName(agent),
		Stage:   stage,
		Kind:    kind,
		Message: msg,
		LogPath: logPath,
		Elapsed: elapsed,
		Partial: keepPartial(logPath, outPath, r.Redact),
		WIP:     r.saveWIP(ctx, wt, logIssue(logPath), base, logOut),
		Base:    base,
	}
}
