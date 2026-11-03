package main

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// amf's memory is a per-repo record of the issues, pull requests and runs
// amf has seen, so a new issue can be compared with past work. It lives
// under <memory dir>/amf-memory/<host>/<owner>/<repo>/, in one of three
// backends chosen with --memory-backend: sqlite (the default), markdown or
// logfiles. See docs/SPECS.md#memory.

// memoryDirName is the directory amf keeps every repo's memory in, under
// the memory dir. amf's own commits never stage it.
const memoryDirName = "amf-memory"

// memoryExclude is the pathspec that keeps memory files out of amf's commits.
const memoryExclude = ":(exclude)" + memoryDirName

// The memory backends.
const (
	backendSQLite   = "sqlite"
	backendMarkdown = "markdown"
	backendLogfiles = "logfiles"
)

var memoryBackends = []string{backendSQLite, backendMarkdown, backendLogfiles}

// MemIssue is an issue as memory has it.
type MemIssue struct {
	Repo      string   `json:"repo"`
	Number    int      `json:"number"`
	Title     string   `json:"title"`
	Body      string   `json:"body,omitempty"`
	State     string   `json:"state"`
	Labels    []string `json:"labels,omitempty"`
	Author    string   `json:"author,omitempty"`
	CreatedAt string   `json:"created_at,omitempty"`
	UpdatedAt string   `json:"updated_at,omitempty"`
	ClosedAt  string   `json:"closed_at,omitempty"`
}

// MemPR is a pull request as memory has it. Issue is the issue it is
// linked to, or 0.
type MemPR struct {
	Repo       string `json:"repo"`
	Number     int    `json:"number"`
	Title      string `json:"title"`
	Body       string `json:"body,omitempty"`
	State      string `json:"state"`
	Merged     bool   `json:"merged"`
	MergedAt   string `json:"merged_at,omitempty"`
	HeadBranch string `json:"head_branch,omitempty"`
	Issue      int    `json:"issue,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
}

// MemRun is one amf run, watch or auto attempt at an issue.
type MemRun struct {
	Repo       string   `json:"repo"`
	ID         string   `json:"id"`
	Issue      int      `json:"issue"`
	Command    string   `json:"command"`
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at,omitempty"`
	Outcome    string   `json:"outcome"` // success, failed or skipped
	Agents     []string `json:"agents,omitempty"`
	ExitCodes  []int    `json:"exit_codes,omitempty"`
	PR         int      `json:"pr,omitempty"`
	Error      string   `json:"error,omitempty"`
	LogTail    string   `json:"log_tail,omitempty"`
	Summary    string   `json:"summary,omitempty"`
}

// IssueHistory is everything memory has about one issue.
type IssueHistory struct {
	Issue *MemIssue `json:"issue"`
	PRs   []MemPR   `json:"pull_requests"`
	Runs  []MemRun  `json:"runs"`
}

// Hit is one similar-search result. Kind is issue, pr or run; Ref is the
// issue or PR number, or the run's ID.
type Hit struct {
	Kind   string  `json:"kind"`
	Ref    string  `json:"ref"`
	Issue  int     `json:"issue,omitempty"`
	PR     int     `json:"pr,omitempty"`
	Title  string  `json:"title"`
	Score  float64 `json:"score"`
	What   string  `json:"what,omitempty"`
	Merged bool    `json:"merged,omitempty"`
}

// MemoryStore is a memory backend. Every store belongs to one repo, its
// Key; it refuses to open on another repo's data, and reads only its own.
type MemoryStore interface {
	Key() string
	UpsertIssue(MemIssue) error
	UpsertPR(MemPR) error
	RecordRun(MemRun) error
	History(issue int) (IssueHistory, error)
	// Search ranks the stored issues, PRs and runs by similarity to text,
	// best first, at most limit.
	Search(text string, limit int) ([]Hit, error)
	SyncCursor() (string, error)
	SetSyncCursor(string) error
	Close() error
}

// memoryOptions are the memory flags: --memory-backend and --memory-dir.
type memoryOptions struct {
	Backend string
	Dir     string
}

// addMemoryFlags registers --memory-backend and --memory-dir.
func addMemoryFlags(fs interface {
	StringVar(*string, string, string, string)
}) *memoryOptions {
	mo := &memoryOptions{}
	fs.StringVar(&mo.Backend, "memory-backend", backendSQLite, "where amf keeps the repo's memory: sqlite, markdown or logfiles")
	fs.StringVar(&mo.Dir, "memory-dir", "", "directory the amf-memory folder goes in (default: the current directory)")
	return mo
}

// check rejects an unknown backend.
func (mo memoryOptions) check() error {
	if !slices.Contains(memoryBackends, mo.Backend) {
		return fmt.Errorf("--memory-backend %q: must be one of sqlite, markdown or logfiles", mo.Backend)
	}
	return nil
}

// baseDir is --memory-dir made absolute, or the current directory.
func (mo memoryOptions) baseDir() (string, error) {
	d := mo.Dir
	if d == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		d = wd
	}
	return filepath.Abs(d)
}

// repoKey is the repo's memory key, host/owner/repo lower-cased, from the
// GitHub API URL and owner/name. Without a repo it is local/<hash of root>.
func repoKey(apiURL, repo, root string) string {
	repo = strings.ToLower(strings.TrimSpace(repo))
	if repo == "" {
		abs, _ := filepath.Abs(root)
		return "local/" + shortHash(abs)
	}
	host := "github.com"
	if u, err := url.Parse(apiURL); err == nil && u.Host != "" {
		host = strings.ToLower(u.Host)
	}
	host = strings.TrimPrefix(host, "api.")
	return host + "/" + repo
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:8])
}

var safeSegment = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// keyPath is where key's store lives under base: base/amf-memory/<key>, or
// base/amf-memory/_hash/<hash> when a part of key isn't a plain name.
func keyPath(base, key string) string {
	parts := strings.Split(key, "/")
	ok := len(parts) == 3 || (len(parts) == 2 && parts[0] == "local")
	for _, p := range parts {
		if !safeSegment.MatchString(p) || strings.Contains(p, "..") {
			ok = false
		}
	}
	if !ok {
		return filepath.Join(base, memoryDirName, "_hash", shortHash(key))
	}
	return filepath.Join(append([]string{base, memoryDirName}, parts...)...)
}

// openMemory opens the store mo selects for the repo key under mo's dir,
// creating it if needed.
func openMemory(mo memoryOptions, key string) (MemoryStore, error) {
	if err := mo.check(); err != nil {
		return nil, err
	}
	base, err := mo.baseDir()
	if err != nil {
		return nil, err
	}
	dir := keyPath(base, key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	switch mo.Backend {
	case backendMarkdown:
		return openMarkdownStore(dir, key)
	case backendLogfiles:
		return openLogStore(dir, key)
	}
	return openSQLiteStore(filepath.Join(dir, "memory.db"), key)
}

// errRepoMismatch is a store that belongs to another repo.
func errRepoMismatch(path, have, want string) error {
	return fmt.Errorf("memory at %s belongs to %s, not %s; refusing to mix repos", path, have, want)
}

// closesRef finds "Closes #N", "Fixes #N" or "Resolves #N" in a PR body.
var closesRef = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s+#(\d+)\b`)

// linkedIssue is the issue a PR is for: amf/issue-N as its head branch, or
// a closing keyword in its body.
func linkedIssue(head, body string) int {
	if s, ok := strings.CutPrefix(head, "amf/issue-"); ok {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	if m := closesRef.FindStringSubmatch(body); m != nil {
		n, _ := strconv.Atoi(m[1])
		return n
	}
	return 0
}

// stopwords are left out of similarity: they match everything.
var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an and are as at be by for from has have in is it its of on or that the this to was were will with
		when what which not no do does should can add fix make use issue pr amf`) {
		stopwords[w] = true
	}
}

// tokens are text's lower-case words and numbers, without stopwords or
// one-letter words.
func tokens(text string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(f) > 1 && !stopwords[f] {
			out = append(out, f)
		}
	}
	return out
}

// titleOverlap is the Jaccard overlap of two texts' token sets, 0 to 1.
func titleOverlap(a, b string) float64 {
	sa, sb := map[string]bool{}, map[string]bool{}
	for _, t := range tokens(a) {
		sa[t] = true
	}
	for _, t := range tokens(b) {
		sb[t] = true
	}
	if len(sa) == 0 || len(sb) == 0 {
		return 0
	}
	inter := 0
	for t := range sa {
		if sb[t] {
			inter++
		}
	}
	return float64(inter) / float64(len(sa)+len(sb)-inter)
}

// memDoc is one searchable record: its title counts twice.
type memDoc struct {
	hit         Hit
	title, body string
}

// rankTFIDF scores docs against query with TF-IDF (title tokens weigh
// double), dropping docs that share no token with it. It is the
// similarity of the markdown and logfiles backends.
func rankTFIDF(docs []memDoc, query string, limit int) []Hit {
	q := tokens(query)
	if len(q) == 0 || len(docs) == 0 {
		return nil
	}
	df := map[string]int{}
	tfs := make([]map[string]float64, len(docs))
	for i, d := range docs {
		tf := map[string]float64{}
		for _, t := range tokens(d.title) {
			tf[t] += 2
		}
		for _, t := range tokens(d.body) {
			tf[t]++
		}
		for t := range tf {
			df[t]++
		}
		tfs[i] = tf
	}
	qset := map[string]bool{}
	for _, t := range q {
		qset[t] = true
	}
	var hits []Hit
	for i, d := range docs {
		var score, norm float64
		for _, v := range tfs[i] {
			norm += v
		}
		for t := range qset {
			if tf := tfs[i][t]; tf > 0 {
				idf := math.Log(1 + float64(len(docs))/float64(df[t]))
				score += (1 + math.Log(tf)) * idf
			}
		}
		if score == 0 {
			continue
		}
		h := d.hit
		h.Score = math.Round(score/math.Sqrt(1+norm/20)*1000) / 1000
		hits = append(hits, h)
	}
	sortHits(hits)
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

func sortHits(hits []Hit) {
	slices.SortStableFunc(hits, func(a, b Hit) int {
		if c := cmp.Compare(b.Score, a.Score); c != 0 {
			return c
		}
		return cmp.Compare(a.Kind+a.Ref, b.Kind+b.Ref)
	})
}

// similar ranks past work like text, leaving out issue exclude (0 for
// none) and its PRs and runs, and says what happened to each.
func similar(s MemoryStore, text string, exclude, limit int) ([]Hit, error) {
	raw, err := s.Search(text, limit*3+5)
	if err != nil {
		return nil, err
	}
	var out []Hit
	for _, h := range raw {
		if exclude > 0 && h.Issue == exclude {
			continue
		}
		h.What, h.Merged = describeHit(s, h)
		out = append(out, h)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// describeHit says in a few words what became of a hit.
func describeHit(s MemoryStore, h Hit) (string, bool) {
	if h.Issue == 0 {
		return h.What, h.Merged
	}
	hist, err := s.History(h.Issue)
	if err != nil {
		return h.What, h.Merged
	}
	for _, pr := range hist.PRs {
		if pr.Merged {
			return fmt.Sprintf("merged in PR #%d", pr.Number), true
		}
	}
	if n := len(hist.Runs); n > 0 {
		last := hist.Runs[n-1]
		if last.Outcome == "failed" {
			return "last run failed: " + truncate(oneLine(last.Error), 120), false
		}
		if last.PR > 0 {
			return fmt.Sprintf("run %s, PR #%d", last.Outcome, last.PR), false
		}
		return "last run " + last.Outcome, false
	}
	for _, pr := range hist.PRs {
		return fmt.Sprintf("PR #%d %s", pr.Number, pr.State), false
	}
	if hist.Issue != nil {
		return "issue " + hist.Issue.State, false
	}
	return h.What, h.Merged
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// duplicateOf is the hit that looks like text's duplicate: merged work
// whose title shares most words with title. nil if none.
func duplicateOf(hits []Hit, title string) *Hit {
	for i := range hits {
		if hits[i].Merged && titleOverlap(hits[i].Title, title) >= 0.6 {
			return &hits[i]
		}
	}
	return nil
}

// sortedRuns orders runs by start time.
func sortRuns(runs []MemRun) {
	slices.SortStableFunc(runs, func(a, b MemRun) int { return cmp.Compare(a.StartedAt+a.ID, b.StartedAt+b.ID) })
}

var errNoMemory = errors.New("no memory store")
