package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Ignore Markdown and whitespace when reading names from documentation.
func docText(s string) string {
	s = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`).ReplaceAllString(s, "$1")
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "`", "")), " ")
}

func docTable(t *testing.T, path, heading string) [][]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", path))
	if err != nil {
		t.Fatal(err)
	}
	inSection := false
	var rows [][]string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if !inSection {
			inSection = docText(line) == docText(heading)
			continue
		}
		if !strings.HasPrefix(line, "|") {
			if len(rows) > 0 || strings.HasPrefix(line, "#") {
				break
			}
			continue
		}
		// Escaped pipes in flag choices belong to a cell.
		cells := strings.Split(strings.Trim(strings.ReplaceAll(line, `\|`, "\x00"), "|"), "|")
		if strings.Trim(cells[0], " -:\t") == "" {
			continue // table separator, with optional alignment markers
		}
		for i := range cells {
			cells[i] = docText(strings.ReplaceAll(cells[i], "\x00", "|"))
		}
		rows = append(rows, cells)
	}
	if len(rows) < 2 {
		t.Fatalf("%s: empty table under %s", path, heading)
	}
	return rows[1:] // omit the column headings
}

func helpRows(help, section string) [][]string {
	inSection := false
	var rows [][]string
	for _, line := range strings.Split(help, "\n") {
		line = strings.TrimSpace(line)
		if !inSection {
			inSection = line == section+":"
			continue
		}
		if line == "" && len(rows) > 0 {
			break
		}
		if fields := strings.Fields(line); len(fields) > 0 {
			rows = append(rows, []string{fields[0], strings.Join(fields[1:], " ")})
		}
	}
	return rows
}

// Compare sets of names, ignoring arguments, ordering and descriptions.
func checkDocNames(t *testing.T, label string, rows [][]string, words int, want []string) {
	t.Helper()
	var got []string
	for _, row := range rows {
		fields := strings.Fields(row[0])
		got = append(got, strings.Join(fields[:min(words, len(fields))], " "))
	}
	want = slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	if len(got) == 0 || !slices.Equal(got, want) {
		t.Errorf("%s names = %v, want %v", label, got, want)
	}
}

func TestDocumentationCommands(t *testing.T) {
	checkDocNames(t, "overview commands", helpRows(usage, "Commands"), 1, topCommands)
	var commands []string
	for _, name := range topCommands {
		if _, ok := helpTopics[name]; !ok {
			t.Errorf("command %q has no help topic", name)
		}
		commands = append(commands, "amf "+name)
	}
	for _, path := range []string{"README.md", "docs/COMMANDS.md"} {
		checkDocNames(t, path+" commands", docTable(t, path, "## Commands"), 2, commands)
	}
	for _, group := range []struct {
		name, help string
		names      []string
	}{
		{"issue", issueUsage, issueSubcommands},
		{"pr", prUsage, prSubcommands},
	} {
		names := group.names[:len(group.names)-1] // help is a usage alias
		checkDocNames(t, group.name+" help commands", helpRows(group.help, "Commands"), 1, names)
		var commands []string
		for _, name := range names {
			commands = append(commands, "amf "+group.name+" "+name)
		}
		checkDocNames(t, group.name+" reference", docTable(t, "docs/COMMANDS.md", "## "+group.name+" commands"), 3, commands)
	}
}

func TestDocumentationTopics(t *testing.T) {
	var names, topics []string
	for _, name := range topicNames() {
		if !slices.Contains(topCommands, name) {
			names = append(names, name)
			topics = append(topics, "amf help "+name)
		}
	}
	checkDocNames(t, "overview topics", helpRows(usage, "Topics"), 1, names)
	for _, path := range []string{"README.md", "docs/COMMANDS.md"} {
		checkDocNames(t, path+" topics", docTable(t, path, "## Topics"), 3, topics)
	}
}

func TestDocumentationEnvironment(t *testing.T) {
	var want []string
	entry := regexp.MustCompile(`(?m)^[\t ]*([A-Z][A-Z0-9_]+)[\t ]+\([\t ]*default[\t ]*:`)
	for _, m := range entry.FindAllStringSubmatch(envHelp, -1) {
		want = append(want, m[1])
	}
	if len(want) == 0 {
		t.Fatal("env help has no settings")
	}
	for _, doc := range []struct{ path, heading string }{
		{"README.md", "### Configuration (.env)"},
		{"docs/SPECS.md", "## Configuration (`.env`)"},
	} {
		checkDocNames(t, doc.path+" configuration", docTable(t, doc.path, doc.heading), 1, want)
	}
}

func TestDocumentedFlagsExist(t *testing.T) {
	flagName := regexp.MustCompile(`(?:^|[\s\[|])--?([a-z][a-z-]*)`)
	for _, cmd := range [][]string{
		{"init"}, {"issue", "create"}, {"issue", "list"}, {"issue", "run"}, {"issue", "watch"},
		{"pr", "list"}, {"pr", "merge"}, {"pr", "view"}, {"pr", "diff"}, {"pr", "close"},
		{"pr", "reopen"}, {"pr", "ready"}, {"pr", "draft"},
		{"run"}, {"watch"}, {"auto"}, {"pipeline"}, {"doctor"}, {"update"},
	} {
		t.Run(strings.Join(cmd, " "), func(t *testing.T) {
			var stderr strings.Builder
			args := append(slices.Clone(cmd), "--help")
			if err := run(context.Background(), args, nil, io.Discard, &stderr); err != nil && !errors.Is(err, errUsage) {
				t.Fatal(err)
			}
			accepted := map[string]bool{"h": true, "help": true}
			for _, m := range regexp.MustCompile(`(?m)^  -([a-z][a-z-]*)\b`).FindAllStringSubmatch(stderr.String(), -1) {
				accepted[m[1]] = true
			}
			check := func(text string) {
				t.Helper()
				for _, m := range flagName.FindAllStringSubmatch(text, -1) {
					if !accepted[m[1]] {
						t.Errorf("amf %s documents nonexistent flag %s in %q", strings.Join(cmd, " "), m[0], text)
					}
				}
			}
			if len(cmd) == 1 {
				check(helpTopics[cmd[0]])
			} else {
				for _, row := range helpRows(helpTopics[cmd[0]], "Commands") {
					if strings.Fields(row[0])[0] == cmd[1] {
						check(strings.Join(row, " "))
					}
				}
			}
			for _, path := range []string{"README.md", "docs/COMMANDS.md"} {
				b, err := os.ReadFile(filepath.Join("..", "..", path))
				if err != nil {
					t.Fatal(err)
				}
				prefix := "amf " + strings.Join(cmd, " ")
				for _, line := range strings.Split(string(b), "\n") {
					line, _, _ = strings.Cut(line, "#") // ignore shell comments
					line, _, _ = strings.Cut(line, "|") // ignore piped commands
					if strings.HasPrefix(line, prefix+" ") {
						check(line)
					}
				}
			}
		})
	}
}
