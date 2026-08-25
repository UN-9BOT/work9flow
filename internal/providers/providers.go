// Package providers loads OpenAI-compatible provider definitions from a
// TOML file. The schema mirrors what work9flow expects from any LLM
// runtime: one provider table keyed by short name (e.g. "minim"),
// listing its transport (protocol / base_url / api_key_env) and the
// concrete model catalog. See providers.toml in the repo root for an
// example.
package providers

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"
)

// File is the on-disk shape of providers.toml. Each top-level table
// name is the provider's short id; its body is the Provider struct.
// Top-level tables are captured dynamically by parsing twice: once into
// a map[string]any (to discover table names) and once per table into
// a Provider (which preserves [[provider.models]] array semantics).
type File struct {
	// Providers maps provider id (e.g. "minim") -> definition.
	Providers map[string]Provider
}

// Provider is one OpenAI-compatible LLM provider.
//
//   [minim]
//   display_name = "Custom (minim)"
//   protocol     = "openai"
//   base_url     = "https://api.minimax.io/v1"
//   api_key_env  = "MINIM_API_KEY"
//   default_model = "minim/MiniMax-M3"
//
//   [[minim.models]]
//   id = "MiniMax-M3"
type Provider struct {
	DisplayName   string  `toml:"display_name"`
	Protocol      string  `toml:"protocol"`
	BaseURL       string  `toml:"base_url"`
	APIKeyEnv     string  `toml:"api_key_env"`
	DiscoverModels bool    `toml:"discover_models"`
	DefaultModel  string  `toml:"default_model"`
	Models        []Model `toml:"models"`
}

// Model is one entry in a provider's catalog.
type Model struct {
	ID               string `toml:"id"`
	Tier             string `toml:"tier"`
	ContextWindow    int    `toml:"context_window"`
	MaxOutputTokens  int    `toml:"max_output_tokens"`
	SupportsThinking bool   `toml:"supports_thinking"`
	SupportsVision   bool   `toml:"supports_vision"`
}

// ProviderRef is "<provider>/<model>" — the format work9flow uses when
// forwarding a model choice to DSH (DSH resolves it to the right
// transport internally) and the format it expects on ModelRoles.
type ProviderRef struct {
	Provider string
	Model    string
}

// ParseRef splits "minim/MiniMax-M3" into ("minim", "MiniMax-M3").
// Returns an error if the ref is empty or missing a slash.
func ParseRef(s string) (ProviderRef, error) {
	if s == "" {
		return ProviderRef{}, errors.New("providers: empty model ref")
	}
	i := strings.IndexByte(s, '/')
	if i <= 0 || i == len(s)-1 {
		return ProviderRef{}, fmt.Errorf("providers: model ref %q must be <provider>/<model>", s)
	}
	return ProviderRef{Provider: s[:i], Model: s[i+1:]}, nil
}

// String renders the canonical "<provider>/<model>" form.
func (r ProviderRef) String() string { return r.Provider + "/" + r.Model }

// Lookup returns the Provider matching ref.Provider and the Model
// matching ref.Model. Empty inputs return an error.
func (f File) Lookup(ref ProviderRef) (Provider, Model, error) {
	if ref.Provider == "" || ref.Model == "" {
		return Provider{}, Model{}, errors.New("providers: ref has empty provider or model")
	}
	p, ok := f.Providers[ref.Provider]
	if !ok {
		return Provider{}, Model{}, fmt.Errorf("providers: unknown provider %q", ref.Provider)
	}
	for _, m := range p.Models {
		if m.ID == ref.Model {
			return p, m, nil
		}
	}
	return Provider{}, Model{}, fmt.Errorf("providers: provider %q has no model %q", ref.Provider, ref.Model)
}

// LoadFile reads a TOML providers file from disk and returns the
// decoded File. An empty path returns an empty File (no error).
func LoadFile(path string) (File, error) {
	f := File{Providers: map[string]Provider{}}
	if path == "" {
		return f, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return f, fmt.Errorf("providers: read %s: %w", path, err)
	}
	return Parse(raw)
}

// Parse decodes a TOML byte slice into a File. Used by tests and by
// LoadFile; exported so embedders can supply bytes from anywhere.
//
// BurntSushi/toml v1 does not auto-decode dynamic top-level tables
// (e.g. [minim]) into a map[string]Provider, so we first decode the
// raw document into map[string]toml.Primitive to discover the table
// names, then decode each table into a Provider struct.
func Parse(raw []byte) (File, error) {
	rough := map[string]any{}
	if _, err := toml.Decode(string(raw), &rough); err != nil {
		return File{}, fmt.Errorf("providers: parse toml: %w", err)
	}
	out := File{Providers: map[string]Provider{}}
	for name, val := range rough {
		tbl, ok := val.(map[string]any)
		if !ok {
			// Scalar top-level key — skip (user can put defaults here).
			continue
		}
		// Re-encode the sub-table as TOML and decode into Provider so
		// nested [[provider.models]] arrays land in p.Models correctly.
		var p Provider
		sub, err := toml.Marshal(tbl)
		if err != nil {
			return out, fmt.Errorf("providers: re-encode %s: %w", name, err)
		}
		if _, err := toml.Decode(string(sub), &p); err != nil {
			return out, fmt.Errorf("providers: decode %s: %w", name, err)
		}
		out.Providers[name] = p
	}
	for name, p := range out.Providers {
		if err := p.validate(name); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (p Provider) validate(name string) error {
	if p.Protocol == "" {
		return fmt.Errorf("providers: %s: protocol is required", name)
	}
	switch p.Protocol {
	case "openai":
		// supported
	default:
		return fmt.Errorf("providers: %s: unsupported protocol %q (only \"openai\" today)", name, p.Protocol)
	}
	if p.BaseURL == "" {
		return fmt.Errorf("providers: %s: base_url is required", name)
	}
	if p.APIKeyEnv == "" {
		return fmt.Errorf("providers: %s: api_key_env is required", name)
	}
	if len(p.Models) == 0 {
		return fmt.Errorf("providers: %s: at least one [[%s.models]] entry required", name, name)
	}
	return nil
}
