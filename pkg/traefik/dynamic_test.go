package traefik

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"delightd/pkg/discovery"
)

func TestSyncLLMRoutes(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("HOME", tempDir)

	sources := []discovery.ModelSource{
		{
			Provider: "ollama",
			URL:      "http://localhost:11434",
			Models:   []string{"llama2"},
			Healthy:  true,
		},
		{
			Provider: "llama.cpp",
			URL:      "http://localhost:8000",
			Models:   []string{"unknown-llama-model"},
			Healthy:  true,
		},
		{
			Provider: "unhealthy",
			URL:      "http://localhost:8001",
			Models:   []string{"model"},
			Healthy:  false,
		},
		{
			Provider: "empty-models",
			URL:      "http://localhost:8002",
			Models:   []string{},
			Healthy:  true,
		},
	}

	err := SyncLLMRoutes(sources)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	configDir := filepath.Join(tempDir, "var", "traefik", "dynamic")
	configFile := filepath.Join(configDir, "llms.yml")

	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("expected config file to be created: %v", err)
	}

	var config DynamicConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatalf("failed to unmarshal config: %v", err)
	}

	if len(config.Http.Services) != 2 {
		t.Errorf("expected 2 services, got %d", len(config.Http.Services))
	}
	if len(config.Http.Routers) != 2 {
		t.Errorf("expected 2 routers, got %d", len(config.Http.Routers))
	}

	// Test removal
	emptySources := []discovery.ModelSource{}
	err = SyncLLMRoutes(emptySources)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, err := os.Stat(configFile); !os.IsNotExist(err) {
		t.Errorf("expected config file to be removed, but it exists or error is different: %v", err)
	}
}
