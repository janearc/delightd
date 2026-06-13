package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
)

func TestLoadConfig(t *testing.T) {
	tmpDir := t.TempDir()

	yamlContent := `
system:
  root: "/tmp/var"
  agent_skills:
    enabled: true
    expose_via: ["cli", "mcp"]
projects:
  - name: "test-proj"
    path: "/tmp/work/test-proj"
`
	configPath := filepath.Join(tmpDir, "delight.yaml")
	if err := os.WriteFile(configPath, []byte(yamlContent), 0644); err != nil {
		t.Fatalf("failed to write mock config: %v", err)
	}

	origWD, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(origWD)

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", "/tmp/nonexistent")
	defer os.Setenv("HOME", origHome)

	// Reset viper for tests since it's global
	viper.Reset()

	cfg, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load config failed: %v", err)
	}

	if cfg.System.Root != "/tmp/var" {
		t.Errorf("expected System.Root to be /tmp/var, got %s", cfg.System.Root)
	}

	if !cfg.System.AgentSkills.Enabled {
		t.Errorf("expected AgentSkills to be enabled")
	}
	if len(cfg.System.AgentSkills.ExposeVia) != 2 {
		t.Errorf("expected 2 expose_via methods")
	}

	if len(cfg.Projects) != 1 || cfg.Projects[0].Name != "test-proj" {
		t.Errorf("expected 1 project named test-proj")
	}
}
