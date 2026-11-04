package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// defaultClientID is the GitHub OAuth app amf auth login uses when neither
// --client-id nor AMF_GITHUB_CLIENT_ID is set. Release builds may set it:
//
//	go build -ldflags "-X main.defaultClientID=Iv1.abc123" ./cmd/amf
var defaultClientID = ""

// defaultScopes are the OAuth scopes amf auth login asks for: repo covers
// the issues, labels, pull requests and merges amf makes. A merge of a PR
// that changes .github/workflows also needs workflow (--scopes "repo workflow").
const defaultScopes = "repo"

// ghCommand is the gh binary resolveToken asks for a token, and ghTimeout
// how long it may take. Tests point ghCommand at a fake.
var (
	ghCommand = "gh"
	ghTimeout = 3 * time.Second
)

// oauthBaseURL is where host's OAuth device flow endpoints live. Tests
// point it at a fake server.
var oauthBaseURL = func(host string) string { return "https://" + host }

// pollWait sleeps d between device flow polls; tests make it instant.
var pollWait = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

var errNoToken = errors.New("GITHUB_TOKEN is not set, and neither gh nor amf auth login has a token: run amf auth login, or set GITHUB_TOKEN (amf init writes .env)")

// authHost is the GitHub host an API URL belongs to: github.com for
// https://api.github.com, else the URL's host (GitHub Enterprise Server
// serves its API at https://<host>/api/v3).
func authHost(apiURL string) string {
	u, err := url.Parse(apiURL)
	if err != nil || u.Host == "" {
		return "github.com"
	}
	if h := strings.ToLower(u.Host); h != "api.github.com" {
		return h
	}
	return "github.com"
}

// apiURLForHost is the REST API URL for host.
func apiURLForHost(host string) string {
	if host == "github.com" {
		return "https://api.github.com"
	}
	return "https://" + host + "/api/v3"
}

// resolveToken finds the GitHub token, first match wins:
//  1. GITHUB_TOKEN, then GH_TOKEN, from the environment or .env
//  2. `gh auth token --hostname host`, if gh is on PATH
//  3. the token amf auth login saved for host
//
// It returns the token and where it came from, or "", "".
func resolveToken(get func(key, def string) string, seen map[string]configValue, envFile, host string) (string, string) {
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := get(k, ""); v != "" {
			if seen[k].From == "env" {
				return v, k + " from the environment"
			}
			return v, k + " from " + envFile
		}
	}
	src := ""
	tok := ghToken(host)
	if tok != "" {
		src = "gh auth token"
	} else if h, path, _ := savedToken(host); h.Token != "" {
		tok, src = h.Token, "amf auth login ("+path+")"
	}
	if tok != "" && seen != nil {
		seen["GITHUB_TOKEN"] = configValue{src, tok}
	}
	return tok, src
}

// ghToken asks gh for its token for host, or returns "" on any error.
func ghToken(host string) string {
	path, err := exec.LookPath(ghCommand)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "auth", "token", "--hostname", host).Output()
	if err != nil {
		return ""
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" || strings.ContainsAny(tok, " \t\r\n") {
		return ""
	}
	return tok
}

// configDir is where amf keeps credentials: AMF_CONFIG_DIR, else
// <user config dir>/amf. Unlike amf's other paths it doesn't default to
// the current directory, so a token is never left in a checkout.
func configDir() (string, error) {
	if d := os.Getenv("AMF_CONFIG_DIR"); d != "" {
		return filepath.Abs(d)
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("can't find the user config folder (set AMF_CONFIG_DIR): %w", err)
	}
	return filepath.Join(d, "amf"), nil
}

// hostsPath is the file amf auth login saves tokens to.
func hostsPath() (string, error) {
	d, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "hosts.json"), nil
}

// savedHost is one host's entry in hosts.json.
type savedHost struct {
	User   string `json:"user,omitempty"`
	Token  string `json:"oauth_token"`
	Scopes string `json:"scopes,omitempty"`
}

func readHosts() (map[string]savedHost, string, error) {
	path, err := hostsPath()
	if err != nil {
		return nil, "", err
	}
	hosts := map[string]savedHost{}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return hosts, path, nil
	}
	if err != nil {
		return nil, path, err
	}
	if err := json.Unmarshal(b, &hosts); err != nil {
		return nil, path, fmt.Errorf("%s: %w", path, err)
	}
	return hosts, path, nil
}

// savedToken is the entry amf auth login saved for host, if any.
func savedToken(host string) (savedHost, string, error) {
	hosts, path, err := readHosts()
	if err != nil {
		return savedHost{}, path, err
	}
	return hosts[host], path, nil
}

// writeHosts saves hosts with mode 0600 in a 0700 folder, replacing the
// file atomically; with no hosts left it removes the file.
func writeHosts(hosts map[string]savedHost) (string, error) {
	path, err := hostsPath()
	if err != nil {
		return "", err
	}
	if len(hosts) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return path, err
		}
		return path, nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return path, err
	}
	os.Chmod(dir, 0o700)
	b, err := json.MarshalIndent(hosts, "", "  ")
	if err != nil {
		return path, err
	}
	f, err := os.CreateTemp(dir, ".hosts-*.json")
	if err != nil {
		return path, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		f.Close()
		return path, err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return path, err
	}
	if err := f.Close(); err != nil {
		return path, err
	}
	return path, os.Rename(tmp, path)
}

// githubUser is what GET /user says about a token.
type githubUser struct {
	Login  string
	Scopes string // the X-OAuth-Scopes header; "" for fine-grained and app tokens
}

// fetchUser checks token against apiURL's GET /user.
func fetchUser(ctx context.Context, apiURL, token string) (githubUser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimSuffix(apiURL, "/")+"/user", nil)
	if err != nil {
		return githubUser{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return githubUser{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return githubUser{}, &APIError{Status: resp.StatusCode, Method: "GET", Path: "/user", Message: http.StatusText(resp.StatusCode)}
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return githubUser{}, err
	}
	return githubUser{Login: u.Login, Scopes: resp.Header.Get("X-OAuth-Scopes")}, nil
}

// tokenType names a GitHub token by its prefix.
func tokenType(tok string) string {
	switch {
	case strings.HasPrefix(tok, "github_pat_"):
		return "fine-grained personal access token"
	case strings.HasPrefix(tok, "ghp_"):
		return "classic personal access token"
	case strings.HasPrefix(tok, "gho_"):
		return "OAuth token"
	case strings.HasPrefix(tok, "ghu_"):
		return "GitHub App user token"
	case strings.HasPrefix(tok, "ghs_"):
		return "GitHub App installation token"
	}
	return "unknown"
}

// deviceResponse is GitHub's answer to /login/device/code and to each
// /login/oauth/access_token poll.
type deviceResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
	AccessToken     string `json:"access_token"`
	Scope           string `json:"scope"`
	Error           string `json:"error"`
	ErrorDesc       string `json:"error_description"`
}

func postForm(ctx context.Context, u string, form url.Values) (deviceResponse, error) {
	var r deviceResponse
	req, err := http.NewRequestWithContext(ctx, "POST", u, strings.NewReader(form.Encode()))
	if err != nil {
		return r, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(body, &r); err != nil {
		return r, fmt.Errorf("POST %s: %s: unexpected response", u, resp.Status)
	}
	if r.Error == "" && resp.StatusCode != http.StatusOK {
		return r, fmt.Errorf("POST %s: %s", u, resp.Status)
	}
	return r, nil
}

// deviceFlow runs GitHub's OAuth device flow against base and returns the
// access token.
func deviceFlow(ctx context.Context, base, clientID, scopes string, w io.Writer, browser bool) (string, error) {
	code, err := postForm(ctx, base+"/login/device/code", url.Values{"client_id": {clientID}, "scope": {scopes}})
	if err != nil {
		return "", err
	}
	if code.Error != "" {
		if code.Error == "device_flow_disabled" {
			return "", errors.New("the OAuth app doesn't allow the device flow: check Enable Device Flow in its settings on GitHub")
		}
		return "", fmt.Errorf("GitHub refused the device code: %s", cmpOr(code.ErrorDesc, code.Error))
	}
	uri := cmpOr(code.VerificationURI, base+"/login/device")
	fmt.Fprintf(w, "First copy your one-time code: %s\nThen open %s in a browser and enter it.\n", code.UserCode, uri)
	if browser && openBrowser(uri) == nil {
		fmt.Fprintln(w, "(Opened it in your browser.)")
	}
	fmt.Fprintln(w, "Waiting for you to authorize amf...")
	interval := time.Duration(code.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expires := time.Duration(code.ExpiresIn) * time.Second
	if expires <= 0 {
		expires = 15 * time.Minute
	}
	var waited time.Duration
	for {
		if waited >= expires {
			return "", errors.New("the code expired before it was authorized; run amf auth login again")
		}
		if err := pollWait(ctx, interval); err != nil {
			return "", err
		}
		waited += interval
		r, err := postForm(ctx, base+"/login/oauth/access_token", url.Values{
			"client_id":   {clientID},
			"device_code": {code.DeviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		})
		if err != nil {
			return "", err
		}
		switch r.Error {
		case "":
			if r.AccessToken == "" {
				return "", errors.New("GitHub sent no access token")
			}
			return r.AccessToken, nil
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
			if n := time.Duration(r.Interval) * time.Second; n > interval {
				interval = n
			}
		case "expired_token":
			return "", errors.New("the code expired before it was authorized; run amf auth login again")
		case "access_denied":
			return "", errors.New("authorization was denied on GitHub")
		default:
			return "", fmt.Errorf("GitHub refused the login: %s", cmpOr(r.ErrorDesc, r.Error))
		}
	}
}

// openBrowser opens u in the default browser, best effort.
func openBrowser(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

// configuredAPIURL is AMF_API_URL from the environment or the first .env
// in paths, without the rest of loadConfig.
func configuredAPIURL(paths []string) string {
	if v := os.Getenv("AMF_API_URL"); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		vars, err := parseDotEnv(f)
		f.Close()
		if err == nil && vars["AMF_API_URL"] != "" && !isPlaceholder(vars["AMF_API_URL"]) {
			return strings.TrimSuffix(vars["AMF_API_URL"], "/")
		}
		break
	}
	return "https://api.github.com"
}

var authSubcommands = []string{"login", "status", "logout", "token", "help"}

const authUsage = `Usage:
  amf auth login [--host host] [--client-id id] [--scopes "repo"] [--no-browser]
  amf auth status [--host host]
  amf auth logout [--host host]
  amf auth token [--host host] [--force]

Gets amf a GitHub token. amf uses the first of:
  1. GITHUB_TOKEN, then GH_TOKEN, from the environment or .env
  2. gh auth token, if the GitHub CLI is installed and logged in
  3. the token amf auth login saved
and otherwise stops, saying to run amf auth login or set GITHUB_TOKEN.

  login   log in with GitHub's device flow and save the token
  status  which token amf uses, its account, scopes and type (never the token)
  logout  delete the token amf auth login saved
  token   print the token amf uses, only when stdout isn't a terminal or
          with --force

The host defaults to the one AMF_API_URL points at (github.com).

amf auth login saves tokens to hosts.json, mode 0600, in AMF_CONFIG_DIR or
else amf's folder in the user config directory (~/.config/amf on Linux,
~/Library/Application Support/amf on macOS, %AppData%\amf on Windows).
Unlike amf's other paths, this doesn't default to the current directory, so
a token never lands in a checkout.

login needs a GitHub OAuth app with the device flow enabled: pass its
client ID with --client-id or AMF_GITHUB_CLIENT_ID. To register one: GitHub
Settings > Developer settings > OAuth Apps > New OAuth App, any homepage
and callback URL, check Enable Device Flow, then copy the Client ID (no
client secret is needed).

See also: amf help env, amf help init, amf help doctor
`

const authLoginHelp = `Usage:
  amf auth login [flags]

Logs in to GitHub with the OAuth device flow: amf prints a one-time code,
opens https://github.com/login/device (--no-browser skips that), and waits
while you enter the code and authorize amf. It then checks the token with
GitHub and saves it (see amf help auth for where).

The OAuth app's client ID comes from --client-id, else AMF_GITHUB_CLIENT_ID,
else one built into amf. --scopes defaults to repo; add workflow to let amf
merge pull requests that change .github/workflows.
`

const authStatusHelp = `Usage:
  amf auth status [flags]

Shows which token amf uses and where it came from, the GitHub account, the
token's scopes and its type. It never prints the token.
`

const authLogoutHelp = `Usage:
  amf auth logout [flags]

Deletes the token amf auth login saved for the host. Tokens from the
environment, .env or gh are left alone.
`

const authTokenHelp = `Usage:
  amf auth token [flags]

Prints the token amf uses, for scripts: GH_TOKEN=$(amf auth token). It
refuses to print to a terminal unless --force is given.
`

func init() {
	subHelp["auth login"] = authLoginHelp
	subHelp["auth status"] = authStatusHelp
	subHelp["auth logout"] = authLogoutHelp
	subHelp["auth token"] = authTokenHelp
	helpExamples["auth login"] = "  amf auth login --client-id Ov23liABC123\n  amf auth login --scopes \"repo workflow\" --no-browser\n"
	helpExamples["auth status"] = "  amf auth status\n"
	helpExamples["auth logout"] = "  amf auth logout\n"
	helpExamples["auth token"] = "  GH_TOKEN=$(amf auth token) gh pr list\n"
}

func cmdAuth(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || isHelpArgs(args) {
		w := stdout
		if len(args) == 0 {
			w = stderr
		}
		fmt.Fprint(w, authUsage)
		if len(args) == 0 {
			return errUsage
		}
		return nil
	}
	sub, rest := args[0], args[1:]
	if sub == "help" {
		fmt.Fprint(stdout, authUsage)
		return nil
	}
	if sub != "login" && sub != "status" && sub != "logout" && sub != "token" {
		fmt.Fprintf(stderr, "amf auth: unknown subcommand %q\n", sub)
		if s := suggestFrom(sub, authSubcommands); s != "" {
			fmt.Fprintf(stderr, "Did you mean %q?\n", "auth "+s)
		}
		fmt.Fprintf(stderr, "\n%s", authUsage)
		return errUsage
	}
	fs := newCommandFlags("auth "+sub, stderr)
	host := fs.String("host", "", "the GitHub `host` (default: AMF_API_URL's, github.com)")
	var clientID, scopes *string
	var noBrowser, force *bool
	switch sub {
	case "login":
		clientID = fs.String("client-id", "", "the OAuth app's client `id` (default: AMF_GITHUB_CLIENT_ID)")
		scopes = fs.String("scopes", defaultScopes, "the OAuth `scopes` to ask for, separated by spaces or commas")
		noBrowser = fs.Bool("no-browser", false, "don't open the browser; just print the link")
	case "token":
		force = fs.Bool("force", false, "print the token even to a terminal")
	}
	pos, err := parseArgs(fs, rest)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return errUsage
	}
	if len(pos) != 0 {
		fmt.Fprintf(stderr, "amf auth %s takes no arguments\n", sub)
		return errUsage
	}
	ep := envPaths(ctx)
	apiURL := configuredAPIURL(ep)
	if h := strings.ToLower(strings.TrimSpace(*host)); h != "" && h != authHost(apiURL) {
		*host, apiURL = h, apiURLForHost(h)
	} else {
		*host = authHost(apiURL)
	}
	switch sub {
	case "login":
		return authLogin(ctx, *host, apiURL, *clientID, *scopes, !*noBrowser, stdout)
	case "logout":
		return authLogout(*host, stdout)
	}
	tok, src, err := currentToken(ep, *host, apiURL)
	if err != nil {
		return err
	}
	if sub == "token" {
		if writerIsTerminal(stdout) && !*force {
			return errors.New("refusing to print the token to a terminal; pipe it, or pass --force")
		}
		_, err := fmt.Fprintln(stdout, tok)
		return err
	}
	return authStatus(ctx, *host, apiURL, tok, src, stdout)
}

// currentToken is the token amf would use for host. GITHUB_TOKEN and
// GH_TOKEN go with AMF_API_URL's host; another host only has gh and amf
// auth login.
func currentToken(ep []string, host, apiURL string) (string, string, error) {
	var tok, src string
	if host == authHost(configuredAPIURL(ep)) {
		cfg, err := loadConfig(ep...)
		if cfg.Token == "" && err != nil && !errors.Is(err, errNoToken) && cfg.Claude == "" {
			return "", "", err // .env didn't parse
		}
		tok, src = cfg.Token, cfg.TokenSource
	} else {
		tok, src = resolveToken(func(string, string) string { return "" }, nil, "", host)
	}
	if tok == "" {
		return "", "", fmt.Errorf("not logged in to %s: run amf auth login, or set GITHUB_TOKEN", host)
	}
	return tok, src, nil
}

func clientIDHelp() error {
	return errors.New(`no OAuth client ID: amf auth login needs a GitHub OAuth app with the device flow enabled.
Register one: GitHub Settings > Developer settings > OAuth Apps > New OAuth App
(https://github.com/settings/applications/new); any homepage and callback URL
will do; check "Enable Device Flow"; register it and copy its Client ID (no
client secret is needed). Then run amf auth login --client-id <id>, or set
AMF_GITHUB_CLIENT_ID=<id> in the environment.`)
}

func authLogin(ctx context.Context, host, apiURL, clientID, scopes string, browser bool, w io.Writer) error {
	clientID = cmpOr(cmpOr(clientID, os.Getenv("AMF_GITHUB_CLIENT_ID")), defaultClientID)
	if clientID == "" {
		return clientIDHelp()
	}
	scopes = strings.Join(strings.FieldsFunc(scopes, func(r rune) bool { return r == ',' || r == ' ' }), " ")
	tok, err := deviceFlow(ctx, strings.TrimSuffix(oauthBaseURL(host), "/"), clientID, scopes, w, browser)
	if err != nil {
		return err
	}
	u, err := fetchUser(ctx, apiURL, tok)
	if err != nil {
		return fmt.Errorf("GitHub issued a token, but checking it failed: %w", err)
	}
	hosts, _, err := readHosts()
	if err != nil {
		return err
	}
	hosts[host] = savedHost{User: u.Login, Token: tok, Scopes: u.Scopes}
	path, err := writeHosts(hosts)
	if err != nil {
		return fmt.Errorf("saving the token: %w", err)
	}
	fmt.Fprintf(w, "Logged in to %s as @%s; saved the token to %s\n", host, u.Login, path)
	if os.Getenv("GITHUB_TOKEN") != "" || os.Getenv("GH_TOKEN") != "" {
		fmt.Fprintln(w, "Note: GITHUB_TOKEN or GH_TOKEN is set, and amf uses it before this token.")
	} else if ghToken(host) != "" {
		fmt.Fprintln(w, "Note: gh is logged in, and amf uses its token before this one.")
	}
	return nil
}

func authLogout(host string, w io.Writer) error {
	hosts, path, err := readHosts()
	if err != nil {
		return err
	}
	if _, ok := hosts[host]; !ok {
		fmt.Fprintf(w, "No saved amf auth login token for %s.\n", host)
		return nil
	}
	delete(hosts, host)
	if _, err := writeHosts(hosts); err != nil {
		return err
	}
	fmt.Fprintf(w, "Deleted the saved token for %s from %s\n", host, path)
	return nil
}

func authStatus(ctx context.Context, host, apiURL, tok, src string, w io.Writer) error {
	fmt.Fprintf(w, "%s\n  token source: %s\n  token type:   %s\n", host, src, tokenType(tok))
	u, err := fetchUser(ctx, apiURL, tok)
	if err != nil {
		fmt.Fprintf(w, "  account:      unknown (%v)\n", err)
		return fmt.Errorf("the token from %s doesn't work: %w", src, err)
	}
	scopes := "none reported (fine-grained and app tokens have permissions instead)"
	if u.Scopes != "" {
		list := strings.FieldsFunc(u.Scopes, func(r rune) bool { return r == ',' || r == ' ' })
		sort.Strings(list)
		scopes = strings.Join(list, ", ")
	}
	fmt.Fprintf(w, "  account:      @%s\n  scopes:       %s\n", u.Login, scopes)
	return nil
}

// hasScope reports whether the X-OAuth-Scopes list has scope.
func hasScope(list, scope string) bool {
	for _, s := range strings.FieldsFunc(list, func(r rune) bool { return r == ',' || r == ' ' }) {
		if s == scope {
			return true
		}
	}
	return false
}

// writerIsTerminal reports whether w is a terminal.
func writerIsTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isTerminal(f)
}
