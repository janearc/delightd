package model

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func ollamaDeployment() DeploymentDescriptor {
	return DeploymentDescriptor{
		Name: "mistral-24b", Location: "mistral-24b:latest", Architecture: ArchDecode,
		Role: RoleChat, Backend: BackendOllama,
		TraefikRoute: TraefikRoute{Host: "mistral.localhost", ServicePort: 11434},
	}
}

func transformersDeployment() DeploymentDescriptor {
	return DeploymentDescriptor{
		Name: "flan-t5-large", Location: "/cache/flan", Architecture: ArchEncodeDecode,
		Role: RoleCompletion, Backend: BackendTransformers,
		TraefikRoute: TraefikRoute{Host: "flan.localhost", ServicePort: 9710},
	}
}

func TestBringUp_OllamaRegistersAndWarms(t *testing.T) {
	plan := ollamaDeployment().BringUp()
	joined := strings.Join(plan.IdempotentSteps, "\n")
	if !strings.Contains(joined, "ollama create") || !strings.Contains(joined, "--keepalive") {
		t.Fatalf("ollama up should register and warm; got:\n%s", joined)
	}
}

func TestBringUp_TransformersVerifiesOnDisk(t *testing.T) {
	plan := transformersDeployment().BringUp()
	joined := strings.Join(plan.IdempotentSteps, "\n")
	if !strings.Contains(joined, "test -e") {
		t.Fatalf("transformers up should verify weights on disk; got:\n%s", joined)
	}
	if !strings.Contains(plan.BindsTo, "in-process") {
		t.Fatalf("transformers should bind in-process, got %q", plan.BindsTo)
	}
}

func TestTeardown_OllamaUnloadsTransformersNoProcess(t *testing.T) {
	if got := strings.Join(ollamaDeployment().Teardown().IdempotentSteps, "\n"); !strings.Contains(got, "ollama stop") {
		t.Fatalf("ollama down should unload via ollama stop; got %q", got)
	}
	if got := strings.Join(transformersDeployment().Teardown().IdempotentSteps, "\n"); !strings.Contains(got, "nothing to stop") {
		t.Fatalf("transformers down should be a no-op; got %q", got)
	}
}

func TestHealthURL(t *testing.T) {
	if got := ollamaDeployment().HealthURL(); got != "http://mistral.localhost:11434/" {
		t.Fatalf("ollama health url: %q", got)
	}
	if got := transformersDeployment().HealthURL(); !strings.HasSuffix(got, "/health") {
		t.Fatalf("openai-shim health url should end /health, got %q", got)
	}
}

func TestProbeURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ok := ProbeURL(srv.URL, 2*time.Second)
	if !ok.Reachable || ok.Status != 200 {
		t.Fatalf("expected reachable 200, got %+v", ok)
	}

	down := ProbeURL("http://127.0.0.1:1/", 200*time.Millisecond)
	if down.Reachable {
		t.Fatalf("expected unreachable, got %+v", down)
	}
}
