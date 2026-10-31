package updater

import (
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"

	"github.com/xaoimoon/idunn/internal/cluster"
	"github.com/xaoimoon/idunn/internal/config"
	"github.com/xaoimoon/idunn/internal/engine"
)

func TestTrackedContainers(t *testing.T) {
	mk := func(id, name string, labels map[string]string) *engine.Container {
		return engine.New(container.InspectResponse{ID: id, Name: "/" + name, Config: &container.Config{Labels: labels}})
	}
	on := map[string]string{"idunn.enable": "true"}
	all := []*engine.Container{
		mk("1", "web", on),
		mk("2", "db", nil),
		mk("3", "cache", map[string]string{"idunn.enable": "false"}),
		mk("4", "svc.1.abc", map[string]string{"idunn.enable": "true", "com.docker.swarm.task.id": "t1"}),
		mk("5", "idunn", on),
		mk("6", "web"+engine.OldSuffix, on),
		mk("7", "api", on),
	}
	u := &Updater{
		Config: &config.Config{
			LabelFilter: config.Label{Key: "idunn.enable", Value: "true"},
			LabelIgnore: config.Label{Key: "idunn.enable", Value: "false"},
		},
		Self: cluster.Self{ContainerID: "5"},
	}
	var names []string
	for _, c := range u.trackedContainers(all) {
		names = append(names, c.Name)
	}
	if got := strings.Join(names, " "); got != "web api" {
		t.Errorf("tracked = %q, want opted-in containers only, without Swarm tasks, Iðunn or old containers", got)
	}
}
