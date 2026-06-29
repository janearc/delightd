package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"

	froodv1 "delightd/gen/go/frood/v1"
	registryv1 "delightd/gen/go/registry/v1"

	"delightd/pkg/registry"
)

// subjectChecker answers whether a contract subject is registered with the schema registry.
// It is defined here (the consumer side) so handleRegister can be tested with a fake; the
// real implementation is pkg/schemaregistry.Client.
type subjectChecker interface {
	SubjectExists(ctx context.Context, subject string) (bool, error)
}

// requiredEmitSubject is the contract every frood MUST emit: the fleet heartbeat (Big Little
// Mesh's frood.SubjectHeartbeat). A constant here rather than a dependency for one string;
// metrics ride emits, they are not a special field.
const requiredEmitSubject = "observability.v1.ServiceHealthHeartbeat"

// notRegisteredSubject is the RecordNameStrategy subject for the not-completed event.
const notRegisteredSubject = "registry.v1.NotRegistered"

// guaranteeHealthCheck is the single reachability guarantee delightd makes at join time: the
// reported endpoint MUST answer GET /health with 2xx. It is named as a guarantee, not a
// best-effort probe -- a frood that does not answer here does not join. Full active checking
// of every guaranteed endpoint is a later concern.
func guaranteeHealthCheck(ctx context.Context, e *registryv1.Endpoint) error {
	scheme := e.GetScheme()
	if scheme == "" {
		scheme = "http"
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s://%s/health", scheme, e.GetAddress()), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("/health returned %d", resp.StatusCode)
	}
	return nil
}

// handleRegister is POST /register: a frood presents its declared project, identity, the
// contracts it speaks, and the endpoint(s) it has bound; delightd records it into the live
// registry, or the registration does not complete. A registration that does not complete is
// reported twice: an HTTP error to the caller AND a NotRegistered event on the bus (the
// never-silent rule -- the froods whose registration did not complete are observable, not
// only the ones that joined). Additive: no frood is required to register yet, and recording
// here does not touch the yaml/poll roster. Confirm model: delightd records the endpoint the
// frood reported, it does not assign one.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		s.notRegistered(w, http.StatusBadRequest, "", "", "unreadable_body", "could not read request body")
		return
	}
	var req registryv1.RegisterRequest
	if err := protojson.Unmarshal(body, &req); err != nil {
		s.notRegistered(w, http.StatusBadRequest, "", "", "malformed_request", fmt.Sprintf("invalid RegisterRequest: %v", err))
		return
	}

	endpointAddr := firstEndpointAddress(req.GetEndpoints())

	// 1. declared-project: the claimed project MUST be one delightd manages. An unknown name
	//    returns 404 (the house convention for a route acting on a project name; there is no
	//    authz actor here, so 403 would be the wrong axis).
	if !s.managesProject(req.GetProject()) {
		s.notRegistered(w, http.StatusNotFound, req.GetProject(), endpointAddr, "unknown_project", "project not found")
		return
	}
	// 2. internal consistency: the project the frood claims MUST equal the project its own
	//    identity reports. A mismatch is an internally inconsistent registration.
	if req.GetIdentity().GetProject() != req.GetProject() {
		s.notRegistered(w, http.StatusUnprocessableEntity, req.GetProject(), endpointAddr, "inconsistent_identity",
			fmt.Sprintf("internally inconsistent: request.project=%q but identity.project=%q", req.GetProject(), req.GetIdentity().GetProject()))
		return
	}
	// the endpoint to confirm (confirm model: the first endpoint the frood reported).
	if endpointAddr == "" {
		s.notRegistered(w, http.StatusUnprocessableEntity, req.GetProject(), "", "no_endpoint",
			"no endpoint with an address was reported")
		return
	}
	endpoint := req.GetEndpoints()[0]
	// 3. RULE-4: verify the claimed contract descriptor.
	if status, code, reason := s.verifyContracts(r.Context(), req.GetContracts()); status != 0 {
		s.notRegistered(w, status, req.GetProject(), endpointAddr, code, reason)
		return
	}
	// 4. reachability guarantee: the reported endpoint MUST answer /health.
	if err := s.guaranteeHealthCheck(r.Context(), endpoint); err != nil {
		s.notRegistered(w, http.StatusUnprocessableEntity, req.GetProject(), endpointAddr, "unreachable",
			fmt.Sprintf("endpoint %q did not pass its /health guarantee: %v", endpointAddr, err))
		return
	}

	// 5. record with the lease stamped. Upsert is the atomic collision step: a DIFFERENT
	//    project on the same endpoint is not accepted; the same project re-claiming its own
	//    endpoint is idempotent.
	now := time.Now().UTC()
	reg := &registryv1.Registration{
		Project:        req.GetProject(),
		Identity:       req.GetIdentity(),
		Contracts:      req.GetContracts(),
		Endpoint:       endpoint,
		RegisteredAt:   timestamppb.New(now),
		LeaseExpiresAt: timestamppb.New(now.Add(registry.DefaultLeaseTTL)),
	}
	holder, err := s.reg.Upsert(reg)
	if err != nil {
		if holder != "" {
			s.notRegistered(w, http.StatusConflict, req.GetProject(), endpointAddr, "endpoint_held",
				fmt.Sprintf("endpoint %q is already held by project %q", endpointAddr, holder))
			return
		}
		slog.Error("register: failed to record registration", "project", req.GetProject(), "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to record registration"})
		return
	}

	resp := &registryv1.RegisterResponse{
		Identity:        req.GetIdentity(),
		Endpoint:        endpoint,
		LeaseTtlSeconds: uint32(registry.DefaultLeaseTTL / time.Second),
	}
	b, err := rosterMarshal.Marshal(resp)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to encode RegisterResponse"})
		return
	}
	slog.Info("register: accepted frood", "project", req.GetProject(), "endpoint", endpointAddr)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(b); err != nil {
		slog.Error("register: failed to write response", "error", err)
	}
}

// notRegistered reports that a registration did not complete, and why. It returns the HTTP
// error to the caller FIRST, then emits the never-silent NotRegistered event. The response
// MUST return immediately regardless of broker health, so the emit happens on a detached
// goroutine with its own bounded context -- never on the request context, never before the
// response. A failed or lost emit is logged loudly, never swallowed.
func (s *Server) notRegistered(w http.ResponseWriter, status int, project, endpoint, code, reason string) {
	writeJSON(w, status, errorResponse{Error: reason})
	s.emitNotRegistered(project, endpoint, code, reason)
}

func (s *Server) emitNotRegistered(project, endpoint, code, reason string) {
	if s.events == nil {
		slog.Warn("registration did not complete, but no event publisher is configured: outcome is not on the bus",
			"project", project, "code", code, "reason", reason)
		return
	}
	ev := &registryv1.NotRegistered{
		Project:    project,
		Endpoint:   endpoint,
		Code:       code,
		Reason:     reason,
		OccurredAt: timestamppb.Now(),
	}
	// Detached: a dead or slow broker must not delay the response (already written) or pin
	// the request goroutine. The WaitGroup lets a graceful shutdown -- and the tests -- wait
	// for in-flight emits to finish.
	s.emitWG.Add(1)
	go func() {
		defer s.emitWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := s.events.Publish(ctx, s.eventsTopic, notRegisteredSubject, s.notRegisteredSchema, project, ev); err != nil {
			slog.Error("register: could not emit NotRegistered to the bus", "project", project, "code", code, "error", err)
		}
	}()
}

// managesProject reports whether name is a project delightd manages (in the roster).
func (s *Server) managesProject(name string) bool {
	for _, p := range s.cfg.Projects {
		if p.Name == name {
			return true
		}
	}
	return false
}

// firstEndpointAddress returns the address of the first reported endpoint, or "".
func firstEndpointAddress(endpoints []*registryv1.Endpoint) string {
	if len(endpoints) == 0 {
		return ""
	}
	return endpoints[0].GetAddress()
}

// verifyContracts runs RULE-4 on the claimed descriptor. It returns (0, "", "") on pass, or
// an HTTP status + a stable code + a human reason for a registration that does not complete:
//   - emits MUST contain the required heartbeat subject;
//   - every emits/consumes subject MUST be registered with the schema registry (bus
//     contracts -- the claim is checkable against what is actually on the bus); an SR that
//     cannot answer yields 503 (the registration does not complete, but that is not the
//     frood's fault);
//   - serves subjects are checked only for fully-qualified-name well-formedness, using the
//     protobuf library's own name validator (protoreflect.FullName.IsValid) rather than a
//     hand-rolled pattern. Semantic verification (the subject names a REAL service contract)
//     is deliberately seamed for a future generated contract catalog -- the contract IS the
//     catalog and cannot drift, whereas a hardcoded list or hand-maintained allowlist would.
func (s *Server) verifyContracts(ctx context.Context, d *froodv1.ContractDescriptor) (int, string, string) {
	if !hasSubject(d.GetEmits(), requiredEmitSubject) {
		return http.StatusUnprocessableEntity, "missing_required_subject",
			fmt.Sprintf("descriptor must emit the required subject %q", requiredEmitSubject)
	}
	for _, refs := range [][]*froodv1.ContractRef{d.GetEmits(), d.GetConsumes()} {
		for _, ref := range refs {
			ok, err := s.subjects.SubjectExists(ctx, ref.GetSubject())
			if err != nil {
				return http.StatusServiceUnavailable, "verify_unavailable",
					fmt.Sprintf("could not verify subject %q against the schema registry: %v", ref.GetSubject(), err)
			}
			if !ok {
				return http.StatusUnprocessableEntity, "unknown_subject",
					fmt.Sprintf("subject %q is not registered with the schema registry", ref.GetSubject())
			}
		}
	}
	for _, ref := range d.GetServes() {
		if !protoreflect.FullName(ref.GetSubject()).IsValid() {
			return http.StatusUnprocessableEntity, "malformed_serves",
				fmt.Sprintf("serves subject %q is not a well-formed fully-qualified name", ref.GetSubject())
		}
	}
	return 0, "", ""
}

func hasSubject(refs []*froodv1.ContractRef, subject string) bool {
	for _, ref := range refs {
		if ref.GetSubject() == subject {
			return true
		}
	}
	return false
}
