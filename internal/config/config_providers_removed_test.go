package config

import (
	"reflect"
	"strings"
	"testing"
)

// TestProvidersFileFieldRemoved guards against the inline OpenAI-compatible
// DSH path (ProvidersFile + WORK9FLOW_PROVIDERS_FILE env var + the
// cmd/work9flowd inline-mode branch) coming back. Per bead work9flow-8w0
// (dsh-A.10e, P1, reviewer P1 #1) and the AGENTS.md "no-prod-mocks" rule,
// work9flowd MUST boot through dsh_bridge_addr only. Tests that need a
// fake DSH use internal/llm/localdsh directly (see localdsh/server_test.go
// and engine/worker test rewrites on bead work9flow-4v1.10.1).
func TestProvidersFileFieldRemoved(t *testing.T) {
	typ := reflect.TypeOf(Config{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("yaml")
		if strings.HasPrefix(tag, "providers_file") {
			t.Errorf("Config has forbidden field %s with yaml tag %q — inline DSH path is removed", f.Name, tag)
		}
	}
}

func TestProvidersFileEnvRemoved(t *testing.T) {
	// applyEnv used to read WORK9FLOW_PROVIDERS_FILE. We can't introspect
	// env reads directly, but we can detect the side-effect: with the env
	// var set, Load must NOT propagate it anywhere reachable.
	t.Setenv("WORK9FLOW_PROVIDERS_FILE", "/tmp/should-be-ignored.toml")
	cfg := Defaults()
	applyEnv(&cfg)
	// Use reflection to scan every string-typed field for the path we set.
	v := reflect.ValueOf(cfg)
	if v.Kind() == reflect.Struct {
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			if f.Kind() != reflect.String {
				continue
			}
			if f.CanSet() && strings.Contains(f.String(), "should-be-ignored") {
				t.Errorf("Config.%s = %q — WORK9FLOW_PROVIDERS_FILE leaked into Config",
					v.Type().Field(i).Name, f.String())
			}
			_ = f
		}
	}
}
