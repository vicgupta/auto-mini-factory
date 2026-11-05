package main

import (
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Flag conventions
//
// Every flag has a long form, --full-word in kebab-case, which is what help
// and the docs show. Common flags also have a one-letter short form. The
// letter belongs to the flag's meaning, so --agent is -a on every command
// that takes it; shortFlags is the single list of them, and docs/COMMANDS.md
// has the same table under "Flag conventions".
//
// Go's flag package treats -x and --x alike, so --a, -agent and -b (the old
// spelling of --body) keep working too.

// shortFlags maps a long flag name to its short letter. A letter may mean
// different things on unrelated commands (-s is --state on list commands and
// --squash on amf pr merge), but never two flags on one command:
// TestShortFlagsUnique checks every command.
var shortFlags = map[string]string{
	"agent":           "a",
	"agent-timeout":   "t",
	"approve":         "y",
	"body":            "b",
	"client-id":       "C",
	"check":           "c",
	"comment":         "c",
	"continue":        "c",
	"delete-branch":   "d",
	"dry-run":         "n",
	"eval-rounds":     "e",
	"exclude-label":   "x",
	"file":            "f",
	"force":           "f",
	"host":            "H",
	"interval":        "i",
	"issue":           "i",
	"json":            "j",
	"label":           "l",
	"limit":           "L",
	"merge":           "m",
	"no-browser":      "B",
	"quiet":           "q",
	"rebase":          "r",
	"scopes":          "o",
	"search":          "S",
	"squash":          "s",
	"state":           "s",
	"update-branch":   "u",
	"wait-for-checks": "w",
}

// hiddenFlags still parse but are left out of help: deprecated spellings.
var hiddenFlags = map[string]bool{
	"timeout": true, // deprecated alias of --agent-timeout
}

// longFlag returns the long flag that the short letter s names in fs.
func longFlag(fs *flag.FlagSet, s string) (string, bool) {
	if fs.Lookup(s) != nil {
		return s, true // a real one-letter flag wins
	}
	for long, short := range shortFlags {
		if short == s && fs.Lookup(long) != nil {
			return long, true
		}
	}
	return "", false
}

// shortFor returns fs's short letter for the long flag name, or "".
func shortFor(fs *flag.FlagSet, name string) string {
	s := shortFlags[name]
	if s == "" {
		return ""
	}
	if long, ok := longFlag(fs, s); !ok || long != name {
		return ""
	}
	return s
}

// expandShortFlags rewrites short flags to their long names, so the flag
// package (and fs.Visit) only ever sees long names: -a codex becomes
// --agent codex. It also joins a merge method given as the next word, so
// -m squash and --merge squash mean --merge=squash. Values, and everything
// after --, are left alone.
func expandShortFlags(fs *flag.FlagSet, args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return append(out, args[i:]...)
		}
		if len(a) < 2 || a[0] != '-' || a == "-" {
			out = append(out, a)
			continue
		}
		name := strings.TrimLeft(a, "-")
		value, hasValue := "", false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, value, hasValue = k, v, true
		}
		if len(name) == 1 {
			if long, ok := longFlag(fs, name); ok {
				name = long
			}
		}
		f := fs.Lookup(name)
		if f == nil {
			out = append(out, a) // unknown or -h: the flag package reports it
			continue
		}
		tok := "--" + name
		if hasValue {
			tok += "=" + value
		}
		out = append(out, tok)
		if hasValue {
			continue
		}
		if _, ok := f.Value.(*mergeFlag); ok {
			if i+1 < len(args) && slices.Contains(mergeMethods, args[i+1]) {
				out[len(out)-1] = tok + "=" + args[i+1]
				i++
			}
			continue
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) { // the flag's value, taken as is
			i++
			out = append(out, args[i])
		}
	}
	return out
}

// flagPlaceholder is the value a flag takes in help, e.g. <duration>.
func flagPlaceholder(f *flag.Flag) string {
	if _, ok := f.Value.(*mergeFlag); ok {
		return "[=<method>]"
	}
	name, _ := flag.UnquoteUsage(f)
	if n, ok := placeholders[f.Name]; ok && name != "" {
		name = n
	}
	switch name {
	case "":
		return ""
	case "int":
		name = "n"
	case "string":
		name = "text"
	}
	return " <" + name + ">"
}

// placeholders name the value a flag takes, where its type says too little.
var placeholders = map[string]string{
	"color": "mode",
	"agent": "name", "label": "name", "exclude-label": "name", "log-level": "level", "log-file": "path",
	"state": "state", "search": "text", "comment": "text", "version": "tag", "stale-timeout": "duration",
	"memory-backend": "backend", "memory-dir": "dir",
}

// prettyDefault drops a duration's zero tail: 1h0m0s is 1h, 1m0s is 1m.
func prettyDefault(d string) string {
	if strings.HasSuffix(d, "m0s") {
		d = strings.TrimSuffix(d, "0s")
	}
	if strings.HasSuffix(d, "h0m") {
		d = strings.TrimSuffix(d, "0m")
	}
	return d
}

// flagLines returns fs's help lines, one per visible flag plus --help, as
// left column and description.
func flagLines(fs *flag.FlagSet) [][2]string {
	var rows [][2]string
	fs.VisitAll(func(f *flag.Flag) {
		if hiddenFlags[f.Name] || (pathFlags[f.Name] && fs.Name() != "global") {
			return
		}
		left := "    --" + f.Name
		if s := shortFor(fs, f.Name); s != "" {
			left = "-" + s + ", --" + f.Name
		}
		left += flagPlaceholder(f)
		_, desc := flag.UnquoteUsage(f)
		if d := f.DefValue; d != "" && d != "0" && d != "false" && !strings.Contains(desc, "(default") {
			desc += fmt.Sprintf(" (default %s)", prettyDefault(d))
		}
		rows = append(rows, [2]string{left, desc})
	})
	return append(rows, [2]string{"-h, --help", "show this help"})
}

// printFlags writes fs's flags in one aligned column:
//
//	-a, --agent <name>  agent for ...
//
// pathFlags are amf's global flags, listed in their own section of every
// command's help.
var pathFlags = map[string]bool{"dir": true, "env-file": true, "clone-dir": true}

// printGlobalFlags lists the flags every command takes.
func printGlobalFlags(w io.Writer) {
	fs := flag.NewFlagSet("global", flag.ContinueOnError)
	fs.Bool("quiet", false, "suppress progress; keep warnings, errors and the final summary")
	fs.String("color", "auto", "color status output: auto, always or never (auto honors NO_COLOR)")
	fs.String("dir", "", "work as if amf were started in this `folder` (default: the current directory)")
	fs.String("env-file", "", "read only this .env `file` (default: ./.env, then the checkout's top-level .env)")
	fs.String("clone-dir", "", "use, or clone GITHUB_REPO into, this `folder` (default: the enclosing checkout, else ./<repo name>)")
	rows := flagLines(fs)
	printRows(w, rows[:len(rows)-1]) // without -h, --help
}

func printFlags(w io.Writer, fs *flag.FlagSet) {
	printRows(w, flagLines(fs))
}

func printRows(w io.Writer, rows [][2]string) {
	width := 0
	for _, r := range rows {
		width = max(width, len(r[0]))
	}
	for _, r := range rows {
		fmt.Fprintf(w, "  %-*s  %s\n", width, r[0], r[1])
	}
}
