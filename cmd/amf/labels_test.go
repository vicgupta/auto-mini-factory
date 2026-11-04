package main

import (
	"bytes"
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"text/tabwriter"
)

func TestIssueLabel(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	listJSONEnv(t, srv)
	gh.issues[7] = &Issue{Number: 7, State: "open", Labels: []Label{{Name: labelFailed}, {Name: "bug"}}}

	var out bytes.Buffer
	if err := run(context.Background(), []string{"issue", "label", "7", "--add", labelDone, "--remove", "bug"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got := labelNames(*gh.issues[7]); !reflect.DeepEqual(got, []string{labelDone}) {
		t.Fatalf("labels %v", got)
	}
	if !strings.HasPrefix(out.String(), "#7: added amf:done; removed ") {
		t.Fatalf("output %q", &out)
	}

	// Nothing to change costs no writes.
	gh.writes, out = nil, bytes.Buffer{}
	if err := run(context.Background(), []string{"issue", "label", "7", "--add", labelDone, "--remove", "bug,other"}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if out.String() != "#7 unchanged\n" || len(gh.writes) != 0 {
		t.Fatalf("output %q writes %v", &out, gh.writes)
	}

	for _, args := range [][]string{
		{"7"},
		{"7", "--add", "x", "--remove", "x"},
		{"7", "--add", labelDone + "," + labelFailed},
		{"nope", "--add", "x"},
	} {
		if err := run(context.Background(), append([]string{"issue", "label"}, args...), nil, io.Discard, io.Discard); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
}

func TestIssuePositionalLabels(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		args          []string
		want          []string
		output        string
		writes        int
	}{
		{"add", "label", []string{"#7", "amf:ready", "needs review"}, []string{"amf:failed", "bug", "amf:ready", "needs review"}, "#7: added amf:ready, needs review\n", 1},
		{"remove", "unlabel", []string{"7", "amf:failed", "bug"}, nil, "#7: removed amf:failed, bug\n", 2},
		{"comma separated", "label", []string{"7", " docs, needs review "}, []string{"amf:failed", "bug", "docs", "needs review"}, "#7: added docs, needs review\n", 1},
		{"duplicates", "label", []string{"7", "docs", "docs"}, []string{"amf:failed", "bug", "docs"}, "#7: added docs\n", 1},
		{"remove duplicates", "unlabel", []string{"7", "bug,bug"}, []string{"amf:failed"}, "#7: removed bug\n", 1},
		{"already present", "label", []string{"7", "bug"}, []string{"amf:failed", "bug"}, "#7 unchanged\n", 0},
		{"already absent", "unlabel", []string{"7", "docs"}, []string{"amf:failed", "bug"}, "#7 unchanged\n", 0},
		{"status", "label", []string{"7", "amf:done"}, []string{"bug", "amf:done"}, "#7: added amf:done; removed amf:failed\n", 2},
		{"mixed flags", "label", []string{"7", "docs", "--remove", "bug", "--add", "needs review"}, []string{"amf:failed", "needs review", "docs"}, "#7: added needs review, docs; removed bug\n", 2},
		{"literal flags", "label", []string{"7", "--", "docs", "--help"}, []string{"amf:failed", "bug", "docs", "--help"}, "#7: added docs, --help\n", 1},
		{"remove literal flags", "unlabel", []string{"7", "--", "bug", "--help"}, []string{"amf:failed"}, "#7: removed bug\n", 1},
		{"separator as flag value", "label", []string{"7", "--add", "--", "docs"}, []string{"amf:failed", "bug", "--", "docs"}, "#7: added --, docs\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh, srv := newFakeGitHub(t)
			listJSONEnv(t, srv)
			gh.issues[7] = &Issue{Number: 7, State: "closed", Labels: []Label{{Name: labelFailed}, {Name: "bug"}}}
			var out bytes.Buffer
			err := run(context.Background(), append([]string{"issue", tc.command}, tc.args...), nil, &out, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if got := labelNames(*gh.issues[7]); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("labels = %v, want %v", got, tc.want)
			}
			if out.String() != tc.output || len(gh.writes) != tc.writes {
				t.Fatalf("output = %q, writes = %v; want %q, %d writes", &out, gh.writes, tc.output, tc.writes)
			}
		})
	}
}

func TestDoctorLabelState(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	listJSONEnv(t, srv)
	gh.prs = []map[string]string{
		{"head": "amf/issue-1", "state": "merged"},
		{"head": "amf/issue-2", "state": "open"},
		{"head": "amf/issue-4", "state": "merged"},
	}
	gh.drafts = make([]bool, len(gh.prs))
	gh.issues[1] = &Issue{Number: 1, State: "open", Labels: []Label{{Name: labelFailed}}} // merged → done
	gh.issues[2] = &Issue{Number: 2, State: "open", Labels: []Label{{Name: labelDone}}}   // open PR → pr-open
	gh.issues[3] = &Issue{Number: 3, State: "open", Labels: []Label{{Name: labelPROpen}}} // no PR → failed
	gh.issues[4] = &Issue{Number: 4, State: "closed", Labels: []Label{{Name: labelDone}}} // fine

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	g := newGitHub(cfg)
	check := func(fix bool) string {
		var b bytes.Buffer
		d := &doctor{w: tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)}
		d.checkLabelState(context.Background(), g, fix)
		d.w.Flush()
		return b.String()
	}
	out := check(false)
	for _, w := range []string{"#1 is amf:failed", "should be amf:done", "#2 is amf:done", "should be amf:pr-open", "#3 is amf:pr-open", "should be amf:failed"} {
		if !strings.Contains(out, w) {
			t.Errorf("report lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "#4") || len(gh.writes) != 0 {
		t.Fatalf("report %s writes %v", out, gh.writes)
	}
	out = check(true)
	if !strings.Contains(out, "relabeled") {
		t.Fatalf("fix output %s", out)
	}
	for n, want := range map[int]string{1: labelDone, 2: labelPROpen, 3: labelFailed, 4: labelDone} {
		if got := labelNames(*gh.issues[n]); !reflect.DeepEqual(got, []string{want}) {
			t.Errorf("issue %d labels %v, want %s", n, got, want)
		}
	}
	if out = check(false); !strings.HasPrefix(out, "ok") {
		t.Fatalf("after fix %s", out)
	}
}
