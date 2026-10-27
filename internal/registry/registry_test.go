package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const indexDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// fakeRegistry answers manifest HEADs. Repository "private/app" requires a
// bearer token obtained with user:pass; "missing/app" is a 404 and
// "limited/app" a 429.
type fakeRegistry struct {
	srv    *httptest.Server
	tokens atomic.Int32
	heads  atomic.Int32
	gets   atomic.Int32
}

func newFakeRegistry(t *testing.T) *fakeRegistry {
	f := &fakeRegistry{}
	mux := http.NewServeMux()
	// Like Docker Hub: /v2/ always answers 401 with a bearer challenge, the
	// token endpoint hands out anonymous tokens, and authenticated ones to
	// user:pass.
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		token := "anonymous"
		if user, pass, ok := r.BasicAuth(); ok {
			if user != "user" || pass != "pass" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			token = "tok"
			f.tokens.Add(1)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": token, "expires_in": 300})
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		challenge := func() {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="fake"`, f.srv.URL))
			w.WriteHeader(http.StatusUnauthorized)
		}
		if r.URL.Path == "/v2/" {
			challenge()
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			challenge()
			return
		}
		if r.Method != http.MethodHead {
			f.gets.Add(1)
		} else {
			f.heads.Add(1)
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/v2/private/"):
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		case strings.HasPrefix(r.URL.Path, "/v2/missing/"):
			w.WriteHeader(http.StatusNotFound)
			return
		case strings.HasPrefix(r.URL.Path, "/v2/limited/"):
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Header().Set("Content-Length", "100")
		w.Header().Set("Docker-Content-Digest", indexDigest)
		w.WriteHeader(http.StatusOK)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRegistry) host() string { return strings.TrimPrefix(f.srv.URL, "http://") }

func writeAuth(t *testing.T, host string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	auth := base64.StdEncoding.EncodeToString([]byte("user:pass"))
	b, _ := json.Marshal(map[string]any{"auths": map[string]any{host: map[string]string{"auth": auth}}})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDigestPublicAndPrivate(t *testing.T) {
	f := newFakeRegistry(t)
	creds, err := LoadCredentials(writeAuth(t, f.host()))
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewChecker(creds, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	d, err := c.Digest(ctx, f.host()+"/public/app:1")
	if err != nil || d != indexDigest {
		t.Fatalf("public: %s, %v", d, err)
	}
	for i := 0; i < 3; i++ {
		d, err = c.Digest(ctx, f.host()+"/private/app:latest")
		if err != nil || d != indexDigest {
			t.Fatalf("private: %s, %v", d, err)
		}
	}
	// Tokens are scoped per repository: one for public/app, one for
	// private/app reused by the three HEADs.
	if n := f.tokens.Load(); n != 2 {
		t.Errorf("token fetched %d times, want 2 (reused until expiry)", n)
	}
	if n := f.gets.Load(); n != 0 {
		t.Errorf("%d GET requests, want only HEADs (no pull)", n)
	}
}

func TestDigestErrors(t *testing.T) {
	f := newFakeRegistry(t)
	creds, _ := LoadCredentials("")
	c, _ := NewChecker(creds, "test", true)
	ctx := context.Background()

	cases := map[string]int{
		"/missing/app:1": http.StatusNotFound,
		"/limited/app:1": http.StatusTooManyRequests,
		"/private/app:1": http.StatusUnauthorized, // no credentials
	}
	for path, code := range cases {
		_, err := c.Digest(ctx, f.host()+path)
		var he *HeadError
		if !errors.As(err, &he) || he.StatusCode != code {
			t.Errorf("%s: err = %v, want HTTP %d", path, err, code)
		}
		if code == http.StatusTooManyRequests && !he.RateLimited() {
			t.Error("429 must be reported as rate limited")
		}
	}

	_, err := c.Digest(ctx, "127.0.0.1:1/nothing/here:1")
	var he *HeadError
	if !errors.As(err, &he) || he.StatusCode != 0 {
		t.Errorf("unreachable registry: %v", err)
	}
}

func TestEncodedAuth(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	b := []byte(`{"auths":{"https://index.docker.io/v1/":{"auth":"` +
		base64.StdEncoding.EncodeToString([]byte("hubuser:hubpass")) + `"},"ghcr.io":{"username":"u","password":"p4ssword"}}}`)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := LoadCredentials(p)
	if err != nil {
		t.Fatal(err)
	}

	enc, err := creds.EncodedAuth("nginx:latest")
	if err != nil || enc == "" {
		t.Fatalf("docker hub auth: %q, %v", enc, err)
	}
	raw, _ := base64.URLEncoding.DecodeString(enc)
	var got map[string]string
	_ = json.Unmarshal(raw, &got)
	if got["username"] != "hubuser" || got["password"] != "hubpass" {
		t.Errorf("decoded auth = %v", got)
	}

	if enc, _ := creds.EncodedAuth("quay.io/org/app:1"); enc != "" {
		t.Error("no credentials expected for quay.io")
	}
	if enc, _ := creds.EncodedAuth("ghcr.io/org/app:1"); enc == "" {
		t.Error("credentials expected for ghcr.io")
	}
}

func TestInvalidAuthFileDoesNotLeak(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(p, []byte(`{"auths": topsecretvalue`), 0o600)
	_, err := LoadCredentials(p)
	if err == nil || strings.Contains(err.Error(), "topsecret") {
		t.Errorf("err = %v", err)
	}
}

func TestHeadReliable(t *testing.T) {
	if !HeadReliable("index.docker.io") || !HeadReliable("ghcr.io") || HeadReliable("registry.example.com") {
		t.Error("unexpected HeadReliable results")
	}
}
