package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/unbot/work9flow/internal/agents"
	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/dsh"
	"github.com/unbot/work9flow/internal/engine"
	"github.com/unbot/work9flow/internal/engine/featuredev"
	"github.com/unbot/work9flow/internal/providers"
	"github.com/unbot/work9flow/internal/storage"
)

// TestInlineDSHDrivesRunToDone is the end-to-end smoke for the inline
// localdsh + scripted OpenAI provider path. It mirrors the worker
// test but uses real engine.Runner + localdsh to prove that the wiring
// in cmd/work9flowd works without an external DSH Node process.
func TestInlineDSHDrivesRunToDone(t *testing.T) {
	// Fake OpenAI-compatible provider: every reply returns "advance".
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{"role": "assistant", "content": "ok\noutcome: advance"},
			}},
		})
	}))
	defer provider.Close()

	// 1) Write a providers.toml matching the inline DSH loader.
	dir := t.TempDir()
	provPath := filepath.Join(dir, "providers.toml")
	body := `
[fake]
display_name = "Fake"
protocol = "openai"
base_url = "` + provider.URL + `"
api_key_env = "FAKE_KEY"
default_model = "fake/test"
[[fake.models]]
id = "test"
`
	if err := os.WriteFile(provPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_KEY", "test-key")

	// 2) Load + boot inline DSH against the fake provider (same
	// wiring cmd/work9flowd uses when DSHEndpoint == "").
	pf, err := providers.LoadFile(provPath)
	if err != nil {
		t.Fatal(err)
	}
	pd, _, err := pf.Lookup(providers.ProviderRef{Provider: "fake", Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := newInlineDSH(pd, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// 3) Wire engine + agents.Runner against srv.URL — same as main.go.
	repo, err := storage.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	c := dsh.NewClient(srv.URL)
	ar := agents.New(c, repo)
	ar.PollInterval = 5 * time.Millisecond
	ar.PollBudget = 500 * time.Millisecond
	eng := engine.New(engine.Option{Repo: repo})
	if err := eng.RegisterWorkflow(featuredev.Workflow(ar)); err != nil {
		t.Fatal(err)
	}

	// 4) Create + drive a run through every featuredev stage.
	ctx := context.Background()
	run, err := eng.CreateRun(ctx, engine.CreateRunInput{
		WorkflowID:   "feature-development",
		RepoPath:     "/tmp",
		OriginalTask: "smoke-full task",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		err := eng.Step(ctx, run.ID)
		final, _ := repo.GetRun(ctx, run.ID)
		if err == engine.ErrTerminalRun || (final.State != "" && domain.IsTerminal(final.State)) {
			break
		}
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	final, err := repo.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != domain.RunDone {
		t.Fatalf("state = %q, want DONE", final.State)
	}

	// 5) Sanity: the inline DSH health endpoint reports ok.
	resp, err := http.Get(srv.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Contains(raw, []byte(`"status":"ok"`)) {
		t.Errorf("/v1/health body = %q", string(raw))
	}
}
