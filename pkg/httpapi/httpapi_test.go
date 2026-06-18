package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"delightd/config"
	"delightd/pkg/discovery"
	"delightd/pkg/skills"
	"delightd/pkg/state"
)

type fakeFragments struct{ has map[string]bool }

func (f fakeFragments) HasBashFragment(service string) bool { return f.has[service] }

// noDiscovery replaces the network-probing discovery source in tests.
func noDiscovery(context.Context, *config.DelightConfig) []discovery.ModelSource { return nil }

// errorBackoffMachine drives a machine into the error state, where it holds a
// future NextRetryAt and rejects a new backup trigger with a backoff error.
func errorBackoffMachine(t *testing.T, name string) *state.Machine {
	t.Helper()
	m := state.NewMachine(name)
	ctx := context.Background()
	if err := m.Transition(ctx, state.EventTriggerBackup); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if err := m.Transition(ctx, state.EventBackupFail); err != nil {
		t.Fatalf("fail: %v", err)
	}
	if got := m.GetState(); got != state.StateError {
		t.Fatalf("setup: state = %s, want error", got)
	}
	return m
}

func TestHandleHealth(t *testing.T) {
	cfg := &config.DelightConfig{Projects: []config.ProjectConfig{{Name: "a"}, {Name: "b"}}}
	s := New(cfg, nil, fakeFragments{}, nil, true)

	rr := httptest.NewRecorder()
	s.handleHealth(rr, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}
	if body := rr.Body.String(); !strings.Contains(body, `"active_projects":2`) || !strings.Contains(body, `"dry_run":true`) {
		t.Errorf("body = %q", body)
	}
}

func TestHandleDiscovery(t *testing.T) {
	s := New(&config.DelightConfig{}, nil, fakeFragments{}, nil, false)
	s.discover = noDiscovery

	rr := httptest.NewRecorder()
	s.handleDiscovery(rr, httptest.NewRequest(http.MethodGet, "/discovery/llms", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	var resp struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" {
		t.Errorf("status = %q, want ok", resp.Status)
	}
}

func TestHandleProjectState(t *testing.T) {
	machines := map[string]*state.Machine{"known": state.NewMachine("known")}
	s := New(&config.DelightConfig{}, machines, fakeFragments{}, nil, false)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects/known/state", nil)
	req.SetPathValue("name", "known")
	s.handleProjectState(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("known: code = %d, want 200", rr.Code)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/projects/ghost/state", nil)
	req.SetPathValue("name", "ghost")
	s.handleProjectState(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown: code = %d, want 404", rr.Code)
	}
}

func TestHandleBackup(t *testing.T) {
	machines := map[string]*state.Machine{
		"ready":   state.NewMachine("ready"),
		"backoff": errorBackoffMachine(t, "backoff"),
	}
	s := New(&config.DelightConfig{}, machines, fakeFragments{}, nil, false)

	// fallow machine accepts the trigger
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/projects/ready/backup", nil)
	req.SetPathValue("name", "ready")
	s.handleBackup(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("ready: code = %d, want 200", rr.Code)
	}

	// unknown machine -> 404
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/projects/ghost/backup", nil)
	req.SetPathValue("name", "ghost")
	s.handleBackup(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown: code = %d, want 404", rr.Code)
	}

	// machine in backoff rejects the trigger -> 409
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/projects/backoff/backup", nil)
	req.SetPathValue("name", "backoff")
	s.handleBackup(rr, req)
	if rr.Code != http.StatusConflict {
		t.Errorf("backoff: code = %d, want 409", rr.Code)
	}
}

func TestHandleReset(t *testing.T) {
	machines := map[string]*state.Machine{"stuck": errorBackoffMachine(t, "stuck")}
	s := New(&config.DelightConfig{}, machines, fakeFragments{}, nil, false)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/projects/stuck/reset", nil)
	req.SetPathValue("name", "stuck")
	s.handleReset(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("stuck: code = %d, want 200", rr.Code)
	}
	if got := machines["stuck"].GetState(); got != state.StateFallow {
		t.Errorf("after reset: state = %s, want fallow", got)
	}

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/projects/ghost/reset", nil)
	req.SetPathValue("name", "ghost")
	s.handleReset(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown: code = %d, want 404", rr.Code)
	}
}

func TestHandleGitAll(t *testing.T) {
	// A non-git temp dir exercises wiring + JSON shape without a fixture repo:
	// the per-repo read fails into Error, but the sweep still returns 200. Deep
	// git semantics are covered in pkg/gitstate.
	cfg := &config.DelightConfig{Projects: []config.ProjectConfig{{Name: "p", Path: t.TempDir()}}}
	s := New(cfg, nil, fakeFragments{}, nil, false)

	rr := httptest.NewRecorder()
	s.handleGitAll(rr, httptest.NewRequest(http.MethodGet, "/git", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	var resp struct {
		Status   string `json:"status"`
		Projects []struct {
			Name string `json:"name"`
			Git  struct {
				Error string `json:"error"`
			} `json:"git"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" || len(resp.Projects) != 1 || resp.Projects[0].Name != "p" {
		t.Errorf("unexpected body: %s", rr.Body.String())
	}
	if resp.Projects[0].Git.Error == "" {
		t.Errorf("non-git dir should surface a per-project error")
	}
}

func TestHandleProjectGit(t *testing.T) {
	cfg := &config.DelightConfig{Projects: []config.ProjectConfig{{Name: "known", Path: t.TempDir()}}}
	s := New(cfg, nil, fakeFragments{}, nil, false)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/projects/known/git", nil)
	req.SetPathValue("name", "known")
	s.handleProjectGit(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("known: code = %d, want 200", rr.Code)
	}

	// A project the daemon doesn't manage is a 404 -- unlike introspect, git
	// state is only meaningful for a configured project path.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/projects/ghost/git", nil)
	req.SetPathValue("name", "ghost")
	s.handleProjectGit(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown: code = %d, want 404", rr.Code)
	}
}

func TestMux_RoutingAndMCPGating(t *testing.T) {
	machines := map[string]*state.Machine{"p": state.NewMachine("p")}
	s := New(&config.DelightConfig{}, machines, fakeFragments{}, nil, false)
	s.discover = noDiscovery
	mux := s.Mux()

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/projects/p/introspect", http.StatusOK},
		{http.MethodGet, "/metrics", http.StatusOK},
		{http.MethodGet, "/health", http.StatusOK},
		{http.MethodPost, "/mcp", http.StatusNotFound}, // MCP disabled by default
	} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
		if rr.Code != tc.want {
			t.Errorf("%s %s: code = %d, want %d", tc.method, tc.path, rr.Code, tc.want)
		}
	}
}

func TestMux_MCPEnabled(t *testing.T) {
	cfg := &config.DelightConfig{}
	cfg.System.AgentSkills.Enabled = true
	cfg.System.AgentSkills.ExposeVia = []string{"mcp"}
	agg := skills.NewAggregator(t.TempDir())
	s := New(cfg, nil, fakeFragments{}, agg, false)
	mux := s.Mux()

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"method":"tools/list"}`)))
	if rr.Code == http.StatusNotFound {
		t.Errorf("POST /mcp should be routed when MCP is enabled, got 404")
	}
}
