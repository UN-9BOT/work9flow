// Package config defines work9flow configuration and its loader.
//
// Config is shared by work9flowd and work9flow (TUI). Neither binary
// embeds the other's runtime state — both read the same Config from
// disk/env so endpoints and state directories stay consistent.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config is the on-disk shape of work9flow configuration.
type Config struct {
	StateDir        string            `yaml:"state_dir"`
	RuntimeEndpoint string            `yaml:"runtime_endpoint"`
	DSHBridgeAddr     string            `yaml:"dsh_bridge_addr"`
	WorkspaceDir    string            `yaml:"workspace_dir"`
	IterationLimits map[string]int    `yaml:"iteration_limits"`
	ModelRoles      map[string]string `yaml:"model_roles"`
	// ProvidersFile points at a TOML file describing LLM providers
	// (e.g. providers.toml). When DSHBridgeAddr is empty and this is
	// set, work9flowd boots an inline OpenAI-compatible DSH that
	// routes requests through the named provider/model from ModelRoles.
	ProvidersFile   string            `yaml:"providers_file"`
}

// Defaults returns a Config populated with safe local defaults.
func Defaults() Config {
	return Config{
		StateDir:        defaultStateDir(),
		RuntimeEndpoint: "http://127.0.0.1:7469",
		IterationLimits: map[string]int{"default": 5},
		ModelRoles:      map[string]string{},
	}
}

func defaultStateDir() string {
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "work9flow")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".work9flow"
	}
	return filepath.Join(home, ".work9flow")
}

// Load returns Defaults overlaid with values from path, then env
// (WORK9FLOW_*), then validates. Empty path = env-only overlay.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	applyEnv(&cfg)
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("WORK9FLOW_STATE_DIR"); v != "" {
		cfg.StateDir = v
	}
	if v := os.Getenv("WORK9FLOW_RUNTIME_ENDPOINT"); v != "" {
		cfg.RuntimeEndpoint = v
	}
	if v := os.Getenv("WORK9FLOW_DSH_BRIDGE_ADDR"); v != "" {
		cfg.DSHBridgeAddr = v
	}
	if v := os.Getenv("WORK9FLOW_WORKSPACE_DIR"); v != "" {
		cfg.WorkspaceDir = v
	}
	if v := os.Getenv("WORK9FLOW_PROVIDERS_FILE"); v != "" {
		cfg.ProvidersFile = v
	}
}

// Validate returns an error if cfg is internally inconsistent.
func (c Config) Validate() error {
	if c.StateDir == "" {
		return errors.New("config: state_dir is required")
	}
	if c.RuntimeEndpoint == "" {
		return errors.New("config: runtime_endpoint is required")
	}
	for stage, n := range c.IterationLimits {
		if n <= 0 {
			return fmt.Errorf("config: iteration_limits[%q] must be positive, got %d", stage, n)
		}
	}
	return nil
}
