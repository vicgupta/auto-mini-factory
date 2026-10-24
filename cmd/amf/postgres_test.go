package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// RFC 7677's example exchange, with its fixed client nonce.
func TestScramSHA256(t *testing.T) {
	s := &scramClient{user: "user", password: "pencil", nonce: "rOprNGfwEbeRWgbNEkqO"}
	if got := s.first(); got != "n,,n=user,r=rOprNGfwEbeRWgbNEkqO" {
		t.Fatalf("first = %q", got)
	}
	final, err := s.final("r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096")
	if err != nil {
		t.Fatal(err)
	}
	if want := "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="; final != want {
		t.Fatalf("final = %q, want %q", final, want)
	}
	if !s.verify("v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=") {
		t.Error("the RFC's server signature didn't verify")
	}
	if s.verify("v=" + base64.StdEncoding.EncodeToString([]byte("forged"))) {
		t.Error("a wrong server signature verified")
	}
	// A server nonce that doesn't extend the client's is an attack.
	if _, err := (&scramClient{nonce: "abc"}).final("r=xyz,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"); err == nil {
		t.Error("accepted a server nonce that doesn't start with the client's")
	}
}

func TestParsePostgresURL(t *testing.T) {
	c, err := parsePostgresURL("postgresql://amf:p%40ss@db.example.com:6543/app?sslmode=require&connect_timeout=3")
	if err != nil {
		t.Fatal(err)
	}
	want := pgConfig{Host: "db.example.com", Port: "6543", User: "amf", Password: "p@ss", Database: "app", SSLMode: "require", Timeout: 3 * time.Second}
	if c != want {
		t.Errorf("got %+v, want %+v", c, want)
	}
	if c.String() != "amf@db.example.com:6543" || strings.Contains(c.String(), "p@ss") {
		t.Errorf("String() = %q", c.String())
	}
	c, err = parsePostgresURL("postgres://u@[::1]")
	if err != nil || c.Host != "::1" || c.Port != "5432" || c.SSLMode != "prefer" || c.Database != "" {
		t.Errorf("defaults: %+v, %v", c, err)
	}
	for _, bad := range []string{"mysql://u@h/db", "postgres://u@h/db?sslmode=maybe", "postgres://u@h/db?host=/tmp", "postgres://u@h/db?connect_timeout=x"} {
		if _, err := parsePostgresURL(bad); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}

func TestLogIssue(t *testing.T) {
	for path, want := range map[string]int{"/r/.amf/logs/issue-12.log": 12, "issue-7-code.log": 7, "other.log": 0} {
		if got := logIssue(path); got != want {
			t.Errorf("logIssue(%q) = %d, want %d", path, got, want)
		}
	}
}

func TestLineWriter(t *testing.T) {
	var got []string
	w := &lineWriter{emit: func(s string) { got = append(got, s) }}
	w.Write([]byte("one\r\ntw"))
	w.Write([]byte("o\n\nthree"))
	w.Flush()
	if want := []string{"one", "two", "", "three"}; strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestPostgresServer runs against a real server, given a URL for one whose
// user may create databases: AMF_TEST_POSTGRES_URL=postgres://... go test.
// It drops the amflogs database first.
func TestPostgresServer(t *testing.T) {
	url := os.Getenv("AMF_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AMF_TEST_POSTGRES_URL is not set")
	}
	c, err := parsePostgresURL(url)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	boot := c.Database
	if boot == "" {
		boot = "postgres"
	}
	admin, err := pgConnect(ctx, c, boot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Query(ctx, "DROP DATABASE IF EXISTS amflogs WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	admin.Close()

	var local bytes.Buffer
	d := startDBLog(ctx, c, "o/r", &local, nil)
	if !strings.Contains(local.String(), "created database amflogs") {
		t.Fatalf("startup: %q", local.String())
	}
	w := d.amfWriter()
	w.Write([]byte("amf: #12: Fix it's \"quoted\" title\n"))
	w.Write([]byte("amf: watching o/r\n"))
	aw := d.agentWriter(12, "claude", "issue-12.log")
	aw.Write([]byte("{\"type\":\"x\"}\nbad \x00 utf8 \xff\nno newline"))
	aw.Flush()
	d.Close(30 * time.Second)

	conn, created, err := openLogDatabase(ctx, c, true)
	if err != nil || created {
		t.Fatalf("reopen: created=%v, %v", created, err)
	}
	defer conn.Close()
	rows, err := conn.Query(ctx, "SELECT coalesce(issue::text, '-') || ' ' || message FROM amf_log ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{`12 #12: Fix it's "quoted" title`, "- watching o/r"}; strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Errorf("amf_log = %q, want %q", rows, want)
	}
	rows, err = conn.Query(ctx, "SELECT issue || ' ' || agent || ' ' || log_file || ' ' || line FROM agent_log ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{`12 claude issue-12.log {"type":"x"}`, "12 claude issue-12.log bad  utf8 �", "12 claude issue-12.log no newline"}; strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Errorf("agent_log = %q, want %q", rows, want)
	}
}

// TestPostgresServerRun processes an issue with logging to a real server
// (see TestPostgresServer), and checks both tables have its lines.
func TestPostgresServerRun(t *testing.T) {
	url := os.Getenv("AMF_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AMF_TEST_POSTGRES_URL is not set")
	}
	c, err := parsePostgresURL(url)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := setupRepo(t)
	gh, srv := newFakeGitHub(t)
	gh.issues[1] = readyIssue(1, "Add feature file")
	r, log := newTestRunner(t, srv, root, fakeClaude(t, `
cat >/dev/null; echo hello > feature.txt
echo '{"type":"system","subtype":"init"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"Added it."}'
`))
	r.Repo = "vic/run-" + time.Now().Format("150405.000000")
	ctx := context.Background()
	r.DB = startDBLog(ctx, c, r.Repo, log, nil)
	r.Log = io.MultiWriter(log, r.DB.amfWriter())
	if err := r.Run(ctx, 1); err != nil {
		t.Fatalf("Run: %v\nlog:\n%s", err, log)
	}
	r.Close()
	if strings.Contains(log.String(), "warning") {
		t.Fatalf("log:\n%s", log)
	}

	conn, _, err := openLogDatabase(ctx, c, false)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	amf, err := conn.Query(ctx, "SELECT message FROM amf_log WHERE repo = '"+r.Repo+"' AND issue = 1 ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(amf) < 3 || !strings.HasPrefix(amf[len(amf)-1], "#1: opened ") || !slices.ContainsFunc(amf, func(s string) bool { return strings.HasPrefix(s, "#1: running claude") }) {
		t.Errorf("amf_log = %q\nlog:\n%s", amf, log)
	}
	agent, err := conn.Query(ctx, "SELECT agent || ' ' || log_file || ' ' || line FROM agent_log WHERE repo = '"+r.Repo+"' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	if len(agent) != 2 || agent[0] != `claude issue-1.log {"type":"system","subtype":"init"}` {
		t.Errorf("agent_log = %q", agent)
	}
}
