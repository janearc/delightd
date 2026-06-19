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

// TestResolveRootsDefaults verifies that an empty SystemConfig resolves every
// root to its documented default (with ~ expanded to $HOME) and that BackupsRoot
// derives as ${DaemonRoot}/backups when left unset.
func TestResolveRootsDefaults(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home: %v", err)
	}

	var s SystemConfig
	s.ResolveRoots()

	wantMonitor := filepath.Join(home, "work")
	wantDaemon := filepath.Join(home, "var")
	wantBackups := filepath.Join(home, "var", "backups")
	wantConfig := filepath.Join(home, "etc")

	if s.MonitorRoot != wantMonitor {
		t.Errorf("MonitorRoot: want %q, got %q", wantMonitor, s.MonitorRoot)
	}
	if s.DaemonRoot != wantDaemon {
		t.Errorf("DaemonRoot: want %q, got %q", wantDaemon, s.DaemonRoot)
	}
	if s.BackupsRoot != wantBackups {
		t.Errorf("BackupsRoot derived: want %q, got %q", wantBackups, s.BackupsRoot)
	}
	if s.ConfigRoot != wantConfig {
		t.Errorf("ConfigRoot: want %q, got %q", wantConfig, s.ConfigRoot)
	}
}

// TestResolveRootsBackupsDerivesFromDaemonRoot verifies BackupsRoot follows a
// relocated DaemonRoot when not set explicitly, and that an explicit BackupsRoot
// overrides the derivation rather than being recomputed from DaemonRoot.
func TestResolveRootsBackupsDerivesFromDaemonRoot(t *testing.T) {
	// derived: BackupsRoot unset -> ${DaemonRoot}/backups.
	derived := SystemConfig{DaemonRoot: "/srv/delight/var"}
	derived.ResolveRoots()
	if want := "/srv/delight/var/backups"; derived.BackupsRoot != want {
		t.Errorf("derived BackupsRoot: want %q, got %q", want, derived.BackupsRoot)
	}

	// explicit override wins, independent of DaemonRoot.
	override := SystemConfig{DaemonRoot: "/srv/delight/var", BackupsRoot: "/mnt/cold/backups"}
	override.ResolveRoots()
	if want := "/mnt/cold/backups"; override.BackupsRoot != want {
		t.Errorf("explicit BackupsRoot: want %q, got %q", want, override.BackupsRoot)
	}
	if strings.Contains(override.BackupsRoot, "backups/backups") {
		t.Errorf("doubled backups segment in %q", override.BackupsRoot)
	}
}

// TestRootEnvOverrides verifies each DELIGHT_*_ROOT env var is bound and flows
// through Load into the matching SystemConfig field, even with no config file
// present (the case BindEnv exists to cover).
func TestRootEnvOverrides(t *testing.T) {
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", "/tmp/nonexistent")
	defer os.Setenv("HOME", origHome)

	// point at a dir with no delight.yaml so Load falls back to env + defaults.
	tmpDir := t.TempDir()
	origWD, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(origWD)

	envs := map[string]string{
		"DELIGHT_MONITOR_ROOT": "/opt/projects",
		"DELIGHT_DAEMON_ROOT":  "/opt/state",
		"DELIGHT_BACKUPS_ROOT": "/opt/cold/backups",
		"DELIGHT_CONFIG_ROOT":  "/opt/conf",
	}
	for k, v := range envs {
		t.Setenv(k, v)
	}

	viper.Reset()
	cfg, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load config failed: %v", err)
	}

	if cfg.System.MonitorRoot != "/opt/projects" {
		t.Errorf("MonitorRoot env override: got %q", cfg.System.MonitorRoot)
	}
	if cfg.System.DaemonRoot != "/opt/state" {
		t.Errorf("DaemonRoot env override: got %q", cfg.System.DaemonRoot)
	}
	if cfg.System.BackupsRoot != "/opt/cold/backups" {
		t.Errorf("BackupsRoot env override: got %q", cfg.System.BackupsRoot)
	}
	if cfg.System.ConfigRoot != "/opt/conf" {
		t.Errorf("ConfigRoot env override: got %q", cfg.System.ConfigRoot)
	}
}

// TestDefaultConfigBackupsRoot verifies the shipped delight.yaml leaves
// backups_root unset and that it derives to a "backups" directory under
// daemon_root (~/var) with no doubled segment.
func TestDefaultConfigBackupsRoot(t *testing.T) {
	cfg := loadShippedConfig(t)
	if !strings.HasSuffix(cfg.System.BackupsRoot, "/backups") {
		t.Errorf("shipped backups_root should end in /backups, got %q", cfg.System.BackupsRoot)
	}
	if strings.Contains(cfg.System.BackupsRoot, "backups/backups") {
		t.Errorf("shipped backups_root doubled: %q", cfg.System.BackupsRoot)
	}
	// it should derive from daemon_root, i.e. ${DaemonRoot}/backups.
	if want := filepath.Join(cfg.System.DaemonRoot, "backups"); cfg.System.BackupsRoot != want {
		t.Errorf("shipped backups_root should derive from daemon_root: want %q, got %q",
			want, cfg.System.BackupsRoot)
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
  monitor_root: "/tmp/work"
  daemon_root: "/tmp/var"
  config_root: "/tmp/etc"
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

	if cfg.System.MonitorRoot != "/tmp/work" {
		t.Errorf("expected MonitorRoot to be /tmp/work, got %s", cfg.System.MonitorRoot)
	}
	if cfg.System.DaemonRoot != "/tmp/var" {
		t.Errorf("expected DaemonRoot to be /tmp/var, got %s", cfg.System.DaemonRoot)
	}
	// backups_root unset in this yaml -> derives from daemon_root.
	if cfg.System.BackupsRoot != "/tmp/var/backups" {
		t.Errorf("expected BackupsRoot to derive to /tmp/var/backups, got %s", cfg.System.BackupsRoot)
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
