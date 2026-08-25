package dsh_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/dsh"
)

// fakeBridge is an httptest server that mimics the runtime/dsh-bridge
// HTTP API. It records each request and serves scripted responses so
// tests can assert both sides of the wire without standing up Node.
type fakeBridge struct {
	mu          sync.Mutex
	sessions    map[string]bool
	prompts     map[string]int
	script      []dsh.RawBridgeEvent // events to emit per session
	closed      bool
	shutdownHit bool
	srv         *httptest.Server
}

func newFakeBridge(t *testing.T) *fakeBridge {
	t.Helper()
	fb := &fakeBridge{
		sessions: map[string]bool{},
		prompts:  map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready", "serverInfo": map[string]string{
			"name":    "dsh-bridge-test",
			"version": "test",
		}})
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		var req dsh.CreateSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode CreateSessionRequest: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fb.mu.Lock()
		id := fmt.Sprintf("sess-%s", req.Model)
		fb.sessions[id] = true
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessionId": id,
			"serverInfo": map[string]string{"name": "dsh-bridge-test", "version": "test"},
		})
	})
	mux.HandleFunc("POST /sessions/{id}/prompt", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		fb.mu.Lock()
		fb.prompts[id]++
		fb.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"messageId": "msg-" + id})
	})
	mux.HandleFunc("GET /sessions/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		fb.mu.Lock()
		evs := append([]dsh.RawBridgeEvent(nil), fb.script...)
		fb.mu.Unlock()
		for _, ev := range evs {
			if ev.SessionID != "" && ev.SessionID != id {
				continue
			}
			b, _ := json.Marshal(ev)
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
		// End the stream so SnapshotEvents returns.
		_ = r.Context().Err()
	})
	mux.HandleFunc("POST /sessions/{id}/close", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":  "not_supported",
			"detail": "upstream DSH SDK has no per-session close; shutdown the runtime instead",
		})
	})
	mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, _ *http.Request) {
		fb.mu.Lock()
		fb.shutdownHit = true
		fb.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	fb.srv = httptest.NewServer(mux)
	t.Cleanup(fb.srv.Close)
	return fb
}

func TestNewBridgeStripsTrailingSlash(t *testing.T) {
	b := dsh.NewBridge("http://127.0.0.1:1/")
	if b == nil {
		t.Fatal("nil bridge")
	}
}

func TestBridgeHealth(t *testing.T) {
	fb := newFakeBridge(t)
	b := dsh.NewBridge(fb.srv.URL)
	st, err := b.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if st.Status != "ready" {
		t.Errorf("status = %q, want ready", st.Status)
	}
	if st.ServerInfo.Name != "dsh-bridge-test" {
		t.Errorf("serverInfo.name = %q", st.ServerInfo.Name)
	}
}

func TestBridgeHealthUnreachable(t *testing.T) {
	b := dsh.NewBridge("http://127.0.0.1:1")
	_, err := b.Health(context.Background())
	if err == nil {
		t.Fatal("expected error for unreachable bridge")
	}
	if !errors.Is(err, dsh.ErrUnreachable) {
		t.Errorf("want ErrUnreachable, got %v", err)
	}
}

func TestBridgeCreateSession(t *testing.T) {
	fb := newFakeBridge(t)
	b := dsh.NewBridge(fb.srv.URL)
	ref, err := b.CreateSession(context.Background(), dsh.CreateSessionRequest{
		Cwd:      "/tmp/repo",
		Provider: "deepseek",
		Model:    "deepseek-chat",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if ref.ID != "sess-deepseek-chat" {
		t.Errorf("id = %q", ref.ID)
	}
	if ref.Provider != "deepseek" {
		t.Errorf("provider = %q", ref.Provider)
	}
}

func TestBridgeCreateSessionMissingFields(t *testing.T) {
	fb := newFakeBridge(t)
	b := dsh.NewBridge(fb.srv.URL)
	if _, err := b.CreateSession(context.Background(), dsh.CreateSessionRequest{Cwd: "/tmp"}); err == nil {
		t.Error("expected error for missing provider/model")
	}
}

func TestBridgePrompt(t *testing.T) {
	fb := newFakeBridge(t)
	b := dsh.NewBridge(fb.srv.URL)
	ref, err := b.CreateSession(context.Background(), dsh.CreateSessionRequest{Cwd: "/x", Provider: "deepseek", Model: "deepseek-chat"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.Prompt(context.Background(), ref.ID, []dsh.ContentBlock{{Type: "text", Text: "hello"}})
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if res.MessageID != "msg-"+ref.ID {
		t.Errorf("messageId = %q", res.MessageID)
	}
}

func TestBridgeCloseSessionReturnsNotSupported(t *testing.T) {
	fb := newFakeBridge(t)
	b := dsh.NewBridge(fb.srv.URL)
	ref, err := b.CreateSession(context.Background(), dsh.CreateSessionRequest{Cwd: "/x", Provider: "deepseek", Model: "deepseek-chat"})
	if err != nil {
		t.Fatal(err)
	}
	err = b.CloseSession(context.Background(), ref.ID)
	if err == nil {
		t.Fatal("expected ErrNotSupported")
	}
	if !errors.Is(err, dsh.ErrNotSupported) {
		t.Errorf("want ErrNotSupported, got %v", err)
	}
	if !strings.Contains(err.Error(), "upstream DSH") {
		t.Errorf("error message should mention upstream DSH: %v", err)
	}
}

func TestBridgeShutdown(t *testing.T) {
	fb := newFakeBridge(t)
	b := dsh.NewBridge(fb.srv.URL)
	if err := b.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !fb.shutdownHit {
		t.Error("bridge did not call /shutdown")
	}
}

func TestBridgeSnapshotEventsReturnsNormalizedBatch(t *testing.T) {
	fb := newFakeBridge(t)
	fb.script = []dsh.RawBridgeEvent{
		// session.status is exposed directly (no envelope wrap).
		{Kind: "session.status", SessionID: "sess-x", Status: "running"},
		// session.event wraps an upstream notification inside `event`.
		{Kind: "session.event", SessionID: "sess-x", Event: json.RawMessage(`{"kind":"agent.started","data":{"role":"scout"}}`)},
		{Kind: "session.event", SessionID: "sess-x", Event: json.RawMessage(`{"kind":"tool.completed","data":{"tool":"read"}}`)},
		{Kind: "session.event", SessionID: "sess-x", Event: json.RawMessage(`{"kind":"agent.completed","data":{"outcome":"advance"}}`)},
		{Kind: "session.status", SessionID: "sess-x", Status: "idle"},
	}
	b := dsh.NewBridge(fb.srv.URL)
	batch, err := b.SnapshotEvents(context.Background(), "sess-x")
	if err != nil {
		t.Fatalf("SnapshotEvents: %v", err)
	}
	if len(batch) != 5 {
		t.Fatalf("batch = %d, want 5", len(batch))
	}
	want := []domain.EventKind{
		domain.EventKindAgentRunning,
		domain.EventKindAgentStarted,
		domain.EventKindToolCompleted,
		domain.EventKindAgentCompleted,
		domain.EventKindAgentIdle,
	}
	for i, ev := range batch {
		if ev.Kind != want[i] {
			t.Errorf("batch[%d].Kind = %q, want %q", i, ev.Kind, want[i])
		}
		if ev.SessionID != "sess-x" {
			t.Errorf("batch[%d].SessionID = %q", i, ev.SessionID)
		}
	}
	// The wrapped tool.completed event carried an upstream payload; it
	// must survive unwrap + normalization so reviewers can audit it.
	if !strings.Contains(string(batch[2].Data), `"tool":"read"`) {
		t.Errorf("batch[2].Data missing tool payload: %s", batch[2].Data)
	}
	// agent.completed payload (the agent outcome) must also survive.
	if !strings.Contains(string(batch[3].Data), `"outcome":"advance"`) {
		t.Errorf("batch[3].Data missing outcome payload: %s", batch[3].Data)
	}
}

func TestNormalizeUnknownKindBecomesPassthrough(t *testing.T) {
	raw := &dsh.RawBridgeEvent{
		Kind:      "some.unknown.upstream.kind",
		SessionID: "sess-x",
		Event:     json.RawMessage(`{"hello":"world"}`),
	}
	ev := raw.Normalize(time.Now())
	if ev.Kind != domain.EventKindRawPassthrough {
		t.Errorf("kind = %q, want raw.passthrough", ev.Kind)
	}
	if !strings.Contains(string(ev.Data), `"hello":"world"`) {
		t.Errorf("Data should preserve raw payload: %s", ev.Data)
	}
}

func TestNormalizeNilReceiver(t *testing.T) {
	var raw *dsh.RawBridgeEvent
	ev := raw.Normalize(time.Now())
	if ev.Kind != domain.EventKindRawPassthrough {
		t.Errorf("kind = %q, want raw.passthrough", ev.Kind)
	}
}

func TestBridgeSnapshotEventsEmptySessionID(t *testing.T) {
	fb := newFakeBridge(t)
	b := dsh.NewBridge(fb.srv.URL)
	if _, err := b.SnapshotEvents(context.Background(), ""); err == nil {
		t.Error("expected error for empty sessionID")
	}
}

func TestBridgePromptEmptySessionID(t *testing.T) {
	fb := newFakeBridge(t)
	b := dsh.NewBridge(fb.srv.URL)
	if _, err := b.Prompt(context.Background(), "", nil); err == nil {
		t.Error("expected error for empty sessionID")
	}
}

func TestBridgeCloseSessionEmptySessionID(t *testing.T) {
	fb := newFakeBridge(t)
	b := dsh.NewBridge(fb.srv.URL)
	if err := b.CloseSession(context.Background(), ""); err == nil {
		t.Error("expected error for empty sessionID")
	}
}
