package updater

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/xaoimoon/idunn/internal/notify"
	"github.com/xaoimoon/idunn/internal/versions"
)

// fakeLister serves tags per repository and counts listings.
type fakeLister struct {
	mu    sync.Mutex
	tags  map[string][]string // repository (image without tag) -> tags
	calls map[string]int
}

func (l *fakeLister) Tags(_ context.Context, image string) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	repo := image[:strings.LastIndex(image, ":")]
	l.calls[repo]++
	tags, ok := l.tags[repo]
	if !ok {
		return nil, errors.New("not found")
	}
	return tags, nil
}

func TestVersionCheck(t *testing.T) {
	h := newHarness(t)
	h.cfg.VersionLevel = versions.Patch
	lister := &fakeLister{calls: map[string]int{}, tags: map[string][]string{
		"mariadb":         {"12.3.3", "12.3.4", "13.0.0", "13.0.0-rc1", "latest"},
		"haproxy":         {"2.9-alpine", "3.2-alpine", "3.2"},
		"example.com/app": {"v1.0.0", "v1.1.0"},
	}}
	h.u.Lister = lister

	// Not opted in to updates, still checked.
	h.docker.add("s1", "db_node1", "mariadb:12.3.3@"+d1, map[string]string{"idunn.enable": "false"})
	h.docker.add("s2", "db_node2", "mariadb:12.3.3@"+d1, map[string]string{"idunn.version-level": "minor"})
	h.docker.add("s3", "db_haproxy", "haproxy:2.9-alpine@"+d1, nil)
	// Stack file pinning tag@digest: the tag is still compared.
	h.docker.add("s4", "self", "example.com/app:v1.0.0@"+d1, map[string]string{
		"com.docker.stack.image": "example.com/app:v1.0.0@" + d1})
	h.docker.add("s5", "web", "nginx:latest@"+d1, nil)
	h.docker.add("s6", "quiet", "haproxy:2.9-alpine@"+d1, map[string]string{"idunn.version-level": "none"})
	h.docker.add("s7", "gone", "example.com/missing:1.0@"+d1, nil)

	h.u.VersionCheck(context.Background())

	got := map[string]string{}
	for _, e := range h.sink.all() {
		if e.Kind != notify.KindNewVersion {
			t.Fatalf("unexpected event %s", e.Kind)
		}
		got[e.Service] = e.Tag + " " + e.Bump
	}
	want := map[string]string{
		"db_node1":   "13.0.0 major",
		"db_node2":   "13.0.0 major",
		"db_haproxy": "3.2-alpine major",
		"self":       "v1.1.0 minor",
	}
	if len(got) != len(want) {
		t.Errorf("events = %v, want %v", got, want)
	}
	for s, w := range want {
		if got[s] != w {
			t.Errorf("%s: %q, want %q", s, got[s], w)
		}
	}
	if len(h.sink.passes) != 1 {
		t.Errorf("%d notifications, want a single report", len(h.sink.passes))
	}
	if lister.calls["mariadb"] != 1 || lister.calls["haproxy"] != 1 {
		t.Errorf("listings = %v, want one per repository", lister.calls)
	}
	if lister.calls["nginx"] != 0 {
		t.Error("a non-version tag must not be listed")
	}
	if len(h.docker.updates) != 0 {
		t.Error("the version check must never update a service")
	}
}

func TestVersionCheckFollowerDoesNothing(t *testing.T) {
	h := newHarness(t)
	h.docker.leader = false
	lister := &fakeLister{calls: map[string]int{}, tags: map[string][]string{"mariadb": {"13.0.0"}}}
	h.u.Lister = lister
	h.docker.add("s1", "db", "mariadb:12.3.3", nil)

	h.u.VersionCheck(context.Background())

	if len(h.sink.all()) != 0 || len(lister.calls) != 0 {
		t.Error("a follower must not check versions")
	}
}

func TestReferenceImage(t *testing.T) {
	h := newHarness(t)
	h.docker.add("s1", "a", "nginx:1.27@"+d1, map[string]string{"com.docker.stack.image": "nginx:1.27"})
	h.docker.add("s2", "b", "nginx:1.27@"+d1, nil)
	var images []string
	for _, id := range []string{"s1", "s2"} {
		img, cur := referenceImage(*h.docker.services[id])
		if cur != d1 {
			t.Errorf("%s: current = %q", id, cur)
		}
		images = append(images, img)
	}
	sort.Strings(images)
	if images[0] != "nginx:1.27" || images[1] != "nginx:1.27" {
		t.Errorf("images = %v", images)
	}
}
