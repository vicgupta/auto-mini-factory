package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	releasePlanSchema    = "amf-release-plan/v1"
	releaseRecordSchema  = "amf-release-ownership/v1"
	releasePolicyVersion = 1
	releaseRecordMarker  = "<!-- " + releaseRecordSchema
	publishClaimAsset    = "amf-publish-claim-v1.json"

	maxPlanBytes       = int64(1) << 20
	maxManifestBytes   = int64(1) << 20
	maxPolicyBytes     = int64(1) << 20
	maxRecordBytes     = int64(1) << 20
	maxArtifactBytes   = int64(4) << 30
	maxTotalArtifacts  = int64(16) << 30
	maxDownloadedAsset = int64(4) << 30
)

// releasePlan is the local, explicit input for create, stage and publish.
// Paths are relative to the plan file's directory.
type releasePlan struct {
	Schema    string         `json:"schema"`
	Tag       string         `json:"tag"`
	Commit    string         `json:"commit"`
	Name      string         `json:"name"`
	Notes     string         `json:"notes"`
	Policy    string         `json:"policy"`
	Artifacts []planArtifact `json:"artifacts"`
}

type planArtifact struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// releaseRecord is the durable ownership record stored in the draft body.
// It binds the repository, tag, immutable commit, target policy and exact
// artifact plan, so every retry can compare the same plan instead of trusting
// an arbitrary existing draft.
type releaseRecord struct {
	Schema     string           `json:"schema"`
	Repo       string           `json:"repo"`
	Tag        string           `json:"tag"`
	Commit     string           `json:"commit"`
	Name       string           `json:"name"`
	Notes      string           `json:"notes"`
	PolicyPath string           `json:"policy_path"`
	Policy     releasePolicy    `json:"policy"`
	Artifacts  []recordArtifact `json:"artifacts"`
}

type recordArtifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// releasePolicy comes from the exact target commit, never from a moving
// branch or a local override.
type releasePolicy struct {
	Version          int            `json:"version"`
	RequiredChecks   []policyCheck  `json:"required_checks"`
	RequiredStatuses []policyStatus `json:"required_statuses"`
}

type policyCheck struct {
	Name  string `json:"name"`
	AppID int64  `json:"app_id"`
}

type policyStatus struct {
	Context   string `json:"context"`
	CreatorID int64  `json:"creator_id"`
}

type artifactSnapshot struct {
	Art  recordArtifact
	File string
}

type artifactStager struct {
	dir string
}

func newArtifactStager() (*artifactStager, error) {
	dir, err := os.MkdirTemp("", "amf-release-*")
	if err != nil {
		return nil, err
	}
	return &artifactStager{dir: dir}, nil
}

func (s *artifactStager) Close() {
	if s != nil && s.dir != "" {
		os.RemoveAll(s.dir)
	}
}

// decodeStrictJSON rejects duplicate keys, unknown fields and trailing data.
func decodeStrictJSON(data []byte, v any) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON data")
		}
		return err
	}
	return nil
}

// rejectDuplicateJSONKeys walks the token stream because encoding/json
// silently keeps the last duplicate object key.
func rejectDuplicateJSONKeys(data []byte) error {
	type frame struct {
		object    bool
		keys      map[string]bool
		expectKey bool
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var stack []*frame
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, &frame{object: true, keys: map[string]bool{}, expectKey: true})
			case '[':
				stack = append(stack, &frame{})
			case '}', ']':
				if len(stack) == 0 {
					return errors.New("unexpected closing JSON delimiter")
				}
				stack = stack[:len(stack)-1]
				if len(stack) > 0 && stack[len(stack)-1].object {
					stack[len(stack)-1].expectKey = true
				}
			}
		default:
			if len(stack) == 0 {
				continue
			}
			top := stack[len(stack)-1]
			if !top.object {
				continue
			}
			if top.expectKey {
				key, ok := tok.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				if top.keys[key] {
					return fmt.Errorf("duplicate JSON key %q", key)
				}
				top.keys[key] = true
				top.expectKey = false
			} else {
				top.expectKey = true
			}
		}
	}
}

func readBoundedFile(path string, max int64, what string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s %s is larger than %d bytes", what, path, max)
	}
	return b, nil
}

func loadReleasePlan(planPath, manifestPath string) (releasePlan, []artifactSnapshot, *artifactStager, error) {
	var plan releasePlan
	data, err := readBoundedFile(planPath, maxPlanBytes, "release plan")
	if err != nil {
		return plan, nil, nil, err
	}
	if err := decodeStrictJSON(data, &plan); err != nil {
		return plan, nil, nil, fmt.Errorf("release plan %s: %w", planPath, err)
	}
	if plan.Schema != releasePlanSchema {
		return plan, nil, nil, fmt.Errorf("release plan %s: schema must be %q", planPath, releasePlanSchema)
	}
	plan.Tag = strings.TrimSpace(plan.Tag)
	if err := validateTagName(plan.Tag); err != nil {
		return plan, nil, nil, fmt.Errorf("release plan %s: %w", planPath, err)
	}
	commit, err := validateFullCommitSHA(plan.Commit)
	if err != nil {
		return plan, nil, nil, fmt.Errorf("release plan %s: %w", planPath, err)
	}
	plan.Commit = commit
	plan.Name = strings.TrimSpace(plan.Name)
	if plan.Name == "" {
		plan.Name = plan.Tag
	}
	plan.Policy = strings.TrimSpace(plan.Policy)
	if err := validatePolicyPath(plan.Policy); err != nil {
		return plan, nil, nil, fmt.Errorf("release plan %s: %w", planPath, err)
	}
	if strings.Contains(plan.Notes, releaseRecordMarker) || strings.Contains(plan.Notes, "-->") {
		return plan, nil, nil, fmt.Errorf("release plan %s: notes can't contain the ownership marker", planPath)
	}
	if len(plan.Artifacts) == 0 {
		return plan, nil, nil, fmt.Errorf("release plan %s: artifacts must list at least one file", planPath)
	}

	planDir := filepath.Dir(planPath)
	manifestFile := manifestPath
	if !filepath.IsAbs(manifestFile) {
		manifestFile = filepath.Join(planDir, manifestFile)
	}
	manifestData, err := readBoundedFile(manifestFile, maxManifestBytes, "checksum manifest")
	if err != nil {
		return plan, nil, nil, err
	}
	manifest, err := parseChecksumManifest(manifestData)
	if err != nil {
		return plan, nil, nil, fmt.Errorf("checksum manifest %s: %w", manifestPath, err)
	}

	seen := map[string]bool{}
	artifacts := make([]recordArtifact, 0, len(plan.Artifacts))
	var total int64
	for i := range plan.Artifacts {
		art := &plan.Artifacts[i]
		art.Path = strings.TrimSpace(art.Path)
		art.Name = strings.TrimSpace(art.Name)
		if err := validateStagingPath(art.Path); err != nil {
			return plan, nil, nil, fmt.Errorf("release plan %s: artifact %q: %w", planPath, art.Name, err)
		}
		if err := validateAssetName(art.Name); err != nil {
			return plan, nil, nil, fmt.Errorf("release plan %s: %w", planPath, err)
		}
		key := strings.ToLower(art.Name)
		if seen[key] {
			return plan, nil, nil, fmt.Errorf("release plan %s: duplicate artifact name %q", planPath, art.Name)
		}
		seen[key] = true
		sum, ok := manifest[key]
		if !ok {
			return plan, nil, nil, fmt.Errorf("checksum manifest %s has no entry for artifact %q", manifestPath, art.Name)
		}
		local := art.Path
		if !filepath.IsAbs(local) {
			local = filepath.Join(planDir, local)
		}
		if err := rejectSymlinkPath(planDir, local, art.Name); err != nil {
			return plan, nil, nil, err
		}
		size, err := snapshotAndVerify(local, art.Name, sum)
		if err != nil {
			return plan, nil, nil, err
		}
		total += size
		if total > maxTotalArtifacts {
			return plan, nil, nil, fmt.Errorf("release plan %s: artifacts total more than %d bytes", planPath, maxTotalArtifacts)
		}
		artifacts = append(artifacts, recordArtifact{Name: art.Name, SHA256: sum})
	}
	if len(manifest) != len(plan.Artifacts) {
		var extra []string
		for key := range manifest {
			found := false
			for _, art := range plan.Artifacts {
				if strings.ToLower(art.Name) == key {
					found = true
					break
				}
			}
			if !found {
				extra = append(extra, key)
			}
		}
		sort.Strings(extra)
		return plan, nil, nil, fmt.Errorf("checksum manifest %s lists artifacts not in the plan: %s", manifestPath, strings.Join(extra, ", "))
	}

	stager, err := newArtifactStager()
	if err != nil {
		return plan, nil, nil, err
	}
	snapshots := make([]artifactSnapshot, 0, len(plan.Artifacts))
	for i, art := range plan.Artifacts {
		local := art.Path
		if !filepath.IsAbs(local) {
			local = filepath.Join(planDir, local)
		}
		if err := rejectSymlinkPath(planDir, local, art.Name); err != nil {
			stager.Close()
			return plan, nil, nil, err
		}
		snap, err := snapshotArtifact(stager, local, art.Name, manifest[strings.ToLower(art.Name)])
		if err != nil {
			stager.Close()
			return plan, nil, nil, err
		}
		artifacts[i].Size = snap.Art.Size
		snapshots = append(snapshots, snap)
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Name < artifacts[j].Name })
	return plan, snapshots, stager, nil
}

// parseChecksumManifest reads sha256sum-style lines. It keeps names with
// spaces, rejects duplicate or ambiguous names, and lower-cases the hex.
func parseChecksumManifest(data []byte) (map[string]string, error) {
	out := map[string]string{}
	originals := map[string]string{}
	for lineNo, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) < 66 || line[64] != ' ' {
			return nil, fmt.Errorf("line %d is not <sha256>  <name>", lineNo+1)
		}
		sum := strings.ToLower(line[:64])
		if !isSHA256(sum) {
			return nil, fmt.Errorf("line %d has invalid SHA-256 %q", lineNo+1, line[:64])
		}
		name := strings.TrimLeft(line[64:], " ")
		name = strings.TrimPrefix(name, "*")
		if name == "" || strings.TrimSpace(name) != name {
			return nil, fmt.Errorf("line %d has an empty or ambiguous name", lineNo+1)
		}
		if err := validateAssetName(name); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo+1, err)
		}
		key := strings.ToLower(name)
		if prev, ok := originals[key]; ok {
			return nil, fmt.Errorf("line %d duplicates artifact %q (also %q)", lineNo+1, name, prev)
		}
		originals[key] = name
		out[key] = sum
	}
	if len(out) == 0 {
		return nil, errors.New("no checksums found")
	}
	return out, nil
}

func isSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			if r < 'a' || r > 'f' {
				return false
			}
		}
	}
	return true
}

func validateStagingPath(p string) error {
	if p == "" {
		return errors.New("path can't be empty")
	}
	if filepath.IsAbs(p) || filepath.VolumeName(p) != "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return fmt.Errorf("path %q must be relative", p)
	}
	if strings.ContainsAny(p, "\x00\\") {
		return fmt.Errorf("path %q contains an unsafe character", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return fmt.Errorf("path %q escapes its directory", p)
		}
	}
	return nil
}

func validateAssetName(name string) error {
	if name == "" {
		return errors.New("artifact name can't be empty")
	}
	if len(name) > 255 {
		return fmt.Errorf("artifact name %q is longer than 255 bytes", name)
	}
	if name != filepath.Base(name) || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("artifact name %q must be one file name, not a path", name)
	}
	if name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return fmt.Errorf("artifact name %q is reserved or ambiguous", name)
	}
	if strings.TrimSpace(name) != name {
		return fmt.Errorf("artifact name %q has leading or trailing whitespace", name)
	}
	if strings.EqualFold(name, publishClaimAsset) {
		return fmt.Errorf("artifact name %q is reserved for publication bookkeeping", name)
	}
	for _, r := range name {
		if r < ' ' || r == 0x7f {
			return fmt.Errorf("artifact name %q contains a control character", name)
		}
	}
	return nil
}

func validatePolicyPath(p string) error {
	p = strings.TrimSpace(p)
	if p == "" {
		return errors.New("policy path can't be empty")
	}
	if filepath.IsAbs(p) || filepath.VolumeName(p) != "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") {
		return fmt.Errorf("policy path %q must be a relative repository path", p)
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("policy path %q is not a clean repository path", p)
		}
	}
	return nil
}

// rejectSymlinkPath refuses a symlink anywhere between the plan directory and
// the artifact, so a symlinked parent cannot redirect reads outside it.
func rejectSymlinkPath(planDir, local, name string) error {
	rel, err := filepath.Rel(planDir, local)
	if err != nil {
		return fmt.Errorf("artifact %q path %s is outside %s: %w", name, local, planDir, err)
	}
	cur := planDir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("artifact %q path includes symlink %s", name, cur)
		}
	}
	return nil
}

// snapshotAndVerify hashes one local file without keeping it, so the plan is
// validated before a draft is created.
func snapshotAndVerify(local, name, want string) (int64, error) {
	dir, err := os.MkdirTemp("", "amf-release-check-*")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(dir)
	snap, err := snapshotArtifact(&artifactStager{dir: dir}, local, name, want)
	return snap.Art.Size, err
}

// snapshotArtifact copies the file into the staging directory while hashing
// it. Lstat plus SameFile rejects symlinks and a rename/replacement race, and
// uploads read only the verified snapshot, never the original path.
func snapshotArtifact(stager *artifactStager, local, name, want string) (artifactSnapshot, error) {
	var empty artifactSnapshot
	info, err := os.Lstat(local)
	if err != nil {
		return empty, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return empty, fmt.Errorf("artifact %q is a symlink: %s", name, local)
	}
	if !info.Mode().IsRegular() {
		return empty, fmt.Errorf("artifact %q is not a regular file: %s", name, local)
	}
	if info.Size() > maxArtifactBytes {
		return empty, fmt.Errorf("artifact %q is larger than %d bytes", name, maxArtifactBytes)
	}
	in, err := os.Open(local)
	if err != nil {
		return empty, err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil {
		return empty, err
	}
	if !os.SameFile(info, opened) || !opened.Mode().IsRegular() {
		return empty, fmt.Errorf("artifact %q changed while it was opened: %s", name, local)
	}
	out, err := os.CreateTemp(stager.dir, "artifact-*")
	if err != nil {
		return empty, err
	}
	ok := false
	defer func() {
		if !ok {
			out.Close()
			os.Remove(out.Name())
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(in, maxArtifactBytes+1))
	if err != nil {
		return empty, err
	}
	if n > maxArtifactBytes {
		return empty, fmt.Errorf("artifact %q is larger than %d bytes", name, maxArtifactBytes)
	}
	if err := out.Close(); err != nil {
		return empty, err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return empty, fmt.Errorf("artifact %q has SHA-256 %s, but the checksum manifest says %s; refusing to stage it", name, got, want)
	}
	ok = true
	return artifactSnapshot{Art: recordArtifact{Name: name, SHA256: got, Size: n}, File: out.Name()}, nil
}

func canonicalPolicy(p releasePolicy) releasePolicy {
	p.RequiredChecks = append([]policyCheck(nil), p.RequiredChecks...)
	p.RequiredStatuses = append([]policyStatus(nil), p.RequiredStatuses...)
	sort.Slice(p.RequiredChecks, func(i, j int) bool {
		if p.RequiredChecks[i].Name != p.RequiredChecks[j].Name {
			return p.RequiredChecks[i].Name < p.RequiredChecks[j].Name
		}
		return p.RequiredChecks[i].AppID < p.RequiredChecks[j].AppID
	})
	sort.Slice(p.RequiredStatuses, func(i, j int) bool {
		if p.RequiredStatuses[i].Context != p.RequiredStatuses[j].Context {
			return p.RequiredStatuses[i].Context < p.RequiredStatuses[j].Context
		}
		return p.RequiredStatuses[i].CreatorID < p.RequiredStatuses[j].CreatorID
	})
	return p
}

func validatePolicy(p releasePolicy) error {
	if p.Version != releasePolicyVersion {
		return fmt.Errorf("policy version must be %d, got %d", releasePolicyVersion, p.Version)
	}
	if len(p.RequiredChecks) == 0 && len(p.RequiredStatuses) == 0 {
		return errors.New("policy lists no required checks or statuses")
	}
	seen := map[string]bool{}
	for _, c := range p.RequiredChecks {
		if strings.TrimSpace(c.Name) == "" || c.AppID <= 0 {
			return fmt.Errorf("policy check %q needs a nonempty name and a positive app_id", c.Name)
		}
		key := fmt.Sprintf("%s\x00%d", strings.ToLower(c.Name), c.AppID)
		if seen[key] {
			return fmt.Errorf("policy lists check %q from app %d more than once", c.Name, c.AppID)
		}
		seen[key] = true
	}
	seen = map[string]bool{}
	for _, s := range p.RequiredStatuses {
		if strings.TrimSpace(s.Context) == "" || s.CreatorID <= 0 {
			return fmt.Errorf("policy status %q needs a nonempty context and a positive creator_id", s.Context)
		}
		key := fmt.Sprintf("%s\x00%d", strings.ToLower(s.Context), s.CreatorID)
		if seen[key] {
			return fmt.Errorf("policy lists status %q from creator %d more than once", s.Context, s.CreatorID)
		}
		seen[key] = true
	}
	return nil
}

func buildReleaseRecord(cfg Config, plan releasePlan, policy releasePolicy, artifacts []recordArtifact) releaseRecord {
	arts := append([]recordArtifact(nil), artifacts...)
	sort.Slice(arts, func(i, j int) bool { return arts[i].Name < arts[j].Name })
	return releaseRecord{
		Schema: releaseRecordSchema, Repo: cfg.Repo, Tag: plan.Tag, Commit: plan.Commit,
		Name: plan.Name, Notes: plan.Notes, PolicyPath: plan.Policy,
		Policy: canonicalPolicy(policy), Artifacts: arts,
	}
}

func encodeRecordBody(rec releaseRecord) (string, error) {
	body, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	if int64(len(body)) > maxRecordBytes {
		return "", fmt.Errorf("ownership record is larger than %d bytes", maxRecordBytes)
	}
	out := releaseRecordMarker + "\n" + string(body) + "\n-->"
	if rec.Notes != "" {
		out += "\n\n" + rec.Notes
	}
	return out, nil
}

func parseRecordBody(body string) (releaseRecord, error) {
	var rec releaseRecord
	prefix := releaseRecordMarker + "\n"
	if !strings.HasPrefix(body, prefix) {
		return rec, errors.New("release has no amf ownership record; refusing to treat it as amf's")
	}
	rest := body[len(prefix):]
	end := strings.Index(rest, "\n-->")
	if end < 0 {
		return rec, errors.New("release ownership record is incomplete")
	}
	data := rest[:end]
	tail := rest[end+len("\n-->"):]
	if strings.Contains(tail, releaseRecordMarker) {
		return rec, errors.New("release body has more than one ownership marker")
	}
	if err := decodeStrictJSON([]byte(data), &rec); err != nil {
		return rec, fmt.Errorf("release ownership record: %w", err)
	}
	if rec.Schema != releaseRecordSchema {
		return rec, fmt.Errorf("release ownership record schema is %q, want %q", rec.Schema, releaseRecordSchema)
	}
	if tail != "" {
		if !strings.HasPrefix(tail, "\n\n") || tail[len("\n\n"):] != rec.Notes {
			return rec, errors.New("release body notes do not match the ownership record")
		}
	} else if rec.Notes != "" {
		return rec, errors.New("release body is missing its recorded notes")
	}
	if err := validateStoredRecord(rec); err != nil {
		return rec, err
	}
	rec.Policy = canonicalPolicy(rec.Policy)
	sort.Slice(rec.Artifacts, func(i, j int) bool { return rec.Artifacts[i].Name < rec.Artifacts[j].Name })
	return rec, nil
}

func validateStoredRecord(rec releaseRecord) error {
	if rec.Repo == "" || rec.Tag == "" {
		return errors.New("release ownership record has no repository or tag")
	}
	if err := validateTagName(rec.Tag); err != nil {
		return err
	}
	if _, err := validateFullCommitSHA(rec.Commit); err != nil {
		return err
	}
	if err := validatePolicyPath(rec.PolicyPath); err != nil {
		return err
	}
	if err := validatePolicy(rec.Policy); err != nil {
		return err
	}
	if len(rec.Artifacts) == 0 {
		return errors.New("release ownership record lists no artifacts")
	}
	seen := map[string]bool{}
	for _, art := range rec.Artifacts {
		if err := validateAssetName(art.Name); err != nil {
			return err
		}
		if !isSHA256(art.SHA256) || art.Size < 0 {
			return fmt.Errorf("release ownership record entry %q has an invalid checksum or size", art.Name)
		}
		key := strings.ToLower(art.Name)
		if seen[key] {
			return fmt.Errorf("release ownership record repeats artifact %q", art.Name)
		}
		seen[key] = true
	}
	return nil
}

func compareReleaseRecords(got, want releaseRecord) error {
	got.Policy = canonicalPolicy(got.Policy)
	want.Policy = canonicalPolicy(want.Policy)
	g, err := json.Marshal(got)
	if err != nil {
		return err
	}
	w, err := json.Marshal(want)
	if err != nil {
		return err
	}
	if !bytes.Equal(g, w) {
		return errors.New("existing release does not match the exact recorded plan, tag, SHA, release identity and target policy; refusing to change it")
	}
	return nil
}

func repoContentPath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// FileAtCommit reads a file through the contents API pinned to an exact
// commit SHA. The response must be a small base64 file.
func (g *GitHub) FileAtCommit(ctx context.Context, path, sha string) ([]byte, error) {
	var file struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		Size     int64  `json:"size"`
		SHA      string `json:"sha"`
	}
	endpoint := g.repoPath("/contents/%s?ref=%s", repoContentPath(path), url.QueryEscape(sha))
	if err := g.do(ctx, "GET", endpoint, nil, &file); err != nil {
		return nil, err
	}
	if file.Type != "file" {
		return nil, fmt.Errorf("%s at %s is not a regular file", path, sha)
	}
	if file.Size > maxPolicyBytes {
		return nil, fmt.Errorf("%s at %s is larger than %d bytes", path, sha, maxPolicyBytes)
	}
	if file.Encoding != "base64" {
		return nil, fmt.Errorf("%s at %s has unsupported encoding %q", path, sha, file.Encoding)
	}
	clean := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', ' ', '\t':
			return -1
		}
		return r
	}, file.Content)
	data, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("%s at %s: %w", path, sha, err)
	}
	if int64(len(data)) > maxPolicyBytes {
		return nil, fmt.Errorf("%s at %s is larger than %d bytes", path, sha, maxPolicyBytes)
	}
	return data, nil
}

func fetchReleasePolicy(ctx context.Context, gh *GitHub, path, sha string) (releasePolicy, error) {
	var policy releasePolicy
	data, err := gh.FileAtCommit(ctx, path, sha)
	if err != nil {
		if isStatus(err, http.StatusNotFound) {
			return policy, fmt.Errorf("required release policy %s was not found at commit %s", path, sha)
		}
		return policy, fmt.Errorf("read required release policy %s at %s: %w", path, sha, err)
	}
	if err := decodeStrictJSON(data, &policy); err != nil {
		return policy, fmt.Errorf("release policy %s at %s: %w", path, sha, err)
	}
	if err := validatePolicy(policy); err != nil {
		return policy, fmt.Errorf("release policy %s at %s: %w", path, sha, err)
	}
	return canonicalPolicy(policy), nil
}

// CreateRelease creates a release through the sanctioned REST client.
func (g *GitHub) CreateRelease(ctx context.Context, in map[string]any) (Release, error) {
	var rel Release
	err := g.do(ctx, "POST", g.repoPath("/releases"), in, &rel)
	return rel, err
}

// GetReleaseByID fetches a release by its explicit identity and checks that
// the response really is that release.
func (g *GitHub) GetReleaseByID(ctx context.Context, id int64) (Release, error) {
	var rel Release
	if err := g.do(ctx, "GET", g.repoPath("/releases/%d", id), nil, &rel); err != nil {
		return rel, err
	}
	if rel.ID != id {
		return rel, fmt.Errorf("GitHub returned release %d for release %d", rel.ID, id)
	}
	return rel, nil
}

// UpdateRelease changes only the fields in in on an existing release ID.
func (g *GitHub) UpdateRelease(ctx context.Context, id int64, in map[string]any) (Release, error) {
	var rel Release
	err := g.do(ctx, "PATCH", g.repoPath("/releases/%d", id), in, &rel)
	return rel, err
}

func (g *GitHub) findReleaseByTag(ctx context.Context, tag string) (Release, bool, error) {
	releases, err := g.ListReleases(ctx)
	if err != nil {
		return Release{}, false, err
	}
	var found Release
	ok := false
	for _, rel := range releases {
		if rel.TagName != tag {
			continue
		}
		if ok {
			return Release{}, false, fmt.Errorf("multiple visible releases have tag %q; refusing an ambiguous release identity", tag)
		}
		found, ok = rel, true
	}
	return found, ok, nil
}

// uploadAssetURL builds GitHub's upload endpoint and verifies that it names
// exactly this repository and release. It never accepts a caller-supplied URL.
func (g *GitHub) uploadAssetURL(releaseID int64, name string) (string, error) {
	base, err := url.Parse(g.base)
	if err != nil {
		return "", err
	}
	u := *base
	suffix := fmt.Sprintf("/repos/%s/releases/%d/assets", g.repo, releaseID)
	switch {
	case strings.EqualFold(base.Host, "api.github.com"):
		u.Scheme = "https"
		u.Host = "uploads.github.com"
		u.Path = suffix
	case strings.HasSuffix(base.Path, "/api/v3"):
		u.Path = strings.TrimSuffix(base.Path, "/api/v3") + "/api/uploads" + suffix
	default:
		u.Path = strings.TrimSuffix(base.Path, "/") + suffix
	}
	if !strings.HasSuffix(u.Path, suffix) {
		return "", fmt.Errorf("upload URL %q does not name release %d", u.String(), releaseID)
	}
	if !strings.EqualFold(u.Host, base.Host) && !(strings.EqualFold(base.Host, "api.github.com") && strings.EqualFold(u.Host, "uploads.github.com")) {
		return "", fmt.Errorf("refusing upload to unapproved host %q", u.Host)
	}
	q := u.Query()
	q.Set("name", name)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// UploadReleaseAsset uploads one snapshot with no redirect following. The
// caller re-reads the release to resolve a lost response.
func (g *GitHub) UploadReleaseAsset(ctx context.Context, releaseID int64, name string, body io.Reader, size int64) (ReleaseAsset, error) {
	var asset ReleaseAsset
	endpoint, err := g.uploadAssetURL(releaseID, name)
	if err != nil {
		return asset, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, body)
	if err != nil {
		return asset, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = size
	client := &http.Client{Timeout: g.http.Timeout, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		return asset, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		msg := string(raw)
		var parsed struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &parsed) == nil && parsed.Message != "" {
			msg = parsed.Message
		}
		return asset, &APIError{Method: "POST", Path: endpoint, Status: resp.StatusCode, Message: msg}
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&asset); err != nil {
		return asset, err
	}
	return asset, nil
}

// DownloadReleaseAsset reads an asset through the API, not its browser URL.
// Redirects are allowed only to the API host or GitHub's asset storage; the
// Authorization header is stripped whenever the host or port changes.
func (g *GitHub) DownloadReleaseAsset(ctx context.Context, assetID int64) ([]byte, error) {
	base, err := url.Parse(g.base)
	if err != nil {
		return nil, err
	}
	endpoint := g.base + g.repoPath("/releases/assets/%d", assetID)
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := &http.Client{Timeout: g.http.Timeout, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many asset download redirects")
		}
		if !strings.EqualFold(req.URL.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "http") {
			return fmt.Errorf("refusing asset download redirect to scheme %q", req.URL.Scheme)
		}
		if strings.EqualFold(base.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
			return fmt.Errorf("refusing asset download redirect that downgrades to %q", req.URL.Scheme)
		}
		host := strings.ToLower(req.URL.Host)
		baseHost := strings.ToLower(base.Host)
		allowed := host == baseHost
		if strings.EqualFold(base.Host, "api.github.com") && strings.HasSuffix(host, ".githubusercontent.com") {
			allowed = true
		}
		if !allowed {
			return fmt.Errorf("refusing asset download redirect to %s", req.URL.Host)
		}
		if host != baseHost {
			req.Header.Del("Authorization")
		}
		return nil
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
		msg := string(raw)
		var parsed struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &parsed) == nil && parsed.Message != "" {
			msg = parsed.Message
		}
		return nil, &APIError{Method: "GET", Path: endpoint, Status: resp.StatusCode, Message: msg}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadedAsset+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxDownloadedAsset {
		return nil, fmt.Errorf("asset %d is larger than %d bytes", assetID, maxDownloadedAsset)
	}
	return data, nil
}

func assetMap(assets []ReleaseAsset) (map[string]ReleaseAsset, error) {
	out := map[string]ReleaseAsset{}
	for _, a := range assets {
		key := strings.ToLower(a.Name)
		if _, ok := out[key]; ok {
			return nil, fmt.Errorf("release has duplicate asset name %q", a.Name)
		}
		out[key] = a
	}
	return out, nil
}

func verifyRemoteArtifacts(ctx context.Context, gh *GitHub, rel Release, want []recordArtifact, allowed map[string][]byte) error {
	assets, err := gh.ReleaseAssets(ctx, rel.ID)
	if err != nil {
		return err
	}
	byName, err := assetMap(assets)
	if err != nil {
		return err
	}
	wantByName := map[string]recordArtifact{}
	for _, art := range want {
		wantByName[strings.ToLower(art.Name)] = art
	}
	for key, asset := range byName {
		if _, ok := wantByName[key]; ok {
			continue
		}
		content, ok := allowed[key]
		if !ok {
			return fmt.Errorf("release %d has unexpected asset %q; refusing to change it", rel.ID, asset.Name)
		}
		if asset.State != "uploaded" {
			return fmt.Errorf("release %d asset %q is %q, not a completed upload; refusing to delete or overwrite it", rel.ID, asset.Name, asset.State)
		}
		got, err := gh.DownloadReleaseAsset(ctx, asset.ID)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, content) {
			return fmt.Errorf("release %d asset %q does not match amf's bookkeeping content", rel.ID, asset.Name)
		}
	}
	for _, art := range want {
		asset, ok := byName[strings.ToLower(art.Name)]
		if !ok {
			return fmt.Errorf("release %d is missing artifact %q", rel.ID, art.Name)
		}
		if asset.State != "uploaded" {
			return fmt.Errorf("release %d artifact %q is %q, not a completed upload; refusing to delete or overwrite it", rel.ID, art.Name, asset.State)
		}
		if asset.Size != art.Size {
			return fmt.Errorf("release %d artifact %q is %d bytes, expected %d", rel.ID, art.Name, asset.Size, art.Size)
		}
		data, err := gh.DownloadReleaseAsset(ctx, asset.ID)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != art.SHA256 {
			return fmt.Errorf("release %d artifact %q has different remote bytes than the recorded plan", rel.ID, art.Name)
		}
	}
	return nil
}

func verifyRemoteArtifact(ctx context.Context, gh *GitHub, rel Release, want recordArtifact) error {
	assets, err := gh.ReleaseAssets(ctx, rel.ID)
	if err != nil {
		return err
	}
	byName, err := assetMap(assets)
	if err != nil {
		return err
	}
	asset, ok := byName[strings.ToLower(want.Name)]
	if !ok {
		return fmt.Errorf("release %d is missing artifact %q", rel.ID, want.Name)
	}
	if asset.State != "uploaded" {
		return fmt.Errorf("release %d artifact %q is %q, not a completed upload; refusing to delete or overwrite it", rel.ID, want.Name, asset.State)
	}
	if asset.Size != want.Size {
		return fmt.Errorf("release %d artifact %q is %d bytes, expected %d", rel.ID, want.Name, asset.Size, want.Size)
	}
	data, err := gh.DownloadReleaseAsset(ctx, asset.ID)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != want.SHA256 {
		return fmt.Errorf("release %d artifact %q has different remote bytes than the recorded plan", rel.ID, want.Name)
	}
	return nil
}

func verifyTagAt(ctx context.Context, gh *GitHub, tag, commit string) error {
	peeled, err := gh.TagCommit(ctx, tag)
	if isStatus(err, http.StatusNotFound) {
		return fmt.Errorf("tag %q does not exist in %s; create it first with amf tag create", tag, gh.repo)
	}
	if err != nil {
		return err
	}
	if peeled != commit {
		return fmt.Errorf("tag %q points to commit %s, not the recorded %s; refusing to continue", tag, peeled, commit)
	}
	return nil
}

func recordFromDraft(rel Release, tag, commit string, plan releasePlan, policy releasePolicy, artifacts []recordArtifact, cfg Config) (releaseRecord, error) {
	rec, err := parseRecordBody(rel.Body)
	if err != nil {
		return rec, err
	}
	want := buildReleaseRecord(cfg, plan, policy, artifacts)
	if err := compareReleaseRecords(rec, want); err != nil {
		return rec, err
	}
	if rec.Tag != tag || rec.Commit != commit {
		return rec, errors.New("release identity does not match the requested tag and commit")
	}
	return rec, nil
}

// ---------- release create ----------

type releaseCreateFlags struct {
	plan     *string
	manifest *string
}

func addReleaseCreateFlags(fs *flag.FlagSet) releaseCreateFlags {
	return releaseCreateFlags{
		plan:     fs.String("plan", "", "read the release plan from this `file`"),
		manifest: fs.String("manifest", "", "read the SHA-256 checksum manifest from this `file`"),
	}
}

func cmdReleaseCreate(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("release create", stderr)
	f := addReleaseCreateFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return errUsage
	}
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" {
		fmt.Fprintln(stderr, "usage: amf release create <tag> --plan <file> --manifest <file>")
		return errUsage
	}
	tag := strings.TrimSpace(pos[0])
	if err := validateTagName(tag); err != nil {
		return err
	}
	if strings.TrimSpace(*f.plan) == "" || strings.TrimSpace(*f.manifest) == "" {
		fmt.Fprintln(stderr, "usage: amf release create <tag> --plan <file> --manifest <file>")
		return errUsage
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newTagReleaseGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	plan, snapshots, stager, err := loadReleasePlan(*f.plan, *f.manifest)
	if err != nil {
		return err
	}
	defer stager.Close()
	if plan.Tag != tag {
		return fmt.Errorf("release plan is for tag %q, not %q", plan.Tag, tag)
	}
	if err := gh.CommitExists(ctx, plan.Commit); err != nil {
		return err
	}
	if err := verifyTagAt(ctx, gh, tag, plan.Commit); err != nil {
		return err
	}
	policy, err := fetchReleasePolicy(ctx, gh, plan.Policy, plan.Commit)
	if err != nil {
		return err
	}
	arts := make([]recordArtifact, 0, len(snapshots))
	for _, snap := range snapshots {
		arts = append(arts, snap.Art)
	}
	return createDraft(ctx, cfg, gh, plan, policy, arts, stdout)
}

func createDraft(ctx context.Context, cfg Config, gh *GitHub, plan releasePlan, policy releasePolicy, arts []recordArtifact, stdout io.Writer) error {
	rec := buildReleaseRecord(cfg, plan, policy, arts)
	body, err := encodeRecordBody(rec)
	if err != nil {
		return err
	}
	if rel, ok, err := gh.findReleaseByTag(ctx, plan.Tag); err != nil {
		return err
	} else if ok {
		if _, err := recordFromDraft(rel, plan.Tag, plan.Commit, plan, policy, arts, cfg); err != nil {
			return fmt.Errorf("existing release %d for tag %q: %w", rel.ID, plan.Tag, err)
		}
		if !rel.Draft {
			return fmt.Errorf("release %d for tag %q is already published; use amf release publish to verify it", rel.ID, plan.Tag)
		}
		if err := verifyTagAt(ctx, gh, plan.Tag, plan.Commit); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Draft release %d already matches tag %s at %s.\n", rel.ID, plan.Tag, plan.Commit)
		return err
	}

	created, err := gh.CreateRelease(ctx, map[string]any{
		"tag_name": plan.Tag, "target_commitish": plan.Commit, "name": rec.Name, "body": body, "draft": true,
	})
	if err != nil {
		// The response may have been lost or a concurrent process may have
		// created the same draft. Re-read and accept only an exact match.
		if rel, ok, readErr := gh.findReleaseByTag(ctx, plan.Tag); readErr == nil && ok {
			if _, recErr := recordFromDraft(rel, plan.Tag, plan.Commit, plan, policy, arts, cfg); recErr == nil && rel.Draft {
				_, outErr := fmt.Fprintf(stdout, "Draft release %d already matches tag %s at %s.\n", rel.ID, plan.Tag, plan.Commit)
				return outErr
			}
		}
		return err
	}
	got, err := gh.GetReleaseByID(ctx, created.ID)
	if err != nil {
		if rel, ok, readErr := gh.findReleaseByTag(ctx, plan.Tag); readErr == nil && ok {
			got = rel
		} else {
			return err
		}
	}
	if got.ID != created.ID || got.TagName != plan.Tag || !got.Draft {
		return fmt.Errorf("GitHub returned an unexpected release %d for tag %q", got.ID, got.TagName)
	}
	if _, err := recordFromDraft(got, plan.Tag, plan.Commit, plan, policy, arts, cfg); err != nil {
		return err
	}
	if err := verifyTagAt(ctx, gh, plan.Tag, plan.Commit); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Created draft release %d for tag %s at %s.\n", got.ID, plan.Tag, plan.Commit)
	return err
}

// ---------- release stage ----------

type releaseStageFlags struct {
	plan     *string
	manifest *string
	release  *int64
}

func addReleaseStageFlags(fs *flag.FlagSet) releaseStageFlags {
	return releaseStageFlags{
		plan:     fs.String("plan", "", "read the release plan from this `file`"),
		manifest: fs.String("manifest", "", "read the SHA-256 checksum manifest from this `file`"),
		release:  fs.Int64("release", 0, "pin the draft release by its numeric `id`"),
	}
}

func cmdReleaseStage(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("release stage", stderr)
	f := addReleaseStageFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return errUsage
	}
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" || *f.release <= 0 || strings.TrimSpace(*f.plan) == "" || strings.TrimSpace(*f.manifest) == "" {
		fmt.Fprintln(stderr, "usage: amf release stage <tag> --release <id> --plan <file> --manifest <file>")
		return errUsage
	}
	tag := strings.TrimSpace(pos[0])
	if err := validateTagName(tag); err != nil {
		return err
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newTagReleaseGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	plan, snapshots, stager, err := loadReleasePlan(*f.plan, *f.manifest)
	if err != nil {
		return err
	}
	defer stager.Close()
	if plan.Tag != tag {
		return fmt.Errorf("release plan is for tag %q, not %q", plan.Tag, tag)
	}
	arts := make([]recordArtifact, 0, len(snapshots))
	for _, snap := range snapshots {
		arts = append(arts, snap.Art)
	}
	policy, err := fetchReleasePolicy(ctx, gh, plan.Policy, plan.Commit)
	if err != nil {
		return err
	}
	rel, err := gh.GetReleaseByID(ctx, *f.release)
	if err != nil {
		return err
	}
	if rel.TagName != tag {
		return fmt.Errorf("release %d is for tag %q, not %q", rel.ID, rel.TagName, tag)
	}
	if !rel.Draft {
		return fmt.Errorf("release %d is already published; nothing to stage", rel.ID)
	}
	if _, err := recordFromDraft(rel, tag, plan.Commit, plan, policy, arts, cfg); err != nil {
		return fmt.Errorf("release %d: %w", rel.ID, err)
	}
	if err := verifyTagAt(ctx, gh, tag, plan.Commit); err != nil {
		return err
	}
	// Refuse up front if the draft already contains anything amf does not
	// expect, including an unfinished upload or a publication claim.
	assets, err := gh.ReleaseAssets(ctx, rel.ID)
	if err != nil {
		return err
	}
	expected := map[string]recordArtifact{}
	for _, art := range arts {
		expected[strings.ToLower(art.Name)] = art
	}
	for _, asset := range assets {
		if _, ok := expected[strings.ToLower(asset.Name)]; !ok {
			return fmt.Errorf("release %d has unexpected asset %q; refusing to delete or overwrite it", rel.ID, asset.Name)
		}
	}
	uploaded := 0
	for _, snap := range snapshots {
		assets, err := gh.ReleaseAssets(ctx, rel.ID)
		if err != nil {
			return err
		}
		byName, err := assetMap(assets)
		if err != nil {
			return err
		}
		if _, ok := byName[strings.ToLower(snap.Art.Name)]; ok {
			if err := verifyRemoteArtifact(ctx, gh, rel, snap.Art); err != nil {
				return err
			}
			continue
		}
		in, err := os.Open(snap.File)
		if err != nil {
			return err
		}
		asset, err := gh.UploadReleaseAsset(ctx, rel.ID, snap.Art.Name, in, snap.Art.Size)
		in.Close()
		if err != nil {
			// A lost response or concurrent uploader is safe only when the
			// completed remote bytes already match the recorded plan.
			if verifyErr := verifyRemoteArtifact(ctx, gh, rel, snap.Art); verifyErr == nil {
				continue
			}
			return err
		}
		if asset.Name != "" && asset.Name != snap.Art.Name {
			return fmt.Errorf("GitHub uploaded asset %q for %q", asset.Name, snap.Art.Name)
		}
		if err := verifyRemoteArtifact(ctx, gh, rel, snap.Art); err != nil {
			return err
		}
		uploaded++
	}
	if err := verifyRemoteArtifacts(ctx, gh, rel, arts, nil); err != nil {
		return err
	}
	if uploaded == 0 {
		_, err = fmt.Fprintf(stdout, "Draft release %d already has all %d verified artifacts.\n", rel.ID, len(arts))
		return err
	}
	_, err = fmt.Fprintf(stdout, "Staged %d artifact(s) on draft release %d (%s).\n", len(arts), rel.ID, tag)
	return err
}

// ---------- release publish ----------

func addReleasePublishFlags(fs *flag.FlagSet) releaseStageFlags {
	return addReleaseStageFlags(fs)
}

func cmdReleasePublish(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newCommandFlags("release publish", stderr)
	f := addReleaseStageFlags(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		if err == flag.ErrHelp {
			return nil
		}
		return errUsage
	}
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" || *f.release <= 0 || strings.TrimSpace(*f.plan) == "" || strings.TrimSpace(*f.manifest) == "" {
		fmt.Fprintln(stderr, "usage: amf release publish <tag> --release <id> --plan <file> --manifest <file>")
		return errUsage
	}
	tag := strings.TrimSpace(pos[0])
	if err := validateTagName(tag); err != nil {
		return err
	}
	cfg, err := loadConfig(envPaths(ctx)...)
	if err != nil {
		return err
	}
	gh := newTagReleaseGitHub(cfg)
	if err := checkRepo(ctx, gh, cfg.Repo); err != nil {
		return err
	}
	plan, snapshots, stager, err := loadReleasePlan(*f.plan, *f.manifest)
	if err != nil {
		return err
	}
	defer stager.Close()
	if plan.Tag != tag {
		return fmt.Errorf("release plan is for tag %q, not %q", plan.Tag, tag)
	}
	arts := make([]recordArtifact, 0, len(snapshots))
	for _, snap := range snapshots {
		arts = append(arts, snap.Art)
	}
	policy, err := fetchReleasePolicy(ctx, gh, plan.Policy, plan.Commit)
	if err != nil {
		return err
	}
	rel, err := gh.GetReleaseByID(ctx, *f.release)
	if err != nil {
		return err
	}
	if rel.TagName != tag {
		return fmt.Errorf("release %d is for tag %q, not %q", rel.ID, rel.TagName, tag)
	}
	if _, err := recordFromDraft(rel, tag, plan.Commit, plan, policy, arts, cfg); err != nil {
		return fmt.Errorf("release %d: %w", rel.ID, err)
	}
	if err := verifyTagAt(ctx, gh, tag, plan.Commit); err != nil {
		return err
	}

	claim := publishClaim(recIdentity{repo: cfg.Repo, tag: tag, commit: plan.Commit, arts: arts})
	claimKey := strings.ToLower(publishClaimAsset)

	if !rel.Draft {
		// An identical published retry verifies remote bytes and returns
		// without a write. A claim asset may be present from our publication.
		if err := verifyRemoteArtifacts(ctx, gh, rel, arts, map[string][]byte{claimKey: claim}); err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "Release %d (%s) is already published with all %d verified artifacts.\n", rel.ID, tag, len(arts))
		return err
	}

	// Fail closed on anything unexpected, including our own bookkeeping
	// claim if a prior publication attempt is ambiguous.
	if err := verifyRemoteArtifacts(ctx, gh, rel, arts, nil); err != nil {
		return fmt.Errorf("%w; if an interrupted upload or publish left an unfinished asset or %s, inspect release %d in GitHub and recover it manually; amf never deletes or overwrites it", err, publishClaimAsset, rel.ID)
	}

	checks, err := gh.AllCheckRuns(ctx, plan.Commit)
	if err != nil {
		return err
	}
	statuses, err := gh.AllCommitStatuses(ctx, plan.Commit)
	if err != nil {
		return err
	}
	if err := evaluateReleaseCI(policy, checks, statuses, plan.Commit); err != nil {
		return err
	}

	// Recreate the claim content from the exact record, then create it
	// once. A lost claim response is deliberately ambiguous and fails
	// closed: another cooperating amf may own the same claim.
	if _, err := gh.UploadReleaseAsset(ctx, rel.ID, publishClaimAsset, bytes.NewReader(claim), int64(len(claim))); err != nil {
		return fmt.Errorf("could not create the publication claim on release %d: %w; if %s exists now, inspect the draft and recover it manually; amf never deletes or steals a claim", rel.ID, err, publishClaimAsset)
	}

	// The claim must be the last thing between verification and the PATCH.
	if err := verifyTagAt(ctx, gh, tag, plan.Commit); err != nil {
		return err
	}
	updated, err := gh.UpdateRelease(ctx, rel.ID, map[string]any{"draft": false})
	if err != nil {
		// The PATCH may have committed even though its response was lost.
		if got, readErr := gh.GetReleaseByID(ctx, rel.ID); readErr == nil && !got.Draft {
			if verr := verifyRemoteArtifacts(ctx, gh, got, arts, map[string][]byte{claimKey: claim}); verr == nil {
				_, outErr := fmt.Fprintf(stdout, "Published release %d (%s) at %s.\n", got.ID, tag, plan.Commit)
				return outErr
			}
		}
		return fmt.Errorf("publication outcome for release %d is uncertain: %w; the draft still has the publication claim %s, so amf refuses another PATCH; inspect the release in GitHub and recover it manually", rel.ID, err, publishClaimAsset)
	}
	if updated.ID != rel.ID || updated.Draft {
		return fmt.Errorf("GitHub did not publish release %d as requested", rel.ID)
	}
	if err := verifyTagAt(ctx, gh, tag, plan.Commit); err != nil {
		return err
	}
	if err := verifyRemoteArtifacts(ctx, gh, updated, arts, map[string][]byte{claimKey: claim}); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "Published release %d (%s) at %s.\n", updated.ID, tag, plan.Commit)
	return err
}

type recIdentity struct {
	repo, tag, commit string
	arts              []recordArtifact
}

func publishClaim(id recIdentity) []byte {
	sort.Slice(id.arts, func(i, j int) bool { return id.arts[i].Name < id.arts[j].Name })
	payload := map[string]any{
		"schema": "amf-publish-claim/v1", "repo": id.repo, "tag": id.tag, "commit": id.commit, "artifacts": id.arts,
	}
	b, _ := json.Marshal(payload)
	return b
}

// ---------- exact-target CI ----------

type checkRunDetail struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	HeadSHA    string    `json:"head_sha"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	StartedAt  time.Time `json:"started_at"`
	App        struct {
		ID int64 `json:"id"`
	} `json:"app"`
}

type commitStatusDetail struct {
	ID        int64     `json:"id"`
	Context   string    `json:"context"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	Creator   struct {
		ID int64 `json:"id"`
	} `json:"creator"`
}

// AllCheckRuns follows every page of check runs, including older attempts.
func (g *GitHub) AllCheckRuns(ctx context.Context, sha string) ([]checkRunDetail, error) {
	endpoint := g.repoPath("/commits/%s/check-runs", url.PathEscape(sha))
	path := endpoint + "?per_page=100&filter=all"
	seen := map[string]bool{path: true}
	var runs []checkRunDetail
	for path != "" {
		var page struct {
			CheckRuns []checkRunDetail `json:"check_runs"`
		}
		header, err := g.send(ctx, "GET", path, nil, &page)
		if err != nil {
			return nil, err
		}
		runs = append(runs, page.CheckRuns...)
		if path, err = g.nextPageScoped(header, endpoint, seen); err != nil {
			return nil, err
		}
	}
	return runs, nil
}

// AllCommitStatuses follows every page of commit statuses. The combined
// status endpoint is checked separately so a response for another SHA can't
// be mistaken for this target's statuses.
func (g *GitHub) AllCommitStatuses(ctx context.Context, sha string) ([]commitStatusDetail, error) {
	var combined struct {
		SHA string `json:"sha"`
	}
	if err := g.do(ctx, "GET", g.repoPath("/commits/%s/status", url.PathEscape(sha)), nil, &combined); err != nil {
		return nil, err
	}
	if combined.SHA != "" && combined.SHA != sha {
		return nil, fmt.Errorf("GitHub returned statuses for commit %q, not %q", combined.SHA, sha)
	}
	endpoint := g.repoPath("/commits/%s/statuses", url.PathEscape(sha))
	path := endpoint + "?per_page=100"
	seen := map[string]bool{path: true}
	var statuses []commitStatusDetail
	for path != "" {
		var page []commitStatusDetail
		header, err := g.send(ctx, "GET", path, nil, &page)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, page...)
		if path, err = g.nextPageScoped(header, endpoint, seen); err != nil {
			return nil, err
		}
	}
	return statuses, nil
}

func latestCheckByName(runs []checkRunDetail) map[string]checkRunDetail {
	out := map[string]checkRunDetail{}
	for _, run := range runs {
		key := strings.ToLower(run.Name)
		if prev, ok := out[key]; !ok || run.ID > prev.ID {
			out[key] = run
		}
	}
	return out
}

func latestStatusByContext(statuses []commitStatusDetail) map[string]commitStatusDetail {
	out := map[string]commitStatusDetail{}
	for _, s := range statuses {
		key := strings.ToLower(s.Context)
		if prev, ok := out[key]; !ok || s.ID > prev.ID {
			out[key] = s
		}
	}
	return out
}

func evaluateReleaseCI(policy releasePolicy, runs []checkRunDetail, statuses []commitStatusDetail, sha string) error {
	checks := latestCheckByName(runs)
	contexts := latestStatusByContext(statuses)
	var problems []string
	for _, want := range policy.RequiredChecks {
		run, ok := checks[strings.ToLower(want.Name)]
		if !ok {
			problems = append(problems, fmt.Sprintf("check %q is missing", want.Name))
			continue
		}
		if run.HeadSHA != sha {
			problems = append(problems, fmt.Sprintf("check %q is for commit %s, not %s", want.Name, run.HeadSHA, sha))
			continue
		}
		if run.App.ID != want.AppID {
			problems = append(problems, fmt.Sprintf("check %q is from app %d, not required app %d", want.Name, run.App.ID, want.AppID))
			continue
		}
		if run.Status != "completed" {
			problems = append(problems, fmt.Sprintf("check %q is %s", want.Name, run.Status))
			continue
		}
		if run.Conclusion != "success" {
			problems = append(problems, fmt.Sprintf("check %q concluded %q, not success", want.Name, run.Conclusion))
		}
	}
	for _, want := range policy.RequiredStatuses {
		status, ok := contexts[strings.ToLower(want.Context)]
		if !ok {
			problems = append(problems, fmt.Sprintf("status %q is missing", want.Context))
			continue
		}
		if status.Creator.ID != want.CreatorID {
			problems = append(problems, fmt.Sprintf("status %q is from creator %d, not required creator %d", want.Context, status.Creator.ID, want.CreatorID))
			continue
		}
		if status.State != "success" {
			problems = append(problems, fmt.Sprintf("status %q is %q, not success", want.Context, status.State))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("required CI for commit %s is not satisfied: %s", sha, strings.Join(problems, "; "))
	}
	return nil
}
