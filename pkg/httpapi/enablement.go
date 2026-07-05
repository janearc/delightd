package httpapi

import (
	"encoding/json"
	"net/http"

	"delightd/pkg/enablement"
)

// Enablement routes: the fleet-wide enable/disable state home.
//
// Shape rule, stated once: the handle* methods have net/http's handler
// signature (w, r) and no Go return -- the JSON written to w IS the return
// value, exactly like every other handler in this package (what a PUT
// returns is the stored record, on the wire, as GET would render it).
// Everything else in this file is a pure helper: values in, values out,
// never a write to w. Every write to the wire happens inside a handler,
// where it is visible.

// enablementStore is the slice of pkg/enablement the handlers use, injectable
// so they can be tested with an in-memory fake.
type enablementStore interface {
	Get(project string) (enablement.Record, bool, error)
	Put(rec enablement.Record) error
}

// UseEnablement wires the state home. Until it is wired, the /state routes
// serve 503 degraded -- fail closed.
func (s *Server) UseEnablement(st enablementStore) { s.enablement = st }

// degradedBody is the fail-closed 503 payload served while no store is wired.
var degradedBody = map[string]any{
	"degraded": true,
	"error":    "enablement store unavailable; reads fail closed",
}

// stateResponse is one project's effective enablement as the wire renders it:
// an absent record reads disabled with recorded=false, so a reader always
// sees the doctrine applied, never a hole.
type stateResponse struct {
	Project   string `json:"project"`
	State     string `json:"state"`
	Recorded  bool   `json:"recorded"`
	Reason    string `json:"reason,omitempty"`
	Actor     string `json:"actor,omitempty"`
	ChangedAt string `json:"changed_at,omitempty"`
}

// renderState is pure: record in, wire shape out.
func renderState(project string, rec enablement.Record, found bool) stateResponse {
	if !found {
		return stateResponse{Project: project, State: enablement.StateDisabled, Recorded: false}
	}
	return stateResponse{
		Project: project, State: rec.State, Recorded: true,
		Reason: rec.Reason, Actor: rec.Actor, ChangedAt: rec.ChangedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// knownProject reports whether name is in the roster. Enablement binds to
// managed projects; any other name is a 404, never a new key.
func (s *Server) knownProject(name string) bool {
	for _, p := range s.cfg.Projects {
		if p.Name == name {
			return true
		}
	}
	return false
}

// handleStateAll serves every managed project's effective enablement,
// roster-driven: a project with no record appears as disabled/unrecorded
// rather than being omitted.
func (s *Server) handleStateAll(w http.ResponseWriter, r *http.Request) {
	if s.enablement == nil {
		writeJSON(w, http.StatusServiceUnavailable, degradedBody)
		return
	}
	out := make([]stateResponse, 0, len(s.cfg.Projects))
	for _, p := range s.cfg.Projects {
		rec, found, err := s.enablement.Get(p.Name)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		out = append(out, renderState(p.Name, rec, found))
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": out})
}

func (s *Server) handleStateGet(w http.ResponseWriter, r *http.Request) {
	if s.enablement == nil {
		writeJSON(w, http.StatusServiceUnavailable, degradedBody)
		return
	}
	name := r.PathValue("name")
	if !s.knownProject(name) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "project not found"})
		return
	}
	rec, found, err := s.enablement.Get(name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, renderState(name, rec, found))
}

// handleStatePut is the idempotent write: the full desired state in the body,
// the project from the path, last write wins. The 200 body is the stored
// record as GET renders it; unknown projects and unknown states get an error
// status, never a coercion.
func (s *Server) handleStatePut(w http.ResponseWriter, r *http.Request) {
	if s.enablement == nil {
		writeJSON(w, http.StatusServiceUnavailable, degradedBody)
		return
	}
	name := r.PathValue("name")
	if !s.knownProject(name) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "project not found"})
		return
	}
	var body struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
		Actor  string `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "body does not parse: " + err.Error()})
		return
	}
	rec := enablement.Record{Project: name, State: body.State, Reason: body.Reason, Actor: body.Actor}
	if err := s.enablement.Put(rec); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	rec, found, err := s.enablement.Get(name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, renderState(name, rec, found))
}
