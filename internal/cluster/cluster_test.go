package cluster

import (
	"testing"

	"github.com/moby/moby/api/types/swarm"
)

const id = "35deb63f76e6a1b2c3d4e5f60718293a4b5c6d7e8f9011223344556677889900"

func TestContainerIDFromMountinfo(t *testing.T) {
	mountinfo := `1450 1381 0:345 / / ro,relatime master:400 - overlay overlay rw
1460 1450 8:1 /var/lib/docker/containers/` + id + `/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/sda1 rw
1461 1450 8:1 /var/lib/docker/containers/` + id + `/hostname /etc/hostname rw,relatime - ext4 /dev/sda1 rw
1470 1450 0:350 / /var/run/docker.sock rw - tmpfs tmpfs rw`
	if got := containerIDFromMountinfo(mountinfo); got != id {
		t.Errorf("got %q", got)
	}
	if got := containerIDFromMountinfo("1 2 3:4 / / rw - ext4 /dev/sda1 rw"); got != "" {
		t.Errorf("not in a container: got %q", got)
	}
}

func TestUnavailable(t *testing.T) {
	nodes := []Node{
		{Name: "a", State: swarm.NodeStateReady, Availability: swarm.NodeAvailabilityActive},
		{Name: "b", State: swarm.NodeStateDown, Availability: swarm.NodeAvailabilityActive},
		{Name: "c", State: swarm.NodeStateReady, Availability: swarm.NodeAvailabilityPause},
	}
	got := Unavailable(nodes)
	if len(got) != 2 || got[0].String() != "b (down/active)" || got[1].String() != "c (ready/pause)" {
		t.Errorf("got %v", got)
	}
}
