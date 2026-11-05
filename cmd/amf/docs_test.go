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
	if got := docTable(t, "docs/COMMANDS.md", "## Commands"); !reflect.DeepEqual(got, want) {
		t.Errorf("command reference = %v, help lists %v", got, want)
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

func TestHelpExamplesCoverCommands(t *testing.T) {
	commands := slices.Clone(topCommands)
	for group, subcommands := range helpSubcommands {
		for _, subcommand := range subcommands {
			commands = append(commands, group+" "+subcommand)
		}
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			examples := helpExamples[command]
			if strings.TrimSpace(examples) == "" {
				t.Fatalf("amf %s has no examples", command)
			}
			var out, stderr strings.Builder
			if err := run(context.Background(), append([]string{"help"}, strings.Fields(command)...), nil, &out, &stderr); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "\nExamples:\n"+examples) || stderr.Len() != 0 {
				t.Errorf("amf help %s: missing examples in stdout %q; stderr %q", command, out.String(), stderr.String())
			}
		})
	}
	for _, example := range []string{"amf auto status", "amf auto --abort"} {
		if !strings.Contains(helpExamples["auto"], example+"\n") {
			t.Errorf("auto help lacks %q", example)
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
		{"docs/CONFIGURATION.md", "### Configuration (.env)"},
		{"docs/SPECS.md", "## Configuration (`.env`)"},
	} {
		if got := docTable(t, doc.path, doc.heading); !reflect.DeepEqual(got, want) {
			t.Errorf("%s configuration = %v, help lists %v", doc.path, got, want)
		}
	}
}

func TestUpdateReleaseNotes(t *testing.T) {
	const url = "https://github.com/vicgupta/auto-mini-factory/releases"
	var out strings.Builder
	if err := run(context.Background(), []string{"help", "update"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), url+"\n") {
		t.Errorf("amf help update omits release notes: %s", &out)
	}
	b, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, install, ok := strings.Cut(string(b), "## Install\n")
	if !ok {
		t.Fatal("README.md: missing Install section")
	}
	install, _, _ = strings.Cut(install, "\n## ")
	if !strings.Contains(install, "]("+url+")") {
		t.Error("README.md Install section omits release notes link")
	}
}

func TestJSONDocumentationCoversJSONCommands(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "JSON.md"))
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	for _, cmd := range helpCommands {
		hasJSON := slices.ContainsFunc(helpFlags(commandHelpOutput(t, cmd)), func(f helpFlag) bool {
			return f.long == "json"
		})
		if !hasJSON {
			continue
		}
		name := "amf " + strings.Join(cmd, " ")
		if !strings.Contains(doc, "`"+name) {
			t.Errorf("docs/JSON.md omits %s", name)
		}
	}
}

func TestHelpExamplesKnownCommands(t *testing.T) {
	// Include commands embedded in shell substitutions, such as auth token.
	command := regexp.MustCompile(`(?:^|\$\()amf\s+([^\s)]+)(?:\s+([^\s)]+))?`)
	for key, examples := range helpExamples {
		t.Run(key, func(t *testing.T) {
			for _, line := range strings.Split(strings.TrimSpace(examples), "\n") {
				matches := command.FindAllStringSubmatch(strings.TrimSpace(line), -1)
				if len(matches) == 0 {
					t.Errorf("example has no amf command: %q", line)
					continue
				}
				for _, m := range matches {
					if !slices.Contains(topCommands, m[1]) {
						t.Errorf("example uses unknown command %q: %q", m[1], line)
					}
					if subs, ok := helpSubcommands[m[1]]; ok && !slices.Contains(subs, m[2]) {
						t.Errorf("example uses unknown %s subcommand %q: %q", m[1], m[2], line)
					}
				}
			}
		})
	}
}

func TestDocumentedFlagsExist(t *testing.T) {
	flagName := regexp.MustCompile(`(?:^|[\s\[|(])--?([a-zA-Z][a-z-]*)`)
	for _, cmd := range [][]string{
		{"issue", "close"}, {"issue", "reopen"}, {"issue", "view"},
		{"issue", "label"}, {"issue", "unlabel"},
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
			accepted := map[string]bool{"h": true, "help": true, "dir": true, "env-file": true, "clone-dir": true} // path flags: every command
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
			for _, path := range []string{"README.md", "docs/COMMANDS.md", "docs/CONFIGURATION.md"} {
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
