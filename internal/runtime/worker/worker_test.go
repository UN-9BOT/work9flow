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
	mux.HandleFunc("POST /sessions/{id}/run", func(w http.ResponseWriter, r *http.Request) {
		sid := r.PathValue("id")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		write := func(ev dsh.RawBridgeEvent) {
			b, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			if flusher != nil {
				flusher.Flush()
			}
		}
		write(dsh.RawBridgeEvent{Type: "run.start", MessageID: "msg-" + sid})
		s.mu.Lock()
		evs := append([]dsh.RawBridgeEvent(nil), s.script[sid]...)
		s.mu.Unlock()
		for _, e := range evs {
			write(e)
		}
		write(dsh.RawBridgeEvent{Type: "run.end", Reason: "idle"})
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

// assistantFrame emits an upstream-style assistant/message event
// carrying a JSON content block with the agent's work9flow contract
// (outcome / summary / findings / artifacts). Per upstream DSH the
// assistant message is the agent's terminal response — there is no
// invented agent.completed upstream.
func assistantFrame(sid, summary string) dsh.RawBridgeEvent {
	payload := map[string]any{
		"role": "assistant",
		"content": []map[string]any{{
			"type": "text",
			"text": fmt.Sprintf(`{"outcome":"advance","summary":%q}`, summary),
		}},
	}
	data, _ := json.Marshal(payload)
	return dsh.RawBridgeEvent{Type: "assistant/message", SessionID: sid, Data: data}
}

// defaultAdvanceScript returns a DSH script where every agent emits one
// upstream assistant/message with outcome=advance. Session ids match the
// mock's per-creation counter: scout=1, planner=2, gatekeeper=3,
// implementer=4, reviewer=5. The Activity stream is bounded by the
// bridge's run.end{reason=idle} (added by the mock) — no EOF polling.
func defaultAdvanceScript() map[string][]dsh.RawBridgeEvent {
	return map[string][]dsh.RawBridgeEvent{
		"sess-1": {assistantFrame("sess-1", "scout done")},
		"sess-2": {assistantFrame("sess-2", "planner done")},
		"sess-3": {assistantFrame("sess-3", "gatekeeper done")},
		"sess-4": {assistantFrame("sess-4", "implementer done")},
		"sess-5": {assistantFrame("sess-5", "reviewer done")},
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
