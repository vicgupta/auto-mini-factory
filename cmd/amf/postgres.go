package main

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"
)

// amf has no dependencies, so this is the little of the PostgreSQL wire
// protocol (version 3) it needs to write logs: TLS, password, MD5 and
// SCRAM-SHA-256 authentication, simple queries, and parameterized inserts.

// pgConfig is a parsed postgres:// URL.
type pgConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	Database string // the URL's database; "" if it names none
	SSLMode  string // disable, allow, prefer, require, verify-ca or verify-full
	Timeout  time.Duration
}

// parsePostgresURL parses postgres://user:password@host:port/database?sslmode=...
// Like libpq, it defaults to localhost:5432, the OS user, and sslmode=prefer.
func parsePostgresURL(s string) (pgConfig, error) {
	u, err := url.Parse(s)
	if err != nil {
		return pgConfig{}, errors.New("not a valid URL")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return pgConfig{}, fmt.Errorf("must start with postgres:// or postgresql://, not %s:", u.Scheme)
	}
	c := pgConfig{Host: u.Hostname(), Port: u.Port(), SSLMode: "prefer", Timeout: 10 * time.Second}
	if c.Host == "" {
		c.Host = "localhost"
	}
	if c.Port == "" {
		c.Port = "5432"
	}
	if u.User != nil {
		c.User = u.User.Username()
		c.Password, _ = u.User.Password()
	}
	if c.User == "" {
		if me, err := user.Current(); err == nil {
			c.User = me.Username
		}
	}
	c.Database = strings.TrimPrefix(u.Path, "/")
	for k, v := range u.Query() {
		switch k {
		case "sslmode":
			switch v[0] {
			case "disable", "allow", "prefer", "require", "verify-ca", "verify-full":
				c.SSLMode = v[0]
			default:
				return pgConfig{}, fmt.Errorf("unknown sslmode %q", v[0])
			}
		case "connect_timeout":
			n, err := strconv.Atoi(v[0])
			if err != nil || n < 0 {
				return pgConfig{}, fmt.Errorf("connect_timeout must be a number of seconds, got %q", v[0])
			}
			if n > 0 {
				c.Timeout = time.Duration(n) * time.Second
			}
		case "application_name":
		default:
			return pgConfig{}, fmt.Errorf("amf doesn't support the %s parameter", k)
		}
	}
	return c, nil
}

// String is the server and user, without the password.
func (c pgConfig) String() string {
	return c.User + "@" + net.JoinHostPort(c.Host, c.Port)
}

// pgError is an ErrorResponse from the server.
type pgError struct {
	Code    string // SQLSTATE
	Message string
}

func (e *pgError) Error() string { return e.Message + " (SQLSTATE " + e.Code + ")" }

// isPgCode reports whether err is a server error with this SQLSTATE.
func isPgCode(err error, code string) bool {
	var pe *pgError
	return errors.As(err, &pe) && pe.Code == code
}

// pgConn is one connection, used by one goroutine at a time.
type pgConn struct {
	conn    net.Conn
	r       *bufio.Reader
	w       []byte // the messages being built, sent by flush
	timeout time.Duration

	tls  bool   // the connection is encrypted
	auth string // how amf logged in: trust, password, md5 or scram-sha-256
}

// pgConnect connects to database on c's server and logs in.
func pgConnect(ctx context.Context, c pgConfig, database string) (*pgConn, error) {
	d := net.Dialer{Timeout: c.Timeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(c.Host, c.Port))
	if err != nil {
		return nil, err
	}
	p := &pgConn{conn: conn, timeout: c.Timeout}
	if err := p.startup(ctx, c, database); err != nil {
		p.conn.Close()
		return nil, err
	}
	return p, nil
}

func (p *pgConn) Close() error {
	p.w = append(p.w[:0], 'X', 0, 0, 0, 4) // Terminate
	p.conn.SetDeadline(time.Now().Add(time.Second))
	p.conn.Write(p.w)
	return p.conn.Close()
}

func (p *pgConn) startup(ctx context.Context, c pgConfig, database string) error {
	stop := context.AfterFunc(ctx, func() { p.conn.SetDeadline(time.Now()) })
	defer stop()
	p.conn.SetDeadline(time.Now().Add(c.Timeout))
	defer p.conn.SetDeadline(time.Time{})

	if c.SSLMode != "disable" {
		if err := p.startTLS(c); err != nil {
			return err
		}
	}
	p.r = bufio.NewReader(p.conn)

	var b []byte
	b = binary.BigEndian.AppendUint32(b, 196608) // protocol 3.0
	for _, kv := range [][2]string{{"user", c.User}, {"database", database}, {"client_encoding", "UTF8"}, {"application_name", "amf"}} {
		b = append(append(append(append(b, kv[0]...), 0), kv[1]...), 0)
	}
	b = append(b, 0)
	p.w = binary.BigEndian.AppendUint32(p.w[:0], uint32(len(b)+4))
	p.w = append(p.w, b...)
	if err := p.flush(); err != nil {
		return err
	}

	var scram *scramClient
	for {
		t, body, err := p.read()
		if err != nil {
			return err
		}
		switch t {
		case 'E':
			return parsePgError(body)
		case 'Z':
			return nil
		case 'R':
			if len(body) < 4 {
				return errors.New("postgres: short authentication message")
			}
			code, data := binary.BigEndian.Uint32(body), body[4:]
			switch code {
			case 0: // AuthenticationOk
				if p.auth == "" {
					p.auth = "trust"
				}
			case 3: // cleartext password
				p.auth = "password"
				p.password([]byte(c.Password + "\x00"))
			case 5: // MD5
				if len(data) < 4 {
					return errors.New("postgres: short MD5 salt")
				}
				p.auth = "md5"
				inner := md5.Sum([]byte(c.Password + c.User))
				outer := md5.Sum(append([]byte(hex.EncodeToString(inner[:])), data[:4]...))
				p.password([]byte("md5" + hex.EncodeToString(outer[:]) + "\x00"))
			case 10: // SASL
				if !strings.Contains(string(data), "SCRAM-SHA-256\x00") {
					return fmt.Errorf("postgres: the server offers no authentication amf supports (%q)", strings.TrimRight(string(data), "\x00"))
				}
				p.auth = "scram-sha-256"
				scram = newScramClient("", c.Password)
				first := scram.first()
				msg := append([]byte("SCRAM-SHA-256\x00"), binary.BigEndian.AppendUint32(nil, uint32(len(first)))...)
				p.password(append(msg, first...))
			case 11: // SASLContinue
				if scram == nil {
					return errors.New("postgres: unexpected SASL message")
				}
				final, err := scram.final(string(data))
				if err != nil {
					return err
				}
				p.password([]byte(final))
			case 12: // SASLFinal
				if scram == nil || !scram.verify(string(data)) {
					return errors.New("postgres: the server's SCRAM signature is wrong")
				}
				continue
			default:
				return fmt.Errorf("postgres: unsupported authentication method %d", code)
			}
			if code != 0 {
				if c.Password == "" {
					return errors.New("postgres: the server wants a password, and the URL has none")
				}
				if err := p.flush(); err != nil {
					return err
				}
			}
		}
		// ParameterStatus, BackendKeyData and notices need nothing.
	}
}

// startTLS asks the server for TLS, as sslmode says.
func (p *pgConn) startTLS(c pgConfig) error {
	req := binary.BigEndian.AppendUint32(binary.BigEndian.AppendUint32(nil, 8), 80877103)
	if _, err := p.conn.Write(req); err != nil {
		return err
	}
	var ans [1]byte
	if _, err := io.ReadFull(p.conn, ans[:]); err != nil {
		return err
	}
	if ans[0] != 'S' {
		switch c.SSLMode {
		case "allow", "prefer":
			return nil
		}
		return fmt.Errorf("postgres: the server doesn't support TLS, which sslmode=%s requires", c.SSLMode)
	}
	cfg := &tls.Config{ServerName: c.Host, MinVersion: tls.VersionTLS12}
	switch c.SSLMode {
	case "verify-full":
	case "verify-ca":
		// Check the chain but not the name, as libpq does.
		cfg.InsecureSkipVerify = true
		cfg.VerifyConnection = func(cs tls.ConnectionState) error {
			opts := x509.VerifyOptions{Intermediates: x509.NewCertPool()}
			for _, cert := range cs.PeerCertificates[1:] {
				opts.Intermediates.AddCert(cert)
			}
			_, err := cs.PeerCertificates[0].Verify(opts)
			return err
		}
	default: // allow, prefer, require: encrypted, but not verified
		cfg.InsecureSkipVerify = true
	}
	tc := tls.Client(p.conn, cfg)
	if err := tc.Handshake(); err != nil {
		return fmt.Errorf("postgres: TLS: %w", err)
	}
	p.conn = tc
	p.tls = true
	return nil
}

func (p *pgConn) password(data []byte) {
	p.msg('p', data)
}

// msg appends a message to send.
func (p *pgConn) msg(t byte, body []byte) {
	p.w = append(p.w, t)
	p.w = binary.BigEndian.AppendUint32(p.w, uint32(len(body)+4))
	p.w = append(p.w, body...)
}

func (p *pgConn) flush() error {
	_, err := p.conn.Write(p.w)
	p.w = p.w[:0]
	return err
}

// read returns the next message from the server.
func (p *pgConn) read() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(p.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n < 4 || n > 1<<30 {
		return 0, nil, errors.New("postgres: bad message length")
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(p.r, body); err != nil {
		return 0, nil, err
	}
	return hdr[0], body, nil
}

func parsePgError(body []byte) error {
	e := &pgError{}
	for len(body) > 1 {
		field := body[0]
		v, rest, _ := strings.Cut(string(body[1:]), "\x00")
		body = []byte(rest)
		switch field {
		case 'C':
			e.Code = v
		case 'M':
			e.Message = v
		}
	}
	return e
}

// Query runs sql, which may hold several statements, and returns the first
// column of each row it returns. NULLs are "".
func (p *pgConn) Query(ctx context.Context, sql string) ([]string, error) {
	p.msg('Q', append([]byte(sql), 0))
	return p.results(ctx)
}

// pgInsert is an INSERT with $1, $2... and the rows to run it for. A nil
// argument is NULL.
type pgInsert struct {
	sql  string
	rows [][]*string
}

// Insert runs each insert once per row, in one round trip and one
// transaction: either every row is written or none is.
func (p *pgConn) Insert(ctx context.Context, inserts ...pgInsert) error {
	for _, in := range inserts {
		if len(in.rows) == 0 {
			continue
		}
		p.msg('P', append(append([]byte{0}, in.sql...), 0, 0, 0)) // unnamed statement, types inferred
		for _, row := range in.rows {
			b := []byte{0, 0, 0, 0} // unnamed portal and statement, all text
			b = binary.BigEndian.AppendUint16(b, uint16(len(row)))
			for _, v := range row {
				if v == nil {
					b = binary.BigEndian.AppendUint32(b, 0xffffffff)
					continue
				}
				b = binary.BigEndian.AppendUint32(b, uint32(len(*v)))
				b = append(b, *v...)
			}
			p.msg('B', append(b, 0, 0))
			p.msg('E', []byte{0, 0, 0, 0, 0})
		}
	}
	p.msg('S', nil)
	_, err := p.results(ctx)
	return err
}

// results sends what's queued and reads until the server is ready again.
func (p *pgConn) results(ctx context.Context) ([]string, error) {
	stop := context.AfterFunc(ctx, func() { p.conn.SetDeadline(time.Now()) })
	defer stop()
	p.conn.SetDeadline(time.Now().Add(p.timeout))
	defer p.conn.SetDeadline(time.Time{})
	if err := p.flush(); err != nil {
		return nil, err
	}
	var rows []string
	var qerr error
	for {
		t, body, err := p.read()
		if err != nil {
			return nil, err
		}
		switch t {
		case 'E':
			if qerr == nil {
				qerr = parsePgError(body)
			}
		case 'D':
			if len(body) >= 6 && binary.BigEndian.Uint16(body) > 0 {
				if n := int32(binary.BigEndian.Uint32(body[2:])); n >= 0 && int(n) <= len(body)-6 {
					rows = append(rows, string(body[6:6+n]))
				} else {
					rows = append(rows, "")
				}
			}
		case 'Z':
			return rows, qerr
		}
	}
}

// pgQuoteIdent quotes a name for SQL, such as a database name.
func pgQuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// scramClient is the client side of SCRAM-SHA-256 (RFC 5802, RFC 7677),
// without channel binding.
type scramClient struct {
	user, password, nonce string
	firstBare, authMsg    string
	saltedPassword        []byte
}

func newScramClient(user, password string) *scramClient {
	b := make([]byte, 18)
	rand.Read(b)
	return &scramClient{user: user, password: password, nonce: base64.RawStdEncoding.EncodeToString(b)}
}

func (s *scramClient) first() string {
	s.firstBare = "n=" + s.user + ",r=" + s.nonce
	return "n,," + s.firstBare
}

func (s *scramClient) final(serverFirst string) (string, error) {
	var nonce, salt string
	iter := 0
	for _, f := range strings.Split(serverFirst, ",") {
		k, v, _ := strings.Cut(f, "=")
		switch k {
		case "r":
			nonce = v
		case "s":
			salt = v
		case "i":
			iter, _ = strconv.Atoi(v)
		}
	}
	saltBytes, err := base64.StdEncoding.DecodeString(salt)
	if err != nil || !strings.HasPrefix(nonce, s.nonce) || iter <= 0 {
		return "", errors.New("postgres: bad SCRAM challenge from the server")
	}
	s.saltedPassword, err = pbkdf2.Key(sha256.New, s.password, saltBytes, iter, sha256.Size)
	if err != nil {
		return "", err
	}
	withoutProof := "c=biws,r=" + nonce // biws is base64("n,,")
	s.authMsg = s.firstBare + "," + serverFirst + "," + withoutProof
	clientKey := hmacSHA256(s.saltedPassword, "Client Key")
	stored := sha256.Sum256(clientKey)
	sig := hmacSHA256(stored[:], s.authMsg)
	for i := range clientKey {
		clientKey[i] ^= sig[i]
	}
	return withoutProof + ",p=" + base64.StdEncoding.EncodeToString(clientKey), nil
}

func (s *scramClient) verify(serverFinal string) bool {
	if s.saltedPassword == nil {
		return false
	}
	want := hmacSHA256(hmacSHA256(s.saltedPassword, "Server Key"), s.authMsg)
	got, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(strings.TrimSpace(serverFinal), "v="))
	return err == nil && hmac.Equal(got, want)
}

func hmacSHA256(key []byte, msg string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(msg))
	return h.Sum(nil)
}

// hostname names this machine in the logs.
func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}
