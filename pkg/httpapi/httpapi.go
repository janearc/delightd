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
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	registryv1 "delightd/gen/go/registry/v1"
	resolvev1 "delightd/gen/go/resolve/v1"

	"delightd/config"
	"delightd/pkg/discovery"
	"delightd/pkg/gitstate"
	"delightd/pkg/introspect"
	"delightd/pkg/kube"
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

	// enablement is the per-project enable/disable state home, wired by main via
	// UseEnablement. nil is NOT an empty answer here (contrast reg above): the
	// /state surface serves 503 degraded until a store is wired -- fail closed.
	enablement enablementStore

	// subjects verifies contract subjects against the schema registry at join time, and
	// guaranteeHealthCheck runs the reachability guarantee on a joining endpoint. Both are
	// injectable so handleRegister can be tested without a live SR or network.
	subjects             subjectChecker
	guaranteeHealthCheck func(context.Context, *registryv1.Endpoint) error

	// events publishes the never-silent NotRegistered event; eventsTopic and
	// notRegisteredSchema are its destination and schema text. Wired by main via UseEvents;
	// nil until then (the outcome still returns its HTTP error and logs loudly). emitWG tracks
	// the detached emit goroutines; DrainEvents waits on it, called by the shutdown path (and
	// the tests) so in-flight emits are not dropped.
	events              eventPublisher
	eventsTopic         string
	notRegisteredSchema string
	emitWG              sync.WaitGroup

	// discover is the local-LLM discovery source, injectable so handlers can be
	// tested without probing the network.
	discover func(context.Context, *config.DelightConfig) []discovery.ModelSource

	// clusterReady probes whether delightd can reach the apiserver of the cluster it
	// operates, for the GET /readyz readiness check. Injectable so readiness can be
	// tested without a live apiserver; New defaults it to a real client-go probe.
	clusterReady func(context.Context) error

	// furnishClient provides the client-go handle the /furnish routes use, built lazily
	// from the mounted kubeconfig and memoized on success (never on failure) so the
	// daemon starts without one and a late-mounted kubeconfig is picked up. nil until
	// UseFurnish; the routes fail loud 503 when it is nil or returns an error.
	// furnishKubeDir is the baked manifest root (a kube/ aggregator) the routes read.
	furnishClient  func() (*kube.Client, error)
	furnishKubeDir string

	// controlToken is the bearer the mutating routes and POST /mcp require, read once from
	// the creds mount at startup and wired via UseControlToken. Empty means not provisioned:
	// the gated routes fail closed with 503 while reads and readiness stay open (the daemon
	// still comes up -- availability mandate). It is never logged or otherwise emitted.
	controlToken []byte

	// routePatterns records every pattern Mux registers, in registration order, so a test can
	// assert the bearer gate covers exactly the mutating verbs and no later route can slip it.
	routePatterns []string

	// openMutations records the mutating patterns deliberately registered WITHOUT the bearer
	// gate (citizen rights, e.g. frood announce -- see registerCitizenRoute). A test pins this
	// list to exactly the ruled exemptions so it cannot silently grow.
	openMutations []string
}

// eventPublisher is the subset of Big Little Mesh's emit.Publisher that handleRegister uses
// to put a NotRegistered event on the bus. Defined here so the handler can be tested with a
// fake.
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
		clusterReady:         clusterReadyProbe,
	}
}

// UseEvents wires the bus publisher for the never-silent NotRegistered event (delightd's
// emit.Publisher, the topic to publish on, and the schema text to register). Called by main
// after the publisher is built; without it, a not-completed registration is HTTP + log only.
func (s *Server) UseEvents(pub eventPublisher, topic, notRegisteredSchema string) {
	s.events = pub
	s.eventsTopic = topic
	s.notRegisteredSchema = notRegisteredSchema
}

// UseFurnish wires the /furnish surface: the manifest root the routes read pieces from
// (the baked kube/ aggregator), and a lazy provider of the client-go handle furnish
// converges through. Called by main after config is loaded; without it the /furnish
// routes fail loud 503.
//
// It also repoints the GET /readyz apiserver check at the same provider, so readiness
// reuses furnish's shared, memoized *kube.Client (its discovery transport built once) rather
// than the New()-default clusterReadyProbe rebuilding a kubeconfig, discovery client, and TLS
// transport from scratch on every scrape. This does not weaken the late-mounted-kubeconfig
// property readyz relies on: client is the same LazyClient furnish uses, which memoizes only
// a success (see kube.LazyClient), so a kubeconfig that appears after startup is still picked
// up on the next /readyz poll, exactly as before.
func (s *Server) UseFurnish(kubeDir string, client func() (*kube.Client, error)) {
	s.furnishKubeDir = kubeDir
	s.furnishClient = client
	s.clusterReady = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		c, err := client()
		if err != nil {
			return err // not memoized on failure: a kubeconfig mounted later still resolves
		}
		return c.Ready(ctx)
	}
}

// UseControlToken wires the bearer the mutating routes and POST /mcp require. tok is the token
// read from the creds mount at startup; an empty tok means it was not provisioned, and the
// gated routes serve 503 until it is (reads and readiness stay open regardless). Called by main
// after config is loaded.
func (s *Server) UseControlToken(tok []byte) { s.controlToken = tok }

// DrainEvents blocks until the detached NotRegistered emit goroutines have finished. It is
// called on the shutdown path so an in-flight emit is not dropped when the daemon stops; each
// emit is self-bounded (a 2s context), so this returns promptly. The tests use it too.
func (s *Server) DrainEvents() { s.emitWG.Wait() }

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
// same {status, <plural>[]} envelope SHAPE GET /projects uses -- projects[] there,
// registrations[] here; no surface uses an items[] key -- each entry a protojson Registration.
type registrationsResponse struct {
	Status        string            `json:"status"`
	Registrations []json.RawMessage `json:"registrations"`
}

// servicesResponse is the GET /services body: every roster entry composed as a
// registry.v1.Service (protojson), in the same {status, <plural>[]} envelope shape the other
// roster surfaces use (services[] here). GET /services/{name} returns the bare composed entity instead (one thing, not
// a list), matching the entity-query shape in #42.
type servicesResponse struct {
	Status   string            `json:"status"`
	Services []json.RawMessage `json:"services"`
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

// Mux builds the control-port router with every route registered. Every route goes through
// s.register, which wraps a write verb (POST/PUT/PATCH/DELETE) in the bearer gate and leaves a
// read (GET) open. Gating is derived from the method, not a hand-kept list, so a route added
// later cannot dodge the gate: it is gated by virtue of being a mutation.
func (s *Server) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	s.routePatterns = nil

	s.register(mux, "GET /health", s.handleHealth)                      // liveness + active project count
	s.register(mux, "GET /readyz", s.handleReadyz)                      // provable readiness: roots mounted + apiserver reachable
	s.register(mux, "GET /metrics", metrics.Handler())                  // prometheus exposition
	s.register(mux, "GET /discovery/llms", s.handleDiscovery)           // currently discoverable local LLM endpoints
	s.register(mux, "GET /projects/{name}/state", s.handleProjectState) // backup state-machine diagnostics
	s.register(mux, "POST /projects/{name}/backup", s.handleBackup)     // manually trigger a checkpoint
	s.register(mux, "POST /projects/{name}/reset", s.handleReset)       // clear a stuck error state

	s.register(mux, "GET /projects", s.handleProjectsAll)        // authoritative roster (name/path/essential/deploy/remote_url) for all managed projects
	s.register(mux, "GET /registrations", s.handleRegistrations) // live frood registrations (registry.v1.RegistrationSet); additive, alongside the roster
	s.registerCitizenRoute(mux, "POST /register", s.handleRegister) // frood announce: open by ruling (see registerCitizenRoute)
	s.register(mux, "GET /state", s.handleStateAll)              // enablement home: every project's effective state (absent reads disabled)
	s.register(mux, "GET /state/{name}", s.handleStateGet)       // one project's enablement (NOT the backup diagnostics at /projects/{name}/state)
	s.register(mux, "PUT /state/{name}", s.handleStatePut)       // idempotent enable/disable; roster-bound, reason required on disable

	s.register(mux, "GET /git", s.handleGitAll)                     // live git state (branch/dirty/unpushed) for all managed projects
	s.register(mux, "GET /projects/{name}/git", s.handleProjectGit) // live git state for one managed project
	s.register(mux, "GET /resolve/{name}", s.handleResolve)         // narrow widget-facing resolution (resolve.v1.ResolvedService): scheme+address for one project

	// The composed entity-query surface (#42): ask delightd about one roster entry and get
	// it back with its facets (git/backup/reachable/endpoint) as fields, instead of pulling a
	// facet aggregate and reassembling delightd's internals. Additive -- /git, /projects, and
	// /registrations stay; internalizing those aggregates is a separate, later step of #42.
	s.register(mux, "GET /services", s.handleServicesAll)          // composed roster (entity-query list), optional ?type= filter
	s.register(mux, "GET /services/{name}", s.handleServiceByName) // one composed roster entry, facets as fields

	// Service introspection composes backup-state-machine status with the
	// exports engine's view of generated shims. Unknown services return 200
	// with is_known_to_daemon=false rather than 404; logic lives in pkg/introspect.
	s.register(mux, "GET /projects/{name}/introspect", introspect.Handler(s.machines, s.exports)) // is_known / backing_up / has_fragment

	// The furnish surface: delightd is the operator, so converging the meubilair pieces is an
	// operator API, not a host-run binary. The mutations (up/down) require the control-port
	// bearer; the health/pieces reads are open. What they converge to is the baked,
	// commit-stamped manifest tree, honoring the mutation-is-high-friction principle. health is
	// provable over the wire like /readyz: 200 all-GREEN, 503 if any piece is RED or INDETERMINATE.
	s.register(mux, "GET /furnish/pieces", s.handleFurnishPieces)         // declared pieces
	s.register(mux, "GET /furnish/health", s.handleFurnishHealth)         // ladder for all pieces
	s.register(mux, "GET /furnish/health/{piece}", s.handleFurnishHealth) // ladder for one piece
	s.register(mux, "POST /furnish/{piece}/up", s.handleFurnishUp)        // server-side apply (bearer-gated)
	s.register(mux, "POST /furnish/{piece}/down", s.handleFurnishDown)    // delete (absent is success; bearer-gated)

	if s.mcpEnabled() {
		s.register(mux, "POST /mcp", s.skills.HandleMCP) // agent skill aggregator (MCP; bearer-gated)
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

// readinessCheck is one named probe in the /readyz body: whether it passed and,
// when it did not, why. delightd is ready only when every check is ok.
type readinessCheck struct {
	Name  string `json:"name"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// readinessResponse is the GET /readyz body. Ready is the AND of every check.
type readinessResponse struct {
	Ready  bool             `json:"ready"`
	Checks []readinessCheck `json:"checks"`
}

// handleReadyz is the provable readiness probe: green only when delightd can
// actually do its job -- its operating roots are mounted and readable, and it reaches
// the apiserver of the cluster it operates. This is distinct from GET /health (liveness: the
// process is up and config loaded). A failing check returns 503 with the reason
// named, never a bare "not ready", so one HTTP answer serves a human via the wrapper,
// hm over the wire, and any service alike.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	checks := []readinessCheck{
		s.checkRootsReadable(),
		s.checkClusterReady(r.Context()),
	}
	ready := true
	for _, c := range checks {
		if !c.OK {
			ready = false
		}
	}
	code := http.StatusOK
	if !ready {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, readinessResponse{Ready: ready, Checks: checks})
}

// checkRootsReadable verifies delightd's operating roots exist and are readable. In
// a container these are the volume mounts, so a mount that failed to attach surfaces
// here as a red check instead of as silent misbehaviour downstream.
func (s *Server) checkRootsReadable() readinessCheck {
	// deterministic order so the first reported failure is stable across requests.
	roots := []struct{ name, path string }{
		{"monitor_root", s.cfg.System.MonitorRoot},
		{"daemon_root", s.cfg.System.DaemonRoot},
		{"config_root", s.cfg.System.ConfigRoot},
	}
	for _, root := range roots {
		if root.path == "" {
			return readinessCheck{Name: "roots_readable", OK: false, Error: root.name + " is unset"}
		}
		if _, err := os.ReadDir(root.path); err != nil {
			return readinessCheck{Name: "roots_readable", OK: false, Error: fmt.Sprintf("%s (%s): %v", root.name, root.path, err)}
		}
	}
	return readinessCheck{Name: "roots_readable", OK: true}
}

// checkClusterReady probes apiserver reachability. delightd operates the cluster, so a
// delightd that cannot reach the apiserver is not ready even though its process is up.
func (s *Server) checkClusterReady(ctx context.Context) readinessCheck {
	if err := s.clusterReady(ctx); err != nil {
		return readinessCheck{Name: "apiserver_reachable", OK: false, Error: err.Error()}
	}
	return readinessCheck{Name: "apiserver_reachable", OK: true}
}

// clusterReadyProbe is the production cluster probe: it pings the apiserver's own
// /readyz endpoint via client-go, bounded by a short timeout so a dead apiserver fails the
// check fast instead of hanging the request. The kubeconfig is resolved by the standard
// rules at call time, so startup never depends on one being present.
func clusterReadyProbe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return kube.APIServerReady(ctx, "")
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

// handleServicesAll serves the composed roster: every managed entry as a registry.v1.Service
// with its facets (git/backup/reachable/endpoint) as fields. An optional ?type= filter narrows
// the roster (today only `service` matches; `model` is a valid, currently-empty filter that
// #34 will populate). An unrecognized type is a 400 rather than a silent empty list -- delightd
// never-silent. The git facet is collected with the same concurrent, per-entry-deadline sweep
// GET /git uses, so one slow tree cannot starve the whole answer.
func (s *Server) handleServicesAll(w http.ResponseWriter, r *http.Request) {
	want := registryv1.ServiceType_SERVICE_TYPE_UNSPECIFIED // unset means "no filter"
	if raw := strings.TrimSpace(r.URL.Query().Get("type")); raw != "" {
		t, ok := parseServiceType(raw)
		if !ok {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: "unknown service type: " + raw})
			return
		}
		want = t
	}

	// One bounded, concurrent git sweep for the whole roster, then index by name so each entry
	// composes its own facet. logGitErrors honors gitstate's "the caller logs" contract.
	gits := gitstate.CollectAll(s.cfg.Projects)
	logGitErrors(gits)
	gitByName := make(map[string]gitstate.GitState, len(gits))
	for _, pg := range gits {
		gitByName[pg.Name] = pg.Git
	}

	services := make([]json.RawMessage, 0, len(s.cfg.Projects))
	for _, p := range s.cfg.Projects {
		svc := s.composeService(p, gitByName[p.Name])
		// An unset filter passes everything; otherwise only entries of the requested type.
		if want != registryv1.ServiceType_SERVICE_TYPE_UNSPECIFIED && svc.GetType() != want {
			continue
		}
		raw, err := rosterMarshal.Marshal(svc)
		if err != nil {
			// A entry that cannot be marshaled is a server fault, not a partial roster: fail
			// loudly rather than serve a silently short list.
			slog.Error("failed to marshal composed service", "service", p.Name, "error", err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to encode services"})
			return
		}
		services = append(services, raw)
	}
	writeJSON(w, http.StatusOK, servicesResponse{Status: "ok", Services: services})
}

// handleServiceByName serves one composed roster entry: the bare registry.v1.Service (not a
// list envelope), facets as fields. An entry delightd does not manage is a 404 -- the entity
// must be in the roster for its facets to mean anything.
func (s *Server) handleServiceByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, p := range s.cfg.Projects {
		if p.Name != name {
			continue
		}
		gs := gitstate.Collect(p.Path)
		logGitErrors([]gitstate.ProjectGit{{Name: p.Name, Git: gs}})
		raw, err := rosterMarshal.Marshal(s.composeService(p, gs))
		if err != nil {
			slog.Error("failed to marshal composed service", "service", p.Name, "error", err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to encode service"})
			return
		}
		// Write the composed entity directly (json.RawMessage round-trips its own bytes), so
		// the body is the single composed thing, not a one-element list.
		writeJSON(w, http.StatusOK, json.RawMessage(raw))
		return
	}
	writeJSON(w, http.StatusNotFound, errorResponse{Error: "service not found"})
}

// composeService builds the composed entity for one roster project: its declared identity plus
// the facets delightd holds today. git comes from the (already-collected) oracle read; backup
// from the backup state machine when one runs for the entry; endpoint and reachable from the
// live registration where the entry is registered. A facet delightd has no basis to answer is
// left absent rather than zero-filled, so a consumer can tell "unknown" from a real value. The
// model facet is not composed today -- model deployments are not yet roster entries (#34 folds
// them in and adds the field then).
func (s *Server) composeService(p config.ProjectConfig, gs gitstate.GitState) *registryv1.Service {
	svc := &registryv1.Service{
		Name: p.Name,
		Type: registryv1.ServiceType_SERVICE_TYPE_SERVICE,
		Git:  composeGitFacet(gs),
	}
	if m, ok := s.machines[p.Name]; ok && m != nil {
		svc.Backup = &registryv1.BackupFacet{State: string(m.GetState())}
	}
	// The endpoint and reachability facets come from the live registry: a present registration
	// means the entry is registered (endpoint known) and holds an unexpired lease, so reachable
	// is set true. That is a provisional read from the entry's last heartbeat, not a real-time
	// probe -- it can lag liveness by up to one heartbeat interval. No registration means
	// delightd does not know where it answers -- endpoint absent, reachable left unknown (which
	// is NOT the same as "unreachable").
	if s.reg != nil {
		if reg, ok := s.reg.Get(p.Name); ok {
			svc.Endpoint = reg.GetEndpoint()
			svc.Reachable = proto.Bool(true)
		}
	}
	return svc
}

// composeGitFacet projects the git oracle's full read down to the facet a consumer asks for.
// On a read failure clean is left ABSENT (not a misleading false-is-clean) and error carries
// why -- a fail-closed composition, so an unread tree never reports clean.
func composeGitFacet(gs gitstate.GitState) *registryv1.GitFacet {
	f := &registryv1.GitFacet{
		Branch:   gs.Branch,
		Unpushed: int32(gs.Unpushed),
		Error:    gs.Error,
	}
	if gs.Error == "" {
		f.Clean = proto.Bool(!gs.Dirty)
	}
	return f
}

// parseServiceType maps a ?type= query value to the contract discriminator. It accepts the
// friendly short forms a consumer writes (`service`, `model`), case-insensitively; ok is false
// for anything else so the handler can answer 400 rather than silently filter to nothing.
func parseServiceType(s string) (registryv1.ServiceType, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "service":
		return registryv1.ServiceType_SERVICE_TYPE_SERVICE, true
	case "model":
		return registryv1.ServiceType_SERVICE_TYPE_MODEL, true
	default:
		return registryv1.ServiceType_SERVICE_TYPE_UNSPECIFIED, false
	}
}

// handleResolve serves the narrow widget-facing resolution: given a project name, the one
// address to reach it as a resolve.v1.ResolvedService (protojson, the contract type per RULE-3).
// It is the size-constrained projection the tiny-monitor widget consumes -- where
// registry.v1.Service (#42) is the rich composed entity, this answers only "where does it
// answer". The answer is composed from the live registry: a name delightd holds a registration
// for resolves to that registration's endpoint. A resolution miss is ALWAYS 404, never 503: a
// name delightd cannot resolve -- no live registration, or (only in tests) no registry at all --
// is a not-found, which is NOT the same as "the service is down" and is NOT a server-degraded
// status. A widget that cannot reach delightd at all learns that from the transport failure, not
// from a body here.
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if s.reg == nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "service not resolvable"})
		return
	}
	reg, ok := s.reg.Get(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: "service not resolvable"})
		return
	}
	ep := reg.GetEndpoint()
	resolved := &resolvev1.ResolvedService{
		Name:    name,
		Scheme:  ep.GetScheme(),
		Address: ep.GetAddress(),
	}
	raw, err := rosterMarshal.Marshal(resolved)
	if err != nil {
		slog.Error("failed to marshal resolved service", "project", name, "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to encode resolution"})
		return
	}
	// Write the resolved entity directly (json.RawMessage round-trips its own bytes) so the body
	// is the bare resolve.v1.ResolvedService the widget's generated crate deserializes.
	writeJSON(w, http.StatusOK, json.RawMessage(raw))
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
