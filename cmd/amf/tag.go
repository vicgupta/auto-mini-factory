package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
)

var tagUsage = `usage: amf tag <command>

Commands:
  list [--limit n] [--json]  list tags by name, with their commits
  view <name> [--json]  show a tag's recursively peeled commit
  create <name> --commit <sha>  create a tag only at a full commit SHA

Tags are created once at an immutable commit; amf never moves or deletes
one. Creating a tag that already peels to the same commit succeeds without
a write, and one that peels elsewhere fails.
Run amf help tag <command> or amf tag <command> --help for its flags.
` + "\nExamples:\n" + helpExamples["tag"] + "\nSee also: amf help release, amf help flags\n"

var tagSubcommands = []string{"list", "view", "create", "help"}

// Use the existing sanctioned API client, but refuse all redirects for tag and
// release commands (including the preliminary repository check). Go's default
// redirect policy can forward Authorization across ports or to subdomains;
// these commands must never send credentials to a redirected target.
func newTagReleaseGitHub(cfg Config) *GitHub {
	g := newGitHub(cfg)
	g.http.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return g
}

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
	case "create":
		return cmdTagCreate(ctx, args[1:], stdout, stderr)
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

// Tag is one entry of GitHub's repository tag listing.
type Tag struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

// tagRef is GET /git/ref/tags/{name}. A lightweight tag points straight at a
// commit; an annotated tag points at a tag object.
type tagRef struct {
	Ref    string `json:"ref"`
	Object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}

// tagObject is GET /git/tags/{sha}, an annotated tag object.
type tagObject struct {
	SHA    string `json:"sha"`
	Tag    string `json:"tag"`
	Object struct {
		Type string `json:"type"`
		SHA  string `json:"sha"`
	} `json:"object"`
}

// maxTagDepth bounds how many annotated tag objects amf follows before it
// rejects a tag as too deeply nested.
const maxTagDepth = 16

// ListTags fetches every page of the repository's tags.
func (g *GitHub) ListTags(ctx context.Context) ([]Tag, error) {
	endpoint := g.repoPath("/tags")
	path := endpoint + "?per_page=100"
	seen := map[string]bool{path: true}
	var tags []Tag
	for path != "" {
		var page []Tag
		header, err := g.send(ctx, "GET", path, nil, &page)
		if err != nil {
			return nil, err
		}
		tags = append(tags, page...)
		if path, err = g.nextPageScoped(header, endpoint, seen); err != nil {
			return nil, err
		}
	}
	return tags, nil
}

// TagCommit returns the commit a tag ultimately points to, peeling nested
// annotated tag objects. It rejects incomplete responses, non-commit
// targets, cycles, excessive nesting and responses that identify a different
// ref or tag object. A missing tag is the API's 404.
func (g *GitHub) TagCommit(ctx context.Context, name string) (string, error) {
	var ref tagRef
	if err := g.do(ctx, "GET", g.repoPath("/git/ref/tags/%s", url.PathEscape(name)), nil, &ref); err != nil {
		return "", err
	}
	if want := "refs/tags/" + name; ref.Ref != want {
		return "", fmt.Errorf("GitHub returned tag ref %q for %q", ref.Ref, name)
	}
	if ref.Object.Type == "" || ref.Object.SHA == "" {
		return "", fmt.Errorf("GitHub returned an incomplete tag response for %q", name)
	}

	seen := map[string]bool{}
	typ, sha := ref.Object.Type, ref.Object.SHA
	for depth := 0; ; {
		switch typ {
		case "commit":
			return sha, nil
		case "tag":
			if seen[sha] {
				return "", fmt.Errorf("tag %q has a cycle at tag object %s", name, sha)
			}
			if depth >= maxTagDepth {
				return "", fmt.Errorf("tag %q is nested more than %d tag objects deep", name, maxTagDepth)
			}
			seen[sha] = true
			var obj tagObject
			if err := g.do(ctx, "GET", g.repoPath("/git/tags/%s", url.PathEscape(sha)), nil, &obj); err != nil {
				return "", err
			}
			if obj.SHA == "" || obj.Tag == "" || obj.Object.Type == "" || obj.Object.SHA == "" {
				return "", fmt.Errorf("GitHub returned an incomplete tag object for %q", name)
			}
			if obj.SHA != sha {
				return "", fmt.Errorf("GitHub returned tag object %q for %q", obj.SHA, sha)
			}
			// A ref can alias an existing annotated object whose embedded tag
			// name differs. Its object SHA, not that historical name, is the
			// identity that must match the requested object.
			depth++
			typ, sha = obj.Object.Type, obj.Object.SHA
		default:
			return "", fmt.Errorf("tag %q points to a %s object, not a commit", name, typ)
		}
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

type listedTag struct {
	Name   string `json:"name"`
	Commit string `json:"commit"`
}

func cmdTagList(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("tag list", stderr)
	f := addTagListFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
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
	gh := newTagReleaseGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	tags, err := gh.ListTags(ctx)
	if err != nil {
		return err
	}
	slices.SortFunc(tags, func(a, b Tag) int { return strings.Compare(a.Name, b.Name) })
	matched := len(tags)
	if *f.limit > 0 && len(tags) > *f.limit {
		tags = tags[:*f.limit]
	}
	if *f.asJSON {
		out := make([]listedTag, 0, len(tags))
		for _, t := range tags {
			out = append(out, listedTag{Name: t.Name, Commit: t.Commit.SHA})
		}
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(out)
	}
	if len(tags) == 0 {
		_, err := fmt.Fprintf(stdout, "No tags in %s.\n", cfg.Repo)
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TAG\tCOMMIT")
	for _, t := range tags {
		fmt.Fprintf(tw, "%s\t%s\n", strings.ReplaceAll(t.Name, "\t", " "), t.Commit.SHA)
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

type viewedTag struct {
	Name   string `json:"name"`
	Commit string `json:"commit"`
}

func cmdTagView(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("tag view", stderr)
	asJSON := addTagViewFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return errUsage
	}
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
		fmt.Fprintln(stderr, "usage: amf tag view <name> [--json]")
		return errUsage
	}
	name := strings.TrimSpace(pos[0])
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newTagReleaseGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	commit, err := gh.TagCommit(ctx, name)
	if isStatus(err, http.StatusNotFound) {
		return fmt.Errorf("tag %q was not found in %s", name, cfg.Repo)
	}
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(viewedTag{Name: name, Commit: commit})
	}
	_, err = fmt.Fprintf(stdout, "%-8s %s\n%-8s %s\n", "tag:", name, "commit:", commit)
	return err
}

var fullCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}(?:[0-9a-f]{24})?$`)

// validateFullCommitSHA accepts only a complete, lower-case SHA. A branch or
// short SHA would let the target move after the check, which the release
// workflow must never allow.
func validateFullCommitSHA(sha string) (string, error) {
	sha = strings.TrimSpace(sha)
	if !fullCommitSHA.MatchString(sha) {
		return "", fmt.Errorf("--commit must be a full 40- or 64-character lower-case Git commit SHA, got %q", sha)
	}
	return sha, nil
}

// validateTagName applies git's ref-name rules for a tag, so amf never asks
// GitHub to create an ambiguous or malformed ref.
func validateTagName(name string) error {
	if name == "" {
		return errors.New("tag name can't be empty")
	}
	if name == "@" || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return fmt.Errorf("invalid tag name %q", name)
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock") || strings.Contains(name, "..") || strings.Contains(name, "@{") {
		return fmt.Errorf("invalid tag name %q", name)
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f || strings.ContainsRune("~^:?*[\\", r) {
			return fmt.Errorf("invalid tag name %q", name)
		}
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return fmt.Errorf("invalid tag name %q", name)
		}
	}
	return nil
}

// CommitExists checks that sha names a commit object in the repository, not
// a tag, tree or blob. Tag creation stays at an immutable commit.
func (g *GitHub) CommitExists(ctx context.Context, sha string) error {
	var out struct {
		SHA string `json:"sha"`
	}
	if err := g.do(ctx, "GET", g.repoPath("/git/commits/%s", url.PathEscape(sha)), nil, &out); err != nil {
		if isStatus(err, http.StatusNotFound) {
			return fmt.Errorf("commit %s was not found in %s", sha, g.repo)
		}
		return err
	}
	if out.SHA != sha {
		return fmt.Errorf("GitHub returned commit %q for %q", out.SHA, sha)
	}
	return nil
}

type tagCreateFlags struct {
	commit *string
}

func addTagCreateFlags(fs *flag.FlagSet) tagCreateFlags {
	return tagCreateFlags{commit: fs.String("commit", "", "full 40- or 64-character commit `sha` the tag must point to")}
}

func cmdTagCreate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("tag create", stderr)
	f := addTagCreateFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return errUsage
	}
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
		fmt.Fprintln(stderr, "usage: amf tag create <name> --commit <sha>")
		return errUsage
	}
	name := strings.TrimSpace(pos[0])
	if err := validateTagName(name); err != nil {
		return err
	}
	sha, err := validateFullCommitSHA(*f.commit)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newTagReleaseGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	if err := gh.CommitExists(ctx, sha); err != nil {
		return err
	}
	peeled, err := gh.TagCommit(ctx, name)
	switch {
	case err == nil:
		if peeled != sha {
			return fmt.Errorf("tag %q already points to commit %s, not %s; amf never moves a tag", name, peeled, sha)
		}
		_, err = fmt.Fprintf(stdout, "Tag %s already points to %s.\n", name, sha)
		return err
	case !isStatus(err, http.StatusNotFound):
		return err
	}
	policy, err := fetchReleasePolicy(ctx, gh, defaultReleasePolicyPath, sha)
	if err != nil {
		return err
	}
	if err := requireReleaseCI(ctx, gh, policy, sha); err != nil {
		return err
	}
	in := map[string]string{"ref": "refs/tags/" + name, "sha": sha}
	if err := gh.do(ctx, "POST", gh.repoPath("/git/refs"), in, nil); err != nil {
		// A concurrent creator may have made the same tag between the read
		// and the write. Re-read and accept only the same immutable target.
		if peeled, readErr := gh.TagCommit(ctx, name); readErr == nil {
			if peeled != sha {
				return fmt.Errorf("tag %q was created concurrently at commit %s, not %s; amf never moves a tag", name, peeled, sha)
			}
			_, outErr := fmt.Fprintf(stdout, "Tag %s already points to %s.\n", name, sha)
			return outErr
		}
		return err
	}
	peeled, err = gh.TagCommit(ctx, name)
	if err != nil {
		return err
	}
	if peeled != sha {
		return fmt.Errorf("tag %q now points to commit %s, not %s", name, peeled, sha)
	}
	_, err = fmt.Fprintf(stdout, "Created tag %s at %s.\n", name, sha)
	return err
}
