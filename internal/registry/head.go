package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/xaoimoon/idunn/internal/redact"
)

// Checker resolves the current digest of image tags. It keeps one puller for
// its whole life so that registry tokens are reused until they expire.
type Checker struct {
	puller *remote.Puller
	opts   []name.Option
}

// NewChecker builds a checker using creds for every registry. Extra options
// are for tests (e.g. remote.WithTransport); insecure allows plain HTTP.
func NewChecker(creds *Credentials, userAgent string, insecure bool, extra ...remote.Option) (*Checker, error) {
	opts := append([]remote.Option{
		remote.WithAuthFromKeychain(creds),
		remote.WithUserAgent(userAgent),
	}, extra...)
	p, err := remote.NewPuller(opts...)
	if err != nil {
		return nil, err
	}
	c := &Checker{puller: p}
	if insecure {
		c.opts = append(c.opts, name.Insecure)
	}
	return c, nil
}

// HeadError describes a failed manifest HEAD.
type HeadError struct {
	Registry   string
	StatusCode int // 0 when the registry could not be reached
	Err        error
}

func (e *HeadError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("registry %s answered HTTP %d", e.Registry, e.StatusCode)
	}
	return fmt.Sprintf("registry %s unreachable: %v", e.Registry, redact.Error(e.Err))
}

func (e *HeadError) Unwrap() error { return e.Err }

// RateLimited reports whether the registry returned HTTP 429.
func (e *HeadError) RateLimited() bool { return e.StatusCode == http.StatusTooManyRequests }

// Digest returns the digest the registry serves for an image tag. For a
// multi-arch image this is the index digest, the one Swarm pins.
func (c *Checker) Digest(ctx context.Context, image string) (string, error) {
	ref, err := name.ParseReference(image, c.opts...)
	if err != nil {
		return "", fmt.Errorf("invalid image reference %q: %w", image, err)
	}
	desc, err := c.puller.Head(ctx, ref)
	if err != nil {
		he := &HeadError{Registry: ref.Context().RegistryStr(), Err: err}
		var te *transport.Error
		if errors.As(err, &te) {
			he.StatusCode = te.StatusCode
		}
		return "", he
	}
	return desc.Digest.String(), nil
}

// RegistryOf returns the registry host of an image reference.
func RegistryOf(image string) string {
	ref, err := name.ParseReference(image)
	if err != nil {
		return ""
	}
	return ref.Context().RegistryStr()
}

// headReliable lists registries known to answer manifest HEAD requests. In
// "auto" mode a failed HEAD is only notified for these; elsewhere a failure
// may just mean the registry does not support HEAD, so it is only logged.
var headReliable = map[string]bool{
	dockerHub:           true,
	"ghcr.io":           true,
	"quay.io":           true,
	"lscr.io":           true,
	"gcr.io":            true,
	"registry.k8s.io":   true,
	"public.ecr.aws":    true,
	"mcr.microsoft.com": true,
}

// HeadReliable reports whether registry is known to support HEAD requests.
func HeadReliable(registry string) bool {
	return headReliable[registry] || strings.HasSuffix(registry, ".pkg.dev")
}
