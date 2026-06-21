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

// TestShippedConfigEnablesAgentSkills verifies the shipped delight.yaml actually
// turns the skill generator on. The feature is fully implemented and tested, but
// when agent_skills is absent from the config it defaults off, and the daemon
// skips generation: the ~/var/bin/delight wrapper is never written and POST /mcp
// is never registered. That is precisely the regression that shipped once -- the
// claim ("delightd generates skills") and the behaviour drifted -- so guard it.
func TestShippedConfigEnablesAgentSkills(t *testing.T) {
	cfg := loadShippedConfig(t)
	if !cfg.System.AgentSkills.Enabled {
		t.Fatal("shipped delight.yaml must enable agent_skills; with it off the daemon never generates the CLI wrapper or registers /mcp")
	}
	got := map[string]bool{}
	for _, m := range cfg.System.AgentSkills.ExposeVia {
		got[m] = true
	}
	for _, want := range []string{"cli", "mcp"} {
		if !got[want] {
			t.Errorf("shipped agent_skills.expose_via should include %q, got %v", want, cfg.System.AgentSkills.ExposeVia)
		}
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

// loadFromYAML writes the given delight.yaml into a temp dir and loads it the way
// the daemon does (config discovered on the working dir; HOME pointed away so the
// real ~/etc/delightd is never consulted). It mirrors the existing config tests.
func loadFromYAML(t *testing.T, yamlContent string) *DelightConfig {
	t.Helper()
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "delight.yaml"), []byte(yamlContent), 0644); err != nil {
		t.Fatalf("write mock config: %v", err)
	}
	origWD, _ := os.Getwd()
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(origWD) })
	t.Setenv("HOME", filepath.Join(tmpDir, "nonexistent"))
	viper.Reset()

	cfg, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load returned an error (it should degrade, not fail): %v", err)
	}
	return cfg
}

// TestLoadDropsInvalidProjects: obviously-unusable entries (no name, no path,
// duplicate name) are rejected at load with the daemon marked degraded, while the
// valid projects survive. This is the odysseus-class fix: a bad entry no longer
// poisons the set.
func TestLoadDropsInvalidProjects(t *testing.T) {
	cfg := loadFromYAML(t, `
system:
  monitor_root: "/tmp/work"
projects:
  - name: "good"
    path: "/tmp/work/good"
  - name: ""
    path: "/tmp/work/noname"
  - name: "nopath"
    path: ""
  - name: "good"
    path: "/tmp/work/dup"
`)
	if len(cfg.Projects) != 1 || cfg.Projects[0].Name != "good" {
		t.Fatalf("expected only the valid 'good' project, got %+v", cfg.Projects)
	}
	if !cfg.Degraded {
		t.Error("expected Degraded=true after dropping invalid entries")
	}
	if len(cfg.LoadWarnings) != 3 {
		t.Errorf("expected 3 load warnings (noname, nopath, duplicate), got %d: %v", len(cfg.LoadWarnings), cfg.LoadWarnings)
	}
}

// TestLoadDegradesOnParseError: a syntactically broken delight.yaml must not abort
// startup. Load returns a usable (empty, degraded) config instead of an error --
// the availability mandate: come up in any condition.
func TestLoadDegradesOnParseError(t *testing.T) {
	cfg := loadFromYAML(t, "projects:\n  - name: \"broken\"\n    path: [unterminated\n")
	if !cfg.Degraded {
		t.Error("expected Degraded=true on a parse error")
	}
	if len(cfg.Projects) != 0 {
		t.Errorf("expected no projects from an unparseable file, got %d", len(cfg.Projects))
	}
	if len(cfg.LoadWarnings) == 0 {
		t.Error("expected a load warning explaining the parse failure")
	}
}

// TestLoadCleanConfigIsNotDegraded: the happy path stays clean -- no false degraded.
func TestLoadCleanConfigIsNotDegraded(t *testing.T) {
	cfg := loadFromYAML(t, `
projects:
  - name: "alpha"
    path: "/tmp/work/alpha"
`)
	if cfg.Degraded {
		t.Errorf("clean config should not be degraded, warnings=%v", cfg.LoadWarnings)
	}
	if len(cfg.Projects) != 1 {
		t.Fatalf("expected 1 project, got %d", len(cfg.Projects))
	}
}

// TestLoadMergesFragments: drop-in fragments under projects.d are merged with the
// inline projects list, and a malformed fragment is skipped (degraded), not fatal.
func TestLoadMergesFragments(t *testing.T) {
	dir := t.TempDir()
	pd := filepath.Join(dir, "projects.d")
	if err := os.MkdirAll(pd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pd, "taco.yaml"), []byte("name: taco\npath: /work/taco\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a broken fragment must not abort the load; it is skipped with a warning.
	if err := os.WriteFile(filepath.Join(pd, "broken.yaml"), []byte("name: x\npath: [bad\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DELIGHT_PROJECTS_DIR", pd)

	cfg := loadFromYAML(t, `
projects:
  - name: "inline"
    path: "/work/inline"
`)
	names := map[string]bool{}
	for _, p := range cfg.Projects {
		names[p.Name] = true
	}
	if !names["inline"] || !names["taco"] {
		t.Fatalf("expected both inline and taco projects, got %v", names)
	}
	if !cfg.Degraded {
		t.Error("expected Degraded=true after skipping the broken fragment")
	}
}

// TestLoadFragmentDedupesAgainstInline: a fragment that repeats an inline project
// name is dropped by validateProjects (inline wins), and the daemon degrades.
func TestLoadFragmentDedupesAgainstInline(t *testing.T) {
	dir := t.TempDir()
	pd := filepath.Join(dir, "projects.d")
	os.MkdirAll(pd, 0o755)
	os.WriteFile(filepath.Join(pd, "dup.yaml"), []byte("name: shared\npath: /work/from-fragment\n"), 0o644)
	t.Setenv("DELIGHT_PROJECTS_DIR", pd)

	cfg := loadFromYAML(t, `
projects:
  - name: "shared"
    path: "/work/from-inline"
`)
	count := 0
	for _, p := range cfg.Projects {
		if p.Name == "shared" {
			count++
			if p.Path != "/work/from-inline" {
				t.Errorf("inline should win on dedup, got path %q", p.Path)
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected one 'shared' project after dedup, got %d", count)
	}
}

func TestLintFragmentValid(t *testing.T) {
	dir := t.TempDir()
	// point at a real git repo (this checkout) so the git-repo check stays quiet
	wd, _ := os.Getwd()
	repo := filepath.Dir(wd) // .../delightd (config's parent)
	frag := filepath.Join(dir, "ok.yaml")
	os.WriteFile(frag, []byte("name: taco\npath: "+repo+"\n"), 0o644)

	res := LintFragment(frag)
	if !res.Valid {
		t.Fatalf("expected valid, got errors %v", res.Errors)
	}
	if res.Project == nil || res.Project.Name != "taco" {
		t.Errorf("expected parsed project taco, got %+v", res.Project)
	}
}

func TestLintFragmentStructuralError(t *testing.T) {
	dir := t.TempDir()
	frag := filepath.Join(dir, "bad.yaml")
	os.WriteFile(frag, []byte("name: \"\"\npath: \"\"\n"), 0o644)

	res := LintFragment(frag)
	if res.Valid {
		t.Fatal("expected invalid for empty name and path")
	}
	if len(res.Errors) != 2 {
		t.Errorf("expected 2 errors (empty name, empty path), got %v", res.Errors)
	}
}

func TestLintFragmentMalformed(t *testing.T) {
	dir := t.TempDir()
	frag := filepath.Join(dir, "broken.yaml")
	os.WriteFile(frag, []byte("name: taco\npath: [unterminated\n"), 0o644)

	res := LintFragment(frag)
	if res.Valid {
		t.Fatal("expected invalid for malformed YAML")
	}
	if len(res.Errors) == 0 {
		t.Error("expected a parse error")
	}
}

// a non-existent path is a warning, not an error -- it may be a container path.
func TestLintFragmentPathWarningNotError(t *testing.T) {
	dir := t.TempDir()
	frag := filepath.Join(dir, "frag.yaml")
	os.WriteFile(frag, []byte("name: taco\npath: /work/taco\n"), 0o644)

	res := LintFragment(frag)
	if !res.Valid {
		t.Errorf("a missing path should not invalidate the fragment, got errors %v", res.Errors)
	}
	if len(res.Warnings) == 0 {
		t.Error("expected a warning about the missing path")
	}
}
