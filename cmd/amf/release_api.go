package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// tagInfo is one entry from GitHub's tag list.
type tagInfo struct {
	Name   string
	Commit string
}

// peeledTag is a tag resolved to the commit it ultimately names.
type peeledTag struct {
	Name          string
	ImmediateSHA  string
	ImmediateType string
	Commit        string
	Depth         int
	Annotated     bool
}

// releaseAsset is one asset attached to a GitHub release.
type releaseAsset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	State              string `json:"state"`
	ContentType        string `json:"content_type"`
	BrowserDownloadURL string `json:"browser_download_url"`
	URL                string `json:"url"`
}

// ghRelease is the part of GitHub's release response amf uses.
type ghRelease struct {
	ID              int64          `json:"id"`
	TagName         string         `json:"tag_name"`
	TargetCommitish string         `json:"target_commitish"`
	Name            string         `json:"name"`
	Body            string         `json:"body"`
	Draft           bool           `json:"draft"`
	Prerelease      bool           `json:"prerelease"`
	CreatedAt       string         `json:"created_at"`
	PublishedAt     string         `json:"published_at"`
	HTMLURL         string         `json:"html_url"`
	UploadURL       string         `json:"upload_url"`
	Assets          []releaseAsset `json:"assets"`
}

func (r ghRelease) state() string {
	switch {
	case r.Draft:
		return "draft"
	case r.Prerelease:
		return "prerelease"
	default:
		return "published"
	}
}

// ListTags returns every tag GitHub reports, sorted by name. The commit is
// the one GitHub lists for the tag; GetTagPeeled is the authority for
// annotated tags.
func (g *GitHub) ListTags(ctx context.Context) ([]tagInfo, error) {
	var out []tagInfo
	for path := g.repoPath("/tags?per_page=100"); path != ""; {
		var page []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		header, err := g.send(ctx, "GET", path, nil, &page)
		if err != nil {
			return nil, err
		}
		for _, t := range page {
			out = append(out, tagInfo{Name: t.Name, Commit: t.Commit.SHA})
		}
		if path, err = g.nextPage(header); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(out, func(a, b tagInfo) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

const maxTagDepth = 32

// GetTagPeeled resolves name through every annotated tag object to the
// commit it names. Cycles, excessive nesting and non-commit targets fail
// instead of looping or guessing.
func (g *GitHub) GetTagPeeled(ctx context.Context, name string) (peeledTag, error) {
	var ref struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA  string `json:"sha"`
			Type string `json:"type"`
		} `json:"object"`
	}
	err := g.do(ctx, "GET", g.repoPath("/git/ref/tags/%s", url.PathEscape(name)), nil, &ref)
	if isStatus(err, http.StatusNotFound) {
		return peeledTag{}, &APIError{Method: "GET", Path: g.repoPath("/git/ref/tags/%s", name), Status: http.StatusNotFound, Message: "Not Found"}
	}
	if err != nil {
		return peeledTag{}, err
	}
	peeled := peeledTag{Name: name, ImmediateSHA: ref.Object.SHA, ImmediateType: ref.Object.Type}
	sha, typ := ref.Object.SHA, ref.Object.Type
	seen := map[string]bool{}
	for {
		if sha == "" || typ == "" {
			return peeledTag{}, fmt.Errorf("tag %s has an incomplete object", name)
		}
		switch typ {
		case "commit":
			peeled.Commit = sha
			return peeled, nil
		case "tag":
			if seen[sha] {
				return peeledTag{}, fmt.Errorf("tag %s has a tag-object cycle at %s", name, shortSHA(sha))
			}
			seen[sha] = true
			if peeled.Depth >= maxTagDepth {
				return peeledTag{}, fmt.Errorf("tag %s is nested more than %d tag objects deep", name, maxTagDepth)
			}
			var tag struct {
				SHA    string `json:"sha"`
				Object struct {
					SHA  string `json:"sha"`
					Type string `json:"type"`
				} `json:"object"`
			}
			if err := g.do(ctx, "GET", g.repoPath("/git/tags/%s", url.PathEscape(sha)), nil, &tag); err != nil {
				return peeledTag{}, err
			}
			peeled.Annotated = true
			peeled.Depth++
			sha, typ = tag.Object.SHA, tag.Object.Type
		default:
			return peeledTag{}, fmt.Errorf("tag %s points to a %s, not a commit", name, typ)
		}
	}
}

// CreateTagRef creates refs/tags/name at sha. It never updates an existing
// ref: GitHub answers 422 for one, which callers resolve by re-reading it.
func (g *GitHub) CreateTagRef(ctx context.Context, name, sha string) error {
	return g.do(ctx, "POST", g.repoPath("/git/refs"), map[string]string{
		"ref": "refs/tags/" + name,
		"sha": sha,
	}, nil)
}

// ListReleases returns every release, newest first.
func (g *GitHub) ListReleases(ctx context.Context) ([]ghRelease, error) {
	var out []ghRelease
	for path := g.repoPath("/releases?per_page=100"); path != ""; {
		var page []ghRelease
		header, err := g.send(ctx, "GET", path, nil, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if path, err = g.nextPage(header); err != nil {
			return nil, err
		}
	}
	slices.SortStableFunc(out, func(a, b ghRelease) int {
		if c := strings.Compare(b.CreatedAt, a.CreatedAt); c != 0 {
			return c
		}
		return int(b.ID - a.ID)
	})
	return out, nil
}

// GetReleaseByTag fetches one release, draft included.
func (g *GitHub) GetReleaseByTag(ctx context.Context, tag string) (ghRelease, error) {
	var rel ghRelease
	err := g.do(ctx, "GET", g.repoPath("/releases/tags/%s", url.PathEscape(tag)), nil, &rel)
	return rel, err
}

// LatestRelease fetches the repository's latest published, non-prerelease
// release.
func (g *GitHub) LatestRelease(ctx context.Context) (ghRelease, error) {
	var rel ghRelease
	err := g.do(ctx, "GET", g.repoPath("/releases/latest"), nil, &rel)
	return rel, err
}

// CreateDraftRelease creates a draft release pinned to an existing tag.
func (g *GitHub) CreateDraftRelease(ctx context.Context, tag, commit, name, body string, prerelease bool) (ghRelease, error) {
	in := map[string]any{
		"tag_name":         tag,
		"target_commitish": commit,
		"name":             name,
		"body":             body,
		"draft":            true,
		"prerelease":       prerelease,
	}
	var rel ghRelease
	err := g.do(ctx, "POST", g.repoPath("/releases"), in, &rel)
	return rel, err
}

// PublishRelease turns draft release id into a published release.
func (g *GitHub) PublishRelease(ctx context.Context, id int64) (ghRelease, error) {
	var rel ghRelease
	err := g.do(ctx, "PATCH", g.repoPath("/releases/%d", id), map[string]any{"draft": false}, &rel)
	return rel, err
}

// ListReleaseAssets follows every page of a release's assets.
func (g *GitHub) ListReleaseAssets(ctx context.Context, id int64) ([]releaseAsset, error) {
	var out []releaseAsset
	for path := g.repoPath("/releases/%d/assets?per_page=100", id); path != ""; {
		var page []releaseAsset
		header, err := g.send(ctx, "GET", path, nil, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if path, err = g.nextPage(header); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteReleaseAsset removes an incomplete draft asset so a valid upload can
// replace it. Callers re-read the asset first and only delete an asset whose
// state is not uploaded.
func (g *GitHub) DeleteReleaseAsset(ctx context.Context, id int64) error {
	return g.do(ctx, "DELETE", g.repoPath("/releases/assets/%d", id), nil, nil)
}

// uploadURL validates a release's upload_url and returns it with name set.
// The URL must stay on GitHub's API or uploads host and end in the expected
// release-assets path; no upload is ever sent to an arbitrary host.
func (g *GitHub) uploadURL(rel ghRelease, name string) (*url.URL, error) {
	base, err := url.Parse(g.base)
	if err != nil {
		return nil, fmt.Errorf("invalid AMF_API_URL: %w", err)
	}
	raw := rel.UploadURL
	if raw == "" {
		raw = g.base + g.repoPath("/releases/%d/assets", rel.ID)
	}
	if i := strings.IndexByte(raw, '{'); i >= 0 {
		raw = raw[:i]
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("release %s has an invalid upload URL: %w", rel.TagName, err)
	}
	if u.User != nil || u.Fragment != "" {
		return nil, fmt.Errorf("release %s has an unsafe upload URL", rel.TagName)
	}
	allowHTTP := strings.HasPrefix(g.base, "http://")
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return nil, fmt.Errorf("release %s upload URL must use HTTPS, got %q", rel.TagName, u.Scheme)
	}
	host := strings.ToLower(u.Host)
	if host != strings.ToLower(base.Host) && host != "uploads.github.com" {
		return nil, fmt.Errorf("release %s upload URL host %q is not the GitHub API or uploads host", rel.TagName, u.Host)
	}
	if strings.Contains(u.Path, "..") {
		return nil, fmt.Errorf("release %s has an unsafe upload URL path", rel.TagName)
	}
	want := fmt.Sprintf("/repos/%s/releases/%d/assets", g.repo, rel.ID)
	if !strings.HasSuffix(u.Path, want) {
		return nil, fmt.Errorf("release %s upload URL path %q does not end in %q", rel.TagName, u.Path, want)
	}
	q := u.Query()
	q.Set("name", name)
	u.RawQuery = q.Encode()
	return u, nil
}

// UploadReleaseAsset uploads one already verified asset snapshot. Redirects
// are refused: the Authorization header never follows an upload elsewhere.
func (g *GitHub) UploadReleaseAsset(ctx context.Context, rel ghRelease, name string, data []byte) (releaseAsset, error) {
	u, err := g.uploadURL(rel, name)
	if err != nil {
		return releaseAsset{}, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", u.String(), bytes.NewReader(data))
	if err != nil {
		return releaseAsset{}, err
	}
	req.ContentLength = int64(len(data))
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := &http.Client{
		Timeout:   10 * time.Minute,
		Transport: g.http.Transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return releaseAsset{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return releaseAsset{}, fmt.Errorf("upload of %s was redirected to %q; refusing to forward credentials", name, resp.Header.Get("Location"))
	}
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		msg := string(raw)
		var parsed struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &parsed) == nil && parsed.Message != "" {
			msg = parsed.Message
		}
		return releaseAsset{}, &APIError{Method: "POST", Path: u.Path, Status: resp.StatusCode, Message: msg}
	}
	var asset releaseAsset
	err = json.NewDecoder(resp.Body).Decode(&asset)
	return asset, err
}

// DownloadReleaseAsset checksum reads an asset through the API and returns
// its SHA-256 and size. Authorization is only sent to the API host; a
// redirect to a storage host is followed without it.
func (g *GitHub) DownloadReleaseAssetChecksum(ctx context.Context, asset releaseAsset) (string, int64, error) {
	raw := asset.URL
	if raw == "" {
		raw = asset.BrowserDownloadURL
	}
	if raw == "" {
		return "", 0, fmt.Errorf("asset %s has no download URL", asset.Name)
	}
	base, _ := url.Parse(g.base)
	allowHTTP := strings.HasPrefix(g.base, "http://")
	u, err := url.Parse(raw)
	if err != nil {
		return "", 0, err
	}
	client := &http.Client{
		Timeout:   10 * time.Minute,
		Transport: g.http.Transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	for redirects := 0; ; redirects++ {
		if u.User != nil || u.Fragment != "" {
			return "", 0, fmt.Errorf("asset %s has an unsafe download URL", asset.Name)
		}
		if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
			return "", 0, fmt.Errorf("asset %s download URL must use HTTPS, got %q", asset.Name, u.Scheme)
		}
		req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
		if err != nil {
			return "", 0, err
		}
		if base != nil && strings.EqualFold(u.Host, base.Host) {
			req.Header.Set("Authorization", "Bearer "+g.token)
		}
		req.Header.Set("Accept", "application/octet-stream")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		resp, err := client.Do(req)
		if err != nil {
			return "", 0, err
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			if loc == "" {
				return "", 0, fmt.Errorf("asset %s download redirected without a Location", asset.Name)
			}
			if redirects >= 5 {
				return "", 0, fmt.Errorf("asset %s download redirected too many times", asset.Name)
			}
			next, err := u.Parse(loc)
			if err != nil {
				return "", 0, fmt.Errorf("asset %s has an invalid redirect: %w", asset.Name, err)
			}
			u = next
			continue
		}
		if resp.StatusCode >= 300 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
			resp.Body.Close()
			msg := string(raw)
			var parsed struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(raw, &parsed) == nil && parsed.Message != "" {
				msg = parsed.Message
			}
			return "", 0, &APIError{Method: "GET", Path: u.Path, Status: resp.StatusCode, Message: msg}
		}
		h := sha256.New()
		n, err := io.Copy(h, resp.Body)
		resp.Body.Close()
		if err != nil {
			return "", 0, err
		}
		return hex.EncodeToString(h.Sum(nil)), n, nil
	}
}

// checkRunFull is a check run plus the commit it belongs to.
type checkRunFull struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// commitStatus is one commit status, with when it was created so the latest
// per context wins.
type commitStatus struct {
	ID        int64     `json:"id"`
	Context   string    `json:"context"`
	State     string    `json:"state"`
	SHA       string    `json:"sha"`
	CreatedAt time.Time `json:"created_at"`
}

// checkRunsAll follows every page of a commit's check runs.
func (g *GitHub) checkRunsAll(ctx context.Context, sha string) ([]checkRunFull, error) {
	var out []checkRunFull
	for path := g.repoPath("/commits/%s/check-runs?per_page=100&filter=all", url.PathEscape(sha)); path != ""; {
		var page struct {
			CheckRuns []checkRunFull `json:"check_runs"`
		}
		header, err := g.send(ctx, "GET", path, nil, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page.CheckRuns...)
		if path, err = g.nextPage(header); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// statusesAll follows every page of a commit's statuses.
func (g *GitHub) statusesAll(ctx context.Context, sha string) ([]commitStatus, error) {
	var out []commitStatus
	for path := g.repoPath("/commits/%s/statuses?per_page=100", url.PathEscape(sha)); path != ""; {
		var page []commitStatus
		header, err := g.send(ctx, "GET", path, nil, &page)
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if path, err = g.nextPage(header); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// requiredCheckContexts reads the default branch's required status-check
// contexts. readable is false when GitHub won't show the policy (for example
// to a token without admin access); callers then require every reported
// result to pass but can't detect a required check that never ran.
func (g *GitHub) requiredCheckContexts(ctx context.Context) (contexts []string, readable bool, err error) {
	branch, err := g.DefaultBranch(ctx)
	if err != nil {
		return nil, false, err
	}
	var out struct {
		Contexts []string `json:"contexts"`
		Checks   []struct {
			Context string `json:"context"`
		} `json:"checks"`
	}
	err = g.do(ctx, "GET", g.repoPath("/branches/%s/protection/required_status_checks", url.PathEscape(branch)), nil, &out)
	switch {
	case isStatus(err, http.StatusNotFound):
		return nil, true, nil // branch is not protected
	case isStatus(err, http.StatusForbidden):
		return nil, false, nil
	case err != nil:
		return nil, false, err
	}
	seen := map[string]bool{}
	for _, c := range out.Contexts {
		if c != "" && !seen[c] {
			seen[c] = true
			contexts = append(contexts, c)
		}
	}
	for _, c := range out.Checks {
		if c.Context != "" && !seen[c.Context] {
			seen[c.Context] = true
			contexts = append(contexts, c.Context)
		}
	}
	return contexts, true, nil
}

// requireCI refuses unless every check run and commit status on the exact
// target SHA has passed. It rejects failed, pending, missing, inconclusive
// and mismatched results, and required contexts that never reported.
func (g *GitHub) requireCI(ctx context.Context, sha string) error {
	runs, err := g.checkRunsAll(ctx, sha)
	if err != nil {
		return fmt.Errorf("could not read check runs on %s: %w", shortSHA(sha), err)
	}
	statuses, err := g.statusesAll(ctx, sha)
	if err != nil {
		return fmt.Errorf("could not read commit statuses on %s: %w", shortSHA(sha), err)
	}
	for _, run := range runs {
		if run.HeadSHA != "" && !strings.EqualFold(run.HeadSHA, sha) {
			return fmt.Errorf("check run %q reports %s, not the target %s", run.Name, shortSHA(run.HeadSHA), shortSHA(sha))
		}
	}
	for _, status := range statuses {
		if status.SHA != "" && !strings.EqualFold(status.SHA, sha) {
			return fmt.Errorf("commit status %q reports %s, not the target %s", status.Context, shortSHA(status.SHA), shortSHA(sha))
		}
	}

	latest := map[string]commitStatus{}
	for _, status := range statuses {
		if old, ok := latest[status.Context]; !ok || status.CreatedAt.After(old.CreatedAt) || status.CreatedAt.Equal(old.CreatedAt) && status.ID > old.ID {
			latest[status.Context] = status
		}
	}
	var contexts []string
	for c := range latest {
		contexts = append(contexts, c)
	}
	slices.Sort(contexts)

	failed, pending, inconclusive := false, false, false
	var failedNames []string
	for _, run := range runs {
		if run.Status != "completed" {
			pending = true
			continue
		}
		switch run.Conclusion {
		case "success":
		case "neutral", "skipped":
			inconclusive = true
			failedNames = append(failedNames, run.Name+" ("+run.Conclusion+")")
		default:
			failed = true
			failedNames = append(failedNames, run.Name+" ("+run.Conclusion+")")
		}
	}
	for _, c := range contexts {
		status := latest[c]
		switch status.State {
		case "success":
		case "pending":
			pending = true
		default:
			failed = true
			failedNames = append(failedNames, c+" ("+status.State+")")
		}
	}

	required, readable, err := g.requiredCheckContexts(ctx)
	if err != nil {
		return fmt.Errorf("could not read the required-check policy: %w", err)
	}
	reported := map[string]bool{}
	for _, run := range runs {
		reported[run.Name] = true
	}
	for c := range latest {
		reported[c] = true
	}
	var missing []string
	for _, c := range required {
		if !reported[c] {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("CI on %s is missing required checks (%s); refusing to continue", shortSHA(sha), strings.Join(missing, ", "))
	}

	total := len(runs) + len(latest)
	summary := fmt.Sprintf("%d check %s, %d commit %s", len(runs), plural(len(runs), "run", "runs"), len(latest), plural(len(latest), "status", "statuses"))
	if !readable {
		summary += "; required-check policy is not readable, so only reported results can be enforced"
	}
	switch {
	case total == 0:
		return fmt.Errorf("CI on %s is missing: no check runs or commit statuses reported; refusing to continue", shortSHA(sha))
	case failed:
		return fmt.Errorf("CI on %s failed (%s; %s); refusing to continue", shortSHA(sha), strings.Join(failedNames, ", "), summary)
	case pending:
		return fmt.Errorf("CI on %s is pending (%s); refusing to continue", shortSHA(sha), summary)
	case inconclusive:
		return fmt.Errorf("CI on %s is inconclusive (%s; %s); refusing to continue", shortSHA(sha), strings.Join(failedNames, ", "), summary)
	default:
		return nil
	}
}

// Validators for user input.

func validCommitSHA(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) != 40 && len(s) != 64 {
		return "", fmt.Errorf("commit %q must be a full 40- or 64-character SHA, not a branch or short name", s)
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return "", fmt.Errorf("commit %q must contain only hexadecimal characters", s)
		}
	}
	return strings.ToLower(s), nil
}

func validTagName(s string) error {
	if s == "" || s == "." || s == ".." || len(s) > 255 {
		return fmt.Errorf("invalid tag name %q", s)
	}
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "/") || strings.HasSuffix(s, "/") || strings.HasSuffix(s, ".") {
		return fmt.Errorf("invalid tag name %q", s)
	}
	if strings.Contains(s, "..") || strings.Contains(s, "//") || strings.Contains(s, "@{") {
		return fmt.Errorf("invalid tag name %q", s)
	}
	for _, part := range strings.Split(s, "/") {
		if strings.HasPrefix(part, ".") {
			return fmt.Errorf("invalid tag name %q: a tag part can't begin with a dot", s)
		}
		if strings.HasSuffix(part, ".lock") {
			return fmt.Errorf("invalid tag name %q: a tag part can't end in .lock", s)
		}
	}
	for _, r := range s {
		if r <= ' ' || r == 0x7f || strings.ContainsRune(`~^:?*[\]`, r) {
			return fmt.Errorf("invalid tag name %q", s)
		}
	}
	return nil
}

func validAssetName(s string) error {
	if s == "" || s == "." || s == ".." || len(s) > 255 {
		return fmt.Errorf("invalid asset name %q", s)
	}
	if strings.TrimSpace(s) != s || strings.HasPrefix(s, "-") {
		return fmt.Errorf("invalid asset name %q", s)
	}
	if strings.ContainsAny(s, `/\`) || strings.ContainsRune(s, 0) {
		return fmt.Errorf("asset name %q must be a plain file name without a path", s)
	}
	for _, r := range s {
		if r < ' ' || r == 0x7f {
			return fmt.Errorf("invalid asset name %q", s)
		}
	}
	return nil
}

var errNotFound = errors.New("not found")
