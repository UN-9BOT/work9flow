/**
 * The dsh-bridge HTTP server.
 *
 * Owns ONE dsh-jsonrpc-agent subprocess per process (per upstream
 * DeepSeekHarness semantics: one initialize pins cwd/provider/model for
 * the runtime's lifetime). Multiple sessions can share the same runtime;
 * each session gets its own `HarnessSession.handle` and its own
 * subscription via `client.subscribeSessionTree(id)`.
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
 */
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http'
import { randomUUID } from 'node:crypto'
import { resolve as resolvePath } from 'node:path'
import {
  DeepSeekHarness,
  HarnessClient,
  type ContentBlock,
  type DeepSeekHarnessOptions,
  type HarnessClientOptions,
  type HarnessNotification,
} from '@deepseek-ai/dsh-sdk-client'
import { resolveLaunch, type LaunchSpec } from './launcher.js'
import type {
  BridgeEvent,
  CreateSessionRequest,
  CreateSessionResponse,
  HealthResponse,
  PromptRequest,
  PromptResponse,
  SubagentStopReason,
} from './types.js'

export interface BridgeOptions {
  /** Port to listen on. */
  port: number
  /** Host to bind. Default '127.0.0.1' — the bridge is local-only. */
  host?: string
  /** Runtime launch spec; required unless harnessFactory is supplied. */
  launch?: LaunchSpec
  /** Optional extra HarnessClientOptions (timeouts, env override). */
  clientOptions?: Partial<HarnessClientOptions>
  /** Optional explicit DeepSeekHarnessOptions factory (overrides launch). */
  harnessOptions?: (opts: DeepSeekHarnessOptions) => DeepSeekHarnessOptions
  /**
   * Optional factory for the DeepSeekHarness instance. Defaults to spawning
   * one via the real SDK + LaunchSpec. Tests inject a fake. Production
   * per-AgentRun ownership (4v1.11) will pass a per-call factory here.
   */
  harnessFactory?: (opts: DeepSeekHarnessOptions) => DeepSeekHarness
}

/** Per-session state stored on the bridge. */
interface SessionState {
  id: string
  cwd: string
  provider: string
  model: string
  harness: HarnessSessionHandle
}

/** Thin wrapper around DeepSeekHarness + an attached session id. */
class HarnessSessionHandle {
  readonly harness: DeepSeekHarness
  constructor(harness: DeepSeekHarness) { this.harness = harness }
}

/** A live SSE subscriber. */
interface Subscriber {
  id: string
  sessionId: string
  res: ServerResponse
  filter?: (n: HarnessNotification) => boolean
}

export class Bridge {
  private readonly opts: BridgeOptions
  private readonly host: string
  private harness: DeepSeekHarness | undefined
  private starting: Promise<void> | undefined
  private closing = false
  private readonly sessions = new Map<string, SessionState>()
  private readonly subscribers = new Map<string, Subscriber>()
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
  private resolveLaunchOpts(): HarnessClientOptions {
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
    if (this.harness) {
      await this.harness.close().catch(() => {})
      this.harness = undefined
    }
    this.sessions.clear()
  }

  /** Lazy runtime start with initialize handshake. */
  private async ensureHarness(req: CreateSessionRequest): Promise<DeepSeekHarness> {
    if (this.harness) return this.harness
    if (this.starting) { await this.starting; return this.harness! }

    const start = (async () => {
      const launchOpts = this.opts.launch ? this.resolveLaunchOpts() : {
        // Test-only path: caller supplied harnessFactory but no LaunchSpec.
        command: '', args: [], cwd: req.cwd,
      } as HarnessClientOptions
      const harnessOpts: DeepSeekHarnessOptions = {
        launch: launchOpts,
        cwd: resolvePath(req.cwd),
        provider: req.provider,
        model: req.model,
        ...(req.maxTokens !== undefined ? { maxTokens: req.maxTokens } : {}),
      }
      const finalOpts = this.opts.harnessOptions
        ? this.opts.harnessOptions(harnessOpts)
        : harnessOpts
      const harness = this.opts.harnessFactory
        ? this.opts.harnessFactory(finalOpts)
        : new DeepSeekHarness(finalOpts)
      await harness.start() // initialize handshake (no-op for test fakes)
      this.harness = harness
    })()

    this.starting = start
    try {
      await start
    } finally {
      this.starting = undefined
    }
    return this.harness!
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
    const sessionMatch = /^\/sessions\/([^/]+)(\/prompt|\/events|\/close)?$/.exec(req.url ?? '')
    if (sessionMatch) {
      const [, sessionId, action] = sessionMatch
      if (req.method === 'POST' && action === '/prompt') {
        const body = await readJson<PromptRequest>(req)
        return this.handlePrompt(sessionId, body, res)
      }
      if (req.method === 'GET' && action === '/events') {
        return this.handleEvents(sessionId, req, res)
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
    } else if (this.harness) {
      status = 'ready'
      const server = this.harness.client as HarnessClient
      // serverInfo is set after initialize; the SDK doesn't expose it post-hoc,
      // so we cache the wire-stable name on first handshake. Until we have
      // it cached we still report 'ready' once the harness exists.
      serverInfo = (server as unknown as { _serverInfo?: { name: string; version: string } })._serverInfo
        ?? { name: 'deepseek-harness-sdk-runtime', version: '0.0.1' }
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
    const harness = await this.ensureHarness(body)
    const sessionId = `s-${randomUUID().replaceAll('-', '').slice(0, 16)}`
    const handle = new HarnessSessionHandle(harness)
    this.sessions.set(sessionId, {
      id: sessionId,
      cwd: resolvePath(body.cwd),
      provider: body.provider,
      model: body.model,
      harness: handle,
    })
    const response: CreateSessionResponse = {
      sessionId,
      serverInfo: { name: 'deepseek-harness-sdk-runtime', version: '0.0.1' },
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
      const receipt = await session.harness.harness.client.prompt(sessionId, body.contentBlocks as ContentBlock[])
      sendJson(res, 200, { messageId: receipt } satisfies PromptResponse)
    } catch (err) {
      sendJson(res, 502, { error: 'upstream_prompt_failed', detail: String(err) })
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

  private handleEvents(sessionId: string, req: IncomingMessage, res: ServerResponse): void {
    const session = this.sessions.get(sessionId)
    if (!session) { sendJson(res, 404, { error: 'session_not_found' }); return }
    const subId = randomUUID()
    res.writeHead(200, {
      'content-type': 'text/event-stream',
      'cache-control': 'no-cache',
      'connection': 'keep-alive',
    })
    const subscription = session.harness.harness.client.subscribeSessionTree(sessionId)
    const sub: Subscriber = { id: subId, sessionId, res }
    this.subscribers.set(subId, sub)

    const pump = async (): Promise<void> => {
      try {
        for await (const notification of subscription) {
          if (!this.subscribers.has(subId)) break
          const ev = toBridgeEvent(notification)
          if (ev === null) continue
          // Track parent-child lineage so subagent events can be re-attributed.
          if (ev.kind === 'subagent.started') {
            this.sessionParents.set(ev.childSessionId, ev.parentSessionId)
          }
          res.write(`data: ${JSON.stringify(ev)}\n\n`)
        }
      } catch (err) {
        // Pump failure is a transport-level error, NOT a domain event.
        // Skip the spurious `bridge.error` SSE frame (it would land as
        // raw.passthrough on the Go side and the Runner would keep
        // polling instead of surfacing the failure). Just close the
        // stream; Go's scanner.Err() will report the original error
        // through errCh.
        if (this.subscribers.has(subId)) {
          this.subscribers.delete(subId)
        }
        // Best-effort log; in production this goes to stderr logger.
        try { (this.opts as { onError?: (e: unknown) => void }).onError?.(err) } catch {}
      } finally {
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
      const event = params.event as Record<string, unknown>
      return { kind: 'session.event', sessionId, event }
    }
    case 'session.status': {
      const sessionId = params.sessionId as string
      const status = params.status as 'idle' | 'running'
      return { kind: 'session.status', sessionId, status }
    }
    case 'subagent.started': {
      return {
        kind: 'subagent.started',
        parentSessionId: params.parentSessionId as string,
        childSessionId: params.childSessionId as string,
      }
    }
    case 'subagent.finished': {
      return {
        kind: 'subagent.finished',
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
      // Unknown upstream method — drop with bridge.error so consumers see it.
      return { kind: 'bridge.error', message: `unknown notification method: ${n.method}` }
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
