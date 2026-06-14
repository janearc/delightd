package traefik

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
	"delightd/pkg/discovery"
)

// DynamicConfig represents the Traefik dynamic file configuration structure
type DynamicConfig struct {
	Http struct {
		Routers  map[string]Router  `yaml:"routers,omitempty"`
		Services map[string]Service `yaml:"services,omitempty"`
	} `yaml:"http,omitempty"`
}

type Router struct {
	Rule    string `yaml:"rule"`
	Service string `yaml:"service"`
}

type Service struct {
	LoadBalancer LoadBalancer `yaml:"loadBalancer"`
}

type LoadBalancer struct {
	Servers []Server `yaml:"servers"`
}

type Server struct {
	URL string `yaml:"url"`
}

// SyncLLMRoutes generates and writes the Traefik config for discovered LLMs.
func SyncLLMRoutes(sources []discovery.ModelSource) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	configDir := filepath.Join(home, "var", "traefik", "dynamic")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return err
	}

	configFile := filepath.Join(configDir, "llms.yml")

	config := DynamicConfig{}
	config.Http.Routers = make(map[string]Router)
	config.Http.Services = make(map[string]Service)

	hasActive := false

	for idx, source := range sources {
		if !source.Healthy || len(source.Models) == 0 {
			continue
		}
		
		hasActive = true

		// Create a sanitized name for the service based on provider and port
		serviceName := fmt.Sprintf("llm-%s-%d", source.Provider, idx)
		
		config.Http.Services[serviceName] = Service{
			LoadBalancer: LoadBalancer{
				Servers: []Server{
					{URL: source.URL},
				},
			},
		}

		// Route all requests to this model provider via a unique path
		// e.g. models.local/v1/ollama or models.local/v1/llama-server
		config.Http.Routers[serviceName] = Router{
			Rule:    fmt.Sprintf("Host(`models.local`) && PathPrefix(`/llms/%s/`)", source.Provider),
			Service: serviceName,
		}
	}

	if !hasActive {
		// If no active models, remove the file so Traefik drops the routes.
		_ = os.Remove(configFile)
		return nil
	}

	data, err := yaml.Marshal(&config)
	if err != nil {
		return err
	}

	return os.WriteFile(configFile, data, 0644)
}
