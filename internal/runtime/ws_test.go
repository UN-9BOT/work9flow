package runtime

import (
	"bytes"
"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/protocol"
)

func TestWSReplaysHistoryThenStreams(t *testing.T) {
	_, base, repo := newTestServerWithRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	id := "run-ws"
	if err := repo.CreateRun(context.Background(), domain.WorkflowRun{
		ID: id, WorkflowID: "w", OriginalTask: "t", State: domain.RunDiscovery,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// Append 3 events directly.
	for i := 0; i < 3; i++ {
		if _, err := repo.AppendEvent(context.Background(), id,
			domain.EventKindStageStarted, now.Add(time.Duration(i)*time.Second), nil); err != nil {
			t.Fatal(err)
		}
	}

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/v1/runs/" + id + "/events/stream?after=1"
	c, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer c.CloseNow()

	// Read 2 history events (seq 2 and 3).
	got := readN(t, c, 2, 2*time.Second)
	if got[0].Seq != 2 || got[1].Seq != 3 {
		t.Fatalf("history seqs = %d,%d", got[0].Seq, got[1].Seq)
	}

	// Trigger a live event via the API (publish happens in the handler).
	body, _ := json.Marshal(protocol.SteerRequest{AgentID: "a-1", Message: "nudge"})
	r, err := http.Post(base+"/v1/runs/"+id+"/steer", "application/json", bytes.NewReader(body))
	if err != nil { t.Fatal(err) }
	r.Body.Close()
	one := readN(t, c, 1, 2*time.Second)
	if one[0].Kind != string(domain.EventKindSteerSent) {
		t.Fatalf("live kind = %q, want %q", one[0].Kind, domain.EventKindSteerSent)
	}
}

func TestWSRejectsMissingRun(t *testing.T) {
	_, base, _ := newTestServerWithRepo(t)
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/v1/runs/nope/events/stream"
	_, resp, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		// Dial error may surface as non-nil HTTP failure.
		if resp == nil || resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %v, err = %v", resp, err)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestWSPublishesOnAnswer(t *testing.T) {
	_, base, repo := newTestServerWithRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	id := "run-ws2"
	if err := repo.CreateRun(context.Background(), domain.WorkflowRun{
		ID: id, WorkflowID: "w", OriginalTask: "t", State: domain.RunDiscovery,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateAttention(context.Background(), domain.Attention{
		ID: "att-1", RunID: id, Kind: domain.AttentionQuestion,
		Status: domain.AttentionOpen, Blocking: true, Title: "?", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/v1/runs/" + id + "/events/stream"
	c, _, err := websocket.Dial(context.Background(), wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()

	// Trigger an event via the HTTP API: answer the attention.
	body, _ := json.Marshal(protocol.AttentionAnswerRequest{Answer: json.RawMessage(`"x"`)})
	r, err := http.Post(base+"/v1/attentions/att-1/answer", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()

	// WS should receive the attention.resolved event.
	ev := readN(t, c, 1, 2*time.Second)
	if ev[0].Kind != string(domain.EventKindAttentionResolved) {
		t.Errorf("kind = %q", ev[0].Kind)
	}
}

// readN reads up to n JSON-encoded events from c within d. Uses a
// per-read context with a deadline because github.com/coder/websocket
// has no SetReadDeadline; instead each Read carries its own context.
func readN(t *testing.T, c *websocket.Conn, n int, d time.Duration) []protocol.EventDTO {
	t.Helper()
	out := make([]protocol.EventDTO, 0, n)
	for len(out) < n {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		_, data, err := c.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read after %d events: %v", len(out), err)
		}
		var e protocol.EventDTO
		if err := json.Unmarshal(data, &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		out = append(out, e)
	}
	return out
}
