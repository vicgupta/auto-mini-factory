package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// logOptions are run's and watch's --log-level and --log-file.
type logOptions struct {
	Level string // "" for AMF_LOG_LEVEL or the defaults
	File  string // "" for .amf/runs/<run_id>/events.jsonl, "off" for none
}

// The levels amf logs at without --log-level or AMF_LOG_LEVEL: the console
// shows what it always has, and the file also keeps the details.
const (
	defaultConsoleLevel = slog.LevelInfo
	defaultFileLevel    = slog.LevelDebug
)

// parseLogLevel reads a --log-level or AMF_LOG_LEVEL value, naming src in
// the error.
func parseLogLevel(v, src string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("%s: log level must be debug, info, warn or error, got %q", src, v)
}

// logLevels resolves the console's and the file's levels: --log-level, then
// AMF_LOG_LEVEL, set both; without either, each keeps its default.
func logLevels(flagLevel, envLevel string) (console, file slog.Level, err error) {
	v, src := flagLevel, "--log-level"
	if v == "" {
		v, src = envLevel, "AMF_LOG_LEVEL"
	}
	if v == "" {
		return defaultConsoleLevel, defaultFileLevel, nil
	}
	l, err := parseLogLevel(v, src)
	return l, l, err
}

// newRunID names one run or watch process: the UTC time and 6 random hex
// characters, like 20261023T222500Z-a1b2c3.
func newRunID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b)
}

// eventFile is the JSONL file a run's events go to. Writes are buffered,
// so a slow disk doesn't hold amf up; the buffer is flushed every second
// and on Close.
type eventFile struct {
	mu   sync.Mutex
	f    *os.File
	buf  *bufio.Writer
	stop chan struct{}
	done chan struct{}
}

// openEventFile creates path, and its folder, for a run's events.
func openEventFile(path string) (*eventFile, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	e := &eventFile{f: f, buf: bufio.NewWriterSize(f, 64<<10), stop: make(chan struct{}), done: make(chan struct{})}
	go e.flushEvery(time.Second)
	return e, nil
}

func (e *eventFile) flushEvery(d time.Duration) {
	defer close(e.done)
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			e.mu.Lock()
			e.buf.Flush()
			e.mu.Unlock()
		case <-e.stop:
			return
		}
	}
}

func (e *eventFile) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.buf.Write(p)
}

// Close flushes what's buffered and closes the file. It is safe on nil.
func (e *eventFile) Close() error {
	if e == nil {
		return nil
	}
	close(e.stop)
	<-e.done
	e.mu.Lock()
	defer e.mu.Unlock()
	return errors.Join(e.buf.Flush(), e.f.Close())
}

// newEventLogger returns the logger for a run: each record goes, redacted,
// to the console as amf has always printed it ("amf: <message>") and, when
// file is not nil, as one JSON object per line. Every record carries
// run_id.
func newEventLogger(console io.Writer, consoleLevel slog.Level, file io.Writer, fileLevel slog.Level, red *redactor, runID string) *slog.Logger {
	hs := []slog.Handler{&consoleHandler{w: console, level: consoleLevel}}
	if file != nil {
		hs = append(hs, slog.NewJSONHandler(file, &slog.HandlerOptions{Level: fileLevel}))
	}
	return slog.New(&redactHandler{red: red, next: fanout(hs)}).With("run_id", runID)
}

// consoleHandler prints a record's message as "amf: <message>", without
// its attributes, which only the file shows.
type consoleHandler struct {
	mu    sync.Mutex
	w     io.Writer
	level slog.Level
}

func (h *consoleHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *consoleHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, "amf: "+r.Message+"\n")
	return err
}

func (h *consoleHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *consoleHandler) WithGroup(string) slog.Handler      { return h }

// fanout sends each record to every handler that wants its level.
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, h := range f {
		if h.Enabled(ctx, r.Level) {
			errs = append(errs, h.Handle(ctx, r.Clone()))
		}
	}
	return errors.Join(errs...)
}

func (f fanout) WithAttrs(as []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(as)
	}
	return out
}

func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}

// redactHandler redacts a record's message and its string and error
// attributes before next sees them.
type redactHandler struct {
	red  *redactor
	next slog.Handler
}

func (h *redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}

func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, h.red.Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(h.attr(a))
		return true
	})
	return h.next.Handle(ctx, out)
}

func (h *redactHandler) WithAttrs(as []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(as))
	for i, a := range as {
		red[i] = h.attr(a)
	}
	return &redactHandler{red: h.red, next: h.next.WithAttrs(red)}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{red: h.red, next: h.next.WithGroup(name)}
}

func (h *redactHandler) attr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, h.red.Redact(v.String()))
	case slog.KindGroup:
		as := v.Group()
		red := make([]any, len(as))
		for i, g := range as {
			red[i] = h.attr(g)
		}
		return slog.Group(a.Key, red...)
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, h.red.Redact(x.Error()))
		case fmt.Stringer:
			return slog.String(a.Key, h.red.Redact(x.String()))
		case []string:
			red := make([]string, len(x))
			for i, s := range x {
				red[i] = h.red.Redact(s)
			}
			return slog.Any(a.Key, red)
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}

// event logs the named event about issue n (0 for none): text is the line
// the terminal shows, as amf has always printed it, and attrs are the
// event's fields, which only the event log shows. Debug events have no
// terminal line by default. Without Events (as in tests that build a Runner
// by hand), lines at info and above go to Log as plain text.
func (r *Runner) event(level slog.Level, name string, n int, text string, attrs ...any) {
	if r.Events == nil {
		if level >= slog.LevelInfo {
			fmt.Fprintf(r.Log, "amf: %s\n", text)
		}
		return
	}
	a := []any{"event", name}
	if n > 0 {
		a = append(a, "issue", n)
	}
	r.Events.Log(context.Background(), level, text, append(a, attrs...)...)
}

// statusOf is the amf status label an issue has, or "none".
func statusOf(is Issue) string {
	for _, l := range append([]string{labelReady}, statusLabels...) {
		if is.HasLabel(l) {
			return l
		}
	}
	return "none"
}

// labelNames is an issue's labels.
func labelNames(is Issue) []string {
	var out []string
	for _, l := range is.Labels {
		out = append(out, l.Name)
	}
	return out
}

// eventsKey is where a context carries the logger and issue for the calls
// made under it: git, GitHub and skill downloads log there.
type eventsKey struct{}

type ctxEvents struct {
	log   *slog.Logger
	issue int
}

// withEvents returns ctx carrying log (nil for none) and the issue the
// calls under it are for (0 for none).
func withEvents(ctx context.Context, log *slog.Logger, issue int) context.Context {
	return context.WithValue(ctx, eventsKey{}, ctxEvents{log, issue})
}

// callEvent logs a call's event to the logger ctx carries, at debug, with
// the issue it is for. Without one it does nothing.
func callEvent(ctx context.Context, name, text string, attrs ...any) {
	ce, _ := ctx.Value(eventsKey{}).(ctxEvents)
	if ce.log == nil || !ce.log.Enabled(ctx, slog.LevelDebug) {
		return
	}
	a := []any{"event", name}
	if ce.issue > 0 {
		a = append(a, "issue", ce.issue)
	}
	ce.log.Log(ctx, slog.LevelDebug, text, append(a, attrs...)...)
}

// excerptSize is how much of a payload an event keeps.
const excerptSize = 512

// excerpt is s trimmed to excerptSize bytes, on a rune boundary.
func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= excerptSize {
		return s
	}
	cut := excerptSize
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// ms is d in whole milliseconds, for duration_ms.
func ms(d time.Duration) int64 { return d.Milliseconds() }
