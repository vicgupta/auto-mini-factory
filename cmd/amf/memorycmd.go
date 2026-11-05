package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ghPull is a pull request as GET /pulls lists it, for amf memory sync.
type ghPull struct {
	Number    int     `json:"number"`
	Title     string  `json:"title"`
	Body      string  `json:"body"`
	State     string  `json:"state"`
	MergedAt  *string `json:"merged_at"`
	UpdatedAt string  `json:"updated_at"`
	Head      struct {
		Ref string `json:"ref"`
	} `json:"head"`
}

// IssuesSince returns every issue and pull request updated at or after
// since ("" for all), oldest update first, following pagination. Pull
// requests come back with PullRequest set.
func (g *GitHub) IssuesSince(ctx context.Context, since string) ([]Issue, error) {
	q := url.Values{"state": {"all"}, "sort": {"updated"}, "direction": {"asc"}, "per_page": {"100"}}
	if since != "" {
		q.Set("since", since)
	}
	var out []Issue
	for path := g.repoPath("/issues?%s", q.Encode()); path != ""; {
		var page []Issue
		h, err := g.send(ctx, "GET", path, nil, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if path, err = g.nextPage(h); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// PullsSince returns the pull requests updated at or after since ("" for
// all), newest update first, stopping at the first page that reaches
// older ones.
func (g *GitHub) PullsSince(ctx context.Context, since string) ([]ghPull, error) {
	q := url.Values{"state": {"all"}, "sort": {"updated"}, "direction": {"desc"}, "per_page": {"100"}}
	var out []ghPull
	for path := g.repoPath("/pulls?%s", q.Encode()); path != ""; {
		var page []ghPull
		h, err := g.send(ctx, "GET", path, nil, &page)
		if err != nil {
			return nil, err
		}
		for _, p := range page {
			if since != "" && p.UpdatedAt < since {
				return out, nil
			}
			out = append(out, p)
		}
		if path, err = g.nextPage(h); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func memIssueFrom(is Issue) MemIssue {
	m := MemIssue{Number: is.Number, Title: is.Title, Body: is.Body, State: is.State, Labels: labelNames(is),
		Author: is.User.Login, CreatedAt: is.CreatedAt, ClosedAt: is.ClosedAt}
	if !is.UpdatedAt.IsZero() {
		m.UpdatedAt = is.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return m
}

// syncResult counts what amf memory sync stored.
type syncResult struct {
	Issues int    `json:"issues"`
	PRs    int    `json:"pull_requests"`
	Linked int    `json:"linked"`
	Cursor string `json:"cursor"`
}

// syncMemory backfills s from GitHub: the issues and pull requests updated
// since its cursor (everything the first time), linking each PR to its
// issue by its amf/issue-N branch or a "Closes #N" in its body.
func syncMemory(ctx context.Context, gh *GitHub, s MemoryStore, full bool) (syncResult, error) {
	var res syncResult
	since := ""
	if !full {
		var err error
		if since, err = s.SyncCursor(); err != nil {
			return res, err
		}
	}
	started := time.Now().UTC().Format(time.RFC3339)
	items, err := gh.IssuesSince(ctx, since)
	if err != nil {
		return res, err
	}
	for _, is := range items {
		if is.PullRequest != nil {
			continue
		}
		if err := s.UpsertIssue(memIssueFrom(is)); err != nil {
			return res, err
		}
		res.Issues++
	}
	pulls, err := gh.PullsSince(ctx, since)
	if err != nil {
		return res, err
	}
	for _, p := range pulls {
		pr := MemPR{Number: p.Number, Title: p.Title, Body: p.Body, State: p.State, Merged: p.MergedAt != nil,
			HeadBranch: p.Head.Ref, Issue: linkedIssue(p.Head.Ref, p.Body), UpdatedAt: p.UpdatedAt}
		if p.MergedAt != nil {
			pr.MergedAt, pr.State = *p.MergedAt, "merged"
		}
		if pr.Issue > 0 {
			res.Linked++
		}
		if err := s.UpsertPR(pr); err != nil {
			return res, err
		}
		res.PRs++
	}
	// GitHub's since is inclusive, so the next sync rereads what changed
	// during this one rather than missing it.
	res.Cursor = started
	return res, s.SetSyncCursor(started)
}

var memoryUsage = `Usage:
  amf memory sync [--full] [flags]
  amf memory similar <issue-number | "text"> [--limit n] [--json] [flags]
  amf memory show <issue-number> [--json] [flags]

amf keeps a per-repo memory of issues, pull requests and runs, so it can
point a new issue at similar past work.

  sync     backfill or refresh memory from GitHub's issues and pull requests
           (only what changed since the last sync; --full rereads all)
  similar  rank past issues, PRs and runs like an issue or a piece of text
  show     an issue's full history: its PRs and every run

amf run, watch and auto record every attempt in memory and, when they pick
up an issue, log similar past work (event memory.similar), warn when it
looks like a duplicate of merged work (memory.duplicate; nothing is closed),
and add a short "Similar past work" note to the agent's prompt.

--memory-backend picks where memory is kept (the same flag on run, watch
and auto):
  sqlite    (default) <dir>/amf-memory/<host>/<owner>/<repo>/memory.db,
            searched with SQLite FTS5
  markdown  one file per record: issues/N.md, prs/N.md, runs/<id>.md, plus
            index.md and index.json, in the same per-repo folder
  logfiles  append-only JSON Lines: issues.jsonl, prs.jsonl, runs.jsonl,
            sync.jsonl, in the same per-repo folder
--memory-dir is <dir>, the current directory by default; a relative path is
taken from the current directory. amf never commits the amf-memory folder.
Markdown and log files are written only when that backend is chosen.

Each repo (host/owner/name from GITHUB_REPO) has its own folder and store,
and a store that records another repo refuses to open.
` + "\nExamples:\n" + helpExamples["memory"] + "\nSee also: amf help run, amf help watch, amf help auto\n"

type memoryCommandFlags struct {
	memory *memoryOptions
	asJSON *bool
	limit  *int
	full   *bool
}

func addMemoryCommandFlags(fs *flag.FlagSet) memoryCommandFlags {
	return memoryCommandFlags{
		memory: addMemoryFlags(fs),
		asJSON: fs.Bool("json", false, "print JSON"),
		limit:  fs.Int("limit", 5, "how many results similar prints"),
		full:   fs.Bool("full", false, "with sync, reread every issue and PR instead of only what changed"),
	}
}

func cmdMemory(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || isHelpArgs(args) {
		w := stdout
		if len(args) == 0 {
			w = stderr
		}
		fmt.Fprint(w, memoryUsage)
		if len(args) == 0 {
			return errUsage
		}
		return nil
	}
	sub, rest := args[0], args[1:]
	if sub != "sync" && sub != "similar" && sub != "show" {
		fmt.Fprintf(stderr, "amf memory: unknown subcommand %q\n\n%s", sub, memoryUsage)
		return errUsage
	}
	fs := newCommandFlags("memory "+sub, stderr)
	f := addMemoryCommandFlags(fs)
	pos, err := parseArgs(fs, rest)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return errUsage
	}
	if err := f.memory.check(); err != nil {
		return err
	}
	switch sub {
	case "sync":
		if len(pos) != 0 {
			return errors.New("amf memory sync takes no arguments")
		}
	case "similar", "show":
		if len(pos) == 0 {
			fmt.Fprint(stderr, memoryUsage)
			return errUsage
		}
	}
	if *f.limit < 1 {
		return errors.New("--limit must be at least 1")
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	root, err := git(ctx, ".", "rev-parse", "--show-toplevel")
	inGit := err == nil
	if !inGit {
		root, _ = os.Getwd()
	}
	store, err := openMemory(*f.memory, repoKey(cfg.APIURL, cfg.Repo, root))
	if err != nil {
		return err
	}
	defer store.Close()
	if inGit {
		if err := excludeMemory(root, *f.memory); err != nil {
			fmt.Fprintf(stderr, "amf: warning: could not update .git/info/exclude: %v\n", err)
		}
	}
	gh := newGitHub(cfg)

	switch sub {
	case "sync":
		res, err := syncMemory(ctx, gh, store, *f.full)
		if err != nil {
			return err
		}
		if *f.asJSON {
			return writeJSONOut(stdout, res)
		}
		fmt.Fprintf(stdout, "synced %d %s and %d pull %s (%d linked to issues) into %s memory for %s\n",
			res.Issues, plural(res.Issues, "issue", "issues"), res.PRs, plural(res.PRs, "request", "requests"), res.Linked, f.memory.Backend, store.Key())
		return nil
	case "show":
		n, err := issueArg(pos)
		if err != nil {
			return err
		}
		h, err := store.History(n)
		if err != nil {
			return err
		}
		if *f.asJSON {
			return writeJSONOut(stdout, h)
		}
		printHistory(stdout, n, h)
		return nil
	}
	// similar
	text, exclude := strings.Join(pos, " "), 0
	if n, err := issueArg(pos); err == nil {
		exclude = n
		h, err := store.History(n)
		if err != nil {
			return err
		}
		if h.Issue == nil {
			is, gerr := gh.GetIssue(ctx, n)
			if gerr != nil {
				return fmt.Errorf("#%d is not in memory (run amf memory sync) and GitHub says: %w", n, gerr)
			}
			h.Issue = &MemIssue{Number: n, Title: is.Title, Body: is.Body}
		}
		text = h.Issue.Title + "\n" + h.Issue.Body
	}
	hits, err := similar(store, text, exclude, *f.limit)
	if err != nil {
		return err
	}
	if *f.asJSON {
		if hits == nil {
			hits = []Hit{}
		}
		return writeJSONOut(stdout, hits)
	}
	if len(hits) == 0 {
		fmt.Fprintln(stdout, "no similar past work in memory")
		return nil
	}
	for _, h := range hits {
		fmt.Fprintf(stdout, "%-6.2f %s\n", h.Score, describeLine(h))
	}
	return nil
}

func issueArg(pos []string) (int, error) {
	if len(pos) != 1 {
		return 0, errors.New("want one issue number")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(pos[0]), "#"))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid issue number %q; use a positive number such as 42 or #42", pos[0])
	}
	return n, nil
}

func writeJSONOut(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// describeLine is a hit in one line: what it is and what happened.
func describeLine(h Hit) string {
	var what string
	switch h.Kind {
	case "issue":
		what = fmt.Sprintf("issue #%d %s", h.Issue, h.Title)
	case "pr":
		what = fmt.Sprintf("PR #%d %s", h.PR, h.Title)
	default:
		what = fmt.Sprintf("run %s (%s)", h.Ref, h.Title)
	}
	if h.What != "" {
		what += " — " + h.What
	}
	return what
}

func printHistory(w io.Writer, n int, h IssueHistory) {
	if h.Issue == nil {
		fmt.Fprintf(w, "#%d: not in memory\n", n)
	} else {
		is := h.Issue
		fmt.Fprintf(w, "#%d %s\nstate: %s  labels: %s  author: %s\ncreated: %s  updated: %s  closed: %s\n",
			is.Number, is.Title, is.State, strings.Join(is.Labels, ", "), is.Author, is.CreatedAt, is.UpdatedAt, is.ClosedAt)
	}
	fmt.Fprintf(w, "\npull requests: %d\n", len(h.PRs))
	for _, pr := range h.PRs {
		fmt.Fprintf(w, "  #%d %s (%s, branch %s)\n", pr.Number, pr.Title, prWhat(pr), pr.HeadBranch)
	}
	fmt.Fprintf(w, "\nruns: %d\n", len(h.Runs))
	for _, r := range h.Runs {
		fmt.Fprintf(w, "  %s %s %s agents=%s", r.StartedAt, r.Command, r.Outcome, strings.Join(r.Agents, ","))
		if r.PR > 0 {
			fmt.Fprintf(w, " pr=#%d", r.PR)
		}
		if r.Error != "" {
			fmt.Fprintf(w, " error=%s", truncate(oneLine(r.Error), 200))
		}
		fmt.Fprintln(w)
	}
}

// --- the runner's hooks ---

// openRunnerMemory gives r its memory store. A store that won't open is a
// warning: memory never stops a run.
func (r *Runner) openMemory(mo memoryOptions) {
	key := repoKey(r.GH.base, r.GH.repo, r.Root)
	s, err := openMemory(mo, key)
	if err != nil {
		r.event(slog.LevelWarn, "memory.error", 0, fmt.Sprintf("memory is off for this run: %v", err), "error", err)
		return
	}
	r.Memory = s
	r.memWarn(0, "exclude memory from git", excludeMemory(r.Root, mo))
}

// excludeMemory keeps an amf-memory folder inside the checkout out of git
// status and commits, through .git/info/exclude.
func excludeMemory(root string, mo memoryOptions) error {
	base, err := mo.baseDir()
	if err != nil || root == "" {
		return err
	}
	// Compare real paths: on macOS a temp dir and git's view of it can differ
	// by a /private symlink.
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	if b, err := filepath.EvalSymlinks(base); err == nil {
		base = b
	}
	rel, err := filepath.Rel(root, filepath.Join(base, memoryDirName))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	return excludeFromGit(context.Background(), root, "amf memory", "/"+filepath.ToSlash(rel)+"/")
}

// memWarn reports a memory failure and carries on.
func (r *Runner) memWarn(n int, what string, err error) {
	if err != nil {
		r.event(slog.LevelWarn, "memory.error", n, fmt.Sprintf("memory: could not %s: %v", what, err), "action", what, "error", err)
	}
}

// memTriage looks up past work like an issue's text in memory, logs it,
// warns on a likely duplicate of merged work, and returns a short note for
// the agent's prompt ("" for none).
func (r *Runner) memTriage(n int, title, body string) string {
	if r.Memory == nil {
		return ""
	}
	hits, err := similar(r.Memory, title+"\n"+body, n, 3)
	if err != nil {
		r.memWarn(n, "search", err)
		return ""
	}
	if len(hits) == 0 {
		return ""
	}
	lines := make([]string, len(hits))
	for i, h := range hits {
		lines[i] = describeLine(h)
	}
	label := fmt.Sprintf("#%d", n)
	if n == 0 {
		label = fmt.Sprintf("%q", title)
	}
	r.event(slog.LevelInfo, "memory.similar", n, fmt.Sprintf("%s: similar past work: %s", label, strings.Join(lines, "; ")),
		"hits", len(hits), "top_kind", hits[0].Kind, "top_ref", hits[0].Ref, "top_score", hits[0].Score)
	if d := duplicateOf(hits, title); d != nil {
		r.event(slog.LevelWarn, "memory.duplicate", n, fmt.Sprintf("%s may duplicate already-merged work: %s", label, describeLine(*d)),
			"kind", d.Kind, "ref", d.Ref, "issue", d.Issue, "pr", d.PR)
	}
	var b strings.Builder
	b.WriteString("Similar past work in this repository (from amf's memory; for context only, not part of the issue):\n")
	for _, l := range lines {
		b.WriteString("- " + l + "\n")
	}
	return b.String()
}

// memRecordRun records one attempt at issue n and how it ended.
func (r *Runner) memRecordRun(n int, started time.Time, err error) {
	if r.Memory == nil {
		return
	}
	run := MemRun{
		ID: started.UTC().Format("20060102T150405.000Z") + "-" + strconv.Itoa(n), Issue: n, Command: cmpOr(r.Command, "run"),
		StartedAt: started.UTC().Format(time.RFC3339), FinishedAt: time.Now().UTC().Format(time.RFC3339),
		Outcome: "success", PR: r.lastPR.Number, LogTail: r.Redact.Redact(issueLogTail(r.Root, n, 2048)),
	}
	if r.lastAgent != "" {
		run.Agents = []string{r.lastAgent}
	}
	if r.fellBack != "" {
		run.Summary = r.fellBack
	}
	if err != nil {
		run.Outcome, run.Error = "failed", r.Redact.Redact(truncate(err.Error(), 2000))
		var ee interface{ ExitCode() int }
		if errors.As(err, &ee) {
			run.ExitCodes = []int{ee.ExitCode()}
		}
	}
	if r.lastPR.Number > 0 {
		run.Summary = strings.TrimSpace(fmt.Sprintf("opened or updated PR #%d. %s", r.lastPR.Number, run.Summary))
	}
	r.memWarn(n, "record the run", r.Memory.RecordRun(run))
	if r.lastPR.Number > 0 {
		pr := MemPR{Number: r.lastPR.Number, Title: "", State: "open", HeadBranch: issueBranch(n), Issue: n}
		if h, herr := r.Memory.History(n); herr == nil && h.Issue != nil {
			pr.Title = fmt.Sprintf("Fix #%d: %s", n, h.Issue.Title)
		}
		if r.lastCommit != "" {
			pr.State, pr.Merged, pr.MergedAt = "merged", true, run.FinishedAt
		}
		r.memWarn(n, "record the PR", r.Memory.UpsertPR(pr))
	}
}

// issueLogTail is the last max bytes of issue n's newest agent log.
func issueLogTail(root string, n, max int) string {
	matches, _ := filepath.Glob(filepath.Join(root, ".amf", "logs", fmt.Sprintf("issue-%d*.log", n)))
	var newest string
	var when time.Time
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.ModTime().After(when) {
			newest, when = m, st.ModTime()
		}
	}
	if newest == "" {
		return ""
	}
	b, err := os.ReadFile(newest)
	if err != nil {
		return ""
	}
	if len(b) > max {
		b = b[len(b)-max:]
	}
	return strings.ToValidUTF8(string(b), "")
}
