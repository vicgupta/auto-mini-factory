package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go: release builds have no cgo
)

// sqliteMigrations are the memory database's schema versions, in order;
// schema_migrations records which have run.
var sqliteMigrations = []string{
	// 1: the records, their links and the full-text index.
	`
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE issues (
	repo TEXT NOT NULL, number INTEGER NOT NULL, title TEXT NOT NULL, body TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT '', labels TEXT NOT NULL DEFAULT '[]', author TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT '', closed_at TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (repo, number));
CREATE TABLE pull_requests (
	repo TEXT NOT NULL, number INTEGER NOT NULL, title TEXT NOT NULL, body TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT '', merged INTEGER NOT NULL DEFAULT 0, merged_at TEXT NOT NULL DEFAULT '',
	head_branch TEXT NOT NULL DEFAULT '', issue_number INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (repo, number));
CREATE TABLE issue_prs (
	repo TEXT NOT NULL, issue_number INTEGER NOT NULL, pr_number INTEGER NOT NULL,
	PRIMARY KEY (repo, issue_number, pr_number));
CREATE TABLE runs (
	repo TEXT NOT NULL, id TEXT NOT NULL, issue_number INTEGER NOT NULL, pr_number INTEGER NOT NULL DEFAULT 0,
	command TEXT NOT NULL, started_at TEXT NOT NULL, finished_at TEXT NOT NULL DEFAULT '', outcome TEXT NOT NULL,
	agents TEXT NOT NULL DEFAULT '[]', exit_codes TEXT NOT NULL DEFAULT '[]', error TEXT NOT NULL DEFAULT '',
	log_tail TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (repo, id));
CREATE INDEX runs_issue ON runs (repo, issue_number, started_at);
CREATE VIRTUAL TABLE memory_fts USING fts5(repo UNINDEXED, kind UNINDEXED, ref UNINDEXED, title, body);
`,
}

type sqliteStore struct {
	db   *sql.DB
	key  string
	path string
}

func openSQLiteStore(path, key string) (MemoryStore, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &sqliteStore{db: db, key: key, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("memory database %s: %w", path, err)
	}
	var have string
	switch err := db.QueryRow(`SELECT value FROM meta WHERE key = 'repo'`).Scan(&have); {
	case err == sql.ErrNoRows:
		if _, err := db.Exec(`INSERT INTO meta (key, value) VALUES ('repo', ?)`, key); err != nil {
			db.Close()
			return nil, err
		}
	case err != nil:
		db.Close()
		return nil, err
	case have != key:
		db.Close()
		return nil, errRepoMismatch(path, have, key)
	}
	return s, nil
}

func (s *sqliteStore) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	var v int
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(sqliteMigrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(sqliteMigrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, i+1, time.Now().UTC().Format(time.RFC3339)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *sqliteStore) Key() string  { return s.key }
func (s *sqliteStore) Close() error { return s.db.Close() }

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	if string(b) == "null" {
		return "[]"
	}
	return string(b)
}

func (s *sqliteStore) index(tx *sql.Tx, kind, ref, title, body string) error {
	if _, err := tx.Exec(`DELETE FROM memory_fts WHERE repo = ? AND kind = ? AND ref = ?`, s.key, kind, ref); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO memory_fts (repo, kind, ref, title, body) VALUES (?, ?, ?, ?, ?)`, s.key, kind, ref, title, body)
	return err
}

func (s *sqliteStore) inTx(fn func(*sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *sqliteStore) UpsertIssue(is MemIssue) error {
	return s.inTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO issues (repo, number, title, body, state, labels, author, created_at, updated_at, closed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (repo, number) DO UPDATE SET title = excluded.title, body = excluded.body, state = excluded.state,
	labels = excluded.labels, author = excluded.author, created_at = excluded.created_at,
	updated_at = excluded.updated_at, closed_at = excluded.closed_at`,
			s.key, is.Number, is.Title, is.Body, is.State, jsonText(is.Labels), is.Author, is.CreatedAt, is.UpdatedAt, is.ClosedAt)
		if err != nil {
			return err
		}
		return s.index(tx, "issue", strconv.Itoa(is.Number), is.Title, is.Body)
	})
}

func (s *sqliteStore) UpsertPR(pr MemPR) error {
	return s.inTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO pull_requests (repo, number, title, body, state, merged, merged_at, head_branch, issue_number, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (repo, number) DO UPDATE SET title = excluded.title, body = excluded.body, state = excluded.state,
	merged = excluded.merged, merged_at = excluded.merged_at, head_branch = excluded.head_branch,
	issue_number = excluded.issue_number, updated_at = excluded.updated_at`,
			s.key, pr.Number, pr.Title, pr.Body, pr.State, pr.Merged, pr.MergedAt, pr.HeadBranch, pr.Issue, pr.UpdatedAt)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM issue_prs WHERE repo = ? AND pr_number = ?`, s.key, pr.Number); err != nil {
			return err
		}
		if pr.Issue > 0 {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO issue_prs (repo, issue_number, pr_number) VALUES (?, ?, ?)`, s.key, pr.Issue, pr.Number); err != nil {
				return err
			}
		}
		return s.index(tx, "pr", strconv.Itoa(pr.Number), pr.Title, pr.Body)
	})
}

func (s *sqliteStore) RecordRun(r MemRun) error {
	return s.inTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO runs (repo, id, issue_number, pr_number, command, started_at, finished_at, outcome, agents, exit_codes, error, log_tail, summary)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (repo, id) DO UPDATE SET issue_number = excluded.issue_number, pr_number = excluded.pr_number,
	command = excluded.command, started_at = excluded.started_at, finished_at = excluded.finished_at,
	outcome = excluded.outcome, agents = excluded.agents, exit_codes = excluded.exit_codes, error = excluded.error,
	log_tail = excluded.log_tail, summary = excluded.summary`,
			s.key, r.ID, r.Issue, r.PR, r.Command, r.StartedAt, r.FinishedAt, r.Outcome, jsonText(r.Agents), jsonText(r.ExitCodes), r.Error, r.LogTail, r.Summary)
		if err != nil {
			return err
		}
		if r.PR > 0 {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO issue_prs (repo, issue_number, pr_number) VALUES (?, ?, ?)`, s.key, r.Issue, r.PR); err != nil {
				return err
			}
		}
		return s.index(tx, "run", r.ID, runTitle(r), r.Summary+" "+r.Error)
	})
}

func (s *sqliteStore) SyncCursor() (string, error) {
	var c string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'sync_cursor'`).Scan(&c)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return c, err
}

func (s *sqliteStore) SetSyncCursor(c string) error {
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES ('sync_cursor', ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, c)
	return err
}

func (s *sqliteStore) History(n int) (IssueHistory, error) {
	var h IssueHistory
	var is MemIssue
	var labels string
	err := s.db.QueryRow(`SELECT number, title, body, state, labels, author, created_at, updated_at, closed_at FROM issues WHERE repo = ? AND number = ?`, s.key, n).
		Scan(&is.Number, &is.Title, &is.Body, &is.State, &labels, &is.Author, &is.CreatedAt, &is.UpdatedAt, &is.ClosedAt)
	switch {
	case err == nil:
		is.Repo = s.key
		json.Unmarshal([]byte(labels), &is.Labels)
		h.Issue = &is
	case err != sql.ErrNoRows:
		return h, err
	}
	rows, err := s.db.Query(`SELECT p.number, p.title, p.body, p.state, p.merged, p.merged_at, p.head_branch, p.issue_number, p.updated_at
FROM pull_requests p WHERE p.repo = ? AND (p.issue_number = ? OR p.number IN (SELECT pr_number FROM issue_prs WHERE repo = ? AND issue_number = ?))
ORDER BY p.number`, s.key, n, s.key, n)
	if err != nil {
		return h, err
	}
	for rows.Next() {
		pr := MemPR{Repo: s.key}
		if err := rows.Scan(&pr.Number, &pr.Title, &pr.Body, &pr.State, &pr.Merged, &pr.MergedAt, &pr.HeadBranch, &pr.Issue, &pr.UpdatedAt); err != nil {
			rows.Close()
			return h, err
		}
		h.PRs = append(h.PRs, pr)
	}
	rows.Close()
	rows, err = s.db.Query(`SELECT id, issue_number, pr_number, command, started_at, finished_at, outcome, agents, exit_codes, error, log_tail, summary
FROM runs WHERE repo = ? AND issue_number = ? ORDER BY started_at, id`, s.key, n)
	if err != nil {
		return h, err
	}
	defer rows.Close()
	for rows.Next() {
		r := MemRun{Repo: s.key}
		var agents, codes string
		if err := rows.Scan(&r.ID, &r.Issue, &r.PR, &r.Command, &r.StartedAt, &r.FinishedAt, &r.Outcome, &agents, &codes, &r.Error, &r.LogTail, &r.Summary); err != nil {
			return h, err
		}
		json.Unmarshal([]byte(agents), &r.Agents)
		json.Unmarshal([]byte(codes), &r.ExitCodes)
		h.Runs = append(h.Runs, r)
	}
	return h, rows.Err()
}

// Search ranks with FTS5's bm25, the title weighing twice the body, over
// any of text's words.
func (s *sqliteStore) Search(text string, limit int) ([]Hit, error) {
	toks := tokens(text)
	if len(toks) == 0 {
		return nil, nil
	}
	q := make([]string, len(toks))
	for i, t := range toks {
		q[i] = `"` + t + `"`
	}
	rows, err := s.db.Query(`SELECT kind, ref, title, -bm25(memory_fts, 2.0, 1.0) FROM memory_fts
WHERE memory_fts MATCH ? AND repo = ? ORDER BY bm25(memory_fts, 2.0, 1.0) LIMIT ?`, "{title body}: "+strings.Join(q, " OR "), s.key, limit)
	if err != nil {
		return nil, err
	}
	var hits []Hit
	for rows.Next() {
		var h Hit
		if err := rows.Scan(&h.Kind, &h.Ref, &h.Title, &h.Score); err != nil {
			rows.Close()
			return nil, err
		}
		hits = append(hits, h)
	}
	rows.Close()
	for i := range hits {
		h := &hits[i]
		h.Score = float64(int(h.Score*1000)) / 1000
		switch h.Kind {
		case "issue":
			h.Issue, _ = strconv.Atoi(h.Ref)
			var st string
			s.db.QueryRow(`SELECT state FROM issues WHERE repo = ? AND number = ?`, s.key, h.Issue).Scan(&st)
			h.What = "issue " + st
		case "pr":
			h.PR, _ = strconv.Atoi(h.Ref)
			var pr MemPR
			s.db.QueryRow(`SELECT state, merged, issue_number FROM pull_requests WHERE repo = ? AND number = ?`, s.key, h.PR).Scan(&pr.State, &pr.Merged, &pr.Issue)
			h.Issue, h.Merged, h.What = pr.Issue, pr.Merged, prWhat(pr)
		case "run":
			var r MemRun
			s.db.QueryRow(`SELECT issue_number, pr_number, outcome, error FROM runs WHERE repo = ? AND id = ?`, s.key, h.Ref).Scan(&r.Issue, &r.PR, &r.Outcome, &r.Error)
			h.Issue, h.PR, h.What = r.Issue, r.PR, runWhat(r)
		}
	}
	return hits, nil
}
