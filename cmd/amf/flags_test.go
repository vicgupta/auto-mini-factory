package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// helpCommands is every command and subcommand with a FlagSet.
var helpCommands = [][]string{
	{"init"}, {"issue", "create"}, {"issue", "list"}, {"list"}, {"run"}, {"watch"}, {"auto"},
	{"pr", "list"}, {"pr", "merge"}, {"pr", "view"}, {"pr", "diff"}, {"pr", "close"},
	{"pr", "reopen"}, {"pr", "ready"}, {"pr", "draft"},
	{"memory", "sync"}, {"memory", "similar"}, {"memory", "show"},
	{"pipeline"}, {"doctor"}, {"update"}, {"version"},
}

type helpFlag struct{ short, long, line string }

var flagLine = regexp.MustCompile(`^  (?:-([a-zA-Z]), |    )--([a-z][a-z-]*)(\[=<[a-z]+>\]| <[a-z]+>)?( {2,})\S`)

// helpFlags parses the Flags: section of a command's help.
func helpFlags(help string) []helpFlag {
	_, section, _ := strings.Cut(help, "\nFlags:\n")
	section, _, _ = strings.Cut(section, "\n\n")
	var out []helpFlag
	for _, line := range strings.Split(section, "\n") {
		if m := flagLine.FindStringSubmatch(line); m != nil {
			out = append(out, helpFlag{m[1], m[2], line})
		}
	}
	return out
}

func commandHelpOutput(t *testing.T, cmd []string) string {
	t.Helper()
	var stderr bytes.Buffer
	err := run(context.Background(), append(slices.Clone(cmd), "--help"), nil, io.Discard, &stderr)
	if err != nil && !errors.Is(err, errUsage) {
		t.Fatalf("amf %v --help: %v", cmd, err)
	}
	return stderr.String()
}

// Every flag line has the -x, --long <value> shape, descriptions start in
// one column, no two flags share a letter, and a letter always means the
// flag shortFlags gives it.
func TestShortFlagsUnique(t *testing.T) {
	for _, cmd := range helpCommands {
		t.Run(strings.Join(cmd, " "), func(t *testing.T) {
			help := commandHelpOutput(t, cmd)
			_, section, ok := strings.Cut(help, "\nFlags:\n")
			if !ok {
				t.Fatalf("no Flags: section in\n%s", help)
			}
			section, _, _ = strings.Cut(section, "\n\n")
			lines := strings.Split(strings.TrimRight(section, "\n"), "\n")
			flags := helpFlags(help)
			if len(flags) != len(lines) {
				t.Fatalf("%d of %d flag lines are well formed:\n%s", len(flags), len(lines), section)
			}
			if last := flags[len(flags)-1]; last.short != "h" || last.long != "help" {
				t.Errorf("last flag is %+v, want -h, --help", last)
			}
			col := -1
			seen := map[string]string{}
			for _, f := range flags {
				m := flagLine.FindStringSubmatchIndex(f.line)
				if c := m[9]; col == -1 {
					col = c
				} else if c != col {
					t.Errorf("description of --%s starts at %d, want %d", f.long, c, col)
				}
				if f.short == "" {
					continue
				}
				if prev, dup := seen[f.short]; dup {
					t.Errorf("-%s is both --%s and --%s", f.short, prev, f.long)
				}
				seen[f.short] = f.long
				if f.long != "help" && shortFlags[f.long] != f.short {
					t.Errorf("--%s shows -%s, shortFlags says %q", f.long, f.short, shortFlags[f.long])
				}
			}
		})
	}
	// The shared table itself: each letter at most once per meaning.
	for long, s := range shortFlags {
		if len(s) != 1 || s == "h" {
			t.Errorf("--%s: short form %q must be one letter other than h", long, s)
		}
	}
}

// amf help <command> prints exactly what amf <command> --help does.
func TestHelpMatchesDashHelp(t *testing.T) {
	for key, args := range helpArgs {
		var out bytes.Buffer
		if err := run(context.Background(), append([]string{"help"}, strings.Fields(key)...), nil, &out, io.Discard); err != nil {
			t.Fatalf("amf help %s: %v", key, err)
		}
		if want := commandHelpOutput(t, args); out.String() != want {
			t.Errorf("amf help %s =\n%s\nwant amf %s --help =\n%s", key, out.String(), strings.Join(args, " "), want)
		}
		var short bytes.Buffer
		run(context.Background(), append(slices.Clone(args), "-h"), nil, io.Discard, &short)
		if short.String() != out.String() {
			t.Errorf("amf %s -h differs from amf help %s", strings.Join(args, " "), key)
		}
	}
}

// Every flag help shows is in docs/COMMANDS.md, with its short form.
func TestFlagsDocumentedInCommands(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "COMMANDS.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	for _, cmd := range helpCommands {
		for _, f := range helpFlags(commandHelpOutput(t, cmd)) {
			want := "--" + f.long
			if f.short != "" {
				want = "-" + f.short + ", --" + f.long
			}
			if !strings.Contains(doc, "`"+want) {
				t.Errorf("docs/COMMANDS.md lacks `%s` (amf %s)", want, strings.Join(cmd, " "))
			}
		}
	}
	// The Flag conventions table lists every shared letter.
	_, conv, ok := strings.Cut(doc, "## Flag conventions\n")
	if !ok {
		t.Fatal("docs/COMMANDS.md: missing ## Flag conventions")
	}
	conv, _, _ = strings.Cut(conv, "\n## ")
	var longs []string
	for long := range shortFlags {
		longs = append(longs, long)
	}
	sort.Strings(longs)
	for _, long := range longs {
		if row := "| `-" + shortFlags[long] + "` | `--" + long + "`"; !strings.Contains(conv, row) {
			t.Errorf("Flag conventions table lacks %s", row)
		}
	}
}

// Short and long forms parse to the same flag, and so do the old
// spellings: each pair below fails validation with the same message,
// which names the long flag.
func TestShortAndLongFormsParse(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		want  string
		forms [][]string
	}{
		{"agent timeout must be at least", [][]string{
			{"run", "42", "--agent-timeout", "10s"}, {"run", "42", "-t", "10s"}, {"run", "42", "-t=10s"},
			{"run", "42", "--timeout", "10s"}, {"run", "42", "-agent-timeout", "10s"}, {"run", "42", "--t", "10s"},
			{"watch", "-t", "10s"}, {"issue", "run", "42", "-t", "10s"}}},
		{errDraftMerge.Error(), [][]string{
			{"run", "42", "--draft", "--merge"}, {"run", "42", "--draft", "-m"}, {"run", "42", "--draft", "-m", "squash"},
			{"watch", "--draft", "-m=rebase"}, {"watch", "--draft", "--merge", "squash"}}},
		{"--approve can't be used together", [][]string{
			{"auto", "--abort", "--approve"}, {"auto", "--abort", "-y"}, {"auto", "status", "-y"}}},
		{"--continue resumes the saved run", [][]string{
			{"auto", "--continue", "add a flag"}, {"auto", "-c", "add a flag"}}},
		{"--continue and --dry-run", [][]string{
			{"auto", "--continue", "--dry-run"}, {"auto", "-c", "-n"}}},
		{"--max-issues must be between", [][]string{{"auto", "x", "--max-issues", "11", "-m", "squash"}}},
		{"--limit must be 0 or more", [][]string{
			{"issue", "list", "--limit", "-1"}, {"issue", "list", "-L", "-1"}, {"list", "-L=-1"}}},
		{"--state must be open, closed or all", [][]string{
			{"issue", "list", "--state", "x"}, {"issue", "list", "-s", "x"}}},
		{"--label and --no-label", [][]string{
			{"issue", "create", "T", "--label", "x", "--no-label"}, {"issue", "create", "T", "-l", "x", "--no-label"}}},
		{"use --body or --file, not both", [][]string{
			{"issue", "create", "T", "--body", "b", "--file", "f"}, {"issue", "create", "T", "-b", "b", "-f", "f"},
			{"issue", "T", "--b", "b", "--f", "f"}}},
		{"--squash and --rebase", [][]string{
			{"pr", "merge", "1", "--squash", "--rebase"}, {"pr", "merge", "1", "-s", "-r"}}},
	} {
		for _, args := range tc.forms {
			err := run(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("amf %v: error %v, want %q", args, err, tc.want)
			}
		}
	}
}

func TestExpandShortFlags(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.String("agent", "", "")
	fs.String("body", "", "")
	fs.Bool("dry-run", false, "")
	addMergeFlag(fs)
	for _, tc := range []struct{ in, want string }{
		{"-a codex 42", "--agent codex 42"},
		{"--a=codex", "--agent=codex"},
		{"-agent codex", "--agent codex"},
		{"-b -a", "--body -a"}, // a value that looks like a flag stays a value
		{"-n 42", "--dry-run 42"},
		{"-m squash 42", "--merge=squash 42"},
		{"-m 42", "--merge 42"},
		{"--merge rebase", "--merge=rebase"},
		{"-m=merge", "--merge=merge"},
		{"-z -h", "-z -h"},      // unknown flags and -h are the flag package's
		{"-- -a x", "-- -a x"},  // nothing after -- is a flag
		{"- -n", "- --dry-run"}, // a lone - is a positional argument
	} {
		if got := strings.Join(expandShortFlags(fs, strings.Fields(tc.in)), " "); got != tc.want {
			t.Errorf("expandShortFlags(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPrettyDefault(t *testing.T) {
	for in, want := range map[string]string{"1h0m0s": "1h", "1m0s": "1m", "1h30m0s": "1h30m", "90s": "90s", "2": "2", "amf:ready": "amf:ready"} {
		if got := prettyDefault(in); got != want {
			t.Errorf("prettyDefault(%q) = %q, want %q", in, got, want)
		}
	}
}
