//go:build integration

// Package integration runs Iðunn against a real, disposable Swarm. Start it
// with scripts/integration.sh, never against a cluster you care about.
package integration

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/xaoimoon/idunn/internal/cleanup"
	"github.com/xaoimoon/idunn/internal/cluster"
	"github.com/xaoimoon/idunn/internal/config"
	"github.com/xaoimoon/idunn/internal/notify"
	"github.com/xaoimoon/idunn/internal/registry"
	"github.com/xaoimoon/idunn/internal/updater"
)

type sink struct {
	mu     sync.Mutex
	events []notify.Event
}

func (s *sink) Send(_ context.Context, events []notify.Event) {
	s.mu.Lock()
	s.events = append(s.events, events...)
	s.mu.Unlock()
}

func (s *sink) forService(name string) []notify.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []notify.Event
	for _, e := range s.events {
		if e.Service == name {
			out = append(out, e)
		}
	}
	return out
}

type env struct {
	t        *testing.T
	ctx      context.Context
	docker   *client.Client
	registry string
	nodeID   string
}

func setup(t *testing.T) *env {
	t.Helper()
	if os.Getenv("IDUNN_IT_DISPOSABLE") != "1" {
		t.Skip("run through scripts/integration.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	d, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	nodeID, err := cluster.LocalNodeID(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	nodes, _ := cluster.Nodes(ctx, d)
	if len(nodes) != 1 {
		t.Fatalf("refusing to run on a %d-node Swarm", len(nodes))
	}
	return &env{t: t, ctx: ctx, docker: d, registry: os.Getenv("IDUNN_IT_REGISTRY"), nodeID: nodeID}
}

func (e *env) ref(tag string) string { return e.registry + "/it/app:" + tag }

// retag points dst at the image src, like pushing a new build under a tag.
func (e *env) retag(src, dst string) string {
	e.t.Helper()
	s, err := name.ParseReference(e.ref(src), name.Insecure)
	if err != nil {
		e.t.Fatal(err)
	}
	desc, err := remote.Get(s)
	if err != nil {
		e.t.Fatal(err)
	}
	d, _ := name.NewTag(e.ref(dst), name.Insecure)
	if err := remote.Tag(d, desc); err != nil {
		e.t.Fatal(err)
	}
	return desc.Digest.String()
}

// create deploys a service the way docker stack deploy does: tag@digest in
// the spec, the tag as written in the com.docker.stack.image label.
func (e *env) create(svcName, tag, digest string, labels map[string]string) string {
	e.t.Helper()
	all := map[string]string{"idunn.enable": "true", "com.docker.stack.image": e.ref(tag)}
	for k, v := range labels {
		all[k] = v
	}
	replicas := uint64(1)
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{Name: svcName, Labels: all},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{Image: e.ref(tag) + "@" + digest},
		},
		Mode: swarm.ServiceMode{Replicated: &swarm.ReplicatedService{Replicas: &replicas}},
		UpdateConfig: &swarm.UpdateConfig{
			Parallelism:   1,
			Monitor:       15 * time.Second,
			FailureAction: swarm.UpdateFailureActionPause,
			Order:         swarm.UpdateOrderStopFirst,
		},
	}
	res, err := e.docker.ServiceCreate(e.ctx, client.ServiceCreateOptions{Spec: spec})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { e.docker.ServiceRemove(context.Background(), res.ID, client.ServiceRemoveOptions{}) })
	e.waitRunning(res.ID)
	return res.ID
}

func (e *env) waitRunning(id string) {
	e.t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		tasks, err := e.docker.TaskList(e.ctx, client.TaskListOptions{Filters: make(client.Filters).Add("service", id).Add("desired-state", "running")})
		if err == nil {
			for _, t := range tasks.Items {
				if t.Status.State == swarm.TaskStateRunning {
					return
				}
			}
		}
		time.Sleep(time.Second)
	}
	e.t.Fatalf("service %s never ran", id)
}

func (e *env) image(id string) string {
	e.t.Helper()
	res, err := e.docker.ServiceInspect(e.ctx, id, client.ServiceInspectOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	return res.Service.Spec.TaskTemplate.ContainerSpec.Image
}

func (e *env) version(id string) uint64 {
	res, _ := e.docker.ServiceInspect(e.ctx, id, client.ServiceInspectOptions{})
	return res.Service.Version.Index
}

func (e *env) updater(mutate func(*config.Config)) (*updater.Updater, *sink) {
	e.t.Helper()
	cfg := &config.Config{
		ClusterGuard:      true,
		LabelFilter:       config.Label{Key: "idunn.enable", Value: "true"},
		LabelIgnore:       config.Label{Key: "idunn.enable", Value: "false"},
		RollbackOnFailure: true,
		UpdateTimeout:     3 * time.Minute,
		MaxConcurrent:     1,
		HeadFailureWarn:   config.HeadWarnAlways,
	}
	if mutate != nil {
		mutate(cfg)
	}
	creds, _ := registry.LoadCredentials("")
	checker, err := registry.NewChecker(creds, "idunn-it", true)
	if err != nil {
		e.t.Fatal(err)
	}
	s := &sink{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if testing.Verbose() {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return &updater.Updater{
		Docker: e.docker, Checker: checker, Auth: creds, Notifier: s,
		Config: cfg, Log: log, NodeID: e.nodeID, PollInterval: time.Second,
	}, s
}

func TestUpdateAndCleanup(t *testing.T) {
	e := setup(t)
	d1 := e.retag("good1", "update")
	id := e.create("it_update", "update", d1, nil)
	d2 := e.retag("good2", "update")

	u, events := e.updater(nil)
	u.Pass(e.ctx)

	if got := e.image(id); got != e.ref("update")+"@"+d2 {
		t.Fatalf("image = %s, want new digest %s", got, d2)
	}
	ev := events.forService("it_update")
	if len(ev) != 1 || ev[0].Kind != notify.KindUpdated {
		t.Fatalf("events = %+v", ev)
	}

	// Second pass: nothing to do, nothing notified.
	before := e.version(id)
	u.Pass(e.ctx)
	if e.version(id) != before || len(events.forService("it_update")) != 1 {
		t.Error("a pass without a new image must not change anything")
	}

	// The old task's stopped container still references the old image, so
	// cleanup keeps it.
	e.waitRunning(id)
	repo := normalize(strings.TrimSuffix(e.ref("x"), ":x"))
	if err := cleanup.Local(e.ctx, e.docker, []string{repo}, 0, u.Log); err != nil {
		t.Fatal(err)
	}
	if !e.hasDigest(d1) {
		t.Fatal("an image referenced by a stopped container must not be forced out")
	}

	// Once stopped containers are pruned (as a nightly prune does), cleanup
	// with keep 0 removes it.
	if _, err := e.docker.ContainerPrune(e.ctx, client.ContainerPruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := cleanup.Local(e.ctx, e.docker, []string{repo}, 0, u.Log); err != nil {
		t.Fatal(err)
	}
	if e.hasDigest(d1) {
		t.Error("old image still present after cleanup")
	}
	if !e.hasDigest(d2) {
		t.Error("the image in service must never be removed")
	}
}

func (e *env) hasDigest(digest string) bool {
	imgs, err := e.docker.ImageList(e.ctx, client.ImageListOptions{})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, img := range imgs.Items {
		for _, rd := range img.RepoDigests {
			if strings.HasSuffix(rd, digest) {
				return true
			}
		}
	}
	return false
}

func normalize(repo string) string {
	r, err := name.NewRepository(repo, name.Insecure)
	if err != nil {
		return repo
	}
	return r.Name()
}

func TestFailedUpdateIsRolledBack(t *testing.T) {
	e := setup(t)
	d1 := e.retag("good1", "flaky")
	id := e.create("it_flaky", "flaky", d1, nil)
	e.retag("unhealthy", "flaky")

	u, events := e.updater(nil)
	u.Pass(e.ctx)

	if got := e.image(id); got != e.ref("flaky")+"@"+d1 {
		t.Fatalf("image = %s, want rollback to %s", got, d1)
	}
	ev := events.forService("it_flaky")
	if len(ev) != 1 || ev[0].Kind != notify.KindRollback || ev[0].Level != notify.Warn {
		t.Fatalf("events = %+v", ev)
	}
	t.Logf("rollback reason: %s", ev[0].Message)
}

func TestPinnedMonitorOnlyAndDryRun(t *testing.T) {
	e := setup(t)
	d1 := e.retag("good1", "modes")
	pinned := e.create("it_pinned", "modes", d1, map[string]string{"com.docker.stack.image": e.ref("modes") + "@" + d1})
	monitored := e.create("it_monitored", "modes", d1, map[string]string{"idunn.monitor-only": "true"})
	e.retag("good2", "modes")

	versions := map[string]uint64{pinned: e.version(pinned), monitored: e.version(monitored)}

	u, events := e.updater(nil)
	u.Pass(e.ctx)
	dry, dryEvents := e.updater(func(c *config.Config) { c.DryRun = true })
	dry.Pass(e.ctx)

	for id, v := range versions {
		if e.version(id) != v {
			t.Errorf("service %s was modified", id)
		}
	}
	if ev := events.forService("it_pinned"); len(ev) != 0 {
		t.Errorf("pinned: events = %+v", ev)
	}
	if ev := events.forService("it_monitored"); len(ev) != 1 || ev[0].Kind != notify.KindDetected {
		t.Errorf("monitor-only: events = %+v", ev)
	}
	if n := len(dryEvents.events); n != 0 {
		t.Errorf("dry-run: %d events, want none", n)
	}
}
