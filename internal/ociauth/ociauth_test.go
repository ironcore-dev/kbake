// SPDX-FileCopyrightText: 2026 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package ociauth

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"oras.land/oras-go/v2/registry/remote/auth"
)

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestResolveStaticAuth(t *testing.T) {
	cfg := &dockerConfig{
		Auths: map[string]dockerAuthEntry{
			"ghcr.io": {Auth: b64("user:pass")},
		},
	}
	cred, err := cfg.resolve("ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "user" || cred.Password != "pass" {
		t.Errorf("got %+v", cred)
	}
}

func TestResolveIdentityToken(t *testing.T) {
	cfg := &dockerConfig{
		Auths: map[string]dockerAuthEntry{
			"ghcr.io": {IdentityToken: "idtoken"},
		},
	}
	cred, err := cfg.resolve("ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if cred.RefreshToken != "idtoken" || cred.Username != "" || cred.Password != "" {
		t.Errorf("identity token must map to RefreshToken only, got %+v", cred)
	}
}

// Credentials stored under docker/cli's canonical hub key must be found for
// any hub alias (oras redirects docker.io pulls to registry-1.docker.io).
func TestResolveHubAliases(t *testing.T) {
	cfg := &dockerConfig{
		Auths: map[string]dockerAuthEntry{
			indexDockerIOV1: {Auth: b64("hub:secret")},
		},
	}
	for _, hostport := range []string{"docker.io", "index.docker.io", "registry-1.docker.io"} {
		cred, err := cfg.resolve(hostport)
		if err != nil {
			t.Fatal(err)
		}
		if cred.Username != "hub" || cred.Password != "secret" {
			t.Errorf("resolve(%q) = %+v, want hub credentials", hostport, cred)
		}
	}
}

func TestResolveNoMatchIsAnonymous(t *testing.T) {
	cfg := &dockerConfig{}
	cred, err := cfg.resolve("ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if cred != (auth.Credential{}) {
		t.Errorf("expected zero credential, got %+v", cred)
	}
}

// writeFakeHelper installs a docker-credential-fake binary on PATH that
// answers the helper protocol's `get` with fixed JSON.
func writeFakeHelper(t *testing.T, suffix, username, secret string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("helper protocol shells out; test uses a sh script")
	}
	out, _ := json.Marshal(map[string]string{"ServerURL": "x", "Username": username, "Secret": secret})
	dir := t.TempDir()
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s' '" + string(out) + "'\n"
	path := filepath.Join(dir, "docker-credential-"+suffix)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestResolveHelperCredsStore(t *testing.T) {
	writeFakeHelper(t, "fakestore", "huser", "hsecret")
	cfg := &dockerConfig{CredsStore: "fakestore"}

	cred, err := cfg.resolve("ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "huser" || cred.Password != "hsecret" {
		t.Errorf("got %+v", cred)
	}
}

func TestResolveHelperTokenSentinel(t *testing.T) {
	writeFakeHelper(t, "faketoken", "<token>", "refreshtoken123")
	cfg := &dockerConfig{CredsStore: "faketoken"}

	cred, err := cfg.resolve("ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if cred.RefreshToken != "refreshtoken123" || cred.Username != "" || cred.Password != "" {
		t.Errorf(`"<token>" user must map to RefreshToken only, got %+v`, cred)
	}
}

// A per-registry credHelper beats the global credsStore (docker/cli
// GetAuthConfig precedence).
func TestResolveHelperPrecedence(t *testing.T) {
	writeFakeHelper(t, "perreg", "perreg-user", "perreg-secret")
	writeFakeHelper(t, "global", "global-user", "global-secret")
	cfg := &dockerConfig{
		CredsStore:  "global",
		CredHelpers: map[string]string{"ghcr.io": "perreg"},
	}

	cred, err := cfg.resolve("ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "perreg-user" {
		t.Errorf("credHelpers should win over credsStore, got %+v", cred)
	}

	// ...and the global helper still serves other registries.
	cred, err = cfg.resolve("quay.io")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "global-user" {
		t.Errorf("credsStore should serve unmatched registries, got %+v", cred)
	}
}

// A static entry is used only when no helper matches.
func TestResolveStaticAfterHelpers(t *testing.T) {
	cfg := &dockerConfig{
		Auths: map[string]dockerAuthEntry{"ghcr.io": {Auth: b64("static:entry")}},
	}
	cred, err := cfg.resolve("ghcr.io")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "static" {
		t.Errorf("got %+v", cred)
	}
}

func TestValidHelperSuffix(t *testing.T) {
	for _, ok := range []string{"osxkeychain", "desktop", "ecr-login", "a_b-c.d"} {
		if !validHelperSuffix(ok) {
			t.Errorf("%q should be a valid suffix", ok)
		}
	}
	for _, bad := range []string{"", "../evil", "a/b", "a b", `a"b`, "$(x)"} {
		if validHelperSuffix(bad) {
			t.Errorf("%q must be rejected (path traversal / injection guard)", bad)
		}
	}
}

// A missing config is fine (anonymous pulls); a malformed one errors with the
// file path attached.
func TestLoadDockerConfig(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", filepath.Join(t.TempDir(), "nonexistent"))
	cfg, err := loadDockerConfig()
	if err != nil || cfg != nil {
		t.Errorf("missing config: want (nil, nil), got (%v, %v)", cfg, err)
	}

	bad := t.TempDir()
	if err := os.WriteFile(filepath.Join(bad, "config.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", bad)
	if _, err := loadDockerConfig(); err == nil {
		t.Error("malformed config should error")
	}
}
