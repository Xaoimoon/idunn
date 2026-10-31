package updater

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/moby/moby/client"

	"github.com/xaoimoon/idunn/internal/cluster"
	"github.com/xaoimoon/idunn/internal/notify"
	"github.com/xaoimoon/idunn/internal/redact"
	"github.com/xaoimoon/idunn/internal/registry"
	"github.com/xaoimoon/idunn/internal/versions"
)

// labelVersionLevel sets the smallest bump notified for a service: patch,
// minor, major, or none to leave the service out of the version check.
const labelVersionLevel = "idunn.version-level"

// listTimeout bounds the tag listing of one repository: some registries
// (docker.elastic.co) page through an endless list of build tags.
var listTimeout = 2 * time.Minute

// TagLister lists the tags of an image's repository.
type TagLister interface {
	Tags(ctx context.Context, image string) ([]string, error)
}

// versionItem is a service, or a container in engine mode, as the version
// check sees it.
type versionItem struct {
	name   string
	image  string
	labels map[string]string
}

// VersionCheck looks, for every service whose image tag is a version number,
// for a newer version tag in the registry, and sends what it finds as one
// report. It never updates anything. Unlike passes, it covers all services,
// whatever their opt-in labels, Iðunn included: pinned versions are exactly
// the ones nobody else watches. Only the Raft leader runs it. In engine mode,
// it covers the running containers instead.
func (u *Updater) VersionCheck(ctx context.Context) {
	if !u.versionMu.TryLock() {
		u.Log.Warn("previous version check still running, skipping this one")
		return
	}
	defer u.versionMu.Unlock()

	var items []versionItem
	if u.Engine != nil {
		var err error
		if items, err = u.engineVersionItems(ctx); err != nil {
			u.Log.Error("cannot list containers", "error", err)
			return
		}
	} else {
		leader, err := cluster.IsLeader(ctx, u.Docker, u.NodeID)
		if err != nil {
			u.Log.Error("cannot check Raft leadership", "error", err)
			return
		}
		u.setLeader(leader)
		if !leader {
			return
		}

		res, err := u.Docker.ServiceList(ctx, client.ServiceListOptions{})
		if err != nil {
			u.Log.Error("cannot list services", "error", err)
			return
		}
		services := res.Items
		sort.Slice(services, func(i, j int) bool { return services[i].Spec.Name < services[j].Spec.Name })
		for _, svc := range services {
			image, _ := referenceImage(svc)
			items = append(items, versionItem{name: svc.Spec.Name, image: image, labels: svc.Spec.Labels})
		}
	}
	u.Log.Info("version check started", "services", len(items))

	type listing struct {
		tags []string
		err  error
	}
	byRepo := map[string]listing{} // one tag listing per repository
	skipped := map[string]bool{}   // registries that timed out or answered 429
	var events []notify.Event
	for _, it := range items {
		if ctx.Err() != nil {
			return
		}
		log := u.Log.With("service", it.name)
		min := u.Config.VersionLevel
		if v, ok := it.labels[labelVersionLevel]; ok {
			l, valid := versions.ParseLevel(v)
			if !valid {
				log.Warn("invalid label value, using the global level", "label", labelVersionLevel, "value", v)
			} else {
				min = l
			}
		}
		if min == versions.None {
			continue
		}
		image, _, _ := strings.Cut(it.image, "@") // the stack file may pin tag@digest
		ref, err := name.NewTag(image)
		if err != nil || !versions.IsVersion(ref.TagStr()) {
			log.Debug("tag is not a version, skipped", "image", image)
			continue
		}
		repo := ref.Context().Name()
		reg := ref.Context().RegistryStr()
		l, done := byRepo[repo]
		if !done && skipped[reg] {
			log.Debug("registry skipped for the rest of this check", "image", image, "registry", reg)
			continue
		}
		if !done {
			lctx, cancel := context.WithTimeout(ctx, listTimeout)
			l.tags, l.err = u.Lister.Tags(lctx, image)
			cancel()
			byRepo[repo] = l
			// A registry that is too slow or rate limited would fail the same
			// way for its other repositories: stop asking it until next check.
			var he *registry.HeadError
			if errors.As(l.err, &he) {
				u.registryError(he)
			}
			if l.err != nil && ctx.Err() == nil &&
				(errors.Is(l.err, context.DeadlineExceeded) || (errors.As(l.err, &he) && he.RateLimited())) {
				skipped[reg] = true
				log.Warn("registry skipped for the rest of this check", "registry", reg)
			}
		}
		if l.err != nil {
			log.Warn("cannot list tags", "image", image, "error", redact.Error(l.err))
			continue
		}
		latest, bump, ok := versions.Newer(ref.TagStr(), l.tags, min)
		if !ok {
			log.Debug("no newer version", "image", image)
			continue
		}
		log.Info("newer version available", "image", image, "tag", latest, "bump", bump.String())
		events = append(events, notify.Event{Kind: notify.KindNewVersion, Level: notify.Info, Time: u.now(),
			Service: it.name, Image: image, Tag: latest, Bump: bump.String()})
	}
	u.Log.Info("version check finished", "available", len(events))
	u.versionsFound(events)
	u.Notifier.Send(ctx, events)
}
