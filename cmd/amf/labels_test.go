package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"text/tabwriter"
)

func TestSetIssueStatusAddsBeforeRemoving(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, State: "open", Labels: []Label{{Name: labelFailed}, {Name: "bug"}}}
	g := newGitHub(Config{Token: "test-token", Repo: "vic/demo", APIURL: srv.URL})

	changed, err := g.SetIssueStatus(context.Background(), 1, labelDone)
	if err != nil || !changed {
		t.Fatalf("changed=%t err=%v", changed, err)
	}
	if len(gh.writes) != 2 || gh.writes[0] != "POST /repos/vic/demo/issues/1/labels" || !strings.HasPrefix(gh.writes[1], "DELETE /repos/vic/demo/issues/1/labels/") || !strings.HasSuffix(gh.writes[1], labelFailed) {
		t.Fatalf("writes = %v", gh.writes)
	}
	if got := labelNames(*gh.issues[1]); !reflect.DeepEqual(got, []string{"bug", labelDone}) {
		t.Fatalf("labels = %v", got)
	}

	gh.writes = nil
	changed, err = g.SetIssueStatus(context.Background(), 1, labelDone)
	if err != nil || changed || len(gh.writes) != 0 {
		t.Fatalf("unchanged: changed=%t err=%v writes=%v", changed, err, gh.writes)
	}
}

func TestSetIssueStatusFailureKeepsAStatusLabel(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(*http.Request) bool
		want []string
	}{
		{"add fails", func(r *http.Request) bool {
			return r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/issues/1/labels")
		}, []string{labelFailed, "bug"}},
		{"remove fails", func(r *http.Request) bool {
			return r.Method == "DELETE" && strings.HasSuffix(r.URL.Path, "failed")
		}, []string{labelDone, labelFailed, "bug"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh, srv := newFakeGitHub(t)
			gh.issues[1] = &Issue{Number: 1, State: "open", Labels: []Label{{Name: labelFailed}, {Name: "bug"}}}
			flaky := failingGitHub(t, srv, tc.fail)
			g := newGitHub(Config{Token: "test-token", Repo: "vic/demo", APIURL: flaky.URL})
			if _, err := g.SetIssueStatus(context.Background(), 1, labelDone); err == nil {
				t.Fatal("no error")
			}
			if got := gh.labels(1); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("labels = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRunnerSetStatusAddsBeforeRemoving(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = &Issue{Number: 1, State: "open", Labels: []Label{{Name: labelInProgress}, {Name: "bug"}}}
	r := &Runner{GH: newGitHub(Config{Token: "test-token", Repo: "vic/demo", APIURL: srv.URL})}

	if err := r.setStatus(context.Background(), 1, labelDone); err != nil {
		t.Fatal(err)
	}
	if len(gh.writes) != 4 || gh.writes[0] != "POST /repos/vic/demo/issues/1/labels" {
		t.Fatalf("writes = %v", gh.writes)
	}
	for _, w := range gh.writes[1:] {
		if !strings.HasPrefix(w, "DELETE /repos/vic/demo/issues/1/labels/") {
			t.Fatalf("writes = %v", gh.writes)
		}
	}
	if got := labelNames(*gh.issues[1]); !reflect.DeepEqual(got, []string{"bug", labelDone}) {
		t.Fatalf("labels = %v", got)
	}

	gh.issues[2] = &Issue{Number: 2, State: "open", Labels: []Label{{Name: labelInProgress}, {Name: "bug"}}}
	flaky := failingGitHub(t, srv, func(r *http.Request) bool {
		return r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/issues/2/labels")
	})
	r.GH = newGitHub(Config{Token: "test-token", Repo: "vic/demo", APIURL: flaky.URL})
	if err := r.setStatus(context.Background(), 2, labelDone); err == nil {
		t.Fatal("no error")
	}
	if got := gh.labels(2); !reflect.DeepEqual(got, []string{labelInProgress, "bug"}) {
		t.Fatalf("labels after failed add = %v", got)
	}
}

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

func TestIssueLabelStatusAddsBeforeRemoving(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	listJSONEnv(t, srv)
	gh.issues[7] = &Issue{Number: 7, State: "open", Labels: []Label{{Name: labelFailed}, {Name: "bug"}}}

	var out bytes.Buffer
	if err := run(context.Background(), []string{"issue", "label", "7", labelDone}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(gh.writes) != 2 || gh.writes[0] != "POST /repos/vic/demo/issues/7/labels" || !strings.HasPrefix(gh.writes[1], "DELETE /repos/vic/demo/issues/7/labels/") || !strings.HasSuffix(gh.writes[1], labelFailed) {
		t.Fatalf("writes = %v", gh.writes)
	}
	if got := labelNames(*gh.issues[7]); !reflect.DeepEqual(got, []string{"bug", labelDone}) {
		t.Fatalf("labels = %v", got)
	}
}

func TestIssueBulkLabels(t *testing.T) {
	for _, tc := range []struct {
		name, command string
		args          []string
		changed       []int
		output        string
	}{
		{"queue unlabeled", "label", []string{"--state", " OPEN ", "--unlabeled", labelReady}, []int{1, 5}, "#5: added amf:ready\n#1: added amf:ready\n"},
		{"unlabeled defaults open", "label", []string{"--unlabeled", labelReady}, []int{1, 5}, "#5: added amf:ready\n#1: added amf:ready\n"},
		{"closed", "label", []string{"--state", "closed", labelReady}, []int{3}, "#3: added amf:ready\n"},
		{"existing label", "unlabel", []string{"--label", " bug ", "bug"}, []int{2}, "#2: removed bug\n"},
		{"all states", "unlabel", []string{"--state", "all", "--label", "bug", "bug"}, []int{2, 3}, "#3: removed bug\n#2: removed bug\n"},
		{"unchanged", "label", []string{"--label", "bug", "--add", "bug"}, nil, "#2 unchanged\n"},
		{"no matches", "label", []string{"--label", "missing", labelReady}, nil, "No matching issues in vic/demo.\n"},
		{"literal labels", "label", []string{"--unlabeled", "--", "--help", "docs"}, []int{1, 5}, "#5: added --help, docs\n#1: added --help, docs\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh, srv := newFakeGitHub(t)
			listJSONEnv(t, srv)
			gh.issues[1] = &Issue{Number: 1, State: "open"}
			gh.issues[2] = &Issue{Number: 2, State: "open", Labels: []Label{{Name: "bug"}}}
			gh.issues[3] = &Issue{Number: 3, State: "closed", Labels: []Label{{Name: "bug"}}}
			gh.issues[4] = &Issue{Number: 4, State: "open", PullRequest: &struct{}{}}
			gh.issues[5] = &Issue{Number: 5, State: "open"}
			before := map[int][]string{}
			for n, is := range gh.issues {
				before[n] = labelNames(*is)
			}
			var out, stderr bytes.Buffer
			if err := run(context.Background(), append([]string{"issue", tc.command}, tc.args...), nil, &out, &stderr); err != nil {
				t.Fatal(err)
			}
			if out.String() != tc.output || stderr.Len() != 0 {
				t.Fatalf("stdout=%q stderr=%q, want %q", &out, &stderr, tc.output)
			}
			var changed []int
			for n := 1; n <= len(gh.issues); n++ {
				if !reflect.DeepEqual(before[n], labelNames(*gh.issues[n])) {
					changed = append(changed, n)
				}
			}
			if !reflect.DeepEqual(changed, tc.changed) || len(gh.writes) != len(tc.changed) {
				t.Fatalf("changed=%v writes=%v, want %v", changed, gh.writes, tc.changed)
			}
		})
	}
}

func TestIssueBulkLabelsStatusReplacement(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	listJSONEnv(t, srv)
	gh.issues[1] = &Issue{Number: 1, State: "open", Labels: []Label{{Name: labelFailed}, {Name: "bug"}}}
	gh.issues[2] = &Issue{Number: 2, State: "closed", Labels: []Label{{Name: labelPROpen}, {Name: "bug"}}}
	gh.issues[3] = &Issue{Number: 3, State: "open", Labels: []Label{{Name: labelDone}, {Name: "bug"}}}
	var out bytes.Buffer
	args := []string{"issue", "label", "--state", "all", "--add", labelDone}
	if err := run(context.Background(), args, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if out.String() != "#3 unchanged\n#2: added amf:done; removed amf:pr-open\n#1: added amf:done; removed amf:failed\n" {
		t.Fatalf("output=%q", &out)
	}
	for n, is := range gh.issues {
		if got := labelNames(*is); !reflect.DeepEqual(got, []string{"bug", labelDone}) && !reflect.DeepEqual(got, []string{labelDone, "bug"}) {
			t.Errorf("#%d labels=%v", n, got)
		}
	}
	gh.writes, out = nil, bytes.Buffer{}
	if err := run(context.Background(), args, nil, &out, io.Discard); err != nil || len(gh.writes) != 0 || out.String() != "#3 unchanged\n#2 unchanged\n#1 unchanged\n" {
		t.Fatalf("err=%v writes=%v out=%q", err, gh.writes, &out)
	}
}

func TestIssueBulkLabelsPagination(t *testing.T) {
	gh, srv := newFakeGitHub(t)
	listJSONEnv(t, srv)
	for n := 1; n <= 101; n++ {
		gh.issues[n] = &Issue{Number: n, State: "open"}
	}
	if err := run(context.Background(), []string{"issue", "label", "--unlabeled", labelReady}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	for n, is := range gh.issues {
		if !is.HasLabel(labelReady) {
			t.Errorf("#%d was not labeled", n)
		}
	}
	if len(gh.writes) != 101 {
		t.Fatalf("writes=%d, want 101", len(gh.writes))
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
