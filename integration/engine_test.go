//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/xaoimoon/idunn/internal/config"
	"github.com/xaoimoon/idunn/internal/engine"
	"github.com/xaoimoon/idunn/internal/notify"
	"github.com/xaoimoon/idunn/internal/updater"
)

// engineUpdater is an updater in engine mode on the disposable engine.
func (e *env) engineUpdater(mutate func(*config.Config)) (*updater.Updater, *sink) {
	u, s := e.updater(mutate)
	u.Engine = e.docker
	u.Settle = 3 * time.Second
	return u, s
}

// pull pulls ref, and removes the local reference at the end of the test so
// that the images of other tests can be cleaned up.
func (e *env) pull(ref string) {
	e.t.Helper()
	if err := engine.Pull(e.ctx, e.docker, ref, ""); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { e.docker.ImageRemove(context.Background(), ref, client.ImageRemoveOptions{}) })
}

// run pulls ref and starts a standalone container from it.
func (e *env) run(name, tag string, cfg *container.Config, hc *container.HostConfig) string {
	e.t.Helper()
	e.pull(e.ref(tag))
	if cfg == nil {
		cfg = &container.Config{}
	}
	cfg.Image = e.ref(tag)
	if cfg.Labels == nil {
		cfg.Labels = map[string]string{}
	}
	if _, ok := cfg.Labels["idunn.enable"]; !ok {
		cfg.Labels["idunn.enable"] = "true"
	}
	res, err := e.docker.ContainerCreate(e.ctx, client.ContainerCreateOptions{Name: name, Config: cfg, HostConfig: hc})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() {
		e.docker.ContainerRemove(context.Background(), name, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	})
	if _, err := e.docker.ContainerStart(e.ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		e.t.Fatal(err)
	}
	return res.ID
}

func (e *env) container(name string) *engine.Container {
	e.t.Helper()
	c, err := engine.Inspect(e.ctx, e.docker, name)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func TestEngineUpdateRollbackAndRetry(t *testing.T) {
	e := setup(t)
	e.retag("good1", "engine")
	firstID := e.run("it-engine", "engine", &container.Config{
		Env:     []string{"KEEP=me"},
		Volumes: map[string]struct{}{"/data": {}},
	}, nil)
	before := e.container(firstID)
	var volume string
	for _, m := range before.Info.Mounts {
		if m.Destination == "/data" {
			volume = m.Name
		}
	}
	e.retag("good2", "engine")

	u, events := e.engineUpdater(nil)
	u.Pass(e.ctx)

	c := e.container("it-engine")
	if c.ID == firstID || !c.Running() {
		t.Fatalf("container not replaced or not running: %s running=%v", c.ID, c.Running())
	}
	if got := c.Labels()["idunn.test"]; got != "good2" {
		t.Errorf("label idunn.test = %q: a label inherited from the old image must give way to the new image's", got)
	}
	if !contains(c.Info.Config.Env, "KEEP=me") {
		t.Errorf("env = %v", c.Info.Config.Env)
	}
	reused := false
	for _, m := range c.Info.Mounts {
		reused = reused || (m.Destination == "/data" && m.Name == volume)
	}
	if !reused {
		t.Errorf("anonymous volume %s not reused: %+v", volume, c.Info.Mounts)
	}
	if ev := events.forService("it-engine"); len(ev) != 1 || ev[0].Kind != notify.KindUpdated {
		t.Fatalf("events = %+v", ev)
	}

	// A broken image: the old container comes back.
	updatedID := c.ID
	e.retag("unhealthy", "engine")
	u.Pass(e.ctx)
	c = e.container("it-engine")
	if c.ID != updatedID || !c.Running() {
		t.Fatalf("rollback: container %s running=%v, want %s back", c.ID, c.Running(), updatedID)
	}
	ev := events.forService("it-engine")
	if len(ev) != 2 || ev[1].Kind != notify.KindRollback || !strings.Contains(ev[1].Message, "unhealthy") {
		t.Fatalf("events = %+v", ev)
	}
	t.Logf("rollback reason: %s", ev[1].Message)
	if _, err := engine.Inspect(e.ctx, e.docker, "it-engine"+engine.OldSuffix); err == nil {
		t.Error("the failed container must be removed")
	}

	// Next pass: the same digest is not tried again.
	u.Pass(e.ctx)
	if e.container("it-engine").ID != updatedID {
		t.Error("the rolled-back digest was retried")
	}
	if ev := events.forService("it-engine"); len(ev) != 3 || ev[2].Kind != notify.KindRetrySkipped {
		t.Errorf("events = %+v", ev)
	}
}

// The tag was pulled by someone else (docker compose pull, a redeploy)
// before Iðunn's pass: the container still runs the old image, which has
// lost its tag and, with the containerd store, its repo digests.
func TestEngineTagAlreadyPulled(t *testing.T) {
	e := setup(t)
	e.retag("good1", "moved")
	oldID := e.run("it-moved", "moved", nil, nil)
	e.retag("good2", "moved")
	e.pull(e.ref("moved"))

	u, events := e.engineUpdater(nil)
	u.Pass(e.ctx)

	c := e.container("it-moved")
	if c.ID == oldID || c.Labels()["idunn.test"] != "good2" {
		t.Fatalf("container %s (label %q) not moved to the image of its tag", c.ID, c.Labels()["idunn.test"])
	}
	if ev := events.forService("it-moved"); len(ev) != 1 || ev[0].Kind != notify.KindUpdated {
		t.Fatalf("events = %+v", ev)
	}
}

func TestEngineDependentFollowsItsNetwork(t *testing.T) {
	e := setup(t)
	e.retag("good1", "netns")
	e.retag("good1", "netns-app")
	vpnID := e.run("it-vpn", "netns", nil, nil)
	appID := e.run("it-app", "netns-app", nil, &container.HostConfig{NetworkMode: container.NetworkMode("container:" + vpnID)})
	e.retag("good2", "netns")

	u, events := e.engineUpdater(nil)
	u.Pass(e.ctx)

	vpn, app := e.container("it-vpn"), e.container("it-app")
	if vpn.ID == vpnID || !vpn.Running() {
		t.Fatalf("vpn not updated: %s", vpn.ID)
	}
	if app.ID == appID || !app.Running() {
		t.Fatalf("app not recreated: %s running=%v", app.ID, app.Running())
	}
	if got := app.Info.HostConfig.NetworkMode.ConnectedContainer(); got != vpn.ID {
		t.Errorf("app network = container:%s, want the new vpn %s", got, vpn.ID)
	}
	if ev := events.forService("it-app"); len(ev) != 0 {
		t.Errorf("a dependent recreated without error is not notified: %+v", ev)
	}
}

func TestEngineIgnoresPinned(t *testing.T) {
	e := setup(t)
	d1 := e.retag("good1", "engine-modes")
	ref := e.ref("engine-modes") + "@" + d1
	e.pull(ref)
	res, err := e.docker.ContainerCreate(e.ctx, client.ContainerCreateOptions{Name: "it-engine-pinned",
		Config: &container.Config{Image: ref, Labels: map[string]string{"idunn.enable": "true"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		e.docker.ContainerRemove(context.Background(), res.ID, client.ContainerRemoveOptions{Force: true})
	})
	if _, err := e.docker.ContainerStart(e.ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	e.retag("good2", "engine-modes")

	u, events := e.engineUpdater(nil)
	u.Pass(e.ctx)

	if e.container("it-engine-pinned").ID != res.ID {
		t.Error("a container pinned by digest must not be updated")
	}
	if n := len(events.events); n != 0 {
		t.Errorf("%d events, want none: %+v", n, events.events)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
