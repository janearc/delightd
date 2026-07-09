package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"delightd/pkg/furnish"
	"delightd/pkg/kube"
)

// Every furnish cluster op is deadline-bounded, the same philosophy as /readyz's 3s: no
// handler waits on a wedged apiserver forever, and a hit deadline is named -- operation,
// piece, bound -- never a silent hang. The values scale with the work, not the wish.
const (
	// furnishMutateTimeout bounds a whole up or down: one apiserver write per declared
	// object, each with its own 30s per-request floor (kube.RESTConfig). Generous so a
	// legitimate many-object piece converges; bounded so "forever" is not an outcome.
	furnishMutateTimeout = 60 * time.Second
	// furnishHealthTimeout bounds a whole health read: one Get per declared object,
	// possibly across every piece. Reads are cheap and health is polled, so it sits
	// tighter than the mutations but looser than the single-request /readyz probe.
	furnishHealthTimeout = 15 * time.Second
)

// writeFurnishError writes an op failure. A hit deadline is distinguished as 504 and the
// body names the operation, the piece, and the bound that fired -- a wedged apiserver
// must read as "timed out after X", never as an anonymous 500 (fail loud).
func writeFurnishError(w http.ResponseWriter, op, piece string, timeout time.Duration, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		writeJSON(w, http.StatusGatewayTimeout, map[string]any{
			"error": fmt.Sprintf("%s %s: deadline exceeded after %s waiting on the cluster: %v", op, piece, timeout, err),
		})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
}

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
	// Bounded read. A deadline that fires mid-read surfaces per-object as INDETERMINATE
	// (the taxonomy's "could not be read", with the cause in the reason) -- never RED,
	// because a timeout observes nothing, and never a hang.
	ctx, cancel := context.WithTimeout(r.Context(), furnishHealthTimeout)
	defer cancel()
	healthy := true
	results := map[string]any{}
	for _, p := range names {
		items, err := furnish.Health(ctx, c, filepath.Join(s.furnishKubeDir, p))
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
	ctx, cancel := context.WithTimeout(r.Context(), furnishMutateTimeout)
	defer cancel()
	if err := furnish.Up(ctx, c, dir); err != nil {
		writeFurnishError(w, "furnish.up", name, furnishMutateTimeout, err)
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
	ctx, cancel := context.WithTimeout(r.Context(), furnishMutateTimeout)
	defer cancel()
	if err := furnish.Down(ctx, c, dir); err != nil {
		writeFurnishError(w, "furnish.down", name, furnishMutateTimeout, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"command": "furnish.down", "piece": name, "removed": true})
}
