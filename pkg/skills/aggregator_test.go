package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAggregatorScan(t *testing.T) {
	tmpDir := t.TempDir()

	// Setup mock project with mcp.json
	projDir := filepath.Join(tmpDir, "odysseus")
	os.MkdirAll(projDir, 0755)

	mcpJSON := `{
		"tools": [
			{
				"name": "check_health",
				"description": "checks health",
				"inputSchema": {},
				"handler": {
					"type": "http",
					"method": "GET",
					"url": "http://localhost/health"
				}
			}
		]
	}`
	os.WriteFile(filepath.Join(projDir, "mcp.json"), []byte(mcpJSON), 0644)

	// Setup another project with broken syntax
	proj2Dir := filepath.Join(tmpDir, "broken")
	os.MkdirAll(proj2Dir, 0755)
	os.WriteFile(filepath.Join(proj2Dir, "mcp.json"), []byte(`{ broken json }`), 0644)

	agg := NewAggregator(tmpDir)

	err := agg.ScanProjects([]string{"odysseus", "broken", "nonexistent"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	tools := agg.GetTools()
	// Should have odysseus_check_health and delightd_trigger_backup
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}

	// Verify namespacing
	tool, exists := agg.GetTool("odysseus_check_health")
	if !exists {
		t.Errorf("expected odysseus_check_health to be registered")
	}
	if tool.Handler.Type != "http" {
		t.Errorf("expected http handler, got %s", tool.Handler.Type)
	}

	// Verify dogfooding tool
	_, exists = agg.GetTool("delightd_trigger_backup")
	if !exists {
		t.Errorf("expected delightd_trigger_backup to be registered")
	}

	// Test nonexistent tool
	_, exists = agg.GetTool("not_real")
	if exists {
		t.Errorf("expected false for nonexistent tool")
	}
}
