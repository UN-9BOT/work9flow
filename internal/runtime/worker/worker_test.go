package worker_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// scriptedDSH is a minimal mock for the runtime/dsh-bridge HTTP surface.
// Session ids are assigned per creation order so the script map can be
// keyed by agent role independent of upstream DSH provider/model wiring.
type scriptedDSH struct {
	mu           sync.Mutex
	script       map[string][]dsh.RawBridgeEvent
	createdOrder []string
}

func newScriptedDSH() *scriptedDSH {
	return &scriptedDSH{script: map[string][]dsh.RawBridgeEvent{}}
}

func (s *scriptedDSH) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		idx := len(s.createdOrder) + 1
		id := fmt.Sprintf("sess-%d", idx)
		s.createdOrder = append(s.createdOrder, id)
		s.mu.Unlock()
		_, _ = fmt.Fprintf(w, `{"sessionId":%q,"serverInfo":{"name":"scripted-dsh","version":"test"}}`, id)
	})
	mux.HandleFunc("POST /sessions/{id}/prompt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"messageId":"msg-stub"}`))
	})
	mux.HandleFunc("GET /sessions/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		sid := r.PathValue("id")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		s.mu.Lock()
		evs := append([]dsh.RawBridgeEvent(nil), s.script[sid]...)
		s.mu.Unlock()
		for _, e := range evs {
			b, _ := json.Marshal(e)
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(b)
			_, _ = w.Write([]byte("\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
	})
	mux.HandleFunc("POST /sessions/{id}/close", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
	})
	mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func eventFrame(sid, innerKind string, data json.RawMessage) dsh.RawBridgeEvent {
	inner, _ := json.Marshal(map[string]any{"kind": innerKind, "data": data})
	return dsh.RawBridgeEvent{Kind: "session.event", SessionID: sid, Event: inner}
}

// defaultAdvanceScript returns a DSH script where every agent emits a
// single agent.completed event with outcome=advance. Session ids match
// the mock's per-creation counter: scout=1, planner=2, gatekeeper=3,
// implementer=4, reviewer=5.
func defaultAdvanceScript() map[string][]dsh.RawBridgeEvent {
	now := time.Now().UTC()
	mk := func(sid string) []dsh.RawBridgeEvent {
		return []dsh.RawBridgeEvent{
			eventFrame(sid, "agent.completed", json.RawMessage(`{"outcome":"advance"}`)),
			eventFrame(sid, "session.status", json.RawMessage(fmt.Sprintf(`{"status":"idle","at":%q}`, now.Format(time.RFC3339Nano)))),
		}
	}
	return map[string][]dsh.RawBridgeEvent{
		"sess-1": mk("sess-1"),
		"sess-2": mk("sess-2"),
		"sess-3": mk("sess-3"),
		"sess-4": mk("sess-4"),
		"sess-5": mk("sess-5"),
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) { w.t.Log(string(p)); return len(p), nil }

func newTestWorker(t *testing.T, script map[string][]dsh.RawBridgeEvent) (*worker.Worker, *engine.Engine, storage.Repo, func()) {
	t.Helper()
	repo, err := storage.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	mock := newScriptedDSH()
	mock.script = script
	srv := httptest.NewServer(mock.handler())
	c := dsh.NewBridge(srv.URL)
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
	// Pump enough ticks for scout+planner+gatekeeper+implementer+reviewer,
	// each Step iterates one stage. Six ticks covers the full advance.
	for i := 0; i < 8; i++ {
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
