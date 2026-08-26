// Package dsh is the only place in work9flow that knows DSH exists.
//
// The actual DeepSeek Harness integration lives in two layers:
//
//  1. runtime/dsh-bridge/  — a small TypeScript process that owns ONE
//     dsh-jsonrpc-agent subprocess and speaks the official
//     @deepseek-ai/dsh-sdk-client over stdio JSON-RPC.
//
//  2. This package — a typed HTTP client for the bridge. It exposes only
//     the methods work9flow actually needs: create session, run an
//     owned Activity interval (subscribe-before-prompt + idle-bound),
//     shutdown runtime. NO steer/followup/cancel because upstream DSH
//     SDK has no public method for any of those.
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

// ErrRunIncomplete is returned when the bridge closes the Activity
// stream without a natural `run.end{reason: idle}` — i.e. the upstream
// never reached root session.status=idle inside the caller's deadline.
// Per upstream contract the activity's natural close is the root
// session going idle; we do NOT fabricate a terminal event from EOF
// or from `turn/end` / `assistant/message`.
var ErrRunIncomplete = errors.New("dsh: Activity interval ended without session.status=idle")

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

// RunRequest is the body of POST /sessions/:id/run. The bridge owns
// the Activity lifecycle: subscribe-before-prompt, await
// agent/inbox/spliced(messageId), stream upstream notifications on SSE
// until root session.status=idle, then close.
type RunRequest struct {
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

// RawBridgeEvent is one SSE frame from the bridge.
//
// Wire shape mirrors the bridge's upstream-faithful normalization
// (see runtime/dsh-bridge/src/types.ts). Each upstream SessionEvent is
// flattened to `{sessionId, type, data}` where `type` is the upstream
// `event.type` drawn from the closed SessionEvent catalog:
//
//   agent/inbox/spliced, assistant/message, tool/call, tool/result,
//   step/start, step/end, turn/end
//
// Upstream notification methods that are not SessionEvents keep their
// method name as `type` and carry their own fields: `session.status`,
// `subagent.started`, `subagent.finished`. Activity lifecycle frames
// `run.start` and `run.end` and the transport-error frame use distinct
// `type` values.
//
// We do NOT invent upstream kinds. No `agent.completed`, no
// `agent.started`, no `tool.completed`. The bridge exposes the
// upstream catalog verbatim and `Normalize` maps it onto work9flow
// domain kinds.
type RawBridgeEvent struct {
	Type     string `json:"type"`
	SessionID string `json:"sessionId,omitempty"`

	// session.status payload
	Status string `json:"status,omitempty"`

	// SessionEvent payload (flattened `data` from the upstream envelope)
	Data json.RawMessage `json:"data,omitempty"`

	// run.start payload
	MessageID string `json:"messageId,omitempty"`

	// run.end payload
	Reason string `json:"reason,omitempty"`

	// bridge.transport_error payload
	Message string `json:"message,omitempty"`

	// subagent.* payload
	ParentID string          `json:"parentSessionId,omitempty"`
	ChildID  string          `json:"childSessionId,omitempty"`
	AgentID  string          `json:"agentId,omitempty"`
	Provider string          `json:"provider,omitempty"`
	StopRsn  string          `json:"stopReason,omitempty"`
	LastAssistant json.RawMessage `json:"lastAssistantMessage,omitempty"`
}

// NormalizedEvent is one bridge event translated to work9flow domain
// kinds. Upstream types outside the SessionEvent catalog map to
// EventKindRawPassthrough with the original payload preserved in Data.
type NormalizedEvent struct {
	SessionID string
	Kind      domain.EventKind
	At        time.Time
	Data      json.RawMessage
}

// Normalize maps one RawBridgeEvent to a NormalizedEvent. Upstream
// types outside the catalog drop to EventKindRawPassthrough and the
// raw frame is preserved in Data.
func (r *RawBridgeEvent) Normalize(now time.Time) NormalizedEvent {
	if r == nil {
		return NormalizedEvent{Kind: domain.EventKindRawPassthrough, At: now, Data: json.RawMessage(`null`)}
	}
	switch r.Type {
	case "session.status":
		// Upstream session.status carries the activity-level running/idle
		// state. We map to dedicated domain kinds so consumers can route
		// on AgentIdle as the natural Activity close.
		switch strings.ToLower(r.Status) {
		case "idle":
			return NormalizedEvent{SessionID: r.SessionID, Kind: domain.EventKindAgentIdle, At: now, Data: r.eventData()}
		case "running":
			return NormalizedEvent{SessionID: r.SessionID, Kind: domain.EventKindAgentRunning, At: now, Data: r.eventData()}
		default:
			return NormalizedEvent{SessionID: r.SessionID, Kind: domain.EventKindAgentRunning, At: now, Data: r.eventData()}
		}
	case "assistant/message":
		// Upstream assistant/message carries the agent's last assistant
		// text. Persist the full frame (with `type: "assistant/message"`)
		// as raw.passthrough so the Runner can recover the upstream
		// type from Data and extract the work9flow outcome contract
		// from the content blocks.
		raw, _ := json.Marshal(r)
		return NormalizedEvent{SessionID: r.SessionID, Kind: domain.EventKindRawPassthrough, At: now, Data: raw}
	case "tool/call":
		return NormalizedEvent{SessionID: r.SessionID, Kind: domain.EventKindToolStarted, At: now, Data: r.eventData()}
	case "tool/result":
		return NormalizedEvent{SessionID: r.SessionID, Kind: domain.EventKindToolCompleted, At: now, Data: r.eventData()}
	case "step/start":
		return NormalizedEvent{SessionID: r.SessionID, Kind: domain.EventKindStageStarted, At: now, Data: r.eventData()}
	case "step/end":
		return NormalizedEvent{SessionID: r.SessionID, Kind: domain.EventKindStageCompleted, At: now, Data: r.eventData()}
	case "turn/end":
		// Upstream turn/end is NOT a synonym for activity end. The
		// Activity's natural close is session.status=idle on the root
		// session, surfaced as run.end{reason=idle} by the bridge.
		// We keep the full frame as raw.passthrough so consumers can
		// audit the upstream `type` discriminator.
		raw, _ := json.Marshal(r)
		return NormalizedEvent{SessionID: r.SessionID, Kind: domain.EventKindRawPassthrough, At: now, Data: raw}
	case "agent/inbox/spliced":
		// The durable enqueue receipt confirming the prompt was spliced
		// into the agent's inbox. Persist the full frame as
		// raw.passthrough so consumers can audit the upstream `type`.
		raw, _ := json.Marshal(r)
		return NormalizedEvent{SessionID: r.SessionID, Kind: domain.EventKindRawPassthrough, At: now, Data: raw}
	case "subagent.started":
		// Upstream subagent.started is a lineage edge — the runtime
		// notifies the client of a child session so the client can scope
		// its subscription. Persist as AgentStarted for parity with the
		// old shape; semantics are unchanged.
		return NormalizedEvent{SessionID: r.ChildID, Kind: domain.EventKindAgentStarted, At: now, Data: r.eventData()}
	case "subagent.finished":
		// Upstream subagent.finished is the terminal of a descendant
		// session. Persist as AgentCompleted so the existing reduceOutcome
		// path can pick it up if the descendant is the work9flow agent.
		return NormalizedEvent{SessionID: r.ChildID, Kind: domain.EventKindAgentCompleted, At: now, Data: r.eventData()}
	case "run.start", "run.end":
		// Activity lifecycle frames. Persist the raw frame as
		// raw.passthrough — the Runner reads `run.end{reason=idle}`
		// off the channel to know the interval closed, and audit
		// data preserves the run.start messageId and run.end reason.
		raw, _ := json.Marshal(r)
		return NormalizedEvent{Kind: domain.EventKindRawPassthrough, At: now, Data: raw}
	case "bridge.transport_error":
		// Surface as raw.passthrough — the SSE reader routes this kind
		// to the typed errCh before Normalize runs, so Normalize is
		// only called as a defensive fallback.
		return NormalizedEvent{Kind: domain.EventKindRawPassthrough, At: now, Data: r.eventData()}
	default:
		// Unknown upstream type — preserve the raw frame so consumers can
		// inspect it without the bridge knowing every domain kind. We do
		// NOT invent a domain mapping here.
		raw, _ := json.Marshal(r)
		return NormalizedEvent{Kind: domain.EventKindRawPassthrough, At: now, Data: raw}
	}
}

// eventData returns the upstream event payload if present, otherwise
// an empty JSON object. Used so every NormalizedEvent carries at least
// {} instead of nil — consumers can json.Unmarshal without a guard.
func (r *RawBridgeEvent) eventData() json.RawMessage {
	if len(r.Data) > 0 {
		return r.Data
	}
	return json.RawMessage(`{}`)
}

// Bridge is the typed HTTP client to runtime/dsh-bridge/. The surface
// mirrors the upstream DSH SDK contract:
//
//   - Health / CreateSession / Shutdown — unary.
//   - Run — owned Activity interval (subscribe-before-prompt,
//     await agent/inbox/spliced(messageId), stream until
//     session.status=idle). Returns a bounded event channel that
//     closes when the bridge emits `run.end`.
//
// There is NO Steer / Followup / Cancel method. Upstream DSH has no
// wire-level cancel; abandoning a turn means closing the runtime.
type Bridge struct {
	baseURL string
	// hc handles unary calls (Health / CreateSession /
	// Shutdown). It has a bounded total Timeout so a hung bridge
	// cannot pin a goroutine forever.
	hc *http.Client
	// streamHC handles the SSE stream (Run). It has NO overall
	// Timeout because long-lived real-DSH agent runs exceed any
	// reasonable per-call deadline; lifecycle is owned by the
	// request context and ctx-cancel observed in readSSE.
	streamHC *http.Client
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

// Run starts one owned Activity interval on `sessionID`.
//
// Lifecycle (mirrors upstream `HarnessSession.run`):
//   1. The bridge subscribes to upstream notifications BEFORE it issues
//      `prompt(...)`.
//   2. The bridge awaits `agent/inbox/spliced(messageId)` — the durable
//      enqueue receipt — before forwarding activity.
//   3. The bridge streams upstream notifications until root
//      `session.status=idle`, then emits `run.end{reason=idle}` and
//      closes the stream.
//
// The returned channel emits NormalizedEvents for every upstream
// SessionEvent, every session.status, every subagent notification, and
// both `run.start` and `run.end`. The channel closes when the bridge
// closes the stream. On transport failure the bridge emits
// `bridge.transport_error` BEFORE `run.end{reason=transport_error}`,
// and the SSE reader routes the transport error to errCh so the caller
// can surface it without misclassifying it as a domain event.
//
// The caller MUST read evCh to completion before consulting errCh —
// transport errors arrive during the stream and the errCh is drained
// after evCh closes.
func (b *Bridge) Run(ctx context.Context, sessionID string, blocks []ContentBlock) (<-chan NormalizedEvent, <-chan error) {
	evCh := make(chan NormalizedEvent, 16)
	errCh := make(chan error, 1)
	if sessionID == "" {
		errCh <- fmt.Errorf("dsh: Run: sessionID required")
		close(evCh)
		close(errCh)
		return evCh, errCh
	}
	body, err := json.Marshal(RunRequest{ContentBlocks: blocks})
	if err != nil {
		errCh <- fmt.Errorf("dsh: marshal run body: %w", err)
		close(evCh)
		close(errCh)
		return evCh, errCh
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.baseURL+"/sessions/"+sessionID+"/run", bytes.NewReader(body))
	if err != nil {
		errCh <- err
		close(evCh)
		close(errCh)
		return evCh, errCh
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := b.streamHC.Do(req)
	if err != nil {
		errCh <- fmt.Errorf("%w: %v", ErrUnreachable, err)
		close(evCh)
		close(errCh)
		return evCh, errCh
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		errCh <- &apiError{Status: resp.StatusCode, Detail: "run stream not 200 OK"}
		close(evCh)
		close(errCh)
		return evCh, errCh
	}
	go b.readSSERun(ctx, resp, evCh, errCh)
	return evCh, errCh
}

// readSSERun consumes the SSE stream from /sessions/:id/run. Closes
// evCh + errCh when the bridge emits `run.end` or the stream errors.
func (b *Bridge) readSSERun(ctx context.Context, resp *http.Response, evCh chan<- NormalizedEvent, errCh chan<- error) {
	defer close(evCh)
	defer close(errCh)
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var dataBuf bytes.Buffer
	terminated := false
	sawTransportError := false
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
			// Control frames first — they are not domain events.
			switch raw.Type {
			case "bridge.transport_error":
				sawTransportError = true
				msg := raw.Message
				if msg == "" {
					msg = "dsh: bridge reported transport error"
				}
				select {
				case errCh <- fmt.Errorf("%s", msg):
				default:
				}
				continue
			case "run.end":
				// Terminal: send to evCh so the Runner can inspect the
				// reason and audit the receipt. Then return — the
				// stream is about to close anyway.
				select {
				case evCh <- raw.Normalize(time.Now()):
				case <-ctx.Done():
					return
				}
				if raw.Reason == "transport_error" && !sawTransportError {
					select {
					case errCh <- fmt.Errorf("dsh: run ended due to transport_error"):
					default:
					}
				}
				terminated = true
				return
			}
			select {
			case evCh <- raw.Normalize(time.Now()):
			case <-ctx.Done():
				return
			}
		}
	}
	if !terminated {
		if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
			select {
			case errCh <- err:
			default:
			}
		}
	}
}

// Shutdown asks the bridge to stop the upstream runtime.
func (b *Bridge) Shutdown(ctx context.Context) error {
	return b.doJSON(ctx, http.MethodPost, "/shutdown", nil, nil)
}

// Prompt queues one user message on the session and returns the
// durable enqueue receipt. This is a low-level call that the old
// (pre-Activity) runner used together with SnapshotEvents; the new
// path is `Run`, which owns subscribe-before-prompt + idle-bound
// streaming. Kept for tests and tool-call style external callers.
func (b *Bridge) Prompt(ctx context.Context, sessionID string, blocks []ContentBlock) (PromptResult, error) {
	if sessionID == "" {
		return PromptResult{}, fmt.Errorf("dsh: Prompt: sessionID required")
	}
	var out PromptResult
	if err := b.doJSON(ctx, http.MethodPost, "/sessions/"+sessionID+"/prompt",
		map[string]any{"contentBlocks": blocks}, &out); err != nil {
		return PromptResult{}, err
	}
	return out, nil
}

// CloseSession asks the bridge to close a session. The upstream DSH
// SDK has no per-session close, so this always returns
// ErrNotSupported — surfaced honestly to work9flow callers.
func (b *Bridge) CloseSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("dsh: CloseSession: sessionID required")
	}
	err := b.doJSON(ctx, http.MethodPost, "/sessions/"+sessionID+"/close", nil, nil)
	if err == nil {
		return fmt.Errorf("%w: bridge unexpectedly accepted close for session %s", ErrNotSupported, sessionID)
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotImplemented {
		return fmt.Errorf("%w: %s", ErrNotSupported, apiErr.Detail)
	}
	return err
}

// Events opens the legacy /sessions/:id/events SSE firehose and
// returns a NormalizedEvent channel. The new path is `Run`, which
// owns subscribe-before-prompt + idle-bound streaming. Kept for
// tests and tool-call style external callers.
func (b *Bridge) Events(ctx context.Context, sessionID string) (<-chan NormalizedEvent, <-chan error) {
	evCh := make(chan NormalizedEvent, 16)
	errCh := make(chan error, 1)
	if sessionID == "" {
		errCh <- fmt.Errorf("dsh: Events: sessionID required")
		close(evCh)
		close(errCh)
		return evCh, errCh
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.baseURL+"/sessions/"+sessionID+"/events", nil)
	if err != nil {
		errCh <- err
		close(evCh)
		close(errCh)
		return evCh, errCh
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := b.streamHC.Do(req)
	if err != nil {
		errCh <- err
		close(evCh)
		close(errCh)
		return evCh, errCh
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		errCh <- &apiError{Status: resp.StatusCode, Detail: "events stream not 200 OK"}
		close(evCh)
		close(errCh)
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
			// Bridge transport-control frame: emitted by the bridge when
			// the upstream subscription pump errors out. We route it to
			// the typed errCh and stop reading, instead of passing it
			// through Normalize() (which would mask the failure as a
			// domain raw.passthrough and leave the Runner waiting for
			// an event that will never arrive). See reviewer P1 #2.
			var ctrl struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(payload, &ctrl); err == nil && ctrl.Type == "bridge.transport_error" {
				msg := ctrl.Message
				if msg == "" {
					msg = "dsh: bridge reported transport error"
				}
				select {
				case errCh <- fmt.Errorf("%s", msg):
				default:
				}
				return
			}
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

// SnapshotEvents opens the SSE stream for a session, reads every event
// the bridge pushes, and returns them as a batch when the stream
// closes. It is the polling-friendly counterpart to Events. The new
// path is `Run`, which returns a bounded batch by `run.end` rather
// than EOF. Kept for tests and external callers.
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
