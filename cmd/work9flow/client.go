package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"

	"github.com/unbot/work9flow/internal/protocol"
)

// Client is a thin HTTP client for the work9flowd runtime. It is the
// only thing in the TUI that talks to the runtime; the bubbletea
// model renders Client outputs and calls Client methods.
type Client struct {
	base string
	http *http.Client
}

// NewClient returns a Client rooted at baseURL (no trailing slash).
func NewClient(baseURL string) *Client {
	return &Client{
		base: baseURL,
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// Health returns the runtime's /v1/health payload.
func (c *Client) Health(ctx context.Context) (protocol.HealthResponse, error) {
	var out protocol.HealthResponse
	err := c.do(ctx, "GET", "/v1/health", nil, &out)
	return out, err
}

// ListRuns returns the runtime's run list.
func (c *Client) ListRuns(ctx context.Context) ([]protocol.RunSummary, error) {
	var out protocol.RunListResponse
	if err := c.do(ctx, "GET", "/v1/runs", nil, &out); err != nil {
		return nil, err
	}
	return out.Runs, nil
}

// GetRun returns a single run's detail.
func (c *Client) GetRun(ctx context.Context, id string) (protocol.RunDetail, error) {
	var out protocol.RunGetResponse
	if err := c.do(ctx, "GET", "/v1/runs/"+id, nil, &out); err != nil {
		return protocol.RunDetail{}, err
	}
	return out.Run, nil
}

// CreateRun starts a new run with req.
func (c *Client) CreateRun(ctx context.Context, req protocol.RunCreateRequest) (protocol.RunDetail, error) {
	var out protocol.RunCreateResponse
	if err := c.do(ctx, "POST", "/v1/runs", req, &out); err != nil {
		return protocol.RunDetail{}, err
	}
	return out.Run, nil
}

// CancelRun cancels the run via DELETE.
func (c *Client) CancelRun(ctx context.Context, id string) error {
	return c.do(ctx, "DELETE", "/v1/runs/"+id, nil, nil)
}

// EventsAfter returns events with seq > after. latest is the highest
// seq seen in the returned slice so the caller can resume.
func (c *Client) EventsAfter(ctx context.Context, runID string, after int64) ([]protocol.EventDTO, int64, error) {
	u := "/v1/runs/" + runID + "/events"
	if after > 0 {
		u += "?after=" + int64ToString(after)
	}
	var out protocol.EventListResponse
	if err := c.do(ctx, "GET", u, nil, &out); err != nil {
		return nil, 0, err
	}
	return out.Events, out.LatestSeq, nil
}

// ListAttentions returns the run's open attentions.
func (c *Client) ListAttentions(ctx context.Context, runID string) ([]protocol.AttentionDTO, error) {
	var out protocol.AttentionListResponse
	if err := c.do(ctx, "GET", "/v1/runs/"+runID+"/attentions", nil, &out); err != nil {
		return nil, err
	}
	return out.Attentions, nil
}

// AnswerAttention submits an answer for an attention.
func (c *Client) AnswerAttention(ctx context.Context, attentionID string, answer json.RawMessage) (protocol.AttentionDTO, error) {
	body := protocol.AttentionAnswerRequest{Answer: answer}
	var out protocol.AttentionAnswerResponse
	if err := c.do(ctx, "POST", "/v1/attentions/"+attentionID+"/answer", body, &out); err != nil {
		return protocol.AttentionDTO{}, err
	}
	return out.Attention, nil
}

// Steer sends a steer request to a running agent.
func (c *Client) Steer(ctx context.Context, runID string, req protocol.SteerRequest) error {
	return c.do(ctx, "POST", "/v1/runs/"+runID+"/steer", req, nil)
}

// Followup queues a followup turn for a run.
func (c *Client) Followup(ctx context.Context, runID string, req protocol.FollowupRequest) error {
	return c.do(ctx, "POST", "/v1/runs/"+runID+"/followup", req, nil)
}

// SubscribeEvents opens a WebSocket to /v1/runs/{id}/events/stream
// and returns a buffered channel of EventDTO plus a cancel func.
// The channel is closed when the connection drops or cancel is called.
// The first read may also receive historical events (seq > 0 if the
// caller passed after > 0).
//
// Used by the TUI to live-update the detail/events views without
// re-polling every pollInterval.
func (c *Client) SubscribeEvents(ctx context.Context, runID string, after int64) (<-chan protocol.EventDTO, func(), error) {
	u, err := url.Parse(c.base)
	if err != nil {
		return nil, nil, fmt.Errorf("client: bad base %q: %w", c.base, err)
	}
	u.Scheme = "ws"
	u.Path = "/v1/runs/" + runID + "/events/stream"
	if after > 0 {
		q := u.Query()
		q.Set("after", int64ToString(after))
		u.RawQuery = q.Encode()
	}
	conn, _, err := websocket.Dial(ctx, u.String(), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("client: ws dial: %w", err)
	}
	ch := make(chan protocol.EventDTO, 64)
	cancel := func() {
		_ = conn.Close(websocket.StatusNormalClosure, "client closed")
	}
	go func() {
		defer close(ch)
		defer cancel()
		for {
			// coder/websocket requires the read loop to also handle
			// control frames. We use Read with a per-message deadline.
			readCtx, readCancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, data, err := conn.Read(readCtx)
			readCancel()
			if err != nil {
				return
			}
			var ev protocol.EventDTO
			if err := json.Unmarshal(data, &ev); err != nil {
				continue
			}
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, cancel, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("client: marshal %s %s: %w", method, path, err)
		}
		rdr = bytes.NewReader(buf)
	}
	u, err := url.Parse(c.base)
	if err != nil {
		return fmt.Errorf("client: bad base %q: %w", c.base, err)
	}
	u.Path = path
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rdr)
	if err != nil {
		return fmt.Errorf("client: build %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("client: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("client: %s %s -> %d: %s", method, path, resp.StatusCode, string(raw))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("client: decode %s %s: %w", method, path, err)
	}
	return nil
}

func int64ToString(n int64) string {
	if n == 0 {
		return "0"
	}
	negative := false
	if n < 0 {
		negative = true
		n = -n
	}
	digits := "0123456789"
	out := ""
	for n > 0 {
		out = string(digits[n%10]) + out
		n /= 10
	}
	if negative {
		out = "-" + out
	}
	return out
}
