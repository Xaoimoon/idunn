// Package redact masks registered secrets in strings before they reach logs,
// notifications or error messages.
package redact

import (
	"net/url"
	"sort"
	"strings"
	"sync"
)

const mask = "[REDACTED]"

// minLen avoids masking short, common substrings such as a scheme or port.
const minLen = 6

var (
	mu      sync.RWMutex
	secrets []string
)

// Add registers a secret value.
func Add(s string) {
	s = strings.TrimSpace(s)
	if len(s) < minLen {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	for _, existing := range secrets {
		if existing == s {
			return
		}
	}
	secrets = append(secrets, s)
	// Longest first, so that a secret containing another is masked whole.
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
}

// AddURL registers a URL and its sensitive parts (user info, path segments,
// query values), which notification services turn into tokens and webhook IDs.
func AddURL(raw string) {
	Add(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return
	}
	if u.User != nil {
		Add(u.User.Username())
		if p, ok := u.User.Password(); ok {
			Add(p)
		}
	}
	// Hosts and path segments are only masked when they look like IDs or
	// tokens, so that words such as "webhooks" stay readable in logs.
	addIfToken(u.Hostname())
	for _, seg := range strings.Split(u.Path, "/") {
		addIfToken(seg)
	}
	for _, values := range u.Query() {
		for _, v := range values {
			addIfToken(v)
		}
	}
}

func addIfToken(s string) {
	if strings.ContainsAny(s, "0123456789") {
		Add(s)
	}
}

// String masks every registered secret in s.
func String(s string) string {
	mu.RLock()
	defer mu.RUnlock()
	for _, secret := range secrets {
		s = strings.ReplaceAll(s, secret, mask)
	}
	return s
}

// Error returns a masked copy of err's message, or nil.
func Error(err error) error {
	if err == nil {
		return nil
	}
	return redactedError{msg: String(err.Error()), err: err}
}

type redactedError struct {
	msg string
	err error
}

func (e redactedError) Error() string { return e.msg }
func (e redactedError) Unwrap() error { return e.err }

// reset clears the registry; tests only.
func reset() {
	mu.Lock()
	secrets = nil
	mu.Unlock()
}
