// Package updater runs Iðunn's passes: select services, detect new digests,
// update through the Swarm API, follow the update and roll back on failure.
package updater

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/xaoimoon/idunn/internal/cluster"
	"github.com/xaoimoon/idunn/internal/config"
	"github.com/xaoimoon/idunn/internal/notify"
	"github.com/xaoimoon/idunn/internal/redact"
	"github.com/xaoimoon/idunn/internal/registry"
	"github.com/xaoimoon/idunn/internal/schedule"
)

// DigestChecker returns the digest a registry serves for an image tag.
type DigestChecker interface {
	Digest(ctx context.Context, image string) (string, error)
}

// AuthEncoder returns the X-Registry-Auth header for an image, "" if none.
type AuthEncoder interface {
	EncodedAuth(image string) (string, error)
}

// Notifier sends the events of a pass.
type Notifier interface {
	Send(ctx context.Context, events []notify.Event)
}

// Cleaner removes old images of the given repositories on every node and
// returns the failures as events.
type Cleaner func(ctx context.Context, repos []string) []notify.Event

// Updater holds the dependencies and the in-memory state kept between passes.
type Updater struct {
	Docker   cluster.Docker
	Checker  DigestChecker
	Lister   TagLister // used by VersionCheck only
	Auth     AuthEncoder
	Notifier Notifier
	Cleaner  Cleaner
	Config   *config.Config
	Log      *slog.Logger
	NodeID   string
	Self     cluster.Self

	// Now and PollInterval are overridable in tests.
	Now          func() time.Time
	PollInterval time.Duration

	passMu    sync.Mutex
	versionMu sync.Mutex

	mu               sync.Mutex
	firstSeen        map[string]seen   // image -> newest digest and when it was first seen
	notifiedPending  map[string]string // service ID -> digest already notified as pending
	notifiedDetected map[string]string // service ID -> digest already notified in monitor-only
	notifiedHead     map[string]bool   // image -> HEAD failure already notified
	guardNotified    bool
}

type seen struct {
	digest string
	at     time.Time
}

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Updater) pollInterval() time.Duration {
	if u.PollInterval > 0 {
		return u.PollInterval
	}
	return 5 * time.Second
}

func (u *Updater) init() {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.firstSeen == nil {
		u.firstSeen = map[string]seen{}
		u.notifiedPending = map[string]string{}
		u.notifiedDetected = map[string]string{}
		u.notifiedHead = map[string]bool{}
	}
}

// Tracked lists the services selected by the label filter, minus the ignored
// ones and Iðunn itself, sorted by name.
func (u *Updater) Tracked(ctx context.Context) ([]swarm.Service, error) {
	res, err := u.Docker.ServiceList(ctx, client.ServiceListOptions{
		Filters: make(client.Filters).Add("label", u.Config.LabelFilter.String()),
	})
	if err != nil {
		return nil, err
	}
	var out []swarm.Service
	for _, s := range res.Items {
		if s.ID == u.Self.ServiceID || matches(s.Spec.Labels, u.Config.LabelIgnore) {
			continue
		}
		if !matches(s.Spec.Labels, u.Config.LabelFilter) {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
	return out, nil
}

// pass holds what one pass shares between services.
type pass struct {
	mu          sync.Mutex
	digests     map[string]digestResult // per image, checked once per pass
	rateLimited map[string]bool         // registries that answered 429
	events      []notify.Event
	updatedRepo map[string]bool
	checked     map[string]bool
	leaderLost  bool
}

type digestResult struct {
	digest string
	err    error
	ready  chan struct{}
}

func (p *pass) add(e notify.Event) {
	p.mu.Lock()
	p.events = append(p.events, e)
	p.mu.Unlock()
}

// Pass runs one pass. It returns early, doing nothing, when this instance is
// not the leader, during a blackout window, or when a node is unavailable.
func (u *Updater) Pass(ctx context.Context) {
	if !u.passMu.TryLock() {
		u.Log.Warn("previous pass still running, skipping this one")
		return
	}
	defer u.passMu.Unlock()
	u.init()

	leader, err := cluster.IsLeader(ctx, u.Docker, u.NodeID)
	if err != nil {
		u.Log.Error("cannot check Raft leadership", "error", err)
		return
	}
	if !leader {
		u.Log.Debug("not the Raft leader, standing by")
		return
	}

	now := u.now()
	if w, in := schedule.InBlackout(u.Config.Blackout, now); in {
		u.Log.Info("inside blackout window, pass skipped", "window", w.String())
		return
	}

	if u.Config.ClusterGuard {
		if events, ok := u.guard(ctx); !ok {
			u.Notifier.Send(ctx, events)
			return
		}
	}

	services, err := u.Tracked(ctx)
	if err != nil {
		u.Log.Error("cannot list services", "error", err)
		return
	}
	u.Log.Info("pass started", "services", len(services))

	p := &pass{
		digests:     map[string]digestResult{},
		rateLimited: map[string]bool{},
		updatedRepo: map[string]bool{},
		checked:     map[string]bool{},
	}

	sem := make(chan struct{}, u.Config.MaxConcurrent)
	var wg sync.WaitGroup
	for _, svc := range services {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		// Leadership is checked again before each service, once a slot is
		// free, so that a former leader stops as soon as another manager
		// takes over.
		if leader, err := cluster.IsLeader(ctx, u.Docker, u.NodeID); err != nil || !leader {
			u.Log.Warn("leadership lost or unknown, pass stopped", "error", err)
			p.leaderLost = true
			<-sem
			break
		}
		wg.Add(1)
		go func(svc swarm.Service) {
			defer wg.Done()
			defer func() { <-sem }()
			u.service(ctx, p, svc)
		}(svc)
	}
	wg.Wait()

	u.forgetUnchecked(p)

	if len(p.updatedRepo) > 0 && u.Config.Cleanup && u.Cleaner != nil && !p.leaderLost {
		repos := make([]string, 0, len(p.updatedRepo))
		for r := range p.updatedRepo {
			repos = append(repos, r)
		}
		sort.Strings(repos)
		for _, e := range u.Cleaner(ctx, repos) {
			p.add(e)
		}
	}

	u.Log.Info("pass finished", "events", len(p.events))
	u.Notifier.Send(ctx, p.events)
}

// guard checks node availability. It returns false, with an event the first
// time, while a node is not Ready and Active.
func (u *Updater) guard(ctx context.Context) ([]notify.Event, bool) {
	nodes, err := cluster.Nodes(ctx, u.Docker)
	if err != nil {
		u.Log.Error("cannot list nodes, pass skipped", "error", err)
		return nil, false
	}
	down := cluster.Unavailable(nodes)
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(down) == 0 {
		if u.guardNotified {
			u.Log.Info("all nodes available again")
		}
		u.guardNotified = false
		return nil, true
	}
	names := make([]string, len(down))
	for i, n := range down {
		names[i] = n.String()
	}
	u.Log.Warn("node unavailable, pass postponed", "nodes", strings.Join(names, ", "))
	if u.guardNotified {
		return nil, false
	}
	u.guardNotified = true
	return []notify.Event{{Kind: notify.KindGuard, Level: notify.Warn, Time: u.now(), Nodes: names}}, false
}

// digest returns the registry digest of image, querying the registry once per
// pass even when several services use the same image concurrently.
func (u *Updater) digest(ctx context.Context, p *pass, image string) (string, error) {
	reg := registry.RegistryOf(image)
	p.mu.Lock()
	p.checked[image] = true
	if r, ok := p.digests[image]; ok {
		p.mu.Unlock()
		<-r.ready
		p.mu.Lock()
		r = p.digests[image]
		p.mu.Unlock()
		return r.digest, r.err
	}
	if p.rateLimited[reg] {
		p.mu.Unlock()
		return "", &registry.HeadError{Registry: reg, StatusCode: 429, Err: errors.New("rate limited earlier in this pass")}
	}
	ready := make(chan struct{})
	p.digests[image] = digestResult{ready: ready}
	p.mu.Unlock()

	d, err := u.Checker.Digest(ctx, image)

	p.mu.Lock()
	p.digests[image] = digestResult{digest: d, err: err, ready: ready}
	var he *registry.HeadError
	if errors.As(err, &he) && he.RateLimited() {
		p.rateLimited[reg] = true
	}
	p.mu.Unlock()
	close(ready)
	return d, err
}

// service handles one service; every outcome is logged, notable ones become
// events.
func (u *Updater) service(ctx context.Context, p *pass, svc swarm.Service) {
	t := newTarget(svc, u.Config)
	log := u.Log.With("service", t.Name)
	if t.Skip != "" {
		log.Info("ignored", "reason", t.Skip)
		return
	}
	log = log.With("image", t.Image)

	latest, err := u.digest(ctx, p, t.Image)
	if err != nil {
		u.headFailed(p, log, t, err)
		return
	}
	u.headRecovered(t.Image)

	if latest == t.Current {
		u.forgetService(t.ID)
		log.Debug("up to date", "digest", notify.ShortDigest(latest))
		return
	}

	now := u.now()
	first := u.observe(t.Image, latest, now)
	if t.MinAge > 0 && now.Sub(first) < t.MinAge {
		applyAt := first.Add(t.MinAge)
		log.Info("new image waiting for maturity", "digest", notify.ShortDigest(latest), "apply_after", applyAt.Format(time.RFC3339))
		if u.markOnce(u.notifiedPending, t.ID, latest) {
			p.add(notify.Event{Kind: notify.KindPending, Level: notify.Info, Time: now, Service: t.Name,
				Image: t.Image, NewDigest: latest, ApplyAt: applyAt})
		}
		return
	}

	if u.Config.DryRun {
		log.Info("dry run: would update", "from", notify.ShortDigest(t.Current), "to", notify.ShortDigest(latest))
		return
	}
	if t.MonitorOnly {
		log.Info("new image detected (monitor only)", "digest", notify.ShortDigest(latest))
		if u.markOnce(u.notifiedDetected, t.ID, latest) {
			p.add(notify.Event{Kind: notify.KindDetected, Level: notify.Info, Time: now, Service: t.Name,
				Image: t.Image, NewDigest: latest})
		}
		return
	}

	if e, ok := u.update(ctx, log, svc, t, latest); ok {
		p.add(e)
		if e.Kind == notify.KindUpdated {
			p.mu.Lock()
			p.updatedRepo[repository(t.Image)] = true
			p.mu.Unlock()
			u.forgetService(t.ID)
		}
	}
}

func (u *Updater) headFailed(p *pass, log *slog.Logger, t target, err error) {
	var he *registry.HeadError
	if !errors.As(err, &he) {
		log.Warn("cannot check image", "error", redact.Error(err))
		return
	}
	log.Warn("cannot check image", "registry", he.Registry, "status", he.StatusCode, "error", redact.Error(err))
	notifyIt := false
	switch u.Config.HeadFailureWarn {
	case config.HeadWarnAlways:
		notifyIt = true
	case config.HeadWarnAuto:
		notifyIt = registry.HeadReliable(he.Registry)
	}
	if !notifyIt {
		return
	}
	// Notified once until the image can be checked again, not every hour.
	u.mu.Lock()
	already := u.notifiedHead[t.Image]
	u.notifiedHead[t.Image] = true
	u.mu.Unlock()
	if !already {
		p.add(notify.Event{Kind: notify.KindHeadFailed, Level: notify.Warn, Time: u.now(), Service: t.Name,
			Image: t.Image, Registry: he.Registry, StatusCode: he.StatusCode})
	}
}

func (u *Updater) headRecovered(image string) {
	u.mu.Lock()
	delete(u.notifiedHead, image)
	u.mu.Unlock()
}

// observe records when a digest was first seen for an image and returns that
// time. A different digest restarts the count.
func (u *Updater) observe(image, digest string, now time.Time) time.Time {
	u.mu.Lock()
	defer u.mu.Unlock()
	s, ok := u.firstSeen[image]
	if !ok || s.digest != digest {
		s = seen{digest: digest, at: now}
		u.firstSeen[image] = s
	}
	return s.at
}

// markOnce records digest for a service and reports whether it is new.
func (u *Updater) markOnce(m map[string]string, serviceID, digest string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if m[serviceID] == digest {
		return false
	}
	m[serviceID] = digest
	return true
}

func (u *Updater) forgetService(serviceID string) {
	u.mu.Lock()
	delete(u.notifiedPending, serviceID)
	delete(u.notifiedDetected, serviceID)
	u.mu.Unlock()
}

// forgetUnchecked drops the maturity state of images no service uses anymore.
func (u *Updater) forgetUnchecked(p *pass) {
	if p.leaderLost {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	for image := range u.firstSeen {
		if !p.checked[image] {
			delete(u.firstSeen, image)
		}
	}
}

// update pins the new digest in the service spec, follows the Swarm update
// and rolls back if needed. It returns the event describing the outcome.
func (u *Updater) update(ctx context.Context, log *slog.Logger, svc swarm.Service, t target, digest string) (notify.Event, bool) {
	ev := notify.Event{Time: u.now(), Service: t.Name, Image: t.Image, OldDigest: t.Current, NewDigest: digest}

	auth, err := u.Auth.EncodedAuth(t.Image)
	if err != nil {
		log.Error("cannot build registry auth", "error", redact.Error(err))
		return ev, false
	}
	spec := svc.Spec
	cs := *spec.TaskTemplate.ContainerSpec
	cs.Image = t.Image + "@" + digest
	spec.TaskTemplate.ContainerSpec = &cs

	log.Info("updating", "from", notify.ShortDigest(t.Current), "to", notify.ShortDigest(digest))
	start := u.now()
	res, err := u.Docker.ServiceUpdate(ctx, t.ID, client.ServiceUpdateOptions{
		Version:             svc.Version,
		Spec:                spec,
		EncodedRegistryAuth: auth,
	})
	if err != nil {
		// Nothing changed (e.g. the spec was modified meanwhile): the next
		// pass tries again.
		log.Error("service update refused, retrying next pass", "error", redact.Error(err))
		return ev, false
	}
	for _, w := range res.Warnings {
		log.Warn("swarm warning", "warning", w)
	}

	state, msg := u.wait(ctx, t.ID, start, t.Timeout, false)
	ev.Duration = u.now().Sub(start)
	ev.State = string(state)
	switch state {
	case swarm.UpdateStateCompleted:
		log.Info("updated", "digest", notify.ShortDigest(digest), "duration", ev.Duration.Round(time.Second))
		ev.Kind, ev.Level = notify.KindUpdated, notify.Info
		return ev, true
	case swarm.UpdateStateRollbackCompleted:
		// The service's own update_config.failure_action rolled it back.
		ev.Kind, ev.Level, ev.Message = notify.KindRollback, notify.Warn, u.taskError(ctx, t.ID, msg)
		log.Warn("rolled back by Swarm", "message", ev.Message)
		return ev, true
	case swarm.UpdateStateRollbackPaused:
		ev.Kind, ev.Level, ev.Message = notify.KindRollbackFailed, notify.Error, u.taskError(ctx, t.ID, msg)
		log.Error("rollback paused", "message", ev.Message)
		return ev, true
	}

	// Paused, or timed out while updating.
	reason := u.taskError(ctx, t.ID, msg)
	if !t.Rollback {
		ev.Kind, ev.Level, ev.Message = notify.KindPaused, notify.Error, reason
		log.Error("update not completed, rollback disabled", "state", state, "message", reason)
		return ev, true
	}
	log.Warn("update not completed, rolling back", "state", state, "message", reason)
	rbState, rbMsg := u.rollback(ctx, log, t)
	ev.State = string(rbState)
	if rbState == swarm.UpdateStateRollbackCompleted {
		ev.Kind, ev.Level, ev.Message = notify.KindRollback, notify.Warn, reason
		log.Warn("rolled back")
		return ev, true
	}
	if rbMsg != "" {
		reason = rbMsg
	}
	ev.Kind, ev.Level, ev.Message = notify.KindRollbackFailed, notify.Error, reason
	log.Error("rollback not completed", "state", rbState, "message", reason)
	return ev, true
}

func (u *Updater) rollback(ctx context.Context, log *slog.Logger, t target) (swarm.UpdateState, string) {
	res, err := u.Docker.ServiceInspect(ctx, t.ID, client.ServiceInspectOptions{})
	if err != nil {
		return "", "cannot inspect service: " + err.Error()
	}
	start := u.now()
	if _, err := u.Docker.ServiceUpdate(ctx, t.ID, client.ServiceUpdateOptions{
		Version:  res.Service.Version,
		Spec:     res.Service.Spec,
		Rollback: "previous",
	}); err != nil {
		log.Error("rollback refused", "error", err)
		return "", "rollback refused: " + redact.String(err.Error())
	}
	return u.wait(ctx, t.ID, start, t.Timeout, true)
}

// wait polls UpdateStatus until a final state or the timeout. Statuses that
// started before our request belong to an older update and are ignored.
func (u *Updater) wait(ctx context.Context, serviceID string, since time.Time, timeout time.Duration, rollingBack bool) (swarm.UpdateState, string) {
	deadline := u.now().Add(timeout)
	tick := time.NewTicker(u.pollInterval())
	defer tick.Stop()
	var last *swarm.UpdateStatus
	for {
		res, err := u.Docker.ServiceInspect(ctx, serviceID, client.ServiceInspectOptions{})
		if err == nil {
			us := res.Service.UpdateStatus
			if us != nil && (us.StartedAt == nil || !us.StartedAt.Before(since.Add(-2*time.Second))) {
				last = us
				switch us.State {
				case swarm.UpdateStateCompleted, swarm.UpdateStateRollbackCompleted, swarm.UpdateStateRollbackPaused:
					return us.State, us.Message
				case swarm.UpdateStatePaused:
					if !rollingBack {
						return us.State, us.Message
					}
				}
			}
		}
		if !u.now().Before(deadline) {
			if last == nil {
				return "timeout", "no update status reported"
			}
			return last.State, fmt.Sprintf("timed out after %s (%s)", timeout, last.State)
		}
		select {
		case <-ctx.Done():
			return "canceled", ctx.Err().Error()
		case <-tick.C:
		}
	}
}

// taskError returns the most recent task error of a service, or fallback.
func (u *Updater) taskError(ctx context.Context, serviceID, fallback string) string {
	res, err := u.Docker.TaskList(ctx, client.TaskListOptions{
		Filters: make(client.Filters).Add("service", serviceID),
	})
	if err != nil {
		return fallback
	}
	var best swarm.Task
	for _, t := range res.Items {
		if t.Status.Err != "" && t.Status.Timestamp.After(best.Status.Timestamp) {
			best = t
		}
	}
	if best.Status.Err != "" {
		return best.Status.Err
	}
	return fallback
}
