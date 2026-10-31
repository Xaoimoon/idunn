package engine

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// A self-update in engine mode hands over from one Iðunn container to the
// next: the running instance renames itself with OldSuffix, starts the new
// container under its name with LabelReplaces pointing back at itself, waits
// for it to be healthy, then stops itself. The new instance does nothing
// until the old one has stopped, then removes it. On failure, the old
// instance removes the new container and takes its name back.

// AwaitPredecessor runs when Iðunn starts: if its container replaces another
// one, it waits for that one to stop, then removes it.
func AwaitPredecessor(ctx context.Context, d Docker, self *Container, poll time.Duration, log *slog.Logger) error {
	prev := self.Labels()[LabelReplaces]
	if prev == "" {
		return nil
	}
	if poll <= 0 {
		poll = 2 * time.Second
	}
	logged := false
	for {
		c, err := Inspect(ctx, d, prev)
		if cerrdefs.IsNotFound(err) {
			return nil
		}
		if err == nil && !c.Running() {
			if _, err := d.ContainerRemove(ctx, prev, client.ContainerRemoveOptions{Force: true}); err != nil && !cerrdefs.IsNotFound(err) {
				log.Warn("cannot remove previous instance", "container", c.Name, "error", err)
			} else {
				log.Info("previous instance removed", "container", c.Name)
			}
			return nil
		}
		if !logged {
			log.Info("waiting for the previous instance to stop", "container", prev[:min(12, len(prev))])
			logged = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// Superseded runs when Iðunn starts under a name ending with OldSuffix: a
// hand-over was interrupted, by a reboot for instance. It reports true when
// the new instance runs and replaces this one, which must then stop. Else it
// removes what is left of the new container and takes its name back.
func Superseded(ctx context.Context, d Docker, self *Container, log *slog.Logger) (bool, error) {
	base, ok := strings.CutSuffix(self.Name, OldSuffix)
	if !ok {
		return false, nil
	}
	next, err := Inspect(ctx, d, base)
	switch {
	case cerrdefs.IsNotFound(err):
	case err != nil:
		return false, err
	case next.Labels()[LabelReplaces] != self.ID:
		return false, fmt.Errorf("container %s exists and does not replace this one", base)
	case next.Running():
		return true, nil
	default:
		if _, err := d.ContainerRemove(ctx, next.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
			return false, fmt.Errorf("cannot remove interrupted replacement: %w", err)
		}
		log.Warn("interrupted self-update undone", "container", base)
	}
	if _, err := d.ContainerRename(ctx, self.ID, client.ContainerRenameOptions{NewName: base}); err != nil {
		return false, fmt.Errorf("cannot take name %s back: %w", base, err)
	}
	self.Name = base
	return false, nil
}
