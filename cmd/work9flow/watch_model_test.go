package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/unbot/work9flow/internal/protocol"
)

// TestStartWatchOpensSubscription verifies that calling startWatch on
// the model populates wsRunID + wsCancel. We can't easily observe a
// bubbletea cmd without a full program, but the public field reads
// are sufficient: wsRunID should match the requested run and
// wsCancel must be non-nil.
func TestStartWatchOpensSubscription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		// Drain reads so the connection stays healthy.
		go func() {
			<-r.Context().Done()
			_ = conn.Close(websocket.StatusNormalClosure, "done")
		}()
	}))
	t.Cleanup(srv.Close)

	m := newModel(srv.URL)
	if m.wsRunID != "" {
		t.Fatalf("wsRunID should start empty, got %q", m.wsRunID)
	}
	cmd := m.startWatch("r-watch")
	if cmd == nil {
		t.Fatal("startWatch returned nil cmd")
	}
	if m.wsRunID != "r-watch" {
		t.Errorf("wsRunID = %q, want r-watch", m.wsRunID)
	}
	if m.wsCancel == nil {
		t.Error("wsCancel must be set after startWatch")
	}
	m.stopWatch()
	if m.wsRunID != "" {
		t.Errorf("wsRunID after stop = %q, want empty", m.wsRunID)
	}
	if m.wsCancel != nil {
		t.Error("wsCancel must be nil after stopWatch")
	}
}

func TestIsStateEvent(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"workflow.created", true},
		{"workflow.completed", true},
		{"workflow.failed", true},
		{"stage.started", true},
		{"stage.completed", true},
		{"stage.failed", true},
		{"agent.started", true},
		{"agent.completed", true},
		{"attention.required", true},
		{"attention.resolved", true},
		{"tool.started", false},
		{"artifact.created", false},
		{"user.steer", false},
	}
	for _, c := range cases {
		if got := isStateEvent(c.in); got != c.want {
			t.Errorf("isStateEvent(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestReadOneWSWrapsEvent(t *testing.T) {
	ch := make(chan protocol.EventDTO, 1)
	ch <- protocol.EventDTO{RunID: "r", Seq: 7, Kind: "stage.started"}
	cmd := readOneWS(ch, "r")
	msg := cmd()
	if msg == nil {
		t.Fatal("cmd returned nil msg")
	}
	wm, ok := msg.(wsEventMsg)
	if !ok {
		t.Fatalf("msg type = %T, want wsEventMsg", msg)
	}
	if wm.event.Seq != 7 || wm.runID != "r" {
		t.Errorf("msg = %+v", wm)
	}
}

// keep imports used
var _ = strings.ToLower
var _ = time.Second
