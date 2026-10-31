package updater

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/moby/moby/client"

	"github.com/xaoimoon/idunn/internal/engine"
	"github.com/xaoimoon/idunn/internal/notify"
	"github.com/xaoimoon/idunn/internal/redact"
	"github.com/xaoimoon/idunn/internal/schedule"
)

// defaultSettle is how long a container without healthcheck must keep
// running after an update, in engine mode.
const defaultSettle = 60 * time.Second

func (u *Updater) settle() time.Duration {
	if u.Settle > 0 {
		return u.Settle
	}
	return defaultSettle
}

// TrackedNames lists the names of the tracked services, or containers in
// engine mode.
func (u *Updater) TrackedNames(ctx context.Context) ([]string, error) {
	if u.Engine == nil {
		services, err := u.Tracked(ctx)
		names := make([]string, len(services))
		for i, s := range services {
			names[i] = s.Spec.Name
		}
		return names, err
	}
	all, err := engine.List(ctx, u.Engine)
	if err != nil {
		return nil, err
	}
	cs := u.trackedContainers(all)
	names := make([]string, len(cs))
	for i, c := range cs {
		names[i] = c.Name
	}
	return names, nil
}

// trackedContainers selects, among all, the containers matching the label
// filter and not ignored. Containers managed by Swarm, Iðunn itself and
// containers being replaced are left out.
func (u *Updater) trackedContainers(all []*engine.Container) []*engine.Container {
	var out []*engine.Container
	for _, c := range all {
		labels := c.Labels()
		switch {
		case c.SwarmTask(), c.ID == u.Self.ContainerID, strings.HasSuffix(c.Name, engine.OldSuffix):
		case matches(labels, u.Config.LabelIgnore):
		case matches(labels, u.Config.LabelFilter):
			out = append(out, c)
		}
	}
	return out
}

// enginePass is Pass in engine mode: the tracked containers are handled one
// at a time, every container after the ones it depends on.
func (u *Updater) enginePass(ctx context.Context) {
	u.setLeader(true)
	start := u.now()
	if w, in := schedule.InBlackout(u.Config.Blackout, start); in {
		u.Log.Info("inside blackout window, pass skipped", "window", w.String())
		u.passOutcome("blackout")
		return
	}
	all, err := engine.List(ctx, u.Engine)
	if err != nil {
		u.Log.Error("cannot list containers", "error", err)
		u.passOutcome("error")
		return
	}
	set := engine.NewSet(all, u.Config.ComposeDependsOn)
	tracked := u.trackedContainers(all)
	u.Log.Info("pass started", "containers", len(tracked))

	p := &pass{
		digests:     map[string]digestResult{},
		rateLimited: map[string]bool{},
		updatedRepo: map[string]bool{},
		checked:     map[string]bool{},
	}
	r := &recreations{replaced: map[string]string{}, touched: map[string]bool{}}
	for _, c := range set.Order(tracked) {
		if ctx.Err() != nil {
			break
		}
		u.container(ctx, p, set, r, c)
	}

	// A dependent left out of the selection keeps pointing at the old
	// container: Iðunn leaves it alone, as asked, but says so.
	isTracked := map[string]bool{}
	for _, c := range tracked {
		isTracked[c.Name] = true
	}
	for _, n := range sortedKeys(r.touched) {
		for _, dep := range set.Dependents(n) {
			if !isTracked[dep] {
				u.Log.Warn("dependent container not tracked, not recreated", "container", dep, "depends_on", n)
			}
		}
	}

	if u.Config.SelfUpdate && u.Self.ContainerID != "" && ctx.Err() == nil {
		if self, err := engine.Inspect(ctx, u.Engine, u.Self.ContainerID); err != nil {
			u.Log.Error("cannot inspect own container, self-update skipped", "error", err)
		} else {
			u.engineSelf(ctx, p, self)
		}
	}

	u.forgetUnchecked(p)
	if len(p.updatedRepo) > 0 && u.Config.Cleanup && u.Cleaner != nil {
		for _, e := range u.Cleaner(ctx, sortedKeys(p.updatedRepo)) {
			p.add(e)
		}
	}

	u.Log.Info("pass finished", "events", len(p.events))
	u.passFinished(p, start, len(tracked))
	u.Notifier.Send(ctx, p.events)
	if p.selfApply != nil {
		p.selfApply()
	}
}

// recreations records what a pass changed, for the containers that depend on it.
type recreations struct {
	replaced map[string]string // old container ID -> new container ID
	touched  map[string]bool   // names of the containers stopped by this pass
}

func (r *recreations) record(c *engine.Container, res engine.Result) {
	switch res.Outcome {
	case engine.Unchanged:
		return
	case engine.Updated, engine.Kept:
		r.replaced[c.ID] = res.NewID
	}
	r.touched[c.Name] = true
}

// container handles one container: it is updated when its image changed,
// and recreated as it is when a container it depends on was recreated.
func (u *Updater) container(ctx context.Context, p *pass, set *engine.Set, r *recreations, c *engine.Container) {
	log := u.Log.With("container", c.Name)
	var changed []string
	for _, d := range set.DependsOn(c.Name) {
		if r.touched[d] {
			changed = append(changed, d)
		}
	}
	if u.updateContainer(ctx, log, p, r, c) || len(changed) == 0 {
		return
	}
	if u.Config.DryRun {
		log.Info("dry run: would recreate, a container it depends on was recreated", "depends_on", strings.Join(changed, ","))
		return
	}
	t := u.containerTarget(ctx, c)
	log.Info("recreating, a container it depends on was recreated", "depends_on", strings.Join(changed, ","))
	res := engine.Recreate(ctx, u.Engine, c, u.recreateOptions(t, r, "", false), log)
	r.record(c, res)
	switch res.Outcome {
	case engine.Updated:
		log.Info("recreated", "duration", res.Duration.Round(time.Second))
	case engine.Unchanged:
		log.Error("cannot recreate", "error", res.Message)
	default:
		ev := notify.Event{Time: u.now(), Service: t.Name, Image: t.Image, Duration: res.Duration,
			Message: "recreated after " + strings.Join(changed, ", ") + ": " + res.Message}
		u.recreateEvent(log, &ev, res)
		p.add(ev)
	}
}

// updateContainer applies a new image to c when there is one, following the
// same rules as for services. It reports whether c was recreated.
func (u *Updater) updateContainer(ctx context.Context, log *slog.Logger, p *pass, r *recreations, c *engine.Container) bool {
	t := u.containerTarget(ctx, c)
	if t.Skip != "" {
		log.Info("ignored", "reason", t.Skip)
		return false
	}
	log = log.With("image", t.Image)
	latest, ok := u.newImage(ctx, log, p, t)
	if !ok {
		return false
	}
	if t.MonitorOnly {
		log.Info("new image detected (monitor only)", "digest", notify.ShortDigest(latest))
		if u.markOnce(u.notifiedDetected, t.ID, latest) {
			p.add(notify.Event{Kind: notify.KindDetected, Level: notify.Info, Time: u.now(), Service: t.Name,
				Image: t.Image, NewDigest: latest})
		}
		return false
	}

	auth, err := u.Auth.EncodedAuth(t.Image)
	if err != nil {
		log.Error("cannot build registry auth", "error", redact.Error(err))
		return false
	}
	log.Info("updating", "from", notify.ShortDigest(t.Current), "to", notify.ShortDigest(latest))
	res := engine.Recreate(ctx, u.Engine, c, u.recreateOptions(t, r, auth, true), log)
	r.record(c, res)
	ev := notify.Event{Time: u.now(), Service: t.Name, Image: t.Image, OldDigest: t.Current, NewDigest: latest,
		Duration: res.Duration, Message: res.Message}
	switch res.Outcome {
	case engine.Unchanged:
		// Nothing changed (pull refused, registry down): the next pass tries again.
		log.Error("update not started, retrying next pass", "error", res.Message)
		return false
	case engine.Updated:
		log.Info("updated", "digest", notify.ShortDigest(latest), "duration", ev.Duration.Round(time.Second))
		ev.Kind, ev.Level = notify.KindUpdated, notify.Info
		p.mu.Lock()
		p.updatedRepo[repository(t.Image)] = true
		p.mu.Unlock()
		u.forgetService(t.ID)
	default:
		u.recreateEvent(log, &ev, res)
		if res.Outcome == engine.RolledBack {
			u.mu.Lock()
			u.rolledBackImage[c.Name] = latest
			u.mu.Unlock()
		}
	}
	p.add(ev)
	return true
}

// recreateEvent fills ev for a failed recreation.
func (u *Updater) recreateEvent(log *slog.Logger, ev *notify.Event, res engine.Result) {
	switch res.Outcome {
	case engine.RolledBack:
		ev.Kind, ev.Level, ev.State = notify.KindRollback, notify.Warn, "rollback_completed"
		log.Warn("rolled back", "message", ev.Message)
	case engine.RollbackFailed:
		ev.Kind, ev.Level, ev.State = notify.KindRollbackFailed, notify.Error, "rollback_failed"
		log.Error("rollback not completed", "message", ev.Message)
	case engine.Kept:
		ev.Kind, ev.Level, ev.State = notify.KindPaused, notify.Error, "paused"
		log.Error("update not completed, rollback disabled", "message", ev.Message)
	}
}

// recreateOptions returns the recreation settings for t. With pull set, its
// image is pulled first.
func (u *Updater) recreateOptions(t target, r *recreations, auth string, pull bool) engine.Options {
	o := engine.Options{
		Auth:       auth,
		Timeout:    t.Timeout,
		Settle:     u.settle(),
		Rollback:   t.Rollback,
		Replaced:   r.replaced,
		KeepLabels: []string{u.Config.LabelFilter.Key, u.Config.LabelIgnore.Key},
		Poll:       u.PollInterval,
	}
	if pull {
		o.Pull = t.Image
	}
	return o
}

// newImage returns the new digest of t's image when it should be applied
// now, after the checks shared with services: registry, rolled-back digest,
// maturity, dry run. Monitor-only is left to the caller.
func (u *Updater) newImage(ctx context.Context, log *slog.Logger, p *pass, t target) (string, bool) {
	latest, err := u.digest(ctx, p, t.Image)
	if err != nil {
		u.headFailed(p, log, t, err)
		return "", false
	}
	u.headRecovered(t.Image)
	if slices.Contains(t.Currents, latest) {
		u.forgetService(t.ID)
		log.Debug("up to date", "digest", notify.ShortDigest(latest))
		return "", false
	}
	u.mu.Lock()
	rolledBack := u.rolledBackImage[t.Name] == latest
	u.mu.Unlock()
	if rolledBack {
		log.Warn("new image was rolled back, not retried until a newer one", "digest", notify.ShortDigest(latest))
		if u.markOnce(u.notifiedRetry, t.ID, latest) {
			p.add(notify.Event{Kind: notify.KindRetrySkipped, Level: notify.Warn, Time: u.now(), Service: t.Name,
				Image: t.Image, NewDigest: latest})
		}
		return "", false
	}

	now := u.now()
	first := u.observe(t.Image, latest, now)
	if t.MinAge > 0 && now.Sub(first) < t.MinAge {
		applyAt := first.Add(t.MinAge)
		log.Info("new image waiting for maturity", "digest", notify.ShortDigest(latest), "apply_after", applyAt.Format(time.RFC3339))
		p.mu.Lock()
		p.pending++
		p.mu.Unlock()
		if u.markOnce(u.notifiedPending, t.ID, latest) {
			p.add(notify.Event{Kind: notify.KindPending, Level: notify.Info, Time: now, Service: t.Name,
				Image: t.Image, NewDigest: latest, ApplyAt: applyAt})
		}
		return "", false
	}
	if u.Config.DryRun {
		log.Info("dry run: would update", "from", notify.ShortDigest(t.Current), "to", notify.ShortDigest(latest))
		return "", false
	}
	return latest, true
}

// containerTarget applies the global settings and the container labels, and
// finds the registry digests of the image the container runs. Containers are
// keyed by name: the ID changes with every recreation.
func (u *Updater) containerTarget(ctx context.Context, c *engine.Container) target {
	t := target{
		ID:          c.Name,
		Name:        c.Name,
		Image:       c.Image(),
		MonitorOnly: u.Config.MonitorOnly,
		MinAge:      u.Config.MinAge,
		Timeout:     u.Config.UpdateTimeout,
		Rollback:    u.Config.RollbackOnFailure,
	}
	t.applyLabels(c.Labels())
	switch {
	case t.Image == "", strings.HasPrefix(t.Image, "sha256:"):
		t.Skip = "created from an image ID"
		return t
	case strings.Contains(t.Image, "@"):
		t.Skip = "image pinned by digest"
		return t
	}
	if _, err := name.ParseReference(t.Image); err != nil {
		t.Skip = "invalid image reference"
		return t
	}

	// With the containerd image store, the image ID is the digest the
	// registry served, and an image whose tag moved on (pulled by someone
	// else) loses its repo digests, or disappears. Such a container is not on
	// the image of its tag any more: it is updated.
	if img, err := u.Engine.ImageInspect(ctx, c.Info.Image); err == nil {
		repo := repository(t.Image)
		for _, rd := range img.RepoDigests {
			ref, err := name.NewDigest(rd)
			if err == nil && ref.Context().Name() == repo {
				t.Currents = append(t.Currents, ref.DigestStr())
			}
		}
		if len(img.RepoDigests) == 0 && len(img.RepoTags) > 0 {
			t.Skip = "image built locally, not pulled from a registry"
			return t
		}
	}
	t.Currents = append(t.Currents, c.Info.Image)
	if d := c.Info.ImageManifestDescriptor; d != nil {
		t.Currents = append(t.Currents, d.Digest.String())
	}
	t.Current = t.Currents[0]
	return t
}

// engineSelf handles Iðunn's own container, last: when a new image is to be
// applied, the hand-over runs once the report is sent.
func (u *Updater) engineSelf(ctx context.Context, p *pass, self *engine.Container) {
	t := u.containerTarget(ctx, self)
	log := u.Log.With("container", t.Name)
	if t.Skip != "" {
		log.Info("self-update: ignored", "reason", t.Skip)
		return
	}
	log = log.With("image", t.Image)
	latest, ok := u.newImage(ctx, log, p, t)
	if !ok {
		return
	}
	if t.MonitorOnly {
		log.Info("new image detected (monitor only)", "digest", notify.ShortDigest(latest))
		if u.markOnce(u.notifiedDetected, t.ID, latest) {
			p.add(notify.Event{Kind: notify.KindDetected, Level: notify.Info, Time: u.now(), Service: t.Name,
				Image: t.Image, NewDigest: latest})
		}
		return
	}
	log.Info("self-update scheduled at the end of the pass", "from", notify.ShortDigest(t.Current), "to", notify.ShortDigest(latest))
	p.add(notify.Event{Kind: notify.KindSelfUpdate, Level: notify.Info, Time: u.now(), Service: t.Name,
		Image: t.Image, OldDigest: t.Current, NewDigest: latest})
	p.mu.Lock()
	p.selfApply = func() { u.handOver(ctx, self, t, latest) }
	p.mu.Unlock()
}

// handOver starts the new Iðunn container next to this one and, once it is
// healthy, stops this one. On failure this instance goes on, under its name.
func (u *Updater) handOver(ctx context.Context, self *engine.Container, t target, digest string) {
	log := u.Log.With("container", t.Name, "image", t.Image)
	fail := func(msg string) {
		log.Error("self-update failed", "error", msg)
		u.mu.Lock()
		u.rolledBackImage[self.Name] = digest
		u.mu.Unlock()
		u.Notifier.Send(ctx, []notify.Event{{Kind: notify.KindSelfFailed, Level: notify.Error, Time: u.now(),
			Service: t.Name, Image: t.Image, NewDigest: digest, Message: msg}})
	}
	auth, err := u.Auth.EncodedAuth(t.Image)
	if err != nil {
		fail("cannot build registry auth: " + redact.Error(err).Error())
		return
	}
	o := u.recreateOptions(t, &recreations{}, auth, true)
	o.Rollback, o.HandOver = true, true
	o.Labels = map[string]string{engine.LabelReplaces: self.ID}
	res := engine.Recreate(ctx, u.Engine, self, o, log)
	if res.Outcome != engine.Updated {
		fail(res.Message)
		return
	}
	log.Info("new instance healthy, stopping this one", "new", res.NewID[:min(12, len(res.NewID))])
	// Stopping our own container ends this process: the call is not waited
	// for, so that the pass returns and Iðunn stops cleanly on SIGTERM.
	go u.Engine.ContainerStop(context.WithoutCancel(ctx), self.ID, client.ContainerStopOptions{})
}

// engineVersionItems lists the running containers for the version check,
// whatever their selection labels, like services.
func (u *Updater) engineVersionItems(ctx context.Context) ([]versionItem, error) {
	all, err := engine.List(ctx, u.Engine)
	if err != nil {
		return nil, err
	}
	var items []versionItem
	for _, c := range all {
		if c.SwarmTask() || strings.HasSuffix(c.Name, engine.OldSuffix) {
			continue
		}
		items = append(items, versionItem{name: c.Name, image: c.Image(), labels: c.Labels()})
	}
	return items, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
