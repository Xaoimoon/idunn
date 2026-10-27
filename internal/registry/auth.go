// Package registry checks image digests with manifest HEAD requests and
// provides registry credentials for Swarm pulls.
package registry

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	"github.com/xaoimoon/idunn/internal/redact"
)

const dockerHub = "index.docker.io"

// Credentials holds the auths section of a Docker config.json.
type Credentials struct {
	byRegistry map[string]authn.AuthConfig
}

type dockerConfig struct {
	Auths map[string]authn.AuthConfig `json:"auths"`
}

// LoadCredentials reads a Docker config.json. An empty path yields anonymous
// access everywhere.
func LoadCredentials(path string) (*Credentials, error) {
	c := &Credentials{byRegistry: map[string]authn.AuthConfig{}}
	if path == "" {
		return c, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("registry auth file: cannot read %s", path)
	}
	var cfg dockerConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		// The JSON error could quote part of the file; keep it out.
		return nil, fmt.Errorf("registry auth file %s: invalid JSON", path)
	}
	for host, a := range cfg.Auths {
		if a.Auth != "" && a.Username == "" {
			if raw, err := base64.StdEncoding.DecodeString(a.Auth); err == nil {
				if user, pass, ok := strings.Cut(string(raw), ":"); ok {
					a.Username, a.Password = user, pass
				}
			}
		}
		a.Auth = ""
		redact.Add(a.Password)
		redact.Add(a.IdentityToken)
		redact.Add(a.RegistryToken)
		c.byRegistry[normalizeHost(host)] = a
	}
	return c, nil
}

// normalizeHost maps the spellings found in config.json keys
// ("https://index.docker.io/v1/", "docker.io", "ghcr.io") to a registry host.
func normalizeHost(h string) string {
	h = strings.TrimPrefix(h, "https://")
	h = strings.TrimPrefix(h, "http://")
	h, _, _ = strings.Cut(h, "/")
	switch h {
	case "docker.io", "registry-1.docker.io", "registry.hub.docker.com":
		return dockerHub
	}
	return h
}

// lookup returns the credentials for a registry host.
func (c *Credentials) lookup(registry string) (authn.AuthConfig, bool) {
	a, ok := c.byRegistry[normalizeHost(registry)]
	return a, ok
}

// Resolve implements authn.Keychain.
func (c *Credentials) Resolve(r authn.Resource) (authn.Authenticator, error) {
	if a, ok := c.lookup(r.RegistryStr()); ok {
		return authn.FromConfig(a), nil
	}
	return authn.Anonymous, nil
}

// EncodedAuth returns the X-Registry-Auth value Swarm needs to pull image on
// every node, or "" for anonymous pulls.
func (c *Credentials) EncodedAuth(image string) (string, error) {
	ref, err := name.ParseReference(image)
	if err != nil {
		return "", err
	}
	a, ok := c.lookup(ref.Context().RegistryStr())
	if !ok {
		return "", nil
	}
	payload := map[string]string{
		"username":      a.Username,
		"password":      a.Password,
		"identitytoken": a.IdentityToken,
		"registrytoken": a.RegistryToken,
		"serveraddress": ref.Context().RegistryStr(),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}
