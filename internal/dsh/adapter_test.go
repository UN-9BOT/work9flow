package dsh_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/unbot/work9flow/internal/dsh"
)

// mockDSH is an httptest server that pretends to be DeepSeek Harness.
// It records calls and returns deterministic responses.
type mockDSH struct {
	mu          sync.Mutex
	sessions    map[string]*session
	steers      []recordedCall
	followups   []recordedCall
	cancels     []string
	events      []dsh.RawEvent
	healthHits  atomic.Int32
}

type session struct {
	ID      string
	Role    string
	Model   string
	Canceled bool
}

type recordedCall struct {
	SessionID string
	Body      json.RawMessage
}

func newMockDSH() *mockDSH {
	return &mockDSH{sessions: map[string]*session{}}
}

func (m *mockDSH) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		m.healthHits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		var req dsh.SessionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		s := &session{ID: "sess-" + req.Role, Role: req.Role, Model: req.Model}
		m.sessions[s.ID] = s
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"id": s.ID})
	})
	mux.HandleFunc("/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
		// /v1/sessions/{id}/steer, /followup, /cancel, /events
		path := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) < 2 {
			http.Error(w, "bad path", http.StatusBadRequest)
			return
		}
		sid, op := parts[0], parts[1]
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		defer m.mu.Unlock()
		s, ok := m.sessions[sid]
		if !ok {
			http.Error(w, "no session", http.StatusNotFound)
			return
		}
		switch op {
		case "steer":
			m.steers = append(m.steers, recordedCall{SessionID: sid, Body: body})
		case "followup":
			m.followups = append(m.followups, recordedCall{SessionID: sid, Body: body})
		case "cancel":
			m.cancels = append(m.cancels, sid)
			s.Canceled = true
		case "events":
			// Return the queued events as newline-delimited JSON (NDJSON / SSE-ish).
			w.Header().Set("Content-Type", "application/x-ndjson")
			for _, e := range m.events {
				if e.SessionID != sid {
					continue
				}
				b, _ := json.Marshal(e)
				_, _ = w.Write(b)
				_, _ = w.Write([]byte("\n"))
			}
			return
		default:
			http.Error(w, "unknown op", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

func newTestClient(t *testing.T) (*dsh.Client, *mockDSH, func()) {
	t.Helper()
	mock := newMockDSH()
	srv := httptest.NewServer(mock.handler())
	c := dsh.NewClient(srv.URL)
	return c, mock, srv.Close
}

// ---------- tests ----------

func TestHealth(t *testing.T) {
	c, mock, stop := newTestClient(t)
	defer stop()
	st, err := c.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st != "ok" {
		t.Errorf("status = %q", st)
	}
	if mock.healthHits.Load() != 1 {
		t.Errorf("hits = %d", mock.healthHits.Load())
	}
}

func TestSessionLifecycle(t *testing.T) {
	c, _, stop := newTestClient(t)
	defer stop()
	id, err := c.CreateSession(context.Background(), dsh.SessionRequest{Role: "scout", Model: "deepseek-v3"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "sess-scout" {
		t.Errorf("id = %q", id)
	}
}

func TestSteerFollowupCancel(t *testing.T) {
	c, mock, stop := newTestClient(t)
	defer stop()
	id, _ := c.CreateSession(context.Background(), dsh.SessionRequest{Role: "scout"})

	if err := c.Steer(context.Background(), id, dsh.SteerRequest{Message: "go left"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Followup(context.Background(), id, dsh.FollowupRequest{Message: "any updates?"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if len(mock.steers) != 1 || len(mock.followups) != 1 || len(mock.cancels) != 1 {
		t.Errorf("recorded = steer=%d followup=%d cancel=%d",
			len(mock.steers), len(mock.followups), len(mock.cancels))
	}
}

func TestSteerUnknownSession(t *testing.T) {
	c, _, stop := newTestClient(t)
	defer stop()
	err := c.Steer(context.Background(), "nope", dsh.SteerRequest{Message: "x"})
	if err == nil {
		t.Fatal("expected error for unknown session")
	}
}

func TestEventNormalization(t *testing.T) {
	c, mock, stop := newTestClient(t)
	defer stop()
	id, _ := c.CreateSession(context.Background(), dsh.SessionRequest{Role: "scout"})
	mock.mu.Lock()
	mock.events = []dsh.RawEvent{
		{SessionID: id, Kind: "agent.started", At: time.Unix(1, 0).UTC(), Data: json.RawMessage(`{"role":"scout"}`)},
		{SessionID: id, Kind: "tool.started", At: time.Unix(2, 0).UTC(), Data: json.RawMessage(`{"tool":"read","path":"x.go"}`)},
		{SessionID: id, Kind: "tool.completed", At: time.Unix(3, 0).UTC(), Data: json.RawMessage(`{"tool":"read","ok":true}`)},
		{SessionID: id, Kind: "model.message", At: time.Unix(4, 0).UTC(), Data: json.RawMessage(`{"role":"assistant","text":"hi"}`)},
		{SessionID: id, Kind: "agent.completed", At: time.Unix(5, 0).UTC(), Data: json.RawMessage(`{"role":"scout"}`)},
	}
	mock.mu.Unlock()
	events, err := c.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 {
		t.Fatalf("len = %d", len(events))
	}
	normalized := dsh.Normalize(id, events)
	// model.message is not in the rawKindMap (DSH-internal output
	// stream) so it is dropped; the remaining 4 events are normalised.
	if len(normalized) != 4 {
		t.Fatalf("normalized len = %d, want 4", len(normalized))
	}
	want := []string{"agent.started", "tool.started", "tool.completed", "agent.completed"}
	for i, w := range want {
		if string(normalized[i].Kind) != w {
			t.Errorf("normalized[%d].Kind = %q, want %q", i, normalized[i].Kind, w)
		}
	}
}
