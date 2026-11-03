package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// memSnapshot is a file backend's records in memory: the logfiles and
// markdown backends load it on open and answer reads from it.
type memSnapshot struct {
	Repo   string            `json:"repo"`
	Issues map[int]MemIssue  `json:"issues"`
	PRs    map[int]MemPR     `json:"prs"`
	Runs   map[string]MemRun `json:"runs"`
	Cursor string            `json:"sync_cursor,omitempty"`
}

func newSnapshot(key string) *memSnapshot {
	return &memSnapshot{Repo: key, Issues: map[int]MemIssue{}, PRs: map[int]MemPR{}, Runs: map[string]MemRun{}}
}

// fileStore is the shared half of the file backends: persist writes one
// changed record ("issue", "pr", "run" or "cursor") to disk.
type fileStore struct {
	mu      sync.Mutex
	key     string
	snap    *memSnapshot
	persist func(kind string, rec any) error
}

func (f *fileStore) Key() string { return f.key }

func (f *fileStore) UpsertIssue(is MemIssue) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	is.Repo = f.key
	f.snap.Issues[is.Number] = is
	return f.persist("issue", is)
}

func (f *fileStore) UpsertPR(pr MemPR) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	pr.Repo = f.key
	f.snap.PRs[pr.Number] = pr
	return f.persist("pr", pr)
}

func (f *fileStore) RecordRun(r MemRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.Repo = f.key
	f.snap.Runs[r.ID] = r
	return f.persist("run", r)
}

func (f *fileStore) SyncCursor() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap.Cursor, nil
}

func (f *fileStore) SetSyncCursor(c string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap.Cursor = c
	return f.persist("cursor", map[string]string{"repo": f.key, "cursor": c})
}

func (f *fileStore) History(n int) (IssueHistory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var h IssueHistory
	if is, ok := f.snap.Issues[n]; ok && is.Repo == f.key {
		h.Issue = &is
	}
	for _, pr := range f.snap.PRs {
		if pr.Repo == f.key && pr.Issue == n {
			h.PRs = append(h.PRs, pr)
		}
	}
	slices.SortFunc(h.PRs, func(a, b MemPR) int { return a.Number - b.Number })
	for _, r := range f.snap.Runs {
		if r.Repo == f.key && r.Issue == n {
			h.Runs = append(h.Runs, r)
		}
	}
	sortRuns(h.Runs)
	return h, nil
}

func (f *fileStore) Search(text string, limit int) ([]Hit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var docs []memDoc
	for _, is := range f.snap.Issues {
		if is.Repo == f.key {
			docs = append(docs, memDoc{Hit{Kind: "issue", Ref: strconv.Itoa(is.Number), Issue: is.Number, Title: is.Title, What: "issue " + is.State}, is.Title, is.Body})
		}
	}
	for _, pr := range f.snap.PRs {
		if pr.Repo == f.key {
			docs = append(docs, memDoc{Hit{Kind: "pr", Ref: strconv.Itoa(pr.Number), Issue: pr.Issue, PR: pr.Number, Title: pr.Title, What: prWhat(pr), Merged: pr.Merged}, pr.Title, pr.Body})
		}
	}
	for _, r := range f.snap.Runs {
		if r.Repo == f.key {
			docs = append(docs, memDoc{Hit{Kind: "run", Ref: r.ID, Issue: r.Issue, PR: r.PR, Title: runTitle(r), What: runWhat(r)}, runTitle(r), r.Summary + " " + r.Error})
		}
	}
	return rankTFIDF(docs, text, limit), nil
}

func (f *fileStore) Close() error { return nil }

func prWhat(pr MemPR) string {
	if pr.Merged {
		return "PR merged"
	}
	return "PR " + pr.State
}

func runTitle(r MemRun) string {
	return fmt.Sprintf("%s of #%d: %s", r.Command, r.Issue, r.Outcome)
}

func runWhat(r MemRun) string {
	if r.Error != "" {
		return r.Outcome + ": " + truncate(oneLine(r.Error), 120)
	}
	return r.Outcome
}

// --- logfiles ---

// The logfiles backend appends one JSON object per line to issues.jsonl,
// prs.jsonl, runs.jsonl and sync.jsonl. Each file starts with a header
// record naming the repo; every record carries type, repo and logged_at.
// The latest record for a key wins.
var logFiles = map[string]string{"issue": "issues.jsonl", "pr": "prs.jsonl", "run": "runs.jsonl", "cursor": "sync.jsonl"}

func openLogStore(dir, key string) (MemoryStore, error) {
	snap := newSnapshot(key)
	for kind, name := range logFiles {
		path := filepath.Join(dir, name)
		if err := loadLogFile(path, kind, key, snap); err != nil {
			return nil, err
		}
	}
	fs := &fileStore{key: key, snap: snap}
	fs.persist = func(kind string, rec any) error {
		return appendLog(filepath.Join(dir, logFiles[kind]), kind, key, rec)
	}
	return fs, nil
}

func loadLogFile(path, kind, key string, snap *memSnapshot) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for line := 1; sc.Scan(); line++ {
		b := sc.Bytes()
		if len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		var head struct{ Type, Repo string }
		if err := json.Unmarshal(b, &head); err != nil {
			continue // a torn last line from a crash: skip it
		}
		if head.Type == "header" || head.Repo != key {
			if head.Repo != key {
				return errRepoMismatch(path, head.Repo, key)
			}
			continue
		}
		switch head.Type {
		case "issue":
			var v MemIssue
			if json.Unmarshal(b, &v) == nil {
				snap.Issues[v.Number] = v
			}
		case "pr":
			var v MemPR
			if json.Unmarshal(b, &v) == nil {
				snap.PRs[v.Number] = v
			}
		case "run":
			var v MemRun
			if json.Unmarshal(b, &v) == nil {
				snap.Runs[v.ID] = v
			}
		case "cursor":
			var v struct{ Cursor string }
			if json.Unmarshal(b, &v) == nil {
				snap.Cursor = v.Cursor
			}
		}
	}
	return nil
}

func appendLog(path, kind, key string, rec any) error {
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	if errors.Is(statErr, fs.ErrNotExist) {
		h, _ := json.Marshal(map[string]any{"type": "header", "repo": key, "schema": 1, "logged_at": now})
		if _, err := f.Write(append(h, '\n')); err != nil {
			return err
		}
	}
	// Flatten rec into one object with type and logged_at first.
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	pre := fmt.Sprintf(`{"type":%q,"logged_at":%q,`, kind, now)
	line := append([]byte(pre), bytes.TrimPrefix(b, []byte("{"))...)
	_, err = f.Write(append(line, '\n'))
	return err
}

// --- markdown ---

// The markdown backend writes each record to its own file, issues/N.md,
// prs/N.md and runs/<id>.md, with front matter, and keeps index.md (for
// people) and index.json (every record, which reads use) up to date.
func openMarkdownStore(dir, key string) (MemoryStore, error) {
	snap := newSnapshot(key)
	idx := filepath.Join(dir, "index.json")
	data, err := os.ReadFile(idx)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, snap); err != nil {
			return nil, fmt.Errorf("%s: %w", idx, err)
		}
		if snap.Repo != key {
			return nil, errRepoMismatch(idx, snap.Repo, key)
		}
		if snap.Issues == nil {
			snap.Issues = map[int]MemIssue{}
		}
		if snap.PRs == nil {
			snap.PRs = map[int]MemPR{}
		}
		if snap.Runs == nil {
			snap.Runs = map[string]MemRun{}
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "index.md")); err == nil {
		if r := frontMatterValue(string(b), "repo"); r != "" && r != key {
			return nil, errRepoMismatch(filepath.Join(dir, "index.md"), r, key)
		}
	}
	fs := &fileStore{key: key, snap: snap}
	fs.persist = func(kind string, rec any) error {
		var path, md string
		switch v := rec.(type) {
		case MemIssue:
			path, md = filepath.Join(dir, "issues", fmt.Sprintf("%d.md", v.Number)), issueMarkdown(v)
		case MemPR:
			path, md = filepath.Join(dir, "prs", fmt.Sprintf("%d.md", v.Number)), prMarkdown(v, snap.Issues[v.Issue].Title)
		case MemRun:
			path, md = filepath.Join(dir, "runs", safeFileName(v.ID)+".md"), runMarkdown(v)
		}
		if path != "" {
			if err := writeMemFile(path, []byte(md)); err != nil {
				return err
			}
		}
		j, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			return err
		}
		if err := writeMemFile(idx, j); err != nil {
			return err
		}
		return writeMemFile(filepath.Join(dir, "index.md"), []byte(indexMarkdown(snap)))
	}
	return fs, nil
}

func safeFileName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == ':' || r < ' ' {
			return '_'
		}
		return r
	}, s)
}

// writeMemFile writes a memory file atomically, creating its directory.
func writeMemFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0o644)
}

func frontMatterValue(md, key string) string {
	rest, ok := strings.CutPrefix(md, "---\n")
	if !ok {
		return ""
	}
	fm, _, _ := strings.Cut(rest, "\n---")
	for _, l := range strings.Split(fm, "\n") {
		if v, ok := strings.CutPrefix(l, key+":"); ok {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}

func yamlList(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = strconv.Quote(x)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func frontMatter(kv ...any) string {
	var b strings.Builder
	b.WriteString("---\n")
	for i := 0; i+1 < len(kv); i += 2 {
		switch v := kv[i+1].(type) {
		case string:
			fmt.Fprintf(&b, "%s: %s\n", kv[i], strconv.Quote(v))
		case []string:
			fmt.Fprintf(&b, "%s: %s\n", kv[i], yamlList(v))
		default:
			fmt.Fprintf(&b, "%s: %v\n", kv[i], v)
		}
	}
	b.WriteString("---\n\n")
	return b.String()
}

func issueMarkdown(is MemIssue) string {
	return frontMatter("type", "issue", "repo", is.Repo, "number", is.Number, "state", is.State, "labels", is.Labels,
		"author", is.Author, "created_at", is.CreatedAt, "updated_at", is.UpdatedAt, "closed_at", is.ClosedAt) +
		fmt.Sprintf("# #%d: %s\n\n%s\n", is.Number, is.Title, strings.TrimSpace(is.Body))
}

func prMarkdown(pr MemPR, issueTitle string) string {
	link := ""
	if pr.Issue > 0 {
		link = fmt.Sprintf("Linked issue: [#%d](../issues/%d.md) %s\n\n", pr.Issue, pr.Issue, issueTitle)
	}
	return frontMatter("type", "pr", "repo", pr.Repo, "number", pr.Number, "state", pr.State, "merged", pr.Merged,
		"merged_at", pr.MergedAt, "head_branch", pr.HeadBranch, "issue", pr.Issue, "updated_at", pr.UpdatedAt) +
		fmt.Sprintf("# PR #%d: %s\n\n%s%s\n", pr.Number, pr.Title, link, strings.TrimSpace(pr.Body))
}

func runMarkdown(r MemRun) string {
	codes := make([]string, len(r.ExitCodes))
	for i, c := range r.ExitCodes {
		codes[i] = strconv.Itoa(c)
	}
	s := frontMatter("type", "run", "repo", r.Repo, "id", r.ID, "issue", r.Issue, "pr", r.PR, "command", r.Command,
		"outcome", r.Outcome, "agents", r.Agents, "exit_codes", codes, "started_at", r.StartedAt, "finished_at", r.FinishedAt) +
		fmt.Sprintf("# Run %s: #%d %s\n\nIssue: [#%d](../issues/%d.md)\n", r.ID, r.Issue, r.Outcome, r.Issue, r.Issue)
	if r.PR > 0 {
		s += fmt.Sprintf("PR: [#%d](../prs/%d.md)\n", r.PR, r.PR)
	}
	if r.Summary != "" {
		s += "\n## Summary\n\n" + r.Summary + "\n"
	}
	if r.Error != "" {
		s += "\n## Error\n\n```\n" + r.Error + "\n```\n"
	}
	if r.LogTail != "" {
		s += "\n## Log tail\n\n```\n" + r.LogTail + "\n```\n"
	}
	return s
}

func indexMarkdown(s *memSnapshot) string {
	var b strings.Builder
	b.WriteString(frontMatter("repo", s.Repo, "schema", 1, "issues", len(s.Issues), "prs", len(s.PRs), "runs", len(s.Runs), "sync_cursor", s.Cursor))
	fmt.Fprintf(&b, "# amf memory: %s\n\n## Issues\n\n", s.Repo)
	for _, n := range sortedKeys(s.Issues) {
		is := s.Issues[n]
		fmt.Fprintf(&b, "- [#%d](issues/%d.md) %s (%s)\n", n, n, is.Title, is.State)
	}
	b.WriteString("\n## Pull requests\n\n")
	for _, n := range sortedKeys(s.PRs) {
		pr := s.PRs[n]
		fmt.Fprintf(&b, "- [#%d](prs/%d.md) %s (%s", n, n, pr.Title, prWhat(pr))
		if pr.Issue > 0 {
			fmt.Fprintf(&b, ", issue #%d", pr.Issue)
		}
		b.WriteString(")\n")
	}
	b.WriteString("\n## Runs\n\n")
	runs := make([]MemRun, 0, len(s.Runs))
	for _, r := range s.Runs {
		runs = append(runs, r)
	}
	sortRuns(runs)
	for _, r := range runs {
		fmt.Fprintf(&b, "- [%s](runs/%s.md) %s of #%d: %s\n", r.ID, safeFileName(r.ID), r.Command, r.Issue, r.Outcome)
	}
	return b.String()
}

func sortedKeys[V any](m map[int]V) []int {
	ks := make([]int, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}
