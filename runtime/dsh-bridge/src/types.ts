/**
 * Wire types exposed by the bridge HTTP API.
 *
 * The bridge is a TypeScript process that wraps the official
 * @deepseek-ai/dsh-sdk-client, owns ONE dsh-jsonrpc-agent subprocess,
 * and exposes a tiny HTTP surface for the Go work9flowd.
 *
 * DSH SDK specifics (jsonrpc, agent/* notifications, content-block encoding)
 * stay inside the bridge — work9flow sees normalized shapes only.
 */
import type { ContentBlock } from '@deepseek-ai/dsh-sdk-client'

/** Body of POST /sessions — initialize-on-create handshake. */
export interface CreateSessionRequest {
  /** Workspace cwd recorded on the SDK-created session header. */
  cwd: string
  /** Provider route (e.g. "deepseek-official"). */
  provider: string
  /** Model name (e.g. "deepseek-v4-flash"). */
  model: string
  /** Optional output-token cap for SDK-created agents. */
  maxTokens?: number
}

/** Response of POST /sessions. */
export interface CreateSessionResponse {
  sessionId: string
  serverInfo: { name: string; version: string }
}

/** Body of POST /sessions/:id/prompt. */
export interface PromptRequest {
  contentBlocks: ContentBlock[]
}

/** Response of POST /sessions/:id/prompt — durable enqueue receipt. */
export interface PromptResponse {
  messageId: string
}

/** Body of POST /sessions/:id/run — owned Activity interval. */
export interface RunRequest {
  contentBlocks: ContentBlock[]
}

/** Provider-mapped SDK outcome (matches upstream protocol/src/types.ts). */
export type SdkRunStatus = 'ok' | 'error'

/** Upstream subagent stop reason — passed through as opaque string. */
export type SubagentStopReason = string

/**
 * Exact upstream SessionEvent.type catalog. The bridge normalizes
 * upstream `session.event` notifications by flattening the envelope
 * and exposing `event.type` here. The list is closed — any value
 * outside this catalog that comes out of upstream is treated as a
 * protocol error.
 */
export type SessionEventType =
  | 'agent/inbox/spliced'
  | 'turn/start'
  | 'turn/end'
  | 'step/start'
  | 'step/end'
  | 'user/message'
  | 'assistant/chunk'
  | 'assistant/message'
  | 'tool/call'
  | 'tool/result'
  | 'todo/write'
  | 'request/header'
  | 'request/context'
  | 'session/end-seed'

/** Closed set of `type` values the bridge emits on its SSE streams. */
export type BridgeEventType =
  | SessionEventType
  | 'session.status'
  | 'subagent.started'
  | 'subagent.finished'
  | 'run.start'
  | 'run.end'
  | 'bridge.transport_error'

/** SSE event payload (one upstream notification, normalized). */
export type BridgeEvent =
  | {
      /** Upstream SessionEvent (notification method = session.event). */
      sessionId: string
      type: SessionEventType
      data?: unknown
    }
  | {
      /** Upstream notification method = session.status (not a SessionEvent). */
      sessionId: string
      type: 'session.status'
      status: 'idle' | 'running'
    }
  | {
      /** Upstream notification method = subagent.started (lineage edge). */
      type: 'subagent.started'
      parentSessionId: string
      childSessionId: string
    }
  | {
      /** Upstream notification method = subagent.finished. */
      type: 'subagent.finished'
      provider: string
      agentId: string
      parentSessionId: string
      childSessionId: string
      status: SdkRunStatus
      stopReason: SubagentStopReason
      lastAssistantMessage?: ContentBlock[]
    }
  /**
   * Activity-interval lifecycle frame (POST /sessions/:id/run only).
   * The first SSE event on a /run stream carries the durable enqueue
   * receipt so the consumer can correlate upstream `agent/inbox/spliced`
   * events to the prompt that produced them.
   */
  | {
      type: 'run.start'
      messageId: string
    }
  /**
   * Activity-interval lifecycle frame. Emitted on the /run stream
   * when the root session reaches `session.status = idle`, the
   * natural close of one owned Activity. After this frame the
   * stream is closed; the consumer must not expect any further
   * frames.
   */
  | {
      type: 'run.end'
      reason: 'idle' | 'transport_error'
    }
  /**
   * Transport-level failure surfaced by the bridge when its upstream
   * subscription (SSE → session.event) errors out. The Go side MUST
   * route this frame to the typed transport error channel instead of
   * Normalize() (which would mask the failure as raw.passthrough and
   * leave the Runner waiting for an event that will never arrive).
   * On a /run stream this is followed by a `run.end` with
   * reason=`transport_error` so the consumer always sees the same
   * terminal sequence.
   */
  | {
      type: 'bridge.transport_error'
      message: string
    }

/** Health payload. */
export interface HealthResponse {
  status: 'starting' | 'ready' | 'closed' | 'error'
  serverInfo?: { name: string; version: string }
  message?: string
}
