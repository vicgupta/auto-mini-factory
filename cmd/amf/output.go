package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
)

type outputOptions struct {
	Quiet bool
	Color string
}

type outputKey struct{}

func commandOutput(ctx context.Context) outputOptions {
	o, _ := ctx.Value(outputKey{}).(outputOptions)
	return o
}

// Like path flags, output flags apply to every command, anywhere before --.
func extractOutputFlags(args []string, o *outputOptions) ([]string, error) {
	var rest []string
	var command, subcommand string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return append(rest, args[i:]...), nil
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			if command == "" {
				command = a
			} else if subcommand == "" {
				subcommand = a
			}
			continue
		}
		switch name {
		case "quiet", "q":
			o.Quiet = true
			if hasValue {
				v, err := strconv.ParseBool(value)
				if err != nil {
					return nil, fmt.Errorf("--quiet: %w", err)
				}
				o.Quiet = v
			}
		case "color", "O":
			if !hasValue {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("flag needs an argument: --color")
				}
				i++
				value = args[i]
			}
			switch value {
			case "auto", "always", "never":
				o.Color = value
			default:
				return nil, fmt.Errorf("--color must be auto, always or never, got %q", value)
			}
		default:
			if command == "" && (a == "--update" || a == "-u") {
				command = "update"
			}
			rest = append(rest, a)
			// A flag's next word is data, even when it spells --quiet or
			// --color. Leave it for the command's own flag parser.
			if !hasValue && flagNeedsValue(name, command, subcommand) && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
		}
	}
	return rest, nil
}

// requiredFlagValues lets global extraction skip command flag values without
// parsing them. TestGlobalFlagValueMetadata checks it against command help.
var requiredFlagValues = strings.Fields(`add agent agent-timeout body checkpoint-after
client-id clone-dir color comment commit dir env-file eval-rounds exclude-label file host interval
issue label limit log-file log-level manifest max-issues memory-backend memory-dir plan remove
release scopes search stale-timeout state timeout version`)

func flagNeedsValue(name, command, subcommand string) bool {
	if len(name) == 1 {
		switch name {
		case "f": // --file on issue creation and pr comment, --force elsewhere
			return command == "issue" && (subcommand == "create" || !slices.Contains(issueSubcommands, subcommand)) ||
				command == "pr" && subcommand == "comment"
		case "c": // --comment and --commit take a value; --check and --continue do not
			if command == "auto" || command == "update" {
				return false
			}
			return slices.Contains(requiredFlagValues, "commit")
		case "s": // --state or --squash
			return command == "list" || subcommand == "list" ||
				command == "issue" && (subcommand == "label" || subcommand == "unlabel")
		case "P": // --checkpoint-after takes a value; --part does not
			return command == "auto"
		}
		for long, short := range shortFlags {
			if short == name {
				return slices.Contains(requiredFlagValues, long)
			}
		}
	}
	if name == "version" && command == "" { // amf --version takes no value
		return false
	}
	return slices.Contains(requiredFlagValues, name)
}

func (o outputOptions) colorEnabled(w io.Writer) bool {
	return o.colorForTerminal(writerIsTerminal(w))
}

func (o outputOptions) colorForTerminal(terminal bool) bool {
	switch o.Color {
	case "always":
		return true
	case "never":
		return false
	default:
		return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" && terminal
	}
}

func colorText(s, code string) string { return "\x1b[" + code + "m" + s + "\x1b[0m" }

// Color only the terminal copy, before log fanout, so stored logs stay plain.
type colorPrefixWriter struct{ w io.Writer }

func (w colorPrefixWriter) Write(p []byte) (int, error) {
	s := string(p)
	if strings.HasPrefix(s, "amf:") {
		s = colorText("amf:", "36") + strings.TrimPrefix(s, "amf:")
	}
	if _, err := io.WriteString(w.w, s); err != nil {
		return 0, err
	}
	return len(p), nil
}

func progress(ctx context.Context, w io.Writer, format string, args ...any) {
	if !commandOutput(ctx).Quiet {
		fmt.Fprintf(w, format, args...)
	}
}
