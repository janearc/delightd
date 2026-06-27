// Package model is delightd's model-hosting subsystem: it treats each hosted model as a
// deployment -- a "model availability signifier" -- and reconciles it like any other
// asset.
//
// This file is the deployment descriptor: a model, where its weights resolve, the backend
// that serves it, its architecture and role, and the in-mesh route. architecture and role
// are the dimensions a flat model name cannot carry, and architecture decides which
// backend can host a model at all.
package model

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Architecture is the transformer topology; it decides which backend can host the model.
type Architecture string

const (
	ArchEncode       Architecture = "encode"        // encoder-only -- embeddings/classification
	ArchEncodeDecode Architecture = "encode-decode" // seq2seq (T5/flan) -- conditional generation
	ArchDecode       Architecture = "decode"        // decoder-only (mistral/llama) -- autoregressive chat
)

// Role is what the deployment is consumed as -- the contract the caller binds to.
type Role string

const (
	RoleChat       Role = "chat"
	RoleCompletion Role = "completion"
	RoleEmbedding  Role = "embedding"
)

// Backend is the serving substrate that loads the weights and answers tokens.
type Backend string

const (
	BackendOllama       Backend = "ollama"       // decoder/chat GGUF via ollama (llama.cpp)
	BackendXinference   Backend = "xinference"   // xinference registry/serving
	BackendLlamaCPP     Backend = "llama-cpp"    // bare-metal llama.cpp server (Metal)
	BackendTransformers Backend = "transformers" // bare-metal HF transformers/MLX
)

var (
	architectures = []Architecture{ArchEncode, ArchEncodeDecode, ArchDecode}
	roles         = []Role{RoleChat, RoleCompletion, RoleEmbedding}
	backends      = []Backend{BackendOllama, BackendXinference, BackendLlamaCPP, BackendTransformers}
)

// oneOf reports whether v is one of the allowed values -- the single validity check for
// every enum, rather than a hand-written method per type.
func oneOf[T comparable](v T, allowed []T) bool {
	for _, a := range allowed {
		if v == a {
			return true
		}
	}
	return false
}

// TraefikRoute is the in-mesh routing contract delightd writes (slated to graduate to
// xDS; see docs/model-hosting.md).
type TraefikRoute struct {
	Host        string `yaml:"host"`
	PathPrefix  string `yaml:"path_prefix,omitempty"`
	ServicePort int    `yaml:"service_port"`
}

func (r TraefikRoute) validate(deployment string) error {
	if strings.TrimSpace(r.Host) == "" {
		return fmt.Errorf("deployment %q: traefik_route.host is required", deployment)
	}
	if r.ServicePort < 1 || r.ServicePort > 65535 {
		return fmt.Errorf("deployment %q: traefik_route.service_port %d out of range 1..65535", deployment, r.ServicePort)
	}
	return nil
}

// DeploymentDescriptor is one model availability signifier: a model, at a location,
// served by a backend, reachable behind a route, with a known architecture and role.
type DeploymentDescriptor struct {
	Name             string       `yaml:"name"`
	Location         string       `yaml:"location"`
	Architecture     Architecture `yaml:"architecture"`
	Role             Role         `yaml:"role"`
	Backend          Backend      `yaml:"backend"`
	TraefikRoute     TraefikRoute `yaml:"traefik_route"`
	ContextWindow    int          `yaml:"context_window,omitempty"` // 0 means unset
	LiteLLMModelName string       `yaml:"litellm_model_name,omitempty"`
}

func (d DeploymentDescriptor) servedByOllama() bool { return d.Backend == BackendOllama }

// Validate fails loud on an incoherent descriptor -- a few independent checks.
func (d DeploymentDescriptor) Validate() error {
	if err := d.validateRequired(); err != nil {
		return err
	}
	if err := d.validateEnums(); err != nil {
		return err
	}
	if err := d.TraefikRoute.validate(d.Name); err != nil {
		return err
	}
	return d.validateCoherence()
}

func (d DeploymentDescriptor) validateRequired() error {
	if strings.TrimSpace(d.Name) == "" {
		return fmt.Errorf("deployment: name is required")
	}
	if strings.TrimSpace(d.Location) == "" {
		return fmt.Errorf("deployment %q: location is required", d.Name)
	}
	if d.ContextWindow < 0 {
		return fmt.Errorf("deployment %q: context_window %d must be >= 1", d.Name, d.ContextWindow)
	}
	return nil
}

func (d DeploymentDescriptor) validateEnums() error {
	if !oneOf(d.Architecture, architectures) {
		return fmt.Errorf("deployment %q: invalid architecture %q (want encode|encode-decode|decode)", d.Name, d.Architecture)
	}
	if !oneOf(d.Role, roles) {
		return fmt.Errorf("deployment %q: invalid role %q (want chat|completion|embedding)", d.Name, d.Role)
	}
	if !oneOf(d.Backend, backends) {
		return fmt.Errorf("deployment %q: invalid backend %q (want ollama|xinference|llama-cpp|transformers)", d.Name, d.Backend)
	}
	return nil
}

// validateCoherence is the load-bearing guard: ollama serves decoder-only models, so a
// seq2seq/encoder model on ollama is a config-time error (the flan-t5 / paling stage-4
// blocker), caught here rather than as a runtime surprise.
func (d DeploymentDescriptor) validateCoherence() error {
	if d.servedByOllama() && d.Architecture != ArchDecode {
		return fmt.Errorf("deployment %q: backend 'ollama' serves decoder-only models; architecture %q must use 'xinference' or 'transformers'", d.Name, d.Architecture)
	}
	return nil
}

// ResolvedLiteLLMModelName derives the provider-qualified name LiteLLM uses downstream
// when it is not set explicitly.
func (d DeploymentDescriptor) ResolvedLiteLLMModelName() string {
	if d.LiteLLMModelName != "" {
		return d.LiteLLMModelName
	}
	if d.servedByOllama() {
		return "ollama/" + d.Location
	}
	// xinference and the bare-metal backends front an openai-compatible server.
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
// are rejected: the contract is the guardrail.
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
