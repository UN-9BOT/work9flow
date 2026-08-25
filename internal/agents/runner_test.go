package agents_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/unbot/work9flow/internal/agents"
	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/dsh"
	"github.com/unbot/work9flow/internal/storage"
)

// scriptedDSH is an httptest server that pretends to be DSH. Tests
// pre-load a script of (sessionID -> events); on /events it returns
// the events as NDJSON. The runner polls; the first call that returns
// an agent.completed event lets the runner exit its loop.
type scriptedDSH struct {
	mu     sync.Mutex
	script map[string][]dsh.RawEvent
	hits   int
}

func newScriptedDSH() *scriptedDSH { return &scriptedDSH{script: map[string][]dsh.RawEvent{}} }

func (s *scriptedDSH) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		var req dsh.SessionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		id := "sess-" + req.Role
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	})
	mux.HandleFunc("/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) < 2 {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		sid, op := parts[0], parts[1]
		switch op {
		case "events":
			s.mu.Lock()
			evs := append([]dsh.RawEvent(nil), s.script[sid]...)
			s.hits++
			s.mu.Unlock()
			w.Header().Set("Content-Type", "application/x-ndjson")
			for _, e := range evs {
				b, _ := json.Marshal(e)
				_, _ = w.Write(b)
				_, _ = w.Write([]byte("\n"))
			}
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	return mux
}

func newRig(t *testing.T, script map[string][]dsh.RawEvent) (*agents.Runner, *scriptedDSH, storage.Repo, func()) {
	t.Helper()
	dshMock := newScriptedDSH()
	dshMock.script = script
	srv := httptest.NewServer(dshMock.handler())
	c := dsh.NewClient(srv.URL)
	repo, err := storage.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	r := agents.New(c, repo)
	r.PollInterval = 5 * time.Millisecond
	r.PollBudget = 500 * time.Millisecond
	return r, dshMock, repo, func() { srv.Close(); _ = repo.Close() }
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
	r, _, repo, cleanup := newRig(t, map[string][]dsh.RawEvent{
		"sess-scout": {
			{SessionID: "sess-scout", Kind: "agent.started", At: time.Unix(1, 0).UTC(), Data: json.RawMessage(`{"role":"scout"}`)},
			{SessionID: "sess-scout", Kind: "tool.completed", At: time.Unix(2, 0).UTC(), Data: json.RawMessage(`{"tool":"read"}`)},
			{SessionID: "sess-scout", Kind: "agent.completed", At: time.Unix(3, 0).UTC(),
				Data: json.RawMessage(`{"outcome":"advance","summary":"scout done"}`)},
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
	r, _, repo, cleanup := newRig(t, map[string][]dsh.RawEvent{
		"sess-gatekeeper": {
			{SessionID: "sess-gatekeeper", Kind: "agent.completed", At: time.Unix(1, 0).UTC(),
				Data: json.RawMessage(`{"outcome":"approve","summary":"ok"}`)},
		},
	})
	defer cleanup()
	run := newRun(t, repo, "run-approve")
	out, err := r.Run(context.Background(), run, "gatekeeper", "", agents.Instructions{Message: "review"})
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
			r, _, repo, cleanup := newRig(t, map[string][]dsh.RawEvent{
				"sess-x": {
					{SessionID: "sess-x", Kind: "agent.completed", At: time.Unix(1, 0).UTC(), Data: json.RawMessage(tc.data)},
				},
			})
			defer cleanup()
			run := newRun(t, repo, "run-"+tc.name)
			out, err := r.Run(context.Background(), run, "x", "", agents.Instructions{Message: "go"})
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
	r, _, repo, cleanup := newRig(t, map[string][]dsh.RawEvent{
		"sess-x": {
			{SessionID: "sess-x", Kind: "agent.started", At: time.Unix(1, 0).UTC()},
		},
	})
	defer cleanup()
	run := newRun(t, repo, "run-timeout")
	if _, err := r.Run(context.Background(), run, "x", "", agents.Instructions{}); err == nil {
		t.Fatal("expected ErrSessionIncomplete")
	}
}
