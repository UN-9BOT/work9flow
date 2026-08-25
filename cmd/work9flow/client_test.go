package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/unbot/work9flow/internal/protocol"
)

func newMockRuntime(t *testing.T) (*httptest.Server, *Client) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.HealthResponse{Status: "ok", Version: "test"})
	})
	mux.HandleFunc("/v1/runs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(protocol.RunListResponse{Runs: []protocol.RunSummary{
				{ID: "r1", WorkflowID: "feature-development", State: "PLANNING", Pending: 0},
			}})
		case http.MethodPost:
			var req protocol.RunCreateRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.NewEncoder(w).Encode(protocol.RunCreateResponse{Run: protocol.RunDetail{
				ID: "r-new", WorkflowID: req.WorkflowID, State: "NEW", OriginalTask: req.OriginalTask,
			}})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/v1/runs/r1", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(protocol.RunGetResponse{Run: protocol.RunDetail{
				ID: "r1", WorkflowID: "feature-development", State: "PLANNING", Stage: "planning",
			}})
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/v1/runs/r1/events", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.EventListResponse{
			Events: []protocol.EventDTO{
				{RunID: "r1", Seq: 1, Kind: "workflow.created"},
				{RunID: "r1", Seq: 2, Kind: "stage.started"},
			},
			LatestSeq: 2,
		})
	})
	mux.HandleFunc("/v1/runs/r1/attentions", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(protocol.AttentionListResponse{
			Attentions: []protocol.AttentionDTO{
				{ID: "a1", RunID: "r1", Status: "OPEN", Blocking: true, Title: "which DB?"},
			},
		})
	})
	mux.HandleFunc("/v1/attentions/a1/answer", func(w http.ResponseWriter, r *http.Request) {
		var req protocol.AttentionAnswerRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		_ = json.NewEncoder(w).Encode(protocol.AttentionAnswerResponse{
			Attention: protocol.AttentionDTO{ID: "a1", Status: "ANSWERED", Blocking: true, Title: "which DB?", Answer: req.Answer},
		})
	})
	mux.HandleFunc("/v1/runs/r1/steer", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v1/runs/r1/followup", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	c := NewClient(srv.URL)
	return srv, c
}

func TestClientHealth(t *testing.T) {
	srv, c := newMockRuntime(t)
	defer srv.Close()
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" {
		t.Errorf("status = %q", h.Status)
	}
}

func TestClientListRuns(t *testing.T) {
	srv, c := newMockRuntime(t)
	defer srv.Close()
	runs, err := c.ListRuns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != "r1" {
		t.Errorf("runs = %+v", runs)
	}
}

func TestClientGetRun(t *testing.T) {
	srv, c := newMockRuntime(t)
	defer srv.Close()
	r, err := c.GetRun(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "r1" || r.State != "PLANNING" {
		t.Errorf("run = %+v", r)
	}
}

func TestClientCreateRun(t *testing.T) {
	srv, c := newMockRuntime(t)
	defer srv.Close()
	r, err := c.CreateRun(context.Background(), protocol.RunCreateRequest{
		WorkflowID:   "feature-development",
		OriginalTask: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != "r-new" {
		t.Errorf("id = %q", r.ID)
	}
}

func TestClientCancelRun(t *testing.T) {
	srv, c := newMockRuntime(t)
	defer srv.Close()
	if err := c.CancelRun(context.Background(), "r1"); err != nil {
		t.Fatal(err)
	}
}

func TestClientEventsAfter(t *testing.T) {
	srv, c := newMockRuntime(t)
	defer srv.Close()
	evs, latest, err := c.EventsAfter(context.Background(), "r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 {
		t.Fatalf("events = %d", len(evs))
	}
	if latest != 2 {
		t.Errorf("latest = %d", latest)
	}
}

func TestClientListAttentions(t *testing.T) {
	srv, c := newMockRuntime(t)
	defer srv.Close()
	atts, err := c.ListAttentions(context.Background(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 1 || atts[0].ID != "a1" {
		t.Errorf("atts = %+v", atts)
	}
}

func TestClientAnswerAttention(t *testing.T) {
	srv, c := newMockRuntime(t)
	defer srv.Close()
	a, err := c.AnswerAttention(context.Background(), "a1", json.RawMessage(`"postgres"`))
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != "ANSWERED" {
		t.Errorf("status = %q", a.Status)
	}
}

func TestClientSteerAndFollowup(t *testing.T) {
	srv, c := newMockRuntime(t)
	defer srv.Close()
	if err := c.Steer(context.Background(), "r1", protocol.SteerRequest{AgentID: "a", Message: "go"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Followup(context.Background(), "r1", protocol.FollowupRequest{AgentID: "a", Message: "next"}); err != nil {
		t.Fatal(err)
	}
}

func TestClientHandles404(t *testing.T) {
	srv, c := newMockRuntime(t)
	defer srv.Close()
	_, err := c.GetRun(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want 404", err)
	}
}
