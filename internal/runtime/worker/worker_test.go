package worker_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/log"

	"github.com/unbot/work9flow/internal/agents"
	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/dsh"
	"github.com/unbot/work9flow/internal/engine"
	"github.com/unbot/work9flow/internal/engine/featuredev"
	"github.com/unbot/work9flow/internal/runtime/worker"
	"github.com/unbot/work9flow/internal/storage"
)

// scriptedDSH returns a scripted event stream per session.
type scriptedDSH struct {
	mu     sync.Mutex
	script map[string][]dsh.RawEvent
}

func newScriptedDSH() *scriptedDSH { return &scriptedDSH{script: map[string][]dsh.RawEvent{}} }

func (s *scriptedDSH) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		var req dsh.SessionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, _ = w.Write([]byte(`{"id":"sess-` + req.Role + `"}`))
	})
	mux.HandleFunc("/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) < 2 {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		sid, op := parts[0], parts[1]
		if op != "events" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.mu.Lock()
		evs := append([]dsh.RawEvent(nil), s.script[sid]...)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, e := range evs {
			b, _ := json.Marshal(e)
			_, _ = w.Write(b)
			_, _ = w.Write([]byte("\n"))
		}
	})
	return mux
}

func defaultAdvanceScript() map[string][]dsh.RawEvent {
	now := time.Now().UTC()
	mk := func(role string) []dsh.RawEvent {
		return []dsh.RawEvent{
			{SessionID: "sess-" + role, Kind: "agent.completed", At: now, Data: mustJSON(map[string]string{"outcome": "advance"})},
		}
	}
	return map[string][]dsh.RawEvent{
		"sess-scout":       mk("scout"),
		"sess-planner":     mk("planner"),
		"sess-gatekeeper":  mk("gatekeeper"),
		"sess-implementer": mk("implementer"),
		"sess-reviewer":    mk("reviewer"),
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) { w.t.Log(string(p)); return len(p), nil }

func newTestWorker(t *testing.T, script map[string][]dsh.RawEvent) (*worker.Worker, *engine.Engine, storage.Repo, func()) {
	t.Helper()
	repo, err := storage.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	mock := newScriptedDSH()
	mock.script = script
	srv := httptest.NewServer(mock.handler())
	c := dsh.NewClient(srv.URL)
	ar := agents.New(c, repo)
	ar.PollInterval = 5 * time.Millisecond
	ar.PollBudget = 500 * time.Millisecond
	eng := engine.New(engine.Option{Repo: repo})
	if err := eng.RegisterWorkflow(featuredev.Workflow(ar)); err != nil {
		t.Fatal(err)
	}
	logger := log.NewWithOptions(testWriter{t}, log.Options{Level: log.WarnLevel})
	w := worker.New(worker.Options{Engine: eng, Repo: repo, Logger: logger})
	return w, eng, repo, func() { srv.Close(); _ = repo.Close() }
}

func TestWorkerSkipsTerminalRuns(t *testing.T) {
	repo, _ := storage.OpenSQLite(":memory:")
	defer repo.Close()
	now := time.Now().UTC()
	if err := repo.CreateRun(context.Background(), domain.WorkflowRun{
		ID: "r-done", WorkflowID: "feature-development", OriginalTask: "x",
		State: domain.RunCanceled, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	logger := log.NewWithOptions(testWriter{t}, log.Options{Level: log.WarnLevel})
	w := worker.New(worker.Options{Engine: nil, Repo: repo, Logger: logger})
	// Nil engine + repo -> no work, no crash.
	w.TickOnceForTest(context.Background(), repo)
}

func TestWorkerDrivesRunToDone(t *testing.T) {
	w, _, repo, cleanup := newTestWorker(t, defaultAdvanceScript())
	defer cleanup()
	now := time.Now().UTC()
	if err := repo.CreateRun(context.Background(), domain.WorkflowRun{
		ID: "r-active", WorkflowID: "feature-development", OriginalTask: "x",
		State: domain.RunNew, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// Pump six ticks manually.
	for i := 0; i < 6; i++ {
		w.TickOnceForTest(ctx, repo)
	}
	got, _ := repo.GetRun(ctx, "r-active")
	if got.State != domain.RunDone {
		t.Errorf("state = %q, want DONE", got.State)
	}
}

func TestWorkerRunLoopRespectsContextCancel(t *testing.T) {
	w, _, _, cleanup := newTestWorker(t, defaultAdvanceScript())
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)
	cancel()
	// Give Run a moment to observe ctx.Done() and return.
	time.Sleep(50 * time.Millisecond)
}
