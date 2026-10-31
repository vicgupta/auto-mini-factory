package main

import (
	"archive/tar"
	"archive/zip"
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
	"strconv"
	"strings"
	"time"
)

// releasesURL is where amf's own releases live. AMF_RELEASES_URL, from the
// environment only, overrides it, for a mirror or tests.
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
	*target = strings.TrimSpace(*target)
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
	u := &updater{base: base, current: buildVersion(), exe: exe, out: stdout, goos: runtime.GOOS, goarch: runtime.GOARCH}
	return u.run(ctx, *target, *check)
}

type updater struct {
	base    string // https://github.com/owner/name
	current string // running version
	exe     string // path of the binary to replace
	out     io.Writer
	// goos and goarch pick the release archive, normally runtime.GOOS and
	// runtime.GOARCH. Tests set them to exercise another platform's path.
	goos, goarch string
}

// releaseArchive names the release files for a platform: the folder inside
// the archive, the archive itself (a .zip on Windows, else a .tar.gz) and the
// binary in the folder (amf.exe on Windows, else amf).
func releaseArchive(tag, goos, goarch string) (folder, file, bin string) {
	folder = fmt.Sprintf("amf_%s_%s_%s", tag, goos, goarch)
	if goos == "windows" {
		return folder, folder + ".zip", "amf.exe"
	}
	return folder, folder + ".tar.gz", "amf"
}

func (u *updater) run(ctx context.Context, target string, check bool) error {
	pinned := target != ""
	if !pinned {
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
	if check && pinned && olderRelease(target, u.current) {
		fmt.Fprintf(u.out, "amf %s is older than this build (%s); amf update --version %s would downgrade\n", target, u.current, target)
		return nil
	}
	if check {
		fmt.Fprintf(u.out, "amf %s is available (you have %s); run amf update\n", target, u.current)
		return nil
	}

	fmt.Fprintf(u.out, "Downloading amf %s for %s/%s\n", target, u.goos, u.goarch)
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

// olderRelease reports whether release tag a is older than release tag b.
// It is false unless both are vX.Y.Z.
func olderRelease(a, b string) bool {
	if !releaseTag.MatchString(a) || !releaseTag.MatchString(b) {
		return false
	}
	pa, pb := strings.Split(a[1:], "."), strings.Split(b[1:], ".")
	for i := range pa {
		na, _ := strconv.Atoi(pa[i])
		nb, _ := strconv.Atoi(pb[i])
		if na != nb {
			return na < nb
		}
	}
	return false
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
	folder, file, bin := releaseArchive(tag, u.goos, u.goarch)
	dl := u.base + "/releases/download/" + tag + "/"
	archive, err := fetch(ctx, dl+file)
	if isStatus(err, http.StatusNotFound) {
		return nil, fmt.Errorf("no amf %s for %s/%s; see %s/releases", tag, u.goos, u.goarch, u.base)
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
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == file {
			want = f[0]
		}
	}
	if want == "" {
		return nil, fmt.Errorf("%s is not listed in the release's checksums.txt", file)
	}
	if sum := sha256.Sum256(archive); hex.EncodeToString(sum[:]) != want {
		return nil, fmt.Errorf("checksum mismatch for %s; not updating", file)
	}
	if strings.HasSuffix(file, ".zip") {
		return fromZip(archive, file, folder+"/"+bin)
	}
	return fromTarGz(archive, file, folder+"/"+bin)
}

// fromTarGz returns the regular file at path in a .tar.gz archive.
func fromTarGz(archive []byte, file, path string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("%s has no %s", file, path)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		if h.Name == path && h.Typeflag == tar.TypeReg {
			return io.ReadAll(io.LimitReader(tr, 200<<20))
		}
	}
}

// fromZip returns the regular file at path in a .zip archive.
func fromZip(archive []byte, file, path string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	for _, f := range zr.File {
		if f.Name != path || !f.Mode().IsRegular() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, 200<<20))
	}
	return nil, fmt.Errorf("%s has no %s", file, path)
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
// the expected version, then puts it in place of the old one.
func (u *updater) install(ctx context.Context, bin []byte, tag string) error {
	dir := filepath.Dir(u.exe)
	windows := u.goos == "windows"
	pattern := ".amf-update-*"
	if windows {
		pattern += ".exe"         // Windows only runs files with an executable extension
		os.Remove(u.exe + ".old") // left by the previous update; best effort
	}
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			hint := "e.g. sudo amf update"
			if windows {
				hint = "e.g. from a terminal run as administrator"
			}
			return fmt.Errorf("can't write to %s; re-run with permission to it (%s)", dir, hint)
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
	if !windows {
		return os.Rename(tmp.Name(), u.exe)
	}
	// Windows can't replace a running .exe, but it can rename one. Move the
	// old binary aside to amf.exe.old (removed by the next update), then put
	// the new one in its place, restoring the old one if that fails.
	old := u.exe + ".old"
	if err := os.Rename(u.exe, old); err != nil {
		return fmt.Errorf("moving the running amf aside: %w", err)
	}
	if err := os.Rename(tmp.Name(), u.exe); err != nil {
		if rerr := os.Rename(old, u.exe); rerr != nil {
			return fmt.Errorf("installing the new amf: %v; the old one is at %s (%v)", err, old, rerr)
		}
		return fmt.Errorf("installing the new amf: %w", err)
	}
	return nil
}
