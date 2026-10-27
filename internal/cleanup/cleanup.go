// Package cleanup removes old images after updates. The leader starts a
// global job running "idunn cleanup" from its own image on every node; each
// task then cleans its node's engine.
package cleanup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"

	"github.com/xaoimoon/idunn/internal/cluster"
	"github.com/xaoimoon/idunn/internal/config"
	"github.com/xaoimoon/idunn/internal/notify"
	"github.com/xaoimoon/idunn/internal/redact"
)

// JobLabel marks the cleanup job services.
const JobLabel = "idunn.cleanup"

// Job starts and follows the global cleanup job.
type Job struct {
	Docker  cluster.Docker
	Self    cluster.Self
	Auth    interface{ EncodedAuth(string) (string, error) }
	Keep    int
	Ignore  config.Label // set on the job so that no updater picks it up
	Socket  string       // host path of the Docker socket
	Timeout time.Duration
	Log     *slog.Logger

	PollInterval time.Duration
}

// Run cleans the given repositories on every node and returns one event per
// failed node.
func (j *Job) Run(ctx context.Context, repos []string) []notify.Event {
	fail := func(nodes []string, msg string) []notify.Event {
		return []notify.Event{{Kind: notify.KindCleanupFailed, Level: notify.Warn, Time: time.Now(), Nodes: nodes, Message: msg}}
	}
	if j.Self.Image == "" {
		j.Log.Warn("own image unknown (not running as a Swarm task), image cleanup skipped")
		return nil
	}
	nodes, err := cluster.Nodes(ctx, j.Docker)
	if err != nil {
		return fail([]string{"cluster"}, "cannot list nodes: "+err.Error())
	}
	names := map[string]string{}
	expected := 0
	for _, n := range nodes {
		names[n.ID] = n.Name
		if n.Ready() {
			expected++
		}
	}

	auth, _ := j.Auth.EncodedAuth(j.Self.Image)
	socket := j.Socket
	if socket == "" {
		socket = "/var/run/docker.sock"
	}
	args := append([]string{"cleanup", "--keep", strconv.Itoa(j.Keep), "--"}, repos...)
	spec := swarm.ServiceSpec{
		Annotations: swarm.Annotations{
			Name:   fmt.Sprintf("idunn-cleanup-%d", time.Now().Unix()),
			Labels: map[string]string{JobLabel: "true", j.Ignore.Key: j.Ignore.Value},
		},
		TaskTemplate: swarm.TaskSpec{
			ContainerSpec: &swarm.ContainerSpec{
				Image:    j.Self.Image,
				Args:     args,
				User:     j.Self.User,
				ReadOnly: true,
				Mounts: []mount.Mount{{
					Type:   mount.TypeBind,
					Source: socket,
					Target: "/var/run/docker.sock",
				}},
			},
			RestartPolicy: &swarm.RestartPolicy{Condition: swarm.RestartPolicyConditionNone},
		},
		Mode: swarm.ServiceMode{GlobalJob: &swarm.GlobalJob{}},
	}
	created, err := j.Docker.ServiceCreate(ctx, client.ServiceCreateOptions{Spec: spec, EncodedRegistryAuth: auth})
	if err != nil {
		return fail([]string{"cluster"}, "cannot create cleanup job: "+redact.String(err.Error()))
	}
	j.Log.Info("cleanup job started", "job", spec.Name, "repositories", strings.Join(repos, " "))
	defer func() {
		// The job is removed even if the pass context is canceled.
		rmCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := j.Docker.ServiceRemove(rmCtx, created.ID, client.ServiceRemoveOptions{}); err != nil {
			j.Log.Warn("cannot remove cleanup job", "job", spec.Name, "error", err)
		}
	}()

	tasks, err := j.waitTasks(ctx, created.ID, expected)
	if err != nil {
		return fail([]string{"cluster"}, err.Error())
	}
	var events []notify.Event
	for _, t := range tasks {
		if t.Status.State == swarm.TaskStateComplete {
			continue
		}
		node := names[t.NodeID]
		if node == "" {
			node = t.NodeID
		}
		msg := t.Status.Err
		if msg == "" {
			msg = string(t.Status.State)
		}
		j.Log.Warn("cleanup failed on node", "node", node, "error", msg)
		events = append(events, notify.Event{Kind: notify.KindCleanupFailed, Level: notify.Warn, Time: time.Now(), Nodes: []string{node}, Message: msg})
	}
	if len(events) == 0 {
		j.Log.Info("cleanup job completed", "nodes", len(tasks))
	}
	return events
}

func terminal(s swarm.TaskState) bool {
	switch s {
	case swarm.TaskStateComplete, swarm.TaskStateFailed, swarm.TaskStateRejected,
		swarm.TaskStateShutdown, swarm.TaskStateOrphaned, swarm.TaskStateRemove:
		return true
	}
	return false
}

// waitTasks waits until expected tasks have reached a final state, and
// returns the latest task of each node.
func (j *Job) waitTasks(ctx context.Context, jobID string, expected int) ([]swarm.Task, error) {
	timeout := j.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	interval := j.PollInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		res, err := j.Docker.TaskList(ctx, client.TaskListOptions{
			Filters: make(client.Filters).Add("service", jobID),
		})
		if err == nil {
			latest := map[string]swarm.Task{}
			for _, t := range res.Items {
				if cur, ok := latest[t.NodeID]; !ok || t.Meta.CreatedAt.After(cur.Meta.CreatedAt) {
					latest[t.NodeID] = t
				}
			}
			done := 0
			for _, t := range latest {
				if terminal(t.Status.State) {
					done++
				}
			}
			if done >= expected && done == len(latest) {
				out := make([]swarm.Task, 0, len(latest))
				for _, t := range latest {
					out = append(out, t)
				}
				return out, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("cleanup job did not finish within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// LocalDocker is the API used by the cleanup task on each node.
type LocalDocker interface {
	ImageList(ctx context.Context, options client.ImageListOptions) (client.ImageListResult, error)
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ImageRemove(ctx context.Context, imageID string, options client.ImageRemoveOptions) (client.ImageRemoveResult, error)
}

// Local removes, on the local engine, the images of repos that no running
// container uses, keeping the keep most recent ones per repository. An image
// still referenced by a stopped container (Swarm task history) is never
// forced out: it is kept and goes with the containers when they are pruned.
func Local(ctx context.Context, d LocalDocker, repos []string, keep int, log *slog.Logger) error {
	images, err := d.ImageList(ctx, client.ImageListOptions{})
	if err != nil {
		return fmt.Errorf("cannot list images: %w", err)
	}
	containers, err := d.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		return fmt.Errorf("cannot list containers: %w", err)
	}
	used := map[string]bool{}
	for _, c := range containers.Items {
		used[c.ImageID] = true
	}

	var errs []error
	for _, repo := range repos {
		var candidates []image.Summary
		for _, img := range images.Items {
			if used[img.ID] || !belongsTo(img, repo) {
				continue
			}
			candidates = append(candidates, img)
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].Created > candidates[j].Created })
		if len(candidates) <= keep {
			log.Info("nothing to remove", "repository", repo, "unused", len(candidates))
			continue
		}
		for _, img := range candidates[keep:] {
			_, err := d.ImageRemove(ctx, img.ID, client.ImageRemoveOptions{PruneChildren: true})
			switch {
			case err == nil:
				log.Info("image removed", "repository", repo, "id", notify.ShortDigest(img.ID))
			case isConflict(err):
				log.Info("image still referenced, kept", "repository", repo, "id", notify.ShortDigest(img.ID))
			default:
				log.Error("cannot remove image", "repository", repo, "id", notify.ShortDigest(img.ID), "error", err)
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// belongsTo reports whether one of the image's tags or digests is in repo.
func belongsTo(img image.Summary, repo string) bool {
	for _, ref := range append(append([]string{}, img.RepoTags...), img.RepoDigests...) {
		if ref == "<none>:<none>" || ref == "<none>@<none>" {
			continue
		}
		r, err := name.ParseReference(ref)
		if err != nil {
			continue
		}
		if r.Context().Name() == repo {
			return true
		}
	}
	return false
}

func isConflict(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "conflict") || strings.Contains(msg, "is being used") ||
		strings.Contains(msg, "must be forced") || strings.Contains(msg, "no such image")
}
