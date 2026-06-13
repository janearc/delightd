package metrics

import (
	"fmt"
	"net/http"
	"sync"
)

// Registry holds all internal telemetry counters for delightd
type Registry struct {
	mu       sync.RWMutex
	counters map[string]int64
}

var globalRegistry = &Registry{
	counters: make(map[string]int64),
}

// Inc increments a counter by 1. The key should follow prometheus naming (e.g. delightd_backups_total)
// Labels can be appended to the key if needed: delightd_backups_total{project="odysseus"}
func Inc(key string) {
	globalRegistry.mu.Lock()
	defer globalRegistry.mu.Unlock()
	globalRegistry.counters[key]++
}

// Handler returns an http.HandlerFunc that dumps the metrics in Prometheus exposition format
func Handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		globalRegistry.mu.RLock()
		defer globalRegistry.mu.RUnlock()

		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		for key, val := range globalRegistry.counters {
			fmt.Fprintf(w, "%s %d\n", key, val)
		}
	}
}

// Reset clears the registry (useful for testing)
func Reset() {
	globalRegistry.mu.Lock()
	defer globalRegistry.mu.Unlock()
	globalRegistry.counters = make(map[string]int64)
}
