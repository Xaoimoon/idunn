package updater

import (
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/moby/moby/api/types/swarm"

	"github.com/xaoimoon/idunn/internal/config"
)

// Labels read on services, in deploy.labels. They override global settings.
const (
	labelStackImage  = "com.docker.stack.image"
	labelMonitorOnly = "idunn.monitor-only"
	labelMinAge      = "idunn.min-age"
	labelTimeout     = "idunn.timeout"
	labelRollback    = "idunn.rollback"
)

// target is a service as Iðunn sees it.
type target struct {
	ID      string
	Name    string
	Image   string // reference image: the tag as written in the stack file
	Current string // digest currently pinned in the spec, may be empty

	MonitorOnly bool
	MinAge      time.Duration
	Timeout     time.Duration
	Rollback    bool

	Skip string // reason not to check this service, empty if it is checked
}

// newTarget applies the global settings, then the service labels, and works
// out the reference image and the pinned digest.
func newTarget(svc swarm.Service, cfg *config.Config) target {
	t := target{
		ID:          svc.ID,
		Name:        svc.Spec.Name,
		MonitorOnly: cfg.MonitorOnly,
		MinAge:      cfg.MinAge,
		Timeout:     cfg.UpdateTimeout,
		Rollback:    cfg.RollbackOnFailure,
	}
	labels := svc.Spec.Labels

	var specImage string
	if cs := svc.Spec.TaskTemplate.ContainerSpec; cs != nil {
		specImage = cs.Image
	}
	if specImage == "" {
		t.Skip = "not a container service"
		return t
	}
	base, digest, _ := strings.Cut(specImage, "@")
	t.Current = digest
	t.Image = base
	if stackImage := strings.TrimSpace(labels[labelStackImage]); stackImage != "" {
		t.Image = stackImage
	}
	if strings.Contains(t.Image, "@") {
		t.Skip = "image pinned by digest"
		return t
	}
	if _, err := name.ParseReference(t.Image); err != nil {
		t.Skip = "invalid image reference"
		return t
	}

	if v, ok := labels[labelMonitorOnly]; ok {
		if b, err := strconv.ParseBool(v); err == nil {
			t.MonitorOnly = b
		}
	}
	if v, ok := labels[labelMinAge]; ok {
		if d, err := parseDuration(v); err == nil {
			t.MinAge = d
		}
	}
	if v, ok := labels[labelTimeout]; ok {
		if d, err := parseDuration(v); err == nil && d > 0 {
			t.Timeout = d
		}
	}
	if v, ok := labels[labelRollback]; ok {
		if b, err := strconv.ParseBool(v); err == nil {
			t.Rollback = b
		}
	}

	if us := svc.UpdateStatus; us != nil {
		switch us.State {
		case swarm.UpdateStateUpdating, swarm.UpdateStateRollbackStarted:
			t.Skip = "update already in progress (" + string(us.State) + ")"
		}
	}
	return t
}

func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

// repository returns the normalized repository of an image reference, as
// used to match local images during cleanup.
func repository(image string) string {
	ref, err := name.ParseReference(image)
	if err != nil {
		return ""
	}
	return ref.Context().Name()
}

// matches reports whether labels carry key=value.
func matches(labels map[string]string, l config.Label) bool {
	v, ok := labels[l.Key]
	return ok && v == l.Value
}
