// Package engine updates standalone containers, outside Swarm: it recreates a
// container from its own configuration with the new image, checks that the
// new container is healthy and puts the old one back on failure.
package engine

import (
	"context"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// Docker is the subset of the Docker API used in engine mode. *client.Client
// satisfies it; tests use a fake.
type Docker interface {
	ContainerList(ctx context.Context, options client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerCreate(ctx context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerStart(ctx context.Context, containerID string, options client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerStop(ctx context.Context, containerID string, options client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(ctx context.Context, containerID string, options client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerRename(ctx context.Context, containerID string, options client.ContainerRenameOptions) (client.ContainerRenameResult, error)
	ImagePull(ctx context.Context, refStr string, options client.ImagePullOptions) (client.ImagePullResponse, error)
	ImageInspect(ctx context.Context, imageID string, inspectOpts ...client.ImageInspectOption) (client.ImageInspectResult, error)
	NetworkConnect(ctx context.Context, networkID string, options client.NetworkConnectOptions) (client.NetworkConnectResult, error)
}

// Labels read or set on containers.
const (
	// LabelDependsOn lists, comma-separated, the containers this one depends
	// on: it is recreated whenever one of them is.
	LabelDependsOn = "idunn.depends-on"
	// LabelReplaces is set by a self-update on the new Iðunn container: it
	// names the container it replaces, which it waits for before acting.
	LabelReplaces = "idunn.replaces"

	labelSwarmTask        = "com.docker.swarm.task.id"
	labelComposeProject   = "com.docker.compose.project"
	labelComposeService   = "com.docker.compose.service"
	labelComposeDependsOn = "com.docker.compose.depends_on"
	labelComposeImage     = "com.docker.compose.image"
)

// OldSuffix is appended to the name of a container while its replacement
// starts, until it is removed or renamed back.
const OldSuffix = "-idunn-old"

// Container is an inspected container.
type Container struct {
	ID   string
	Name string // without the leading slash
	Info container.InspectResponse
}

// New wraps an inspect result.
func New(info container.InspectResponse) *Container {
	return &Container{ID: info.ID, Name: strings.TrimPrefix(info.Name, "/"), Info: info}
}

// Labels returns the container labels, never nil.
func (c *Container) Labels() map[string]string {
	if c.Info.Config == nil || c.Info.Config.Labels == nil {
		return map[string]string{}
	}
	return c.Info.Config.Labels
}

// Image returns the image as given when the container was created, such as
// "nginx:1.27" or "nginx@sha256:...".
func (c *Container) Image() string {
	if c.Info.Config == nil {
		return ""
	}
	return c.Info.Config.Image
}

// ShortID returns the 12-character ID Docker uses as default hostname.
func (c *Container) ShortID() string {
	if len(c.ID) > 12 {
		return c.ID[:12]
	}
	return c.ID
}

// Running reports whether the container is running, not restarting.
func (c *Container) Running() bool {
	s := c.Info.State
	return s != nil && s.Running && !s.Restarting
}

// SwarmTask reports whether Swarm manages the container.
func (c *Container) SwarmTask() bool {
	_, ok := c.Labels()[labelSwarmTask]
	return ok
}

// Inspect returns the container with the given ID or name.
func Inspect(ctx context.Context, d Docker, id string) (*Container, error) {
	res, err := d.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	return New(res.Container), nil
}

// List returns the running containers, inspected and sorted by name. A
// container removed between the listing and its inspection is left out.
func List(ctx context.Context, d Docker) ([]*Container, error) {
	res, err := d.ContainerList(ctx, client.ContainerListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]*Container, 0, len(res.Items))
	for _, s := range res.Items {
		c, err := Inspect(ctx, d, s.ID)
		if err != nil {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Set indexes containers and the dependencies between them.
type Set struct {
	byName map[string]*Container
	byID   map[string]*Container
	deps   map[string][]string // container name -> names of the containers it depends on
}

// NewSet indexes all, typically every running container. With compose set,
// the Compose depends_on label counts as a dependency too.
func NewSet(all []*Container, compose bool) *Set {
	s := &Set{byName: map[string]*Container{}, byID: map[string]*Container{}, deps: map[string][]string{}}
	for _, c := range all {
		s.byName[c.Name] = c
		s.byID[c.ID] = c
	}
	for _, c := range all {
		s.deps[c.Name] = s.dependencies(c, all, compose)
	}
	return s
}

// DependsOn returns the names of the containers c depends on.
func (s *Set) DependsOn(name string) []string { return s.deps[name] }

// Dependents returns the names of the containers that depend directly on name.
func (s *Set) Dependents(name string) []string {
	var out []string
	for n, deps := range s.deps {
		for _, d := range deps {
			if d == name {
				out = append(out, n)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// Order sorts cs so that every container comes after the ones it depends on,
// by name otherwise. Containers in a dependency cycle keep the name order.
func (s *Set) Order(cs []*Container) []*Container {
	in := map[string]*Container{}
	for _, c := range cs {
		in[c.Name] = c
	}
	names := make([]string, 0, len(in))
	for n := range in {
		names = append(names, n)
	}
	sort.Strings(names)

	const (
		visiting = 1
		done     = 2
	)
	state := map[string]int{}
	out := make([]*Container, 0, len(cs))
	var visit func(n string)
	visit = func(n string) {
		if state[n] != 0 {
			return // done, or a cycle: left where the name order puts it
		}
		state[n] = visiting
		for _, d := range s.deps[n] {
			if _, ok := in[d]; ok {
				visit(d)
			}
		}
		state[n] = done
		out = append(out, in[n])
	}
	for _, n := range names {
		visit(n)
	}
	return out
}

// dependencies lists, deduplicated, the containers c depends on: the
// idunn.depends-on label, the Compose depends_on label when compose is set,
// legacy links, and the containers whose network, IPC or PID namespace or
// volumes c uses. The last ones always count: c cannot run without them.
func (s *Set) dependencies(c *Container, all []*Container, compose bool) []string {
	var out []string
	seen := map[string]bool{c.Name: true}
	add := func(ref string) {
		n := s.resolve(ref)
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	labels := c.Labels()
	for _, n := range strings.Split(labels[LabelDependsOn], ",") {
		add(n)
	}
	if compose {
		project := labels[labelComposeProject]
		for _, dep := range strings.Split(labels[labelComposeDependsOn], ",") {
			// "service:condition:restart"
			svc, _, _ := strings.Cut(strings.TrimSpace(dep), ":")
			if svc == "" || project == "" {
				continue
			}
			for _, o := range all {
				ol := o.Labels()
				if ol[labelComposeProject] == project && ol[labelComposeService] == svc {
					add(o.Name)
				}
			}
		}
	}
	if hc := c.Info.HostConfig; hc != nil {
		for _, l := range hc.Links {
			// "/db:/web/db" in inspect output, "db:alias" when created.
			n, _, _ := strings.Cut(l, ":")
			add(n)
		}
		if hc.NetworkMode.IsContainer() {
			add(hc.NetworkMode.ConnectedContainer())
		}
		if hc.IpcMode.IsContainer() {
			add(hc.IpcMode.Container())
		}
		if hc.PidMode.IsContainer() {
			add(hc.PidMode.Container())
		}
		for _, v := range hc.VolumesFrom {
			n, _, _ := strings.Cut(v, ":")
			add(n)
		}
	}
	return out
}

// resolve turns a container name or ID, full or abbreviated, into the name of
// a known container, "" if none matches.
func (s *Set) resolve(ref string) string {
	ref = strings.TrimPrefix(strings.TrimSpace(ref), "/")
	if ref == "" {
		return ""
	}
	if c, ok := s.byName[ref]; ok {
		return c.Name
	}
	if c, ok := s.byID[ref]; ok {
		return c.Name
	}
	if len(ref) >= 12 {
		for id, c := range s.byID {
			if strings.HasPrefix(id, ref) {
				return c.Name
			}
		}
	}
	return ""
}
