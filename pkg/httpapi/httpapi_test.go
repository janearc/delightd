package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	registryv1 "delightd/gen/go/registry/v1"
	resolvev1 "delightd/gen/go/resolve/v1"

	"delightd/config"
	"delightd/pkg/discovery"
	"delightd/pkg/registry"
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
	s := New(cfg, nil, fakeFragments{}, nil, true, nil)

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
	s := New(&config.DelightConfig{}, nil, fakeFragments{}, nil, false, nil)
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
	s := New(&config.DelightConfig{}, machines, fakeFragments{}, nil, false, nil)

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
	s := New(&config.DelightConfig{}, machines, fakeFragments{}, nil, false, nil)

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
	s := New(&config.DelightConfig{}, machines, fakeFragments{}, nil, false, nil)

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
	s := New(cfg, nil, fakeFragments{}, nil, false, nil)

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

func TestHandleProjectsAll(t *testing.T) {
	// Two projects exercise the roster fields end-to-end: an essential workload
	// with a deploy block, and a non-essential CLI tool with none. A non-git temp
	// path means RemoteURL resolves empty (omitted), so the shape is exercised
	// without a fixture repo -- the cheap-remote read is covered in pkg/gitstate.
	cfg := &config.DelightConfig{Projects: []config.ProjectConfig{
		{
			Name:      "obs-svc",
			Path:      t.TempDir(),
			Essential: true,
			Deploy:    config.DeployConfig{Kind: "kube", Deployment: "obs-svc-agg"},
		},
		{Name: "taco", Path: t.TempDir(), Essential: false},
	}}
	s := New(cfg, nil, fakeFragments{}, nil, false, nil)

	rr := httptest.NewRecorder()
	s.handleProjectsAll(rr, httptest.NewRequest(http.MethodGet, "/projects", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q", ct)
	}

	var resp struct {
		Status   string `json:"status"`
		Projects []struct {
			Name      string `json:"name"`
			Path      string `json:"path"`
			Essential bool   `json:"essential"`
			Deploy    struct {
				Kind       string   `json:"kind"`
				Deployment string   `json:"deployment"`
				Command    []string `json:"command"`
			} `json:"deploy"`
			RemoteURL string `json:"remote_url"`
		} `json:"projects"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != "ok" || len(resp.Projects) != 2 {
		t.Fatalf("unexpected body: %s", rr.Body.String())
	}

	// roster order follows config order; assert the carried fields per project.
	if resp.Projects[0].Name != "obs-svc" || !resp.Projects[0].Essential {
		t.Errorf("first project: %+v", resp.Projects[0])
	}
	if resp.Projects[0].Deploy.Kind != "kube" || resp.Projects[0].Deploy.Deployment != "obs-svc-agg" {
		t.Errorf("deploy block not surfaced: %+v", resp.Projects[0].Deploy)
	}
	if resp.Projects[1].Name != "taco" || resp.Projects[1].Essential {
		t.Errorf("second project: %+v", resp.Projects[1])
	}
	// a non-git path yields no remote: remote_url is omitted (empty).
	if resp.Projects[1].RemoteURL != "" {
		t.Errorf("non-git path should resolve no remote, got %q", resp.Projects[1].RemoteURL)
	}
}

func TestRosterBackCompatShape(t *testing.T) {
	// The roster JSON for a watcher project must be byte-shape-unchanged for the fields
	// fleet already reads: name, path, essential, deploy, remote_url. This locks the
	// protojson serialization against the prior hand-written encoding/json shape. In
	// particular essential=false must still be emitted -- it would vanish under protojson's
	// default zero-omission, which is why essential is modeled `optional` and always set.

	// a deployable, essential watcher with a resolved remote (no kind -> watcher default):
	deployable := config.ProjectConfig{
		Name:      "alpha",
		Path:      "/work/alpha",
		Essential: true,
		Deploy:    config.DeployConfig{Kind: "kube", Deployment: "alpha", Command: []string{"run", "alpha"}},
	}
	raw, err := rosterMarshal.Marshal(projectToProto(deployable, "git@github.com:janearc/alpha.git"))
	if err != nil {
		t.Fatalf("marshal deployable: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode deployable: %v", err)
	}
	want := map[string]any{
		"name":       "alpha",
		"path":       "/work/alpha",
		"essential":  true,
		"deploy":     map[string]any{"kind": "kube", "deployment": "alpha", "command": []any{"run", "alpha"}},
		"remote_url": "git@github.com:janearc/alpha.git",
	}
	for k, v := range want {
		if !reflect.DeepEqual(got[k], v) {
			t.Errorf("field %q: got %#v, want %#v", k, got[k], v)
		}
	}
	// the new discriminator is additive and defaults to watcher.
	if got["kind"] != "KIND_WATCHER" {
		t.Errorf("kind: got %#v, want KIND_WATCHER", got["kind"])
	}

	// the critical edge: a non-essential, non-deployable watcher with no remote.
	// essential=false MUST be present (not omitted), deploy MUST be an empty object
	// (not null/absent), and remote_url MUST be omitted.
	bare := config.ProjectConfig{Name: "beta", Path: "/work/beta", Essential: false}
	raw, err = rosterMarshal.Marshal(projectToProto(bare, ""))
	if err != nil {
		t.Fatalf("marshal bare: %v", err)
	}
	got = nil
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode bare: %v", err)
	}
	if v, ok := got["essential"]; !ok || v != false {
		t.Errorf("essential must be present and false, got %#v (present=%v)", v, ok)
	}
	if v, ok := got["deploy"]; !ok || !reflect.DeepEqual(v, map[string]any{}) {
		t.Errorf("deploy must be an empty object, got %#v (present=%v)", v, ok)
	}
	if _, ok := got["remote_url"]; ok {
		t.Errorf("remote_url must be omitted when unresolved, got %#v", got["remote_url"])
	}
}

func TestHandleRegistrations(t *testing.T) {
	// A nil registry serves an empty RegistrationSet (the route is additive).
	s := New(&config.DelightConfig{}, nil, fakeFragments{}, nil, false, nil)
	rr := httptest.NewRecorder()
	s.handleRegistrations(rr, httptest.NewRequest(http.MethodGet, "/registrations", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("nil registry: code = %d, want 200", rr.Code)
	}
	var empty struct {
		Registrations []json.RawMessage `json:"registrations"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &empty); err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if len(empty.Registrations) != 0 {
		t.Fatalf("nil registry should serve empty set, got: %s", rr.Body.String())
	}

	// A populated registry serves the live registrations in the {status, registrations[]}
	// envelope, each a protojson Registration.
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry", "registry.db"), nil)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	defer reg.Close()
	if err := reg.Put(&registryv1.Registration{
		Project:  "paling",
		Endpoint: &registryv1.Endpoint{Scheme: "http", Address: "paling.fleet:8090"},
	}); err != nil {
		t.Fatal(err)
	}
	s2 := New(&config.DelightConfig{}, nil, fakeFragments{}, nil, false, reg)
	rr2 := httptest.NewRecorder()
	s2.handleRegistrations(rr2, httptest.NewRequest(http.MethodGet, "/registrations", nil))
	if rr2.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr2.Code)
	}
	body := rr2.Body.String()
	if !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("missing {status} envelope: %s", body)
	}
	if !strings.Contains(body, `"project":"paling"`) || !strings.Contains(body, `"address":"paling.fleet:8090"`) {
		t.Fatalf("registration not surfaced (proto field names expected): %s", body)
	}
}

func TestHandleProjectGit(t *testing.T) {
	cfg := &config.DelightConfig{Projects: []config.ProjectConfig{{Name: "known", Path: t.TempDir()}}}
	s := New(cfg, nil, fakeFragments{}, nil, false, nil)

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

// decodeServices unmarshals a {status, services[]} body into name-keyed composed entities.
func decodeServices(t *testing.T, body []byte) (string, map[string]map[string]any) {
	t.Helper()
	var resp struct {
		Status   string            `json:"status"`
		Services []json.RawMessage `json:"services"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode services envelope: %v", err)
	}
	byName := make(map[string]map[string]any, len(resp.Services))
	for _, raw := range resp.Services {
		var svc map[string]any
		if err := json.Unmarshal(raw, &svc); err != nil {
			t.Fatalf("decode service: %v", err)
		}
		name, _ := svc["name"].(string)
		byName[name] = svc
	}
	return resp.Status, byName
}

func TestHandleServiceByName(t *testing.T) {
	// paling: registered (so endpoint + reachable compose from the live registration) with a
	// backup machine; a non-git temp path so the git facet composes an error and clean is
	// ABSENT (fail-closed -- an unread tree never reports clean).
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry", "registry.db"), nil)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	defer reg.Close()
	if err := reg.Put(&registryv1.Registration{
		Project:  "paling",
		Endpoint: &registryv1.Endpoint{Scheme: "http", Address: "paling.fleet:8090"},
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.DelightConfig{Projects: []config.ProjectConfig{{Name: "paling", Path: t.TempDir()}}}
	machines := map[string]*state.Machine{"paling": state.NewMachine("paling")}
	s := New(cfg, machines, fakeFragments{}, nil, false, reg)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/services/paling", nil)
	req.SetPathValue("name", "paling")
	s.handleServiceByName(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("found: code = %d, want 200", rr.Code)
	}

	// The body is the bare composed entity (not a list envelope).
	var svc map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &svc); err != nil {
		t.Fatalf("decode entity: %v", err)
	}
	if svc["name"] != "paling" {
		t.Errorf("name = %#v", svc["name"])
	}
	if svc["type"] != "SERVICE_TYPE_SERVICE" {
		t.Errorf("type = %#v, want SERVICE_TYPE_SERVICE", svc["type"])
	}
	if svc["reachable"] != true {
		t.Errorf("reachable = %#v, want true (entry is registered)", svc["reachable"])
	}
	// endpoint composed from the live registration.
	if ep, ok := svc["endpoint"].(map[string]any); !ok || ep["address"] != "paling.fleet:8090" {
		t.Errorf("endpoint not composed from registration: %#v", svc["endpoint"])
	}
	// backup facet from the state machine (fallow at rest).
	if bk, ok := svc["backup"].(map[string]any); !ok || bk["state"] != string(state.StateFallow) {
		t.Errorf("backup facet = %#v, want state=%q", svc["backup"], state.StateFallow)
	}
	// git facet: a non-git path surfaces an error and OMITS clean.
	gf, ok := svc["git"].(map[string]any)
	if !ok {
		t.Fatalf("git facet missing: %#v", svc["git"])
	}
	if e, _ := gf["error"].(string); e == "" {
		t.Errorf("git error should be surfaced for a non-git path: %#v", gf)
	}
	if _, present := gf["clean"]; present {
		t.Errorf("clean must be absent when the git read failed, got %#v", gf["clean"])
	}

	// An entry the daemon does not manage is a 404.
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/services/ghost", nil)
	req.SetPathValue("name", "ghost")
	s.handleServiceByName(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("unknown: code = %d, want 404", rr.Code)
	}
}

func TestHandleServicesAll(t *testing.T) {
	// paling is registered and has a backup machine; taco is neither -- so the composed
	// entities must differ exactly in the facets delightd has a basis to answer.
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry", "registry.db"), nil)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	defer reg.Close()
	if err := reg.Put(&registryv1.Registration{
		Project:  "paling",
		Endpoint: &registryv1.Endpoint{Scheme: "http", Address: "paling.fleet:8090"},
	}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.DelightConfig{Projects: []config.ProjectConfig{
		{Name: "paling", Path: t.TempDir()},
		{Name: "taco", Path: t.TempDir()},
	}}
	machines := map[string]*state.Machine{"paling": state.NewMachine("paling")}
	s := New(cfg, machines, fakeFragments{}, nil, false, reg)

	// Unfiltered: the whole roster, composed.
	rr := httptest.NewRecorder()
	s.handleServicesAll(rr, httptest.NewRequest(http.MethodGet, "/services", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
	status, byName := decodeServices(t, rr.Body.Bytes())
	if status != "ok" || len(byName) != 2 {
		t.Fatalf("unexpected roster: %s", rr.Body.String())
	}

	// paling: registered + machine -> reachable, endpoint, backup all present.
	paling := byName["paling"]
	if paling["reachable"] != true {
		t.Errorf("paling reachable = %#v, want true", paling["reachable"])
	}
	if _, ok := paling["endpoint"]; !ok {
		t.Errorf("paling endpoint should be composed from its registration")
	}
	if _, ok := paling["backup"]; !ok {
		t.Errorf("paling backup facet should be present")
	}

	// taco: no registration, no machine -> reachable/endpoint/backup ABSENT, but type is set.
	taco := byName["taco"]
	if _, ok := taco["reachable"]; ok {
		t.Errorf("taco has no registration; reachable must be absent (unknown), got %#v", taco["reachable"])
	}
	if _, ok := taco["endpoint"]; ok {
		t.Errorf("taco has no registration; endpoint must be absent, got %#v", taco["endpoint"])
	}
	if _, ok := taco["backup"]; ok {
		t.Errorf("taco has no backup machine; backup facet must be absent, got %#v", taco["backup"])
	}
	if taco["type"] != "SERVICE_TYPE_SERVICE" {
		t.Errorf("taco type = %#v, want SERVICE_TYPE_SERVICE", taco["type"])
	}

	// ?type=service matches every step-1 entry.
	rr = httptest.NewRecorder()
	s.handleServicesAll(rr, httptest.NewRequest(http.MethodGet, "/services?type=service", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("type=service: code = %d, want 200", rr.Code)
	}
	if _, byName := decodeServices(t, rr.Body.Bytes()); len(byName) != 2 {
		t.Errorf("type=service should match both entries, got %d", len(byName))
	}

	// ?type=model is a valid filter with no entries yet (#34 populates the model roster).
	rr = httptest.NewRecorder()
	s.handleServicesAll(rr, httptest.NewRequest(http.MethodGet, "/services?type=model", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("type=model: code = %d, want 200", rr.Code)
	}
	if _, byName := decodeServices(t, rr.Body.Bytes()); len(byName) != 0 {
		t.Errorf("type=model should be empty in step 1, got %d", len(byName))
	}

	// An unrecognized type is a loud 400, not a silent empty list.
	rr = httptest.NewRecorder()
	s.handleServicesAll(rr, httptest.NewRequest(http.MethodGet, "/services?type=bogus", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("type=bogus: code = %d, want 400", rr.Code)
	}
}

func TestHandleResolve(t *testing.T) {
	// A nil registry resolves nothing: every name is a 404 (delightd holds no address it
	// could return). A miss is always 404, never 503.
	s := New(&config.DelightConfig{}, nil, fakeFragments{}, nil, false, nil)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/resolve/obs-agg", nil)
	req.SetPathValue("name", "obs-agg")
	s.handleResolve(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("nil registry: code = %d, want 404", rr.Code)
	}

	// A registered project resolves to its live endpoint as a bare resolve.v1.ResolvedService.
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry", "registry.db"), nil)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	defer reg.Close()
	if err := reg.Put(&registryv1.Registration{
		Project:  "obs-agg",
		Endpoint: &registryv1.Endpoint{Scheme: "http", Address: "obs-svc-agg.fleet:8090"},
	}); err != nil {
		t.Fatal(err)
	}
	s2 := New(&config.DelightConfig{}, nil, fakeFragments{}, nil, false, reg)

	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/resolve/obs-agg", nil)
	req.SetPathValue("name", "obs-agg")
	s2.handleResolve(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("registered: code = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	// The body is the bare ResolvedService -- the three strings the widget's narrow crate
	// deserializes, with proto field names.
	var got resolvev1.ResolvedService
	if err := protojson.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode ResolvedService: %v (body: %s)", err, rr.Body.String())
	}
	if got.GetName() != "obs-agg" || got.GetScheme() != "http" || got.GetAddress() != "obs-svc-agg.fleet:8090" {
		t.Fatalf("resolved = %+v, want {obs-agg http obs-svc-agg.fleet:8090}", &got)
	}

	// A name with no live registration is a 404 -- "not resolvable" is distinct from "down".
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/resolve/ghost", nil)
	req.SetPathValue("name", "ghost")
	s2.handleResolve(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unregistered: code = %d, want 404", rr.Code)
	}
}

func TestMux_RoutingAndMCPGating(t *testing.T) {
	machines := map[string]*state.Machine{"p": state.NewMachine("p")}
	s := New(&config.DelightConfig{}, machines, fakeFragments{}, nil, false, nil)
	s.discover = noDiscovery
	mux := s.Mux()

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/projects/p/introspect", http.StatusOK},
		{http.MethodGet, "/projects", http.StatusOK},
		{http.MethodGet, "/services", http.StatusOK},
		{http.MethodGet, "/registrations", http.StatusOK},
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
	s := New(cfg, nil, fakeFragments{}, agg, false, nil)
	mux := s.Mux()

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"method":"tools/list"}`)))
	if rr.Code == http.StatusNotFound {
		t.Errorf("POST /mcp should be routed when MCP is enabled, got 404")
	}
}
