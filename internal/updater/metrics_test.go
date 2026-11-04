package updater

import (
	"context"
	"strings"
	"testing"

	"github.com/xaoimoon/idunn/internal/metrics"
	"github.com/xaoimoon/idunn/internal/versions"
)

func render(t *testing.T, r *metrics.Registry) string {
	t.Helper()
	var b strings.Builder
	if err := r.Write(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestPassMetrics(t *testing.T) {
	h := newHarness(t)
	h.u.Metrics = metrics.New()
	h.cfg.MinAge = 1 << 62 // the second service waits forever
	h.docker.add("s1", "web", "nginx:1@"+d1, map[string]string{labelMinAge: "0"})
	h.docker.add("s2", "api", "api:1@"+d1, nil)
	h.checker.digests["nginx:1"] = d2
	h.checker.digests["api:1"] = d2

	h.pass()

	out := render(t, h.u.Metrics)
	for _, want := range []string{
		"idunn_leader 1\n",
		`idunn_passes_total{outcome="completed"} 1`,
		"idunn_services_tracked 2\n",
		"idunn_images_pending 1\n",
		`idunn_updates_total{outcome="updated",service="web"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestFollowerMetrics(t *testing.T) {
	h := newHarness(t)
	h.u.Metrics = metrics.New()
	h.docker.leader = false

	h.pass()

	out := render(t, h.u.Metrics)
	if !strings.Contains(out, "idunn_leader 0\n") || !strings.Contains(out, `idunn_passes_total{outcome="standby"} 1`) {
		t.Errorf("metrics:\n%s", out)
	}
}

func TestVersionCheckMetrics(t *testing.T) {
	h := newHarness(t)
	h.u.Metrics = metrics.New()
	h.cfg.VersionLevel = versions.Patch
	h.u.Lister = &fakeLister{calls: map[string]int{}, tags: map[string][]string{"mariadb": {"12.3.3", "13.0.2"}}}
	h.docker.add("db", "db_node1", "mariadb:12.3.3", nil)

	h.u.VersionCheck(context.Background())

	want := `idunn_new_version_available{bump="major",image="mariadb:12.3.3",latest="13.0.2",service="db_node1"} 1`
	if out := render(t, h.u.Metrics); !strings.Contains(out, want) {
		t.Errorf("missing %q in:\n%s", want, out)
	}

	// Once upgraded, the series disappears at the next check.
	h.docker.services["db"].Spec.TaskTemplate.ContainerSpec.Image = "mariadb:13.0.2"
	h.u.VersionCheck(context.Background())
	if out := render(t, h.u.Metrics); strings.Contains(out, "db_node1") {
		t.Errorf("stale series left:\n%s", out)
	}
}
