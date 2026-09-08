// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

// Package ociauth builds registry repositories authenticated from the user's
// local Docker config ($DOCKER_CONFIG/config.json or ~/.docker/config.json).
//
// Both static credentials ("auths") and credential helpers
// ("credsStore"/"credHelpers", e.g. docker-credential-osxkeychain as used by
// Docker Desktop on macOS) are supported — without depending on docker/cli:
// the config subset kbake needs is parsed directly, and helpers are invoked
// through the small standalone github.com/docker/docker-credential-helpers
// module, the same helper protocol docker/cli itself shells out to.
//
// The docker config is read lazily on first repository creation, so a
// malformed or unreadable config only surfaces for commands that actually
// contact a registry.
package ociauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	helperclient "github.com/docker/docker-credential-helpers/client"
	helpercreds "github.com/docker/docker-credential-helpers/credentials"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
)

// indexDockerIOV1 is docker/cli's canonical auth key for Docker Hub.
const indexDockerIOV1 = "https://index.docker.io/v1/"

// dockerHubHosts are the aliases referring to Docker Hub's registry service.
// oras redirects docker.io traffic to registry-1.docker.io, so a credential
// lookup for a hub pull can arrive under any of these names.
var dockerHubHosts = map[string]bool{
	"docker.io":            true,
	"index.docker.io":      true,
	"registry-1.docker.io": true,
}

// dockerConfig mirrors the subset of ~/.docker/config.json kbake consumes.
type dockerConfig struct {
	Auths       map[string]dockerAuthEntry `json:"auths"`
	CredsStore  string                     `json:"credsStore"`
	CredHelpers map[string]string          `json:"credHelpers"`
}

type dockerAuthEntry struct {
	Auth          string `json:"auth"` // base64("user:pass")
	IdentityToken string `json:"identitytoken"`
	Username      string `json:"username"`
	Password      string `json:"password"`
}

// normalizeHost folds the Docker Hub aliases to the canonical host. A
// hostport arrives scheme-less; scheme/path are stripped defensively.
func normalizeHost(hostport string) string {
	host := strings.TrimPrefix(hostport, "https://")
	host = strings.TrimPrefix(host, "http://")
	host, _, _ = strings.Cut(host, "/")
	if dockerHubHosts[host] {
		return "index.docker.io"
	}
	return host
}

// authServerKey is the key docker uses in auths/credHelpers entries for host.
func authServerKey(hostport string) string {
	if normalizeHost(hostport) == "index.docker.io" {
		return indexDockerIOV1
	}
	return normalizeHost(hostport)
}

// credential converts a static entry. IdentityToken maps to a refresh token,
// mirroring docker/cli.
func (e dockerAuthEntry) credential() (auth.Credential, error) {
	if e.IdentityToken != "" {
		return auth.Credential{RefreshToken: e.IdentityToken}, nil
	}
	if e.Auth != "" {
		raw, err := base64.StdEncoding.DecodeString(e.Auth)
		if err != nil {
			return auth.Credential{}, fmt.Errorf("malformed base64 in auth entry: %w", err)
		}
		user, pass, ok := strings.Cut(string(raw), ":")
		if !ok {
			return auth.Credential{}, errors.New("malformed auth entry (want base64-encoded user:pass)")
		}
		return auth.Credential{Username: user, Password: pass}, nil
	}
	return auth.Credential{Username: e.Username, Password: e.Password}, nil
}

// resolve mirrors docker/cli's GetAuthConfig precedence: a per-registry
// helper (credHelpers), then the global helper (credsStore), then a static
// auths entry. A helper reporting "credentials not found" (e.g.
// docker-credential-desktop on a machine that never logged in to the
// registry) is not an error — like docker/cli, resolution moves on to the
// next source. The zero Credential means "no credentials configured" — the
// request proceeds anonymously.
func (c *dockerConfig) resolve(hostport string) (auth.Credential, error) {
	key := authServerKey(hostport)
	host := normalizeHost(hostport)

	if suffix, ok := c.CredHelpers[key]; ok {
		cred, err := fromHelper(suffix, key)
		if !errors.Is(err, errNotInStore) {
			return cred, err
		}
	} else if key != host {
		// Some configs key hub helpers by bare host name instead.
		if suffix, ok := c.CredHelpers[host]; ok {
			cred, err := fromHelper(suffix, host)
			if !errors.Is(err, errNotInStore) {
				return cred, err
			}
		}
	}
	if c.CredsStore != "" {
		cred, err := fromHelper(c.CredsStore, key)
		if !errors.Is(err, errNotInStore) {
			return cred, err
		}
	}
	if entry, ok := c.Auths[key]; ok {
		return entry.credential()
	}
	if key != host {
		if entry, ok := c.Auths[host]; ok {
			return entry.credential()
		}
	}
	return auth.Credential{}, nil
}

// errNotInStore marks a credential helper's standard "no credentials for
// this server" outcome. docker/cli treats it as "no credentials configured"
// and proceeds anonymously; kbake mirrors that so a global credsStore (e.g.
// Docker Desktop's docker-credential-desktop) cannot break anonymous pulls
// of public images for users who never ran docker login.
var errNotInStore = errors.New("credentials not found in credential store")

// fromHelper invokes docker-credential-<suffix> via the documented helper
// protocol. The suffix is validated so a crafted config file cannot turn the
// helper name into an arbitrary path.
func fromHelper(suffix, serverURL string) (auth.Credential, error) {
	if !validHelperSuffix(suffix) {
		return auth.Credential{}, fmt.Errorf("invalid credential helper suffix %q", suffix)
	}
	prog := helperclient.NewShellProgramFunc("docker-credential-" + suffix)
	creds, err := helperclient.Get(prog, serverURL)
	if err != nil {
		if helpercreds.IsErrCredentialsNotFound(err) {
			return auth.Credential{}, errNotInStore
		}
		return auth.Credential{}, fmt.Errorf("docker-credential-%s get %s: %w", suffix, serverURL, err)
	}
	// "<token>" is the helper protocol's sentinel for an identity token.
	if creds.Username == "<token>" {
		return auth.Credential{RefreshToken: creds.Secret}, nil
	}
	return auth.Credential{Username: creds.Username, Password: creds.Secret}, nil
}

func validHelperSuffix(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

func configPath() string {
	if dir := os.Getenv("DOCKER_CONFIG"); dir != "" {
		return filepath.Join(dir, "config.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".docker", "config.json")
}

// loadDockerConfig returns nil (no error) when no docker config exists —
// pulling anonymously is the normal state on machines without a docker
// login.
func loadDockerConfig() (*dockerConfig, error) {
	path := configPath()
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg dockerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &cfg, nil
}

var (
	clientOnce sync.Once
	client     *auth.Client
	clientErr  error
)

// defaultClient builds (once) the shared registry client from the local
// docker config. A nil client (no error) means no config exists — pull
// anonymously.
func defaultClient() (*auth.Client, error) {
	clientOnce.Do(func() {
		cfg, err := loadDockerConfig()
		if err != nil {
			clientErr = err
			return
		}
		if cfg == nil {
			return
		}
		client = &auth.Client{
			Cache: auth.NewCache(),
			Credential: func(_ context.Context, hostport string) (auth.Credential, error) {
				return cfg.resolve(hostport)
			},
		}
	})
	return client, clientErr
}

// NewRepository creates a remote.Repository for ref's registry, authenticated
// from the local docker config (static entries and docker-credential-*
// helpers alike). It is the repository factory kbake's CLI passes to
// registry-touching commands.
func NewRepository(ref registry.Reference) (*remote.Repository, error) {
	repo, err := remote.NewRepository(ref.Registry + "/" + ref.Repository)
	if err != nil {
		return nil, err
	}
	c, err := defaultClient()
	if err != nil {
		return nil, fmt.Errorf("registry auth from docker config: %w", err)
	}
	if c != nil {
		repo.Client = c
	}
	return repo, nil
}
