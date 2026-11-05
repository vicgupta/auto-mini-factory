package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestMan(t *testing.T) {
	// Generation needs neither configuration nor a checkout, and never reads stdin.
	t.Chdir(t.TempDir())
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GITHUB_REPO", "")
	var out, stderr bytes.Buffer
	if err := run(context.Background(), []string{"man"}, nil, &out, &stderr); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", &stderr)
	}
	page := out.String()
	for _, want := range []string{
		`.TH AMF 1`, `.SH NAME`, `.SH SYNOPSIS`, `.SH DESCRIPTION`,
		`.SS "amf run"`, `.SS "amf pr merge"`, `.SS "amf auth login"`,
		`.SS "amf memory similar"`, `.SS "env"`,
		`\-\-agent\-timeout`, `\-\-delete\-branch`, `GITHUB_TOKEN`,
		`Exit status:`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("man page missing %q", want)
		}
	}
	// Repeated generation must not depend on map iteration order or the date.
	var again bytes.Buffer
	if err := run(context.Background(), []string{"man"}, nil, &again, io.Discard); err != nil {
		t.Fatal(err)
	}
	if page != again.String() {
		t.Fatal("man page is not deterministic")
	}
}

func TestManArguments(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want error
	}{
		{[]string{"man", "--help"}, nil},
		{[]string{"man", "-h"}, nil},
		{[]string{"help", "man"}, nil},
		{[]string{"man", "extra"}, errUsage},
		{[]string{"man", "--unknown"}, errUsage},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var out, stderr bytes.Buffer
			if err := run(context.Background(), tc.args, nil, &out, &stderr); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if strings.HasPrefix(out.String(), ".TH") || out.Len()+stderr.Len() == 0 {
				t.Fatalf("expected help or usage, got stdout %q, stderr %q", &out, &stderr)
			}
		})
	}
}

func TestManLiteralText(t *testing.T) {
	var out strings.Builder
	manText(&out, ".env\n'quoted\n\\path --flag\nplan → code …\n")
	if want := ".nf\n\\&.env\n\\&'quoted\n\\&\\epath \\-\\-flag\n\\&plan \\[u2192] code \\[u2026]\n.fi\n"; out.String() != want {
		t.Fatalf("literal text = %q, want %q", out.String(), want)
	}
}
