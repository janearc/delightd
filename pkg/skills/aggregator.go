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
	mu           sync.RWMutex
	workDir      string
	selfManifest string
	tools        map[string]Tool
}

// NewAggregator creates a new thread-safe skill aggregator
func NewAggregator(workDir string) *Aggregator {
	return &Aggregator{
		workDir: workDir,
		tools:   make(map[string]Tool),
	}
}

// SetSelfManifest points the aggregator at delightd's OWN mcp.json (baked into the image
// at <ConfigRoot>/mcp.json). delightd dogfoods the same skill contract it requires of every
// fleet project: its tools (furnish, backup, reset) are declared there and registered under
// the "delightd" namespace, not hardcoded. Empty path -> delightd contributes no self-tools.
func (a *Aggregator) SetSelfManifest(path string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.selfManifest = path
}

// ScanProjects searches all managed projects for mcp.json files and registers their tools,
// then loads delightd's own manifest (the self-manifest) under the "delightd" namespace.
func (a *Aggregator) ScanProjects(projects []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Clear existing tools
	a.tools = make(map[string]Tool)

	for _, proj := range projects {
		a.loadManifest(filepath.Join(a.workDir, proj, "mcp.json"), proj)
	}

	// delightd's own tools come from its baked mcp.json, dogfooding the fleet contract
	// rather than a hardcoded injection. Absent path or file -> no self-tools (fail soft).
	if a.selfManifest != "" {
		a.loadManifest(a.selfManifest, "delightd")
	}

	return nil
}

// loadManifest reads one mcp.json and registers its tools under prefix (namespaced
// prefix_tool). A missing file is silent; an unreadable or malformed one is logged and
// skipped, so one bad manifest never breaks the aggregate. Caller holds a.mu.
func (a *Aggregator) loadManifest(path, prefix string) {
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("failed to read mcp.json", "source", prefix, "path", path, "error", err)
		}
		return
	}
	var manifest Manifest
	if err := json.Unmarshal(b, &manifest); err != nil {
		slog.Warn("invalid mcp.json syntax", "source", prefix, "path", path, "error", err)
		return
	}
	for _, t := range manifest.Tools {
		toolName := prefix + "_" + t.Name
		t.Name = toolName
		a.tools[toolName] = t
		slog.Info("registered agent skill", "source", prefix, "tool", toolName)
	}
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
