package httpapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	citizenv1 "delightd/gen/go/citizen/v1"
	registryv1 "delightd/gen/go/registry/v1"

	"delightd/config"
	"delightd/pkg/registry"
)

// fakeSubjects is a stub schema-registry: known subjects pass, others fail, and a non-nil
// err simulates an SR outage.
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

// registerServer builds a Server for /register tests: a roster of project names, a fake SR
// with the given known subjects, and a health probe that succeeds. The registry is real but
// backed by a temp path.
func registerServer(t *testing.T, projects []string, known map[string]bool) (*Server, *registry.Registry) {
	t.Helper()
	cfg := &config.DelightConfig{}
	for _, p := range projects {
		cfg.Projects = append(cfg.Projects, config.ProjectConfig{Name: p})
	}
	reg := registry.New(filepath.Join(t.TempDir(), "registry", "registrations.json"), nil)
	s := New(cfg, nil, fakeFragments{}, nil, false, reg)
	s.subjects = fakeSubjects{known: known}
	s.probeHealth = func(context.Context, *registryv1.Endpoint) error { return nil }
	return s, reg
}

// validReq is a register request that passes every admission check given a server that
// manages `project` and knows the heartbeat subject.
func validReq(project, addr string) *registryv1.RegisterRequest {
	return &registryv1.RegisterRequest{
		Project:  project,
		Identity: &citizenv1.Identity{ServiceName: project, Project: project, Version: "v1"},
		Contracts: &citizenv1.ContractDescriptor{
			Emits:  []*citizenv1.ContractRef{{Subject: requiredEmitSubject}},
			Serves: []*citizenv1.ContractRef{{Subject: "dataprovider.v1.DataProvider"}},
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
	s, reg := registerServer(t, []string{"paling"}, map[string]bool{requiredEmitSubject: true})
	rr := post(t, s, validReq("paling", "paling.fleet:8090"))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	// the response is a RegisterResponse (protojson) with the confirmed endpoint + lease.
	body := rr.Body.String()
	if !contains(body, `"address":"paling.fleet:8090"`) || !contains(body, `"lease_ttl_seconds":90`) {
		t.Fatalf("unexpected RegisterResponse: %s", body)
	}
	// the registration was recorded.
	if g, ok := reg.Get("paling"); !ok || g.GetEndpoint().GetAddress() != "paling.fleet:8090" {
		t.Fatalf("registration not recorded: %v ok=%v", g, ok)
	}
}

func TestRegister_UndeclaredProject(t *testing.T) {
	s, _ := registerServer(t, []string{"paling"}, map[string]bool{requiredEmitSubject: true})
	rr := post(t, s, validReq("ghost", "ghost:1"))
	if rr.Code != http.StatusForbidden {
		t.Fatalf("undeclared project: code = %d, want 403; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRegister_ProjectMismatch(t *testing.T) {
	s, _ := registerServer(t, []string{"paling"}, map[string]bool{requiredEmitSubject: true})
	req := validReq("paling", "paling:1")
	req.Identity.Project = "something-else"
	rr := post(t, s, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("self-consistency mismatch: code = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRegister_EndpointCollision(t *testing.T) {
	s, reg := registerServer(t, []string{"paling", "magpie"}, map[string]bool{requiredEmitSubject: true})
	// magpie already holds the address.
	if err := reg.Put(&registryv1.Registration{Project: "magpie", Endpoint: &registryv1.Endpoint{Address: "shared:9000"}}); err != nil {
		t.Fatal(err)
	}
	rr := post(t, s, validReq("paling", "shared:9000"))
	if rr.Code != http.StatusConflict {
		t.Fatalf("endpoint collision: code = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRegister_IdempotentReRegister(t *testing.T) {
	s, _ := registerServer(t, []string{"paling"}, map[string]bool{requiredEmitSubject: true})
	if rr := post(t, s, validReq("paling", "paling:8090")); rr.Code != http.StatusOK {
		t.Fatalf("first register: code = %d", rr.Code)
	}
	// the same project re-registering its own endpoint is a renewal, not a conflict.
	if rr := post(t, s, validReq("paling", "paling:8090")); rr.Code != http.StatusOK {
		t.Fatalf("re-register: code = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRegister_UnknownEmitSubject(t *testing.T) {
	s, _ := registerServer(t, []string{"paling"}, map[string]bool{requiredEmitSubject: true})
	req := validReq("paling", "paling:1")
	req.Contracts.Emits = append(req.Contracts.Emits, &citizenv1.ContractRef{Subject: "unknown.v1.Thing"})
	rr := post(t, s, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unknown emit subject: code = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRegister_MissingRequiredHeartbeat(t *testing.T) {
	s, _ := registerServer(t, []string{"paling"}, map[string]bool{"some.v1.Other": true})
	req := validReq("paling", "paling:1")
	req.Contracts.Emits = []*citizenv1.ContractRef{{Subject: "some.v1.Other"}} // no heartbeat
	rr := post(t, s, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("missing heartbeat: code = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRegister_MalformedServesFQN(t *testing.T) {
	s, _ := registerServer(t, []string{"paling"}, map[string]bool{requiredEmitSubject: true})
	req := validReq("paling", "paling:1")
	req.Contracts.Serves = []*citizenv1.ContractRef{{Subject: "not a valid fqn"}}
	rr := post(t, s, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed serves FQN: code = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

func TestRegister_UnreachableHealth(t *testing.T) {
	s, _ := registerServer(t, []string{"paling"}, map[string]bool{requiredEmitSubject: true})
	s.probeHealth = func(context.Context, *registryv1.Endpoint) error {
		return context.DeadlineExceeded
	}
	rr := post(t, s, validReq("paling", "paling:1"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("unreachable health: code = %d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// contains is a tiny substring helper to keep the assertions readable.
func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
