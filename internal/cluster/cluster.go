// Package cluster wraps the few Docker API calls Iðunn needs to reason about
// the Swarm: leadership, node availability and its own service.
package cluster

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/client"
)

// Docker is the subset of the Docker API used by Iðunn. *client.Client
// satisfies it; tests use a fake.
type Docker interface {
	Info(ctx context.Context, options client.InfoOptions) (client.SystemInfoResult, error)
	NodeInspect(ctx context.Context, nodeID string, options client.NodeInspectOptions) (client.NodeInspectResult, error)
	NodeList(ctx context.Context, options client.NodeListOptions) (client.NodeListResult, error)
	ServiceList(ctx context.Context, options client.ServiceListOptions) (client.ServiceListResult, error)
	ServiceInspect(ctx context.Context, serviceID string, options client.ServiceInspectOptions) (client.ServiceInspectResult, error)
	ServiceUpdate(ctx context.Context, serviceID string, options client.ServiceUpdateOptions) (client.ServiceUpdateResult, error)
	ServiceCreate(ctx context.Context, options client.ServiceCreateOptions) (client.ServiceCreateResult, error)
	ServiceRemove(ctx context.Context, serviceID string, options client.ServiceRemoveOptions) (client.ServiceRemoveResult, error)
	TaskList(ctx context.Context, options client.TaskListOptions) (client.TaskListResult, error)
	ContainerInspect(ctx context.Context, containerID string, options client.ContainerInspectOptions) (client.ContainerInspectResult, error)
}

// LocalNodeID returns the Swarm node ID of the engine Iðunn talks to, and
// fails if that engine is not a Swarm manager.
func LocalNodeID(ctx context.Context, d Docker) (string, error) {
	res, err := d.Info(ctx, client.InfoOptions{})
	if err != nil {
		return "", err
	}
	s := res.Info.Swarm
	if s.LocalNodeState != swarm.LocalNodeStateActive || s.NodeID == "" {
		return "", fmt.Errorf("this engine is not part of an active Swarm (state %q)", s.LocalNodeState)
	}
	if !s.ControlAvailable {
		return "", fmt.Errorf("node %s is not a Swarm manager", s.NodeID)
	}
	return s.NodeID, nil
}

// IsLeader reports whether nodeID is the current Raft leader.
func IsLeader(ctx context.Context, d Docker, nodeID string) (bool, error) {
	res, err := d.NodeInspect(ctx, nodeID, client.NodeInspectOptions{})
	if err != nil {
		return false, err
	}
	return res.Node.ManagerStatus != nil && res.Node.ManagerStatus.Leader, nil
}

// Node is a node's name and health, as reported by the managers.
type Node struct {
	ID           string
	Name         string
	State        swarm.NodeState
	Availability swarm.NodeAvailability
}

func (n Node) String() string {
	return fmt.Sprintf("%s (%s/%s)", n.Name, n.State, n.Availability)
}

// Ready reports whether the node can run tasks.
func (n Node) Ready() bool {
	return n.State == swarm.NodeStateReady && n.Availability == swarm.NodeAvailabilityActive
}

// Nodes lists the cluster nodes, sorted by name.
func Nodes(ctx context.Context, d Docker) ([]Node, error) {
	res, err := d.NodeList(ctx, client.NodeListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(res.Items))
	for _, n := range res.Items {
		out = append(out, Node{
			ID:           n.ID,
			Name:         n.Description.Hostname,
			State:        n.Status.State,
			Availability: n.Spec.Availability,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Unavailable returns the nodes that are not Ready and Active.
func Unavailable(nodes []Node) []Node {
	var out []Node
	for _, n := range nodes {
		if !n.Ready() {
			out = append(out, n)
		}
	}
	return out
}

// Self describes the container Iðunn runs in and, in Swarm, its service.
type Self struct {
	ServiceID   string
	Image       string // image reference of the running task, tag@digest
	User        string
	ContainerID string
	Name        string // container name, without the leading slash
}

// FindSelf identifies Iðunn's own service from its container. The container
// ID is read from /proc/self/mountinfo, which works whatever the hostname or
// network mode; the hostname (the short ID by default) is the fallback.
func FindSelf(ctx context.Context, d Docker) (Self, error) {
	id := ""
	if b, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		id = containerIDFromMountinfo(string(b))
	}
	if id == "" {
		host, err := os.Hostname()
		if err != nil {
			return Self{}, err
		}
		id = host
	}
	res, err := d.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return Self{}, fmt.Errorf("cannot inspect own container %q: %w", id, err)
	}
	c := res.Container
	if c.Config == nil {
		return Self{ContainerID: c.ID, Name: strings.TrimPrefix(c.Name, "/")}, nil
	}
	return Self{
		ServiceID:   c.Config.Labels["com.docker.swarm.service.id"],
		Image:       c.Config.Image,
		User:        c.Config.User,
		ContainerID: c.ID,
		Name:        strings.TrimPrefix(c.Name, "/"),
	}, nil
}

// containerIDFromMountinfo finds the container ID in the source path of the
// files Docker bind-mounts into every container (/etc/hostname,
// /etc/resolv.conf), e.g. ".../docker/containers/<64 hex>/hostname".
func containerIDFromMountinfo(mountinfo string) string {
	const marker = "/containers/"
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		switch fields[4] {
		case "/etc/hostname", "/etc/resolv.conf", "/etc/hosts":
		default:
			continue
		}
		_, rest, ok := strings.Cut(fields[3], marker)
		if !ok {
			continue
		}
		id, _, _ := strings.Cut(rest, "/")
		if len(id) == 64 && isHex(id) {
			return id
		}
	}
	return ""
}

func isHex(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
