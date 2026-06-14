package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// ModelSource represents a discovered LLM service provider and its available models.
type ModelSource struct {
	Provider string   `json:"provider"`
	URL      string   `json:"url"`
	Models   []string `json:"models"`
	Healthy  bool     `json:"healthy"`
}

// OllamaTagsResponse represents the /api/tags response from Ollama
type OllamaTagsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

// DiscoverLocalLLMs checks standard ports for known local LLM servers.
func DiscoverLocalLLMs(ctx context.Context) []ModelSource {
	var sources []ModelSource

	// 1. Check Ollama (default port 11434)
	if ollama := checkOllama(ctx, "http://localhost:11434"); ollama.Healthy {
		sources = append(sources, ollama)
	}

	// 2. Check llama.cpp or compatible OpenAI endpoints (common ports 8000-8020)
	for port := 8000; port <= 8020; port++ {
		url := fmt.Sprintf("http://localhost:%d", port)
		if llamaCpp := checkLlamaCpp(ctx, url); llamaCpp.Healthy {
			sources = append(sources, llamaCpp)
		}
	}

	return sources
}

func checkOllama(ctx context.Context, baseURL string) ModelSource {
	source := ModelSource{
		Provider: "ollama",
		URL:      baseURL,
		Healthy:  false,
		Models:   []string{},
	}

	client := http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	if err != nil {
		return source
	}

	resp, err := client.Do(req)
	if err != nil {
		return source
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		source.Healthy = true
		var tags OllamaTagsResponse
		if err := json.NewDecoder(resp.Body).Decode(&tags); err == nil {
			for _, m := range tags.Models {
				source.Models = append(source.Models, m.Name)
			}
		}
	}
	return source
}

func checkLlamaCpp(ctx context.Context, baseURL string) ModelSource {
	source := ModelSource{
		Provider: "llama.cpp",
		URL:      baseURL,
		Healthy:  false,
		Models:   []string{},
	}

	// We simply check if the server is responsive.
	// We'll try the /health endpoint which is common for llama.cpp server.
	client := http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/health", nil)
	if err != nil {
		return source
	}

	resp, err := client.Do(req)
	if err == nil {
		defer resp.Body.Close()
	}

	if err == nil && resp.StatusCode == http.StatusOK {
		source.Healthy = true
		source.Models = append(source.Models, "unknown-llama-model")
	} else {
		// Fallback: try /v1/models for OpenAI API compatible servers
		req, _ = http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/models", nil)
		resp2, err2 := client.Do(req)
		if err2 == nil {
			defer resp2.Body.Close()
			if resp2.StatusCode == http.StatusOK {
				source.Healthy = true
				source.Models = append(source.Models, "openai-compatible-model")
			}
		}
	}

	return source
}
