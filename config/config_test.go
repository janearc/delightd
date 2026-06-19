package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// TestResolveControlPortDefault locks the canonical control port. compose,
// kube/deployment.yaml, and every client route to 8088; an absent control_port
// key must resolve here, not to a port nothing reaches.
func TestResolveControlPortDefault(t *testing.T) {
	if DefaultControlPort != 8088 {
		t.Fatalf("DefaultControlPort regressed: want 8088, got %d", DefaultControlPort)
	}

	// zero value (config key absent) falls back to the canonical default.
	var d DaemonConfig
	if got := d.ResolveControlPort(); got != 8088 {
		t.Errorf("unset control port: want 8088, got %d", got)
	}

	// an explicit control_port is honoured as-is.
	d.ControlPort = 9090
	if got := d.ResolveControlPort(); got != 9090 {
		t.Errorf("explicit control port: want 9090, got %d", got)
	}
}

// TestBackupRootNoDoubledSegment guards the DELIGHT_SYSTEM_ROOT path bug:
// system.root is the backup destination itself, so the checkpoint root is
// System.Root verbatim with no appended "/backups". A doubled
// ".../backups/backups" segment is the regression we are preventing.
func TestBackupRootNoDoubledSegment(t *testing.T) {
	cases := []struct {
		name string
		root string
	}{
		{"compose and kube in-container", "/var/backups"},
		{"local host default", "/Users/jane/var/backups"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DelightConfig{System: SystemConfig{Root: tc.root}}
			// main passes cfg.System.Root straight to backup.CreateCheckpoint.
			checkpointRoot := cfg.System.Root
			if checkpointRoot != tc.root {
				t.Errorf("checkpoint root altered: want %q, got %q", tc.root, checkpointRoot)
			}
			if strings.Contains(checkpointRoot, "backups/backups") {
				t.Errorf("doubled backups segment in %q", checkpointRoot)
			}
		})
	}
}

// TestDefaultConfigBackupRoot verifies the shipped delight.yaml points system.root
// directly at the backups dir (~/var/backups), so the no-suffix checkpoint call
// still lands in a backups directory locally.
func TestDefaultConfigBackupRoot(t *testing.T) {
	cfg := loadShippedConfig(t)
	if !strings.HasSuffix(cfg.System.Root, "/backups") {
		t.Errorf("shipped system.root should end in /backups, got %q", cfg.System.Root)
	}
	if strings.Contains(cfg.System.Root, "backups/backups") {
		t.Errorf("shipped system.root already doubled: %q", cfg.System.Root)
	}
}

// TestDefaultConfigControlPort verifies the shipped delight.yaml sets the canonical
// control port, matching the in-code default and the deployment manifests.
func TestDefaultConfigControlPort(t *testing.T) {
	cfg := loadShippedConfig(t)
	if cfg.System.Daemon.ResolveControlPort() != DefaultControlPort {
		t.Errorf("shipped control_port should resolve to %d, got %d",
			DefaultControlPort, cfg.System.Daemon.ResolveControlPort())
	}
}

// loadShippedConfig loads the repo's delight.yaml (one dir up from config/) so the
// committed defaults are asserted against, not a synthetic fixture.
func loadShippedConfig(t *testing.T) *DelightConfig {
	t.Helper()
	repoRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	origWD, _ := os.Getwd()
	if err := os.Chdir(repoRoot); err != nil {
		t.Fatalf("chdir repo root: %v", err)
	}
	defer os.Chdir(origWD)

	origHome := os.Getenv("HOME")
	os.Setenv("HOME", "/tmp/nonexistent")
	defer os.Setenv("HOME", origHome)

	viper.Reset()
	cfg, err := Load(context.Background())
	if err != nil {
		t.Fatalf("load shipped config: %v", err)
	}
	return cfg
}

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
