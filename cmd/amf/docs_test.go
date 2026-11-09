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
	checkDocNames(t, "docs/COMMANDS.md commands", docTable(t, "docs/COMMANDS.md", "## Commands"), 2, commands)
	for _, group := range []struct {
		name, help string
		names      []string
	}{
		{"issue", issueUsage, issueSubcommands},
		{"pr", prUsage, prSubcommands},
		{"tag", tagUsage, tagSubcommands},
		{"release", releaseUsage, releaseSubcommands},
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
	checkDocNames(t, "docs/COMMANDS.md topics", docTable(t, "docs/COMMANDS.md", "## Topics"), 3, topics)
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
	var want []string
	entry := regexp.MustCompile(`(?m)^[\t ]*([A-Z][A-Z0-9_]+)[\t ]+\([\t ]*default[\t ]*:`)
	for _, m := range entry.FindAllStringSubmatch(envHelp, -1) {
		want = append(want, m[1])
	}
	if len(want) == 0 {
		t.Fatal("env help has no settings")
	}
	for _, doc := range []struct{ path, heading string }{
		{"docs/CONFIGURATION.md", "### Configuration (.env)"},
		{"docs/SPECS.md", "## Configuration (`.env`)"},
	} {
		checkDocNames(t, doc.path+" configuration", docTable(t, doc.path, doc.heading), 1, want)
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

// The README is the user-facing overview; keep the recent commands and flags
// it advertises in step with the CLI.
func TestReadmeMentionsCommandsAndFlags(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	readme := string(b)
	for _, want := range []string{
		"amf pr merge", "--update-branch", "--wait-for-checks",
		"amf pr sweep", "--merge", "--dry-run", "--json",
		"amf pr view",
		"amf tag list", "amf tag view", "amf release list", "amf release latest", "--state published",
		"amf pr comment", "--body", "--file",
		"amf issue label",
		"amf doctor --fix-labels",
		"amf auth login", "amf auth status", "amf auth logout", "amf auth token",
		"--client-id", "AMF_GITHUB_CLIENT_ID",
		"-D, --draft", "-O, --color",
		"amf/backup/issue-42-",
		"AMF_AGENT_IDLE_TIMEOUT",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README.md does not mention %q", want)
		}
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
		{"pr", "list"}, {"pr", "merge"}, {"pr", "view"}, {"pr", "diff"}, {"pr", "comment"}, {"pr", "close"},
		{"pr", "reopen"}, {"pr", "ready"}, {"pr", "draft"},
		{"tag", "list"}, {"tag", "view"}, {"release", "list"}, {"release", "view"}, {"release", "latest"},
		{"run"}, {"watch"}, {"auto"}, {"pipeline"}, {"doctor"}, {"update"},
	} {
		t.Run(strings.Join(cmd, " "), func(t *testing.T) {
			var stderr strings.Builder
			args := append(slices.Clone(cmd), "--help")
			if err := run(context.Background(), args, nil, io.Discard, &stderr); err != nil && !errors.Is(err, errUsage) {
				t.Fatal(err)
			}
			accepted := map[string]bool{"h": true, "help": true, "dir": true, "env-file": true, "clone-dir": true} // path flags: every command
			if cmd[0] == "update" {
				accepted["update"] = true // amf --update, a command alias
				accepted["u"] = true      // amf -u, root short alias of update
			}
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
				for _, row := range helpRows(helpTopics[cmd[0]], "Commands") {
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
