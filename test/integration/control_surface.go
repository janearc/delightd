//go:build integration

// verifyControlSurface drives delightd's documented control surface (docs/api.md) against
// a running daemon: one subtest per route. It takes the daemon's base URL and a
// surfaceExpect, so any deployment shape can reuse it; the container e2e (image_test.go)
// points it at the containerized daemon. Add a route to the docs, add its subtest here.
//
// It assumes the daemon runs the synthetic config this package builds: two managed
// projects -- "fake-clean" (essential, kube deploy block, clean tree) and "fake-dirty"
// (non-essential, one un-pushed commit, dirty tree). Whether the /mcp surface is on is
// per-deployment (surfaceExpect.skillsEnabled).
package integration

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// surfaceExpect carries the few facts that differ by deployment (the project paths as
// the daemon sees them: host temp dirs for the bare binary, /work/<name> in the
// container). Everything else about the two synthetic projects is fixed.
type surfaceExpect struct {
	cleanPath string // absolute path to fake-clean as the daemon sees it
	dirtyPath string // absolute path to fake-dirty
	// apiserverReachable is the expected /readyz apiserver_reachable result: true when a
	// cluster is in reach (bare binary with a kubeconfig), false in the scratch
	// container (no kubeconfig/route wired yet). roots_readable is asserted green always.
	apiserverReachable bool
	// wantModel, when set, is a model name a mocked LLM backend serves and delightd is
	// configured to discover -- /discovery/llms must report it. Empty skips the model
	// assertion (only status is checked).
	wantModel string
	// skillsEnabled is true when agent_skills + MCP are on: /mcp is registered and its
	// tools/list must include delightd's own tools. False asserts /mcp is 404 (skills off).
	skillsEnabled bool
}

// verifyControlSurface drives every route in docs/api.md against base and asserts each
// behaves as documented.
func verifyControlSurface(t *testing.T, base string, exp surfaceExpect) {
	t.Run("GET /health", func(t *testing.T) {
		var r struct {
			Status         string `json:"status"`
			ActiveProjects int    `json:"active_projects"`
			DryRun         bool   `json:"dry_run"`
		}
		mustGetJSON(t, base+"/health", http.StatusOK, &r)
		if r.Status != "ok" {
			t.Errorf("status = %q, want ok", r.Status)
		}
		if r.ActiveProjects != 2 {
			t.Errorf("active_projects = %d, want 2", r.ActiveProjects)
		}
	})

	t.Run("GET /readyz", func(t *testing.T) {
		// 200 when ready, 503 when a check is red; either way the body carries the checks.
		var r struct {
			Ready  bool `json:"ready"`
			Checks []struct {
				Name string `json:"name"`
				OK   bool   `json:"ok"`
			} `json:"checks"`
		}
		getJSONAnyStatus(t, base+"/readyz", &r)
		got := map[string]bool{}
		for _, c := range r.Checks {
			got[c.Name] = c.OK
		}
		if ok, present := got["roots_readable"]; !present || !ok {
			t.Errorf("roots_readable = %v (present=%v), want true", ok, present)
		}
		if ok, present := got["apiserver_reachable"]; !present || ok != exp.apiserverReachable {
			t.Errorf("apiserver_reachable = %v, want %v", ok, exp.apiserverReachable)
		}
	})

	t.Run("GET /metrics", func(t *testing.T) {
		// Prometheus text exposition. delightd's counters are per-project labelled and
		// only materialise once the eval loop has ticked, so a freshly-started container
		// may expose an empty body -- assert the endpoint is wired (200) and, when
		// present, that it is prometheus format.
		body, status := getRaw(t, base+"/metrics")
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200", status)
		}
		if body != "" && !strings.Contains(body, "#") {
			t.Errorf("non-empty body is not prometheus exposition: %.120q", body)
		}
	})

	t.Run("GET /discovery/llms", func(t *testing.T) {
		// The handler probes the configured LLM providers live on each request, so with a
		// mocked backend configured, discovery must report its model.
		var r struct {
			Status  string `json:"status"`
			Sources []struct {
				Provider string   `json:"provider"`
				Models   []string `json:"models"`
				Healthy  bool     `json:"healthy"`
			} `json:"sources"`
		}
		mustGetJSON(t, base+"/discovery/llms", http.StatusOK, &r)
		if r.Status != "ok" {
			t.Errorf("status = %q, want ok", r.Status)
		}
		if exp.wantModel != "" {
			found := false
			for _, s := range r.Sources {
				for _, m := range s.Models {
					if m == exp.wantModel {
						found = true
					}
				}
			}
			if !found {
				t.Errorf("/discovery/llms did not report mocked model %q: %+v", exp.wantModel, r.Sources)
			}
		}
	})

	t.Run("GET /projects", func(t *testing.T) {
		p := fetchRoster(t, base)
		if len(p) != 2 {
			t.Fatalf("roster has %d projects, want 2", len(p))
		}
		clean, dirty := p["fake-clean"], p["fake-dirty"]
		if !clean.Essential {
			t.Errorf("fake-clean essential = false, want true")
		}
		if clean.Deploy.Kind != "kube" || clean.Deploy.Deployment != "fake-clean-agg" {
			t.Errorf("fake-clean deploy block not carried: %+v", clean.Deploy)
		}
		if clean.Path != exp.cleanPath {
			t.Errorf("fake-clean path = %q, want %q", clean.Path, exp.cleanPath)
		}
		if dirty.Essential {
			t.Errorf("fake-dirty essential = true, want false")
		}
	})

	t.Run("GET /git", func(t *testing.T) {
		g := fetchGitAll(t, base)
		if len(g) != 2 {
			t.Fatalf("/git has %d projects, want 2", len(g))
		}
		if g["fake-clean"].Dirty {
			t.Errorf("fake-clean dirty, want clean")
		}
		if !g["fake-dirty"].Dirty {
			t.Errorf("fake-dirty clean, want dirty")
		}
		if g["fake-dirty"].Unpushed < 1 {
			t.Errorf("fake-dirty unpushed = %d, want >= 1", g["fake-dirty"].Unpushed)
		}
	})

	t.Run("GET /projects/{name}/git", func(t *testing.T) {
		var r struct {
			Name string `json:"name"`
			Git  struct {
				Dirty bool `json:"dirty"`
			} `json:"git"`
		}
		mustGetJSON(t, base+"/projects/fake-dirty/git", http.StatusOK, &r)
		if !r.Git.Dirty {
			t.Errorf("fake-dirty single git not dirty: %+v", r)
		}
	})

	t.Run("GET /projects/{name}/state", func(t *testing.T) {
		// Backup state-machine diagnostics: a known project answers 200 with a body.
		if _, status := getRaw(t, base+"/projects/fake-dirty/state"); status != http.StatusOK {
			t.Errorf("status = %d, want 200", status)
		}
	})

	t.Run("GET /projects/{name}/introspect", func(t *testing.T) {
		if _, status := getRaw(t, base+"/projects/fake-dirty/introspect"); status != http.StatusOK {
			t.Errorf("status = %d, want 200", status)
		}
	})

	t.Run("GET /state", func(t *testing.T) {
		if _, status := getRaw(t, base+"/state"); status != http.StatusOK {
			t.Errorf("status = %d, want 200", status)
		}
	})

	t.Run("GET /state/{name}", func(t *testing.T) {
		if _, status := getRaw(t, base+"/state/fake-clean"); status != http.StatusOK {
			t.Errorf("status = %d, want 200", status)
		}
	})

	t.Run("PUT /state/{name}", func(t *testing.T) {
		// Idempotent write: the full desired state in the body -- state + reason
		// (required on disable) + actor (required). The response is the stored record.
		body := `{"state":"disabled","reason":"integration test","actor":"itest"}`
		resp, status := putBody(t, base+"/state/fake-clean", body)
		if status != http.StatusOK {
			t.Fatalf("PUT /state/fake-clean = %d, want 200; body %.200q", status, resp)
		}
		if !strings.Contains(resp, "disabled") {
			t.Errorf("stored record does not reflect the disable: %.200q", resp)
		}
	})

	t.Run("GET /services", func(t *testing.T) {
		var r struct {
			Services []struct {
				Name string `json:"name"`
			} `json:"services"`
		}
		mustGetJSON(t, base+"/services", http.StatusOK, &r)
		names := map[string]bool{}
		for _, s := range r.Services {
			names[s.Name] = true
		}
		if !names["fake-clean"] || !names["fake-dirty"] {
			t.Errorf("composed roster missing a project: %+v", r.Services)
		}
	})

	t.Run("GET /services/{name}", func(t *testing.T) {
		if _, status := getRaw(t, base+"/services/fake-clean"); status != http.StatusOK {
			t.Errorf("status = %d, want 200", status)
		}
	})

	t.Run("GET /resolve/{name}", func(t *testing.T) {
		// Nothing is registered (no schema registry, so /register fail-closes), so
		// fake-clean has no live registration to resolve to. The documented answer for a
		// resolution miss is 404 "not resolvable", never 503.
		if _, status := getRaw(t, base+"/resolve/fake-clean"); status != http.StatusNotFound {
			t.Errorf("status = %d, want 404 (no live registration -> not resolvable)", status)
		}
	})

	t.Run("GET /registrations", func(t *testing.T) {
		if _, status := getRaw(t, base+"/registrations"); status != http.StatusOK {
			t.Errorf("status = %d, want 200", status)
		}
	})

	t.Run("POST /register (fail-closed without schema registry)", func(t *testing.T) {
		// No schema registry in the harness, so registration fail-closes -- the
		// documented behaviour (#88). Assert it does not falsely succeed.
		status := postBody(t, base+"/register", `{}`)
		if status == http.StatusOK {
			t.Errorf("POST /register = 200 with no schema registry; want a fail-closed error status")
		}
	})

	t.Run("POST /projects/{name}/reset", func(t *testing.T) {
		if status := postBody(t, base+"/projects/fake-dirty/reset", ``); status != http.StatusOK {
			t.Errorf("POST /projects/fake-dirty/reset = %d, want 200", status)
		}
	})

	t.Run("POST /mcp (agent drives delightd's own tools)", func(t *testing.T) {
		if !exp.skillsEnabled {
			// skills off -> /mcp is not registered.
			if status := postBody(t, base+"/mcp", `{}`); status != http.StatusNotFound {
				t.Errorf("POST /mcp = %d, want 404 (route unregistered when skills disabled)", status)
			}
			return
		}
		// skills on: an agent enumerating a containerized delightd's tools must see
		// delightd's own operator surface (its baked mcp.json, loaded as self-tools).
		resp, err := http.Post(base+"/mcp", "application/json",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		if err != nil {
			t.Fatalf("POST /mcp: %v", err)
		}
		defer resp.Body.Close()
		var body struct {
			Result struct {
				Tools []struct {
					Name string `json:"name"`
				} `json:"tools"`
			} `json:"result"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode tools/list: %v", err)
		}
		names := map[string]bool{}
		for _, tool := range body.Result.Tools {
			names[tool.Name] = true
		}
		for _, want := range []string{"delightd_furnish_up", "delightd_furnish_health", "delightd_trigger_backup"} {
			if !names[want] {
				t.Errorf("tools/list missing %s -- an agent cannot drive delightd in the container: %v", want, names)
			}
		}
	})

	// POST /projects/{name}/backup last: it mutates state (writes a checkpoint) and the
	// caller waits on the .tgz, so keep the read assertions above it clean.
	t.Run("POST /projects/{name}/backup", func(t *testing.T) {
		if status := postBody(t, base+"/projects/fake-dirty/backup", ``); status != http.StatusOK {
			t.Fatalf("POST /projects/fake-dirty/backup = %d, want 200", status)
		}
	})
}

// --- small HTTP helpers, tolerant of non-200 where the API documents one ---

type rosterProject struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Essential bool   `json:"essential"`
	Deploy    struct {
		Kind       string `json:"kind"`
		Deployment string `json:"deployment"`
	} `json:"deploy"`
}

func fetchRoster(t *testing.T, base string) map[string]rosterProject {
	t.Helper()
	var r struct {
		Projects []rosterProject `json:"projects"`
	}
	mustGetJSON(t, base+"/projects", http.StatusOK, &r)
	m := map[string]rosterProject{}
	for _, p := range r.Projects {
		m[p.Name] = p
	}
	return m
}

type gitProject struct {
	Dirty    bool `json:"dirty"`
	Unpushed int  `json:"unpushed"`
}

func fetchGitAll(t *testing.T, base string) map[string]gitProject {
	t.Helper()
	var r struct {
		Projects []struct {
			Name string     `json:"name"`
			Git  gitProject `json:"git"`
		} `json:"projects"`
	}
	mustGetJSON(t, base+"/git", http.StatusOK, &r)
	m := map[string]gitProject{}
	for _, p := range r.Projects {
		m[p.Name] = p.Git
	}
	return m
}

func mustGetJSON(t *testing.T, url string, wantStatus int, v any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	if res.StatusCode != wantStatus {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("GET %s = %d, want %d; body %.200q", url, res.StatusCode, wantStatus, body)
	}
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func getJSONAnyStatus(t *testing.T, url string, v any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

func getRaw(t *testing.T, url string) (string, int) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return string(body), res.StatusCode
}

func putBody(t *testing.T, url, body string) (string, int) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT %s: %v", url, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b), res.StatusCode
}

func postBody(t *testing.T, url, body string) int {
	t.Helper()
	res, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer res.Body.Close()
	return res.StatusCode
}
