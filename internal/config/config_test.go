package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsAreValid(t *testing.T) {
	cfg := Defaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("defaults should validate, got: %v", err)
	}
	if cfg.StateDir == "" {
		t.Fatal("defaults must include a state dir")
	}
	if cfg.RuntimeEndpoint == "" {
		t.Fatal("defaults must include a runtime endpoint")
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load("/nonexistent/path/to/config.yaml")
	if err == nil {
		t.Fatal("Load must error on missing file")
	}
	if !strings.Contains(err.Error(), "read config") {
		t.Fatalf("error should mention read, got: %v", err)
	}
}

func TestLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "work9flow.yaml")
	yaml := `state_dir: /tmp/wf-state
runtime_endpoint: http://127.0.0.1:9000
dsh_bridge_addr: http://127.0.0.1:8011
workspace_dir: /tmp/wf-ws
iteration_limits:
  default: 3
  review: 7
model_roles:
  planner: deepseek-chat
  reviewer: deepseek-reasoner
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.StateDir != "/tmp/wf-state" {
		t.Errorf("StateDir = %q", cfg.StateDir)
	}
	if cfg.RuntimeEndpoint != "http://127.0.0.1:9000" {
		t.Errorf("RuntimeEndpoint = %q", cfg.RuntimeEndpoint)
	}
	if cfg.IterationLimits["review"] != 7 {
		t.Errorf("iteration_limits[review] = %d", cfg.IterationLimits["review"])
	}
	if cfg.ModelRoles["planner"] != "deepseek-chat" {
		t.Errorf("model_roles[planner] = %q", cfg.ModelRoles["planner"])
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	t.Setenv("WORK9FLOW_STATE_DIR", "/env/state")
	t.Setenv("WORK9FLOW_RUNTIME_ENDPOINT", "http://127.0.0.1:5555")
	t.Setenv("WORK9FLOW_DSH_BRIDGE_ADDR", "http://127.0.0.1:6666")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.StateDir != "/env/state" {
		t.Errorf("StateDir = %q", cfg.StateDir)
	}
	if cfg.RuntimeEndpoint != "http://127.0.0.1:5555" {
		t.Errorf("RuntimeEndpoint = %q", cfg.RuntimeEndpoint)
	}
	if cfg.DSHBridgeAddr != "http://127.0.0.1:6666" {
		t.Errorf("DSHBridgeAddr = %q", cfg.DSHBridgeAddr)
	}
}

func TestValidateRejectsBadLimits(t *testing.T) {
	cfg := Defaults()
	cfg.IterationLimits["bad"] = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate must reject non-positive iteration limit")
	}
}

func TestValidateRejectsEmptyState(t *testing.T) {
	cfg := Defaults()
	cfg.StateDir = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate must reject empty state_dir")
	}
}
