// Package httpapi implements delightd's control-port HTTP surface. Handlers are
// constructed against explicit dependencies so they can be unit-tested in
// isolation, rather than living as closures inside main().
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

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

// Mux builds the control-port router with every route registered.
func (s *Server) Mux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /metrics", metrics.Handler())
	mux.HandleFunc("GET /discovery/llms", s.handleDiscovery)
	mux.HandleFunc("GET /projects/{name}/state", s.handleProjectState)
	mux.HandleFunc("POST /projects/{name}/backup", s.handleBackup)
	mux.HandleFunc("POST /projects/{name}/reset", s.handleReset)

	// Service introspection composes backup-state-machine status with the
	// exports engine's view of generated shims. Unknown services return 200
	// with is_known_to_daemon=false rather than 404; logic lives in pkg/introspect.
	mux.HandleFunc("GET /projects/{name}/introspect", introspect.Handler(s.machines, s.exports))

	if s.mcpEnabled() {
		mux.HandleFunc("POST /mcp", s.skills.HandleMCP)
		slog.Info("MCP Server exposed on POST /mcp")
	}

	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `{"status":"ok", "active_projects":%d, "dry_run":%t}`, len(s.cfg.Projects), s.dryRun)
}

func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	sources := s.discover(r.Context(), s.cfg)
	w.Header().Set("Content-Type", "application/json")
	// response envelope: {"status":"ok","sources":[...discovered llm endpoints...]}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"sources": sources,
	})
}

func (s *Server) handleProjectState(w http.ResponseWriter, r *http.Request) {
	machine, ok := s.machines[r.PathValue("name")]
	if !ok {
		http.Error(w, `{"error":"project not found"}`, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(machine.GetDiagnostics())
}

func (s *Server) handleBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	machine, ok := s.machines[name]
	if !ok {
		http.Error(w, `{"error":"project not found"}`, http.StatusNotFound)
		return
	}
	if err := machine.Transition(r.Context(), state.EventTriggerBackup); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"backup_triggered", "project":"%s"}`, name)
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	machine, ok := s.machines[name]
	if !ok {
		http.Error(w, `{"error":"project not found"}`, http.StatusNotFound)
		return
	}
	if err := machine.Transition(r.Context(), state.EventClearError); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"%s"}`, err.Error()), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"error_cleared", "project":"%s"}`, name)
}

// mcpEnabled reports whether agent skills are exposed over MCP per config.
func (s *Server) mcpEnabled() bool {
	if !s.cfg.System.AgentSkills.Enabled {
		return false
	}
	for _, method := range s.cfg.System.AgentSkills.ExposeVia {
		if method == "mcp" {
			return true
		}
	}
	return false
}
