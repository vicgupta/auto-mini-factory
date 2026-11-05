package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
)

const manHelp = `Usage:
  amf man

Prints a section 1 man page to stdout, generated from amf's help text,
including command flags, examples and configuration settings.
Needs no configuration, GitHub access or external tools.

Generate and preview it:
  amf man > amf.1
  man ./amf.1

Install it (use sudo if the destination requires it):
  install -d /usr/local/share/man/man1
  install -m 644 amf.1 /usr/local/share/man/man1/amf.1
  man amf
`

func cmdMan(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("man", stderr)
	pos, err := parseArgs(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errUsage
	}
	if len(pos) != 0 {
		fs.Usage()
		return errUsage
	}

	var b strings.Builder
	b.WriteString(".TH AMF 1 \"\" \"amf\" \"User Commands\"\n.SH NAME\namf \\- autonomous mini factory\n.SH SYNOPSIS\n.B amf\ncommand [arguments]\n.SH DESCRIPTION\n")
	manText(&b, usage)
	b.WriteString(".SH REFERENCE\n")
	topics := topicNames()
	for key := range subHelp {
		topics = append(topics, key)
	}
	slices.Sort(topics)
	for _, key := range topics {
		var help strings.Builder
		if err := run(ctx, append([]string{"help"}, strings.Fields(key)...), nil, &help, &help); err != nil {
			return err
		}
		title := key
		if slices.Contains(topCommands, key) || strings.Contains(key, " ") {
			title = "amf " + key
		}
		fmt.Fprintf(&b, ".SS %q\n", title)
		manText(&b, help.String())
	}
	_, err = io.WriteString(stdout, b.String())
	return err
}

// manText preserves help's spacing and treats it as literal text: backslashes
// must not become escapes, and leading dots or apostrophes must not be requests.
// Unicode is encoded as glyph escapes so no encoding preprocessor is needed.
func manText(w *strings.Builder, text string) {
	w.WriteString(".nf\n\\&")
	for _, r := range strings.TrimRight(text, "\n") {
		switch r {
		case '\\':
			w.WriteString(`\e`)
		case '-':
			w.WriteString(`\-`)
		case '\n':
			w.WriteString("\n\\&")
		default:
			if r > 127 {
				fmt.Fprintf(w, `\[u%04X]`, r)
			} else {
				w.WriteRune(r)
			}
		}
	}
	w.WriteString("\n.fi\n")
}
