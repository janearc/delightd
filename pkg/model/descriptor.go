// Package model is delightd's model-hosting subsystem (folded in from model-svc; see
// docs/model-hosting.md). It treats each hosted model as a deployment -- a "model
// availability signifier" -- and reconciles it like any other asset.
//
// This file is the deployment descriptor, ported from model-svc's Pydantic schema. A
// deployment declares a model, where its weights resolve, the backend that serves it,
// its architecture and role, and the in-mesh route. The architecture/role pair is the
// fleet-specific dimension LiteLLM cannot express, and architecture is what decides which
// backend can host a model at all.
package model

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Architecture is the transformer topology. It determines which backend can host the
// model at all: a seq2seq model cannot ride a decoder-only ollama endpoint.
type Architecture string

const (
	ArchEncode       Architecture = "encode"        // encoder-only -- embeddings/classification
	ArchEncodeDecode Architecture = "encode-decode" // seq2seq (T5/flan) -- conditional generation
	ArchDecode       Architecture = "decode"        // decoder-only (mistral/llama) -- autoregressive chat
)

// Role is what the deployment is consumed as -- the contract the caller binds to,
// declared rather than inferred from the weights.
type Role string

const (
	RoleChat       Role = "chat"
	RoleCompletion Role = "completion"
	RoleEmbedding  Role = "embedding"
)

// Backend is the serving substrate that loads the weights and answers tokens. LiteLLM is
// the gateway, never a backend -- it routes TO these.
type Backend string

const (
	BackendOllama       Backend = "ollama"       // decoder/chat GGUF via ollama (llama.cpp)
	BackendXinference   Backend = "xinference"   // xinference registry/serving
	BackendLlamaCPP     Backend = "llama-cpp"    // bare-metal llama.cpp server (Metal)
	BackendTransformers Backend = "transformers" // bare-metal HF transformers/MLX (seq2seq on Metal)
)

// TraefikRoute is the in-mesh routing contract delightd writes today (slated to graduate
// to xDS; see docs/model-hosting.md §future).
type TraefikRoute struct {
	Host        string `yaml:"host"`
	PathPrefix  string `yaml:"path_prefix,omitempty"`
	ServicePort int    `yaml:"service_port"`
}

// DeploymentDescriptor is one model availability signifier: a declaration that a model,
// at a location, served by a backend, is (or should be) reachable behind a route, with a
// known architecture and role.
type DeploymentDescriptor struct {
	Name             string       `yaml:"name"`
	Location         string       `yaml:"location"`
	Architecture     Architecture `yaml:"architecture"`
	Role             Role         `yaml:"role"`
	Backend          Backend      `yaml:"backend"`
	TraefikRoute     TraefikRoute `yaml:"traefik_route"`
	ContextWindow    *int         `yaml:"context_window,omitempty"`
	LiteLLMModelName string       `yaml:"litellm_model_name,omitempty"`
}

// Validate fails loud on an incoherent descriptor. The load-bearing guard: ollama is
// decoder-only, so a seq2seq/encoder model declared on ollama is a config-time error
// (the exact gap that blocks serving flan-t5 as if it were a chat decoder), not a
// runtime surprise.
func (d DeploymentDescriptor) Validate() error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("deployment: name is required")
	}
	if strings.TrimSpace(d.Location) == "" {
		return fmt.Errorf("deployment %q: location is required", d.Name)
	}
	if !d.Architecture.valid() {
		return fmt.Errorf("deployment %q: invalid architecture %q (want encode|encode-decode|decode)", d.Name, d.Architecture)
	}
	if !d.Role.valid() {
		return fmt.Errorf("deployment %q: invalid role %q (want chat|completion|embedding)", d.Name, d.Role)
	}
	if !d.Backend.valid() {
		return fmt.Errorf("deployment %q: invalid backend %q (want ollama|xinference|llama-cpp|transformers)", d.Name, d.Backend)
	}
	if strings.TrimSpace(d.TraefikRoute.Host) == "" {
		return fmt.Errorf("deployment %q: traefik_route.host is required", d.Name)
	}
	if d.TraefikRoute.ServicePort < 1 || d.TraefikRoute.ServicePort > 65535 {
		return fmt.Errorf("deployment %q: traefik_route.service_port %d out of range 1..65535", d.Name, d.TraefikRoute.ServicePort)
	}
	if d.ContextWindow != nil && *d.ContextWindow < 1 {
		return fmt.Errorf("deployment %q: context_window %d must be >= 1", d.Name, *d.ContextWindow)
	}
	if d.Backend == BackendOllama && d.Architecture != ArchDecode {
		return fmt.Errorf("deployment %q: backend 'ollama' serves decoder-only models; architecture %q must use 'xinference' or 'transformers'", d.Name, d.Architecture)
	}
	return nil
}

func (a Architecture) valid() bool {
	switch a {
	case ArchEncode, ArchEncodeDecode, ArchDecode:
		return true
	}
	return false
}

func (r Role) valid() bool {
	switch r {
	case RoleChat, RoleCompletion, RoleEmbedding:
		return true
	}
	return false
}

func (b Backend) valid() bool {
	switch b {
	case BackendOllama, BackendXinference, BackendLlamaCPP, BackendTransformers:
		return true
	}
	return false
}

// ResolvedLiteLLMModelName derives the provider-qualified name LiteLLM uses downstream
// when it is not set explicitly.
func (d DeploymentDescriptor) ResolvedLiteLLMModelName() string {
	if d.LiteLLMModelName != "" {
		return d.LiteLLMModelName
	}
	if d.Backend == BackendOllama {
		return "ollama/" + d.Location
	}
	// xinference exposes an openai-compatible shim; bare-metal backends front an
	// openai-compatible local server -- both qualify by name.
	return "openai/" + d.Name
}

// IsHFCacheLocation is a cheap heuristic (reporting only, never used to mutate an asset):
// an absolute path, a .gguf file, or a huggingface cache path.
func (d DeploymentDescriptor) IsHFCacheLocation() bool {
	loc := d.Location
	return strings.HasPrefix(loc, "/") || strings.HasSuffix(loc, ".gguf") || strings.Contains(loc, "huggingface")
}

// DeploymentSet is the declared set of model deployments delightd knows about.
type DeploymentSet struct {
	Deployments []DeploymentDescriptor `yaml:"deployments"`
}

// Validate validates every descriptor and rejects duplicate names.
func (s DeploymentSet) Validate() error {
	seen := make(map[string]bool, len(s.Deployments))
	for _, d := range s.Deployments {
		if err := d.Validate(); err != nil {
			return err
		}
		if seen[d.Name] {
			return fmt.Errorf("duplicate deployment name: %q", d.Name)
		}
		seen[d.Name] = true
	}
	return nil
}

// ByName returns the deployment with the given name, or ok=false.
func (s DeploymentSet) ByName(name string) (DeploymentDescriptor, bool) {
	for _, d := range s.Deployments {
		if d.Name == name {
			return d, true
		}
	}
	return DeploymentDescriptor{}, false
}

// LoadDeploymentSet reads and validates a deployment set from a YAML file. Unknown fields
// are rejected -- the contract is the guardrail (the Pydantic extra="forbid" parallel).
func LoadDeploymentSet(path string) (DeploymentSet, error) {
	f, err := os.Open(path)
	if err != nil {
		return DeploymentSet{}, fmt.Errorf("open deployment set %q: %w", path, err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	var set DeploymentSet
	if err := dec.Decode(&set); err != nil {
		return DeploymentSet{}, fmt.Errorf("parse deployment set %q: %w", path, err)
	}
	if err := set.Validate(); err != nil {
		return DeploymentSet{}, fmt.Errorf("invalid deployment set %q: %w", path, err)
	}
	return set, nil
}
