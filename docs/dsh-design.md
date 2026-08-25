# DSH client v2 — production design

> Status: **proposed**. Author: work9flow-4v1.1 (research synthesis).
> Implements epic: `work9flow-4v1` (dsh: production-grade client).

This document is the research synthesis for upgrading `internal/dsh`
from a one-shot NDJSON-polling client to a production-grade streaming,
resumable, observable client. It captures the upstream contract, the
reference patterns we considered, the decisions we made for work9flow,
and the migration path from v1.

## 1. Upstream DSH

The DSH that work9flow speaks to is the DeepSeek Harness agent runtime
(`github.com/deepseek-ai/deepseek-harness`). It is a Cordis-based plugin
harness where everything — sessions, tools, LLM providers, subagents —
is a service. Two surfaces matter to us:

- **HTTP BFF** (`packages/api/`) — Remote BFF assembly + Typert RPC gateway.
- **Wire SDK** (`packages/sdk/`) — JSON-RPC protocol, server, and TypeScript client.
- **Python runtime** (`python/`) — Bundled runtime and Python SDK.

Their `AGENTS.md` states the **pre-release stance** that matches
work9flow exactly: *"prefer the correct foundation over compatibility
shims: rename or repackage freely and update every reference together."*
That alignment is why we are willing to break v1's surface for v2 — there
are no external consumers yet, and foundation > compat.

We do **not** modify upstream DSH. The work9flow DSH client must speak
whichever surface upstream exposes. Today that surface is HTTP + NDJSON
(`POST /v1/sessions`, `GET /v1/sessions/{id}/events`, `POST
/v1/sessions/{id}/cancel`). The v2 design accommodates a future WebSocket
or MCP-SSE transport without breaking the v2 client.

## 2. Current surface (v1)

`internal/dsh/adapter.go` exposes:

| Method | Verb + path | Behaviour |
|---|---|---|
| `CreateSession` | `POST /v1/sessions` | returns session id |
| `Steer` | `POST /v1/sessions/{id}/steer` | runtime guidance |
| `Followup` | `POST /v1/sessions/{id}/followup` | next user turn |
| `Cancel` | `POST /v1/sessions/{id}/cancel` | ask DSH to stop |
| `Events` | `GET /v1/sessions/{id}/events` | **blocks until complete, returns slice** |
| `Health` | `GET /v1/health` | `{status}` |

`Events` is the weak link: it reads the full stream, then returns. There
is no cursor, no progress, no resumption, and no in-flight cancel
short of closing the HTTP body. Everything else compiles fine for the
single-shot use we have today.

Inline fallback `internal/llm/localdsh/server.go` mimics the same HTTP
shape so `make smoke-full` can run end-to-end without an external DSH.

## 3. Reference patterns considered

| Pattern | Source | Adopt? | Why |
|---|---|---|---|
| **MCP Streamable HTTP + SSE** | `modelcontextprotocol.io` 2025-06-17 | partial | Server-push with cursor + resumption = standard pattern. Use as contract model, not the wire format (upstream DSH does not speak MCP today). |
| **Claude Agent SDK** `query()` loop | docs.anthropic.com | yes (for tool protocol) | `query({prompt, options})` returns async iterable of SDK messages. We mirror that shape: `Stream(ctx, sessionID)` yields events until cancel or terminal. `resumable sessions` + custom tools give us the production-quality lifecycle. |
| **Claude Agent SDK hooks** | same | yes (for metrics) | PreToolUse/PostToolUse hooks at the runner boundary become our telemetry seam. |
| **Anthropic streaming cancellation** | docs.anthropic.com | yes | Anthropic-server cannot stop its own mid-flight inference; cancellation is a session-level concept. Work9flow cancellation therefore lives in DSH, not the LLM provider. |
| **Aider conventions** | aider.chat docs | no | Aider is repo-edit-centric; work9flow is run-centric with multi-role fan-out. Borrow nothing. |
| **Cursor Agent / Cline** | public docs | partial | Their WS-first event streaming is the UX we want for TUI live updates; we already have server-side WS in `internal/runtime/ws.go publishBroker`. The client side is what v2 adds. |

## 4. Decisions for work9flow v2

| Decision | Choice | Reason |
|---|---|---|
| Streaming transport | **NDJSON now, WS-ready** | Upstream is HTTP+NDJSON. Mirror that; keep an internal seam (`Transport` interface) so a future WebSocket transport drops in. |
| Cursor / resumption | **monotonic event id + `since=` query param** | Mirrors MCP pattern. Persist last-seen id on `AgentRun.SessionRef`. |
| Cancellation | **DSH-level cancel, not provider-level** | Anthropic-streaming rule: cannot stop mid-flight on the LLM side. The DSH session owns the lifecycle. |
| Tool-call protocol | **extend existing `EventKindTool{Started,Completed,Failed}`** | Already mapped in `rawKindMap`. Wire shape gains `tool.id`, `tool.name`, `tool.input`, `tool.output` fields. Runner handles them in-place. |
| Multi-session fan-out | **one `StreamingClient`, many `SessionRef`s, merged channel** | Reviewer requires reading many sessions in one engine step; keep ordering per-session, parallel across sessions. |
| Retry / backoff | **wrap `http.Client.RoundTripper`**, jitter, honor `Retry-After` | Standard HTTP behaviour; do not reinvent. |
| Observability | **`slog` structured logs + counters + last-event-id per session** | Stdlib + counters only; no Prometheus dependency yet. |
| Backward compat with v1 | **yes** | `Client.Events(...)` and `Client.Cancel(...)` keep working. New API is additive: `StreamingClient.Stream(...)`. |

## 5. Surface area (v2)

```
package dsh

type StreamingClient struct{ ... }

func NewStreamingClient(baseURL string, opts ...Option) *StreamingClient

// Stream opens a live event stream. cursor == "" starts from head.
// Returns:
//   - events: ordered channel of NormalizedEvent (tool.* included)
//   - errs:   terminal errors only (transport failure after backoff)
//   - cancel: closes the stream and asks DSH to stop the session
func (c *StreamingClient) Stream(ctx context.Context, sessionID, cursor string) (
    <-chan NormalizedEvent, <-chan error, func(error),
)

// Resume re-issues Stream from the last-seen cursor recorded on the session.
func (c *StreamingClient) Resume(ctx context.Context, ref SessionRef) (
    <-chan NormalizedEvent, <-chan error, func(error),
)

// Cancel is the same as v1's Cancel but idempotent and safe to call
// after a stream drop.
func (c *StreamingClient) Cancel(ctx context.Context, sessionID string) error

// OpenMany creates N sessions and returns a single merged stream.
func (c *StreamingClient) OpenMany(ctx context.Context, n int, req SessionRequest) (
    []SessionRef, <-chan NormalizedEvent, <-chan error, func(error),
)
```

Wire additions on `internal/protocol/api.go EventDTO`:

```go
type EventDTO struct {
    ...
    SessionID string          `json:"session_id"`
    Kind      string          `json:"kind"`
    At        time.Time       `json:"at"`
    Data      json.RawMessage `json:"data,omitempty"`
    ID        string          `json:"id,omitempty"`      // NEW: monotonic cursor
    Tool      *ToolPayload    `json:"tool,omitempty"`    // NEW: when kind in tool.*
}

type ToolPayload struct {
    ID     string          `json:"id"`
    Name   string          `json:"name"`
    Input  json.RawMessage `json:"input,omitempty"`
    Output json.RawMessage `json:"output,omitempty"`
    Err    string          `json:"err,omitempty"`
}
```

`internal/llm/localdsh/server.go` gains an opt-in `?since=<id>` filter
and emits one `tool.completed` event per session (default-on for
smoke-full, default-off in production for backward compat).

`internal/agents/runner.go` `Runner.Run` consumes `EventKindToolStarted`
+ `EventKindToolCompleted` and records them on `AgentRun.ToolCalls []ToolCall`.

## 6. Migration plan

Sequenced off the beads epic `work9flow-4v1`:

1. **4v1.1 — this doc.** ✅
2. **4v1.2 — streaming abstraction.** `internal/dsh/stream.go`; `StreamingClient.Stream` over chunked NDJSON; `Transport` interface for future WS.
3. **4v1.3 — session resumption.** `RawEvent.ID` + `localdsh ?since=` + `Runner` records last cursor.
4. **4v1.4 — tool-call protocol.** Wire `ToolPayload`; `Runner` records tool calls; inline smoke emits a tool event.
5. **4v1.5 — cancellation cleanup.** Cancel drains in-flight, surfaces `agent.canceled` if upstream hasn't.
6. **4v1.6 — backoff.** RoundTripper wrapper; `Retry-After` aware; 5 attempts cap.
7. **4v1.7 — multi-session fan-out.** `OpenMany` + merged channel ordering.
8. **4v1.8 — metrics + tracing.** `slog` fields, counters on `StreamingClient`, token counts from completion events.
9. **4v1.9 — smoke gate.** `make smoke-full` exercises resume-after-drop, cancel-mid-stream, tool-call-through-runner.

## 7. Out of scope

- Modifying upstream `deepseek-ai/deepseek-harness`.
- Adding MCP server-side compliance (no upstream DSH speaks MCP).
- Replacing `cmd/work9flow` TUI; it already subscribes to its own WS (`internal/runtime/ws.go`) and is independent of the DSH client.
- Changing `internal/runtime/ws.go` server-side handler beyond additive `?since=`.

## 8. Validation gates

Every step in the sequence must pass:

```sh
go test ./...
go vet ./...
make smoke         # CRUD-only, fast
make smoke-full    # inline DSH → DONE end-to-end
```

Plus per-step acceptance defined in the corresponding bead.

