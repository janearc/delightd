package httpapi

import (
	"fmt"
	"net/http"
	"path/filepath"

	"delightd/pkg/furnish"
	"delightd/pkg/kube"
)

// The /furnish handlers expose delightd's operator action -- converging the meubilair
// pieces -- over the control port, using the shared pkg/furnish operations. delightd holds
// one persistent cluster handle (the lazy furnishClient provider) rather than building a
// client per call. See Mux for the route table and the bearer-gated-mutation / baked-manifest
// rationale.

// furnishCluster resolves the client-go handle, or writes a 503 and returns false. The
// daemon operates the cluster; if it cannot reach a kubeconfig it fails loud rather than
// presenting an uncertain answer.
func (s *Server) furnishCluster(w http.ResponseWriter) (*kube.Client, bool) {
	if s.furnishClient == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "furnish not wired: no cluster client"})
		return nil, false
	}
	c, err := s.furnishClient()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "cluster unavailable: " + err.Error()})
		return nil, false
	}
	return c, true
}

// furnishPiece resolves a declared piece name to its directory, or writes an error (404 if
// undeclared, 500 if the aggregator itself cannot be read) and returns false. Resolving
// against the declaration mirrors the CLI: an undeclared name never reaches the cluster.
func (s *Server) furnishPiece(w http.ResponseWriter, name string) (string, bool) {
	pieces, err := furnish.Pieces(s.furnishKubeDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return "", false
	}
	for _, p := range pieces {
		if p == name {
			return filepath.Join(s.furnishKubeDir, name), true
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("unknown piece %q", name)})
	return "", false
}

// handleFurnishPieces lists the declared pieces from the baked manifest root.
func (s *Server) handleFurnishPieces(w http.ResponseWriter, r *http.Request) {
	pieces, err := furnish.Pieces(s.furnishKubeDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"command": "furnish.list", "kube": s.furnishKubeDir, "pieces": pieces})
}

// handleFurnishHealth reports the health ladder for one piece (GET /furnish/health/{piece})
// or all pieces (GET /furnish/health). Provable over the wire like /readyz: 200 when every
// piece is GREEN, 503 when any object is RED or INDETERMINATE, body carries the ladders.
func (s *Server) handleFurnishHealth(w http.ResponseWriter, r *http.Request) {
	var names []string
	if p := r.PathValue("piece"); p != "" {
		if _, ok := s.furnishPiece(w, p); !ok {
			return
		}
		names = []string{p}
	} else {
		all, err := furnish.Pieces(s.furnishKubeDir)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		names = all
	}
	c, ok := s.furnishCluster(w)
	if !ok {
		return
	}
	healthy := true
	results := map[string]any{}
	for _, p := range names {
		items, err := furnish.Health(r.Context(), c, filepath.Join(s.furnishKubeDir, p))
		if err != nil {
			// A piece that will not render is a hard error -- nothing to report against.
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		ok, ladder := furnish.PieceHealth(items)
		if !ok {
			healthy = false
		}
		results[p] = ladder
	}
	code := http.StatusOK
	if !healthy {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"command": "furnish.health", "healthy": healthy, "results": results})
}

// handleFurnishUp converges one piece via server-side apply.
func (s *Server) handleFurnishUp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("piece")
	dir, ok := s.furnishPiece(w, name)
	if !ok {
		return
	}
	c, ok := s.furnishCluster(w)
	if !ok {
		return
	}
	if err := furnish.Up(r.Context(), c, dir); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"command": "furnish.up", "piece": name, "applied": true})
}

// handleFurnishDown removes one piece's objects (an already-absent object is success).
func (s *Server) handleFurnishDown(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("piece")
	dir, ok := s.furnishPiece(w, name)
	if !ok {
		return
	}
	c, ok := s.furnishCluster(w)
	if !ok {
		return
	}
	if err := furnish.Down(r.Context(), c, dir); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"command": "furnish.down", "piece": name, "removed": true})
}
