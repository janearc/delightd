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
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	registryv1 "delightd/gen/go/registry/v1"

	"delightd/config"
	"delightd/pkg/discovery"
	"delightd/pkg/gitstate"
	"delightd/pkg/introspect"
	"delightd/pkg/metrics"
	"delightd/pkg/registry"
	"delightd/pkg/schemaregistry"
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

	// reg is the live frood registry served by GET /registrations. It MAY be nil in
	// tests that do not exercise registration; handlers treat nil as an empty registry.
	reg *registry.Registry

	// subjects verifies contract subjects against the schema registry at join time, and
	// guaranteeHealthCheck runs the reachability guarantee on a joining endpoint. Both are
	// injectable so handleRegister can be tested without a live SR or network.
	subjects             subjectChecker
	guaranteeHealthCheck func(context.Context, *registryv1.Endpoint) error

	// events publishes the never-silent RegisterRefused event; eventsTopic and refusedSchema
	// are its destination and schema text. Wired by main via UseEvents; nil until then (a
	// refusal still returns its HTTP error and logs loudly).
	events        eventPublisher
	eventsTopic   string
	refusedSchema string

	// discover is the local-LLM discovery source, injectable so handlers can be
	// tested without probing the network.
	discover func(context.Context, *config.DelightConfig) []discovery.ModelSource
}

// eventPublisher is the subset of Big Little Mesh's emit.Publisher that handleRegister uses
// to put a RegisterRefused on the bus. Defined here so the handler can be tested with a fake.
type eventPublisher interface {
	Publish(ctx context.Context, topic, subject, schemaText, key string, msg proto.Message) error
}

// New constructs a Server wired to the live daemon dependencies. reg MAY be nil (the
// registry is additive; a nil registry serves an empty set). The schema-registry checker is
// built from config; the bus event publisher is wired separately via UseEvents.
func New(cfg *config.DelightConfig, machines map[string]*state.Machine, exports introspect.FragmentChecker, skillAgg *skills.Aggregator, dryRun bool, reg *registry.Registry) *Server {
	return &Server{
		cfg:      cfg,
		machines: machines,
		exports:  exports,
		skills:   skillAgg,
		dryRun:   dryRun,
		reg:      reg,
		// An unset SR URL yields a checker whose checks fail loudly rather than passing.
		subjects:             schemaregistry.New(cfg.System.Kafka.SchemaRegistryURL),
		guaranteeHealthCheck: guaranteeHealthCheck,
		discover:             discovery.DiscoverLocalLLMs,
	}
}

// UseEvents wires the bus publisher for the never-silent RegisterRefused event (delightd's
// emit.Publisher, the topic to publish on, and the schema text to register). Called by main
// after the publisher is built; without it, refusals are HTTP + log only.
func (s *Server) UseEvents(pub eventPublisher, topic, refusedSchema string) {
	s.events = pub
	s.eventsTopic = topic
	s.refusedSchema = refusedSchema
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

// rosterResponse is the GET /projects body: every managed project as a
// registry.v1.Project (protojson). This makes fleet membership a first-class,
// queryable, contract-typed surface rather than something inferred from GET /git.
// Each entry is a marshaled Project; the {status, projects[]} envelope is unchanged,
// and the roster fields fleet already reads (name/path/essential/deploy/remote_url)
// keep their prior JSON shape.
type rosterResponse struct {
	Status   string            `json:"status"`
	Projects []json.RawMessage `json:"projects"`
}

// registrationsResponse is the GET /registrations body: the live frood registrations in the
// same {status, items[]} envelope GET /projects uses, each item a protojson Registration.
type registrationsResponse struct {
	Status        string            `json:"status"`
	Registrations []json.RawMessage `json:"registrations"`
}

// rosterMarshal serves the roster wire. UseProtoNames keeps snake_case field names
// (name/path/essential/deploy/remote_url) byte-identical to the prior hand-written
// shape. EmitUnpopulated is deliberately left off, so the wire stays sparse
// (omitempty-equivalent); the one field that must always appear -- essential -- is
// modeled `optional` and always set, so it emits even when false without zero-filling
// every other field.
var rosterMarshal = protojson.MarshalOptions{UseProtoNames: true}

// projectKind maps the yaml kind string to the contract discriminator. Empty/absent is
// WATCHER -- the default that keeps every existing project unchanged -- and an
// unrecognized value also falls back to WATCHER rather than emitting UNSPECIFIED.
func projectKind(s string) registryv1.Kind {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "listener":
		return registryv1.Kind_KIND_LISTENER
	default:
		return registryv1.Kind_KIND_WATCHER
	}
}

// projectToProto maps a config project plus its live remote URL into the
// registry.v1.Project contract. The yaml config stays the loader; this is the seam
// toward fuller config->contract unification, not the unification itself. essential is
// always set (incl. false) so it always emits; remote_url is set only when resolved so
// it stays omitted otherwise; deploy is always present (an empty object for
// non-deployable projects), each matching the prior wire shape. RemoteURL is read
// per-request (cheap: repo config only, no worktree walk).
func projectToProto(p config.ProjectConfig, remoteURL string) *registryv1.Project {
	proj := &registryv1.Project{
		Name:      p.Name,
		Path:      p.Path,
		Essential: proto.Bool(p.Essential),
		Deploy: &registryv1.Deploy{
			Kind:       p.Deploy.Kind,
			Deployment: p.Deploy.Deployment,
			Command:    p.Deploy.Command,
		},
		Kind: projectKind(p.Kind),
	}
	if remoteURL != "" {
		proj.RemoteUrl = proto.String(remoteURL)
	}
	return proj
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
	mux.HandleFunc("GET /registrations", s.handleRegistrations)    // live frood registrations (registry.v1.RegistrationSet); additive, alongside the roster
	mux.HandleFunc("POST /register", s.handleRegister)             // a frood joins the live registry (additive, optional; not yet required)
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
	projects := make([]json.RawMessage, 0, len(s.cfg.Projects))
	for _, p := range s.cfg.Projects {
		raw, err := rosterMarshal.Marshal(projectToProto(p, gitstate.RemoteURL(p.Path)))
		if err != nil {
			// a project that cannot be marshaled is a server fault, not a partial roster:
			// fail the whole request loudly rather than serve a silently short list.
			slog.Error("failed to marshal project for roster", "project", p.Name, "error", err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to encode roster"})
			return
		}
		projects = append(projects, raw)
	}
	writeJSON(w, http.StatusOK, rosterResponse{Status: "ok", Projects: projects})
}

// handleRegistrations serves the live frood registry as a registry.v1.RegistrationSet
// (protojson, the contract type per RULE-3 -- no hand-rolled JSON). It is additive and sits
// alongside GET /projects: the roster is the static declared set, this is the live
// registered set. A nil registry (tests) serves an empty set.
func (s *Server) handleRegistrations(w http.ResponseWriter, r *http.Request) {
	set := &registryv1.RegistrationSet{}
	if s.reg != nil {
		set = s.reg.Set()
	}
	regs := make([]json.RawMessage, 0, len(set.GetRegistrations()))
	for _, reg := range set.GetRegistrations() {
		b, err := rosterMarshal.Marshal(reg)
		if err != nil {
			slog.Error("failed to marshal registration", "project", reg.GetProject(), "error", err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to encode registrations"})
			return
		}
		regs = append(regs, b)
	}
	writeJSON(w, http.StatusOK, registrationsResponse{Status: "ok", Registrations: regs})
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
