package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// releasesURL is where amf's own releases live. AMF_RELEASES_URL overrides it (for tests).
const releasesURL = "https://github.com/vicgupta/auto-mini-factory"

// cmdUpdate replaces the running amf binary with a release from GitHub.
func cmdUpdate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	target := fs.String("version", "", "release to install, e.g. v0.0.7 (default: latest)")
	check := fs.Bool("check", false, "only report whether an update is available")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return errUsage
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "usage: amf update [--version vX.Y.Z] [--check]")
		return errUsage
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	base := releasesURL
	if v := os.Getenv("AMF_RELEASES_URL"); v != "" {
		base = strings.TrimSuffix(v, "/")
	}
	u := &updater{base: base, current: buildVersion(), exe: exe, out: stdout}
	return u.run(ctx, *target, *check)
}

type updater struct {
	base    string // https://github.com/owner/name
	current string // running version
	exe     string // path of the binary to replace
	out     io.Writer
}

func (u *updater) run(ctx context.Context, target string, check bool) error {
	if target == "" {
		latest, err := u.latest(ctx)
		if err != nil {
			return err
		}
		target = latest
	} else if !strings.HasPrefix(target, "v") {
		target = "v" + target
	}

	if target == u.current {
		fmt.Fprintf(u.out, "amf %s is up to date\n", u.current)
		return nil
	}
	if check {
		fmt.Fprintf(u.out, "amf %s is available (you have %s); run amf update\n", target, u.current)
		return nil
	}

	fmt.Fprintf(u.out, "Downloading amf %s for %s/%s\n", target, runtime.GOOS, runtime.GOARCH)
	bin, err := u.download(ctx, target)
	if err != nil {
		return err
	}
	if err := u.install(ctx, bin, target); err != nil {
		return err
	}
	fmt.Fprintf(u.out, "Updated amf %s -> %s at %s\n", u.current, target, u.exe)
	return nil
}

// latest is the tag that /releases/latest redirects to.
func (u *updater) latest(ctx context.Context) (string, error) {
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u.base+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("finding the latest release: %w", err)
	}
	resp.Body.Close()
	tag := path.Base(resp.Header.Get("Location"))
	if resp.StatusCode/100 != 3 || !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("finding the latest release: unexpected response %s from %s/releases/latest", resp.Status, u.base)
	}
	return tag, nil
}

// download fetches the release archive for this platform, verifies it
// against the release's checksums.txt, and returns the amf binary inside.
func (u *updater) download(ctx context.Context, tag string) ([]byte, error) {
	name := fmt.Sprintf("amf_%s_%s_%s", tag, runtime.GOOS, runtime.GOARCH)
	dl := u.base + "/releases/download/" + tag + "/"
	archive, err := fetch(ctx, dl+name+".tar.gz")
	if isStatus(err, http.StatusNotFound) {
		return nil, fmt.Errorf("no amf %s for %s/%s; see %s/releases", tag, runtime.GOOS, runtime.GOARCH, u.base)
	}
	if err != nil {
		return nil, err
	}
	sums, err := fetch(ctx, dl+"checksums.txt")
	if err != nil {
		return nil, err
	}

	want := ""
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == name+".tar.gz" {
			want = f[0]
		}
	}
	if want == "" {
		return nil, fmt.Errorf("%s.tar.gz is not listed in the release's checksums.txt", name)
	}
	if sum := sha256.Sum256(archive); hex.EncodeToString(sum[:]) != want {
		return nil, fmt.Errorf("checksum mismatch for %s.tar.gz; not updating", name)
	}

	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("%s.tar.gz: %w", name, err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s.tar.gz has no %s/amf", name, name)
		}
		if err != nil {
			return nil, fmt.Errorf("%s.tar.gz: %w", name, err)
		}
		if h.Name == name+"/amf" && h.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, 200<<20))
		}
	}
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{Method: "GET", Path: url, Status: resp.StatusCode, Message: resp.Status}
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

// install writes bin next to the running binary, checks it runs and reports
// the expected version, then renames it over the old one.
func (u *updater) install(ctx context.Context, bin []byte, tag string) error {
	dir := filepath.Dir(u.exe)
	tmp, err := os.CreateTemp(dir, ".amf-update-*")
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("can't write to %s; re-run with permission to it (e.g. sudo amf update)", dir)
		}
		return err
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(bin)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}

	out, err := exec.CommandContext(ctx, tmp.Name(), "version").Output()
	if err != nil {
		return fmt.Errorf("the downloaded amf %s doesn't run here: %w", tag, err)
	}
	if got := strings.TrimSpace(string(out)); got != "amf "+tag {
		return fmt.Errorf("the downloaded binary reports %q, expected %q; not updating", got, "amf "+tag)
	}
	return os.Rename(tmp.Name(), u.exe)
}
