package gen

import (
	"path/filepath"
	"testing"

	"delightd/pkg/skills"
)

// TestDelightdSkillManifest asserts the shipped mcp.json parses and declares delightd's
// own agent tools. This enforces the doctrine "a capability is not done until it emits
// JSON and has a wrapper and a skill": the furnish operator surface cannot ship without
// its agent skill, and a malformed mcp.json fails the build rather than silently leaving
// the daemon with no self-tools.
func TestDelightdSkillManifest(t *testing.T) {
	agg := skills.NewAggregator(t.TempDir())
	agg.SetSelfManifest(filepath.Join(repoRoot(t), "mcp.json"))
	if err := agg.ScanProjects(nil); err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, want := range []string{
		"delightd_furnish_list",
		"delightd_furnish_health",
		"delightd_furnish_up",
		"delightd_furnish_down",
		"delightd_trigger_backup",
		"delightd_reset",
	} {
		tool, ok := agg.GetTool(want)
		if !ok {
			t.Errorf("mcp.json missing tool %s", want)
			continue
		}
		if tool.Handler.Type != "http" {
			t.Errorf("%s handler = %q, want http", want, tool.Handler.Type)
		}
	}
}
