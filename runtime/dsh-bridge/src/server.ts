/**
 * The dsh-bridge HTTP server.
 *
 * Owns ONE dsh-jsonrpc-agent subprocess per process (per upstream
 * HarnessClient semantics: one initialize pins cwd/provider/model for
 * the runtime's lifetime). Multiple sessions can share the same runtime;
 * each session gets its own subscription via
 * `client.subscribeSessionTree(id)`.
 *
 * For work9flow's per-AgentRun ownership model (4v1.11) we will spawn
 * one bridge process per AgentRun — this MVP keeps one bridge = one
 * runtime and exposes the contract.
 *
 * NO invented semantics:
 *   - cancellation is unsupported upstream; /close returns 501 with a
 *     clear message rather than emulating it.
 *   - SSE streams only upstream notifications (session.event, session.status,
 *     subagent.started, subagent.finished). No agent/* fan-out, no stealth
 *     steering. The Go client maps these to work9flow domain events.
 *   - serverInfo on /health and /sessions is the value the runtime
 *     actually returned in its `initialize` handshake. We never fabricate
 *     name/version; if the handshake has not yet completed we report
 *     `{ name: 'unknown', version: 'unknown' }` so callers can tell the
 *     gap from a real identity.
 *
 * Activity interval (`POST /sessions/:id/run`) mirrors upstream
 * `HarnessSession.run`:
 *   1. Subscribe FIRST (`subscribeSessionTree(id)`).
 *   2. `prompt(...)` → durable enqueue receipt (messageId).
 *   3. Wait until that messageId appears in an `agent/inbox/spliced`
 *      session.event for the root session — this is the receipt.
 *      Skip events before that (they are noise from a prior turn).
 *   4. Forward notifications on SSE until the root session reaches
 *      `session.status = idle`. Stream closes after `run.end`.
 *
 * The bridge never relies on EOF or on invented terminal events
 * (`agent.completed`, `turn/end` as a synonym for activity end).
 * The activity interval's natural close is `session.status=idle` for
 * the root session — same signal upstream `HarnessSession.run` uses.
 */
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http'
import { randomUUID } from 'node:crypto'
import { resolve as resolvePath } from 'node:path'
import {
  HarnessClient,
  type ContentBlock,
  type HarnessClientOptions,
  type HarnessNotification,
} from '@deepseek-ai/dsh-sdk-client'
import type { InitializeResult } from '@deepseek-ai/dsh-sdk-protocol'
import { resolveLaunch, type LaunchSpec } from './launcher.js'
import type {
  BridgeEvent,
  CreateSessionRequest,
  CreateSessionResponse,
  HealthResponse,
  PromptRequest,
  PromptResponse,
  RunRequest,
  SessionEventType,
  SubagentStopReason,
} from './types.js'

/** Unknown serverInfo when the runtime handshake has not yet completed. */
const UNKNOWN_SERVER_INFO: { name: string; version: string } = {
  name: 'unknown',
  version: 'unknown',
}

/** Exact upstream SessionEvent.type catalog. Anything outside is a protocol error. */
const SESSION_EVENT_TYPES: ReadonlySet<string> = new Set<SessionEventType>([
  'agent/inbox/spliced',
  'assistant/message',
  'tool/call',
  'tool/result',
  'step/start',
  'step/end',
  'turn/end',
])

function isSessionEventType(value: unknown): value is SessionEventType {
  return typeof value === 'string' && SESSION_EVENT_TYPES.has(value)
}

/**
 * Expected runtime server identity, asserted at startup. This is the
 * `serverInfo` value the upstream DSH runtime reports in its
 * `initialize` handshake. It is the runtime's PROTOCOL identity, NOT
 * the npm release pin of the SDK.
 *
 * Release provenance (sdkClient, sdkProtocol, upstreamCommit) lives in
 * runtime/dsh-bridge/COMPATIBILITY.md and is enforced out-of-band by
 * the build, not by initialize.
 *
 * If upstream bumps the protocol identity, bump this constant in
 * lockstep with the upstream SDK release notes — do NOT use the npm
 * version number here.
 */
export const EXPECTED_SERVER_INFO = {
  name: 'deepseek-harness-sdk-runtime',
  version: '0.0.1',
} as const

/**
 * Validate the runtime-reported serverInfo against
 * EXPECTED_SERVER_INFO. Throws on:
 *   - wrong server name
 *   - wrong server protocol identity (version)
 *   - missing/empty/malformed serverInfo fields
 *
 * Per user decision (reviewer P1 #3, 2026-08-25): fail HARD on any
 * server protocol identity drift. No patch-level leniency, no
 * warning-only. (This validates the wire-stable initialize.serverInfo,
 * NOT the npm SDK release pin — see COMPATIBILITY.md for the two
 * version surfaces.)
 *
 * Callers should invoke this immediately after `client.initialize()`
 * returns, before the first /sessions request is served. The bridge
 * uses this in defaultRuntimeFactory; tests can call it directly.
 */
export function validateServerIdentity(
  serverInfo: { name: string; version: string },
): void {
  if (!serverInfo || typeof serverInfo !== 'object') {
    throw new Error(
      `bridge serverInfo missing or not an object: ${JSON.stringify(serverInfo)}. ` +
      `Refusing to bridge against a runtime that did not complete the initialize handshake.`,
    )
  }
  const { name, version } = serverInfo
  if (typeof name !== 'string' || name === '') {
    throw new Error(
      `bridge serverInfo.name is missing or empty: ${JSON.stringify(serverInfo)}. ` +
      `Refusing to bridge against a runtime without a stable identity.`,
    )
  }
  if (typeof version !== 'string' || version === '') {
    throw new Error(
      `bridge serverInfo.version is missing or empty: ${JSON.stringify(serverInfo)}. ` +
      `Refusing to bridge against a runtime without a stable protocol identity.`,
    )
  }
  if (name !== EXPECTED_SERVER_INFO.name) {
    throw new Error(
      `bridge serverInfo.name mismatch: expected ${EXPECTED_SERVER_INFO.name}, ` +
      `runtime reports ${name}. ` +
      `Refusing to bridge against an unknown runtime.`,
    )
  }
  if (version !== EXPECTED_SERVER_INFO.version) {
    throw new Error(
      `bridge serverInfo.version mismatch: expected ${EXPECTED_SERVER_INFO.version}, ` +
      `runtime reports ${version}. ` +
      `Refusing to bridge against an SDK/runtime protocol drift. ` +
      `If upstream intentionally bumped the protocol identity, update ` +
      `EXPECTED_SERVER_INFO in src/server.ts.`,
    )
  }
}

/**
 * The small surface the bridge actually drives. We use HarnessClient
 * directly (not the high-level DeepSeekHarness) so the bridge can
 * capture the wire-stable `InitializeResult.serverInfo` and surface it
 * honestly. Tests inject a fake via `runtimeFactory`.
 */
export interface BridgeRuntime {
  /** Run the spawn + initialize handshake. Called once at first session. */
  start(): Promise<void>
  /** Underlying JSON-RPC client. The bridge calls prompt / subscribe on it. */
  readonly client: HarnessClient
  /** Wire-stable server identity reported by the runtime's `initialize` reply. */
  readonly serverInfo: { name: string; version: string }
  /** Tear down the runtime process. Idempotent. */
  close(): Promise<void>
}

export interface BridgeOptions {
  /** Port to listen on. */
  port: number
  /** Host to bind. Default '127.0.0.1' — the bridge is local-only. */
  host?: string
  /** Runtime launch spec; required unless runtimeFactory is supplied. */
  launch?: LaunchSpec
  /** Optional extra HarnessClientOptions (timeouts, env override). */
  clientOptions?: Partial<HarnessClientOptions>
  /**
   * Optional factory for a fully-built BridgeRuntime. The default factory
   * spawns one HarnessClient from the LaunchSpec and reports the real
   * InitializeResult.serverInfo. Tests inject a fake here. Production
   * per-AgentRun ownership (4v1.11) will pass a per-call factory here.
   */
  runtimeFactory?: (req: CreateSessionRequest) => BridgeRuntime | Promise<BridgeRuntime>
  /** Optional error sink (used by the SSE pump). Defaults to stderr. */
  onError?: (err: unknown) => void
}

/** Per-session state stored on the bridge. */
interface SessionState {
  id: string
  cwd: string
  provider: string
  model: string
  runtime: BridgeRuntime
}

/** A live SSE subscriber (for the legacy /events firehose). */
interface Subscriber {
  id: string
  sessionId: string
  res: ServerResponse
  filter?: (n: HarnessNotification) => boolean
}

export class Bridge {
  private readonly opts: BridgeOptions
  private readonly host: string
  private runtime: BridgeRuntime | undefined
  private starting: Promise<BridgeRuntime> | undefined
  /**
   * The exact spec the runtime was initialized with. Per upstream
   * DSH semantics, cwd/provider/model/maxTokens pin the process for
   * its lifetime, so every subsequent /sessions must match. See
   * reviewer P1 #3 (route-mismatch): silently accepting a second
   * /sessions with different params would record the new params on
   * SessionState while the underlying runtime stayed on the first.
   */
  private initializedSpec: { cwd: string; provider: string; model: string; maxTokens?: number } | undefined
  private closing = false
  private readonly sessions = new Map<string, SessionState>()
  private readonly subscribers = new Map<string, Subscriber>()
  /** Active /run sessionIds — one Activity per session at a time. */
  private readonly activeRuns = new Set<string>()
  private server: ReturnType<typeof createServer> | undefined
  private readonly sessionParents = new Map<string, string>() // childSessionId -> parentSessionId

  constructor(opts: BridgeOptions) {
    this.opts = opts
    this.host = opts.host ?? '127.0.0.1'
  }

  /** Start listening. The runtime is owned and shut down on close(). */
  async listen(): Promise<void> {
    return new Promise((resolve, reject) => {
      const srv = createServer((req, res) => this.route(req, res).catch((err) => this.handleHttpError(res, err)))
      srv.on('error', reject)
      srv.listen(this.opts.port, this.host, () => {
        this.server = srv
        resolve()
      })
    })
  }

  /**
   * Resolve the LaunchSpec into HarnessClientOptions. Pure resolver —
   * the upstream HarnessClient owns the child process lifecycle and
   * spawns the dsh-jsonrpc-agent itself. The bridge never spawns
   * directly, so exactly one process is started per runtime.
   */
  private resolveLaunchOpts(req: CreateSessionRequest): HarnessClientOptions {
    if (!this.opts.launch) throw new Error('bridge launch spec missing')
    const resolved = resolveLaunch(this.opts.launch)
    return {
      command: resolved.command,
      args: resolved.args,
      ...(resolved.cwd ? { cwd: resolved.cwd } : {}),
      ...(resolved.env ? { env: resolved.env } : {}),
      ...(this.opts.clientOptions ?? {}),
    }
  }

  /**
   * Default runtime factory: spawn a HarnessClient and capture the
   * real serverInfo from the `initialize` handshake. The harness/client
   * is NOT created from a cordis cwd — the bridge just owns the process
   * and the runtime creates sessions on its first prompt.
   */
  private async defaultRuntimeFactory(req: CreateSessionRequest): Promise<BridgeRuntime> {
    const launchOpts = this.opts.launch
      ? this.resolveLaunchOpts(req)
      : {
        // Test-only path: caller supplied runtimeFactory but no LaunchSpec.
        command: '', args: [], cwd: req.cwd,
      } as HarnessClientOptions
    const client = new HarnessClient(launchOpts)
    let serverInfo: { name: string; version: string } = UNKNOWN_SERVER_INFO
    try {
      client.start()
      const init: InitializeResult = await client.initialize({
        cwd: resolvePath(req.cwd),
        provider: req.provider,
        model: req.model,
        ...(req.maxTokens !== undefined ? { maxTokens: req.maxTokens } : {}),
      })
      serverInfo = {
        name: init.serverInfo.name,
        version: init.serverInfo.version,
      }
      // Fail-hard on SDK/runtime version drift (reviewer P1 #3).
      validateServerIdentity(serverInfo)
    } catch (err) {
      // Tear down the (failed) client before propagating so we never
      // leak a child process.
      try { await client.close() } catch {}
      throw err
    }
    return {
      client,
      serverInfo,
      start: async () => { /* already started above */ },
      close: () => client.close(),
    }
  }

  /** Stop listening, close all subscribers, close the runtime. */
  async close(): Promise<void> {
    if (this.closing) return
    this.closing = true
    for (const sub of [...this.subscribers.values()]) {
      try { sub.res.end() } catch {}
    }
    this.subscribers.clear()
    if (this.server) {
      await new Promise<void>((resolve) => this.server!.close(() => resolve()))
    }
    if (this.runtime) {
      await this.runtime.close().catch(() => {})
      this.runtime = undefined
    }
    this.sessions.clear()
    this.activeRuns.clear()
  }

  /**
   * Compare a /sessions request to the runtime's initialized spec.
   * Returns null on match, or a stable mismatch description on diff.
   * Omitting fields counts as "unset" — maxTokens undefined in both
   * is a match. Used by handleCreateSession to fail fast with 409.
   */
  private routeDiff(req: CreateSessionRequest): string | null {
    const spec = this.initializedSpec
    if (!spec) return null
    const reqCwd = resolvePath(req.cwd)
    if (spec.cwd !== reqCwd) return `cwd: runtime=${spec.cwd} request=${reqCwd}`
    if (spec.provider !== req.provider) return `provider: runtime=${spec.provider} request=${req.provider}`
    if (spec.model !== req.model) return `model: runtime=${spec.model} request=${req.model}`
    const specMax = spec.maxTokens
    const reqMax = req.maxTokens
    if ((specMax ?? null) !== (reqMax ?? null)) {
      return `maxTokens: runtime=${specMax ?? 'unset'} request=${reqMax ?? 'unset'}`
    }
    return null
  }

  /** Lazy runtime start with initialize handshake. */
  private async ensureRuntime(req: CreateSessionRequest): Promise<BridgeRuntime> {
    if (this.runtime) return this.runtime
    if (this.starting) return this.starting

    const start = (async () => {
      const factory = this.opts.runtimeFactory ?? ((r) => this.defaultRuntimeFactory(r))
      const runtime = await factory(req)
      await runtime.start() // no-op for default factory (already initialized)
      // Pin the exact spec the runtime was initialized with. Per upstream
      // DSH semantics, cwd/provider/model/maxTokens are process-wide and
      // cannot be re-initialized; this is the only honest contract.
      this.initializedSpec = {
        cwd: resolvePath(req.cwd),
        provider: req.provider,
        model: req.model,
        ...(req.maxTokens !== undefined ? { maxTokens: req.maxTokens } : {}),
      }
      return runtime
    })()

    this.starting = start
    try {
      const runtime = await start
      this.runtime = runtime
      return runtime
    } finally {
      this.starting = undefined
    }
  }

  /** HTTP router. */
  private async route(req: IncomingMessage, res: ServerResponse): Promise<void> {
    if (req.method === 'GET' && req.url === '/health') {
      return this.handleHealth(res)
    }
    if (req.method === 'POST' && req.url === '/shutdown') {
      return this.handleShutdown(res)
    }
    if (req.method === 'POST' && req.url === '/sessions') {
      const body = await readJson<CreateSessionRequest>(req)
      return this.handleCreateSession(body, res)
    }
    const sessionMatch = /^\/sessions\/([^/]+)(\/prompt|\/events|\/run|\/close)?$/.exec(req.url ?? '')
    if (sessionMatch) {
      const [, sessionId, action] = sessionMatch
      if (req.method === 'POST' && action === '/prompt') {
        const body = await readJson<PromptRequest>(req)
        return this.handlePrompt(sessionId, body, res)
      }
      if (req.method === 'GET' && action === '/events') {
        return this.handleEvents(sessionId, req, res)
      }
      if (req.method === 'POST' && action === '/run') {
        const body = await readJson<RunRequest>(req)
        return this.handleRun(sessionId, body, req, res)
      }
      if (req.method === 'POST' && action === '/close') {
        return this.handleCloseSession(sessionId, res)
      }
    }
    sendJson(res, 404, { error: 'not_found', path: req.url })
  }

  private handleHealth(res: ServerResponse): void {
    let status: HealthResponse['status'] = 'starting'
    let serverInfo: HealthResponse['serverInfo'] | undefined
    let message: string | undefined
    if (this.closing) {
      status = 'closed'
    } else if (this.runtime) {
      status = 'ready'
      serverInfo = this.runtime.serverInfo
    } else if (this.starting) {
      status = 'starting'
    }
    sendJson(res, 200, { status, serverInfo, message } satisfies HealthResponse)
  }

  private async handleCreateSession(body: CreateSessionRequest, res: ServerResponse): Promise<void> {
    if (this.closing) { sendJson(res, 503, { error: 'closing' }); return }
    if (!body?.cwd || !body.provider || !body.model) {
      sendJson(res, 400, { error: 'missing fields: cwd, provider, model' }); return
    }
    // If a runtime is already initialized (or is initializing for a
    // different spec), reject the new /sessions up front. We cannot
    // ask the upstream to re-initialize — that would violate the
    // process-wide pin and silently leave the runtime on the old
    // spec while SessionState records the new one.
    if (this.initializedSpec) {
      const diff = this.routeDiff(body)
      if (diff) {
        sendJson(res, 409, {
          error: 'runtime_route_mismatch',
          detail: `DSH runtime is initialized with a different route; one bridge = one initialize. (${diff})`,
        })
        return
      }
    }
    const runtime = await this.ensureRuntime(body)
    const sessionId = `s-${randomUUID().replaceAll('-', '').slice(0, 16)}`
    this.sessions.set(sessionId, {
      id: sessionId,
      cwd: resolvePath(body.cwd),
      provider: body.provider,
      model: body.model,
      runtime,
    })
    const response: CreateSessionResponse = {
      sessionId,
      serverInfo: runtime.serverInfo,
    }
    sendJson(res, 200, response)
  }

  private async handlePrompt(sessionId: string, body: PromptRequest, res: ServerResponse): Promise<void> {
    const session = this.sessions.get(sessionId)
    if (!session) { sendJson(res, 404, { error: 'session_not_found' }); return }
    if (!body?.contentBlocks || !Array.isArray(body.contentBlocks)) {
      sendJson(res, 400, { error: 'missing contentBlocks[]' }); return
    }
    try {
      // client.prompt returns the durable enqueue receipt { messageId: ... };
      // unwrap so the wire carries just the messageId string.
      const receipt = await session.runtime.client.prompt(sessionId, body.contentBlocks as ContentBlock[])
      sendJson(res, 200, { messageId: receipt } satisfies PromptResponse)
    } catch (err) {
      sendJson(res, 502, { error: 'upstream_prompt_failed', detail: String(err) })
    }
  }

  /**
   * POST /sessions/:id/run — owned Activity interval.
   *
   * Mirrors upstream `HarnessSession.run`:
   *   1. Subscribe FIRST.
   *   2. `prompt(...)` → messageId (durable enqueue receipt).
   *   3. Wait for `agent/inbox/spliced(sessionId, messageId)` — the
   *      receipt that the message was spliced into the agent's inbox.
   *      Skip events before that (they are noise from a prior turn).
   *   4. Forward notifications on SSE until root
   *      `session.status === idle`, then emit `run.end` and close.
   *
   * Wire shape: each SSE frame is `data: <JSON>` followed by `\n\n`.
   * Frame JSON is a BridgeEvent (see types.ts): upstream SessionEvents
   * are flattened to `{sessionId, type, data?}` where `type` is the
   * upstream `event.type`; subagent.* and session.status keep their
   * upstream notification-method name as `type`; lifecycle frames
   * `run.start` / `run.end` bracket the activity.
   *
   * The bridge does NOT rely on EOF or on a fabricated terminal
   * event. The activity interval's natural close is
   * `session.status = idle` for the root session.
   *
   * On transport failure the bridge emits `bridge.transport_error`
   * followed by `run.end{reason: transport_error}` so the consumer
   * always sees a closed stream with one terminal frame.
   */
  private async handleRun(sessionId: string, body: RunRequest, req: IncomingMessage, res: ServerResponse): Promise<void> {
        const session = this.sessions.get(sessionId)
    if (!session) { sendJson(res, 404, { error: 'session_not_found' }); return }
    if (!body?.contentBlocks || !Array.isArray(body.contentBlocks)) {
      sendJson(res, 400, { error: 'missing contentBlocks[]' }); return
    }
if (this.activeRuns.has(sessionId)) {
      sendJson(res, 409, { error: 'run_in_progress', detail: 'one Activity interval per session; close the previous run before starting a new one' })
      return
    }
    // Mark the session active BEFORE writing the response headers so a
    // concurrent second /run sees the in-progress state and 409s
    // instead of racing past the guard.
    this.activeRuns.add(sessionId)
    res.writeHead(200, {
      'content-type': 'text/event-stream',
      'cache-control': 'no-cache',
      'connection': 'close',
    })

    // Subscribe FIRST. The subscription must be alive before we issue
    // `prompt(...)`, otherwise an upstream `agent/inbox/spliced`
    // receipt for the new message could fire before we observe it.
    const subscription = session.runtime.client.subscribeSessionTree(sessionId)
    let transportErr: unknown
    let aborted = false
    let terminated = false
    let splicedMessageId: string | undefined
    let reachedIdle = false
    let messageId: string | undefined

    const finish = (reason: 'idle' | 'transport_error'): void => {
      if (terminated) return
      terminated = true
      this.activeRuns.delete(sessionId)
      if (reason === 'transport_error') {
        const msg = (transportErr instanceof Error ? transportErr.message : String(transportErr)) || 'subscription_pump_error'
        try { writeSse(res, { type: 'bridge.transport_error', message: msg }) } catch { /* res may be torn down */ }
      }
      try { writeSse(res, { type: 'run.end', reason }) } catch { /* ignore */ }
      try { subscription.close() } catch {}
      try { res.end() } catch {}
    }

    const isSplicedReceipt = (ev: { type: string; data?: unknown }): boolean => {
      if (ev.type !== 'agent/inbox/spliced') return false
      if (!messageId) return false
      const data = ev.data as { inserted?: Array<{ id?: string }> } | undefined
      if (!data?.inserted) return false
      for (const m of data.inserted) {
        if (m?.id === messageId) return true
      }
      return false
    }

    const pump = async (): Promise<void> => {
      try {
        for await (const notification of subscription) {
          if (aborted || terminated) return
          const ev = toBridgeEvent(notification)
          if (ev === null) continue
          if (ev.type === 'subagent.started') {
            this.sessionParents.set(ev.childSessionId, ev.parentSessionId)
          }
          // Receipt correlation: until we observe agent/inbox/spliced
          // carrying the messageId from THIS prompt, any session.status
          // or other activity events belong to a prior turn and must NOT
          // close this Activity. The receipt buffer handles events
          // before prompt() resolves; live events are dropped until the
          // spliced receipt is observed.
          if (!splicedMessageId) {
            if (isSplicedReceipt(ev as { type: string; data?: unknown })) {
              splicedMessageId = messageId
              writeSse(res, ev)
            }
            // else: drop prior-turn noise (events that arrived between
            // subscribeSessionTree and the matching receipt).
          } else {
            writeSse(res, ev)
          }
          // session.status=idle on the ROOT session closes the Activity.
          // Subagent descendants may also go idle independently — that
          // is not the activity close for the root session.
          if (
            ev.type === 'session.status' &&
            ev.sessionId === sessionId &&
            ev.status === 'idle'
          ) {
            reachedIdle = true
            finish('idle')
            return
          }
        }
      } catch (err) {
        if (!aborted) transportErr = err
      }
      // Subscription closed. The ONLY natural close is root
      // session.status=idle; any other close (EOF, transport tear-down,
      // missed idle frame) is a transport_error. Reaching this branch
      // with `reachedIdle === false` is the failure mode the Runner
      // detects via the missing run.end{reason=idle}.
      if (!terminated) {
        if (reachedIdle) {
          finish('idle')
        } else {
          transportErr = transportErr ?? new Error('subscription_closed_before_root_idle')
          finish('transport_error')
        }
      }
    }

    void pump()

    req.on('close', () => {
      aborted = true
      try { subscription.close() } catch {}
      if (!terminated) {
        // Client disconnected before idle. Emit a terminal frame so the
        // server side stays consistent, then close.
        transportErr = transportErr ?? new Error('client_disconnected_before_root_idle')
        finish('transport_error')
      }
    })

    // Issue the prompt AFTER the subscription is wired up. Failure here
    // closes the stream with a synthetic transport_error so the
    // consumer sees one terminal frame.
    try {
      messageId = await session.runtime.client.prompt(sessionId, body.contentBlocks as ContentBlock[])
      if (aborted) return
      writeSse(res, { type: 'run.start', messageId })
    } catch (err) {
      if (!aborted) {
        transportErr = err
        finish('transport_error')
      }
    }
  }

  private handleCloseSession(sessionId: string, res: ServerResponse): void {
    // Upstream SDK has NO per-session close. Only the runtime process close
    // exists. We do not emulate one — surface the gap honestly.
    sendJson(res, 501, {
      error: 'not_supported',
      detail: 'DSH upstream has no per-session close protocol. To end work on a session, abandon it (the runtime stays owned and reusable). To stop the entire runtime, POST /shutdown.',
      sessionId,
    })
  }

  /**
   * GET /sessions/:id/events — firehose of upstream notifications on SSE.
   *
   * This endpoint is kept for tests and tooling that need to inspect
   * the raw notification stream. Production callers SHOULD use
   * `POST /sessions/:id/run` instead — it owns the activity lifecycle
   * (subscribe-before-prompt, await inbox/spliced, idle-bound) and
   * closes on `session.status = idle` rather than on EOF.
   *
   * Frames are BridgeEvents with upstream-faithful `type` discriminators
   * (see types.ts). The stream ends when the subscription closes (clean
   * transport close) or after a `bridge.transport_error` frame on a
   * pump failure.
   */
  private handleEvents(sessionId: string, req: IncomingMessage, res: ServerResponse): void {
    const session = this.sessions.get(sessionId)
    if (!session) { sendJson(res, 404, { error: 'session_not_found' }); return }
    const subId = randomUUID()
    res.writeHead(200, {
      'content-type': 'text/event-stream',
      'cache-control': 'no-cache',
      'connection': 'close',
    })
    const subscription = session.runtime.client.subscribeSessionTree(sessionId)
    const sub: Subscriber = { id: subId, sessionId, res }
    this.subscribers.set(subId, sub)

    const pump = async (): Promise<void> => {
      let transportErr: unknown
      try {
        for await (const notification of subscription) {
          if (!this.subscribers.has(subId)) break
          const ev = toBridgeEvent(notification)
          if (ev === null) continue
          // Track parent-child lineage so subagent events can be re-attributed.
          if (ev.type === 'subagent.started') {
            this.sessionParents.set(ev.childSessionId, ev.parentSessionId)
          }
          writeSse(res, ev)
        }
      } catch (err) {
        // A clean EOF (subscription closed without error) is NOT a
        // transport failure and must not produce a transport_error
        // frame. Any thrown error here means the upstream runtime or
        // the SSE pipe itself broke: capture it so the finally block
        // can emit an explicit bridge.transport_error control frame.
        transportErr = err
      } finally {
        if (transportErr !== undefined) {
          // Surface the failure as an explicit control frame BEFORE
          // closing the stream. A plain EOF is not enough: Go's
          // bufio.Scanner.Err() reports nil for a clean close, so the
          // Runner would just hang waiting for an event that never
          // arrives. The Go side routes this frame to a typed
          // transport-error channel instead of Normalize().
          try {
            const msg = (transportErr instanceof Error ? transportErr.message : String(transportErr)) || 'subscription_pump_error'
            writeSse(res, { type: 'bridge.transport_error', message: msg })
          } catch { /* res may already be torn down */ }
        }
        subscription.close()
        this.subscribers.delete(subId)
        try { res.end() } catch {}
      }
    }
    void pump()

    req.on('close', () => {
      subscription.close()
      this.subscribers.delete(subId)
      try { res.end() } catch {}
    })
  }

  private handleShutdown(res: ServerResponse): void {
    // Write the response BEFORE closing the server, otherwise
    // server.close() will block waiting for this active connection.
    res.writeHead(204, { 'content-type': 'application/json' })
    res.end()
    void this.close()
  }

  private handleHttpError(res: ServerResponse, err: unknown): void {
    if (res.headersSent) return
    sendJson(res, 500, { error: 'bridge_error', detail: String(err) })
  }
}

/** Write one SSE frame: `data: <json>\n\n`. */
function writeSse(res: ServerResponse, ev: BridgeEvent): void {
  res.write(`data: ${JSON.stringify(ev)}\n\n`)
}

/** Read JSON body up to 1 MiB. */
async function readJson<T>(req: IncomingMessage): Promise<T> {
  return await new Promise<T>((resolve, reject) => {
    const chunks: Buffer[] = []
    let total = 0
    req.on('data', (chunk: Buffer) => {
      total += chunk.length
      if (total > 1024 * 1024) { reject(new Error('body too large')); req.destroy(); return }
      chunks.push(chunk)
    })
    req.on('end', () => {
      const buf = Buffer.concat(chunks).toString('utf8')
      if (!buf) { resolve({} as T); return }
      try { resolve(JSON.parse(buf) as T) } catch (err) { reject(err) }
    })
    req.on('error', reject)
  })
}

function sendJson(res: ServerResponse, status: number, body: unknown): void {
  res.writeHead(status, { 'content-type': 'application/json' })
  res.end(JSON.stringify(body))
}

/** Map one upstream notification to a BridgeEvent. Exported for unit tests. */
export function toBridgeEvent(n: HarnessNotification): BridgeEvent | null {
  const params = n.params as Record<string, unknown>
  switch (n.method) {
    case 'session.event': {
      const sessionId = params.sessionId as string
      const event = params.event
      if (!event || typeof event !== 'object') return null
      const ev = event as Record<string, unknown>
      const type = ev.type
      if (!isSessionEventType(type)) {
        // Unknown upstream SessionEvent.type — refuse to forward
        // fabricated shapes. Surface as null; the consumer should
        // not see made-up kinds.
        return null
      }
      const data = ev.data
      return data === undefined
        ? { sessionId, type }
        : { sessionId, type, data }
    }
    case 'session.status': {
      const sessionId = params.sessionId as string
      const status = params.status as 'idle' | 'running'
      return { sessionId, type: 'session.status', status }
    }
    case 'subagent.started': {
      return {
        type: 'subagent.started',
        parentSessionId: params.parentSessionId as string,
        childSessionId: params.childSessionId as string,
      }
    }
    case 'subagent.finished': {
      return {
        type: 'subagent.finished',
        provider: params.provider as string,
        agentId: params.agentId as string,
        parentSessionId: params.parentSessionId as string,
        childSessionId: params.childSessionId as string,
        status: params.status as 'ok' | 'error',
        stopReason: params.stopReason as SubagentStopReason,
        ...(params.lastAssistantMessage !== undefined ? { lastAssistantMessage: params.lastAssistantMessage as ContentBlock[] } : {}),
      }
    }
    default:
      // Unknown upstream notification method — surface as null; the
      // bridge does not invent event kinds. Consumers may inspect the
      // raw stream if they need unmodeled upstream data.
      return null
  }
}

/** CLI entry: `node lib/server.js --port 7777 --cordis ...`. */
async function main(): Promise<void> {
  const args = process.argv.slice(2)
  let port = 7777
  let host = '127.0.0.1'
  let cordisConfig = process.env.DSH_CORDIS_CONFIG ?? ''
  let mode: 'exe' | 'node' | undefined
  for (let i = 0; i < args.length; i += 2) {
    const k = args[i]
    const v = args[i + 1]
    if (k === '--port') port = Number(v)
    else if (k === '--host') host = v
    else if (k === '--cordis') cordisConfig = v
    else if (k === '--mode') mode = v === 'node' ? 'node' : 'exe'
  }
  if (!cordisConfig) {
    process.stderr.write('ERROR: --cordis <path-to-cordis.yml> (or DSH_CORDIS_CONFIG env) is required\n')
    process.exit(2)
  }
  const bridge = new Bridge({
    port,
    host,
    launch: { mode, cordisConfig },
  })
  await bridge.listen()
  process.stdout.write(`dsh-bridge listening on http://${host}:${port}\n`)
  process.on('SIGINT', () => { void bridge.close().then(() => process.exit(0)) })
  process.on('SIGTERM', () => { void bridge.close().then(() => process.exit(0)) })
}

const isMain = import.meta.url === `file://${process.argv[1]}`
if (isMain) { void main() }
