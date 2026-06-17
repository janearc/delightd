// Package httpapi implements delightd's control-port HTTP surface. Handlers are
// constructed against explicit dependencies so they can be unit-tested in
// isolation, rather than living as closures inside main().
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"

	"delightd/config"
	"delightd/pkg/discovery"
	"delightd/pkg/introspect"
	"delightd/pkg/metrics"
	"delightd/pkg/skills"
	"delightd/pkg/state"
)

// Server holds the daemon state the control-port handlers read from. The
// machines map is built once at daemon startup and never mutated afterwards, so
// handlers read it without a lock; per-machine state is guarded internally.
type Server struct {
	cfg      *config.DelightConfig
	machines map[string]*state.Machine
	exports  introspect.FragmentChecker
	skills   *skills.Aggregator
	dryRun   bool

	// discover is the local-LLM discovery source, injectable so handlers can be
	// tested without probing the network.
	discover func(context.Context, *config.DelightConfig) []discovery.ModelSource
}

// New constructs a Server wired to the live daemon dependencies.
func New(cfg *config.DelightConfig, machines map[string]*state.Machine, exports introspect.FragmentChecker, skillAgg *skills.Aggregator, dryRun bool) *Server {
	return &Server{
		cfg:      cfg,
		machines: machines,
		exports:  exports,
		skills:   skillAgg,
		dryRun:   dryRun,
		discover: discovery.DiscoverLocalLLMs,
	}
}

// healthResponse is the GET /health body.
type healthResponse struct {
	Status         string `json:"status"`
	ActiveProjects int    `json:"active_projects"`
	DryRun         bool   `json:"dry_run"`
}

// discoveryResponse is the GET /discovery/llms body.
type discoveryResponse struct {
	Status  string                  `json:"status"`
	Sources []discovery.ModelSource `json:"sources"`
}

// projectActionResponse is returned by the backup/reset control endpoints.
type projectActionResponse struct {
	Status  string `json:"status"`
	Project string `json:"project"`
}

// errorResponse is the body for any non-2xx control-port reply.
type errorResponse struct {
	Error string `json:"error"`
}

// Mux builds the control-port router with every route registered.
func (s *Server) Mux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.handleHealth)                      // liveness + active project count
	mux.HandleFunc("GET /metrics", metrics.Handler())                  // prometheus exposition
	mux.HandleFunc("GET /discovery/llms", s.handleDiscovery)           // currently discoverable local LLM endpoints
	mux.HandleFunc("GET /projects/{name}/state", s.handleProjectState) // backup state-machine diagnostics
	mux.HandleFunc("POST /projects/{name}/backup", s.handleBackup)     // manually trigger a checkpoint
	mux.HandleFunc("POST /projects/{name}/reset", s.handleReset)       // clear a stuck error state

	// Service introspection composes backup-state-machine status with the
	// exports engine's view of generated shims. Unknown services return 200
	// with is_known_to_daemon=false rather than 404; logic lives in pkg/introspect.
	mux.HandleFunc("GET /projects/{name}/introspect", introspect.Handler(s.machines, s.exports)) // is_known / backing_up / has_fragment

	if s.mcpEnabled() {
		mux.HandleFunc("POST /mcp", s.skills.HandleMCP) // agent skill aggregator (MCP)
		slog.Info("MCP server successfully exposed", "route", "POST /mcp")
	}

	return mux
}

// writeJSON encodes payload as the JSON response body with the given status.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		slog.Error("failed to encode control-port response", "error", err)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{
		Status:         "ok",
		ActiveProjects: len(s.cfg.Projects),
		DryRun:         s.dryRun,
	})
}

func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, discoveryResponse{
		Status:  "ok",
		Sources: s.discover(r.Context(), s.cfg),
	})
}

func (s *Server) handleProjectState(w http.ResponseWriter, r *http.Request) {
	machine, ok := s.machines[r.PathValue("name")]
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "project not found"})
		return
	}
	writeJSON(w, http.StatusOK, machine.GetDiagnostics())
}

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	machine, ok := s.machines[name]
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "project not found"})
		return
	}
	if err := machine.Transition(r.Context(), state.EventTriggerBackup); err != nil {
		writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, projectActionResponse{Status: "backup_triggered", Project: name})
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	machine, ok := s.machines[name]
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "project not found"})
		return
	}
	if err := machine.Transition(r.Context(), state.EventClearError); err != nil {
		writeJSON(w, http.StatusConflict, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, projectActionResponse{Status: "error_cleared", Project: name})
}

// mcpEnabled reports whether agent skills are exposed over MCP per config.
func (s *Server) mcpEnabled() bool {
	return s.cfg.System.AgentSkills.Enabled &&
		slices.Contains(s.cfg.System.AgentSkills.ExposeVia, "mcp")
}
