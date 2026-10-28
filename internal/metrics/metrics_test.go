package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWrite(t *testing.T) {
	r := New()
	r.Add("idunn_updates_total", "Updates.", Labels{"service": "web", "outcome": "updated"}, 1)
	r.Add("idunn_updates_total", "Updates.", Labels{"service": "web", "outcome": "updated"}, 1)
	r.Set("idunn_leader", "Leader.", nil, 1)
	r.Set("idunn_build_info", "Build.", Labels{"version": `v"1`}, 1)

	var b strings.Builder
	if err := r.Write(&b); err != nil {
		t.Fatal(err)
	}
	want := `# HELP idunn_build_info Build.
# TYPE idunn_build_info gauge
idunn_build_info{version="v\"1"} 1
# HELP idunn_leader Leader.
# TYPE idunn_leader gauge
idunn_leader 1
# HELP idunn_updates_total Updates.
# TYPE idunn_updates_total counter
idunn_updates_total{outcome="updated",service="web"} 2
`
	if b.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", b.String(), want)
	}
}

func TestResetAndHandler(t *testing.T) {
	r := New()
	r.Set("idunn_new_version_available", "New.", Labels{"service": "a"}, 1)
	r.Reset("idunn_new_version_available", "New.")
	r.Set("idunn_new_version_available", "New.", Labels{"service": "b"}, 1)

	rec := httptest.NewRecorder()
	r.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if strings.Contains(body, `service="a"`) || !strings.Contains(body, `service="b"`) {
		t.Errorf("body = %s", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain; version=0.0.4") {
		t.Errorf("content type = %s", ct)
	}

	var nilReg *Registry
	nilReg.Add("x", "x", nil, 1) // metrics disabled: no-op
}
