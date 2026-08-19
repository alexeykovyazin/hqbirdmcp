// Package selfobs is P5.5: in-process counters and a Prometheus-text dump.
package selfobs

import (
	"fmt"
	"strings"
	"sync"
)

type Registry struct {
	mu     sync.Mutex
	counts map[string]int
}

func New() *Registry { return &Registry{counts: map[string]int{}} }

func (r *Registry) Add(name string, n int) {
	r.mu.Lock()
	r.counts[name] += n
	r.mu.Unlock()
}

func (r *Registry) Set(name string, n int) {
	r.mu.Lock()
	r.counts[name] = n
	r.mu.Unlock()
}

func (r *Registry) Get(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[name]
}

// Prometheus dumps name→value as text. Cardinality is the map size (capped).
func (r *Registry) Prometheus() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	n := 0
	for k, v := range r.counts {
		if n >= 200 {
			break
		}
		fmt.Fprintf(&b, "%s %d\n", sanitize(k), v)
		n++
	}
	return b.String()
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return s
}
