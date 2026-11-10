package main

import (
	"bytes"
	"context"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// helpEntry matches a two-column help entry and captures where its
// description starts.
var helpEntry = regexp.MustCompile(`^  \S.*?\S {2,}(\S)`)

// helpSection returns the non-empty lines of the section that starts with
// header, up to the next blank line.
func helpSection(text, header string) []string {
	_, rest, ok := strings.Cut(text, header+"\n")
	if !ok {
		return nil
	}
	rest, _, _ = strings.Cut(rest, "\n\n")
	return strings.Split(strings.TrimRight(rest, "\n"), "\n")
}

// TestHelpFitsEightyColumns keeps every help line within 80 columns and every
// flag and command entry on one line. It walks the overview, amf help, every
// topic and group, and every command in helpCommands.
func TestHelpFitsEightyColumns(t *testing.T) {
	pages := map[string][]string{
		"overview": {"help"},
		"--help":   {"--help"},
	}
	for _, topic := range topicNames() {
		pages["help "+topic] = append([]string{"help"}, strings.Fields(topic)...)
	}
	for _, cmd := range helpCommands {
		pages[strings.Join(cmd, " ")+" --help"] = append(slices.Clone(cmd), "--help")
	}
	// Group pages list their subcommands; issue run/watch are aliases.
	for _, group := range []string{"issue", "pr", "auth", "tag", "release", "memory", "issue run", "issue watch"} {
		pages[group+" --help"] = append(strings.Fields(group), "--help")
	}
	keys := make([]string, 0, len(pages))
	for key := range pages {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args := pages[key]
		t.Run(key, func(t *testing.T) {
			var out, stderr bytes.Buffer
			_ = run(context.Background(), args, nil, &out, &stderr)
			text := out.String() + stderr.String()
			if strings.TrimSpace(text) == "" {
				t.Fatalf("amf %v printed no help", args)
			}
			for i, line := range strings.Split(text, "\n") {
				if n := utf8.RuneCountInString(line); n > 80 {
					t.Errorf("line %d is %d columns: %q", i+1, n, line)
				}
			}
			// Any two-column entry (name, two or more spaces, description),
			// in any section, must not continue on a line indented to its
			// description column.
			lines := strings.Split(text, "\n")
			for i := 0; i+1 < len(lines); i++ {
				m := helpEntry.FindStringSubmatchIndex(lines[i])
				if m == nil {
					continue
				}
				col := m[2]
				next := lines[i+1]
				if indent := len(next) - len(strings.TrimLeft(next, " ")); indent == col && strings.TrimSpace(next) != "" {
					t.Errorf("entry spans lines: %q / %q", lines[i], next)
				}
			}
			for _, section := range []struct {
				header string
				entry  func(string) bool
			}{
				{"Flags:", func(s string) bool { return strings.HasPrefix(s, "  -") || strings.HasPrefix(s, "    --") }},
				{"Global flags (any command, before --):", func(s string) bool { return strings.HasPrefix(s, "  -") || strings.HasPrefix(s, "    --") }},
				{"Commands:", func(s string) bool { return strings.HasPrefix(s, "  ") && !strings.HasPrefix(s, "   ") }},
				{"Topics:", func(s string) bool { return strings.HasPrefix(s, "  ") && !strings.HasPrefix(s, "   ") }},
			} {
				for _, line := range helpSection(text, section.header) {
					if !section.entry(line) {
						t.Errorf("%s entry spans lines: %q", section.header, line)
					}
				}
			}
		})
	}
}
