// Package dsh is the work9flow boundary against the DeepSeek Harness
// (DSH) execution kernel.
//
// DSH ships as a Node service, not a Go module, so this package
// speaks HTTP/JSON to it. Nothing in work9flow (runtime, protocol,
// TUI) imports DSH-internal types — the only coupling point is the
// contract declared here.
package dsh

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrUnreachable is returned when DSH cannot be contacted.
var ErrUnreachable = errors.New("dsh: unreachable")

// Client talks to a DSH HTTP endpoint. Safe for concurrent use.
type Client struct {
	base   string
	http   *http.Client
}

// NewClient returns a Client rooted at baseURL (no trailing slash).
func NewClient(baseURL string) *Client {
	return &Client{
		base: baseURL,
		http: &http.Client{Timeout: 10 * time.Second},
	}
}

// SessionRequest is what work9flow sends to start a DSH session.
// Only fields work9flow actually needs; DSH extras are ignored.
type SessionRequest struct {
	Role  string `json:"role"`
	Model string `json:"model,omitempty"`
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
