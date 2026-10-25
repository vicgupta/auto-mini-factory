package main

import (
	"bytes"
	"context"
	"flag"
	"reflect"
	"strings"
	"testing"
)

func TestRunMergesPR(t *testing.T) {
	for _, method := range mergeMethods {
		t.Run(method, func(t *testing.T) {
			root, origin := setupRepo(t)
			gh, srv := newFakeGitHub(t)
			gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", State: "open", Labels: []Label{{Name: "amf:ready"}}}
			r, log := newTestRunner(t, srv, root, fakeClaude(t, `cat >/dev/null; echo hello > feature.txt; echo done`))
			r.Merge = method

			if err := r.Run(context.Background(), 1); err != nil {
				t.Fatalf("Run: %v\nlog:\n%s", err, log)
			}
			pr := gh.prs[0]
			if pr["state"] != "merged" || pr["merge_method"] != method {
				t.Fatalf("PR = %v, want merged with %s", pr, method)
			}
			// The merge is pinned to the commit amf pushed.
			if want := mustGit(t, root, "--git-dir", origin, "rev-parse", "amf/issue-1"); pr["merge_sha"] != want {
				t.Fatalf("merged sha %q, want the pushed head %q", pr["merge_sha"], want)
			}
			if got, want := gh.labels(1), []string{"amf:done"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("labels = %v, want %v", got, want)
			}
			want := "amf opened https://github.com/vic/demo/pull/101 for this issue and " + mergedVerb[method] + " it."
			if c := gh.comments[1]; len(c) != 1 || c[0] != want {
				t.Fatalf("comments = %q, want %q", c, want)
			}
			if !strings.Contains(log.String(), "#1: "+mergedVerb[method]+" https://github.com/vic/demo/pull/101") {
				t.Fatalf("log doesn't report the merge:\n%s", log)
			}
		})
	}
}

func TestRunWithoutMergeLeavesPROpen(t *testing.T) {
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, Title: "Add feature file", State: "open"}
	r, _ := newTestRunner(t, srv, root, fakeClaude(t, `cat >/dev/null; echo hello > feature.txt; echo done`))

	if err := r.Run(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if pr := gh.prs[0]; pr["state"] != "open" || pr["merge_attempts"] != "" {
		t.Fatalf("PR = %v, want it open and never merged", pr)
	}
}

func TestMergeFlag(t *testing.T) {
	for _, tc := range []struct {
		args    []string
		want    string
		wantErr string
	}{
		{args: []string{"42"}, want: ""},
		{args: []string{"42", "--merge"}, want: "merge"},
		{args: []string{"--merge", "42"}, want: "merge"},
		{args: []string{"42", "--merge=merge"}, want: "merge"},
		{args: []string{"42", "--merge=squash"}, want: "squash"},
		{args: []string{"42", "--merge=rebase"}, want: "rebase"},
		{args: []string{"42", "--merge=true"}, want: "merge"},
		{args: []string{"42", "--merge=false"}, want: ""},
		{args: []string{"42", "--merge=fast-forward"}, wantErr: `--merge: want merge, squash or rebase, got "fast-forward"`},
		{args: []string{"42", "--merge", "squash"}, wantErr: "--merge takes its method after =: --merge=squash, not --merge squash"},
	} {
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		m := addMergeFlag(fs)
		pos, err := parseArgs(fs, tc.args)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		err = m.check(pos)
		switch {
		case tc.wantErr != "" && (err == nil || err.Error() != tc.wantErr):
			t.Errorf("%v: error %v, want %q", tc.args, err, tc.wantErr)
		case tc.wantErr == "" && err != nil:
			t.Errorf("%v: %v", tc.args, err)
		case tc.wantErr == "" && m.value != tc.want:
			t.Errorf("%v: --merge is %q, want %q", tc.args, m.value, tc.want)
		}
	}
}

// A bad --merge fails run and watch before anything else, even without a
// .env.
func TestMergeFlagRejectedAtStartup(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, args := range [][]string{{"run", "1", "--merge=ff"}, {"watch", "--merge=ff"}, {"run", "1", "--merge", "rebase"}, {"run", "1", "--merge", "--draft"}, {"watch", "--draft", "--merge=squash"}} {
		var stderr bytes.Buffer
		err := run(context.Background(), args, strings.NewReader(""), &bytes.Buffer{}, &stderr)
		if err == nil || !strings.Contains(err.Error(), "--merge") {
			t.Errorf("%v: error %v, want one about --merge", args, err)
		}
	}
}
