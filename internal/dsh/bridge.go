// Package dsh is the only place in work9flow that knows DSH exists.
//
// The actual DeepSeek Harness integration lives in two layers:
//
//  1. runtime/dsh-bridge/  — a small TypeScript process that owns ONE
//     dsh-jsonrpc-agent subprocess and speaks the official
//     @deepseek-ai/dsh-sdk-client over stdio JSON-RPC.
//
//  2. This package — a typed HTTP client for the bridge. It exposes only
//     the methods work9flow actually needs: create session, prompt, read
//     events (SSE), shutdown runtime. NO steer/followup/cancel because
//     upstream DSH SDK has no public method for any of those.
//
// `SessionRef`, `ContentBlock`, `NormalizedEvent` are work9flow-domain
// shapes. JSON-RPC, content-block encoding, and the wire envelopes stay
// inside the bridge and this package. NOTHING upstream of dsh/ imports
// dsh-session, jsonrpc, or any SDK type.
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
	"strings"
	"time"

	"github.com/unbot/work9flow/internal/domain"
)

// ErrUnreachable is returned when the bridge cannot be contacted.
var ErrUnreachable = errors.New("dsh: bridge unreachable")

// ErrNotSupported signals that an upstream capability gap was hit honestly
// (e.g. per-session close on DSH). It is NOT a bug to be papered over.
var ErrNotSupported = errors.New("dsh: operation not supported by upstream DSH")

// CreateSessionRequest is the body of POST /sessions on the bridge. The
// handshake fields match what upstream `initialize` accepts.
type CreateSessionRequest struct {
	Cwd       string `json:"cwd"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	MaxTokens *int   `json:"maxTokens,omitempty"`
}

// SessionRef is the work9flow-side handle to a DSH session. Stored on
// AgentRun; never exposed verbatim to the TUI (only its id and role).
type SessionRef struct {
	ID       string `json:"id"`
	Cwd      string `json:"cwd"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// ContentBlock is a single content unit inside a session/prompt request.
// Kept minimal — only the text variant is needed by current work9flow
// agents. Extend when a new block type is actually used.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// PromptRequest is the body of POST /sessions/:id/prompt.
type PromptRequest struct {
	ContentBlocks []ContentBlock `json:"contentBlocks"`
}

// PromptResult is the durable enqueue receipt returned by prompt().
type PromptResult struct {
	MessageID string `json:"messageId"`
}

// HealthStatus is the bridge + initialize handshake state.
type HealthStatus struct {
	Status     string `json:"status"`
	ServerInfo struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo,omitempty"`
	Message string `json:"message,omitempty"`
}

// RawBridgeEvent is one upstream notification off the bridge's SSE stream.
// The bridge maps wire method names to {kind, ...payload} shapes; we then
// further map to domain.EventKind for work9flow consumers.
type RawBridgeEvent struct {
	Kind      string          `json:"kind"`
	SessionID string          `json:"sessionId,omitempty"`
	Status    string          `json:"status,omitempty"`
	Event     json.RawMessage `json:"event,omitempty"`
	ParentID  string          `json:"parentSessionId,omitempty"`
	ChildID   string          `json:"childSessionId,omitempty"`
	AgentID   string          `json:"agentId,omitempty"`
	Provider  string          `json:"provider,omitempty"`
	StopRsn   string          `json:"stopReason,omitempty"`
	Message   string          `json:"message,omitempty"`
}

// NormalizedEvent is one bridge event translated to work9flow domain kinds.
// Unknown upstream kinds map to EventKindRawPassthrough (Data preserved).
type NormalizedEvent struct {
	SessionID string
	Kind      domain.EventKind
	At        time.Time
	Data      json.RawMessage
}

// rawKindMap maps bridge event kinds to work9flow domain kinds.
var rawKindMap = map[string]domain.EventKind{
	"agent.started":      domain.EventKindAgentStarted,
	"agent.completed":    domain.EventKindAgentCompleted,
	"agent.failed":       domain.EventKindAgentFailed,
	"agent.canceled":     domain.EventKindAgentCanceled,
	"tool.started":       domain.EventKindToolStarted,
	"tool.completed":     domain.EventKindToolCompleted,
	"tool.failed":        domain.EventKindToolFailed,
	"stage.started":      domain.EventKindStageStarted,
	"stage.completed":    domain.EventKindStageCompleted,
	"stage.failed":       domain.EventKindStageFailed,
	"session.status":     domain.EventKindAgentRunning, // override below by Status field
	"session.event":      domain.EventKindRawPassthrough,
	"subagent.started":   domain.EventKindAgentStarted,
	"subagent.finished":  domain.EventKindAgentCompleted,
}

// Normalize maps one RawBridgeEvent to a NormalizedEvent. Unknown kinds
// drop to EventKindRawPassthrough and keep the original payload in Data.
func (r *RawBridgeEvent) Normalize(now time.Time) NormalizedEvent {
	if r == nil {
		return NormalizedEvent{Kind: domain.EventKindRawPassthrough, At: now, Data: json.RawMessage(`null`)}
	}
	// session.event wraps an upstream notification (e.g. agent.completed)
	// inside the `event` field. Unwrap it so consumers see the inner kind,
	// not the outer envelope kind.
	if r.Kind == "session.event" && len(r.Event) > 0 {
		var inner struct {
			Kind string          `json:"kind"`
			Data json.RawMessage `json:"data,omitempty"`
		}
		if err := json.Unmarshal(r.Event, &inner); err == nil && inner.Kind != "" {
			if mapped, ok := rawKindMap[inner.Kind]; ok {
				data := inner.Data
				if len(data) == 0 {
					data = json.RawMessage(`{}`)
				}
				return NormalizedEvent{SessionID: r.SessionID, Kind: mapped, At: now, Data: data}
			}
			// Inner kind is unknown — surface it under raw.passthrough
			// with the original session.event envelope preserved.
			return NormalizedEvent{SessionID: r.SessionID, Kind: domain.EventKindRawPassthrough, At: now, Data: r.Event}
		}
	}
	// session.status is special-cased: the same upstream kind can be
	// either running or idle depending on the Status sub-field, so we
	// map both onto dedicated domain kinds and fall back to AgentRunning
	// for any unknown status value.
	if r.Kind == "session.status" {
		switch strings.ToLower(r.Status) {
		case "running":
			return NormalizedEvent{SessionID: r.SessionID, Kind: domain.EventKindAgentRunning, At: now, Data: r.eventData()}
		case "idle":
			return NormalizedEvent{SessionID: r.SessionID, Kind: domain.EventKindAgentIdle, At: now, Data: r.eventData()}
		default:
			return NormalizedEvent{SessionID: r.SessionID, Kind: domain.EventKindAgentRunning, At: now, Data: r.eventData()}
		}
	}
	if mapped, ok := rawKindMap[r.Kind]; ok {
		return NormalizedEvent{SessionID: r.SessionID, Kind: mapped, At: now, Data: r.eventData()}
	}
	// Preserve the raw payload for unknown kinds so consumers can still
	// inspect them without the bridge knowing every domain kind.
	raw, _ := json.Marshal(r)
	return NormalizedEvent{Kind: domain.EventKindRawPassthrough, At: now, Data: raw}
}

// eventData returns the upstream event payload if present, otherwise
// an empty JSON object. Used so every NormalizedEvent carries at least
// {} instead of nil — consumers can json.Unmarshal without a guard.
func (r *RawBridgeEvent) eventData() json.RawMessage {
	if len(r.Event) > 0 {
		return r.Event
	}
	return json.RawMessage(`{}`)
}

// Bridge is the typed HTTP client to runtime/dsh-bridge/. Methods mirror
// the upstream DSH SDK contract: create session (with initialize handshake),
// prompt (durable messageId receipt), events (SSE), shutdown.
//
// There is NO Steer / Followup / Cancel method. Upstream DSH has no
// wire-level cancel; abandoning a turn means closing the runtime.
type Bridge struct {
	baseURL    string
	// hc handles unary calls (Health / CreateSession / Prompt /
	// CloseSession / Shutdown). It has a bounded total Timeout so a
	// hung bridge cannot pin a goroutine forever.
	hc         *http.Client
	// streamHC handles the SSE Events stream. It has NO overall
	// Timeout because long-lived real-DSH agent runs exceed any
	// reasonable per-call deadline; lifecycle is owned by the
	// request context and ctx-cancel observed in readSSE.
	streamHC   *http.Client
}

// NewBridge constructs a Bridge pointed at the dsh-bridge HTTP API.
// baseURL is the bridge root, e.g. "http://127.0.0.1:7777".
func NewBridge(baseURL string) *Bridge {
	return &Bridge{
		baseURL:  strings.TrimRight(baseURL, "/"),
		hc:       &http.Client{Timeout: 10 * time.Second},
		streamHC: &http.Client{}, // no Timeout: SSE stream is long-lived
	}
}

// SetHTTPClient overrides the default HTTP client (for tests).
func (b *Bridge) SetHTTPClient(hc *http.Client) { b.hc = hc }

// SetStreamHTTPClient overrides the streaming HTTP client (for tests).
func (b *Bridge) SetStreamHTTPClient(hc *http.Client) { b.streamHC = hc }

// Health probes the bridge. Returns the bridge status (starting/ready/closed).
func (b *Bridge) Health(ctx context.Context) (HealthStatus, error) {
	var out HealthStatus
	if err := b.doJSON(ctx, http.MethodGet, "/health", nil, &out); err != nil {
		return HealthStatus{}, err
	}
	return out, nil
}

// CreateSession asks the bridge to lazily initialize the runtime and mint
// a session id. The bridge performs the upstream `initialize` handshake on
// the first /sessions call, pinning cwd+provider+model+maxTokens.
func (b *Bridge) CreateSession(ctx context.Context, req CreateSessionRequest) (SessionRef, error) {
	if req.Cwd == "" || req.Provider == "" || req.Model == "" {
		return SessionRef{}, fmt.Errorf("dsh: CreateSession: cwd, provider, model required")
	}
	var out struct {
		SessionID  string `json:"sessionId"`
		ServerInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := b.doJSON(ctx, http.MethodPost, "/sessions", req, &out); err != nil {
		return SessionRef{}, err
	}
	return SessionRef{
		ID:       out.SessionID,
		Cwd:      req.Cwd,
		Provider: req.Provider,
		Model:    req.Model,
	}, nil
}

// Prompt queues one user message on the session and returns the durable
// enqueue receipt (messageId). The agent run continues in the background;
// observe via Events.
func (b *Bridge) Prompt(ctx context.Context, sessionID string, blocks []ContentBlock) (PromptResult, error) {
	if sessionID == "" {
		return PromptResult{}, fmt.Errorf("dsh: Prompt: sessionID required")
	}
	if len(blocks) == 0 {
		return PromptResult{}, fmt.Errorf("dsh: Prompt: at least one content block required")
	}
	var out PromptResult
	body := PromptRequest{ContentBlocks: blocks}
	path := "/sessions/" + sessionID + "/prompt"
	if err := b.doJSON(ctx, http.MethodPost, path, body, &out); err != nil {
		return PromptResult{}, err
	}
	if out.MessageID == "" {
		return PromptResult{}, fmt.Errorf("dsh: Prompt: empty messageId in response")
	}
	return out, nil
}

// CloseSession asks the bridge to end a session. DSH upstream has no
// per-session close — the only honest answer is 501 with detail. Use
// Shutdown to stop the entire runtime.
func (b *Bridge) CloseSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("dsh: CloseSession: sessionID required")
	}
	err := b.doJSON(ctx, http.MethodPost, "/sessions/"+sessionID+"/close", nil, nil)
	if err == nil {
		return nil
	}
	// 501 not_supported is the expected answer from a correctly built bridge.
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.Status == 501 {
		return fmt.Errorf("%w: %s", ErrNotSupported, apiErr.Detail)
	}
	return err
}

// Shutdown gracefully stops the bridge and the underlying DSH runtime.
func (b *Bridge) Shutdown(ctx context.Context) error {
	return b.doJSON(ctx, http.MethodPost, "/shutdown", nil, nil)
}

// Events opens an SSE stream of normalized upstream notifications for the
// given session. The returned channels close when the bridge closes the
// stream (EOF, runtime exit, or ctx cancel).
func (b *Bridge) Events(ctx context.Context, sessionID string) (<-chan NormalizedEvent, <-chan error) {
	evCh := make(chan NormalizedEvent, 16)
	errCh := make(chan error, 1)
	if sessionID == "" {
		errCh <- fmt.Errorf("dsh: Events: sessionID required")
		close(evCh); close(errCh)
		return evCh, errCh
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+"/sessions/"+sessionID+"/events", nil)
	if err != nil {
		errCh <- err; close(evCh); close(errCh)
		return evCh, errCh
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := b.streamHC.Do(req)
	if err != nil {
		errCh <- err; close(evCh); close(errCh)
		return evCh, errCh
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		errCh <- &apiError{Status: resp.StatusCode, Detail: "events stream not 200 OK"}
		close(evCh); close(errCh)
		return evCh, errCh
	}
	go b.readSSE(ctx, resp, evCh, errCh)
	return evCh, errCh
}

// readSSE consumes the SSE stream line-by-line and fans events into evCh.
// A single SSE event block is a series of `data: <json>` lines followed by
// a blank line; we only inspect `data:` lines.
func (b *Bridge) readSSE(ctx context.Context, resp *http.Response, evCh chan<- NormalizedEvent, errCh chan<- error) {
	defer close(evCh)
	defer close(errCh)
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var dataBuf bytes.Buffer
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "data: "):
			dataBuf.WriteString(strings.TrimPrefix(line, "data: "))
		case line == "":
			if dataBuf.Len() == 0 {
				continue
			}
			payload := dataBuf.Bytes()
			dataBuf.Reset()
			var raw RawBridgeEvent
			if err := json.Unmarshal(payload, &raw); err != nil {
				select {
				case errCh <- fmt.Errorf("dsh: SSE decode: %w", err):
				default:
				}
				continue
			}
			select {
			case evCh <- raw.Normalize(time.Now()):
			case <-ctx.Done():
				return
			}
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
		select {
		case errCh <- err:
		default:
		}
	}
}

// apiError surfaces an HTTP error from the bridge.
type apiError struct {
	Status int
	Detail string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("dsh: bridge returned %d: %s", e.Status, e.Detail)
}

// doJSON is the workhorse: HTTP request with JSON body, JSON response,
// typed error mapping.
func (b *Bridge) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("dsh: marshal: %w", err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("dsh: read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		// 204 (No Content) is also a success path — caller passes nil out.
		if resp.StatusCode == http.StatusNoContent {
			return nil
		}
		var apiErrBody struct {
			Error  string `json:"error"`
			Detail string `json:"detail"`
		}
		_ = json.Unmarshal(respBody, &apiErrBody)
		return &apiError{Status: resp.StatusCode, Detail: apiErrBody.Detail}
	}
	if resp.StatusCode == http.StatusNoContent || len(respBody) == 0 || out == nil {
		return nil
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		return fmt.Errorf("dsh: decode body: %w (raw=%q)", err, string(respBody))
	}
	return nil
}
// SnapshotEvents opens the SSE stream for a session, reads every event
// the bridge pushes, and returns them as a batch when the stream
// closes. It is the polling-friendly counterpart to Events: callers
// (e.g. agents.Runner) drive a snapshot loop and inspect each batch
// for a terminal kind. This keeps the runner composable while still
// using the real streaming transport under the hood.
func (b *Bridge) SnapshotEvents(ctx context.Context, sessionID string) ([]NormalizedEvent, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("dsh: SnapshotEvents: sessionID required")
	}
	evCh, errCh := b.Events(ctx, sessionID)
	var batch []NormalizedEvent
	for ev := range evCh {
		batch = append(batch, ev)
	}
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, io.EOF) {
			return batch, err
		}
	default:
	}
	return batch, nil
}
