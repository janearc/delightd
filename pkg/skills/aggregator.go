package skills

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Aggregator manages the discovery and invocation of skills across the fleet.
type Aggregator struct {
	mu      sync.RWMutex
	workDir string
	tools   map[string]Tool
}

// NewAggregator creates a new thread-safe skill aggregator
func NewAggregator(workDir string) *Aggregator {
	return &Aggregator{
		workDir: workDir,
		tools:   make(map[string]Tool),
	}
}

// ScanProjects searches all managed projects for mcp.json files and registers their tools
func (a *Aggregator) ScanProjects(projects []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Clear existing tools
	a.tools = make(map[string]Tool)

	for _, proj := range projects {
		mcpFile := filepath.Join(a.workDir, proj, "mcp.json")
		b, err := os.ReadFile(mcpFile)
		if err != nil {
			if os.IsNotExist(err) {
				continue // project doesn't have an mcp.json
			}
			slog.Warn("failed to read mcp.json", "project", proj, "error", err)
			continue
		}

		var manifest Manifest
		if err := json.Unmarshal(b, &manifest); err != nil {
			slog.Warn("invalid mcp.json syntax", "project", proj, "error", err)
			continue
		}

		for _, t := range manifest.Tools {
			// Namespace the tool by project name to avoid collisions
			toolName := proj + "_" + t.Name
			t.Name = toolName
			a.tools[toolName] = t
			slog.Info("registered agent skill", "project", proj, "tool", toolName)
		}
	}

	// Always inject the internal dogfooding tool: delightd_trigger_backup
	a.tools["delightd_trigger_backup"] = Tool{
		Name:        "delightd_trigger_backup",
		Description: "Manually trigger an immediate backup for a specific project.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"project":{"type":"string","description":"Name of the project to backup"}},"required":["project"]}`),
		Handler: HandlerDef{
			Type:   "internal",
			Method: "backup",
		},
	}

	return nil
}

// GetTools returns a copy of all registered tools
func (a *Aggregator) GetTools() []Tool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	var result []Tool
	for _, t := range a.tools {
		result = append(result, t)
	}
	return result
}

// GetTool looks up a specific tool by name
func (a *Aggregator) GetTool(name string) (Tool, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	t, exists := a.tools[name]
	return t, exists
}
