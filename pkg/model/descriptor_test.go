package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validOllamaDeployment() DeploymentDescriptor {
	return DeploymentDescriptor{
		Name: "mistral-24b", Location: "mistral-24b:latest",
		Architecture: ArchDecode, Role: RoleChat, Backend: BackendOllama,
		TraefikRoute: TraefikRoute{Host: "mistral-24b.models.localhost", ServicePort: 11434},
	}
}

func TestValidate_OllamaRejectsNonDecoder(t *testing.T) {
	d := validOllamaDeployment()
	d.Architecture = ArchEncodeDecode // flan-on-ollama: the paling stage-4 blocker
	err := d.Validate()
	if err == nil {
		t.Fatal("expected ollama + encode-decode to be rejected")
	}
	if !strings.Contains(err.Error(), "decoder-only") {
		t.Fatalf("expected a decoder-only message, got %v", err)
	}
}

func TestValidate_OllamaDecodeIsOK(t *testing.T) {
	if err := validOllamaDeployment().Validate(); err != nil {
		t.Fatalf("expected valid, got %v", err)
	}
}

func TestValidate_RejectsBadEnumsAndPort(t *testing.T) {
	cases := map[string]func(*DeploymentDescriptor){
		"bad role":     func(d *DeploymentDescriptor) { d.Role = "vibes" },
		"bad backend":  func(d *DeploymentDescriptor) { d.Backend = "vllm" },
		"bad arch":     func(d *DeploymentDescriptor) { d.Architecture = "quantum" },
		"port too big": func(d *DeploymentDescriptor) { d.TraefikRoute.ServicePort = 70000 },
		"port zero":    func(d *DeploymentDescriptor) { d.TraefikRoute.ServicePort = 0 },
		"no host":      func(d *DeploymentDescriptor) { d.TraefikRoute.Host = "" },
		"no name":      func(d *DeploymentDescriptor) { d.Name = "" },
		"no location":  func(d *DeploymentDescriptor) { d.Location = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := validOllamaDeployment()
			mutate(&d)
			if err := d.Validate(); err == nil {
				t.Fatalf("%s: expected validation error", name)
			}
		})
	}
}

func TestResolvedLiteLLMModelName(t *testing.T) {
	ollama := validOllamaDeployment()
	if got := ollama.ResolvedLiteLLMModelName(); got != "ollama/mistral-24b:latest" {
		t.Fatalf("ollama derive: got %q", got)
	}

	flan := DeploymentDescriptor{
		Name: "flan-t5-large", Location: "/cache/flan", Architecture: ArchEncodeDecode,
		Role: RoleCompletion, Backend: BackendTransformers,
		TraefikRoute: TraefikRoute{Host: "flan.localhost", ServicePort: 9710},
	}
	if got := flan.ResolvedLiteLLMModelName(); got != "openai/flan-t5-large" {
		t.Fatalf("transformers derive: got %q", got)
	}

	flan.LiteLLMModelName = "openai/custom"
	if got := flan.ResolvedLiteLLMModelName(); got != "openai/custom" {
		t.Fatalf("explicit override: got %q", got)
	}
}

func TestDeploymentSet_RejectsDuplicateNames(t *testing.T) {
	s := DeploymentSet{Deployments: []DeploymentDescriptor{validOllamaDeployment(), validOllamaDeployment()}}
	if err := s.Validate(); err == nil {
		t.Fatal("expected duplicate-name rejection")
	}
}

const exampleYAML = `
deployments:
  - name: mistral-24b
    location: "mistral-24b:latest"
    architecture: decode
    role: chat
    backend: ollama
    context_window: 32768
    litellm_model_name: "ollama/mistral-24b:latest"
    traefik_route:
      host: "mistral-24b.models.localhost"
      path_prefix: "/v1"
      service_port: 11434
  - name: flan-t5-large
    location: "/cache/flan-t5-large/snapshots/abc"
    architecture: encode-decode
    role: completion
    backend: transformers
    traefik_route:
      host: "flan-t5-large.models.localhost"
      service_port: 9710
`

func TestLoadDeploymentSet(t *testing.T) {
	path := writeTemp(t, "deployments.yaml", exampleYAML)
	set, err := LoadDeploymentSet(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(set.Deployments) != 2 {
		t.Fatalf("want 2 deployments, got %d", len(set.Deployments))
	}
	flan, ok := set.ByName("flan-t5-large")
	if !ok {
		t.Fatal("flan-t5-large not found by name")
	}
	if flan.Backend != BackendTransformers || flan.Architecture != ArchEncodeDecode {
		t.Fatalf("flan parsed wrong: %+v", flan)
	}
}

func TestLoadDeploymentSet_RejectsUnknownField(t *testing.T) {
	bad := strings.Replace(exampleYAML, "    role: chat", "    role: chat\n    flavor: spicy", 1)
	path := writeTemp(t, "bad.yaml", bad)
	if _, err := LoadDeploymentSet(path); err == nil {
		t.Fatal("expected unknown field 'flavor' to be rejected")
	}
}

func TestLoadDeploymentSet_RejectsIncoherentDeployment(t *testing.T) {
	bad := strings.Replace(exampleYAML, "    architecture: decode", "    architecture: encode-decode", 1)
	path := writeTemp(t, "incoherent.yaml", bad)
	if _, err := LoadDeploymentSet(path); err == nil {
		t.Fatal("expected ollama + encode-decode to be rejected on load")
	}
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	return path
}
