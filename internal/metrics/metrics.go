// Package metrics keeps Iðunn's counters and gauges and renders them in the
// Prometheus text exposition format. It has no dependency: the format is
// simple and the set of metrics small.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Labels are the label pairs of one series.
type Labels map[string]string

func (l Labels) key() string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escape(l[k]))
		b.WriteByte('"')
	}
	return b.String()
}

func escape(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`, `"`, `\"`).Replace(s)
}

type family struct {
	help, kind string
	series     map[string]float64 // label key -> value
}

// Registry holds every metric family.
type Registry struct {
	mu       sync.Mutex
	families map[string]*family
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{families: map[string]*family{}}
}

func (r *Registry) family(name, kind, help string) *family {
	f, ok := r.families[name]
	if !ok {
		f = &family{help: help, kind: kind, series: map[string]float64{}}
		r.families[name] = f
	}
	return f
}

// Add increments a counter.
func (r *Registry) Add(name, help string, l Labels, v float64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.family(name, "counter", help).series[l.key()] += v
}

// Set sets a gauge.
func (r *Registry) Set(name, help string, l Labels, v float64) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.family(name, "gauge", help).series[l.key()] = v
}

// Reset removes every series of a gauge family, before it is filled again
// with the current set (e.g. the newer versions found by the last check).
func (r *Registry) Reset(name, help string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.family(name, "gauge", help).series = map[string]float64{}
}

// Write renders the metrics, families and series sorted for stable output.
func (r *Registry) Write(w io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.families))
	for n := range r.families {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		f := r.families[n]
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", n, f.help, n, f.kind); err != nil {
			return err
		}
		keys := make([]string, 0, len(f.series))
		for k := range f.series {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := strconv.FormatFloat(f.series[k], 'f', -1, 64)
			var err error
			if k == "" {
				_, err = fmt.Fprintf(w, "%s %s\n", n, v)
			} else {
				_, err = fmt.Fprintf(w, "%s{%s} %s\n", n, k, v)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Handler serves the metrics.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = r.Write(w)
	})
}
