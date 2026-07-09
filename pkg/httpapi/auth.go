package httpapi

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// bearerPrefix is the scheme token an Authorization header must carry for a control-port
// mutation. The value after it is compared against the provisioned control token.
const bearerPrefix = "Bearer "

// register adds one route to the mux, wrapping it in the bearer gate when its pattern names a
// write verb (POST/PUT/PATCH/DELETE) and leaving a read (GET) open. The pattern is recorded so
// a test can assert the gate covers exactly the mutating routes. Deriving "mutating" from the
// method -- not a maintained allowlist -- means a route added later is gated by construction.
func (s *Server) register(mux *http.ServeMux, pattern string, h http.HandlerFunc) {
	s.routePatterns = append(s.routePatterns, pattern)
	if isMutatingPattern(pattern) {
		h = s.requireBearer(h)
	}
	mux.HandleFunc(pattern, h)
}

// isMutatingPattern reports whether a "METHOD /path" mux pattern names a state-changing verb.
// Reads (GET, and HEAD/OPTIONS were there any) are not mutations and stay open.
func isMutatingPattern(pattern string) bool {
	method, _, _ := strings.Cut(pattern, " ")
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

// requireBearer gates a mutating route behind the control-port bearer token. Reads and
// readiness are never wrapped with it, so liveness stays universally readable with no
// credential. When the token was not provisioned (missing or empty at startup) the route fails
// closed with 503 naming the problem, rather than accepting an unauthenticated mutation -- the
// daemon still came up (availability mandate). A missing or non-matching bearer is 401 with a
// WWW-Authenticate challenge. The compare is constant-time so a wrong token cannot be recovered
// byte by byte from response timing.
func (s *Server) requireBearer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.controlToken) == 0 {
			writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "control token not provisioned"})
			return
		}
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, bearerPrefix) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "missing or invalid bearer token"})
			return
		}
		presented := strings.TrimPrefix(auth, bearerPrefix)
		if subtle.ConstantTimeCompare([]byte(presented), s.controlToken) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(w, http.StatusUnauthorized, errorResponse{Error: "missing or invalid bearer token"})
			return
		}
		next(w, r)
	}
}
