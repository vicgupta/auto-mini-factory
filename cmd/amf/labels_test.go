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
