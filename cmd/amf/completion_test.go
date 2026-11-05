package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func completionScript(t *testing.T, shell string) string {
	t.Helper()
	var out, stderr bytes.Buffer
	if err := run(context.Background(), []string{"completion", shell}, nil, &out, &stderr); err != nil {
		t.Fatalf("completion %s: %v: %s", shell, err, &stderr)
	}
	if stderr.Len() != 0 || out.Len() == 0 {
		t.Fatalf("stdout = %q, stderr = %q", &out, &stderr)
	}
	return out.String()
}

func TestCompletionUsage(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{}, {"powershell"}, {"bash", "extra"}, {"--unknown"}} {
		var out, stderr bytes.Buffer
		err := run(context.Background(), append([]string{"completion"}, args...), nil, &out, &stderr)
		if !errors.Is(err, errUsage) || out.Len() != 0 || !strings.Contains(stderr.String(), "amf completion") {
			t.Errorf("completion %v: err = %v, stdout = %q, stderr = %q", args, err, &out, &stderr)
		}
	}
}

// Keep completion aligned with actual command help, including global flags,
// flag arity and short forms, as commands acquire new flags. Completion now
// builds each case from the command's own FlagSet, and this asserts it still
// matches what help prints for every command.
func TestCompletionFlagsMatchHelp(t *testing.T) {
	options := completionOptions()
	for _, cmd := range helpCommands {
		key := strings.Join(cmd, " ")
		help := commandHelpOutput(t, cmd)
		var want []string
		for _, f := range append(helpFlags(help), helpGlobalFlags(help)...) {
			if f.long == "help" {
				continue // help is added to every generated case
			}
			suffix := ""
			if strings.Contains(f.line, "--"+f.long+" <") {
				suffix = "="
			}
			want = append(want, "--"+f.long+suffix)
			if f.short != "" {
				want = append(want, "-"+f.short+suffix)
			}
		}
		want = append(want, "--help", "-h")
		var got []string
		for _, word := range options[key] {
			if strings.HasPrefix(word, "-") {
				got = append(got, word)
			}
		}
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s flags = %v, want %v", key, got, want)
		}
	}
}

func helpGlobalFlags(help string) []helpFlag {
	_, section, _ := strings.Cut(help, "\nGlobal flags (any command, before --):\n")
	section, _, _ = strings.Cut(section, "\n\n")
	var out []helpFlag
	for _, line := range strings.Split(section, "\n") {
		if m := flagLine.FindStringSubmatch(line); m != nil {
			out = append(out, helpFlag{m[1], m[2], line})
		}
	}
	return out
}

func TestCompletionWriteError(t *testing.T) {
	if err := cmdCompletion([]string{"bash"}, completionFailWriter{}, io.Discard); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("write error = %v, want %v", err, io.ErrClosedPipe)
	}
}

type completionFailWriter struct{}

func (completionFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCompletionShells(t *testing.T) {
	t.Chdir(t.TempDir()) // generating scripts needs no configuration or checkout
	for _, shell := range []string{"bash", "zsh", "fish"} {
		t.Run(shell, func(t *testing.T) {
			script := completionScript(t, shell)
			registration := map[string]string{
				"bash": "complete -o default -F _amf_complete amf",
				"zsh":  "compdef _amf_complete amf",
				"fish": "complete -c amf -f -a",
			}
			if !strings.Contains(script, registration[shell]) {
				t.Fatalf("missing %s completion registration", shell)
			}
			bin, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s unavailable; generated script checked by other tests", shell)
			}
			path := filepath.Join(t.TempDir(), "completion")
			if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := exec.Command(bin, "-n", path).CombinedOutput(); err != nil {
				t.Fatalf("syntax: %v: %s", err, out)
			}
			for _, tc := range []struct {
				line, want, absent string
			}{
				{"amf ", "completion", "--agent"},
				{"amf pr ", "merge", "--agent"},
				{"amf pr merge 42 --", "--squash", "--agent"},
				{"amf issue run 42 --", "--agent", "--squash"},
				{"amf run 42 -", "-a", "--squash"},
				{"amf run 42 -", "-D", "--squash"},
				{"amf version -", "-O", "--agent"},
				{"amf completion ", "fish", "run"},
				{"amf auto ", "status", "--squash"},
				{"amf auto status --", "--dir", "--agent"},
				{"amf help ", "env", "--agent"},
				{"amf help pr ", "merge", "--agent"},
				{"amf --dir 'a directory' issue list --", "--state", "--agent"},
				{"amf issue create --body run --", "--no-label", "--agent"},
				{"amf watch --label=run --", "--interval", "--squash"},
				{"amf run 42 --agent ", "", "--agent"},
				{"amf run 42 -- ", "", "--agent"},
			} {
				var invoke string
				switch shell {
				case "bash":
					invoke = `source "$1"; eval "COMP_WORDS=($2)"; [[ "$2" == *' ' ]] && COMP_WORDS+=(""); COMP_CWORD=$((${#COMP_WORDS[@]}-1)); _amf_complete; printf '%s\n' "${COMPREPLY[@]}"`
				case "zsh":
					// compadd normally runs inside ZLE; capture its arguments here.
					invoke = `autoload -Uz compinit; compinit -D; source "$1"; eval "words=($2)"; [[ "$2" == *' ' ]] && words+=(""); CURRENT=${#words[@]}; compadd() { shift; printf '%s\n' "$@"; }; _files() { :; }; _amf_complete`
				case "fish":
					invoke = `source $argv[1]; complete -C "$argv[2]"`
				}
				argv := []string{"-c", invoke, "amf-test", path, tc.line}
				if shell == "fish" {
					argv = []string{"-c", invoke, path, tc.line}
				}
				out, err := exec.Command(bin, argv...).CombinedOutput()
				if err != nil {
					t.Fatalf("%q: %v: %s", tc.line, err, out)
				}
				var got []string
				for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
					word, _, _ := strings.Cut(line, "\t") // fish descriptions
					got = append(got, word)
				}
				if tc.want != "" && !slices.Contains(got, tc.want) || tc.absent != "" && slices.Contains(got, tc.absent) {
					t.Errorf("%q: got %q, want %q, absent %q", tc.line, got, tc.want, tc.absent)
				}
			}
		})
	}
}
