package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// GitHub is a minimal REST client scoped to one repository.
type GitHub struct {
	base  string
	token string
	repo  string // owner/name
	http  *http.Client
}

func newGitHub(cfg Config) *GitHub {
	return &GitHub{
		base:  cfg.APIURL,
		token: cfg.Token,
		repo:  cfg.Repo,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

type Label struct {
	Name string `json:"name"`
}

type Issue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	State       string    `json:"state"`
	HTMLURL     string    `json:"html_url"`
	Labels      []Label   `json:"labels"`
	PullRequest *struct{} `json:"pull_request,omitempty"`
	User        struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (is Issue) HasLabel(name string) bool {
	for _, l := range is.Labels {
		if l.Name == name {
			return true
		}
	}
	return false
}

type PullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

// APIError is a non-2xx response from GitHub.
type APIError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

func isStatus(err error, status int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == status
}

func (g *GitHub) do(ctx context.Context, method, path string, in, out any) error {
	_, err := g.send(ctx, method, path, in, out)
	return err
}

// send is do, also returning the response headers (for pagination). It
// logs a github.call event to the logger ctx carries.
func (g *GitHub) send(ctx context.Context, method, path string, in, out any) (h http.Header, err error) {
	begin := time.Now()
	var sent int
	var resp *http.Response
	counted := &countingReader{}
	errBody := ""
	defer func() {
		tmpl, query := templatePath(path, g.repo)
		attrs := []any{"method", method, "path", tmpl, "duration_ms", ms(time.Since(begin)), "bytes_out", sent}
		status := 0
		if resp != nil {
			status = resp.StatusCode
			attrs = append(attrs, "status", status, "bytes_in", counted.n,
				"ratelimit_remaining", resp.Header.Get("X-RateLimit-Remaining"),
				"ratelimit_reset", resp.Header.Get("X-RateLimit-Reset"),
				"request_id", resp.Header.Get("X-GitHub-Request-Id"))
		}
		if p := query.Get("page"); p != "" {
			attrs = append(attrs, "page", p)
		}
		if errBody != "" {
			attrs = append(attrs, "error_body", excerpt(errBody))
		} else if err != nil {
			attrs = append(attrs, "error", err)
		}
		callEvent(ctx, "github.call", fmt.Sprintf("github %s %s: %d", method, tmpl, status), attrs...)
	}()
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		sent = len(b)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err = g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	counted.r = resp.Body

	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(counted, 8<<10))
		errBody = string(raw)
		msg := string(raw)
		var parsed struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &parsed) == nil && parsed.Message != "" {
			msg = parsed.Message
		}
		return nil, &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: msg}
	}
	if out == nil {
		return resp.Header, nil
	}
	return resp.Header, json.NewDecoder(counted).Decode(out)
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// numberSegment is a path segment that is an issue, PR or comment number.
var numberSegment = regexp.MustCompile(`^\d+$`)

// templatePath is an API path with the repo, numbers and label names as
// {repo}, {n} and {label}, so calls group by endpoint, and its query.
func templatePath(path, repo string) (string, url.Values) {
	p, rawQuery, _ := strings.Cut(path, "?")
	query, _ := url.ParseQuery(rawQuery)
	if repo != "" {
		if rest, ok := strings.CutPrefix(p, "/repos/"+repo); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
			p = "/repos/{repo}" + rest
		}
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		switch {
		case numberSegment.MatchString(s):
			segs[i] = "{n}"
		case i > 0 && segs[i-1] == "labels" && i == len(segs)-1 && i >= 2 && segs[i-2] == "{n}":
			segs[i] = "{label}"
		}
	}
	return strings.Join(segs, "/"), query
}

func (g *GitHub) repoPath(format string, a ...any) string {
	return "/repos/" + g.repo + fmt.Sprintf(format, a...)
}

func (g *GitHub) DefaultBranch(ctx context.Context) (string, error) {
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.do(ctx, "GET", g.repoPath(""), nil, &repo); err != nil {
		return "", err
	}
	if repo.DefaultBranch == "" {
		return "", errors.New("github did not report a default branch")
	}
	return repo.DefaultBranch, nil
}

func (g *GitHub) CreateIssue(ctx context.Context, title, body string, labels []string) (Issue, error) {
	in := map[string]any{"title": title, "body": body}
	if len(labels) > 0 {
		in["labels"] = labels
	}
	var is Issue
	err := g.do(ctx, "POST", g.repoPath("/issues"), in, &is)
	return is, err
}

func (g *GitHub) GetIssue(ctx context.Context, n int) (Issue, error) {
	var is Issue
	err := g.do(ctx, "GET", g.repoPath("/issues/%d", n), nil, &is)
	return is, err
}

// ListIssues returns every issue (not pull request) in the given state
// (open, closed or all) carrying label, or any label if label is empty,
// following GitHub's pagination. Issues are oldest first if asc, else newest first.
func (g *GitHub) ListIssues(ctx context.Context, state, label string, asc bool) ([]Issue, error) {
	q := url.Values{"state": {state}, "sort": {"created"}, "direction": {"desc"}, "per_page": {"100"}}
	if asc {
		q.Set("direction", "asc")
	}
	if label != "" {
		q.Set("labels", label)
	}
	var issues []Issue
	for path := g.repoPath("/issues?%s", q.Encode()); path != ""; {
		var page []Issue
		header, err := g.send(ctx, "GET", path, nil, &page)
		if err != nil {
			return nil, err
		}
		for _, is := range page {
			if is.PullRequest == nil {
				issues = append(issues, is)
			}
		}
		if path, err = g.nextPage(header); err != nil {
			return nil, err
		}
	}
	// GitHub sorts by created_at, which has one-second resolution, so issues
	// opened in the same second can come back in either order. Issue numbers
	// always increase, so they settle ties.
	slices.SortStableFunc(issues, func(a, b Issue) int {
		if asc {
			return a.Number - b.Number
		}
		return b.Number - a.Number
	})
	return issues, nil
}

// nextPage is the path of the rel="next" page in a Link header, or "" on the last page.
func (g *GitHub) nextPage(h http.Header) (string, error) {
	for _, link := range strings.Split(h.Get("Link"), ",") {
		target, params, ok := strings.Cut(strings.TrimSpace(link), ";")
		if !ok || !strings.Contains(params, `rel="next"`) {
			continue
		}
		target = strings.Trim(strings.TrimSpace(target), "<>")
		path, ok := strings.CutPrefix(target, g.base)
		if !ok {
			return "", fmt.Errorf("github pagination link %q is outside %s", target, g.base)
		}
		return path, nil
	}
	return "", nil
}

// LabelSpec describes a repository label.
type LabelSpec struct {
	Name, Color, Description string
}

// EnsureLabel creates the label if the repository doesn't have it yet and
// reports whether it did.
func (g *GitHub) EnsureLabel(ctx context.Context, l LabelSpec) (created bool, err error) {
	err = g.do(ctx, "GET", g.repoPath("/labels/%s", url.PathEscape(l.Name)), nil, nil)
	if err == nil {
		return false, nil
	}
	if !isStatus(err, http.StatusNotFound) {
		return false, err
	}
	in := map[string]string{"name": l.Name, "color": l.Color, "description": l.Description}
	err = g.do(ctx, "POST", g.repoPath("/labels"), in, nil)
	if isStatus(err, http.StatusUnprocessableEntity) { // created by someone else meanwhile
		return false, nil
	}
	return err == nil, err
}

// AddLabels adds labels to an issue; GitHub creates labels that don't exist.
func (g *GitHub) AddLabels(ctx context.Context, n int, labels ...string) error {
	return g.do(ctx, "POST", g.repoPath("/issues/%d/labels", n), map[string]any{"labels": labels}, nil)
}

// RemoveLabel removes a label from an issue. A label that isn't there is not an error.
func (g *GitHub) RemoveLabel(ctx context.Context, n int, label string) error {
	err := g.do(ctx, "DELETE", g.repoPath("/issues/%d/labels/%s", n, url.PathEscape(label)), nil, nil)
	if isStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

func (g *GitHub) Comment(ctx context.Context, n int, body string) error {
	return g.do(ctx, "POST", g.repoPath("/issues/%d/comments", n), map[string]any{"body": body}, nil)
}

// CreatePR opens a pull request from head into base, as a draft when draft
// is true.
func (g *GitHub) CreatePR(ctx context.Context, title, head, base, body string, draft bool) (PullRequest, error) {
	in := map[string]any{"title": title, "head": head, "base": base, "body": body}
	if draft {
		in["draft"] = true
	}
	var pr PullRequest
	err := g.do(ctx, "POST", g.repoPath("/pulls"), in, &pr)
	return pr, err
}

// OpenPR finds the open pull request from head, a branch in this repository.
func (g *GitHub) OpenPR(ctx context.Context, head string) (pr PullRequest, ok bool, err error) {
	owner, _, _ := strings.Cut(g.repo, "/")
	q := url.Values{"head": {owner + ":" + head}, "state": {"open"}}
	var prs []PullRequest
	if err := g.do(ctx, "GET", g.repoPath("/pulls?%s", q.Encode()), nil, &prs); err != nil {
		return PullRequest{}, false, err
	}
	if len(prs) == 0 {
		return PullRequest{}, false, nil
	}
	return prs[0], true, nil
}

// UpdatePR replaces a pull request's title and body, leaving it a draft or
// ready for review as it was.
func (g *GitHub) UpdatePR(ctx context.Context, n int, title, body string) (PullRequest, error) {
	var pr PullRequest
	err := g.do(ctx, "PATCH", g.repoPath("/pulls/%d", n), map[string]any{"title": title, "body": body}, &pr)
	return pr, err
}
