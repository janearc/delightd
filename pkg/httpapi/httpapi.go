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
	"delightd/pkg/gitstate"
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
	// Degraded is true when the daemon came up but could not fully read its config
	// (a bad config file or rejected project entries); Warnings explains why. This
	// makes a fail-open startup visible instead of silent.
	Degraded bool     `json:"degraded"`
	Warnings []string `json:"warnings,omitempty"`
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

// gitStateResponse is the GET /git body: every managed project with its git
// state as an element. fleet-svc consumes this to gate destructive
// host-migration, so it is computed per-request rather than served from a cache.
type gitStateResponse struct {
	Status   string                `json:"status"`
	Projects []gitstate.ProjectGit `json:"projects"`
}

// rosterEntry is one project in the GET /projects listing: the roster fields
// delightd now owns (the seam in docs/fleet-and-delightd.md) plus the live remote
// URL. fleet-svc consumes this for its lifecycle, bootstrap, and tier-0
// classification in place of parsing WorkstationConfig.yaml. RemoteURL is read
// per-request (cheap: repo config only, no worktree walk) and omitted when no
// remote can be resolved.
type rosterEntry struct {
	Name      string              `json:"name"`
	Path      string              `json:"path"`
	Essential bool                `json:"essential"`
	Deploy    config.DeployConfig `json:"deploy"`
	RemoteURL string              `json:"remote_url,omitempty"`
}

// rosterResponse is the GET /projects body: every managed project with its
// roster fields. This makes fleet membership a first-class, queryable surface
// rather than something inferred from GET /git.
type rosterResponse struct {
	Status   string        `json:"status"`
	Projects []rosterEntry `json:"projects"`
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

	mux.HandleFunc("GET /projects", s.handleProjectsAll)           // authoritative roster (name/path/essential/deploy/remote_url) for all managed projects
	mux.HandleFunc("GET /git", s.handleGitAll)                     // live git state (branch/dirty/unpushed) for all managed projects
	mux.HandleFunc("GET /projects/{name}/git", s.handleProjectGit) // live git state for one managed project

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
	status := "ok"
	if s.cfg.Degraded {
		status = "degraded"
	}
	writeJSON(w, http.StatusOK, healthResponse{
		Status:         status,
		ActiveProjects: len(s.cfg.Projects),
		DryRun:         s.dryRun,
		Degraded:       s.cfg.Degraded,
		Warnings:       s.cfg.LoadWarnings,
	})
}

func (s *Server) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, discoveryResponse{
		Status:  "ok",
		Sources: s.discover(r.Context(), s.cfg),
	})
}

// handleProjectsAll serves the authoritative roster: every managed project with
// the fields fleet acts on (essential tier, deploy block) plus its live remote
// URL. The remote URL is the only per-request read and is cheap (repo config
// only); the rest comes straight from the loaded config, so this is the
// membership query that GET /git only answered implicitly.
func (s *Server) handleProjectsAll(w http.ResponseWriter, r *http.Request) {
	projects := make([]rosterEntry, 0, len(s.cfg.Projects))
	for _, p := range s.cfg.Projects {
		projects = append(projects, rosterEntry{
			Name:      p.Name,
			Path:      p.Path,
			Essential: p.Essential,
			Deploy:    p.Deploy,
			RemoteURL: gitstate.RemoteURL(p.Path),
		})
	}
	writeJSON(w, http.StatusOK, rosterResponse{Status: "ok", Projects: projects})
}

func (s *Server) handleGitAll(w http.ResponseWriter, r *http.Request) {
	projects := gitstate.CollectAll(s.cfg.Projects)
	logGitErrors(projects)
	writeJSON(w, http.StatusOK, gitStateResponse{Status: "ok", Projects: projects})
}

func (s *Server) handleProjectGit(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, p := range s.cfg.Projects {
		if p.Name == name {
			pg := gitstate.ProjectGit{Name: p.Name, Git: gitstate.Collect(p.Path)}
			logGitErrors([]gitstate.ProjectGit{pg})
			writeJSON(w, http.StatusOK, pg)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, errorResponse{Error: "project not found"})
}

// logGitErrors emits a warning for each project whose git state could not be
// read. pkg/gitstate returns failures in-band and never logs; surfacing them
// here is the other half of that contract.
func logGitErrors(projects []gitstate.ProjectGit) {
	for _, p := range projects {
		if p.Git.Error != "" {
			slog.Warn("git state read failed", "project", p.Name, "error", p.Git.Error)
		}
	}
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
