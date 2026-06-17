// Package introspect answers service introspection queries against the daemon's
// live state. It composes the per-project backup state machine with the exports
// engine's view of generated shims to produce a ServiceBackupStatus.
//
// JSON field names mirror delight.v1.ServiceBackupStatus so the wire contract is
// stable when introspection graduates to Protobuf over Kafka.
package introspect

import (
	"encoding/json"
	"net/http"

	"delightd/pkg/state"
)

// ServiceBackupStatus is the daemon's view of a single service. Field names
// mirror delight.v1.ServiceBackupStatus.
type ServiceBackupStatus struct {
	ServiceName         string `json:"service_name"`
	IsKnownToDaemon     bool   `json:"is_known_to_daemon"`
	IsActivelyBackingUp bool   `json:"is_actively_backing_up"`
	HasBashFragment     bool   `json:"has_bash_fragment"`
}

// FragmentChecker reports whether the daemon manages a generated bash shim
// (docker wrapper) for a service. The exports engine satisfies this interface.
type FragmentChecker interface {
	HasBashFragment(service string) bool
}

// ServiceStatus assembles the introspection view for a single service. An
// unknown service is reported with IsKnownToDaemon=false rather than treated as
// an error: "the daemon has never heard of this service" is a valid, queryable
// answer, not a 404.
//
// The machines map is built once at daemon startup and never mutated
// afterwards, so reads here are safe without the caller's lock.
func ServiceStatus(name string, machines map[string]*state.Machine, fc FragmentChecker) ServiceBackupStatus {
	status := ServiceBackupStatus{
		ServiceName:     name,
		HasBashFragment: fc.HasBashFragment(name),
	}

	if m, ok := machines[name]; ok {
		status.IsKnownToDaemon = true
		status.IsActivelyBackingUp = m.GetState() == state.StateBackingUp
	}

	return status
}

// Handler serves GET /projects/{name}/introspect. Unknown services return 200
// with is_known_to_daemon=false; introspection deliberately never 404s.
func Handler(machines map[string]*state.Machine, fc FragmentChecker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ServiceStatus(name, machines, fc))
	}
}
