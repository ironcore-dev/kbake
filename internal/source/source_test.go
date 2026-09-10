// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package source

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/mod/module"
)

func TestParseAddress(t *testing.T) {
	cases := []struct {
		in      string
		want    Address
		wantErr string
	}{
		{
			in: "git.kernel.org/pub/scm/linux/kernel/git/stable/linux@v7.1",
			want: Address{
				Host: "git.kernel.org",
				Path: "pub/scm/linux/kernel/git/stable",
				Repo: "linux",
				Ref:  "v7.1",
			},
		},
		{
			in: "github.com/torvalds/linux@v6.10",
			want: Address{
				Host: "github.com",
				Path: "torvalds",
				Repo: "linux",
				Ref:  "v6.10",
			},
		},
		{
			// repo directly under the host: no path fragments.
			in: "example.com/linux@v1.0.0",
			want: Address{
				Host: "example.com",
				Path: "",
				Repo: "linux",
				Ref:  "v1.0.0",
			},
		},
		{
			// a commit SHA ref.
			in: "example.com/linux@0123456789abcdef0123456789abcdef01234567",
			want: Address{
				Host: "example.com",
				Repo: "linux",
				Ref:  "0123456789abcdef0123456789abcdef01234567",
			},
		},
		{in: "", wantErr: "empty address"},
		{in: "https://git.kernel.org/linux@v7.1", wantErr: "schemaless"},
		{in: "git.kernel.org/linux", wantErr: "@<ref>"},
		{in: "git.kernel.org/linux@", wantErr: "empty ref"},
		{in: "git.kernel.org/@v1", wantErr: "repository"}, // host/ with nothing after
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseAddress(c.in)
			if c.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (%+v)", c.wantErr, got)
				}
				if !strings.Contains(err.Error(), c.wantErr) {
					t.Errorf("error = %q, want substring %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestAddressCloneURLAndModulePath(t *testing.T) {
	a := Address{Host: "git.kernel.org", Path: "pub/scm/linux/kernel/git/stable", Repo: "linux", Ref: "v7.1"}
	if got, want := a.CloneURL(), "https://git.kernel.org/pub/scm/linux/kernel/git/stable/linux"; got != want {
		t.Errorf("CloneURL = %q, want %q", got, want)
	}
	if got, want := a.modulePath(), "git.kernel.org/pub/scm/linux/kernel/git/stable/linux"; got != want {
		t.Errorf("modulePath = %q, want %q", got, want)
	}

	b := Address{Host: "example.com", Repo: "linux", Ref: "v1.0.0"} // no path
	if got, want := b.CloneURL(), "https://example.com/linux"; got != want {
		t.Errorf("CloneURL (no path) = %q, want %q", got, want)
	}
	if got, want := b.modulePath(), "example.com/linux"; got != want {
		t.Errorf("modulePath (no path) = %q, want %q", got, want)
	}
}

func TestSelectHighestSemver(t *testing.T) {
	cases := []struct {
		name      string
		tags      []string
		wantOlder string
		wantMajor string
	}{
		{
			name:      "picks highest release (semver not lexical order)",
			tags:      []string{"v1.1.0", "v1.2.0", "v1.10.0", "v1.2.1"},
			wantOlder: "v1.10.0", // 1.10.0 > 1.2.1
			wantMajor: "v1",
		},
		{
			name:      "prerelease sorts before release",
			tags:      []string{"v1.2.0", "v1.2.0-rc1"},
			wantOlder: "v1.2.0",
			wantMajor: "v1",
		},
		{
			// go's semver is lenient: "v7.1" / "v7" are valid, so they win over
			// v1.2.3 (7 > 1). This mirrors how the kernel's v7.x tags behave.
			name:      "lenient semver (v7.1 is valid)",
			tags:      []string{"v7.1", "v7", "v1.2.3"},
			wantOlder: "v7.1",
			wantMajor: "v7",
		},
		{
			// genuinely non-semver tags are ignored.
			name:      "non-semver tags ignored",
			tags:      []string{"master", "1.0", "v1.2.3.4", "release-1", "v1.2.3"},
			wantOlder: "v1.2.3",
			wantMajor: "v1",
		},
		{
			name:      "no semver tags",
			tags:      []string{"master", "1.0", "v1.2.3.4", "release-1"},
			wantOlder: "",
			wantMajor: "",
		},
		{
			name:      "empty",
			tags:      nil,
			wantOlder: "",
			wantMajor: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			older, major := selectHighestSemver(c.tags)
			if older != c.wantOlder {
				t.Errorf("older = %q, want %q", older, c.wantOlder)
			}
			if major != c.wantMajor {
				t.Errorf("major = %q, want %q", major, c.wantMajor)
			}
		})
	}
}

// hasGit skips a test when git is unavailable.
func hasGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}
}

// makeRepo creates a local git repo (non-bare) with two commits: the first is
// tagged firstTag (unless ""), the tip (second commit) is tagged with tipTags.
// Returns the repo dir, the first commit SHA, and the tip commit SHA.
func makeRepo(t *testing.T, firstTag string, tipTags ...string) (dir, first, tip string) {
	t.Helper()
	dir = t.TempDir()
	git := func(args ...string) {
		t.Helper()
		out, err := exec.Command("git", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	rev := func(spec string) string {
		t.Helper()
		out, err := exec.Command("git", "-C", dir, "rev-parse", spec).Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	git("-c", "init.defaultBranch=main", "init", dir)
	git("-C", dir, "config", "user.email", "kbake@example.com")
	git("-C", dir, "config", "user.name", "kbake")
	write(t, filepath.Join(dir, "a.txt"), "a\n")
	git("-C", dir, "add", "a.txt")
	git("-C", dir, "commit", "-m", "first")
	if firstTag != "" {
		git("-C", dir, "tag", firstTag)
	}
	first = rev("HEAD")
	write(t, filepath.Join(dir, "b.txt"), "b\n")
	git("-C", dir, "add", "b.txt")
	git("-C", dir, "commit", "-m", "second")
	for _, tg := range tipTags {
		git("-C", dir, "tag", tg)
	}
	tip = rev("HEAD")
	return dir, first, tip
}

// makeBare clones src into a bare repo, as fetchGit's cache object store would
// hold it, so the git-backed helpers operate on a bare repo (matching reality).
func makeBare(t *testing.T, src string) string {
	t.Helper()
	bare := t.TempDir()
	out, err := exec.Command("git", "clone", "--bare", src, bare).CombinedOutput()
	if err != nil {
		t.Fatalf("git clone --bare: %v\n%s", err, out)
	}
	return bare
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestGitResolveRefAndIsTag exercises resolveRef and isTag against a real
// bare repo: a branch, a tag, and a SHA all resolve, and tags are recognised
// as tags (a branch/SHA is not).
func TestGitResolveRef(t *testing.T) {
	hasGit(t)
	t.Parallel()
	// first commit tagged v1.2.3; tip tagged v9.9.9.
	src, first, tip := makeRepo(t, "v1.2.3", "v9.9.9")
	bare := makeBare(t, src)

	cases := []struct{ ref, want string }{
		{"main", tip},     // branch -> tip
		{"v9.9.9", tip},   // tag on tip -> tip
		{tip, tip},        // SHA -> itself
		{"v1.2.3", first}, // tag on first commit -> first commit
	}
	for _, c := range cases {
		got, err := resolveRef(bare, c.ref)
		if err != nil {
			t.Errorf("resolveRef(%q): %v", c.ref, err)
			continue
		}
		if got != c.want {
			t.Errorf("resolveRef(%q) = %q, want %q", c.ref, got, c.want)
		}
	}
}

// TestRemoteRefKind verifies tag/branch/other classification via ls-remote
// (the mechanism that replaced the broken local-ref isTag check: fetching a
// tag into a bare repo only writes FETCH_HEAD, not refs/tags/<tag>).
func TestRemoteRefKind(t *testing.T) {
	hasGit(t)
	t.Parallel()
	src, _, _ := makeRepo(t, "v1.2.3", "v9.9.9")
	bare := makeBare(t, src)

	cases := []struct {
		ref  string
		want string
	}{
		{"v1.2.3", refTag},
		{"v9.9.9", refTag},
		{"main", refBranch},
		{"nonexistent", refOther},
	}
	for _, c := range cases {
		got, _ := remoteRefKind(bare, c.ref)
		if got != c.want {
			t.Errorf("remoteRefKind(%q) = %q, want %q", c.ref, got, c.want)
		}
	}
}

// TestRemoteSemverTags verifies the ls-remote --tags extraction (incl. the
// ^{} annotated-tag peeled form).
func TestRemoteSemverTags(t *testing.T) {
	hasGit(t)
	t.Parallel()
	src, _, _ := makeRepo(t, "v1.2.3", "v9.9.9", "release-1")
	bare := makeBare(t, src)

	tags := remoteSemverTags(bare)
	// v1.2.3 and v9.9.9 are semver; release-1 is not (selectHighestSemver
	// filters it, but remoteSemverTags returns all names).
	got := map[string]bool{}
	for _, tg := range tags {
		got[tg] = true
	}
	for _, w := range []string{"v1.2.3", "v9.9.9", "release-1"} {
		if !got[w] {
			t.Errorf("remoteSemverTags missing %q: got %v", w, tags)
		}
	}
	older, major := selectHighestSemver(tags)
	if older != "v9.9.9" || major != "v9" {
		t.Errorf("selectHighestSemver = (%q, %q), want (v9.9.9, v9)", older, major)
	}
}

// TestResolveVersionTag verifies a tag ref resolves to the tag name as the
// version (no pseudo-version).
func TestResolveVersionTag(t *testing.T) {
	hasGit(t)
	t.Parallel()
	src, _, tip := makeRepo(t, "v1.2.3", "v9.9.9")
	bare := makeBare(t, src)
	addr := Address{Host: "example.com", Repo: "linux", Ref: "v9.9.9"}
	v, err := resolveVersion(bare, addr, tip)
	if err != nil {
		t.Fatal(err)
	}
	if v != "v9.9.9" {
		t.Errorf("resolveVersion(tag v9.9.9) = %q, want v9.9.9", v)
	}
}

// TestResolveVersionBranch verifies the go-style pseudo-version for a branch
// ref whose highest advertised semver tag is v1.2.3: base increments to v1.2.4
// with the -0 prerelease separator (go form 2), timestamp = commit UTC time,
// revision = 12-byte commit prefix.
func TestResolveVersionBranch(t *testing.T) {
	hasGit(t)
	t.Parallel()
	src, _, tip := makeRepo(t, "v1.2.3") // tip untagged; branch main -> tip
	bare := makeBare(t, src)
	addr := Address{Host: "example.com", Repo: "linux", Ref: "main"}
	ver, err := resolveVersion(bare, addr, tip)
	if err != nil {
		t.Fatal(err)
	}
	wantRev := tip
	if len(wantRev) > 12 {
		wantRev = wantRev[:12]
	}
	commitTime, _ := exec.Command("git", "-C", src, "show", "-s", "--format=%ct", tip).Output()
	sec, _ := parseUnix(string(commitTime))
	want := module.PseudoVersion("v1", "v1.2.3", time.Unix(sec, 0).UTC(), wantRev)
	if ver != want {
		t.Errorf("resolveVersion(main) = %q, want %q", ver, want)
	}
	if !strings.HasPrefix(ver, "v1.2.4-0.") {
		t.Errorf("version %q should start with v1.2.4-0. (form 2, base v1.2.3)", ver)
	}
	if !strings.HasSuffix(ver, "-"+wantRev) {
		t.Errorf("version %q should end with -%s (12-byte revision)", ver, wantRev)
	}
}

// TestResolveVersionBranchNoBase checks the v0.0.0-… form (form 1) when no
// semver tag is advertised (only a non-semver tag).
func TestResolveVersionBranchNoBase(t *testing.T) {
	hasGit(t)
	t.Parallel()
	src, _, tip := makeRepo(t, "release-1") // only non-semver tag
	bare := makeBare(t, src)
	addr := Address{Host: "example.com", Repo: "linux", Ref: "main"}
	ver, err := resolveVersion(bare, addr, tip)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ver, "v0.0.0-") {
		t.Errorf("version %q should start with v0.0.0- (form 1, no base)", ver)
	}
}

// TestCheckoutTree verifies checkoutTree materialises a plain tree snapshot
// (no .git) at the given commit, borrowing the bare repo's objects.
func TestCheckoutTree(t *testing.T) {
	hasGit(t)
	t.Parallel()
	src, _, tip := makeRepo(t, "v1.2.3")
	bare := makeBare(t, src)
	treeDir := filepath.Join(t.TempDir(), "linux@v1.2.3")

	if err := checkoutTree(bare, tip, treeDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(treeDir, ".git")); err == nil {
		t.Error("treeDir should have no .git directory")
	}
	for _, f := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(treeDir, f)); err != nil {
			t.Errorf("checkout missing %s: %v", f, err)
		}
	}
}

// parseUnix parses a decimal unix timestamp, tolerating trailing whitespace.
func parseUnix(s string) (int64, error) { //nolint:unparam
	var n int64
	for _, ch := range strings.TrimSpace(s) {
		if ch < '0' || ch > '9' {
			break
		}
		n = n*10 + int64(ch-'0')
	}
	return n, nil
}

// TestRefMapRoundtrip covers the persisted ref→version mapping used to reuse a
// cached tree offline (when the fetch fails but a previous run resolved the
// ref). saveRefMap updates an existing entry in place and appends a new one.
func TestRefMapRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "refs")
	// Missing file → not found.
	if _, ok := lookupRefMap(path, "main"); ok {
		t.Fatal("lookup should miss on absent file")
	}
	if err := saveRefMap(path, "main", "v0.0.0-20260101000000-aaaaaaaaaaaa", "deadbeef"); err != nil {
		t.Fatal(err)
	}
	if err := saveRefMap(path, "v6.10.5", "v6.10.5", "cafef00d"); err != nil {
		t.Fatal(err)
	}
	// Update main in place (no duplicate line) and keep v6.10.5.
	if err := saveRefMap(path, "main", "v0.0.0-20260202000000-bbbbbbbbbbbb", "feedface"); err != nil {
		t.Fatal(err)
	}
	e, ok := lookupRefMap(path, "main")
	if !ok {
		t.Fatal("lookup main missed")
	}
	if e.version != "v0.0.0-20260202000000-bbbbbbbbbbbb" || e.commit != "feedface" {
		t.Errorf("updated entry = %+v", e)
	}
	if _, ok := lookupRefMap(path, "v6.10.5"); !ok {
		t.Error("v6.10.5 entry lost after update")
	}
	if _, ok := lookupRefMap(path, "nope"); ok {
		t.Error("lookup nope should miss")
	}
}
