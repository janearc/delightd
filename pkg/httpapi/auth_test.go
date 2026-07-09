package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"delightd/config"
	"delightd/pkg/skills"
	"delightd/pkg/state"
)

const testToken = "s3cr3t-control-token"

// gatedServer builds a Server with the control token provisioned and MCP enabled, so every
// mutating route (including POST /mcp) is registered and gated. It carries one known project
// and its machine so a gated route reaches a real handler once authenticated.
func gatedServer(t *testing.T) *Server {
	t.Helper()
	cfg := &config.DelightConfig{Projects: []config.ProjectConfig{{Name: "known"}}}
	cfg.System.AgentSkills.Enabled = true
	cfg.System.AgentSkills.ExposeVia = []string{"mcp"}
	s := New(cfg, map[string]*state.Machine{"known": state.NewMachine("known")}, fakeFragments{}, skills.NewAggregator(t.TempDir()), false, nil)
	s.discover = noDiscovery
	s.UseControlToken([]byte(testToken))
	s.UseEnablement(fakeEnablement{})
	return s
}

// concretePath fills the {name}/{piece} placeholders so a routed request lands on a handler.
func concretePath(pattern string) (method, path string) {
	method, rest, _ := strings.Cut(pattern, " ")
	rest = strings.ReplaceAll(rest, "{name}", "known")
	rest = strings.ReplaceAll(rest, "{piece}", "delightd")
	return method, rest
}

func TestRequireBearer_MissingAndWrongAndRight(t *testing.T) {
	s := gatedServer(t)
	mux := s.Mux()

	// A mutating route with no Authorization header is 401 with a Bearer challenge.
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/projects/known/backup", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: code = %d, want 401", rr.Code)
	}
	if ch := rr.Header().Get("WWW-Authenticate"); ch != "Bearer" {
		t.Errorf("missing token: WWW-Authenticate = %q, want Bearer", ch)
	}

	// A wrong token (same length, different bytes -- exercises the constant-time compare) is 401.
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/projects/known/backup", nil)
	req.Header.Set("Authorization", "Bearer "+strings.Repeat("x", len(testToken)))
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: code = %d, want 401", rr.Code)
	}

	// A non-Bearer scheme is 401, never treated as anonymous-allowed.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/projects/known/backup", nil)
	req.Header.Set("Authorization", "Basic "+testToken)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("non-bearer scheme: code = %d, want 401", rr.Code)
	}

	// The right token reaches the handler (200 for a fallow machine), never 401.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/projects/known/backup", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("right token: code = %d, want 200", rr.Code)
	}
}

// TestRequireBearer_NotProvisioned: with no control token loaded, a gated route fails closed
// with 503 naming the problem, while reads and readiness stay open.
func TestRequireBearer_NotProvisioned(t *testing.T) {
	cfg := &config.DelightConfig{Projects: []config.ProjectConfig{{Name: "known"}}}
	s := New(cfg, map[string]*state.Machine{"known": state.NewMachine("known")}, fakeFragments{}, nil, false, nil)
	s.discover = noDiscovery
	// deliberately no UseControlToken
	mux := s.Mux()

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/projects/known/backup", nil)
	req.Header.Set("Authorization", "Bearer anything") // a token cannot help: none is provisioned
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("not provisioned: code = %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "control token not provisioned") {
		t.Errorf("not provisioned: body = %q, want it to name the problem", rr.Body.String())
	}

	// A read is unaffected by the missing token.
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("read with no token: code = %d, want 200", rr.Code)
	}
}

// TestGateCoversEveryMutatingRoute is the regression guard: it derives the route set from what
// Mux actually registered, and asserts EVERY write verb is gated (401 without a bearer) and
// EVERY read is open (never 401). A route added later is covered automatically -- it is gated
// by its method, and this table sees it because it reads s.routePatterns, not a hand-kept list.
func TestGateCoversEveryMutatingRoute(t *testing.T) {
	s := gatedServer(t)
	mux := s.Mux()

	if len(s.routePatterns) == 0 {
		t.Fatal("no routes registered; the table below would vacuously pass")
	}

	var sawMutating, sawRead bool
	for _, pattern := range s.routePatterns {
		method, path := concretePath(pattern)

		// Without a bearer.
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(method, path, strings.NewReader("{}")))

		if isMutatingPattern(pattern) {
			sawMutating = true
			if rr.Code != http.StatusUnauthorized {
				t.Errorf("%s: mutating route not gated (code = %d without a bearer, want 401)", pattern, rr.Code)
			}
			// With the right bearer it must NOT be 401 -- the gate opens.
			rr = httptest.NewRecorder()
			req := httptest.NewRequest(method, path, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer "+testToken)
			mux.ServeHTTP(rr, req)
			if rr.Code == http.StatusUnauthorized {
				t.Errorf("%s: correct bearer still rejected (401)", pattern)
			}
			continue
		}

		sawRead = true
		if rr.Code == http.StatusUnauthorized {
			t.Errorf("%s: read route must never be gated, got 401", pattern)
		}
	}
	if !sawMutating || !sawRead {
		t.Fatalf("table did not exercise both classes (mutating=%v, read=%v)", sawMutating, sawRead)
	}
}
