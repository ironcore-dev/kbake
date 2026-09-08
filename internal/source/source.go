// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package source fetches the kernel source tree described by a Kernelfile's
// `from: scratch` address.
//
// The address is a single schemaless git string, go-get-style:
//
//	git.kernel.org/pub/scm/linux/kernel/git/stable/linux@v7.1
//
// (host / path fragments / repo @ ref). Only git is supported. Fetching uses
// a go-style module cache: the materialized tree at a given version lives at
//
//	<cacheDir>/<host>/<path>/<repo>@<version>/
//
// with NO .git directory — a plain snapshot of the repository at that
// revision, copied verbatim into a per-arch working tree to build. <version>
// is the tag name when the ref names a tag, otherwise a go pseudo-version
// (v0.0.0-yyyymmddhhmmss-abcdef123456, or the based form when a semver tag is
// reachable). A bare object store under <cacheDir>/cache/vcs/ holds the git
// objects so subsequent fetches pull only the delta.
package source

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ironcore-dev/kbake/internal/xbufio"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/ironcore-dev/kbake/internal/progress"
)

// Source mirrors kernelfile.Source (re-declared here so this package has no
// dependency on the kernelfile package): the schemaless git address.
type Source struct {
	Address string
}

// Address is a parsed schemaless git address. The host and path fragments are
// preserved verbatim so the cache layout mirrors the go module cache:
//
//	<cacheDir>/<Host>/<Path>/<Repo>@<version>
//
// Path is the repository's directory prefix between the host and the repo; it
// is "" when the repo sits directly under the host (e.g. "example.com/linux"
// -> Host=example.com, Path="", Repo=linux).
type Address struct {
	Host string
	Path string // directory prefix between host and repo; "" if none
	Repo string // the repository name (last path segment)
	Ref  string // the requested ref: a tag, branch, or commit SHA
}

// ParseAddress splits a schemaless address into its parts. The address must
// be of the form <host>[/<path>]/<repo>@<ref> with no scheme.
func ParseAddress(addr string) (Address, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return Address{}, errors.New("source: empty address")
	}
	if strings.Contains(addr, "://") {
		return Address{}, fmt.Errorf("source: address %q must be schemaless (drop the https:// prefix)", addr)
	}
	at := strings.LastIndex(addr, "@")
	if at < 0 {
		return Address{}, fmt.Errorf("source: address %q must include @<ref>", addr)
	}
	repoPath := addr[:at]
	ref := addr[at+1:]
	if ref == "" {
		return Address{}, fmt.Errorf("source: address %q has an empty ref after @", addr)
	}
	if repoPath == "" {
		return Address{}, fmt.Errorf("source: address %q has an empty repository path", addr)
	}
	host, rest, ok := strings.Cut(repoPath, "/")
	if !ok {
		return Address{}, fmt.Errorf("source: address %q is missing a path/repo after the host", addr)
	}
	if rest == "" {
		return Address{}, fmt.Errorf("source: address %q is missing a repository", addr)
	}
	var pathStr, repo string
	if last := strings.LastIndex(rest, "/"); last < 0 {
		repo = rest
		pathStr = ""
	} else {
		repo = rest[last+1:]
		pathStr = rest[:last]
	}
	if host == "" || repo == "" {
		return Address{}, fmt.Errorf("source: address %q has an empty host or repo", addr)
	}
	return Address{Host: host, Path: pathStr, Repo: repo, Ref: ref}, nil
}

// CloneURL returns the https:// URL git is invoked with. go-get resolves a
// schemaless import path over https; kbake does the same.
func (a Address) CloneURL() string {
	return "https://" + a.Host + "/" + a.pathRepo()
}

// modulePath returns the "<host>/<path>/<repo>" part (no version) — the
// leading directories of the cache layout, mirroring a go module path.
func (a Address) modulePath() string {
	return a.Host + "/" + a.pathRepo()
}

func (a Address) pathRepo() string {
	if a.Path == "" {
		return a.Repo
	}
	return a.Path + "/" + a.Repo
}

// Result describes a fetched source tree.
type Result struct {
	// Dir is the materialized cache directory containing the kernel tree at
	// the resolved version (no .git). The build copies this verbatim.
	Dir string
	// Ref is the original schemaless address.
	Ref string
	// CloneURL is the https:// URL git was invoked with.
	CloneURL string
	// Commit is the resolved commit SHA.
	Commit string
	// Version is the resolved cache version: the tag name when the ref names a
	// tag, otherwise a go-style pseudo-version.
	Version string
	// SourceDateEpoch is the commit's unix timestamp (UTC), for SOURCE_DATE_EPOCH.
	SourceDateEpoch string
}

// Fetch obtains the source tree for the git address in s and returns the
// materialized cache directory holding the tree at the resolved version (no
// .git). cacheDir is the go-style module cache root; the extracted tree is
// stored at <cacheDir>/<host>/<path>/<repo>@<version> and a bare object store
// (for incremental fetches + ref resolution) at
// <cacheDir>/cache/vcs/<host>/<path>/<repo>.git. If the version's directory
// already exists it is reused and no git operation is performed.
func Fetch(ctx context.Context, ref string, cacheDir string) (*Result, error) {
	if err := assertTool("git"); err != nil {
		return nil, err
	}
	addr, err := ParseAddress(ref)
	if err != nil {
		return nil, err
	}
	if cacheDir == "" {
		return nil, errors.New("source: no cache directory configured (set --cache-dir)")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	return fetchGit(ref, addr, cacheDir, progress.ReporterFromContext(ctx))
}

// fetchGit resolves addr into a materialized tree cache directory.
func fetchGit(ref string, addr Address, cacheDir string, rep progress.Reporter) (*Result, error) {
	bareRepo := filepath.Join(cacheDir, "cache", "vcs", addr.modulePath()) + ".git"
	if err := os.MkdirAll(filepath.Dir(bareRepo), 0o755); err != nil {
		return nil, err
	}

	// 1. Ensure the bare object store exists (instant; never a full clone).
	rep.Detail("ensuring cache repo")
	if _, err := os.Stat(bareRepo); os.IsNotExist(err) {
		if err := runQuiet("git", "init", "--bare", bareRepo); err != nil {
			return nil, fmt.Errorf("git init (cache): %w", err)
		}
	} else if err != nil {
		return nil, fmt.Errorf("stat cache repo: %w", err)
	}
	cloneURL := addr.CloneURL()
	if err := ensureOrigin(bareRepo, cloneURL); err != nil {
		return nil, err
	}

	// 2. Warm fast path: if a previous run resolved this ref and the tree cache
	//    for that version still exists, reuse it verbatim — no fetch, no network.
	//    A branch ref therefore pins to the version first resolved (delete the
	//    cache to re-fetch the moved tip); this is deliberate, so a repeat build
	//    is reproducible and git-free ("just copy this exact directory"). The
	//    commit's epoch is recomputed from the bare repo's objects (local).
	if e, ok := lookupRefMap(refMapPath(bareRepo), addr.Ref); ok {
		treeDir := filepath.Join(cacheDir, addr.modulePath()) + "@" + e.version
		if _, err := os.Stat(treeDir); err == nil {
			rep.Detail(fmt.Sprintf("using cached %s@%s", addr.modulePath(), e.version))
			epoch, _ := output("git", "-C", bareRepo, "show", "-s", "--format=%ct", e.commit)
			return &Result{
				Dir:             treeDir,
				Ref:             ref,
				CloneURL:        cloneURL,
				Commit:          e.commit,
				Version:         e.version,
				SourceDateEpoch: strings.TrimSpace(epoch),
			}, nil
		}
	}

	// 3. Fetch the ref. A shallow fetch (--depth 1) is enough to resolve the
	//    tip commit and, for a tag, to use the tag name as the version — keeping
	//    the common (tag) build fast. A fetch failure (e.g. offline) is
	//    non-fatal: we fall back to whatever objects the cache already holds.
	rep.Detail(fmt.Sprintf("fetching %s from remote", addr.Ref))
	// Run the fetch through git-noise filters so the reporter's tail stays
	// readable (the reporter itself no longer filters — that's a git-specific
	// concern, handled here at the source). Closing the filters flushes any
	// buffered partial line; a fetch failure (e.g. offline) is non-fatal: we
	// fall back to whatever objects the cache already holds.
	stdout := filterGitNoiseWriter(rep)
	stderr := filterGitNoiseWriter(rep)
	if err := runTo(stdout, stderr, "git", "-C", bareRepo, "fetch", "--depth", "1", "--progress", "origin", addr.Ref); err != nil {
		_, _ = fmt.Fprintf(rep, "kbake: warning: cache fetch failed (continuing with cached repo): %v\n", err)
	}
	_ = stdout.Close()
	_ = stderr.Close()

	// 4. Resolve the ref to a commit using local objects (FETCH_HEAD from the
	//    fetch above, or a cached remote-tracking ref). If this fails (e.g.
	//    offline and the ref isn't cached) fall back to a previously persisted
	//    ref→version mapping so a cached tree can still be reused offline.
	commit, err := resolveRef(bareRepo, addr.Ref)
	version, offline := "", false
	if err != nil {
		if e, ok := lookupRefMap(refMapPath(bareRepo), addr.Ref); ok {
			commit, version, offline = e.commit, e.version, true
		} else {
			return nil, err
		}
	} else {
		// 5. Determine the cache version (tag name or go-style pseudo-version),
		//    using `git ls-remote` (ref advertisement only, no object download)
		//    rather than local refs — a bare repo's fetch of a tag only writes
		//    FETCH_HEAD, not refs/tags/<tag>, so local refs can't tell tags from
		//    branches.
		version, err = resolveVersion(bareRepo, addr, commit)
		if err != nil {
			return nil, err
		}
	}

	// 6. Materialize (or reuse) the tree cache at <cacheDir>/<modulePath>@<version>.
	//    Materialization is atomic (temp sibling + rename), so a treeDir only
	//    ever exists complete — mere existence proves completeness, which the
	//    warm path above relies on.
	treeDir := filepath.Join(cacheDir, addr.modulePath()) + "@" + version
	if _, err := os.Stat(treeDir); err == nil {
		rep.Detail(fmt.Sprintf("using cached %s@%s", addr.modulePath(), version))
	} else if offline {
		// Offline but the tree for the persisted version is gone — can't rebuild
		// it without the network. Surface a clear error rather than hanging.
		return nil, fmt.Errorf("ref %q: cached tree for %s missing and fetch failed (retry online)", addr.Ref, version)
	} else {
		rep.Detail(fmt.Sprintf("checking out %s@%s", addr.modulePath(), version))
		if err := materializeAtomic(bareRepo, commit, treeDir); err != nil {
			return nil, err
		}
	}

	// 7. Persist the ref→version map only now, once the tree is known to be
	//    complete (a refmap pointing at a half-materialized tree would poison
	//    the warm path). A write error is non-fatal: the mapping only speeds up
	//    later offline resolution; the tree cache remains reusable without it.
	if !offline {
		_ = saveRefMap(refMapPath(bareRepo), addr.Ref, version, commit)
	}

	// 8. Epoch from the commit (committer date); reads only the commit object,
	//    so it works on a shallow repo.
	epoch, _ := output("git", "-C", bareRepo, "show", "-s", "--format=%ct", commit)

	return &Result{
		Dir:             treeDir,
		Ref:             ref,
		CloneURL:        cloneURL,
		Commit:          commit,
		Version:         version,
		SourceDateEpoch: strings.TrimSpace(epoch),
	}, nil
}

// ensureOrigin makes sure the bare repo has an "origin" remote pointing at url
// (added on first use, updated if the address changed).
func ensureOrigin(bareRepo, url string) error {
	if runQuiet("git", "-C", bareRepo, "config", "remote.origin.url") != nil {
		return runQuiet("git", "-C", bareRepo, "remote", "add", "origin", url)
	}
	return runQuiet("git", "-C", bareRepo, "remote", "set-url", "origin", url)
}

// resolveRef resolves ref to a full commit SHA using only objects in bareRepo.
// ref may be a tag, a branch, HEAD, or a (complete) commit SHA — all handled
// by trying the canonical ref locations in turn and peeling with ^{commit}.
func resolveRef(bareRepo, ref string) (string, error) {
	for _, c := range []string{"refs/tags/" + ref, "refs/remotes/origin/" + ref, ref, "FETCH_HEAD"} {
		out, err := output("git", "-C", bareRepo, "rev-parse", "--verify", "--quiet", c+"^{commit}")
		if err == nil {
			if sha := strings.TrimSpace(out); sha != "" {
				return sha, nil
			}
		}
	}
	return "", fmt.Errorf("ref %q not in cache and fetch failed; retry online to populate the cache", ref)
}

// resolveVersion determines the cache version for addr.Ref/commit: the tag
// name if the remote advertises ref as a tag, otherwise a go-style
// pseudo-version. Tag-vs-branch is decided via `git ls-remote` (ref
// advertisement only, no object download) because a bare repo's local refs
// are unreliable for this: `git fetch --depth 1 origin <tag>` writes only
// FETCH_HEAD, not refs/tags/<tag>.
func resolveVersion(bareRepo string, addr Address, commit string) (string, error) { //nolint:unparam
	kind, _ := remoteRefKind(bareRepo, addr.Ref)
	if kind == refTag {
		return addr.Ref, nil
	}
	// Branch or SHA → pseudo-version. The base is the highest semver tag the
	// remote advertises (ls-remote --tags, no download). We do NOT verify the
	// tag is an ancestor of commit: a --depth 1 shallow repo can't walk
	// ancestry, so `git tag --merged` (go's normal source) is unusable here.
	// For monotonically-versioned repos like the kernel the highest advertised
	// tag is the nearest ancestor in practice. If ls-remote fails (offline) the
	// base is empty → the v0.0.0-… form, still a valid, stable pseudo-version.
	older, major := selectHighestSemver(remoteSemverTags(bareRepo))
	return pseudoVersion(bareRepo, commit, older, major), nil
}

// refKind classifies a ref as the remote advertises it.
const (
	refTag    = "tag"
	refBranch = "branch"
	refOther  = "other" // a raw commit SHA, or not advertised
)

// remoteRefKind runs `git ls-remote origin <ref>` and classifies the ref. It
// returns refOther (with a nil error) when the ref isn't advertised (a SHA or
// a non-existent ref) or when ls-remote itself fails (offline) — the fetch
// step is the source of truth for whether the ref is actually reachable, so a
// classification miss here is never fatal.
func remoteRefKind(bareRepo, ref string) (string, error) {
	out, err := output("git", "-C", bareRepo, "ls-remote", "origin", ref)
	if err != nil {
		return refOther, nil
	}
	for line := range strings.SplitSeq(out, "\n") {
		// "<sha>\trefs/tags/v1.2.3" or "...\trefs/tags/v1.2.3^{}" (annotated)
		// or "<sha>\trefs/heads/main".
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		switch refPath := strings.TrimSpace(parts[1]); {
		case strings.HasPrefix(refPath, "refs/tags/"):
			return refTag, nil
		case strings.HasPrefix(refPath, "refs/heads/"):
			return refBranch, nil
		}
	}
	return refOther, nil
}

// remoteSemverTags lists every tag the remote advertises (`git ls-remote
// --tags origin`) and returns the tag names (semver-valid or not; the caller
// filters). ls-remote is pure ref advertisement — no objects are downloaded —
// so this works where `git tag --merged` cannot (a shallow repo can't walk
// ancestry).
func remoteSemverTags(bareRepo string) []string {
	out, err := output("git", "-C", bareRepo, "ls-remote", "--tags", "origin")
	if err != nil {
		return nil
	}
	var tags []string
	for line := range strings.SplitSeq(out, "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		refPath := strings.TrimSpace(parts[1])
		if !strings.HasPrefix(refPath, "refs/tags/") {
			continue
		}
		name := strings.TrimPrefix(refPath, "refs/tags/")
		name = strings.TrimSuffix(name, "^{}") // annotated tag's peeled form
		if name != "" {
			tags = append(tags, name)
		}
	}
	return tags
}

// pseudoVersion builds a go-style pseudo-version for commit using
// module.PseudoVersion. older/major is the chosen base tag ("" for the
// v0.0.0-… form). The timestamp is the commit's committer date (reads only
// the commit object, so it works on a shallow repo); the revision is go's
// 12-byte commit prefix.
func pseudoVersion(bareRepo, commit, older, major string) string {
	epochStr, _ := output("git", "-C", bareRepo, "show", "-s", "--format=%ct", commit)
	var t time.Time
	if sec, err := strconv.ParseInt(strings.TrimSpace(epochStr), 10, 64); err == nil {
		t = time.Unix(sec, 0).UTC()
	}
	rev := commit
	if len(rev) > 12 {
		rev = rev[:12]
	}
	return module.PseudoVersion(major, older, t, rev)
}

// selectHighestSemver picks the highest semver-valid tag from tags and returns
// it plus its major ("vN"); both are "" if none is semver-valid. Pure (no git)
// so it can be unit tested directly. (go's semver is lenient: "v7.1"/"v7" are
// valid, so kernel tags qualify.)
func selectHighestSemver(tags []string) (older, major string) {
	for _, tag := range tags {
		if tag == "" || !semver.IsValid(tag) {
			continue
		}
		if older == "" || semver.Compare(tag, older) > 0 {
			older = tag
		}
	}
	if older != "" {
		major = semver.Major(older)
	}
	return older, major
}

// refMapPath is the sibling file of the bare repo that persists ref→version
// mappings so an offline build can reuse a previously resolved cache entry.
func refMapPath(bareRepo string) string { return bareRepo + ".kbakerefs" }

type refEntry struct{ ref, version, commit string }

// lookupRefMap returns the persisted mapping for ref, if any.
func lookupRefMap(path, ref string) (refEntry, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return refEntry{}, false
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) == 3 && f[0] == ref {
			return refEntry{ref: f[0], version: f[1], commit: f[2]}, true
		}
	}
	return refEntry{}, false
}

// saveRefMap records (or updates) the mapping for ref and rewrites the file so
// stale entries for other refs are retained but a ref keeps only its latest.
func saveRefMap(path, ref, version, commit string) error {
	existing, _ := os.ReadFile(path)
	var lines []string
	saw := false
	for line := range strings.SplitSeq(string(existing), "\n") {
		if line == "" {
			continue
		}
		if f := strings.SplitN(line, "\t", 3); len(f) == 3 && f[0] == ref {
			lines = append(lines, ref+"\t"+version+"\t"+commit)
			saw = true
			continue
		}
		lines = append(lines, line)
	}
	if !saw {
		lines = append(lines, ref+"\t"+version+"\t"+commit)
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// checkoutTree materialises the working tree at commit (borrowing bareRepo's
// object store via alternates, so no objects are copied) into treeDir, then
// removes the .git directory so treeDir is a plain snapshot — like an
// extracted go module. The build copies this verbatim per arch.
func checkoutTree(bareRepo, commit, treeDir string) error {
	if err := os.MkdirAll(treeDir, 0o755); err != nil {
		return err
	}
	if err := runQuiet("git", "init", treeDir); err != nil {
		return fmt.Errorf("git init: %w", err)
	}
	absBare, _ := filepath.Abs(bareRepo)
	altDir := filepath.Join(treeDir, ".git", "objects", "info")
	if err := os.MkdirAll(altDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(altDir, "alternates"), []byte(filepath.Join(absBare, "objects")+"\n"), 0o644); err != nil {
		return err
	}
	if err := runQuiet("git", "-C", treeDir, "checkout", commit); err != nil {
		return fmt.Errorf("git checkout %s: %w", commit, err)
	}
	_ = os.RemoveAll(filepath.Join(treeDir, ".git"))
	return nil
}

// materializeAtomic materialises the tree at commit into treeDir atomically:
// the checkout runs in a unique temp sibling and is renamed onto treeDir only
// on success (same filesystem, so the rename is atomic). treeDir therefore
// never exists in a partially materialized state — its mere existence proves
// completeness to every cache reader, the invariant the warm path and the
// per-version reuse check rely on. Leftover ".kbake-checkout-*" dirs from a
// killed process are never reused and are safe to remove by hand.
func materializeAtomic(bareRepo, commit, treeDir string) error {
	if err := os.MkdirAll(filepath.Dir(treeDir), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(treeDir), ".kbake-checkout-*")
	if err != nil {
		return fmt.Errorf("creating temp checkout dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }() // no-op after a successful rename

	if err := checkoutTree(bareRepo, commit, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, treeDir); err != nil {
		return fmt.Errorf("moving checkout into cache: %w", err)
	}
	return nil
}

// --- helpers ---

func assertTool(name string) error {
	_, err := exec.LookPath(name)
	if err != nil {
		return fmt.Errorf("required tool %q not found in PATH", name)
	}
	return nil
}

// runTo runs name with args, writing stdout and stderr to w (which may be nil,
// in which case output is discarded).
func runTo(stdout, stderr io.Writer, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func isGitNoise(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	if !strings.HasPrefix(s, "remote: ") {
		return false
	}
	for _, p := range []string{"Enumerating objects", "Counting objects", "Compressing objects"} {
		if strings.HasPrefix(s, "remote: "+p) {
			return true
		}
	}
	return false
}

func filterGitNoiseWriter(w io.Writer) io.WriteCloser {
	sw := xbufio.NewScanWriter(w)
	sw.Split(func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		advance, token, err = bufio.ScanLines(data, atEOF)
		if err != nil {
			return advance, token, err
		}

		if isGitNoise(string(token)) {
			// Filtered lines are dropped entirely, terminator included.
			return advance, nil, nil
		}
		if token != nil {
			// ScanLines strips the line terminator, and ScanWriter forwards
			// tokens verbatim — re-add the terminator so downstream line-oriented
			// consumers (the build log file, the TTY tail's own line scanner)
			// see real lines again. append reuses ScanLines' already-consumed
			// delimiter area of data, which nothing reads past this point
			// (ScanWriter advances by `advance`).
			token = append(token, '\n')
		}
		return advance, token, nil
	})
	return sw
}

// runQuiet runs a command, discarding its stdout/stderr on success so the
// terminal stays clean — e.g. hiding git's detached-HEAD advice ('You are in
// "detached HEAD" state...') during a materialisation checkout, which
// --quiet alone does not suppress (it's an advice.* message, not regular
// output). On failure the captured stderr is folded into the returned error so
// git's diagnostic is still surfaced and logged.
func runQuiet(name string, args ...string) error { //nolint:unparam
	var stderr strings.Builder
	cmd := exec.Command(name, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func output(name string, args ...string) (string, error) { //nolint:unparam
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}
