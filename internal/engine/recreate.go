package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/xaoimoon/idunn/internal/redact"
)

// Outcome is how a recreation ended.
type Outcome int

const (
	// Unchanged: it failed before the old container was touched.
	Unchanged Outcome = iota
	// Updated: the new container runs and the old one is removed.
	Updated
	// RolledBack: the new container failed and the old one runs again.
	RolledBack
	// RollbackFailed: the old container could not be started again.
	RollbackFailed
	// Kept: the new container failed and rollback is disabled. The old one
	// is left stopped, renamed with OldSuffix.
	Kept
)

// Options drive one recreation.
type Options struct {
	Pull     string        // image reference to pull first, "" to use the local image
	Auth     string        // X-Registry-Auth for the pull, may be empty
	Timeout  time.Duration // how long the new container has to become healthy
	Settle   time.Duration // without healthcheck: how long it must keep running
	Rollback bool
	// Replaced maps containers recreated earlier in the pass to their
	// replacements (old ID -> new ID).
	Replaced   map[string]string
	KeepLabels []string          // label keys kept even when the image sets them
	Labels     map[string]string // labels added to the new container
	Poll       time.Duration     // state polling interval, 2 s by default
	// HandOver leaves the old container running and in place once the new
	// one is healthy: used by Iðunn on itself, the old instance then stops.
	HandOver bool
}

// Result describes a recreation.
type Result struct {
	Outcome  Outcome
	NewID    string // the new container, when it exists
	ImageID  string // image of the new container
	Message  string // why it failed
	Duration time.Duration
}

// opTimeout bounds each Docker call that changes a container, and the whole
// rollback. Those calls ignore the pass being canceled: a container is never
// left half replaced because Iðunn is stopping.
const opTimeout = 2 * time.Minute

// Recreate replaces old by a container created from the same configuration
// with the current image of its reference, pulled first if o.Pull is set:
// the old container is renamed, the new one created under its name, the old
// one stopped and the new one started. It then waits for the new container
// to be healthy, or to keep running for o.Settle without a healthcheck, and
// removes the old one. On failure it removes the new container and starts
// the old one again under its name.
func Recreate(ctx context.Context, d Docker, old *Container, o Options, log *slog.Logger) Result {
	start := time.Now()
	res := func(out Outcome, msg string) Result {
		return Result{Outcome: out, Message: msg, Duration: time.Since(start)}
	}
	if o.Pull != "" {
		if err := Pull(ctx, d, o.Pull, o.Auth); err != nil {
			return res(Unchanged, "cannot pull image: "+redact.Error(err).Error())
		}
	}
	newImage, err := d.ImageInspect(ctx, old.Image())
	if err != nil {
		return res(Unchanged, "cannot inspect image: "+err.Error())
	}
	so := specOptions{newImageID: newImage.ID, replaced: o.Replaced, keep: o.KeepLabels, labels: o.Labels}
	if oldImage, err := d.ImageInspect(ctx, old.Info.Image); err == nil {
		so.imageConfig = oldImage.Config
	} else {
		// The old image is gone (its tag moved and it was removed): what
		// matches the new image is left to it, labels by key.
		so.imageConfig, so.guessed = newImage.Config, true
	}
	s, err := newSpec(old, so)
	if err != nil {
		return res(Unchanged, "cannot copy configuration: "+err.Error())
	}

	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
	defer cancel()
	if _, err := d.ContainerRename(dctx, old.ID, client.ContainerRenameOptions{NewName: old.Name + OldSuffix}); err != nil {
		return res(Unchanged, "cannot rename container: "+err.Error())
	}
	created, err := d.ContainerCreate(dctx, client.ContainerCreateOptions{
		Name: old.Name, Config: s.config, HostConfig: s.host, NetworkingConfig: s.network,
	})
	if err != nil {
		r := res(RolledBack, "cannot create container: "+err.Error())
		if msg := restore(ctx, d, old, "", false); msg != "" {
			r.Outcome, r.Message = RollbackFailed, r.Message+"; "+msg
		}
		return r
	}
	for _, w := range created.Warnings {
		log.Warn("docker warning", "warning", w)
	}
	newID := created.ID
	fail := func(msg string, oldStopped bool) Result {
		r := res(RolledBack, msg)
		r.NewID, r.ImageID = newID, newImage.ID
		if m := restore(ctx, d, old, newID, oldStopped); m != "" {
			r.Outcome, r.Message = RollbackFailed, msg+"; "+m
		}
		r.Duration = time.Since(start)
		return r
	}
	for name, ep := range s.extra {
		if _, err := d.NetworkConnect(dctx, name, client.NetworkConnectOptions{Container: newID, EndpointConfig: ep}); err != nil {
			return fail(fmt.Sprintf("cannot connect to network %s: %v", name, err), false)
		}
	}
	stopped := !o.HandOver
	if stopped {
		if _, err := d.ContainerStop(dctx, old.ID, client.ContainerStopOptions{}); err != nil {
			return fail("cannot stop old container: "+err.Error(), true)
		}
	}
	if _, err := d.ContainerStart(dctx, newID, client.ContainerStartOptions{}); err != nil {
		return fail("cannot start container: "+err.Error(), stopped)
	}
	if err := WaitHealthy(ctx, d, newID, o.Timeout, o.Settle, o.Poll); err != nil {
		if !o.Rollback && !o.HandOver {
			r := res(Kept, err.Error())
			r.NewID, r.ImageID = newID, newImage.ID
			return r
		}
		return fail(err.Error(), stopped)
	}
	if !o.HandOver {
		if _, err := d.ContainerRemove(dctx, old.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
			log.Warn("cannot remove old container", "container", old.Name+OldSuffix, "error", err)
		}
	}
	r := res(Updated, "")
	r.NewID, r.ImageID = newID, newImage.ID
	return r
}

// restore removes the new container, if any, gives the old one its name back
// and starts it again if it was stopped. It returns what went wrong, "" when
// the old container runs again.
func restore(ctx context.Context, d Docker, old *Container, newID string, start bool) string {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
	defer cancel()
	if newID != "" {
		if _, err := d.ContainerRemove(rctx, newID, client.ContainerRemoveOptions{Force: true}); err != nil {
			return "cannot remove new container: " + err.Error()
		}
	}
	if _, err := d.ContainerRename(rctx, old.ID, client.ContainerRenameOptions{NewName: old.Name}); err != nil {
		return "cannot rename old container back: " + err.Error()
	}
	if !start {
		return ""
	}
	if _, err := d.ContainerStart(rctx, old.ID, client.ContainerStartOptions{}); err != nil {
		return "cannot start old container: " + err.Error()
	}
	return ""
}

// ErrUnhealthy is wrapped by WaitHealthy failures.
var ErrUnhealthy = errors.New("container not healthy")

// WaitHealthy waits until the container reports healthy, when it has a
// healthcheck, or until it has kept running for settle without one. It fails
// as soon as the container stops, restarts or reports unhealthy, and after
// timeout.
func WaitHealthy(ctx context.Context, d Docker, id string, timeout, settle, poll time.Duration) error {
	if poll <= 0 {
		poll = 2 * time.Second
	}
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	if settle > timeout {
		settle = timeout
	}
	start := time.Now()
	deadline := start.Add(timeout)
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		c, err := Inspect(ctx, d, id)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("%w: %v", ErrUnhealthy, ctx.Err())
			}
			return fmt.Errorf("%w: cannot inspect: %v", ErrUnhealthy, err)
		}
		st := c.Info.State
		switch {
		case st == nil:
		case st.Restarting || (!st.Running && st.Status != container.StateCreated):
			return fmt.Errorf("%w: %s (exit code %d)%s", ErrUnhealthy, st.Status, st.ExitCode, withError(st.Error))
		case c.Info.RestartCount > 0:
			return fmt.Errorf("%w: restarted %d times", ErrUnhealthy, c.Info.RestartCount)
		case st.Health != nil:
			switch st.Health.Status {
			case container.Healthy:
				return nil
			case container.Unhealthy:
				return fmt.Errorf("%w: unhealthy%s", ErrUnhealthy, lastProbe(st.Health))
			}
		case time.Since(start) >= settle:
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w: not healthy after %s", ErrUnhealthy, timeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ErrUnhealthy, ctx.Err())
		case <-tick.C:
		}
	}
}

func withError(s string) string {
	if s == "" {
		return ""
	}
	return ": " + s
}

func lastProbe(h *container.Health) string {
	if len(h.Log) == 0 || h.Log[len(h.Log)-1] == nil {
		return ""
	}
	return withError(h.Log[len(h.Log)-1].Output)
}

// Pull pulls ref and waits for the end of the pull.
func Pull(ctx context.Context, d Docker, ref, auth string) error {
	resp, err := d.ImagePull(ctx, ref, client.ImagePullOptions{RegistryAuth: auth})
	if err != nil {
		return err
	}
	defer resp.Close()
	return resp.Wait(ctx)
}
