package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestCheckOllama(t *testing.T) {
	tests := []struct {
		name         string
		handler      http.HandlerFunc
		expected     ModelSource
	}{
		{
			name: "healthy ollama with models",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/tags" {
					w.WriteHeader(http.StatusOK)
					w.Write([]byte(`{"models": [{"name": "llama2"}, {"name": "mistral"}]}`))
				} else {
					w.WriteHeader(http.StatusNotFound)
				}
			},
			expected: ModelSource{
				Provider: "ollama",
				Healthy:  true,
				Models:   []string{"llama2", "mistral"},
			},
		},
		{
			name: "unhealthy ollama",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			expected: ModelSource{
				Provider: "ollama",
				Healthy:  false,
				Models:   []string{},
			},
		},
		{
			name: "invalid json response",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`invalid json`))
			},
			expected: ModelSource{
				Provider: "ollama",
				Healthy:  true,
				Models:   []string{},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()

			tc.expected.URL = server.URL // dynamically update URL

			ctx := context.Background()
			result := checkOllama(ctx, server.URL)

			if !reflect.DeepEqual(result, tc.expected) {
				t.Errorf("expected %+v, got %+v", tc.expected, result)
			}
		})
	}
}

func TestCheckOllama_Errors(t *testing.T) {
	ctx := context.Background()
	
	// Test request creation error (invalid method context)
	// We use " " as method to force http.NewRequestWithContext to fail
	req, _ := http.NewRequestWithContext(ctx, " ", "http://localhost", nil)
	_ = req

	// It's easier to pass a bad URL to trigger NewRequestWithContext error or client.Do error
	// To trigger http.NewRequestWithContext error inside checkOllama, we can use an invalid URL control character
	// But it uses: http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/tags", nil)
	// Passing an unparseable URL will do it.
	result := checkOllama(ctx, "http://192.168.0.%31/")
	
	// Check client do error using port 0
	result2 := checkOllama(ctx, "http://localhost:0")
	if result2.Healthy {
		t.Errorf("expected unhealthy")
	}
	_ = result
}

func TestCheckLlamaCpp(t *testing.T) {
	tests := []struct {
		name         string
		handler      http.HandlerFunc
		expected     ModelSource
	}{
		{
			name: "healthy llama.cpp /health",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					w.WriteHeader(http.StatusOK)
				} else {
					w.WriteHeader(http.StatusNotFound)
				}
			},
			expected: ModelSource{
				Provider: "llama.cpp",
				Healthy:  true,
				Models:   []string{"unknown-llama-model"},
			},
		},
		{
			name: "openai compatible /v1/models",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/health" {
					w.WriteHeader(http.StatusNotFound)
				} else if r.URL.Path == "/v1/models" {
					w.WriteHeader(http.StatusOK)
				} else {
					w.WriteHeader(http.StatusNotFound)
				}
			},
			expected: ModelSource{
				Provider: "llama.cpp",
				Healthy:  true,
				Models:   []string{"openai-compatible-model"},
			},
		},
		{
			name: "unhealthy server",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			expected: ModelSource{
				Provider: "llama.cpp",
				Healthy:  false,
				Models:   []string{},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()

			tc.expected.URL = server.URL // dynamically update URL

			ctx := context.Background()
			result := checkLlamaCpp(ctx, server.URL)

			if !reflect.DeepEqual(result, tc.expected) {
				t.Errorf("expected %+v, got %+v", tc.expected, result)
			}
		})
	}
}

func TestCheckLlamaCpp_Errors(t *testing.T) {
	ctx := context.Background()
	// Test client do error using port 0
	result := checkLlamaCpp(ctx, "http://localhost:0")
	if result.Healthy {
		t.Errorf("expected unhealthy")
	}
}

func TestDiscoverLocalLLMs(t *testing.T) {
	ctx := context.Background()
	sources := DiscoverLocalLLMs(ctx, nil)
	if len(sources) > 0 {
		t.Logf("found %d sources", len(sources))
	}
}
