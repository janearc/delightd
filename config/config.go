package config

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

type DaemonConfig struct {
	ControlPort int    `mapstructure:"control_port"`
	PidFile     string `mapstructure:"pid_file"`
}

// DefaultControlPort is delightd's canonical control port. compose publishes
// 127.0.0.1:8088, the kube Deployment uses containerPort 8088, and every client
// (the generated CLI wrapper, curl callers) routes there. This is the single
// source of the default so a missing or zero config never drops the listener onto
// a port nothing reaches.
const DefaultControlPort = 8088

// ResolveControlPort returns the configured control port, falling back to
// DefaultControlPort when it is unset (the zero value from an absent config key).
func (d DaemonConfig) ResolveControlPort() int {
	if d.ControlPort == 0 {
		return DefaultControlPort
	}
	return d.ControlPort
}

type LLMProviderConfig struct {
	Name string `mapstructure:"name"`
	Type string `mapstructure:"type"`
	URL  string `mapstructure:"url"`
}

type LLMDiscoveryConfig struct {
	Providers []LLMProviderConfig `mapstructure:"providers"`
}

type SystemConfig struct {
	// The four roots delightd operates over. They are deliberately distinct: the
	// tree the daemon monitors is not where it keeps its own runtime state, and
	// where it writes backups is a single directory under that state tree, not the
	// state tree itself. Each is independently configurable (env + yaml) like a
	// ./configure --prefix; see DefaultMonitorRoot/DefaultDaemonRoot/
	// DefaultConfigRoot and ResolveRoots for the defaults and the BackupsRoot
	// derivation.

	// MonitorRoot is the tree delightd watches: the parent of the managed
	// projects' git working trees (read-only in container deployments). yaml
	// key system.monitor_root, env DELIGHT_MONITOR_ROOT, default ~/work.
	MonitorRoot string `mapstructure:"monitor_root"`
	// DaemonRoot is delightd's own runtime/state tree (pid file, exports, the
	// backups directory). yaml key system.daemon_root, env DELIGHT_DAEMON_ROOT,
	// default ~/var.
	DaemonRoot string `mapstructure:"daemon_root"`
	// BackupsRoot is the directory backup archives are written INTO (one archive
	// subtree per project). It is the literal destination, not a parent the
	// daemon appends "/backups" to. yaml key system.backups_root, env
	// DELIGHT_BACKUPS_ROOT; when unset it derives from DaemonRoot as
	// ${DaemonRoot}/backups, but an explicit value overrides that.
	BackupsRoot string `mapstructure:"backups_root"`
	// ConfigRoot is where delightd resolves its configuration and registry. yaml
	// key system.config_root, env DELIGHT_CONFIG_ROOT, default ~/etc.
	ConfigRoot string `mapstructure:"config_root"`

	AgentSkills  AgentSkillsConfig  `mapstructure:"agent_skills"`
	Daemon       DaemonConfig       `mapstructure:"daemon"`
	LLMDiscovery LLMDiscoveryConfig `mapstructure:"llm_discovery"`
	Kafka        KafkaConfig        `mapstructure:"kafka"`
}

// Default roots, expressed with a leading ~ which ResolveRoots expands to the
// current user's home. They are package-level so tests and deployment docs can
// reference the single source.
const (
	DefaultMonitorRoot = "~/work"
	DefaultDaemonRoot  = "~/var"
	DefaultConfigRoot  = "~/etc"
)

// ResolveRoots fills any unset root with its default, derives BackupsRoot from
// DaemonRoot (${DaemonRoot}/backups) when it is not set explicitly, and expands
// a leading ~ in each to the current user's home. It is idempotent: calling it
// on an already-resolved config is a no-op for the derivation and only re-runs
// the (stable) home expansion. Order matters -- BackupsRoot derives from the
// resolved DaemonRoot, so DaemonRoot is settled first.
func (s *SystemConfig) ResolveRoots() {
	if s.MonitorRoot == "" {
		s.MonitorRoot = DefaultMonitorRoot
	}
	if s.DaemonRoot == "" {
		s.DaemonRoot = DefaultDaemonRoot
	}
	if s.ConfigRoot == "" {
		s.ConfigRoot = DefaultConfigRoot
	}
	// BackupsRoot defaults to a "backups" directory under DaemonRoot, but an
	// explicit value (yaml or DELIGHT_BACKUPS_ROOT) wins. DaemonRoot is expanded
	// before joining so the derived path carries no leftover ~.
	s.DaemonRoot = expandHome(s.DaemonRoot)
	if s.BackupsRoot == "" {
		s.BackupsRoot = filepath.Join(s.DaemonRoot, "backups")
	}
	s.MonitorRoot = expandHome(s.MonitorRoot)
	s.BackupsRoot = expandHome(s.BackupsRoot)
	s.ConfigRoot = expandHome(s.ConfigRoot)
}

// expandHome resolves a leading ~ to the current user's home directory. Roots in
// delight.yaml are written with ~ by convention.
func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return path
}

// KafkaConfig configures the event-emission path. When Brokers is empty, event
// publishing is disabled and the daemon runs exactly as before -- backups never
// depend on Kafka being present.
type KafkaConfig struct {
	Brokers           []string `mapstructure:"brokers"`
	SchemaRegistryURL string   `mapstructure:"schema_registry_url"`
	Topic             string   `mapstructure:"topic"`
}

type AgentSkillsConfig struct {
	Enabled   bool     `mapstructure:"enabled"`
	ExposeVia []string `mapstructure:"expose_via"`
}

type BackupRotationConfig struct {
	MaxArchives int `mapstructure:"max_archives"`
}

type BackupConfig struct {
	CheckInterval string               `mapstructure:"check_interval"`
	Rotation      BackupRotationConfig `mapstructure:"rotation"`
	// Exclude lists project-relative paths kept out of the checkpoint, on top of
	// the built-in skips. This is how large, regenerable trees (e.g. model
	// weights) are excluded from a project's backups.
	Exclude []string `mapstructure:"exclude"`
}

type ProjectConfig struct {
	Name   string       `mapstructure:"name"`
	Path   string       `mapstructure:"path"`
	Backup BackupConfig `mapstructure:"backup"`
}

type DelightConfig struct {
	System   SystemConfig    `mapstructure:"system"`
	Projects []ProjectConfig `mapstructure:"projects"`

	// Degraded is set when Load could not fully read its configuration but chose to
	// come up anyway rather than fail closed -- the availability mandate is that
	// delightd starts in any condition. LoadWarnings carries the human-readable
	// reasons (surfaced on /health). Both are derived at load time, never read from
	// config, so the unmarshaler ignores them.
	Degraded     bool     `mapstructure:"-" json:"degraded"`
	LoadWarnings []string `mapstructure:"-" json:"load_warnings,omitempty"`
}

// markDegraded records that the daemon is coming up with incomplete config and
// why. When an underlying error is present it is handed to the structured logger
// as the error object (not flattened with %v) so its full detail survives into the
// log/telemetry; LoadWarnings keeps a string form for the /health response. err may
// be nil for a synthetic reason (e.g. a rejected project entry).
func (c *DelightConfig) markDegraded(reason string, err error) {
	c.Degraded = true
	if err != nil {
		c.LoadWarnings = append(c.LoadWarnings, fmt.Sprintf("%s: %v", reason, err))
		slog.Error("delightd config degraded", "reason", reason, "error", err)
		return
	}
	c.LoadWarnings = append(c.LoadWarnings, reason)
	slog.Error("delightd config degraded", "reason", reason)
}

// validateProjects drops entries that are obviously unusable so a single bad entry
// cannot break the daemon or confuse downstream consumers. A project with no name
// or no path, or a duplicate name, is rejected here with a warning. A path that
// merely does not exist is NOT dropped: gitstate reports that as missing_path at
// sweep time, which is the right place to surface a transient/unmounted tree.
func (c *DelightConfig) validateProjects() []ProjectConfig {
	seen := make(map[string]bool, len(c.Projects))
	valid := make([]ProjectConfig, 0, len(c.Projects))
	for i, p := range c.Projects {
		switch {
		case strings.TrimSpace(p.Name) == "":
			c.markDegraded(fmt.Sprintf("project[%d] dropped: empty name (path %q)", i, p.Path), nil)
		case strings.TrimSpace(p.Path) == "":
			c.markDegraded(fmt.Sprintf("project %q dropped: empty path", p.Name), nil)
		case seen[p.Name]:
			c.markDegraded(fmt.Sprintf("project %q dropped: duplicate name", p.Name), nil)
		default:
			seen[p.Name] = true
			valid = append(valid, p)
		}
	}
	return valid
}

// Load initializes Viper, reads the configuration agnosticly, and unmarshals it.
// It accepts a context to comply with our explicit trace passing standard.
func Load(ctx context.Context) (*DelightConfig, error) {
	viper.SetConfigName("delight")
	viper.SetConfigType("yaml")

	// Agnostic resolution paths
	viper.AddConfigPath("$HOME/etc/delightd")
	viper.AddConfigPath(".")

	// Enable 12-factor environment variable overrides. The prefix + "." -> "_"
	// replacer maps system.monitor_root -> DELIGHT_MONITOR_ROOT, etc.
	viper.SetEnvPrefix("delight")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	// Bind the four roots to their env vars explicitly. Two reasons: AutomaticEnv
	// only consults the environment for keys viper already knows about (present in
	// a config file or bound) -- with no config file the roots would never be read
	// from the environment; and the env names are deliberately short
	// (DELIGHT_MONITOR_ROOT, not the AutomaticEnv-derived
	// DELIGHT_SYSTEM_MONITOR_ROOT), so the binding maps the nested config key to
	// the chosen env name directly.
	rootEnvBindings := map[string]string{
		"system.monitor_root": "DELIGHT_MONITOR_ROOT",
		"system.daemon_root":  "DELIGHT_DAEMON_ROOT",
		"system.backups_root": "DELIGHT_BACKUPS_ROOT",
		"system.config_root":  "DELIGHT_CONFIG_ROOT",
	}
	for key, env := range rootEnvBindings {
		if err := viper.BindEnv(key, env); err != nil {
			return nil, fmt.Errorf("failed to bind env %s for %s: %w", env, key, err)
		}
	}

	var cfg DelightConfig

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// Not degraded -- running on env + defaults is a supported mode -- but
			// surface the error: it can explain which paths were searched or why the
			// lookup failed.
			slog.Warn("no config file found, falling back to environment variables and defaults", "error", err)
		} else {
			// A malformed config file must not take the control plane down. Come up
			// degraded -- env + defaults, whatever projects we can still read -- and
			// make the failure loud and queryable (cfg.Degraded, /health) instead of
			// returning an error that aborts startup.
			cfg.markDegraded("config parse failed", err)
		}
	}

	if err := viper.Unmarshal(&cfg); err != nil {
		// Same posture for a shape mismatch: degrade, do not abort.
		cfg.markDegraded("config unmarshal failed", err)
	}

	// Reject obviously-unusable project entries before any consumer sees them. The
	// returned type is unchanged, so gitstate/httpapi/backup are untouched.
	cfg.Projects = cfg.validateProjects()

	// Apply defaults, derive BackupsRoot from DaemonRoot when unset, and expand
	// ~ so consumers receive absolute paths.
	cfg.System.ResolveRoots()

	return &cfg, nil
}
