package model

import "testing"

func TestRenderLiteLLM(t *testing.T) {
	ctx := 32768
	set := DeploymentSet{Deployments: []DeploymentDescriptor{
		{
			Name: "mistral-24b", Location: "mistral-24b:latest", Architecture: ArchDecode,
			Role: RoleChat, Backend: BackendOllama, ContextWindow: ctx,
			LiteLLMModelName: "ollama/mistral-24b:latest",
			TraefikRoute:     TraefikRoute{Host: "mistral.localhost", ServicePort: 11434},
		},
		{
			Name: "flan-t5-large", Location: "/cache/flan", Architecture: ArchEncodeDecode,
			Role: RoleCompletion, Backend: BackendTransformers,
			TraefikRoute: TraefikRoute{Host: "flan.localhost", PathPrefix: "/v1", ServicePort: 9710},
		},
	}}

	cfg := RenderLiteLLM(set)
	if len(cfg.ModelList) != 2 {
		t.Fatalf("want 2 entries, got %d", len(cfg.ModelList))
	}

	// ollama: native provider, api_base without path prefix, no api_key, max_tokens set.
	mistral := cfg.ModelList[0]
	p := mistral.LiteLLMParams
	if p["model"] != "ollama/mistral-24b:latest" {
		t.Fatalf("mistral model: %v", p["model"])
	}
	if p["api_base"] != "http://mistral.localhost:11434" {
		t.Fatalf("mistral api_base: %v", p["api_base"])
	}
	if _, hasKey := p["api_key"]; hasKey {
		t.Fatal("ollama should not carry an api_key")
	}
	if p["max_tokens"] != 32768 {
		t.Fatalf("mistral max_tokens: %v", p["max_tokens"])
	}
	if mistral.ModelInfo["architecture"] != "decode" {
		t.Fatalf("mistral model_info.architecture: %v", mistral.ModelInfo["architecture"])
	}

	// transformers: openai shim, api_base with path prefix, api_key present, no max_tokens.
	flan := cfg.ModelList[1]
	fp := flan.LiteLLMParams
	if fp["model"] != "openai/flan-t5-large" {
		t.Fatalf("flan model: %v", fp["model"])
	}
	if fp["api_base"] != "http://flan.localhost:9710/v1" {
		t.Fatalf("flan api_base: %v", fp["api_base"])
	}
	if _, hasKey := fp["api_key"]; !hasKey {
		t.Fatal("openai-shim backend should carry an api_key")
	}
	if _, hasMax := fp["max_tokens"]; hasMax {
		t.Fatal("flan has no context_window, should have no max_tokens")
	}
}
