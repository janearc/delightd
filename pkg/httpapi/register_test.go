package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	froodv1 "delightd/gen/go/frood/v1"
	registryv1 "delightd/gen/go/registry/v1"

	"delightd/config"
	"delightd/pkg/registry"
)

// fakeSubjects is a stub schema-registry: known subjects pass, others fail, a non-nil err
// simulates an SR outage.
type fakeSubjects struct {
	known map[string]bool
	err   error
}

func (f fakeSubjects) SubjectExists(_ context.Context, subject string) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return f.known[subject], nil
}

// fakeEvents records the NotRegistered events the handler publishes, so tests can assert
// the never-silent behavior.
type fakeEvents struct {
	mu    sync.Mutex
	last  *registryv1.NotRegistered
	count int
}

func (f *fakeEvents) Publish(_ context.Context, _, _, _, _ string, msg proto.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.count++
	if nr, ok := msg.(*registryv1.NotRegistered); ok {
		f.last = nr
	}
	return nil
}

func (f *fakeEvents) notRegistered() (*registryv1.NotRegistered, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last, f.count
}

func registerServer(t *testing.T, projects []string, known map[string]bool) (*Server, *fakeEvents) {
	t.Helper()
	cfg := &config.DelightConfig{}
	for _, p := range projects {
		cfg.Projects = append(cfg.Projects, config.ProjectConfig{Name: p})
	}
	reg, err := registry.Open(filepath.Join(t.TempDir(), "registry", "registry.db"), nil)
	if err != nil {
		t.Fatalf("open registry: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	s := New(cfg, nil, fakeFragments{}, nil, false, reg)
	s.subjects = fakeSubjects{known: known}
	s.guaranteeHealthCheck = func(context.Context, *registryv1.Endpoint) error { return nil }
	ev := &fakeEvents{}
	s.UseEvents(ev, "delight.events", "schema-text")
	return s, ev
}

func validReq(project, addr string) *registryv1.RegisterRequest {
	return &registryv1.RegisterRequest{
		Project:  project,
		Identity: &froodv1.Identity{ServiceName: project, Project: project, Version: "v1"},
		Contracts: &froodv1.ContractDescriptor{
			Emits:  []*froodv1.ContractRef{{Subject: requiredEmitSubject}},
			Serves: []*froodv1.ContractRef{{Subject: "dataprovider.v1.DataProvider"}},
		},
		Endpoints: []*registryv1.Endpoint{{Scheme: "http", Address: addr}},
	}
}

func post(t *testing.T, s *Server, req *registryv1.RegisterRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := protojson.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	rr := httptest.NewRecorder()
	s.handleRegister(rr, httptest.NewRequest(http.MethodPost, "/register", bytes.NewReader(body)))
	return rr
}

func TestRegister_HappyPath(t *testing.T) {
	s, ev := registerServer(t, []string{"paling"}, map[string]bool{requiredEmitSubject: true})
	rr := post(t, s, validReq("paling", "paling.fleet:8090"))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !contains(body, `"address":"paling.fleet:8090"`) || !contains(body, `"lease_ttl_seconds":90`) {
		t.Fatalf("unexpected RegisterResponse: %s", body)
	}
	if g, ok := s.reg.Get("paling"); !ok || g.GetEndpoint().GetAddress() != "paling.fleet:8090" {
		t.Fatalf("registration not recorded: %v ok=%v", g, ok)
	}
	// a completed join emits no NotRegistered event.
	s.emitWG.Wait()
	if _, n := ev.notRegistered(); n != 0 {
		t.Fatalf("happy path should emit no NotRegistered, got %d", n)
	}
}

func TestRegister_UnknownProject_EmitsNotRegistered(t *testing.T) {
	s, ev := registerServer(t, []string{"paling"}, map[string]bool{requiredEmitSubject: true})
	rr := post(t, s, validReq("ghost", "ghost:1"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown project: code = %d, want 404; body=%s", rr.Code, rr.Body.String())
	}
	if !contains(rr.Body.String(), `"error":"project not found"`) {
		t.Fatalf("unknown project body = %s, want project-not-found", rr.Body.String())
	}
	// never-silent: the outcome is also on the bus, with a stable code (emit is detached).
	s.emitWG.Wait()
	last, n := ev.notRegistered()
	if n != 1 || last == nil || last.GetCode() != "unknown_project" || last.GetProject() != "ghost" {
		t.Fatalf("expected one NotRegistered{code=unknown_project, project=ghost}, got n=%d last=%v", n, last)
	}
}

func TestRegister_InconsistentIdentity(t *testing.T) {
	s, ev := registerServer(t, []string{"paling"}, map[string]bool{requiredEmitSubject: true})
	req := validReq("paling", "paling:1")
	req.Identity.Project = "something-else"
	rr := post(t, s, req)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("inconsistent identity: code = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
	s.emitWG.Wait()
	if last, _ := ev.notRegistered(); last == nil || last.GetCode() != "inconsistent_identity" {
		t.Fatalf("expected NotRegistered code=inconsistent_identity, got %v", last)
	}
}

func TestRegister_EndpointCollision(t *testing.T) {
	s, ev := registerServer(t, []string{"paling", "magpie"}, map[string]bool{requiredEmitSubject: true})
	if rr := post(t, s, validReq("magpie", "shared:9000")); rr.Code != http.StatusOK {
		t.Fatalf("seed magpie: code = %d", rr.Code)
	}
	rr := post(t, s, validReq("paling", "shared:9000"))
	if rr.Code != http.StatusConflict {
		t.Fatalf("endpoint collision: code = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	s.emitWG.Wait()
	if last, _ := ev.notRegistered(); last == nil || last.GetCode() != "endpoint_held" {
		t.Fatalf("expected NotRegistered code=endpoint_held, got %v", last)
	}
}

func TestRegister_IdempotentReRegister(t *testing.T) {
	s, _ := registerServer(t, []string{"paling"}, map[string]bool{requiredEmitSubject: true})
	if rr := post(t, s, validReq("paling", "paling:8090")); rr.Code != http.StatusOK {
		t.Fatalf("first register: code = %d", rr.Code)
	}
	if rr := post(t, s, validReq("paling", "paling:8090")); rr.Code != http.StatusOK {
		t.Fatalf("re-register own endpoint should be idempotent: code = %d; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRegister_UnknownEmitSubject(t *testing.T) {
	s, _ := registerServer(t, []string{"paling"}, map[string]bool{requiredEmitSubject: true})
	req := validReq("paling", "paling:1")
	req.Contracts.Emits = append(req.Contracts.Emits, &froodv1.ContractRef{Subject: "unknown.v1.Thing"})
	if rr := post(t, s, req); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown emit subject: code = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRegister_MissingRequiredHeartbeat(t *testing.T) {
	s, _ := registerServer(t, []string{"paling"}, map[string]bool{"some.v1.Other": true})
	req := validReq("paling", "paling:1")
	req.Contracts.Emits = []*froodv1.ContractRef{{Subject: "some.v1.Other"}}
	if rr := post(t, s, req); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing heartbeat: code = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRegister_MalformedServesName(t *testing.T) {
	s, _ := registerServer(t, []string{"paling"}, map[string]bool{requiredEmitSubject: true})
	req := validReq("paling", "paling:1")
	req.Contracts.Serves = []*froodv1.ContractRef{{Subject: "not a valid name"}}
	if rr := post(t, s, req); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("malformed serves name: code = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRegister_UnreachableHealth(t *testing.T) {
	s, _ := registerServer(t, []string{"paling"}, map[string]bool{requiredEmitSubject: true})
	s.guaranteeHealthCheck = func(context.Context, *registryv1.Endpoint) error {
		return context.DeadlineExceeded
	}
	if rr := post(t, s, validReq("paling", "paling:1")); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unreachable health: code = %d, want 422; body=%s", rr.Code, rr.Body.String())
	}
}

func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
