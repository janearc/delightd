package model

import "fmt"

// LiteLLMConfig is the LiteLLM proxy config DERIVED from the deployment set, so the
// deployments stay the one source of truth and the LiteLLM model_list is never hand-
// maintained alongside them (the "derive, don't hardcode" rule).
type LiteLLMConfig struct {
	ModelList       []LiteLLMModel `json:"model_list" yaml:"model_list"`
	GeneralSettings map[string]any `json:"general_settings" yaml:"general_settings"`
	LiteLLMSettings map[string]any `json:"litellm_settings" yaml:"litellm_settings"`
}

// LiteLLMModel is one model_list entry: the {model_name, litellm_params} LiteLLM
// understands, plus model_info metadata (architecture/role/backend) so a downstream
// router sees the fleet dimensions without reparsing the deployment set.
type LiteLLMModel struct {
	ModelName     string         `json:"model_name" yaml:"model_name"`
	LiteLLMParams map[string]any `json:"litellm_params" yaml:"litellm_params"`
	ModelInfo     map[string]any `json:"model_info" yaml:"model_info"`
}

// litellmParams maps a descriptor to LiteLLM params. Backends differ in how LiteLLM
// reaches them: ollama has a native provider, while the others front an openai-compatible
// shim reached via api_base.
func (d DeploymentDescriptor) litellmParams() map[string]any {
	host, port := d.TraefikRoute.Host, d.TraefikRoute.ServicePort
	params := map[string]any{"model": d.ResolvedLiteLLMModelName()}
	if d.servedByOllama() {
		params["api_base"] = fmt.Sprintf("http://%s:%d", host, port)
	} else {
		params["api_base"] = fmt.Sprintf("http://%s:%d%s", host, port, d.TraefikRoute.PathPrefix)
		params["api_key"] = "sk-local" // local mesh, non-secret placeholder
	}
	if d.ContextWindow > 0 {
		params["max_tokens"] = d.ContextWindow
	}
	return params
}

// RenderLiteLLM derives the LiteLLM proxy config from a deployment set.
func RenderLiteLLM(set DeploymentSet) LiteLLMConfig {
	models := make([]LiteLLMModel, 0, len(set.Deployments))
	for _, d := range set.Deployments {
		models = append(models, LiteLLMModel{
			ModelName:     d.Name,
			LiteLLMParams: d.litellmParams(),
			ModelInfo: map[string]any{
				"architecture": string(d.Architecture),
				"role":         string(d.Role),
				"backend":      string(d.Backend),
				"location":     d.Location,
				"traefik_host": d.TraefikRoute.Host,
			},
		})
	}
	return LiteLLMConfig{
		ModelList:       models,
		GeneralSettings: map[string]any{"telemetry": false},
		LiteLLMSettings: map[string]any{"callbacks": []string{"prometheus"}, "drop_params": true},
	}
}
