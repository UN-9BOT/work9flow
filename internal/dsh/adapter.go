package dsh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/unbot/work9flow/internal/domain"
)

// ErrUnreachable is returned when DSH cannot be contacted.
var ErrUnreachable = errors.New("dsh: unreachable")

// SessionRequest is what work9flow sends to start a DSH session.
// Only fields work9flow actually needs; DSH extras are ignored.
type SessionRequest struct {
	Role  string `json:"role"`
	Model string `json:"model,omitempty"`
}

// SessionRef is the work9flow-side handle to a DSH session. Stored
// on AgentRun.SessionRef; never exposed to the TUI directly.
type SessionRef struct {
	ID     string `json:"id"`
	Role   string `json:"role"`
	Model  string `json:"model,omitempty"`
}

// SteerRequest is sent to DSH as runtime guidance to a currently
// running session.
type SteerRequest struct {
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// FollowupRequest is the normal next user turn.
type FollowupRequest struct {
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// RawEvent is one entry in DSH's session event stream. DSH may emit
// many kinds; Normalize maps the known subset to work9flow event
// kinds and preserves unknowns as RawPassthrough (Data only).
type RawEvent struct {
	SessionID string          `json:"session_id"`
	Kind      string          `json:"kind"`
	At        time.Time       `json:"at"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// NormalizedEvent is a RawEvent annotated with its work9flow kind.
type NormalizedEvent struct {
	SessionID string
	Kind      domain.EventKind
	At        time.Time
	Data      json.RawMessage
}

// rawKindMap maps DSH event names to work9flow EventKind. Unknown
// kinds return "", which Normalize drops.
var rawKindMap = map[string]domain.EventKind{
	"agent.started":    domain.EventKindAgentStarted,
	"agent.completed":  domain.EventKindAgentCompleted,
	"agent.failed":     domain.EventKindAgentFailed,
	"agent.canceled":   domain.EventKindAgentCanceled,
	"tool.started":     domain.EventKindToolStarted,
	"tool.completed":   domain.EventKindToolCompleted,
	"tool.failed":      domain.EventKindToolFailed,
	"stage.started":    domain.EventKindStageStarted,
	"stage.completed":  domain.EventKindStageCompleted,
	"stage.failed":     domain.EventKindStageFailed,
}

// Normalize maps a slice of RawEvents to work9flow NormalizedEvents.
// Unknown DSH kinds are dropped (we never want to leak DSH-internal
// taxonomy into work9flow's public event model).
func Normalize(sessionID string, raw []RawEvent) []NormalizedEvent {
	out := make([]NormalizedEvent, 0, len(raw))
	for _, r := range raw {
		wfKind, ok := rawKindMap[r.Kind]
		if !ok {
			continue
		}
		out = append(out, NormalizedEvent{
			SessionID: sessionID,
			Kind:      wfKind,
			At:        r.At,
			Data:      r.Data,
		})
	}
	return out
}

// Client talks to a DSH HTTP endpoint. Safe for concurrent use.
type Client struct {
	base   string
	http   *http.Client
}

// NewClient returns a Client rooted at baseURL (no trailing slash).
func NewClient(baseURL string) *Client {
	return &Client{
		base:   baseURL,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

// WithHTTPClient returns a Client using the given http.Client. Used
// by tests to inject a custom RoundTripper.
func (c *Client) WithHTTPClient(hc *http.Client) *Client {
	cp := *c
	cp.http = hc
	return &cp
}

// Health returns the DSH-reported status string (e.g. "ok").
func (c *Client) Health(ctx context.Context) (string, error) {
	var out struct {
		Status string `json:"status"`
	}
	if err := c.do(ctx, "GET", "/v1/health", nil, &out); err != nil {
		return "", err
	}
	return out.Status, nil
}

// CreateSession asks DSH to start a session and returns its id.
func (c *Client) CreateSession(ctx context.Context, req SessionRequest) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.do(ctx, "POST", "/v1/sessions", req, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// Steer sends runtime guidance to an in-flight session. DSH applies
// it at the next supported model-step boundary.
func (c *Client) Steer(ctx context.Context, sessionID string, req SteerRequest) error {
	return c.do(ctx, "POST", sessionPath(sessionID, "steer"), req, nil)
}

// Followup queues a normal next user turn.
func (c *Client) Followup(ctx context.Context, sessionID string, req FollowupRequest) error {
	return c.do(ctx, "POST", sessionPath(sessionID, "followup"), req, nil)
}

// Cancel asks DSH to stop a session.
func (c *Client) Cancel(ctx context.Context, sessionID string) error {
	return c.do(ctx, "POST", sessionPath(sessionID, "cancel"), nil, nil)
}

// Events reads the full session event stream. DSH returns newline-
// delimited JSON; we decode and return the slice.
func (c *Client) Events(ctx context.Context, sessionID string) ([]RawEvent, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.base+sessionPath(sessionID, "events"), nil)
	if err != nil {
		return nil, fmt.Errorf("dsh: build events req: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: events: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: events status %d", ErrUnreachable, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("dsh: events -> %d", resp.StatusCode)
	}
	var out []RawEvent
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e RawEvent
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("dsh: decode event: %w", err)
		}
		out = append(out, e)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("dsh: scan events: %w", err)
	}
	return out, nil
}

func sessionPath(id, op string) string {
	return "/v1/sessions/" + id + "/" + op
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("dsh: marshal %s %s: %w", method, path, err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return fmt.Errorf("dsh: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w: status %d", ErrUnreachable, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("dsh: %s %s -> %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("dsh: decode %s %s: %w", method, path, err)
	}
	return nil
}
