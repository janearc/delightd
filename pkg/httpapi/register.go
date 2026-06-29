package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	citizenv1 "delightd/gen/go/citizen/v1"
	registryv1 "delightd/gen/go/registry/v1"
)

// subjectChecker answers whether a contract subject is registered with the schema registry.
// It is defined here (the consumer side) so handleRegister can be tested with a fake; the
// real implementation is pkg/schemaregistry.Client.
type subjectChecker interface {
	SubjectExists(ctx context.Context, subject string) (bool, error)
}

// defaultLeaseTTLSeconds is delightd's lease policy for a fresh registration. The lease
// fields are SET here; their ENFORCEMENT (renewal + reap) is a later step (R4).
const defaultLeaseTTLSeconds = 90

// requiredEmitSubject is the contract every citizen MUST emit: the fleet heartbeat (blm's
// citizen.SubjectHeartbeat). It is a constant here rather than an import of blm for one
// string. Metrics rides emits; it is not a special field.
const requiredEmitSubject = "observability.v1.ServiceHealthHeartbeat"

// contractFQN matches a contract's RecordNameStrategy identity: a lowercase dotted package
// ending in a version segment (vN), then a PascalCase message name -- e.g.
// "dataprovider.v1.DataProvider". This is the syntactic well-formedness check for `serves`
// subjects. Semantic verification (that the subject names a REAL service contract) is
// deliberately seamed: there is no good runtime source today, and the right one is a
// generated contract catalog (the contract IS the catalog, cannot drift), which is its own
// future bucket. Hardcoding blm's contracts here would couple the orchestrator to every
// service it brokers; a config allowlist would drift -- the exact failure this campaign
// exists to kill.
var contractFQN = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*\.v[0-9]+\.[A-Z][A-Za-z0-9]*$`)

// defaultHealthProbe GETs the endpoint's /health once with a short timeout and errors unless
// it answers 2xx. This is the single reachability probe at register time; full active-probing
// of every guaranteed endpoint is deferred.
func defaultHealthProbe(ctx context.Context, e *registryv1.Endpoint) error {
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

// handleRegister is POST /register: a citizen presents its declared project, identity, the
// contracts it speaks, and the endpoint(s) it has bound; delightd admits it into the live
// registry or rejects it with a visible reason (never a silent drop). Additive -- no citizen
// is required to register yet, and recording here does not touch the yaml/poll roster.
// CONFIRM model: delightd records the endpoint the citizen reports, it does not assign one.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "could not read request body"})
		return
	}
	var req registryv1.RegisterRequest
	if err := protojson.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("invalid RegisterRequest: %v", err)})
		return
	}

	// 1. declared-project: the claimed project MUST be one delightd manages.
	if !s.managesProject(req.GetProject()) {
		writeJSON(w, http.StatusForbidden, errorResponse{Error: fmt.Sprintf("project %q is not a managed project", req.GetProject())})
		return
	}
	// 2. self-consistency: the claimed project MUST equal the identity's self-reported
	//    project. A mismatch is a contradictory registration (claim vs self-report).
	if req.GetIdentity().GetProject() != req.GetProject() {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("project mismatch: request.project=%q identity.project=%q", req.GetProject(), req.GetIdentity().GetProject())})
		return
	}
	// CONFIRM model: record the endpoint the citizen reported (the first bound endpoint).
	endpoints := req.GetEndpoints()
	if len(endpoints) == 0 || endpoints[0].GetAddress() == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "no endpoint with an address was reported"})
		return
	}
	endpoint := endpoints[0]
	// 3. collision (CAS): a DIFFERENT live registration holding this endpoint's authority is
	//    a conflict. The SAME project re-registering its own endpoint is idempotent (a
	//    renewal/update). Address (host:port/authority) is the collision unit; scheme is not
	//    part of the key -- two citizens cannot bind the same address regardless of scheme.
	if holder, ok := s.endpointHolder(endpoint.GetAddress()); ok && holder != req.GetProject() {
		writeJSON(w, http.StatusConflict, errorResponse{Error: fmt.Sprintf("endpoint %q is already held by project %q", endpoint.GetAddress(), holder)})
		return
	}
	// 4. RULE-4: verify the claimed contract descriptor.
	if status, msg := s.verifyContracts(r.Context(), req.GetContracts()); status != 0 {
		writeJSON(w, status, errorResponse{Error: msg})
		return
	}
	// reachability: the reported endpoint MUST answer /health once.
	if err := s.probeHealth(r.Context(), endpoint); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: fmt.Sprintf("endpoint %q failed its /health probe: %v", endpoint.GetAddress(), err)})
		return
	}

	// admit: record the Registration with its lease (TTL policy is delightd's; enforcement
	// is R4).
	now := time.Now().UTC()
	reg := &registryv1.Registration{
		Project:        req.GetProject(),
		Identity:       req.GetIdentity(),
		Contracts:      req.GetContracts(),
		Endpoint:       endpoint,
		RegisteredAt:   timestamppb.New(now),
		LeaseExpiresAt: timestamppb.New(now.Add(defaultLeaseTTLSeconds * time.Second)),
	}
	if err := s.reg.Put(reg); err != nil {
		slog.Error("register: failed to record registration", "project", req.GetProject(), "error", err)
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to record registration"})
		return
	}

	resp := &registryv1.RegisterResponse{
		Identity:        req.GetIdentity(),
		Endpoint:        endpoint,
		LeaseTtlSeconds: defaultLeaseTTLSeconds,
	}
	b, err := rosterMarshal.Marshal(resp)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "failed to encode RegisterResponse"})
		return
	}
	slog.Info("register: admitted citizen", "project", req.GetProject(), "endpoint", endpoint.GetAddress())
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(b); err != nil {
		slog.Error("register: failed to write response", "error", err)
	}
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

// endpointHolder returns the project whose live registration holds the given endpoint
// address, if any.
func (s *Server) endpointHolder(address string) (string, bool) {
	if s.reg == nil {
		return "", false
	}
	for _, reg := range s.reg.List() {
		if reg.GetEndpoint().GetAddress() == address {
			return reg.GetProject(), true
		}
	}
	return "", false
}

// verifyContracts runs RULE-4 on the claimed descriptor. It returns (0, "") on pass, or an
// HTTP status + message to reject with.
//   - emits MUST contain the required heartbeat subject;
//   - every emits/consumes subject MUST be registered with the schema registry (bus
//     contracts -- the claim is checkable against what is actually on the bus);
//   - serves subjects are checked for FQN well-formedness only; semantic verification is
//     seamed (see contractFQN).
func (s *Server) verifyContracts(ctx context.Context, d *citizenv1.ContractDescriptor) (int, string) {
	if !hasSubject(d.GetEmits(), requiredEmitSubject) {
		return http.StatusBadRequest, fmt.Sprintf("descriptor must emit the required subject %q", requiredEmitSubject)
	}
	for _, refs := range [][]*citizenv1.ContractRef{d.GetEmits(), d.GetConsumes()} {
		for _, ref := range refs {
			ok, err := s.subjects.SubjectExists(ctx, ref.GetSubject())
			if err != nil {
				return http.StatusBadGateway, fmt.Sprintf("could not verify subject %q against the schema registry: %v", ref.GetSubject(), err)
			}
			if !ok {
				return http.StatusBadRequest, fmt.Sprintf("subject %q is not registered with the schema registry", ref.GetSubject())
			}
		}
	}
	for _, ref := range d.GetServes() {
		if !contractFQN.MatchString(ref.GetSubject()) {
			return http.StatusBadRequest, fmt.Sprintf("serves subject %q is not a well-formed contract FQN", ref.GetSubject())
		}
	}
	return 0, ""
}

func hasSubject(refs []*citizenv1.ContractRef, subject string) bool {
	for _, ref := range refs {
		if ref.GetSubject() == subject {
			return true
		}
	}
	return false
}
