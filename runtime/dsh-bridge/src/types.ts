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

/** Provider-mapped SDK outcome (matches upstream protocol/src/types.ts). */
export type SdkRunStatus = 'ok' | 'error'

/** Upstream subagent stop reason — passed through as opaque string. */
export type SubagentStopReason = string

/** SSE event payload (one upstream notification, normalized). */
export type BridgeEvent =
  | {
      kind: 'session.event'
      sessionId: string
      event: Record<string, unknown>
    }
  | {
      kind: 'session.status'
      sessionId: string
      status: 'idle' | 'running'
    }
  | {
      kind: 'subagent.started'
      parentSessionId: string
      childSessionId: string
    }
  | {
      kind: 'subagent.finished'
      provider: string
      agentId: string
      parentSessionId: string
      childSessionId: string
      status: SdkRunStatus
      stopReason: SubagentStopReason
      lastAssistantMessage?: ContentBlock[]
    }
  | {
      kind: 'bridge.error'
      message: string
    }
  /**
   * Transport-level failure surfaced by the bridge when its upstream
   * subscription (SSE → session.event) errors out. The Go side MUST
   * route this frame to the typed transport error channel instead of
   * Normalize() (which would mask the failure as raw.passthrough and
   * leave the Runner waiting for an event that will never arrive).
   * A plain HTTP EOF is NOT sufficient: bufio.Scanner.Err() reports
   * nil for a clean close, so the bridge must emit this explicit
   * control frame before closing the stream.
   */
  | {
      kind: 'bridge.transport_error'
      message: string
    }

/** Health payload. */
export interface HealthResponse {
  status: 'starting' | 'ready' | 'closed' | 'error'
  serverInfo?: { name: string; version: string }
  message?: string
}
