package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
)

var tagUsage = `usage: amf tag <command>

Commands:
  list [--limit n] [--json]  list tags, by name
  view <tag> [--json]  show a tag and the commit it peels to

Run amf help tag <command> or amf tag <command> --help for its flags.
` + "\nExamples:\n" + helpExamples["tag"] + "\nSee also: amf help release\n"

var tagSubcommands = []string{"list", "view", "help"}

func cmdTag(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, err := fmt.Fprint(stdout, tagUsage)
		return err
	}
	switch args[0] {
	case "help", "-h", "--help":
		_, err := fmt.Fprint(stdout, tagUsage)
		return err
	case "list":
		return cmdTagList(ctx, args[1:], stdout, stderr)
	case "view":
		return cmdTagView(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "amf: unknown tag command %q\n", args[0])
		if s := suggestFrom(args[0], tagSubcommands); s != "" {
			fmt.Fprintf(stderr, "Did you mean %q?\n", s)
		}
		fmt.Fprintln(stderr)
		fmt.Fprint(stderr, tagUsage)
		return errUsage
	}
}

var releaseUsage = `usage: amf release <command>

Commands:
  list [--state all|draft|published|prerelease] [--limit n] [--json]  list releases, newest first
  view <tag> [--json]  show a release
  latest [--json]  show the latest published, non-prerelease release
  create <tag> --commit sha  create a draft release pinned to an explicit commit
  stage <tag> --manifest file --asset path...  upload and verify assets in the draft
  verify <tag> --manifest file  verify the draft's assets without changing anything
  publish <tag> --manifest file  verify everything again, then publish the draft

Nothing except an explicit publish makes a release public.
` + "\nExamples:\n" + helpExamples["release"] + "\nSee also: amf help tag\n"

var releaseSubcommands = []string{"list", "view", "latest", "create", "stage", "verify", "publish", "help"}

func cmdRelease(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
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
	case "create":
		return cmdReleaseCreate(ctx, args[1:], stdin, stdout, stderr)
	case "stage":
		return cmdReleaseStage(ctx, args[1:], stdout, stderr)
	case "verify":
		return cmdReleaseVerify(ctx, args[1:], stdout, stderr)
	case "publish":
		return cmdReleasePublish(ctx, args[1:], stdout, stderr)
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

type tagListFlags struct {
	limit  *int
	asJSON *bool
}

func addTagListFlags(fs *flag.FlagSet) tagListFlags {
	return tagListFlags{
		limit:  fs.Int("limit", 0, "print at most N tags, 0 means no limit"),
		asJSON: fs.Bool("json", false, "print a JSON array of name and commit"),
	}
}

func cmdTagList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("tag list", stderr)
	f := addTagListFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: amf tag list [--limit n] [--json]")
		return errUsage
	}
	if *f.limit < 0 {
		return fmt.Errorf("--limit must be 0 or more, got %d", *f.limit)
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	tags, err := gh.ListTags(ctx)
	if err != nil {
		return err
	}
	matched := len(tags)
	if *f.limit > 0 && len(tags) > *f.limit {
		tags = tags[:*f.limit]
	}
	type row struct {
		Name   string `json:"name"`
		Commit string `json:"commit"`
	}
	out := make([]row, 0, len(tags))
	for _, t := range tags {
		out = append(out, row{Name: t.Name, Commit: t.Commit})
	}
	if *f.asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(out)
	}
	if len(out) == 0 {
		_, err := fmt.Fprintf(stdout, "No tags in %s.\n", cfg.Repo)
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TAG\tCOMMIT")
	for _, t := range out {
		fmt.Fprintf(tw, "%s\t%s\n", t.Name, t.Commit)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(tags) < matched {
		fmt.Fprintf(stderr, "Showing %d of %d tags; use --limit 0 to list them all.\n", len(tags), matched)
	}
	return nil
}

func addTagViewFlags(fs *flag.FlagSet) *bool {
	return fs.Bool("json", false, "print one JSON object")
}

func cmdTagView(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("tag view", stderr)
	asJSON := addTagViewFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: amf tag view <tag> [--json]")
		return errUsage
	}
	if err := validTagName(pos[0]); err != nil {
		return err
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	t, err := gh.GetTagPeeled(ctx, pos[0])
	if err != nil {
		if isStatus(err, 404) {
			return fmt.Errorf("tag %s was not found in %s", pos[0], cfg.Repo)
		}
		return err
	}
	if *asJSON {
		out := struct {
			Name   string `json:"name"`
			Type   string `json:"type"`
			SHA    string `json:"sha"`
			Commit string `json:"commit"`
			Depth  int    `json:"depth"`
		}{Name: t.Name, Type: tagType(t), SHA: t.ImmediateSHA, Commit: t.Commit, Depth: t.Depth}
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(out)
	}
	if _, err := fmt.Fprintf(stdout, "%s\n", t.Name); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%-8s %s\n", "type:", tagType(t)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "%-8s %s\n", "object:", t.ImmediateSHA); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "%-8s %s\n", "commit:", t.Commit)
	return err
}

func tagType(t peeledTag) string {
	if t.Annotated {
		if t.Depth == 1 {
			return "annotated"
		}
		return fmt.Sprintf("annotated (%d deep)", t.Depth)
	}
	return "lightweight"
}

type releaseListFlags struct {
	state  *string
	limit  *int
	asJSON *bool
}

func addReleaseListFlags(fs *flag.FlagSet) releaseListFlags {
	return releaseListFlags{
		state:  fs.String("state", "all", "all, draft, published or prerelease"),
		limit:  fs.Int("limit", 0, "print at most N releases, 0 means no limit"),
		asJSON: fs.Bool("json", false, "print a JSON array"),
	}
}

func cmdReleaseList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("release list", stderr)
	f := addReleaseListFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: amf release list [--state all|draft|published|prerelease] [--limit n] [--json]")
		return errUsage
	}
	if *f.limit < 0 {
		return fmt.Errorf("--limit must be 0 or more, got %d", *f.limit)
	}
	*f.state = strings.ToLower(strings.TrimSpace(*f.state))
	switch *f.state {
	case "all", "draft", "published", "prerelease":
	default:
		return fmt.Errorf("--state must be all, draft, published or prerelease, got %q", *f.state)
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	releases, err := gh.ListReleases(ctx)
	if err != nil {
		return err
	}
	if *f.state != "all" {
		releases = slices.DeleteFunc(releases, func(r ghRelease) bool { return r.state() != *f.state })
	}
	matched := len(releases)
	if *f.limit > 0 && len(releases) > *f.limit {
		releases = releases[:*f.limit]
	}
	if *f.asJSON {
		out := make([]listedRelease, 0, len(releases))
		for _, r := range releases {
			out = append(out, newListedRelease(r))
		}
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(out)
	}
	if len(releases) == 0 {
		which := "releases"
		if *f.state != "all" {
			which = *f.state + " releases"
		}
		_, err := fmt.Fprintf(stdout, "No %s in %s.\n", which, cfg.Repo)
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TAG\tSTATE\tTARGET\tNAME")
	for _, r := range releases {
		name := r.Name
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.TagName, r.state(), r.TargetCommitish, strings.ReplaceAll(name, "\t", " "))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(releases) < matched {
		fmt.Fprintf(stderr, "Showing %d of %d releases; use --limit 0 to list them all.\n", len(releases), matched)
	}
	return nil
}

type listedRelease struct {
	ID          int64  `json:"id"`
	Tag         string `json:"tag"`
	Name        string `json:"name"`
	State       string `json:"state"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	Target      string `json:"target"`
	URL         string `json:"url"`
	CreatedAt   string `json:"created_at"`
	PublishedAt string `json:"published_at"`
}

func newListedRelease(r ghRelease) listedRelease {
	return listedRelease{
		ID: r.ID, Tag: r.TagName, Name: r.Name, State: r.state(),
		Draft: r.Draft, Prerelease: r.Prerelease, Target: r.TargetCommitish,
		URL: r.HTMLURL, CreatedAt: r.CreatedAt, PublishedAt: r.PublishedAt,
	}
}

type viewedAsset struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	State string `json:"state"`
	URL   string `json:"url"`
}

type viewedRelease struct {
	listedRelease
	Body      string        `json:"body"`
	TagCommit string        `json:"tag_commit"`
	Assets    []viewedAsset `json:"assets"`
}

func newViewedRelease(r ghRelease, tagCommit string) viewedRelease {
	out := viewedRelease{listedRelease: newListedRelease(r), Body: r.Body, TagCommit: tagCommit}
	out.Assets = make([]viewedAsset, 0, len(r.Assets))
	for _, a := range r.Assets {
		out.Assets = append(out.Assets, viewedAsset{Name: a.Name, Size: a.Size, State: a.State, URL: a.BrowserDownloadURL})
	}
	return out
}

func addReleaseViewFlags(fs *flag.FlagSet) *bool {
	return fs.Bool("json", false, "print one JSON object")
}

func cmdReleaseView(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("release view", stderr)
	asJSON := addReleaseViewFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: amf release view <tag> [--json]")
		return errUsage
	}
	if err := validTagName(pos[0]); err != nil {
		return err
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	rel, err := gh.GetReleaseByTag(ctx, pos[0])
	if err != nil {
		if isStatus(err, 404) {
			return fmt.Errorf("release %s was not found in %s", pos[0], cfg.Repo)
		}
		return err
	}
	return printRelease(ctx, gh, rel, *asJSON, stdout)
}

func cmdReleaseLatest(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("release latest", stderr)
	asJSON := addReleaseViewFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
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
	gh := newGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	rel, err := gh.LatestRelease(ctx)
	if err != nil {
		if isStatus(err, 404) {
			return fmt.Errorf("no published release in %s", cfg.Repo)
		}
		return err
	}
	return printRelease(ctx, gh, rel, *asJSON, stdout)
}

func printRelease(ctx context.Context, gh *GitHub, rel ghRelease, asJSON bool, stdout io.Writer) error {
	tagCommit := "unknown"
	if t, err := gh.GetTagPeeled(ctx, rel.TagName); err == nil {
		tagCommit = t.Commit
	} else if !isStatus(err, 404) {
		return err
	}
	assets := rel.Assets
	if len(assets) == 0 && rel.ID != 0 {
		var err error
		if assets, err = gh.ListReleaseAssets(ctx, rel.ID); err != nil {
			return err
		}
	}
	rel.Assets = assets
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(newViewedRelease(rel, tagCommit))
	}
	name := rel.Name
	if name == "" {
		name = rel.TagName
	}
	if _, err := fmt.Fprintf(stdout, "%s %s\n", rel.TagName, name); err != nil {
		return err
	}
	rows := [][2]string{
		{"state:", rel.state()},
		{"target:", rel.TargetCommitish},
		{"tag:", tagCommit},
		{"url:", rel.HTMLURL},
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(stdout, "%-8s %s\n", row[0], row[1]); err != nil {
			return err
		}
	}
	if len(assets) == 0 {
		if _, err := fmt.Fprintln(stdout, "assets:  none"); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(stdout, "assets:  %d\n", len(assets)); err != nil {
			return err
		}
		for _, a := range assets {
			if _, err := fmt.Fprintf(stdout, "  %s  %d bytes  %s\n", a.Name, a.Size, a.State); err != nil {
				return err
			}
		}
	}
	if body := strings.TrimRight(rel.Body, "\n"); body != "" {
		_, err := fmt.Fprintf(stdout, "\n%s\n", body)
		return err
	}
	return nil
}

type releaseCreateFlags struct {
	commit     *string
	name       *string
	body       *string
	file       *string
	prerelease *bool
}

func addReleaseCreateFlags(fs *flag.FlagSet) releaseCreateFlags {
	return releaseCreateFlags{
		commit:     fs.String("commit", "", "full 40- or 64-character commit `sha` the tag must name"),
		name:       fs.String("name", "", "release `name` (default: the tag)"),
		body:       fs.String("body", "", "`text` of the release notes"),
		file:       fs.String("file", "", "read the release notes from the `path` (- for stdin)"),
		prerelease: fs.Bool("prerelease", false, "mark the draft as a prerelease"),
	}
}

func cmdReleaseCreate(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newCommandFlags("release create", stderr)
	f := addReleaseCreateFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: amf release create <tag> --commit sha [--name text] [--body text | --file path] [--prerelease]")
		return errUsage
	}
	tag := pos[0]
	if err := validTagName(tag); err != nil {
		return err
	}
	commit, err := validCommitSHA(*f.commit)
	if err != nil {
		return err
	}
	var bodySet, fileSet bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "body":
			bodySet = true
		case "file":
			fileSet = true
		}
	})
	if bodySet && fileSet {
		return errors.New("use --body or --file, not both")
	}
	body := *f.body
	switch {
	case *f.file == "-":
		body, err = readAll(stdin)
	case *f.file != "":
		var b []byte
		b, err = os.ReadFile(*f.file)
		body = string(b)
	}
	if err != nil {
		return err
	}
	name := strings.TrimSpace(*f.name)
	if name == "" {
		name = tag
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}

	peeled, err := gh.GetTagPeeled(ctx, tag)
	tagExists := err == nil
	if err != nil && !isStatus(err, 404) {
		return err
	}
	if tagExists && peeled.Commit != commit {
		return fmt.Errorf("tag %s already points to %s, not %s; refusing to move an existing tag", tag, peeled.Commit, commit)
	}
	if err := gh.requireCI(ctx, commit); err != nil {
		return err
	}
	if !tagExists {
		if err := gh.CreateTagRef(ctx, tag, commit); err != nil {
			// Another process may have created the same tag between the
			// read and the write. Re-read rather than assuming failure.
			again, readErr := gh.GetTagPeeled(ctx, tag)
			if readErr != nil {
				return err
			}
			if again.Commit != commit {
				return fmt.Errorf("tag %s was created at %s while amf tried to create it at %s; refusing to move it", tag, again.Commit, commit)
			}
		}
	}

	rel, err := gh.GetReleaseByTag(ctx, tag)
	switch {
	case err == nil && rel.Draft:
		_, err = fmt.Fprintf(stdout, "Draft release %s already exists %s\n", tag, rel.HTMLURL)
		return err
	case err == nil:
		return fmt.Errorf("release %s is already published at %s; refusing to modify it", tag, rel.HTMLURL)
	case !isStatus(err, 404):
		return err
	}
	created, err := gh.CreateDraftRelease(ctx, tag, commit, name, body, *f.prerelease)
	if err != nil {
		if now, readErr := gh.GetReleaseByTag(ctx, tag); readErr == nil {
			if now.Draft {
				_, err = fmt.Fprintf(stdout, "Draft release %s already exists %s\n", tag, now.HTMLURL)
				return err
			}
			return fmt.Errorf("release %s is already published at %s; refusing to modify it", tag, now.HTMLURL)
		}
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Created draft release %s %s\n", tag, created.HTMLURL); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Stage assets with: amf release stage %s --manifest <file> --asset <path>...\n", tag)
	return err
}

// releaseManifest is the expected asset set pinned to one tag and commit.
type releaseManifest struct {
	Tag    string          `json:"tag"`
	Commit string          `json:"commit"`
	Assets []manifestAsset `json:"assets"`
}

type manifestAsset struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   *int64 `json:"size,omitempty"`
}

func readReleaseManifest(path string) (releaseManifest, error) {
	var m releaseManifest
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if len(b) > 1<<20 {
		return m, fmt.Errorf("manifest %s is larger than 1 MiB", path)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("%s: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return m, fmt.Errorf("%s: unexpected data after the manifest object", path)
	}
	return m, nil
}

func validateReleaseManifest(m releaseManifest, tag string, requireAssets bool) (releaseManifest, error) {
	if err := validTagName(m.Tag); err != nil {
		return m, fmt.Errorf("manifest: %w", err)
	}
	if m.Tag != tag {
		return m, fmt.Errorf("manifest is for tag %s, not %s", m.Tag, tag)
	}
	commit, err := validCommitSHA(m.Commit)
	if err != nil {
		return m, fmt.Errorf("manifest: %w", err)
	}
	m.Commit = commit
	if requireAssets && len(m.Assets) == 0 {
		return m, errors.New("manifest has no assets")
	}
	names := map[string]bool{}
	for i := range m.Assets {
		a := &m.Assets[i]
		if err := validAssetName(a.Name); err != nil {
			return m, fmt.Errorf("manifest asset %d: %w", i+1, err)
		}
		key := strings.ToLower(a.Name)
		if names[key] {
			return m, fmt.Errorf("manifest lists asset %q more than once", a.Name)
		}
		names[key] = true
		sum := strings.TrimSpace(a.SHA256)
		if len(sum) != 64 {
			return m, fmt.Errorf("manifest asset %q has an invalid sha256 %q", a.Name, a.SHA256)
		}
		for _, r := range sum {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
				return m, fmt.Errorf("manifest asset %q has an invalid sha256 %q", a.Name, a.SHA256)
			}
		}
		a.SHA256 = strings.ToLower(sum)
		if a.Size != nil && *a.Size < 0 {
			return m, fmt.Errorf("manifest asset %q has a negative size", a.Name)
		}
	}
	return m, nil
}

// pathList is a repeatable flag that keeps each value whole, including
// spaces and commas.
type pathList []string

func (p *pathList) String() string { return strings.Join(*p, ",") }
func (p *pathList) Set(v string) error {
	*p = append(*p, v)
	return nil
}

type releaseStageFlags struct {
	manifest *string
	assets   pathList
}

func addReleaseStageFlags(fs *flag.FlagSet) *releaseStageFlags {
	f := &releaseStageFlags{manifest: fs.String("manifest", "", "read tag, commit and expected SHA-256 checksums from this `path`")}
	fs.Var(&f.assets, "asset", "local asset `path` (repeatable; its base name must match a manifest entry)")
	return f
}

func loadReleaseAssets(m releaseManifest, paths []string) (map[string][]byte, error) {
	if len(m.Assets) == 0 {
		return nil, errors.New("manifest has no assets")
	}
	if len(paths) == 0 {
		return nil, errors.New("provide at least one --asset")
	}
	entries := make(map[string]manifestAsset, len(m.Assets))
	for _, a := range m.Assets {
		entries[a.Name] = a
	}
	files := make(map[string][]byte, len(paths))
	for _, path := range paths {
		name := filepath.Base(path)
		entry, ok := entries[name]
		if !ok {
			return nil, fmt.Errorf("asset %s is not named in the manifest", path)
		}
		if _, dup := files[name]; dup {
			return nil, fmt.Errorf("asset %q was given more than once", name)
		}
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("asset %s is a symlink; only regular files can be staged", path)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("asset %s is not a regular file", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		got := hex.EncodeToString(sum[:])
		if !strings.EqualFold(got, entry.SHA256) {
			return nil, fmt.Errorf("asset %s has sha256 %s, want %s", path, got, entry.SHA256)
		}
		if entry.Size != nil && int64(len(data)) != *entry.Size {
			return nil, fmt.Errorf("asset %s is %d bytes, want %d", path, len(data), *entry.Size)
		}
		files[name] = data
	}
	for _, entry := range m.Assets {
		if _, ok := files[entry.Name]; !ok {
			return nil, fmt.Errorf("manifest asset %q has no --asset file", entry.Name)
		}
	}
	return files, nil
}

func cmdReleaseStage(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("release stage", stderr)
	f := addReleaseStageFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: amf release stage <tag> --manifest file --asset path...")
		return errUsage
	}
	tag := pos[0]
	if err := validTagName(tag); err != nil {
		return err
	}
	if *f.manifest == "" {
		return errors.New("--manifest is required")
	}
	m, err := readReleaseManifest(*f.manifest)
	if err != nil {
		return err
	}
	if m, err = validateReleaseManifest(m, tag, true); err != nil {
		return err
	}
	files, err := loadReleaseAssets(m, f.assets)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}

	rel, err := draftForUpdate(ctx, gh, tag, m.Commit)
	if err != nil {
		return err
	}
	if err := gh.requireCI(ctx, m.Commit); err != nil {
		return err
	}
	initial, err := gh.ListReleaseAssets(ctx, rel.ID)
	if err != nil {
		return err
	}
	wanted := make(map[string]bool, len(m.Assets))
	for _, a := range m.Assets {
		wanted[a.Name] = true
	}
	for _, a := range initial {
		if !wanted[a.Name] {
			return fmt.Errorf("release %s has asset %q that is not in the manifest; refusing to continue", tag, a.Name)
		}
	}

	uploaded, skipped := 0, 0
	for _, entry := range slices.SortedFunc(slices.Values(m.Assets), func(a, b manifestAsset) int {
		return strings.Compare(a.Name, b.Name)
	}) {
		remote, err := gh.ListReleaseAssets(ctx, rel.ID)
		if err != nil {
			return err
		}
		current, exists := assetByName(remote, entry.Name)
		if exists {
			if current.State != "" && current.State != "uploaded" {
				// An earlier upload stopped before completing. Re-read the
				// draft and delete only that same incomplete asset; a
				// different asset that appeared meanwhile is left alone.
				incompleteID := current.ID
				rel, err = draftForUpdate(ctx, gh, tag, m.Commit)
				if err != nil {
					return err
				}
				remote, err = gh.ListReleaseAssets(ctx, rel.ID)
				if err != nil {
					return err
				}
				current, exists = assetByName(remote, entry.Name)
				switch {
				case exists && current.ID == incompleteID && current.State != "" && current.State != "uploaded":
					if err := gh.DeleteReleaseAsset(ctx, current.ID); err != nil {
						return fmt.Errorf("could not remove incomplete asset %s: %w", entry.Name, err)
					}
					exists = false
				case exists && current.ID != incompleteID && current.State != "" && current.State != "uploaded":
					return fmt.Errorf("asset %s was replaced by another incomplete upload; refusing to delete it", entry.Name)
				}
			}
			if exists {
				if err := verifyOneAsset(ctx, gh, entry, current); err != nil {
					return err
				}
				skipped++
				continue
			}
		}
		if _, err := gh.UploadReleaseAsset(ctx, rel, entry.Name, files[entry.Name]); err != nil {
			if isStatus(err, 422) || isStatus(err, 409) {
				// A concurrent upload may have won the name race. Accept it
				// only when its content is exactly what this manifest pins.
				latest, readErr := gh.GetReleaseByTag(ctx, tag)
				if readErr != nil {
					return err
				}
				if !latest.Draft {
					return fmt.Errorf("release %s was published while assets were being staged", tag)
				}
				assets, listErr := gh.ListReleaseAssets(ctx, latest.ID)
				if listErr != nil {
					return listErr
				}
				raced, ok := assetByName(assets, entry.Name)
				if ok {
					if verifyErr := verifyOneAsset(ctx, gh, entry, raced); verifyErr == nil {
						skipped++
						rel = latest
						continue
					}
				}
				return fmt.Errorf("asset %s already exists in draft release %s with different content; refusing to overwrite or delete it", entry.Name, tag)
			}
			return err
		}
		uploaded++
	}

	if _, err := verifyReleaseAssets(ctx, gh, rel, m); err != nil {
		return err
	}
	if err := recheckTag(ctx, gh, tag, m.Commit); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "Staged %d and skipped %d identical %s in draft release %s; not published.\n",
		uploaded, skipped, plural(uploaded+skipped, "asset", "assets"), tag); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Publish with: amf release publish %s --manifest %s\n", tag, *f.manifest)
	return err
}

func assetByName(assets []releaseAsset, name string) (releaseAsset, bool) {
	for _, a := range assets {
		if a.Name == name {
			return a, true
		}
	}
	return releaseAsset{}, false
}

func draftForUpdate(ctx context.Context, gh *GitHub, tag, commit string) (ghRelease, error) {
	peeled, err := gh.GetTagPeeled(ctx, tag)
	if err != nil {
		if isStatus(err, 404) {
			return ghRelease{}, fmt.Errorf("tag %s does not exist; run amf release create first", tag)
		}
		return ghRelease{}, err
	}
	if peeled.Commit != commit {
		return ghRelease{}, fmt.Errorf("tag %s now points to %s, not the pinned %s; refusing to continue", tag, peeled.Commit, commit)
	}
	rel, err := gh.GetReleaseByTag(ctx, tag)
	if err != nil {
		if isStatus(err, 404) {
			return ghRelease{}, fmt.Errorf("release %s does not exist; run amf release create first", tag)
		}
		return ghRelease{}, err
	}
	if !rel.Draft {
		return ghRelease{}, fmt.Errorf("release %s is already published; refusing to modify it", tag)
	}
	return rel, nil
}

func verifyOneAsset(ctx context.Context, gh *GitHub, entry manifestAsset, asset releaseAsset) error {
	if asset.State != "" && asset.State != "uploaded" {
		return fmt.Errorf("asset %s is not fully uploaded (state %s)", entry.Name, asset.State)
	}
	sum, size, err := gh.DownloadReleaseAssetChecksum(ctx, asset)
	if err != nil {
		return fmt.Errorf("could not verify asset %s: %w", entry.Name, err)
	}
	if asset.Size > 0 && size != asset.Size {
		return fmt.Errorf("asset %s is %d bytes on GitHub but was listed as %d", entry.Name, size, asset.Size)
	}
	if entry.Size != nil && size != *entry.Size {
		return fmt.Errorf("asset %s is %d bytes, want %d", entry.Name, size, *entry.Size)
	}
	if !strings.EqualFold(sum, entry.SHA256) {
		return fmt.Errorf("asset %s has sha256 %s on GitHub, want %s", entry.Name, sum, entry.SHA256)
	}
	return nil
}

func verifyReleaseAssets(ctx context.Context, gh *GitHub, rel ghRelease, m releaseManifest) (map[string]releaseAsset, error) {
	assets, err := gh.ListReleaseAssets(ctx, rel.ID)
	if err != nil {
		return nil, err
	}
	expected := make(map[string]manifestAsset, len(m.Assets))
	for _, a := range m.Assets {
		expected[a.Name] = a
	}
	for _, a := range assets {
		if _, ok := expected[a.Name]; !ok {
			return nil, fmt.Errorf("release %s has asset %q that is not in the manifest; refusing to continue", rel.TagName, a.Name)
		}
	}
	out := make(map[string]releaseAsset, len(m.Assets))
	for _, entry := range m.Assets {
		asset, ok := assetByName(assets, entry.Name)
		if !ok {
			return nil, fmt.Errorf("release %s is missing asset %s", rel.TagName, entry.Name)
		}
		if err := verifyOneAsset(ctx, gh, entry, asset); err != nil {
			return nil, err
		}
		out[entry.Name] = asset
	}
	return out, nil
}

func cmdReleaseVerify(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("release verify", stderr)
	manifest := fs.String("manifest", "", "read tag, commit and expected SHA-256 checksums from this `path`")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: amf release verify <tag> --manifest file")
		return errUsage
	}
	tag := pos[0]
	if err := validTagName(tag); err != nil {
		return err
	}
	if *manifest == "" {
		return errors.New("--manifest is required")
	}
	m, err := readReleaseManifest(*manifest)
	if err != nil {
		return err
	}
	if m, err = validateReleaseManifest(m, tag, true); err != nil {
		return err
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	peeled, err := gh.GetTagPeeled(ctx, tag)
	if err != nil {
		if isStatus(err, 404) {
			return fmt.Errorf("tag %s does not exist", tag)
		}
		return err
	}
	if peeled.Commit != m.Commit {
		return fmt.Errorf("tag %s now points to %s, not the pinned %s", tag, peeled.Commit, m.Commit)
	}
	rel, err := gh.GetReleaseByTag(ctx, tag)
	if err != nil {
		if isStatus(err, 404) {
			return fmt.Errorf("release %s does not exist; run amf release create first", tag)
		}
		return err
	}
	if _, err := verifyReleaseAssets(ctx, gh, rel, m); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Verified %d %s in %s release %s; nothing was changed or published.\n",
		len(m.Assets), plural(len(m.Assets), "asset", "assets"), rel.state(), tag)
	return err
}

func cmdReleasePublish(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("release publish", stderr)
	manifest := fs.String("manifest", "", "read tag, commit and expected SHA-256 checksums from this `path`")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: amf release publish <tag> --manifest file")
		return errUsage
	}
	tag := pos[0]
	if err := validTagName(tag); err != nil {
		return err
	}
	if *manifest == "" {
		return errors.New("--manifest is required")
	}
	m, err := readReleaseManifest(*manifest)
	if err != nil {
		return err
	}
	if m, err = validateReleaseManifest(m, tag, true); err != nil {
		return err
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	peeled, err := gh.GetTagPeeled(ctx, tag)
	if err != nil {
		if isStatus(err, 404) {
			return fmt.Errorf("tag %s does not exist; run amf release create first", tag)
		}
		return err
	}
	if peeled.Commit != m.Commit {
		return fmt.Errorf("tag %s points to %s, not the pinned %s; refusing to publish", tag, peeled.Commit, m.Commit)
	}
	rel, err := gh.GetReleaseByTag(ctx, tag)
	if err != nil {
		if isStatus(err, 404) {
			return fmt.Errorf("release %s does not exist; run amf release create first", tag)
		}
		return err
	}
	if err := gh.requireCI(ctx, m.Commit); err != nil {
		return err
	}

	id := rel.ID
	if !rel.Draft {
		if err := recheckTag(ctx, gh, tag, m.Commit); err != nil {
			return err
		}
		if _, err := verifyReleaseAssets(ctx, gh, rel, m); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Release %s is already published %s\n", tag, rel.HTMLURL)
		return err
	}

	before, err := verifyReleaseAssets(ctx, gh, rel, m)
	if err != nil {
		return err
	}
	// Re-read the pin after verification, then compare the exact asset
	// identities GitHub exposes before flipping the draft live.
	if err := recheckTag(ctx, gh, tag, m.Commit); err != nil {
		return err
	}
	current, err := gh.GetReleaseByTag(ctx, tag)
	if err != nil {
		return err
	}
	if current.ID != id {
		return fmt.Errorf("release %s was replaced while publishing; refusing to continue", tag)
	}
	if !current.Draft {
		if _, err := verifyReleaseAssets(ctx, gh, current, m); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Release %s is already published %s\n", tag, current.HTMLURL)
		return err
	}
	afterAssets, err := gh.ListReleaseAssets(ctx, id)
	if err != nil {
		return err
	}
	after := make(map[string]releaseAsset, len(afterAssets))
	for _, a := range afterAssets {
		after[a.Name] = a
	}
	if len(before) != len(after) {
		return fmt.Errorf("assets changed while release %s was being verified; refusing to publish", tag)
	}
	for name, old := range before {
		now, ok := after[name]
		if !ok || now.ID != old.ID || now.Size != old.Size || now.State != old.State {
			return fmt.Errorf("asset %s changed while release %s was being verified; refusing to publish", name, tag)
		}
	}
	if err := recheckTag(ctx, gh, tag, m.Commit); err != nil {
		return err
	}
	current, err = gh.GetReleaseByTag(ctx, tag)
	if err != nil {
		return err
	}
	if current.ID != id || !current.Draft {
		return fmt.Errorf("release %s changed before it could be published; refusing to continue", tag)
	}
	published, err := gh.PublishRelease(ctx, id)
	if err != nil {
		// A concurrent retry may have published this same release after
		// our last read. Treat that as success only after full verification.
		if now, readErr := gh.GetReleaseByTag(ctx, tag); readErr == nil && !now.Draft {
			if _, verifyErr := verifyReleaseAssets(ctx, gh, now, m); verifyErr != nil {
				return err
			}
			_, outErr := fmt.Fprintf(stdout, "Release %s is already published %s\n", tag, now.HTMLURL)
			return outErr
		}
		return err
	}
	url := published.HTMLURL
	if url == "" {
		url = rel.HTMLURL
	}
	_, err = fmt.Fprintf(stdout, "Published release %s %s\n", tag, url)
	return err
}

func recheckTag(ctx context.Context, gh *GitHub, tag, commit string) error {
	peeled, err := gh.GetTagPeeled(ctx, tag)
	if err != nil {
		return fmt.Errorf("could not re-read tag %s: %w", tag, err)
	}
	if peeled.Commit != commit {
		return fmt.Errorf("tag %s moved to %s, not the pinned %s; refusing to publish", tag, peeled.Commit, commit)
	}
	return nil
}
