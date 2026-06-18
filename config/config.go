package config

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/spf13/viper"
)

type DaemonConfig struct {
	ControlPort int    `mapstructure:"control_port"`
	PidFile     string `mapstructure:"pid_file"`
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
	Root         string             `mapstructure:"root"`
	ConfigRoot   string             `mapstructure:"config_root"`
	AgentSkills  AgentSkillsConfig  `mapstructure:"agent_skills"`
	Daemon       DaemonConfig       `mapstructure:"daemon"`
	LLMDiscovery LLMDiscoveryConfig `mapstructure:"llm_discovery"`
	Kafka        KafkaConfig        `mapstructure:"kafka"`
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
}

// Load initializes Viper, reads the configuration agnosticly, and unmarshals it.
// It accepts a context to comply with our explicit trace passing standard.
func Load(ctx context.Context) (*DelightConfig, error) {
	viper.SetConfigName("delight")
	viper.SetConfigType("yaml")

	// Agnostic resolution paths
	viper.AddConfigPath("$HOME/etc/delightd")
	viper.AddConfigPath(".")

	// Enable 12-factor environment variable overrides (e.g. DELIGHT_SYSTEM_ROOT)
	viper.SetEnvPrefix("delight")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	viper.AutomaticEnv()

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}
		slog.Warn("no config file found, falling back to environment variables and defaults")
	}

	var cfg DelightConfig
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal configuration: %w", err)
	}

	return &cfg, nil
}
