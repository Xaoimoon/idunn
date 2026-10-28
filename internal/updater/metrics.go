package updater

import (
	"strconv"
	"time"

	"github.com/xaoimoon/idunn/internal/metrics"
	"github.com/xaoimoon/idunn/internal/notify"
	"github.com/xaoimoon/idunn/internal/registry"
)

// Metric names and help texts. Every instance exposes them; only the leader's
// pass and update metrics move, idunn_leader tells which one it is.
const (
	mLeader         = "idunn_leader"
	mPasses         = "idunn_passes_total"
	mLastPass       = "idunn_last_pass_timestamp_seconds"
	mPassDuration   = "idunn_last_pass_duration_seconds"
	mTracked        = "idunn_services_tracked"
	mPending        = "idunn_images_pending"
	mUpdates        = "idunn_updates_total"
	mCleanupFailed  = "idunn_cleanup_failures_total"
	mRegistryErrors = "idunn_registry_errors_total"
	mNewVersion     = "idunn_new_version_available"
	mLastCheck      = "idunn_last_version_check_timestamp_seconds"

	hNewVersion = "1 for each service whose image has a newer version tag, as of the last version check."
)

func (u *Updater) setLeader(leader bool) {
	v := 0.0
	if leader {
		v = 1
	}
	u.Metrics.Set(mLeader, "1 if this instance is the Raft leader, the only one acting.", nil, v)
}

// passOutcome counts a pass by how it ended: completed, standby (not the
// leader), blackout, guard (node unavailable) or error.
func (u *Updater) passOutcome(outcome string) {
	u.Metrics.Add(mPasses, "Passes by outcome.", metrics.Labels{"outcome": outcome}, 1)
}

func (u *Updater) passFinished(p *pass, start time.Time, tracked int) {
	if u.Metrics == nil {
		return
	}
	u.passOutcome("completed")
	end := u.now()
	u.Metrics.Set(mLastPass, "End of the last completed pass, Unix time.", nil, float64(end.Unix()))
	u.Metrics.Set(mPassDuration, "Duration of the last completed pass.", nil, end.Sub(start).Seconds())
	u.Metrics.Set(mTracked, "Services selected by the label filter at the last pass.", nil, float64(tracked))
	p.mu.Lock()
	defer p.mu.Unlock()
	u.Metrics.Set(mPending, "Services whose new image waits for the maturity delay.", nil, float64(p.pending))
	for _, e := range p.events {
		switch e.Kind {
		case notify.KindUpdated, notify.KindRollback, notify.KindRollbackFailed, notify.KindPaused,
			notify.KindSelfUpdate, notify.KindRetrySkipped:
			u.Metrics.Add(mUpdates, "Update attempts by service and outcome.",
				metrics.Labels{"service": e.Service, "outcome": string(e.Kind)}, 1)
		case notify.KindCleanupFailed:
			u.Metrics.Add(mCleanupFailed, "Failed image cleanups.", nil, 1)
		}
	}
}

func (u *Updater) registryError(he *registry.HeadError) {
	u.Metrics.Add(mRegistryErrors, "Failed registry requests (HEAD or tag listing), by registry and HTTP code (0: unreachable or timeout).",
		metrics.Labels{"registry": he.Registry, "code": strconv.Itoa(he.StatusCode)}, 1)
}

// versionsFound replaces the newer versions found by the previous check.
func (u *Updater) versionsFound(events []notify.Event) {
	if u.Metrics == nil {
		return
	}
	u.Metrics.Reset(mNewVersion, hNewVersion)
	for _, e := range events {
		u.Metrics.Set(mNewVersion, hNewVersion,
			metrics.Labels{"service": e.Service, "image": e.Image, "latest": e.Tag, "bump": e.Bump}, 1)
	}
	u.Metrics.Set(mLastCheck, "End of the last version check, Unix time.", nil, float64(u.now().Unix()))
}
