package model

import (
	"fmt"
	"net/http"
	"time"
)

// up/down emit deterministic, idempotent PLANS rather than executing. Bringing a model up
// (registering a GGUF, warming it into RAM) is an explicit operator action, and delightd
// does not touch the sacred assets. Real execution behind an --execute flag is a later
// increment; today the plans are runnable by hand or by an operator/fleet step.

// UpPlan is the idempotent recipe to realise a deployment, plus the metadata that says
// what it binds to and where it routes.
type UpPlan struct {
	Deployment       string       `json:"deployment"`
	Backend          Backend      `json:"backend"`
	Architecture     Architecture `json:"architecture"`
	Role             Role         `json:"role"`
	BindsTo          string       `json:"binds_to"`
	TraefikRoute     TraefikRoute `json:"traefik_route"`
	Location         string       `json:"location"`
	LiteLLMModelName string       `json:"litellm_model_name"`
	IdempotentSteps  []string     `json:"idempotent_steps"`
}

// DownPlan frees a deployment's RAM, leaving its registration and weights intact.
type DownPlan struct {
	Deployment      string   `json:"deployment"`
	Backend         Backend  `json:"backend"`
	IdempotentSteps []string `json:"idempotent_steps"`
}

// BringUp returns the steps to realise this deployment. ollama registers the GGUF (if it
// isn't already) and warms it into RAM; transformers only verifies the weights are on
// disk, since it runs in the consumer's process, not a server of its own.
func (d DeploymentDescriptor) BringUp() UpPlan {
	var steps []string
	var binds string
	switch d.Backend {
	case BackendOllama:
		steps = []string{
			fmt.Sprintf("ollama list | grep -q %q || ollama create %q -f modelfiles/%s.Modelfile", d.Location, d.Location, d.Name),
			fmt.Sprintf("ollama ps | grep -q %q || ollama run %q --keepalive 30m </dev/null", d.Location, d.Location),
		}
		binds = "ollama (bare-metal, Metal)"
	case BackendTransformers:
		steps = []string{
			fmt.Sprintf("test -e %q  # weights present for the in-process consumer to load", d.Location),
		}
		binds = "transformers/MLX, in-process in the consumer (no standalone server)"
	case BackendLlamaCPP:
		steps = []string{
			fmt.Sprintf("llama-server -m %q --host 127.0.0.1 --port %d -ngl 999", d.Location, d.TraefikRoute.ServicePort),
		}
		binds = "llama.cpp server (bare-metal, Metal)"
	case BackendXinference:
		steps = []string{"# xinference container register -- not wired yet"}
		binds = "xinference (container) -- not wired"
	}
	return UpPlan{
		Deployment: d.Name, Backend: d.Backend, Architecture: d.Architecture, Role: d.Role,
		BindsTo: binds, TraefikRoute: d.TraefikRoute, Location: d.Location,
		LiteLLMModelName: d.ResolvedLiteLLMModelName(), IdempotentSteps: steps,
	}
}

// Teardown returns the steps to free this deployment's RAM. ollama unloads but keeps the
// registration; the in-process transformers model has no process of its own to stop.
func (d DeploymentDescriptor) Teardown() DownPlan {
	var steps []string
	switch d.Backend {
	case BackendOllama:
		steps = []string{fmt.Sprintf("ollama stop %q  # unload from RAM; registration and weights stay", d.Location)}
	case BackendTransformers:
		steps = []string{"# nothing to stop -- the model is loaded inside the consumer's process"}
	case BackendLlamaCPP:
		steps = []string{fmt.Sprintf("pkill -f 'port %d'  # stop the bare-metal server", d.TraefikRoute.ServicePort)}
	case BackendXinference:
		steps = []string{"# xinference container stop -- not wired yet"}
	}
	return DownPlan{Deployment: d.Name, Backend: d.Backend, IdempotentSteps: steps}
}

// Probe is a read-only reachability result for one endpoint.
type Probe struct {
	URL       string `json:"url"`
	Reachable bool   `json:"reachable"`
	Status    int    `json:"status,omitempty"`
	Error     string `json:"error,omitempty"`
}

// HealthURL is where a backend answers a liveness GET: ollama replies to a bare GET at
// root ("Ollama is running"); the openai-compatible shims expose /health.
func (d DeploymentDescriptor) HealthURL() string {
	r := d.TraefikRoute
	if d.Backend == BackendOllama {
		return fmt.Sprintf("http://%s:%d/", r.Host, r.ServicePort)
	}
	return fmt.Sprintf("http://%s:%d/health", r.Host, r.ServicePort)
}

// ProbeURL does a read-only GET and reports whether the endpoint answered. An HTTP error
// status still counts as reachable (the endpoint responded). It never touches assets.
func ProbeURL(url string, timeout time.Duration) Probe {
	client := http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return Probe{URL: url, Reachable: false, Error: "unreachable"}
	}
	defer resp.Body.Close()
	return Probe{URL: url, Reachable: true, Status: resp.StatusCode}
}
