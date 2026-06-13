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

type SystemConfig struct {
	Root       string       `mapstructure:"root"`
	ConfigRoot string       `mapstructure:"config_root"`
	Daemon     DaemonConfig `mapstructure:"daemon"`
}

type BackupRotationConfig struct {
	MaxArchives int `mapstructure:"max_archives"`
}

type BackupConfig struct {
	CheckInterval string               `mapstructure:"check_interval"`
	Rotation      BackupRotationConfig `mapstructure:"rotation"`
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
