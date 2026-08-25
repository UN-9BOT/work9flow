package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/unbot/work9flow/internal/providers"
)

func TestLoadProvidersFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "providers.toml")
	body := `
[minim]
display_name = "Custom (minim)"
protocol = "openai"
base_url = "https://api.minimax.io/v1"
api_key_env = "MINIM_API_KEY"
default_model = "minim/MiniMax-M3"
[[minim.models]]
id = "MiniMax-M3"
context_window = 400000
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := providers.LoadFile(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	pd, ok := f.Providers["minim"]
	if !ok {
		t.Fatalf("no minim provider; got %v", f.Providers)
	}
	if pd.DefaultModel != "minim/MiniMax-M3" || len(pd.Models) != 1 {
		t.Errorf("unexpected: %+v", pd)
	}
}

func TestLoadProvidersEmptyPath(t *testing.T) {
	f, err := providers.LoadFile("")
	if err != nil {
		t.Fatal(err)
	}
	if f.Providers == nil || len(f.Providers) != 0 {
		t.Errorf("expected empty providers, got %+v", f.Providers)
	}
}
