package cleanup

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/client"
)

type fakeLocal struct {
	images     []image.Summary
	containers []container.Summary
	removed    []string
	failOn     map[string]error
}

func (f *fakeLocal) ImageList(context.Context, client.ImageListOptions) (client.ImageListResult, error) {
	return client.ImageListResult{Items: f.images}, nil
}

func (f *fakeLocal) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{Items: f.containers}, nil
}

func (f *fakeLocal) ImageRemove(_ context.Context, id string, _ client.ImageRemoveOptions) (client.ImageRemoveResult, error) {
	if err := f.failOn[id]; err != nil {
		return client.ImageRemoveResult{}, err
	}
	f.removed = append(f.removed, id)
	return client.ImageRemoveResult{}, nil
}

func img(id string, created int64, refs ...string) image.Summary {
	s := image.Summary{ID: id, Created: created}
	for _, r := range refs {
		if strings.Contains(r, "@") {
			s.RepoDigests = append(s.RepoDigests, r)
		} else {
			s.RepoTags = append(s.RepoTags, r)
		}
	}
	return s
}

const dg = "@sha256:0000000000000000000000000000000000000000000000000000000000000000"

func TestLocalKeepsInUseAndMostRecent(t *testing.T) {
	f := &fakeLocal{
		images: []image.Summary{
			img("current", 50, "nginx"+dg),                // in use
			img("old1", 40, "nginx"+dg),                   // most recent unused: kept
			img("old2", 30, "docker.io/library/nginx"+dg), // removed
			img("old3", 20, "nginx:1.25"),                 // removed
			img("other", 10, "redis"+dg),                  // other repository
		},
		containers: []container.Summary{{ImageID: "current"}},
	}
	err := Local(context.Background(), f, []string{"index.docker.io/library/nginx"}, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(f.removed)
	if len(f.removed) != 2 || f.removed[0] != "old2" || f.removed[1] != "old3" {
		t.Errorf("removed = %v", f.removed)
	}
}

func TestLocalKeepZeroAndConflicts(t *testing.T) {
	f := &fakeLocal{
		images: []image.Summary{
			img("a", 2, "ghcr.io/org/app"+dg),
			img("b", 1, "ghcr.io/org/app"+dg),
		},
		failOn: map[string]error{"a": errors.New("Error response from daemon: conflict: unable to delete a (must be forced)")},
	}
	err := Local(context.Background(), f, []string{"ghcr.io/org/app"}, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("a conflict is not a failure: %v", err)
	}
	if len(f.removed) != 1 || f.removed[0] != "b" {
		t.Errorf("removed = %v", f.removed)
	}

	f.failOn = map[string]error{"b": errors.New("disk on fire")}
	f.removed = nil
	if err := Local(context.Background(), f, []string{"ghcr.io/org/app"}, 0, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
		t.Error("an unexpected removal error must fail the task")
	}
}
