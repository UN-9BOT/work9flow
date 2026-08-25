package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/unbot/work9flow/internal/protocol"
)

// newWSServer starts an httptest server that mimics the runtime's
// /v1/runs/{id}/events/stream endpoint. The handler upgrades the
// connection, sends each entry from `events` as a text frame, then
// blocks until the client closes.
func newWSServer(t *testing.T, runID string, events []protocol.EventDTO) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/runs/"+runID+"/events/stream", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		for _, e := range events {
			payload, _ := json.Marshal(e)
			if werr := conn.Write(ctx, websocket.MessageText, payload); werr != nil {
				return
			}
		}
		// Drain reads so pongs get answered; exit when the client
		// closes the connection.
		for {
			if _, _, rerr := conn.Read(ctx); rerr != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestSubscribeEventsReceivesAll(t *testing.T) {
	events := []protocol.EventDTO{
		{RunID: "r1", Seq: 1, Kind: "workflow.created", At: 100},
		{RunID: "r1", Seq: 2, Kind: "stage.started", At: 101},
		{RunID: "r1", Seq: 3, Kind: "agent.completed", At: 102},
	}
	srv := newWSServer(t, "r1", events)
	c := NewClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch, end, err := c.SubscribeEvents(ctx, "r1", 0)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer end()
	got := 0
	deadline := time.After(2 * time.Second)
	for got < len(events) {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed early at %d/%d", got, len(events))
			}
			if ev.Seq != int64(got+1) {
				t.Errorf("seq = %d, want %d", ev.Seq, got+1)
			}
			got++
		case <-deadline:
			t.Fatalf("timeout waiting for event %d", got+1)
		}
	}
}

func TestSubscribeEventsCancel(t *testing.T) {
	srv := newWSServer(t, "r1", []protocol.EventDTO{
		{RunID: "r1", Seq: 1, Kind: "workflow.created", At: 100},
	})
	c := NewClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch, end, err := c.SubscribeEvents(ctx, "r1", 0)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("first event never arrived")
	}
	end()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected channel closed after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Error("cancel did not close channel within 2s")
	}
}

func TestSubscribeEventsAfter(t *testing.T) {
	// After should be passed through as ?after=N. Verify the server
	// receives the expected query value.
	var gotAfter string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/runs/r1/events/stream", func(w http.ResponseWriter, r *http.Request) {
		gotAfter = r.URL.Query().Get("after")
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "ok")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, end, err := c.SubscribeEvents(ctx, "r1", 42)
	if err != nil {
		t.Fatal(err)
	}
	end()
	if gotAfter != "42" {
		t.Errorf("server got after=%q, want 42", gotAfter)
	}
}
