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
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		msg := string(raw)
		var parsed struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &parsed) == nil && parsed.Message != "" {
			msg = parsed.Message
		}
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: msg}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
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

// ListOpenIssues returns up to 100 open issues (not pull requests) carrying
// label, oldest first.
func (g *GitHub) ListOpenIssues(ctx context.Context, label string) ([]Issue, error) {
	q := url.Values{
		"state":     {"open"},
		"labels":    {label},
		"sort":      {"created"},
		"direction": {"asc"},
		"per_page":  {"100"},
	}
	var all []Issue
	if err := g.do(ctx, "GET", g.repoPath("/issues?%s", q.Encode()), nil, &all); err != nil {
		return nil, err
	}
	issues := all[:0]
	for _, is := range all {
		if is.PullRequest == nil {
			issues = append(issues, is)
		}
	}
	return issues, nil
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

func (g *GitHub) CreatePR(ctx context.Context, title, head, base, body string) (PullRequest, error) {
	in := map[string]any{"title": title, "head": head, "base": base, "body": body}
	var pr PullRequest
	err := g.do(ctx, "POST", g.repoPath("/pulls"), in, &pr)
	return pr, err
}
