package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
)

var releaseUsage = `usage: amf release <command>

Commands:
  list [--state all|published|draft|prerelease] [--limit n] [--json]  list releases, newest first
  view <tag> [--json]  show a release, its assets and its tag's actual commit
  latest [--json]  show GitHub's latest published non-prerelease release

Releases are read-only in amf: it cannot create, stage, upload, publish or
delete releases or assets.
Run amf help release <command> or amf release <command> --help for its flags.
` + "\nExamples:\n" + helpExamples["release"] + "\nSee also: amf help tag, amf help flags\n"

var releaseSubcommands = []string{"list", "view", "latest", "help"}

func cmdRelease(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, err := fmt.Fprint(stdout, releaseUsage)
		return err
	}
	switch args[0] {
	case "help", "-h", "--help":
		_, err := fmt.Fprint(stdout, releaseUsage)
		return err
	case "list":
		return cmdReleaseList(ctx, args[1:], stdout, stderr)
	case "view":
		return cmdReleaseView(ctx, args[1:], stdout, stderr)
	case "latest":
		return cmdReleaseLatest(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "amf: unknown release command %q\n", args[0])
		if s := suggestFrom(args[0], releaseSubcommands); s != "" {
			fmt.Fprintf(stderr, "Did you mean %q?\n", s)
		}
		fmt.Fprintln(stderr)
		fmt.Fprint(stderr, releaseUsage)
		return errUsage
	}
}

// Release is one GitHub release. TargetCommitish is what GitHub recorded when
// the release was created; it is not proof of the tag's actual ref.
type Release struct {
	ID              int64          `json:"id"`
	TagName         string         `json:"tag_name"`
	TargetCommitish string         `json:"target_commitish"`
	Name            string         `json:"name"`
	Body            string         `json:"body"`
	Draft           bool           `json:"draft"`
	Prerelease      bool           `json:"prerelease"`
	CreatedAt       time.Time      `json:"created_at"`
	PublishedAt     *time.Time     `json:"published_at"`
	HTMLURL         string         `json:"html_url"`
	Assets          []ReleaseAsset `json:"assets"`
}

// ReleaseAsset is one release asset, as GitHub returns it.
type ReleaseAsset struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	ContentType        string `json:"content_type"`
	State              string `json:"state"`
	DownloadCount      int    `json:"download_count"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// ListReleases fetches every page of the repository's releases.
func (g *GitHub) ListReleases(ctx context.Context) ([]Release, error) {
	endpoint := g.repoPath("/releases")
	path := endpoint + "?per_page=100"
	seen := map[string]bool{path: true}
	var releases []Release
	for path != "" {
		var page []Release
		header, err := g.send(ctx, "GET", path, nil, &page)
		if err != nil {
			return nil, err
		}
		releases = append(releases, page...)
		if path, err = g.nextPageScoped(header, endpoint, seen); err != nil {
			return nil, err
		}
	}
	return releases, nil
}

// GetRelease fetches the release for a tag.
func (g *GitHub) GetRelease(ctx context.Context, tag string) (Release, error) {
	var rel Release
	err := g.do(ctx, "GET", g.repoPath("/releases/tags/%s", url.PathEscape(tag)), nil, &rel)
	// GitHub documents the by-tag endpoint for published releases. Drafts
	// visible to these same credentials must be found in the paginated list;
	// do not assume a 404 proves no draft exists or broaden authentication.
	if isStatus(err, http.StatusNotFound) {
		releases, listErr := g.ListReleases(ctx)
		if listErr != nil {
			return Release{}, listErr
		}
		found := false
		for _, candidate := range releases {
			if candidate.TagName != tag {
				continue
			}
			if found {
				return Release{}, fmt.Errorf("multiple visible releases have tag %q; refusing an ambiguous view", tag)
			}
			rel, found = candidate, true
		}
		if found {
			return rel, nil
		}
	}
	if err == nil && rel.TagName != tag {
		return Release{}, fmt.Errorf("GitHub returned release tag %q for %q", rel.TagName, tag)
	}
	return rel, err
}

// LatestRelease fetches GitHub's latest published non-prerelease release.
// It reports false, without an error, when the repository has none.
func (g *GitHub) LatestRelease(ctx context.Context) (Release, bool, error) {
	var rel Release
	err := g.do(ctx, "GET", g.repoPath("/releases/latest"), nil, &rel)
	if isStatus(err, http.StatusNotFound) {
		return Release{}, false, nil
	}
	return rel, err == nil, err
}

// ReleaseAssets fetches every page of a release's assets. The inline asset
// array in a release response is not trusted to be complete.
func (g *GitHub) ReleaseAssets(ctx context.Context, id int64) ([]ReleaseAsset, error) {
	endpoint := g.repoPath("/releases/%d/assets", id)
	path := endpoint + "?per_page=100"
	seen := map[string]bool{path: true}
	var assets []ReleaseAsset
	for path != "" {
		var page []ReleaseAsset
		header, err := g.send(ctx, "GET", path, nil, &page)
		if err != nil {
			return nil, err
		}
		assets = append(assets, page...)
		if path, err = g.nextPageScoped(header, endpoint, seen); err != nil {
			return nil, err
		}
	}
	return assets, nil
}

func releaseState(rel Release) string {
	switch {
	case rel.Draft:
		return "draft"
	case rel.Prerelease:
		return "prerelease"
	default:
		return "published"
	}
}

func releaseMatchesState(rel Release, state string) bool {
	switch state {
	case "all":
		return true
	case "published":
		return !rel.Draft && !rel.Prerelease
	case "draft":
		return rel.Draft
	case "prerelease":
		return rel.Prerelease
	}
	return false
}

// releaseCommit resolves a release tag to its actually peeled commit. A draft
// whose tag does not exist yet is explicitly unknown; a published release
// with a missing tag is an error.
func releaseCommit(ctx context.Context, gh *GitHub, rel Release) (*string, error) {
	if rel.TagName == "" {
		if rel.Draft {
			return nil, nil
		}
		return nil, fmt.Errorf("release %q has no tag name", rel.HTMLURL)
	}
	sha, err := gh.TagCommit(ctx, rel.TagName)
	if isStatus(err, http.StatusNotFound) {
		if rel.Draft {
			return nil, nil
		}
		return nil, fmt.Errorf("tag %q for release %q was not found in %s", rel.TagName, displayReleaseName(rel), gh.repo)
	}
	if err != nil {
		return nil, err
	}
	return &sha, nil
}

func displayReleaseName(rel Release) string {
	if rel.Name != "" {
		return rel.Name
	}
	return rel.TagName
}

type releaseAssetJSON struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	State       string `json:"state"`
	Downloads   int    `json:"download_count"`
	URL         string `json:"url"`
}

type listedRelease struct {
	ID              int64              `json:"id"`
	Tag             string             `json:"tag"`
	Name            string             `json:"name"`
	State           string             `json:"state"`
	Draft           bool               `json:"draft"`
	Prerelease      bool               `json:"prerelease"`
	TargetCommitish string             `json:"target_commitish"`
	CreatedAt       string             `json:"created_at"`
	PublishedAt     *string            `json:"published_at"`
	URL             string             `json:"url"`
	Assets          []releaseAssetJSON `json:"assets"`
}

type viewedRelease struct {
	listedRelease
	Commit *string `json:"commit"`
}

func releaseAssetsJSON(assets []ReleaseAsset) []releaseAssetJSON {
	out := make([]releaseAssetJSON, 0, len(assets))
	for _, a := range assets {
		out = append(out, releaseAssetJSON{ID: a.ID, Name: a.Name, Size: a.Size, ContentType: a.ContentType, State: a.State, Downloads: a.DownloadCount, URL: a.BrowserDownloadURL})
	}
	return out
}

func releaseJSON(rel Release, assets []ReleaseAsset) listedRelease {
	var published *string
	if rel.PublishedAt != nil {
		s := rel.PublishedAt.Format(time.RFC3339)
		published = &s
	}
	return listedRelease{
		ID: rel.ID, Tag: rel.TagName, Name: displayReleaseName(rel), State: releaseState(rel),
		Draft: rel.Draft, Prerelease: rel.Prerelease, TargetCommitish: rel.TargetCommitish,
		CreatedAt: rel.CreatedAt.Format(time.RFC3339), PublishedAt: published, URL: rel.HTMLURL,
		Assets: releaseAssetsJSON(assets),
	}
}

func printRelease(stdout io.Writer, rel Release, commit *string, assets []ReleaseAsset) error {
	title := rel.TagName
	if name := displayReleaseName(rel); name != rel.TagName {
		title += " " + name
	}
	if _, err := fmt.Fprintln(stdout, title); err != nil {
		return err
	}
	commitText := "unknown"
	if commit != nil {
		commitText = *commit
	}
	target := rel.TargetCommitish
	if target == "" {
		target = "-"
	}
	published := "-"
	if rel.PublishedAt != nil {
		published = rel.PublishedAt.Format(time.RFC3339)
	}
	if _, err := fmt.Fprintf(stdout, "%-11s %s\n%-11s %s\n%-11s %s\n%-11s %s\n%-11s %s\n%-11s %s\n%-11s %s\n%-11s %d\n",
		"state:", releaseState(rel),
		"tag:", rel.TagName,
		"commit:", commitText,
		"target:", target,
		"created:", rel.CreatedAt.Format(time.RFC3339),
		"published:", published,
		"url:", rel.HTMLURL,
		"assets:", len(assets),
	); err != nil {
		return err
	}
	for _, a := range assets {
		if _, err := fmt.Fprintf(stdout, "  %s (%d bytes, %d downloads)\n", a.Name, a.Size, a.DownloadCount); err != nil {
			return err
		}
	}
	return nil
}

type releaseListFlags struct {
	state  *string
	limit  *int
	asJSON *bool
}

func addReleaseListFlags(fs *flag.FlagSet) releaseListFlags {
	return releaseListFlags{
		state:  fs.String("state", "all", "all, published, draft or prerelease"),
		limit:  fs.Int("limit", 0, "print at most N releases, 0 means no limit"),
		asJSON: fs.Bool("json", false, "print a JSON array of tag, name, state, target_commitish, dates, url and assets"),
	}
}

func cmdReleaseList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("release list", stderr)
	f := addReleaseListFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return errUsage
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: amf release list [--state all|published|draft|prerelease] [--limit n] [--json]")
		return errUsage
	}
	if *f.limit < 0 {
		return fmt.Errorf("--limit must be 0 or more, got %d", *f.limit)
	}
	*f.state = strings.ToLower(strings.TrimSpace(*f.state))
	switch *f.state {
	case "all", "published", "draft", "prerelease":
	default:
		return fmt.Errorf("--state must be all, published, draft or prerelease, got %q", *f.state)
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newTagReleaseGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	releases, err := gh.ListReleases(ctx)
	if err != nil {
		return err
	}
	slices.SortStableFunc(releases, func(a, b Release) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	releases = slices.DeleteFunc(releases, func(rel Release) bool { return !releaseMatchesState(rel, *f.state) })
	matched := len(releases)
	if *f.limit > 0 && len(releases) > *f.limit {
		releases = releases[:*f.limit]
	}

	type row struct {
		rel    Release
		assets []ReleaseAsset
	}
	rows := make([]row, 0, len(releases))
	for _, rel := range releases {
		assets, err := gh.ReleaseAssets(ctx, rel.ID)
		if err != nil {
			return err
		}
		rows = append(rows, row{rel: rel, assets: assets})
	}

	if *f.asJSON {
		out := make([]listedRelease, 0, len(rows))
		for _, r := range rows {
			out = append(out, releaseJSON(r.rel, r.assets))
		}
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(out)
	}
	if len(rows) == 0 {
		which := "releases"
		if *f.state != "all" {
			which = *f.state + " releases"
		}
		_, err := fmt.Fprintf(stdout, "No %s in %s.\n", which, cfg.Repo)
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TAG\tSTATE\tASSETS\tNAME")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\n", strings.ReplaceAll(r.rel.TagName, "\t", " "), releaseState(r.rel), len(r.assets), strings.ReplaceAll(displayReleaseName(r.rel), "\t", " "))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(rows) < matched {
		fmt.Fprintf(stderr, "Showing %d of %d releases; use --limit 0 to list them all.\n", len(rows), matched)
	}
	return nil
}

func addReleaseViewFlags(fs *flag.FlagSet) *bool {
	return fs.Bool("json", false, "print one JSON object")
}

func cmdReleaseView(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("release view", stderr)
	asJSON := addReleaseViewFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return errUsage
	}
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
		fmt.Fprintln(stderr, "usage: amf release view <tag> [--json]")
		return errUsage
	}
	tag := strings.TrimSpace(pos[0])
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newTagReleaseGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	rel, err := gh.GetRelease(ctx, tag)
	if isStatus(err, http.StatusNotFound) {
		return fmt.Errorf("release for tag %q was not found in %s", tag, cfg.Repo)
	}
	if err != nil {
		return err
	}
	return printOneRelease(ctx, gh, rel, stdout, *asJSON)
}

func cmdReleaseLatest(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("release latest", stderr)
	asJSON := addReleaseViewFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return errUsage
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: amf release latest [--json]")
		return errUsage
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newTagReleaseGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	rel, ok, err := gh.LatestRelease(ctx)
	if err != nil {
		return err
	}
	if !ok {
		if *asJSON {
			enc := json.NewEncoder(stdout)
			enc.SetEscapeHTML(false)
			return enc.Encode(nil)
		}
		_, err := fmt.Fprintf(stdout, "No published releases in %s.\n", cfg.Repo)
		return err
	}
	return printOneRelease(ctx, gh, rel, stdout, *asJSON)
}

func printOneRelease(ctx context.Context, gh *GitHub, rel Release, stdout io.Writer, asJSON bool) error {
	assets, err := gh.ReleaseAssets(ctx, rel.ID)
	if err != nil {
		return err
	}
	commit, err := releaseCommit(ctx, gh, rel)
	if err != nil {
		return err
	}
	if asJSON {
		out := viewedRelease{listedRelease: releaseJSON(rel, assets), Commit: commit}
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(out)
	}
	return printRelease(stdout, rel, commit, assets)
}
