package agents_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/unbot/work9flow/internal/agents"
	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/dsh"
	"github.com/unbot/work9flow/internal/storage"
)

// scriptedBridge is an httptest server that mimics runtime/dsh-bridge.
// Tests pre-load a list of SSE frames per session; on /events it
// writes each frame as `data: <json>\n\n` and closes the stream so
// SnapshotEvents returns the whole batch.
type scriptedBridge struct {
	mu     sync.Mutex
	script map[string][]dsh.RawBridgeEvent
}

func newScriptedBridge() *scriptedBridge { return &scriptedBridge{script: map[string][]dsh.RawBridgeEvent{}} }

// eventFrame builds a session.event envelope around an inner upstream
// notification, so tests can describe what the upstream agent emits.
func eventFrame(sessionID, innerKind string, data json.RawMessage) dsh.RawBridgeEvent {
	inner, _ := json.Marshal(map[string]any{"kind": innerKind, "data": data})
	return dsh.RawBridgeEvent{Kind: "session.event", SessionID: sessionID, Event: inner}
}

func statusFrame(sessionID, status string) dsh.RawBridgeEvent {
	return dsh.RawBridgeEvent{Kind: "session.status", SessionID: sessionID, Status: status}
}

func (s *scriptedBridge) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":     "ready",
			"serverInfo": map[string]string{"name": "dsh-bridge-test", "version": "test"},
		})
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		var req dsh.CreateSessionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessionId":  "sess-" + req.Model,
			"serverInfo": map[string]string{"name": "dsh-bridge-test", "version": "test"},
		})
	})
	mux.HandleFunc("POST /sessions/{id}/prompt", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"messageId": "msg-1"})
	})
	mux.HandleFunc("GET /sessions/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		s.mu.Lock()
		frames := append([]dsh.RawBridgeEvent(nil), s.script[id]...)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, f := range frames {
			b, _ := json.Marshal(f)
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
	return mux
}

func newRig(t *testing.T, script map[string][]dsh.RawBridgeEvent) (*agents.Runner, storage.Repo, func()) {
	t.Helper()
	mock := newScriptedBridge()
	mock.script = script
	srv := httptest.NewServer(mock.handler())
	c := dsh.NewBridge(srv.URL)
	repo, err := storage.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	r := agents.New(c, repo)
	r.Provider = "deepseek"
	r.PollInterval = 5 * time.Millisecond
	r.PollBudget = 500 * time.Millisecond
	return r, repo, func() { srv.Close(); _ = repo.Close() }
}

func newRun(t *testing.T, repo storage.Repo, id string) domain.WorkflowRun {
	t.Helper()
	now := time.Now().UTC()
	run := domain.WorkflowRun{
		ID:           id,
		WorkflowID:   "feature-development",
		RepoPath:     "/tmp/repo",
		OriginalTask: "build it",
		State:        domain.RunDiscovery,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := repo.CreateRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestRunPersistsEventsAndReturnsAdvance(t *testing.T) {
	r, repo, cleanup := newRig(t, map[string][]dsh.RawBridgeEvent{
		"sess-deepseek-v3": {
			eventFrame("sess-deepseek-v3", "agent.started", json.RawMessage(`{"role":"scout"}`)),
			eventFrame("sess-deepseek-v3", "tool.completed", json.RawMessage(`{"tool":"read"}`)),
			eventFrame("sess-deepseek-v3", "agent.completed", json.RawMessage(`{"outcome":"advance","summary":"scout done"}`)),
		},
	})
	defer cleanup()
	run := newRun(t, repo, "run-x")

	out, err := r.Run(context.Background(), run, "scout", "deepseek-v3",
		agents.Instructions{Message: "go", Payload: json.RawMessage(`{"task":"x"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != "advance" {
		t.Errorf("kind = %q", out.Kind)
	}
	if out.Summary != "scout done" {
		t.Errorf("summary = %q", out.Summary)
	}
	evs, _ := repo.EventsAfter(context.Background(), run.ID, 0)
	if len(evs) < 3 {
		t.Errorf("events = %d", len(evs))
	}
	found := false
	for _, e := range evs {
		if e.Kind == domain.EventKindAgentStarted {
			found = true
		}
	}
	if !found {
		t.Errorf("missing agent.started event")
	}
}

func TestRunReducesApprove(t *testing.T) {
	r, repo, cleanup := newRig(t, map[string][]dsh.RawBridgeEvent{
		"sess-m": {
			eventFrame("sess-m", "agent.completed", json.RawMessage(`{"outcome":"approve","summary":"ok"}`)),
		},
	})
	defer cleanup()
	run := newRun(t, repo, "run-approve")
	out, err := r.Run(context.Background(), run, "gatekeeper", "m", agents.Instructions{Message: "review"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != "approve" {
		t.Errorf("kind = %q", out.Kind)
	}
}

func TestRunReducesReviseAndWaitUser(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"revise", `{"outcome":"revise","findings":{"gap":"x"}}`, "revise"},
		{"wait_user", `{"outcome":"wait_user","questions":["which DB?"]}`, "wait_user"},
		{"missing_outcome", `{"summary":"hi"}`, "advance"},
		{"unknown_kind", `{"outcome":"foo"}`, "advance"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, repo, cleanup := newRig(t, map[string][]dsh.RawBridgeEvent{
				"sess-m": {
					eventFrame("sess-m", "agent.completed", json.RawMessage(tc.data)),
				},
			})
			defer cleanup()
			run := newRun(t, repo, "run-"+tc.name)
			out, err := r.Run(context.Background(), run, "x", "m", agents.Instructions{Message: "go"})
			if err != nil {
				t.Fatal(err)
			}
			if out.Kind != tc.want {
				t.Errorf("kind = %q, want %q", out.Kind, tc.want)
			}
		})
	}
}

func TestRunErrorsWhenSessionIncomplete(t *testing.T) {
	r, repo, cleanup := newRig(t, map[string][]dsh.RawBridgeEvent{
		"sess-m": {
			eventFrame("sess-m", "agent.started", json.RawMessage(`{}`)),
		},
	})
	defer cleanup()
	run := newRun(t, repo, "run-timeout")
	if _, err := r.Run(context.Background(), run, "x", "m", agents.Instructions{}); err == nil {
		t.Fatal("expected ErrSessionIncomplete")
	}
}

func TestRunRejectsNilBridge(t *testing.T) {
	repo, _ := storage.OpenSQLite(":memory:")
	defer repo.Close()
	r := agents.New(nil, repo)
	r.Provider = "deepseek"
	run := newRun(t, repo, "run-nil")
	if _, err := r.Run(context.Background(), run, "x", "m", agents.Instructions{}); err == nil {
		t.Fatal("expected nil bridge error")
	}
}

func TestRunRequiresProvider(t *testing.T) {
	srv := httptest.NewServer(newScriptedBridge().handler())
	defer srv.Close()
	repo, _ := storage.OpenSQLite(":memory:")
	defer repo.Close()
	r := agents.New(dsh.NewBridge(srv.URL), repo)
	// Provider deliberately unset.
	run := newRun(t, repo, "run-prov")
	if _, err := r.Run(context.Background(), run, "x", "m", agents.Instructions{}); err == nil {
		t.Fatal("expected missing-provider error")
	}
}
