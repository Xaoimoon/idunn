package updater

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/xaoimoon/idunn/internal/notify"
	"github.com/xaoimoon/idunn/internal/registry"
	"github.com/xaoimoon/idunn/internal/versions"
)

// orderSink records, for each notification, how many service updates had
// already been requested.
type orderSink struct {
	docker *fakeDocker
	sent   [][]notify.Event
	before []int
}

func (s *orderSink) Send(_ context.Context, events []notify.Event) {
	if len(events) == 0 {
		return
	}
	s.docker.mu.Lock()
	s.before = append(s.before, len(s.docker.updates))
	s.docker.mu.Unlock()
	s.sent = append(s.sent, events)
}

func TestSelfUpdateComesLastAfterTheReport(t *testing.T) {
	h := newHarness(t)
	h.cfg.SelfUpdate = true
	h.u.Self.ServiceID = "self"
	sink := &orderSink{docker: h.docker}
	h.u.Notifier = sink
	h.docker.add("self", "idunn_idunn", "idunn:1@"+d1, map[string]string{"idunn.enable": "false"})
	h.docker.add("s1", "web", "nginx:1@"+d1, nil)
	h.checker.digests["idunn:1"] = d2
	h.checker.digests["nginx:1"] = d2

	h.pass()

	if len(h.docker.updates) != 2 {
		t.Fatalf("%d updates, want web then Iðunn", len(h.docker.updates))
	}
	if got := h.docker.imageOf("self"); got != "idunn:1@"+d2 {
		t.Errorf("own image = %s", got)
	}
	if h.docker.updates[1].Spec.Name != "idunn_idunn" {
		t.Errorf("last update is %s, want Iðunn itself", h.docker.updates[1].Spec.Name)
	}
	if len(sink.sent) != 1 || sink.before[0] != 1 {
		t.Fatalf("report sent after %v updates, want before the self-update", sink.before)
	}
	var kinds []notify.Kind
	for _, e := range sink.sent[0] {
		kinds = append(kinds, e.Kind)
	}
	if len(kinds) != 2 || kinds[0] != notify.KindUpdated || kinds[1] != notify.KindSelfUpdate {
		t.Errorf("events = %v", kinds)
	}
	if len(h.cleaned) != 1 || h.cleaned[0][0] != "index.docker.io/library/nginx" {
		t.Errorf("cleanup = %v, Iðunn's repository must not be cleaned before it is replaced", h.cleaned)
	}
}

func TestSelfUpdateOffOrMonitorOnly(t *testing.T) {
	for _, tc := range []struct {
		name        string
		selfUpdate  bool
		monitorOnly bool
		want        []notify.Kind
	}{
		{"disabled", false, false, nil},
		{"monitor-only", true, true, []notify.Kind{notify.KindDetected}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.SelfUpdate = tc.selfUpdate
			h.cfg.MonitorOnly = tc.monitorOnly
			h.u.Self.ServiceID = "self"
			h.docker.add("self", "idunn", "idunn:1@"+d1, nil)
			h.checker.digests["idunn:1"] = d2

			h.pass()

			if len(h.docker.updates) != 0 {
				t.Errorf("%d updates, want none", len(h.docker.updates))
			}
			if k := h.sink.kinds(); len(k) != len(tc.want) || (len(k) == 1 && k[0] != tc.want[0]) {
				t.Errorf("events = %v, want %v", k, tc.want)
			}
		})
	}
}

func TestSelfUpdateRefusedIsNotified(t *testing.T) {
	h := newHarness(t)
	h.cfg.SelfUpdate = true
	h.u.Self.ServiceID = "self"
	h.docker.add("self", "idunn", "idunn:1@"+d1, nil)
	h.checker.digests["idunn:1"] = d2
	h.docker.services["self"].Version.Index = 7 // changed between inspect and update
	h.u.Docker = &staleVersion{h.docker}

	h.pass()

	k := h.sink.kinds()
	if len(k) != 2 || k[0] != notify.KindSelfUpdate || k[1] != notify.KindSelfFailed {
		t.Errorf("events = %v", k)
	}
}

// staleVersion returns services with an outdated version index, so that the
// following ServiceUpdate is refused.
type staleVersion struct{ *fakeDocker }

func (s *staleVersion) ServiceInspect(ctx context.Context, id string, o client.ServiceInspectOptions) (client.ServiceInspectResult, error) {
	res, err := s.fakeDocker.ServiceInspect(ctx, id, o)
	res.Service.Version.Index--
	return res, err
}

func TestRolledBackDigestIsNotRetried(t *testing.T) {
	h := newHarness(t)
	h.docker.add("s1", "web", "nginx:1@"+d1, nil)
	h.checker.digests["nginx:1"] = d2
	h.docker.onUpdate = func(svc *swarm.Service, opts client.ServiceUpdateOptions) {
		if opts.Rollback == "previous" {
			setStatus(svc, swarm.UpdateStateRollbackCompleted)
		} else {
			setStatus(svc, swarm.UpdateStatePaused)
		}
	}

	h.pass() // update, paused, rolled back
	h.pass() // same digest: skipped and notified
	h.pass() // still the same: silent

	if len(h.docker.updates) != 2 {
		t.Fatalf("%d calls, want one update and one rollback", len(h.docker.updates))
	}
	k := h.sink.kinds()
	if len(k) != 2 || k[0] != notify.KindRollback || k[1] != notify.KindRetrySkipped {
		t.Fatalf("events = %v", k)
	}

	h.docker.onUpdate = func(svc *swarm.Service, _ client.ServiceUpdateOptions) {
		setStatus(svc, swarm.UpdateStateCompleted)
	}
	h.checker.digests["nginx:1"] = d3
	h.pass()
	if got := h.docker.imageOf("s1"); got != "nginx:1@"+d3 {
		t.Errorf("a newer image must be applied, image = %s", got)
	}
}

// slowLister times out on one registry.
type slowLister struct{ fakeLister }

func (l *slowLister) Tags(ctx context.Context, image string) ([]string, error) {
	if strings.HasPrefix(image, "slow.example/") {
		l.mu.Lock()
		l.calls[image]++
		l.mu.Unlock()
		return nil, &registry.HeadError{Registry: "slow.example", Err: context.DeadlineExceeded}
	}
	return l.fakeLister.Tags(ctx, image)
}

func TestVersionCheckSkipsSlowRegistry(t *testing.T) {
	h := newHarness(t)
	h.cfg.VersionLevel = versions.Patch
	lister := &slowLister{fakeLister{calls: map[string]int{}, tags: map[string][]string{"mariadb": {"12.3.3", "13.0.0"}}}}
	h.u.Lister = lister
	h.docker.add("a", "a", "slow.example/beats/filebeat:9.5.5", nil)
	h.docker.add("b", "b", "slow.example/logstash/logstash:9.5.5", nil)
	h.docker.add("c", "c", "mariadb:12.3.3", nil)

	h.u.VersionCheck(context.Background())

	slow := 0
	for img, n := range lister.calls {
		if img != "mariadb" {
			slow += n
		}
	}
	if slow != 1 {
		t.Errorf("slow registry listed %d times, want once", slow)
	}
	if k := h.sink.kinds(); len(k) != 1 || k[0] != notify.KindNewVersion {
		t.Errorf("events = %v, the other registries must still be checked", k)
	}
	if !errors.Is(&registry.HeadError{Err: context.DeadlineExceeded}, context.DeadlineExceeded) {
		t.Error("HeadError must unwrap to the timeout")
	}
}
