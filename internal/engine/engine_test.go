package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	dockerspec "github.com/moby/docker-image-spec/specs-go/v1"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/jsonstream"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	cerrdefs "github.com/containerd/errdefs"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fake is an in-memory engine. A container created from an image listed in
// broken exits right after it starts.
type fake struct {
	mu         sync.Mutex
	containers map[string]*container.InspectResponse
	images     map[string]image.InspectResponse // by ID
	tags       map[string]string                // reference -> image ID
	broken     map[string]bool                  // image IDs whose containers exit
	pulls      []string
	next       int
	connects   []string
}

func newFakeEngine() *fake {
	return &fake{
		containers: map[string]*container.InspectResponse{},
		images:     map[string]image.InspectResponse{},
		tags:       map[string]string{},
		broken:     map[string]bool{},
	}
}

func (f *fake) image(id, ref string, cfg ocispec.ImageConfig) {
	f.images[id] = image.InspectResponse{ID: id, RepoDigests: []string{ref + "@sha256:" + strings.Repeat(id[len(id)-1:], 64)},
		Config: &dockerspec.DockerOCIImageConfig{ImageConfig: cfg}}
	f.tags[ref] = id
}

func (f *fake) run(name, ref string, cfg *container.Config, hc *container.HostConfig) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("%064d", f.next)
	if cfg == nil {
		cfg = &container.Config{}
	}
	cfg.Image = ref
	if hc == nil {
		hc = &container.HostConfig{}
	}
	f.containers[id] = &container.InspectResponse{
		ID: id, Name: "/" + name, Image: f.tags[ref], Config: cfg, HostConfig: hc,
		State:           &container.State{Status: container.StateRunning, Running: true},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{}},
	}
	return id
}

func (f *fake) find(ref string) *container.InspectResponse {
	if c, ok := f.containers[ref]; ok {
		return c
	}
	for _, c := range f.containers {
		if c.Name == "/"+ref {
			return c
		}
	}
	return nil
}

func (f *fake) byName(name string) *container.InspectResponse {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.find(name)
}

func (f *fake) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []container.Summary
	for id, c := range f.containers {
		if c.State.Running {
			out = append(out, container.Summary{ID: id})
		}
	}
	return client.ContainerListResult{Items: out}, nil
}

func (f *fake) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.find(id)
	if c == nil {
		return client.ContainerInspectResult{}, cerrdefs.ErrNotFound.WithMessage("no such container")
	}
	cp := *c
	st := *c.State
	cp.State = &st
	return client.ContainerInspectResult{Container: cp}, nil
}

func (f *fake) ContainerCreate(_ context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.find(o.Name) != nil {
		return client.ContainerCreateResult{}, errors.New("name in use")
	}
	f.next++
	id := fmt.Sprintf("%064d", f.next)
	nets := map[string]*network.EndpointSettings{}
	if o.NetworkingConfig != nil {
		for k, v := range o.NetworkingConfig.EndpointsConfig {
			nets[k] = v
		}
	}
	f.containers[id] = &container.InspectResponse{
		ID: id, Name: "/" + o.Name, Image: f.tags[o.Config.Image], Config: o.Config, HostConfig: o.HostConfig,
		State:           &container.State{Status: container.StateCreated},
		NetworkSettings: &container.NetworkSettings{Networks: nets},
	}
	return client.ContainerCreateResult{ID: id}, nil
}

func (f *fake) ContainerStart(_ context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.find(id)
	if c == nil {
		return client.ContainerStartResult{}, errors.New("no such container")
	}
	if f.broken[c.Image] {
		c.State = &container.State{Status: container.StateExited, ExitCode: 1}
	} else {
		c.State = &container.State{Status: container.StateRunning, Running: true}
	}
	return client.ContainerStartResult{}, nil
}

func (f *fake) ContainerStop(_ context.Context, id string, _ client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.find(id); c != nil {
		c.State = &container.State{Status: container.StateExited}
	}
	return client.ContainerStopResult{}, nil
}

func (f *fake) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.find(id)
	if c == nil {
		return client.ContainerRemoveResult{}, cerrdefs.ErrNotFound
	}
	delete(f.containers, c.ID)
	return client.ContainerRemoveResult{}, nil
}

func (f *fake) ContainerRename(_ context.Context, id string, o client.ContainerRenameOptions) (client.ContainerRenameResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if other := f.find(o.NewName); other != nil && other.ID != id {
		return client.ContainerRenameResult{}, errors.New("name in use")
	}
	f.find(id).Name = "/" + o.NewName
	return client.ContainerRenameResult{}, nil
}

type pullResp struct{ io.ReadCloser }

func (pullResp) JSONMessages(context.Context) iter.Seq2[jsonstream.Message, error] { return nil }
func (pullResp) Wait(context.Context) error                                        { return nil }

func (f *fake) ImagePull(_ context.Context, ref string, _ client.ImagePullOptions) (client.ImagePullResponse, error) {
	f.mu.Lock()
	f.pulls = append(f.pulls, ref)
	f.mu.Unlock()
	return pullResp{io.NopCloser(strings.NewReader(""))}, nil
}

func (f *fake) ImageInspect(_ context.Context, ref string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, ok := f.tags[ref]; ok {
		ref = id
	}
	img, ok := f.images[ref]
	if !ok {
		return client.ImageInspectResult{}, cerrdefs.ErrNotFound
	}
	return client.ImageInspectResult{InspectResponse: img}, nil
}

func (f *fake) NetworkConnect(_ context.Context, name string, o client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connects = append(f.connects, name)
	f.find(o.Container).NetworkSettings.Networks[name] = o.EndpointConfig
	return client.NetworkConnectResult{}, nil
}

func inspect(t *testing.T, f *fake, id string) *Container {
	t.Helper()
	c, err := Inspect(context.Background(), f, id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNewSpecKeepsOnlyWhatWasSetOnTheContainer(t *testing.T) {
	img := &dockerspec.DockerOCIImageConfig{ImageConfig: ocispec.ImageConfig{
		Env:          []string{"PATH=/usr/bin", "APP_VERSION=1"},
		Cmd:          []string{"serve"},
		Entrypoint:   []string{"/app"},
		WorkingDir:   "/srv",
		Labels:       map[string]string{"org.opencontainers.image.version": "1.0", "idunn.enable": "true"},
		Volumes:      map[string]struct{}{"/data": {}},
		ExposedPorts: map[string]struct{}{"80/tcp": {}},
	}}
	p80, _ := network.ParsePort("80/tcp")
	p443, _ := network.ParsePort("443/tcp")
	id := strings.Repeat("a", 64)
	c := New(container.InspectResponse{
		ID: id, Name: "/web",
		Config: &container.Config{
			Hostname:     id[:12],
			Image:        "nginx:1",
			Env:          []string{"PATH=/usr/bin", "APP_VERSION=1", "TZ=UTC"},
			Cmd:          []string{"serve"},
			Entrypoint:   []string{"/app"},
			WorkingDir:   "/srv",
			Volumes:      map[string]struct{}{"/data": {}},
			ExposedPorts: network.PortSet{p80: {}, p443: {}},
			Labels: map[string]string{
				"org.opencontainers.image.version": "1.0",
				"idunn.enable":                     "true",
				"com.docker.compose.project":       "site",
				"com.docker.compose.image":         "sha256:old",
				LabelReplaces:                      "someone",
			},
		},
		HostConfig: &container.HostConfig{NetworkMode: "site_default", Binds: []string{"/srv/conf:/etc/app:ro"}},
		Mounts: []container.MountPoint{
			{Type: mount.TypeVolume, Name: strings.Repeat("b", 64), Destination: "/data", RW: true},
			{Type: mount.TypeVolume, Name: "named", Destination: "/named", RW: true},
			{Type: mount.TypeBind, Source: "/srv/conf", Destination: "/etc/app"},
		},
		NetworkSettings: &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{
			"site_default": {Aliases: []string{"web", id[:12]}, NetworkID: "n1"},
			"backend":      {Aliases: []string{"web"}},
		}},
	})
	s, err := newSpec(c, specOptions{imageConfig: img, newImageID: "sha256:new", keep: []string{"idunn.enable"}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.config
	if cfg.Hostname != "" {
		t.Errorf("hostname = %q, the old short ID must not be reused", cfg.Hostname)
	}
	if !slices.Equal(cfg.Env, []string{"TZ=UTC"}) {
		t.Errorf("env = %v", cfg.Env)
	}
	if cfg.Cmd != nil || cfg.Entrypoint != nil || cfg.WorkingDir != "" || len(cfg.Volumes) != 0 {
		t.Errorf("image defaults kept: cmd %v entrypoint %v workdir %q volumes %v", cfg.Cmd, cfg.Entrypoint, cfg.WorkingDir, cfg.Volumes)
	}
	if _, ok := cfg.ExposedPorts[p80]; ok || len(cfg.ExposedPorts) != 1 {
		t.Errorf("exposed ports = %v, want 443 only", cfg.ExposedPorts)
	}
	want := map[string]string{"idunn.enable": "true", "com.docker.compose.project": "site", "com.docker.compose.image": "sha256:new"}
	if fmt.Sprint(cfg.Labels) != fmt.Sprint(want) {
		t.Errorf("labels = %v, want %v", cfg.Labels, want)
	}
	if cfg.Image != "nginx:1" {
		t.Errorf("image = %q, the reference must stay as written", cfg.Image)
	}
	if len(s.host.Mounts) != 1 || s.host.Mounts[0].Source != strings.Repeat("b", 64) || s.host.Mounts[0].Target != "/data" {
		t.Errorf("mounts = %+v, want the anonymous volume reused", s.host.Mounts)
	}
	if !slices.Equal(s.host.Binds, []string{"/srv/conf:/etc/app:ro"}) {
		t.Errorf("binds = %v", s.host.Binds)
	}
	ep := s.network.EndpointsConfig["site_default"]
	if ep == nil || !slices.Equal(ep.Aliases, []string{"web"}) || ep.NetworkID != "" {
		t.Errorf("primary endpoint = %+v", ep)
	}
	if _, ok := s.extra["backend"]; !ok || len(s.extra) != 1 {
		t.Errorf("extra networks = %v", s.extra)
	}
}

func TestNewSpecContainerNetworkMode(t *testing.T) {
	oldVPN, newVPN := strings.Repeat("1", 64), strings.Repeat("2", 64)
	p80, _ := network.ParsePort("80/tcp")
	c := New(container.InspectResponse{
		ID: strings.Repeat("c", 64), Name: "/app",
		Config:     &container.Config{Image: "app:1", Hostname: "vpn-host", ExposedPorts: network.PortSet{p80: {}}},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode("container:" + oldVPN), VolumesFrom: []string{oldVPN[:12] + ":ro"}},
	})
	s, err := newSpec(c, specOptions{replaced: map[string]string{oldVPN: newVPN}})
	if err != nil {
		t.Fatal(err)
	}
	if s.host.NetworkMode != container.NetworkMode("container:"+newVPN) {
		t.Errorf("network mode = %s", s.host.NetworkMode)
	}
	if !slices.Equal(s.host.VolumesFrom, []string{newVPN + ":ro"}) {
		t.Errorf("volumes from = %v", s.host.VolumesFrom)
	}
	if s.config.Hostname != "" || s.config.ExposedPorts != nil || s.network != nil {
		t.Errorf("hostname %q, ports %v, network %v: none allowed with a container network", s.config.Hostname, s.config.ExposedPorts, s.network)
	}
}

func TestSetOrderAndDependencies(t *testing.T) {
	f := newFakeEngine()
	f.image("sha256:i1", "img:1", ocispec.ImageConfig{})
	vpn := f.run("vpn", "img:1", nil, nil)
	f.run("torrent", "img:1", nil, &container.HostConfig{NetworkMode: container.NetworkMode("container:" + vpn)})
	f.run("db", "img:1", &container.Config{Labels: map[string]string{
		"com.docker.compose.project": "site", "com.docker.compose.service": "db"}}, nil)
	f.run("web", "img:1", &container.Config{Labels: map[string]string{
		"com.docker.compose.project": "site", "com.docker.compose.service": "web",
		"com.docker.compose.depends_on": "db:service_started:false"}}, nil)
	f.run("worker", "img:1", &container.Config{Labels: map[string]string{LabelDependsOn: "web, ghost"}},
		&container.HostConfig{Links: []string{"/db:/worker/db"}})

	all, err := List(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	names := func(cs []*Container) string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Name)
		}
		return strings.Join(out, " ")
	}

	s := NewSet(all, false)
	if got := strings.Join(s.DependsOn("torrent"), ","); got != "vpn" {
		t.Errorf("torrent depends on %q", got)
	}
	if got := strings.Join(s.DependsOn("worker"), ","); got != "web,db" {
		t.Errorf("worker depends on %q (unknown containers are dropped)", got)
	}
	if got := s.DependsOn("web"); len(got) != 0 {
		t.Errorf("compose depends_on must be ignored unless asked: %v", got)
	}
	if got := names(s.Order(all)); got != "db vpn torrent web worker" {
		t.Errorf("order = %s", got)
	}

	s = NewSet(all, true)
	if got := strings.Join(s.DependsOn("web"), ","); got != "db" {
		t.Errorf("web depends on %q with compose depends_on", got)
	}
	if got := strings.Join(s.Dependents("db"), ","); got != "web,worker" {
		t.Errorf("dependents of db = %q", got)
	}
}

func TestRecreateUpdates(t *testing.T) {
	f := newFakeEngine()
	f.image("sha256:i1", "app:1", ocispec.ImageConfig{})
	oldID := f.run("app", "app:1", &container.Config{Env: []string{"A=1"}}, nil)
	f.image("sha256:i2", "app:1", ocispec.ImageConfig{}) // the tag now points to a new image
	old := inspect(t, f, oldID)

	res := Recreate(context.Background(), f, old, Options{Pull: "app:1", Settle: 10 * time.Millisecond, Poll: time.Millisecond, Rollback: true}, quiet)
	if res.Outcome != Updated {
		t.Fatalf("outcome = %v: %s", res.Outcome, res.Message)
	}
	if !slices.Equal(f.pulls, []string{"app:1"}) {
		t.Errorf("pulls = %v", f.pulls)
	}
	c := f.byName("app")
	if c == nil || c.ID != res.NewID || c.Image != "sha256:i2" || !c.State.Running {
		t.Fatalf("new container = %+v", c)
	}
	if !slices.Equal(c.Config.Env, []string{"A=1"}) {
		t.Errorf("env = %v", c.Config.Env)
	}
	if f.byName("app"+OldSuffix) != nil || f.byName(oldID) != nil {
		t.Error("old container not removed")
	}
}

func TestRecreateRollsBack(t *testing.T) {
	f := newFakeEngine()
	f.image("sha256:i1", "app:1", ocispec.ImageConfig{})
	oldID := f.run("app", "app:1", nil, nil)
	f.image("sha256:i2", "app:1", ocispec.ImageConfig{})
	f.broken["sha256:i2"] = true

	res := Recreate(context.Background(), f, inspect(t, f, oldID), Options{Settle: time.Second, Poll: time.Millisecond, Rollback: true}, quiet)
	if res.Outcome != RolledBack || !strings.Contains(res.Message, "exited") {
		t.Fatalf("outcome = %v: %s", res.Outcome, res.Message)
	}
	c := f.byName("app")
	if c == nil || c.ID != oldID || !c.State.Running {
		t.Fatalf("old container not running under its name: %+v", c)
	}
	if len(f.containers) != 1 {
		t.Errorf("%d containers left, want the old one only", len(f.containers))
	}
}

func TestRecreateWithoutRollbackKeepsBoth(t *testing.T) {
	f := newFakeEngine()
	f.image("sha256:i1", "app:1", ocispec.ImageConfig{})
	oldID := f.run("app", "app:1", nil, nil)
	f.image("sha256:i2", "app:1", ocispec.ImageConfig{})
	f.broken["sha256:i2"] = true

	res := Recreate(context.Background(), f, inspect(t, f, oldID), Options{Settle: time.Second, Poll: time.Millisecond}, quiet)
	if res.Outcome != Kept {
		t.Fatalf("outcome = %v", res.Outcome)
	}
	if old := f.byName("app" + OldSuffix); old == nil || old.ID != oldID || old.State.Running {
		t.Errorf("old container = %+v, want it stopped and renamed", old)
	}
}

func TestHandOver(t *testing.T) {
	f := newFakeEngine()
	f.image("sha256:i1", "idunn:latest", ocispec.ImageConfig{})
	selfID := f.run("idunn", "idunn:latest", nil, nil)
	f.image("sha256:i2", "idunn:latest", ocispec.ImageConfig{})
	self := inspect(t, f, selfID)

	res := Recreate(context.Background(), f, self, Options{Pull: "idunn:latest", HandOver: true, Rollback: true,
		Labels: map[string]string{LabelReplaces: selfID}, Settle: 10 * time.Millisecond, Poll: time.Millisecond}, quiet)
	if res.Outcome != Updated {
		t.Fatalf("outcome = %v: %s", res.Outcome, res.Message)
	}
	old := f.byName("idunn" + OldSuffix)
	if old == nil || !old.State.Running {
		t.Fatal("the old instance must keep running until it stops itself")
	}
	next := inspect(t, f, "idunn")
	if next.Labels()[LabelReplaces] != selfID {
		t.Fatalf("new instance labels = %v", next.Labels())
	}

	// An old instance restarted meanwhile sees it is replaced.
	sup, err := Superseded(context.Background(), f, inspect(t, f, selfID), quiet)
	if err != nil || !sup {
		t.Fatalf("superseded = %v, %v", sup, err)
	}

	// The new instance waits for the old one to stop, then removes it.
	done := make(chan error, 1)
	go func() { done <- AwaitPredecessor(context.Background(), f, next, time.Millisecond, quiet) }()
	select {
	case err := <-done:
		t.Fatalf("returned before the old instance stopped: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	f.ContainerStop(context.Background(), selfID, client.ContainerStopOptions{})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if f.byName(selfID) != nil {
		t.Error("old instance not removed")
	}
}

func TestSupersededUndoesInterruptedHandOver(t *testing.T) {
	f := newFakeEngine()
	f.image("sha256:i1", "idunn:latest", ocispec.ImageConfig{})
	selfID := f.run("idunn"+OldSuffix, "idunn:latest", nil, nil)
	nextID := f.run("idunn", "idunn:latest", &container.Config{Labels: map[string]string{LabelReplaces: selfID}}, nil)
	f.ContainerStop(context.Background(), nextID, client.ContainerStopOptions{})

	self := inspect(t, f, selfID)
	sup, err := Superseded(context.Background(), f, self, quiet)
	if err != nil || sup {
		t.Fatalf("superseded = %v, %v", sup, err)
	}
	if self.Name != "idunn" || f.byName("idunn").ID != selfID || f.byName(nextID) != nil {
		t.Error("the old instance must take its name back and the stopped replacement go")
	}
}
