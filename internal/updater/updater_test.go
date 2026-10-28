package updater

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/xaoimoon/idunn/internal/cluster"
	"github.com/xaoimoon/idunn/internal/config"
	"github.com/xaoimoon/idunn/internal/notify"
	"github.com/xaoimoon/idunn/internal/registry"
	"github.com/xaoimoon/idunn/internal/schedule"
)

const (
	d1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	d2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	d3 = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

// fakeDocker is an in-memory Swarm. onUpdate decides the UpdateStatus a
// service reaches after a ServiceUpdate.
type fakeDocker struct {
	mu       sync.Mutex
	leader   bool
	nodes    []swarm.Node
	services map[string]*swarm.Service
	updates  []client.ServiceUpdateOptions
	onUpdate func(svc *swarm.Service, opts client.ServiceUpdateOptions)
	tasks    []swarm.Task
}

func newFake() *fakeDocker {
	return &fakeDocker{
		leader: true,
		nodes: []swarm.Node{
			node("n1", "alpha", swarm.NodeStateReady, swarm.NodeAvailabilityActive),
			node("n2", "beta", swarm.NodeStateReady, swarm.NodeAvailabilityActive),
		},
		services: map[string]*swarm.Service{},
		onUpdate: func(svc *swarm.Service, _ client.ServiceUpdateOptions) {
			setStatus(svc, swarm.UpdateStateCompleted)
		},
	}
}

func node(id, name string, st swarm.NodeState, av swarm.NodeAvailability) swarm.Node {
	n := swarm.Node{ID: id}
	n.Description.Hostname = name
	n.Status.State = st
	n.Spec.Availability = av
	return n
}

func setStatus(svc *swarm.Service, st swarm.UpdateState) {
	now := time.Now()
	svc.UpdateStatus = &swarm.UpdateStatus{State: st, StartedAt: &now}
}

func (f *fakeDocker) add(id, name, image string, labels map[string]string) {
	if labels == nil {
		labels = map[string]string{}
	}
	if _, ok := labels["idunn.enable"]; !ok {
		labels["idunn.enable"] = "true"
	}
	svc := &swarm.Service{ID: id}
	svc.Version.Index = 1
	svc.Spec.Name = name
	svc.Spec.Labels = labels
	svc.Spec.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{Image: image}
	f.services[id] = svc
}

func (f *fakeDocker) Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error) {
	return client.SystemInfoResult{}, nil
}

func (f *fakeDocker) NodeInspect(_ context.Context, id string, _ client.NodeInspectOptions) (client.NodeInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return client.NodeInspectResult{Node: swarm.Node{ID: id, ManagerStatus: &swarm.ManagerStatus{Leader: f.leader}}}, nil
}

func (f *fakeDocker) NodeList(context.Context, client.NodeListOptions) (client.NodeListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return client.NodeListResult{Items: append([]swarm.Node{}, f.nodes...)}, nil
}

func (f *fakeDocker) ServiceList(_ context.Context, opts client.ServiceListOptions) (client.ServiceListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []swarm.Service
	for _, s := range f.services {
		ok := true
		for l := range opts.Filters["label"] {
			k, v, _ := cut(l)
			if s.Spec.Labels[k] != v {
				ok = false
			}
		}
		if ok {
			out = append(out, *s)
		}
	}
	return client.ServiceListResult{Items: out}, nil
}

func cut(s string) (string, string, bool) {
	for i := range s {
		if s[i] == '=' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func (f *fakeDocker) ServiceInspect(_ context.Context, id string, _ client.ServiceInspectOptions) (client.ServiceInspectResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.services[id]
	if !ok {
		return client.ServiceInspectResult{}, errors.New("no such service")
	}
	return client.ServiceInspectResult{Service: *s}, nil
}

func (f *fakeDocker) ServiceUpdate(_ context.Context, id string, opts client.ServiceUpdateOptions) (client.ServiceUpdateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.services[id]
	if opts.Version.Index != s.Version.Index {
		return client.ServiceUpdateResult{}, errors.New("update out of sequence")
	}
	f.updates = append(f.updates, opts)
	s.Version.Index++
	if opts.Rollback == "" {
		s.PreviousSpec = &swarm.ServiceSpec{}
		*s.PreviousSpec = s.Spec
		s.Spec = opts.Spec
	} else if s.PreviousSpec != nil {
		// Like Swarm: the rolled-back spec becomes the previous one.
		s.Spec, *s.PreviousSpec = *s.PreviousSpec, s.Spec
	}
	f.onUpdate(s, opts)
	return client.ServiceUpdateResult{}, nil
}

func (f *fakeDocker) ServiceCreate(context.Context, client.ServiceCreateOptions) (client.ServiceCreateResult, error) {
	return client.ServiceCreateResult{}, errors.New("not used")
}

func (f *fakeDocker) ServiceRemove(context.Context, string, client.ServiceRemoveOptions) (client.ServiceRemoveResult, error) {
	return client.ServiceRemoveResult{}, nil
}

func (f *fakeDocker) TaskList(context.Context, client.TaskListOptions) (client.TaskListResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return client.TaskListResult{Items: f.tasks}, nil
}

func (f *fakeDocker) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return client.ContainerInspectResult{}, errors.New("not used")
}

func (f *fakeDocker) imageOf(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.services[id].Spec.TaskTemplate.ContainerSpec.Image
}

// fakeChecker serves digests per image and counts queries.
type fakeChecker struct {
	mu      sync.Mutex
	digests map[string]string
	errs    map[string]error
	calls   map[string]int
}

func (c *fakeChecker) Digest(_ context.Context, image string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls[image]++
	if err := c.errs[image]; err != nil {
		return "", err
	}
	return c.digests[image], nil
}

type noAuth struct{}

func (noAuth) EncodedAuth(string) (string, error) { return "", nil }

type sink struct {
	mu     sync.Mutex
	passes [][]notify.Event
}

func (s *sink) Send(_ context.Context, events []notify.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(events) > 0 {
		s.passes = append(s.passes, events)
	}
}

func (s *sink) all() []notify.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []notify.Event
	for _, p := range s.passes {
		out = append(out, p...)
	}
	return out
}

func (s *sink) kinds() []notify.Kind {
	var out []notify.Kind
	for _, e := range s.all() {
		out = append(out, e.Kind)
	}
	return out
}

type harness struct {
	docker  *fakeDocker
	checker *fakeChecker
	sink    *sink
	cfg     *config.Config
	u       *Updater
	clock   time.Time
	cleaned [][]string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		docker:  newFake(),
		checker: &fakeChecker{digests: map[string]string{}, errs: map[string]error{}, calls: map[string]int{}},
		sink:    &sink{},
		clock:   time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC),
		cfg: &config.Config{
			ClusterGuard:      true,
			LabelFilter:       config.Label{Key: "idunn.enable", Value: "true"},
			LabelIgnore:       config.Label{Key: "idunn.enable", Value: "false"},
			RollbackOnFailure: true,
			UpdateTimeout:     time.Minute,
			MaxConcurrent:     1,
			Cleanup:           true,
			CleanupKeep:       1,
			HeadFailureWarn:   config.HeadWarnAuto,
		},
	}
	h.u = &Updater{
		Docker:       h.docker,
		Checker:      h.checker,
		Auth:         noAuth{},
		Notifier:     h.sink,
		Config:       h.cfg,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		NodeID:       "n1",
		Now:          func() time.Time { return h.clock },
		PollInterval: time.Millisecond,
		Cleaner: func(_ context.Context, repos []string) []notify.Event {
			h.cleaned = append(h.cleaned, repos)
			return nil
		},
	}
	return h
}

func (h *harness) pass() { h.u.Pass(context.Background()) }

func TestUpdatesToNewDigest(t *testing.T) {
	h := newHarness(t)
	h.docker.add("s1", "web_app", "nginx:1.27@"+d1, map[string]string{labelStackImage: "nginx:1.27"})
	h.checker.digests["nginx:1.27"] = d2

	h.pass()

	if got := h.docker.imageOf("s1"); got != "nginx:1.27@"+d2 {
		t.Fatalf("image = %s", got)
	}
	k := h.sink.kinds()
	if len(k) != 1 || k[0] != notify.KindUpdated {
		t.Fatalf("events = %v", k)
	}
	if len(h.cleaned) != 1 || h.cleaned[0][0] != "index.docker.io/library/nginx" {
		t.Errorf("cleanup = %v", h.cleaned)
	}
}

func TestNoChangeIsSilent(t *testing.T) {
	h := newHarness(t)
	h.docker.add("s1", "web_app", "nginx:1.27@"+d1, nil)
	h.checker.digests["nginx:1.27"] = d1

	h.pass()

	if len(h.docker.updates) != 0 || len(h.sink.all()) != 0 || len(h.cleaned) != 0 {
		t.Errorf("updates=%d events=%v cleaned=%v", len(h.docker.updates), h.sink.kinds(), h.cleaned)
	}
}

func TestPinnedAndIgnoredServices(t *testing.T) {
	h := newHarness(t)
	h.docker.add("pinned", "pinned", "nginx:1@"+d1, map[string]string{labelStackImage: "nginx:1@" + d1})
	h.docker.add("ignored", "ignored", "nginx:1@"+d1, map[string]string{"idunn.enable": "false"})
	h.docker.add("self", "idunn", "idunn:1@"+d1, nil)
	h.docker.add("other", "other", "nginx:1@"+d1, map[string]string{"idunn.enable": "nope"})
	h.u.Self.ServiceID = "self"
	h.checker.digests["nginx:1"] = d2
	h.checker.digests["idunn:1"] = d2

	h.pass()

	if len(h.docker.updates) != 0 {
		t.Errorf("%d updates, want none", len(h.docker.updates))
	}
	if n := len(h.checker.calls); n != 0 {
		t.Errorf("registry queried for %v", h.checker.calls)
	}
}

func TestTrackedList(t *testing.T) {
	h := newHarness(t)
	h.cfg.LabelFilter = config.Label{Key: "autodeploy", Value: "true"}
	h.cfg.LabelIgnore = config.Label{Key: "autodeploy", Value: "false"}
	h.docker.add("b", "b_svc", "x:1", map[string]string{"autodeploy": "true", "idunn.enable": "-"})
	h.docker.add("a", "a_svc", "x:1", map[string]string{"autodeploy": "true", "idunn.enable": "-"})
	h.docker.add("c", "c_svc", "x:1", map[string]string{"autodeploy": "false", "idunn.enable": "-"})
	h.docker.add("d", "d_svc", "x:1", map[string]string{"idunn.enable": "-"})

	got, err := h.u.Tracked(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Spec.Name != "a_svc" || got[1].Spec.Name != "b_svc" {
		t.Errorf("tracked = %v", got)
	}
}

func TestHeadOncePerImagePerPass(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxConcurrent = 3
	for _, id := range []string{"s1", "s2", "s3", "s4"} {
		h.docker.add(id, id, "redis:7@"+d1, nil)
	}
	h.checker.digests["redis:7"] = d1

	h.pass()
	h.pass()

	if n := h.checker.calls["redis:7"]; n != 2 {
		t.Errorf("HEAD count = %d, want 1 per pass (2)", n)
	}
}

func TestMaturity(t *testing.T) {
	h := newHarness(t)
	h.cfg.MinAge = 6 * time.Hour
	h.docker.add("s1", "app", "app:1@"+d1, nil)
	h.checker.digests["app:1"] = d2

	h.pass() // first seen at 12:00
	h.clock = h.clock.Add(time.Hour)
	h.pass()
	if len(h.docker.updates) != 0 {
		t.Fatal("updated before maturity")
	}
	if k := h.sink.kinds(); len(k) != 1 || k[0] != notify.KindPending {
		t.Fatalf("pending must be notified once: %v", k)
	}

	// A newer digest restarts the count.
	h.clock = h.clock.Add(4 * time.Hour) // 17:00
	h.checker.digests["app:1"] = d3
	h.pass()
	h.clock = h.clock.Add(2 * time.Hour) // 19:00, 7 h after d2, 2 h after d3
	h.pass()
	if len(h.docker.updates) != 0 {
		t.Fatal("count must restart when the digest changes")
	}
	if n := len(h.sink.kinds()); n != 2 {
		t.Errorf("events = %v, want one pending per digest", h.sink.kinds())
	}

	h.clock = h.clock.Add(4 * time.Hour) // 23:00, 6 h after d3
	h.pass()
	if got := h.docker.imageOf("s1"); got != "app:1@"+d3 {
		t.Fatalf("image = %s", got)
	}
}

func TestMinAgeLabelOverride(t *testing.T) {
	h := newHarness(t)
	h.cfg.MinAge = 6 * time.Hour
	h.docker.add("s1", "app", "app:1@"+d1, map[string]string{labelMinAge: "0"})
	h.checker.digests["app:1"] = d2
	h.pass()
	if len(h.docker.updates) != 1 {
		t.Error("idunn.min-age=0 must apply immediately")
	}
}

func TestMonitorOnlyAndDryRun(t *testing.T) {
	for _, mode := range []string{"monitor", "dry"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			if mode == "monitor" {
				h.cfg.MonitorOnly = true
			} else {
				h.cfg.DryRun = true
			}
			h.docker.add("s1", "app", "app:1@"+d1, nil)
			h.checker.digests["app:1"] = d2
			h.pass()
			h.pass()
			if len(h.docker.updates) != 0 {
				t.Fatal("no ServiceUpdate allowed")
			}
			k := h.sink.kinds()
			if mode == "monitor" && (len(k) != 1 || k[0] != notify.KindDetected) {
				t.Errorf("monitor-only must notify once: %v", k)
			}
			if mode == "dry" && len(k) != 0 {
				t.Errorf("dry-run only logs: %v", k)
			}
		})
	}
}

func TestMonitorOnlyLabelOverridesGlobal(t *testing.T) {
	h := newHarness(t)
	h.cfg.MonitorOnly = true
	h.docker.add("pilot", "pilot", "app:1@"+d1, map[string]string{labelMonitorOnly: "false"})
	h.docker.add("other", "other", "app:1@"+d1, nil)
	h.checker.digests["app:1"] = d2
	h.pass()
	if len(h.docker.updates) != 1 || h.docker.imageOf("pilot") != "app:1@"+d2 || h.docker.imageOf("other") != "app:1@"+d1 {
		t.Errorf("only the pilot service must be updated (%d updates)", len(h.docker.updates))
	}
}

func TestPausedUpdateIsRolledBack(t *testing.T) {
	h := newHarness(t)
	h.docker.add("s1", "app", "app:1@"+d1, nil)
	h.checker.digests["app:1"] = d2
	h.docker.tasks = []swarm.Task{{Status: swarm.TaskStatus{Err: "task: non-zero exit (1)", Timestamp: time.Now()}}}
	h.docker.onUpdate = func(svc *swarm.Service, opts client.ServiceUpdateOptions) {
		if opts.Rollback == "previous" {
			svc.Spec = *svc.PreviousSpec
			setStatus(svc, swarm.UpdateStateRollbackCompleted)
			return
		}
		setStatus(svc, swarm.UpdateStatePaused)
	}

	h.pass()

	if len(h.docker.updates) != 2 || h.docker.updates[1].Rollback != "previous" {
		t.Fatalf("expected update then rollback, got %d calls", len(h.docker.updates))
	}
	ev := h.sink.all()
	if len(ev) != 1 || ev[0].Kind != notify.KindRollback || ev[0].Level != notify.Warn || ev[0].Message != "task: non-zero exit (1)" {
		t.Fatalf("events = %+v", ev)
	}
	if len(h.cleaned) != 0 {
		t.Error("no cleanup after a rollback")
	}
}

func TestSwarmRollbackAndRollbackDisabled(t *testing.T) {
	h := newHarness(t)
	h.docker.add("auto", "auto", "a:1@"+d1, nil)
	h.docker.add("manual", "manual", "m:1@"+d1, map[string]string{labelRollback: "false"})
	h.checker.digests["a:1"] = d2
	h.checker.digests["m:1"] = d2
	h.docker.onUpdate = func(svc *swarm.Service, _ client.ServiceUpdateOptions) {
		if svc.ID == "auto" {
			setStatus(svc, swarm.UpdateStateRollbackCompleted) // failure_action: rollback
		} else {
			setStatus(svc, swarm.UpdateStatePaused)
		}
	}

	h.pass()

	if len(h.docker.updates) != 2 {
		t.Fatalf("%d calls, want no extra rollback request", len(h.docker.updates))
	}
	got := map[string]notify.Event{}
	for _, e := range h.sink.all() {
		got[e.Service] = e
	}
	if got["auto"].Kind != notify.KindRollback || got["manual"].Kind != notify.KindPaused || got["manual"].Level != notify.Error {
		t.Errorf("events = %+v", got)
	}
}

func TestTimeoutTriggersRollback(t *testing.T) {
	h := newHarness(t)
	h.docker.add("s1", "app", "app:1@"+d1, nil)
	h.checker.digests["app:1"] = d2
	h.docker.onUpdate = func(svc *swarm.Service, opts client.ServiceUpdateOptions) {
		if opts.Rollback == "previous" {
			setStatus(svc, swarm.UpdateStateRollbackCompleted)
			return
		}
		setStatus(svc, swarm.UpdateStateUpdating)
	}
	// Each poll moves the clock forward by 20 s; the timeout is 1 min.
	base := h.clock
	var mu sync.Mutex
	n := 0
	h.u.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		n++
		return base.Add(time.Duration(n) * 20 * time.Second)
	}

	h.pass()

	if len(h.docker.updates) != 2 || h.docker.updates[1].Rollback != "previous" {
		t.Fatalf("expected a rollback after the timeout, got %d calls", len(h.docker.updates))
	}
	if k := h.sink.kinds(); len(k) != 1 || k[0] != notify.KindRollback {
		t.Errorf("events = %v", k)
	}
}

func TestUpdateInProgressIsSkipped(t *testing.T) {
	h := newHarness(t)
	h.docker.add("s1", "app", "app:1@"+d1, nil)
	setStatus(h.docker.services["s1"], swarm.UpdateStateUpdating)
	h.checker.digests["app:1"] = d2
	h.pass()
	if len(h.docker.updates) != 0 {
		t.Error("service with an update in progress must be skipped")
	}
}

func TestNotLeaderDoesNothing(t *testing.T) {
	h := newHarness(t)
	h.docker.leader = false
	h.docker.add("s1", "app", "app:1@"+d1, nil)
	h.checker.digests["app:1"] = d2
	h.pass()
	if len(h.checker.calls) != 0 || len(h.docker.updates) != 0 {
		t.Error("a follower must not check nor update")
	}
}

func TestBlackout(t *testing.T) {
	h := newHarness(t)
	h.cfg.Blackout, _ = schedule.ParseWindows("11:00-13:00")
	h.docker.add("s1", "app", "app:1@"+d1, nil)
	h.checker.digests["app:1"] = d2
	h.pass()
	if len(h.checker.calls) != 0 || len(h.docker.updates) != 0 {
		t.Error("nothing may happen inside a blackout window")
	}
}

func TestClusterGuard(t *testing.T) {
	h := newHarness(t)
	h.docker.add("s1", "app", "app:1@"+d1, nil)
	h.checker.digests["app:1"] = d2
	h.docker.nodes[1] = node("n2", "beta", swarm.NodeStateReady, swarm.NodeAvailabilityDrain)

	h.pass()
	h.pass()
	if len(h.docker.updates) != 0 {
		t.Fatal("no update while a node is drained")
	}
	ev := h.sink.all()
	if len(ev) != 1 || ev[0].Kind != notify.KindGuard || ev[0].Nodes[0] != "beta (ready/drain)" {
		t.Fatalf("guard must be notified once: %+v", ev)
	}

	h.docker.nodes[1] = node("n2", "beta", swarm.NodeStateDown, swarm.NodeAvailabilityActive)
	h.pass()
	if len(h.sink.all()) != 1 {
		t.Error("still one notification while the situation lasts")
	}

	h.docker.nodes[1] = node("n2", "beta", swarm.NodeStateReady, swarm.NodeAvailabilityActive)
	h.pass()
	if len(h.docker.updates) != 1 {
		t.Error("update expected once every node is back")
	}
}

func TestHeadFailures(t *testing.T) {
	h := newHarness(t)
	h.docker.add("hub", "hub", "gone/app:1@"+d1, nil)
	h.docker.add("priv", "priv", "registry.example.com/app:1@"+d1, nil)
	h.checker.errs["gone/app:1"] = &registry.HeadError{Registry: "index.docker.io", StatusCode: 404}
	h.checker.errs["registry.example.com/app:1"] = &registry.HeadError{Registry: "registry.example.com", StatusCode: 405}

	h.pass()
	h.pass()

	ev := h.sink.all()
	if len(ev) != 1 || ev[0].Service != "hub" || ev[0].StatusCode != 404 {
		t.Fatalf("auto mode: one notification for Docker Hub only, got %+v", ev)
	}

	// Recovery re-arms the notification.
	delete(h.checker.errs, "gone/app:1")
	h.checker.digests["gone/app:1"] = d1
	h.pass()
	h.checker.errs["gone/app:1"] = &registry.HeadError{Registry: "index.docker.io", StatusCode: 404}
	h.pass()
	if n := len(h.sink.all()); n != 2 {
		t.Errorf("events = %d, want 2", n)
	}
}

func TestRateLimitStopsRegistryForThePass(t *testing.T) {
	h := newHarness(t)
	h.docker.add("a", "a", "one:1@"+d1, nil)
	h.docker.add("b", "b", "two:1@"+d1, nil)
	h.checker.errs["one:1"] = &registry.HeadError{Registry: "index.docker.io", StatusCode: 429}
	h.checker.digests["two:1"] = d2
	h.pass()
	if h.checker.calls["two:1"] != 0 {
		t.Error("no more HEAD to a registry after a 429 in the same pass")
	}
}

func TestLeadershipLostMidPass(t *testing.T) {
	h := newHarness(t)
	h.docker.add("a", "a", "app:1@"+d1, nil)
	h.docker.add("b", "b", "app:1@"+d1, nil)
	h.checker.digests["app:1"] = d2
	h.docker.onUpdate = func(svc *swarm.Service, _ client.ServiceUpdateOptions) {
		setStatus(svc, swarm.UpdateStateCompleted)
		h.docker.leader = false // called with the lock held
	}
	h.pass()
	if len(h.docker.updates) != 1 {
		t.Errorf("%d updates, want 1 before leadership was lost", len(h.docker.updates))
	}
	if len(h.cleaned) != 0 {
		t.Error("no cleanup once leadership is lost")
	}
}

var _ cluster.Docker = (*fakeDocker)(nil)
