package providers

import (
	"strings"
	"testing"
)

const sample = `
[minim]
display_name = "Custom (minim)"
protocol = "openai"
base_url = "https://api.minimax.io/v1"
api_key_env = "MINIM_API_KEY"
discover_models = false
default_model = "minim/MiniMax-M3"

[[minim.models]]
id = "MiniMax-M3"
tier = "strong"
context_window = 400000
max_output_tokens = 131072
supports_thinking = true
supports_vision = true
`

func TestParseSample(t *testing.T) {
	f, err := Parse([]byte(sample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, ok := f.Providers["minim"]
	if !ok {
		t.Fatalf("minim not loaded; got keys=%v", keys(f.Providers))
	}
	if p.Protocol != "openai" || p.BaseURL != "https://api.minimax.io/v1" || p.APIKeyEnv != "MINIM_API_KEY" {
		t.Errorf("provider fields wrong: %+v", p)
	}
	if p.DefaultModel != "minim/MiniMax-M3" {
		t.Errorf("default_model wrong: %q", p.DefaultModel)
	}
	if len(p.Models) != 1 {
		t.Fatalf("want 1 model, got %d", len(p.Models))
	}
	m := p.Models[0]
	if m.ID != "MiniMax-M3" || m.ContextWindow != 400000 || !m.SupportsThinking {
		t.Errorf("model fields wrong: %+v", m)
	}
}

func TestParseRef(t *testing.T) {
	cases := []struct {
		in      string
		wantOK  bool
		provider, model string
	}{
		{"minim/MiniMax-M3", true, "minim", "MiniMax-M3"},
		{"", false, "", ""},
		{"noslash", false, "", ""},
		{"/leading-slash", false, "", ""},
		{"trailing-slash/", false, "", ""},
	}
	for _, c := range cases {
		r, err := ParseRef(c.in)
		if (err == nil) != c.wantOK {
			t.Errorf("ParseRef(%q) err=%v wantOK=%v", c.in, err, c.wantOK)
			continue
		}
		if c.wantOK && (r.Provider != c.provider || r.Model != c.model) {
			t.Errorf("ParseRef(%q) = %+v want %s/%s", c.in, r, c.provider, c.model)
		}
	}
	if r := (ProviderRef{Provider: "x", Model: "y"}); r.String() != "x/y" {
		t.Errorf("String: %q", r.String())
	}
}

func TestLookup(t *testing.T) {
	f, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Lookup(ProviderRef{}); err == nil {
		t.Error("empty ref: expected error")
	}
	if _, _, err := f.Lookup(ProviderRef{Provider: "missing", Model: "x"}); err == nil {
		t.Error("missing provider: expected error")
	}
	if _, _, err := f.Lookup(ProviderRef{Provider: "minim", Model: "missing"}); err == nil {
		t.Error("missing model: expected error")
	}
	p, m, err := f.Lookup(ProviderRef{Provider: "minim", Model: "MiniMax-M3"})
	if err != nil {
		t.Fatalf("lookup ok: %v", err)
	}
	if p.APIKeyEnv != "MINIM_API_KEY" || m.Tier != "strong" {
		t.Errorf("lookup returned wrong: %+v %+v", p, m)
	}
}

func TestValidateRejects(t *testing.T) {
	bad := []string{
		"[x]\nbase_url = \"u\"\napi_key_env = \"E\"\nprotocol = \"bogus\"\n[[x.models]]\nid = \"a\"\n",
		"[x]\nprotocol = \"openai\"\napi_key_env = \"E\"\n[[x.models]]\nid = \"a\"\n",
		"[x]\nprotocol = \"openai\"\nbase_url = \"u\"\n[[x.models]]\nid = \"a\"\n",
		"[x]\nprotocol = \"openai\"\nbase_url = \"u\"\napi_key_env = \"E\"\n",
	}
	for i, b := range bad {
		if _, err := Parse([]byte(b)); err == nil {
			t.Errorf("case %d should fail, body=%q", i, strings.TrimSpace(b))
		}
	}
}

func keys(m map[string]Provider) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
