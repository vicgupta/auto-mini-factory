package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// logDatabase is the database amf creates on AMF_POSTGRES_URL's server and
// writes its logs to.
const logDatabase = "amflogs"

// logSchema creates amf's tables. amf_log holds amf's own progress lines,
// as amf run and amf watch print them; agent_log holds what each agent run
// printed, as .amf/logs/<log_file> has it.
const logSchema = `
CREATE TABLE IF NOT EXISTS amf_log (
	id bigserial PRIMARY KEY,
	logged_at timestamptz NOT NULL,
	host text NOT NULL,
	repo text NOT NULL,
	issue integer,
	message text NOT NULL
);
CREATE INDEX IF NOT EXISTS amf_log_issue ON amf_log (repo, issue, logged_at);
CREATE TABLE IF NOT EXISTS agent_log (
	id bigserial PRIMARY KEY,
	logged_at timestamptz NOT NULL,
	host text NOT NULL,
	repo text NOT NULL,
	issue integer NOT NULL,
	agent text NOT NULL,
	log_file text NOT NULL,
	line text NOT NULL
);
CREATE INDEX IF NOT EXISTS agent_log_issue ON agent_log (repo, issue, logged_at);
`

const (
	insertAmfLog   = `INSERT INTO amf_log (logged_at, host, repo, issue, message) VALUES ($1::timestamptz, $2, $3, $4::integer, $5)`
	insertAgentLog = `INSERT INTO agent_log (logged_at, host, repo, issue, agent, log_file, line) VALUES ($1::timestamptz, $2, $3, $4::integer, $5, $6, $7)`
)

// openLogDatabase connects to the amflogs database on c's server, creating
// it (when create is set) and its tables if they're missing. created reports
// whether it created the database.
func openLogDatabase(ctx context.Context, c pgConfig, create bool) (conn *pgConn, created bool, err error) {
	conn, err = pgConnect(ctx, c, logDatabase)
	if isPgCode(err, "3D000") { // invalid_catalog_name: no such database
		if !create {
			return nil, false, err
		}
		// Create it from the URL's database, or the postgres one every
		// server has.
		boot := c.Database
		if boot == "" || boot == logDatabase {
			boot = "postgres"
		}
		admin, aerr := pgConnect(ctx, c, boot)
		if aerr != nil {
			return nil, false, fmt.Errorf("database %s doesn't exist, and amf can't connect to %s to create it: %w", logDatabase, boot, aerr)
		}
		_, cerr := admin.Query(ctx, "CREATE DATABASE "+pgQuoteIdent(logDatabase))
		admin.Close()
		if cerr != nil && !isPgCode(cerr, "42P04") { // duplicate_database: another amf won
			return nil, false, fmt.Errorf("creating database %s: %w", logDatabase, cerr)
		}
		created = cerr == nil
		conn, err = pgConnect(ctx, c, logDatabase)
	}
	if err != nil {
		return nil, false, err
	}
	if create {
		if _, err := conn.Query(ctx, logSchema); err != nil && !isPgCode(err, "23505") && !isPgCode(err, "42P07") {
			// 23505 and 42P07 are two amfs creating the tables at once.
			conn.Close()
			return nil, false, fmt.Errorf("creating tables in %s: %w", logDatabase, err)
		}
	}
	return conn, created, nil
}

// logRow is one line for amf_log (agent == "") or agent_log.
type logRow struct {
	at          time.Time
	issue       int // 0 for none
	agent, file string
	line        string
}

// dbLog copies amf's log lines to PostgreSQL in the background, so a slow or
// unreachable server never holds up an issue. Lines still go to the
// terminal and .amf/logs as before; while the server can't be reached they
// queue in memory, and amf warns on the terminal and keeps retrying.
type dbLog struct {
	cfg        pgConfig
	host, repo string
	local      io.Writer // for amf's own warnings about the database

	mu      sync.Mutex
	closed  bool
	ch      chan logRow
	dropped int
	done    chan struct{}

	conn *pgConn // owned by run
}

const (
	dbLogQueue   = 10000 // lines waiting to be sent before new ones are dropped
	dbLogBatch   = 500   // lines per insert
	dbLogMaxWait = time.Second
)

// startDBLog starts copying logs to the amflogs database on c's server. It
// connects (and creates what's missing) before returning, so the first line
// says whether that worked.
func startDBLog(ctx context.Context, c pgConfig, repo string, local io.Writer) *dbLog {
	d := &dbLog{cfg: c, host: hostname(), repo: repo, local: local, ch: make(chan logRow, dbLogQueue), done: make(chan struct{})}
	conn, created, err := openLogDatabase(ctx, c, true)
	switch {
	case err != nil:
		fmt.Fprintf(local, "amf: warning: can't log to PostgreSQL at %s: %v; logging locally only, and retrying\n", c, err)
	case created:
		fmt.Fprintf(local, "amf: created database %s at %s; logging there too\n", logDatabase, c)
	default:
		fmt.Fprintf(local, "amf: logging to database %s at %s too\n", logDatabase, c)
	}
	d.conn = conn
	go d.run(err != nil)
	return d
}

// logError sends the error amf is about to exit with, which main prints
// only to the terminal.
func (d *dbLog) logError(err error) {
	if d != nil && err != nil {
		d.add(logRow{at: time.Now(), line: err.Error()})
	}
}

// add queues a line to send. It never blocks: with the queue full, the line
// only stays local.
func (d *dbLog) add(r logRow) {
	r.line = strings.ToValidUTF8(strings.ReplaceAll(r.line, "\x00", ""), "�") // text can't hold either
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	select {
	case d.ch <- r:
	default:
		d.dropped++
	}
}

// Close sends what's queued, waiting up to timeout for the server.
func (d *dbLog) Close(timeout time.Duration) {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	close(d.ch)
	d.mu.Unlock()
	select {
	case <-d.done:
	case <-time.After(timeout):
		fmt.Fprintf(d.local, "amf: warning: gave up sending logs to PostgreSQL after %s; they're still in the local logs\n", timeout)
	}
}

func (d *dbLog) run(down bool) {
	defer close(d.done)
	defer func() {
		if d.conn != nil {
			d.conn.Close()
		}
	}()
	var pending []logRow
	backoff, retryAt := time.Second, time.Time{}
	if down {
		retryAt = time.Now().Add(backoff)
	}
	tick := time.NewTicker(dbLogMaxWait)
	defer tick.Stop()
	closed := false
	for !closed || len(pending) > 0 {
		select {
		case r, ok := <-d.ch:
			if !ok {
				closed = true
				break
			}
			pending = append(pending, r)
			if len(pending) > dbLogQueue { // the server's been down a while
				d.mu.Lock()
				d.dropped += len(pending) - dbLogQueue
				d.mu.Unlock()
				pending = append(pending[:0], pending[len(pending)-dbLogQueue:]...)
			}
			if len(pending) < dbLogBatch {
				continue // wait a little for more lines to send together
			}
		case <-tick.C:
		}
		if len(pending) == 0 || !closed && time.Now().Before(retryAt) {
			continue
		}
		err := d.send(&pending)
		if err == nil {
			if down {
				fmt.Fprintf(d.local, "amf: logging to PostgreSQL at %s again\n", d.cfg)
				down, backoff = false, time.Second
			}
			d.mu.Lock()
			if d.dropped > 0 {
				fmt.Fprintf(d.local, "amf: warning: %d log %s couldn't be queued for PostgreSQL; they're only in the local logs\n", d.dropped, plural(d.dropped, "line", "lines"))
				d.dropped = 0
			}
			d.mu.Unlock()
			continue
		}
		if d.conn != nil {
			d.conn.Close()
			d.conn = nil
		}
		if closed {
			fmt.Fprintf(d.local, "amf: warning: couldn't send %d log %s to PostgreSQL: %v; they're only in the local logs\n", len(pending), plural(len(pending), "line", "lines"), err)
			return
		}
		if !down {
			fmt.Fprintf(d.local, "amf: warning: can't log to PostgreSQL at %s: %v; logging locally only, and retrying\n", d.cfg, err)
			down = true
		}
		retryAt = time.Now().Add(backoff)
		backoff = min(backoff*2, time.Minute)
	}
}

// send writes pending to the database, connecting first if need be, and
// empties it once the server has every line.
func (d *dbLog) send(pending *[]logRow) error {
	ctx, cancel := context.WithTimeout(context.Background(), d.cfg.Timeout+10*time.Second)
	defer cancel()
	if d.conn == nil {
		conn, _, err := openLogDatabase(ctx, d.cfg, true)
		if err != nil {
			return err
		}
		d.conn = conn
	}
	for len(*pending) > 0 {
		batch := (*pending)[:min(len(*pending), dbLogBatch)]
		var amf, agent [][]*string
		for _, r := range batch {
			at := r.at.UTC().Format(time.RFC3339Nano)
			var issue *string
			if r.issue > 0 {
				s := strconv.Itoa(r.issue)
				issue = &s
			}
			if r.agent == "" {
				amf = append(amf, []*string{&at, &d.host, &d.repo, issue, &r.line})
			} else {
				agent = append(agent, []*string{&at, &d.host, &d.repo, issue, &r.agent, &r.file, &r.line})
			}
		}
		if err := d.conn.Insert(ctx, pgInsert{insertAmfLog, amf}, pgInsert{insertAgentLog, agent}); err != nil {
			return err
		}
		*pending = (*pending)[len(batch):]
	}
	*pending = nil
	return nil
}

// issuePrefix finds the issue an amf line is about: "amf: #12: ...".
var issuePrefix = regexp.MustCompile(`^#(\d+)\b`)

// amfWriter returns a writer that sends each line of amf's own output to
// amf_log, without its "amf: " prefix.
func (d *dbLog) amfWriter() io.Writer {
	return &lineWriter{emit: func(line string) {
		line = strings.TrimPrefix(line, "amf: ")
		issue := 0
		if m := issuePrefix.FindStringSubmatch(line); m != nil {
			issue, _ = strconv.Atoi(m[1])
		}
		d.add(logRow{at: time.Now(), issue: issue, line: line})
	}}
}

// agentWriter returns a writer that sends each line an agent prints to
// agent_log. Flush sends a last line that doesn't end in a newline.
func (d *dbLog) agentWriter(issue int, agent, file string) *lineWriter {
	return &lineWriter{emit: func(line string) {
		d.add(logRow{at: time.Now(), issue: issue, agent: agent, file: file, line: line})
	}}
}

// lineWriter calls emit with each line written to it, without the newline.
type lineWriter struct {
	mu   sync.Mutex
	buf  []byte
	emit func(string)
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.emit(strings.TrimSuffix(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// Flush emits what's left after the last newline.
func (w *lineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.emit(string(w.buf))
		w.buf = nil
	}
}
