package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Compare wording as well as names, ignoring only Markdown and whitespace.
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
	_, section, ok := strings.Cut(string(b), heading+"\n")
	if !ok {
		t.Fatalf("%s: missing %s", path, heading)
	}
	var rows [][]string
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "|---") {
			continue
		}
		if !strings.HasPrefix(line, "| ") {
			if len(rows) > 0 {
				break
			}
			continue
		}
		// Escaped pipes in flag choices belong to a cell.
		cells := strings.Split(strings.Trim(line, "|"), " | ")
		for i := range cells {
			cells[i] = docText(strings.ReplaceAll(cells[i], `\|`, "|"))
		}
		rows = append(rows, cells)
	}
	if len(rows) < 2 {
		t.Fatalf("%s: empty table under %s", path, heading)
	}
	return rows[1:] // omit the column headings
}

func helpCommandRows(help, prefix string) [][]string {
	_, section, _ := strings.Cut(help, "Commands:\n")
	section, _, _ = strings.Cut(section, "\n\n")
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSpace(section), "\n") {
		cells := regexp.MustCompile(` {2,}`).Split(strings.TrimSpace(line), 2)
		if len(cells) == 2 {
			rows = append(rows, []string{prefix + cells[0], cells[1]})
		}
	}
	return rows
}

func TestDocumentationCommands(t *testing.T) {
	want := helpCommandRows(usage, "amf ")
	var names []string
	for _, row := range want {
		names = append(names, strings.TrimPrefix(row[0], "amf "))
	}
	if !slices.Equal(names, topCommands) {
		t.Fatalf("overview commands = %v, want %v", names, topCommands)
	}
	for _, path := range []string{"README.md", "docs/COMMANDS.md"} {
		if got := docTable(t, path, "## Commands"); !reflect.DeepEqual(got, want) {
			t.Errorf("%s commands = %v, help lists %v", path, got, want)
		}
	}
	for _, group := range []struct {
		name, help string
		names      []string
	}{
		{"issue", issueUsage, issueSubcommands},
		{"pr", prUsage, prSubcommands},
	} {
		rows := helpCommandRows(group.help, "amf "+group.name+" ")
		var names []string
		for _, row := range rows {
			names = append(names, strings.Fields(row[0])[2])
		}
		if !slices.Equal(names, group.names[:len(group.names)-1]) { // help is a usage alias
			t.Errorf("%s help commands = %v, want %v", group.name, names, group.names)
		}
		if !strings.Contains(usage, strings.Join(names, ", ")) {
			t.Errorf("overview omits %s subcommands: %v", group.name, names)
		}
		if got := docTable(t, "docs/COMMANDS.md", "## "+group.name+" commands"); !reflect.DeepEqual(got, rows) {
			t.Errorf("%s reference = %v, help lists %v", group.name, got, rows)
		}
	}
}

func TestDocumentationEnvironment(t *testing.T) {
	var want [][]string
	entry := regexp.MustCompile(`(?m)^  ([A-Z][A-Z0-9_]+) \(default: ([^\n]+)\)\n((?:    [^\n]+\n)+)`)
	for _, m := range entry.FindAllStringSubmatch(envHelp, -1) {
		want = append(want, []string{m[1], m[2], docText(m[3])})
	}
	if len(want) == 0 {
		t.Fatal("env help has no settings with defaults")
	}
	for _, doc := range []struct{ path, heading string }{
		{"README.md", "### Configuration (.env)"},
		{"docs/SPECS.md", "## Configuration (`.env`)"},
	} {
		if got := docTable(t, doc.path, doc.heading); !reflect.DeepEqual(got, want) {
			t.Errorf("%s configuration = %v, help lists %v", doc.path, got, want)
		}
	}
}

func TestDocumentedFlagsExist(t *testing.T) {
	flagName := regexp.MustCompile(`(?:^|[\s\[|(])--?([a-zA-Z][a-z-]*)`)
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
			for _, f := range helpFlags(stderr.String()) {
				accepted[f.long] = true
				if f.short != "" {
					accepted[f.short] = true
				}
			}
			if accepted["agent-timeout"] {
				accepted["timeout"] = true // hidden, deprecated alias
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
				for _, row := range helpCommandRows(helpTopics[cmd[0]], "") {
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
