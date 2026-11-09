package main

import (
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
)

var tagUsage = `usage: amf tag <command>

Commands:
  list [--limit n] [--json]  list tags by name, with their commits
  view <name> [--json]  show a tag's recursively peeled commit

Tags are read-only in amf: it cannot create, move or delete tags.
Run amf help tag <command> or amf tag <command> --help for its flags.
` + "\nExamples:\n" + helpExamples["tag"] + "\nSee also: amf help release, amf help flags\n"

var tagSubcommands = []string{"list", "view", "help"}

// Use the existing sanctioned API client, but refuse all redirects for these
// inspection commands (including the preliminary repository check). Go's
// default redirect policy can forward Authorization across ports or to
// subdomains; inspection must never send credentials to a redirected target.
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
