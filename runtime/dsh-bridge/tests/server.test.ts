/**
 * Bridge HTTP layer tests using a fake BridgeRuntime.
 *
 * The fake implements the small surface our bridge actually calls:
 *   - start(): Promise<void>
 *   - close(): Promise<void>
 *   - client.prompt(sessionId, blocks): Promise<{messageId}>
 *   - client.subscribeSessionTree(id): async iterable of notifications
 *   - serverInfo: { name, version } — exposed honestly on /health and /sessions
 *
 * No real DSH runtime required.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'

import {
  Bridge,
  toBridgeEvent,
} from '../src/server.ts'
import type { BridgeEvent } from '../src/types.ts'
import type {
  ContentBlock,
  HarnessClient,
  HarnessNotification,
  NotificationSubscription,
} from '@deepseek-ai/dsh-sdk-client'
import type { BridgeRuntime } from '../src/server.ts'

interface FakeSubscription extends NotificationSubscription {
  push(n: HarnessNotification): void
  close(): void
  fail(err: Error): void
}

function makeFakeSubscription(): FakeSubscription {
  const queue: HarnessNotification[] = []
  const waiters: Array<{ resolve: (n: HarnessNotification) => void; reject: (e: Error) => void }> = []
  let closed = false
  let failure: Error | undefined
  const sub: FakeSubscription = {
    [Symbol.asyncIterator](): AsyncIterator<HarnessNotification> {
      const it: AsyncIterator<HarnessNotification> = {
        next: () => sub.next().then((value) => ({ value, done: false })),
      }
      return it
    },
    async next(): Promise<HarnessNotification> {
      if (closed) throw failure ?? new Error('subscription closed')
      const queued = queue.shift()
      if (queued !== undefined) return queued
      if (failure !== undefined) throw failure
      return new Promise<HarnessNotification>((resolve, reject) => {
        waiters.push({ resolve, reject })
      })
    },
    tryNext(): HarnessNotification | undefined { return queue.shift() },
    push(n: HarnessNotification) {
      if (closed) return
      const w = waiters.shift()
      if (w) w.resolve(n); else queue.push(n)
    },
    close() {
      closed = true
      queue.length = 0
      for (const w of waiters.splice(0)) w.reject(new Error('subscription closed'))
    },
    fail(err: Error) {
      failure = err
      for (const w of waiters.splice(0)) w.reject(err)
    },
  }
  return sub
}

function makeFakeRuntime(opts: {
  onPrompt?: (id: string, blocks: ContentBlock[]) => string
  onSubscribe?: (id: string) => FakeSubscription
  startShouldThrow?: boolean
  promptShouldThrow?: Error
  serverInfo?: { name: string; version: string }
}): BridgeRuntime {
  const subscriptions = new Map<string, FakeSubscription>()
  let messageCounter = 0
  const client = {
    prompt: async (sessionId: string, blocks: ContentBlock[]) => {
      if (opts.promptShouldThrow) throw opts.promptShouldThrow
      // Mirrors upstream signature Promise<string> (messageId receipt).
      const id = opts.onPrompt ? opts.onPrompt(sessionId, blocks) : `m-${++messageCounter}`
      return id
    },
    subscribeSessionTree: (sessionId: string): NotificationSubscription => {
      let sub = subscriptions.get(sessionId)
      if (!sub) {
        sub = opts.onSubscribe ? opts.onSubscribe(sessionId) : makeFakeSubscription()
        subscriptions.set(sessionId, sub)
      }
      return sub
    },
  } as unknown as HarnessClient

  return {
    client,
    serverInfo: opts.serverInfo ?? { name: 'unknown', version: 'unknown' },
    start: async () => {
      if (opts.startShouldThrow) throw new Error('handshake_failed')
    },
    close: async () => {},
  }
}

async function jsonPost(url: string, body: unknown): Promise<{ status: number; json: any }> {
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  })
  let json: any = null
  try { json = await res.json() } catch { /* empty */ }
  return { status: res.status, json }
}

/**
 * Read an SSE stream into BridgeEvent frames, stopping at the first
 * stream end (server-side close). Honours an external AbortSignal.
 */
async function readSSE(url: string, signal: AbortSignal, opts?: { method?: string; body?: unknown }): Promise<BridgeEvent[]> {
  const method = opts?.method ?? 'GET'
  const init: RequestInit = { method, signal }
  if (method !== 'GET' && opts?.body !== undefined) {
    init.headers = { 'content-type': 'application/json' }
    init.body = JSON.stringify(opts.body)
  }
  const res = await fetch(url, init)
  if (res.status !== 200) throw new Error(`expected 200, got ${res.status}`)
  const reader = res.body!.getReader()
  const dec = new TextDecoder()
  let buf = ''
  const out: BridgeEvent[] = []
  const killer = new AbortController()
  signal.addEventListener('abort', () => killer.abort())
  try {
    while (true) {
      const { value, done } = await reader.read()
      if (done) break
      buf += dec.decode(value, { stream: true })
      let nl: number
      while ((nl = buf.indexOf('\n\n')) !== -1) {
        const chunk = buf.slice(0, nl)
        buf = buf.slice(nl + 2)
        for (const line of chunk.split('\n')) {
          if (line.startsWith('data: ')) {
            out.push(JSON.parse(line.slice(6)))
          }
        }
      }
    }
  } catch (err) {
    if ((err as Error).name !== 'AbortError') throw err
  } finally {
    killer.abort()
    try { reader.releaseLock() } catch {}
  }
  return out
}

test('POST /sessions returns a sessionId and persists state', async () => {
  const runtime = makeFakeRuntime({})
  const bridge = new Bridge({
    port: 0,
    runtimeFactory: () => runtime,
  })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`

  const r = await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })
  assert.equal(r.status, 200)
  assert.ok(typeof r.json.sessionId === 'string' && r.json.sessionId.length > 0)
  await bridge.close()
})

test('POST /sessions 400 on missing fields', async () => {
  const runtime = makeFakeRuntime({})
  const bridge = new Bridge({ port: 0, runtimeFactory: () => runtime })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`

  const r1 = await jsonPost(`${base}/sessions`, { cwd: '/tmp' })
  assert.equal(r1.status, 400)
  const r2 = await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p' })
  assert.equal(r2.status, 400)
  const r3 = await jsonPost(`${base}/sessions`, { cwd: '/tmp', model: 'm' })
  assert.equal(r3.status, 400)
  await bridge.close()
})

test('POST /sessions 409 on different route (initialized runtime is process-wide pinned)', async () => {
  let promptCount = 0
  const runtime = makeFakeRuntime({
    onPrompt: () => { promptCount++; return 'm-1' },
  })
  const bridge = new Bridge({ port: 0, runtimeFactory: () => runtime })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`

  const r1 = await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm1' })
  assert.equal(r1.status, 200)

  const r2 = await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm2' })
  assert.equal(r2.status, 409, 'second /sessions with different model must 409')
  assert.equal(r2.json.error, 'runtime_route_mismatch')
  await bridge.close()
})

test('POST /sessions/:id/prompt returns messageId from upstream', async () => {
  const runtime = makeFakeRuntime({
    onPrompt: (_id, blocks) => `m-for-${blocks.length}`,
  })
  const bridge = new Bridge({ port: 0, runtimeFactory: () => runtime })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`
  const created = await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })
  const sid = created.json.sessionId as string
  const r = await jsonPost(`${base}/sessions/${sid}/prompt`, {
    contentBlocks: [{ type: 'text', text: 'hi' }],
  })
  assert.equal(r.status, 200)
  assert.equal(r.json.messageId, 'm-for-1')
  await bridge.close()
})

test('POST /sessions/:id/prompt 404 on unknown session', async () => {
  const runtime = makeFakeRuntime({})
  const bridge = new Bridge({ port: 0, runtimeFactory: () => runtime })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`

  const r = await jsonPost(`${base}/sessions/s-bogus/prompt`, {
    contentBlocks: [{ type: 'text', text: 'hi' }],
  })
  assert.equal(r.status, 404)
  await bridge.close()
})

test('POST /sessions/:id/close returns 501 (upstream has no per-session close)', async () => {
  const runtime = makeFakeRuntime({})
  const bridge = new Bridge({ port: 0, runtimeFactory: () => runtime })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`
  const created = await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })
  const sid = created.json.sessionId as string
  const r = await fetch(`${base}/sessions/${sid}/close`, { method: 'POST' })
  assert.equal(r.status, 501)
  const body = await r.json() as any
  assert.equal(body.error, 'not_supported')
  await bridge.close()
})

test('GET /sessions/:id/events streams upstream notifications as SSE (upstream `type` discriminator)', async () => {
  // Verifies the new wire shape: `kind` is gone; each SessionEvent is
  // a flattened {sessionId, type, data?} with `type` drawn from the
  // upstream catalog (assistant/message, turn/end, ...). Subagent
  // notifications keep their notification-method name as `type`.
  const sub = makeFakeSubscription()
  const runtime = makeFakeRuntime({
    onSubscribe: () => sub,
  })
  const bridge = new Bridge({ port: 0, runtimeFactory: () => runtime })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`
  const created = await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })
  const sid = created.json.sessionId as string

  const controller = new AbortController()
  const readerPromise = readSSE(`${base}/sessions/${sid}/events`, controller.signal)

  // Allow the subscription pump to start before we push notifications.
  await new Promise((r) => setTimeout(r, 25))
  sub.push({ method: 'session.event', params: { sessionId: sid, event: { type: 'turn/end', data: { reason: 'end_turn' } } } })
  sub.push({ method: 'session.status', params: { sessionId: sid, status: 'idle' } })
  sub.push({ method: 'subagent.started', params: { parentSessionId: sid, childSessionId: 'c-1' } })
  await new Promise((r) => setTimeout(r, 25))
  controller.abort()

  const out = await readerPromise
  const event = out.find((e) => (e as any).type === 'turn/end') as any
  const status = out.find((e) => (e as any).type === 'session.status') as any
  const started = out.find((e) => (e as any).type === 'subagent.started') as any
  assert.ok(event, 'expected upstream turn/end SSE frame')
  assert.equal(event.sessionId, sid)
  assert.equal(event.type, 'turn/end')
  assert.deepEqual(event.data, { reason: 'end_turn' })

  assert.ok(status)
  assert.equal(status.status, 'idle')

  assert.ok(started)
  assert.equal(started.childSessionId, 'c-1')
  await bridge.close()
})

test('GET /sessions/:id/events emits bridge.transport_error when the subscription pump errors', async () => {
  // Reviewer P1 #2 (fhn): a plain HTTP EOF after the upstream pump
  // catches an error is not enough — bufio.Scanner.Err() reports nil
  // for a clean close, so the Runner would hang. The bridge emits an
  // explicit bridge.transport_error control frame before closing.
  const sub = makeFakeSubscription()
  const runtime = makeFakeRuntime({
    onSubscribe: () => sub,
  })
  const bridge = new Bridge({ port: 0, runtimeFactory: () => runtime })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`
  const created = await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })
  const sid = created.json.sessionId as string

  const controller = new AbortController()
  const readerPromise = readSSE(`${base}/sessions/${sid}/events`, controller.signal)

  await new Promise((r) => setTimeout(r, 25))
  sub.fail(new Error('upstream_runtime_disconnected'))
  await new Promise((r) => setTimeout(r, 25))
  controller.abort()

  const out = await readerPromise
  const transport = out.find((e) => (e as any).type === 'bridge.transport_error') as any
  assert.ok(transport, 'expected bridge.transport_error frame')
  assert.equal(transport.message, 'upstream_runtime_disconnected')
  await bridge.close()
})

test('GET /health reflects lifecycle', async () => {
  const runtime = makeFakeRuntime({})
  const bridge = new Bridge({ port: 0, runtimeFactory: () => runtime })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`

  // Runtime is initialised lazily on first /sessions. Before that,
  // /health reports `starting` and omits serverInfo.
  const h0 = await (await fetch(`${base}/health`)).json() as any
  assert.equal(h0.status, 'starting')

  // After /sessions, the bridge has run the handshake and reports `ready`.
  await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })
  const h1 = await (await fetch(`${base}/health`)).json() as any
  assert.equal(h1.status, 'ready')
  await bridge.close()
})

test('GET /health and POST /sessions surface the runtime-reported serverInfo (not fabricated)', async () => {
  const wire = { name: 'deepseek-harness-sdk-runtime', version: '0.4.2-rc.7' }
  const bridge = new Bridge({
    port: 0,
    runtimeFactory: () => makeFakeRuntime({ serverInfo: wire }),
  })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`

  const h0 = await (await fetch(`${base}/health`)).json() as any
  assert.equal(h0.status, 'starting')
  assert.equal(h0.serverInfo, undefined, 'must not fabricate serverInfo when runtime not ready')

  const r = await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })
  assert.equal(r.status, 200)
  assert.deepEqual(r.json.serverInfo, wire, 'sessions must report runtime-supplied serverInfo')

  const h = await (await fetch(`${base}/health`)).json() as any
  assert.equal(h.status, 'ready')
  assert.deepEqual(h.serverInfo, wire, 'health must report runtime-supplied serverInfo')

  await bridge.close()
})

test('GET /health without serverInfo when runtime has not initialised (status: starting)', async () => {
  const bridge = new Bridge({
    port: 0,
    runtimeFactory: () => makeFakeRuntime({ startShouldThrow: true }),
  })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`

  const h = await (await fetch(`${base}/health`)).json() as any
  assert.equal(h.status, 'starting')
  assert.equal(h.serverInfo, undefined, 'must not fabricate serverInfo when runtime not ready')

  await bridge.close()
})

test('POST /shutdown returns 204', async () => {
  let startCalled = false
  const runtime = makeFakeRuntime({})
  ;(runtime as any).start = async () => { startCalled = true }
  const bridge = new Bridge({ port: 0, runtimeFactory: () => runtime })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`
  const created = await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })
  assert.equal(created.status, 200)
  assert.ok(startCalled)
  const res = await fetch(`${base}/shutdown`, { method: 'POST' })
  assert.equal(res.status, 204)
  let refused = false
  try { await fetch(`${base}/health`) } catch { refused = true }
  assert.ok(refused, 'post-shutdown fetch should fail')
  await bridge.close()
})

// ---- toBridgeEvent mapping (upstream vocabulary) ----

test('toBridgeEvent flattens session.event to upstream SessionEvent.type', () => {
  // Upstream `assistant/message` notification: the bridge flattens the
  // envelope so the wire carries {sessionId, type, data} directly. No
  // invented `kind`; `type` is the upstream value.
  const ev = toBridgeEvent({
    method: 'session.event',
    params: { sessionId: 's', event: { type: 'assistant/message', data: { text: 'hi' } } },
  })
  assert.equal((ev as any).sessionId, 's')
  assert.equal((ev as any).type, 'assistant/message')
  assert.deepEqual((ev as any).data, { text: 'hi' })

  // turn/end with no data: `data` is omitted (not null) on the wire.
  const ev2 = toBridgeEvent({
    method: 'session.event',
    params: { sessionId: 's', event: { type: 'turn/end' } },
  })
  assert.equal((ev2 as any).type, 'turn/end')
  assert.equal((ev2 as any).data, undefined)
})

test('toBridgeEvent maps session.status and subagent notifications', () => {
  const st = toBridgeEvent({ method: 'session.status', params: { sessionId: 's', status: 'running' } })
  assert.equal((st as any).type, 'session.status')
  assert.equal((st as any).status, 'running')

  const sa = toBridgeEvent({ method: 'subagent.started', params: { parentSessionId: 'p', childSessionId: 'c' } })
  assert.equal((sa as any).type, 'subagent.started')
  assert.equal((sa as any).childSessionId, 'c')

  const sf = toBridgeEvent({
    method: 'subagent.finished',
    params: { provider: 'p', agentId: 'c', parentSessionId: 'p', childSessionId: 'c', status: 'ok', stopReason: 'end_turn' },
  })
  assert.equal((sf as any).type, 'subagent.finished')
  assert.equal((sf as any).status, 'ok')
})

test('toBridgeEvent returns null for unknown upstream SessionEvent.type (no invented kinds)', async () => {
  // Upstream sends an event with a type outside the catalog. The
  // bridge refuses to forward a fabricated kind — surface as null.
  // Reviewer P1 #5 (1jo): drop invented vocabulary, expose the upstream
  // catalog exactly.
  const unk = toBridgeEvent({
    method: 'session.event',
    params: { sessionId: 's', event: { type: 'agent.completed' } },
  })
  assert.equal(unk, null)
})

test('toBridgeEvent returns null for unknown upstream notification methods', () => {
  const unk = toBridgeEvent({ method: 'mystery.method', params: {} })
  assert.equal(unk, null)
})

// ---- POST /sessions/:id/run — owned Activity interval ----

test('POST /sessions/:id/run emits run.start, forwards upstream events, closes on session.status=idle for root', async () => {
  // Reviewer P1 #2 (b8c): subscribe-before-prompt → match
  // agent/inbox/spliced(messageId) → real SessionEvent.type → root
  // session.status=idle. No EOF dependency; no invented terminal event.
  const sub = makeFakeSubscription()
  const runtime = makeFakeRuntime({
    onSubscribe: () => sub,
    onPrompt: () => 'msg-1',
  })
  const bridge = new Bridge({ port: 0, runtimeFactory: () => runtime })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`
  const created = await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })
  const sid = created.json.sessionId as string

  const controller = new AbortController()
  const readerPromise = readSSE(`${base}/sessions/${sid}/run`, controller.signal, { method: 'POST', body: { contentBlocks: [{ type: 'text', text: 'go' }] } })

  // Allow the subscription pump to attach BEFORE we let prompt resolve.
  await new Promise((r) => setTimeout(r, 25))
  // The durable enqueue receipt: bridge waits for this notification to
  // confirm the prompt was spliced into the agent's inbox.
  sub.push({
    method: 'session.event',
    params: {
      sessionId: sid,
      event: {
        type: 'agent/inbox/spliced',
        data: { inserted: [{ id: 'msg-1' }] },
      },
    },
  })
  // Real upstream SessionEvents after the receipt.
  sub.push({
    method: 'session.event',
    params: {
      sessionId: sid,
      event: { type: 'assistant/message', data: { message: { content: [{ type: 'text', text: 'hello' }] } } },
    },
  })
  sub.push({
    method: 'session.event',
    params: {
      sessionId: sid,
      event: { type: 'turn/end' },
    },
  })
  // Root session idle closes the activity.
  sub.push({ method: 'session.status', params: { sessionId: sid, status: 'idle' } })

  const out = await readerPromise
  // Lifecycle: run.start, agent/inbox/spliced, assistant/message,
  // turn/end, session.status(idle), run.end(reason=idle).
  const types = out.map((e: any) => e.type)
  assert.deepEqual(types, [
    'run.start',
    'agent/inbox/spliced',
    'assistant/message',
    'turn/end',
    'session.status',
    'run.end',
  ])
  const start = out.find((e: any) => e.type === 'run.start') as any
  assert.equal(start.messageId, 'msg-1')
  const end = out.find((e: any) => e.type === 'run.end') as any
  assert.equal(end.reason, 'idle')

  // Note: no controller.abort() — the bridge closes the stream itself
  // on idle. The fetch above resolves when the server closes the body.

  await bridge.close()
})

test('POST /sessions/:id/run ignores idle on non-root sessions (subagent completion does not close root Activity)', async () => {
  // Upstream subagent descendants can go idle independently. The root
  // session staying running means the Activity is still open. The
  // bridge must NOT close on a descendant's idle.
  const sub = makeFakeSubscription()
  const runtime = makeFakeRuntime({
    onSubscribe: () => sub,
    onPrompt: () => 'msg-1',
  })
  const bridge = new Bridge({ port: 0, runtimeFactory: () => runtime })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`
  const created = await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })
  const sid = created.json.sessionId as string

  const controller = new AbortController()
  const readerPromise = readSSE(`${base}/sessions/${sid}/run`, controller.signal, { method: 'POST', body: { contentBlocks: [{ type: 'text', text: 'go' }] } })

  await new Promise((r) => setTimeout(r, 25))
  sub.push({
    method: 'session.event',
    params: { sessionId: sid, event: { type: 'agent/inbox/spliced', data: { inserted: [{ id: 'msg-1' }] } } },
  })
  // Subagent descendant goes idle. The root session is still running.
  sub.push({ method: 'session.status', params: { sessionId: 'c-1', status: 'idle' } })
  // ...then the root session finally goes idle.
  sub.push({ method: 'session.status', params: { sessionId: sid, status: 'idle' } })

  const out = await readerPromise
  const end = out.find((e: any) => e.type === 'run.end') as any
  assert.equal(end.reason, 'idle', 'root idle is the activity close, not the descendant idle')
  // Only one run.end frame total.
  assert.equal(out.filter((e: any) => e.type === 'run.end').length, 1)

  controller.abort()
  await bridge.close()
})

test('POST /sessions/:id/run emits bridge.transport_error + run.end(reason=transport_error) on subscription pump error', async () => {
  // The bridge MUST surface pump failures as bridge.transport_error
  // before the activity close (run.end with reason=transport_error).
  // This is what the Go side routes to its typed errCh.
  const sub = makeFakeSubscription()
  const runtime = makeFakeRuntime({
    onSubscribe: () => sub,
    onPrompt: () => 'msg-1',
  })
  const bridge = new Bridge({ port: 0, runtimeFactory: () => runtime })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`
  const created = await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })
  const sid = created.json.sessionId as string

  const controller = new AbortController()
  const readerPromise = readSSE(`${base}/sessions/${sid}/run`, controller.signal, { method: 'POST', body: { contentBlocks: [{ type: 'text', text: 'go' }] } })

  await new Promise((r) => setTimeout(r, 25))
  sub.push({
    method: 'session.event',
    params: { sessionId: sid, event: { type: 'agent/inbox/spliced', data: { inserted: [{ id: 'msg-1' }] } } },
  })
  await new Promise((r) => setTimeout(r, 10))
  sub.fail(new Error('upstream_runtime_disconnected'))

  const out = await readerPromise
  const transport = out.find((e: any) => e.type === 'bridge.transport_error') as any
  assert.ok(transport, 'expected bridge.transport_error frame')
  assert.equal(transport.message, 'upstream_runtime_disconnected')
  const end = out.find((e: any) => e.type === 'run.end') as any
  assert.equal(end.reason, 'transport_error')
  // Transport_error frame appears BEFORE run.end so the consumer sees
  // the same terminal sequence (error then terminal close).
  const transportIdx = out.findIndex((e: any) => e.type === 'bridge.transport_error')
  const endIdx = out.findIndex((e: any) => e.type === 'run.end')
  assert.ok(transportIdx < endIdx, 'bridge.transport_error must precede run.end')

  controller.abort()
  await bridge.close()
})

test('POST /sessions/:id/run 409 when an Activity is already in progress on the session', async () => {
  // Only one Activity per session; concurrent runs on the same session
  // would race on the same upstream subscription.
  const sub = makeFakeSubscription()
  const runtime = makeFakeRuntime({
    onSubscribe: () => sub,
    onPrompt: () => 'msg-1',
  })
  const bridge = new Bridge({ port: 0, runtimeFactory: () => runtime })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`
  const created = await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })
  const sid = created.json.sessionId as string

  // Open first /run and keep it open (do NOT push idle).
  const c1 = new AbortController()
  const r1 = readSSE(`${base}/sessions/${sid}/run`, c1.signal, { method: 'POST', body: { contentBlocks: [{ type: 'text', text: 'go' }] } })
  await new Promise((r) => setTimeout(r, 25))
  // Receipt for the first prompt must arrive before any session.status,
  // otherwise the bridge's receipt-correlation guard treats status frames
  // as prior-turn noise and the pump would never observe idle.
  sub.push({
    method: 'session.event',
    params: {
      sessionId: sid,
      event: { type: 'agent/inbox/spliced', data: { inserted: [{ id: 'msg-1' }] } },
    },
  })

  // Second /run on the same session must 409.
  const r2 = await fetch(`${base}/sessions/${sid}/run`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ contentBlocks: [{ type: 'text', text: 'hi' }] }),
  })
  assert.equal(r2.status, 409)
  const body = await r2.json() as any
  assert.equal(body.error, 'run_in_progress')

  // Tidy up: close first stream, then close bridge.
  sub.push({ method: 'session.status', params: { sessionId: sid, status: 'idle' } })
  await r1
  c1.abort()
  await bridge.close()
})

// Receipt-correlation guard. The pump must drop prior-turn session.status
// frames and any receipts with a foreign messageId; the matching receipt
// is the gate that opens live forwarding, after which root session.status=idle
// closes the Activity. Each scenario uses its own bridge + subscription so
// prior-run state cannot leak.
test('POST /sessions/:id/run drops prior root idle until matching receipt arrives', async () => {
  const sub = makeFakeSubscription()
  const runtime = makeFakeRuntime({
    onSubscribe: () => sub,
    onPrompt: () => 'msg-1',
  })
  const bridge = new Bridge({ port: 0, runtimeFactory: () => runtime })
  await bridge.listen()
  const base = `http://127.0.0.1:${(bridge as any).server.address().port}`
  const sid = (await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })).json.sessionId

  const ctl = new AbortController()
  const reader = readSSE(`${base}/sessions/${sid}/run`, ctl.signal, { method: 'POST', body: { contentBlocks: [{ type: 'text', text: 'go' }] } })

  // Let the pump start + prompt() resolve + run.start fire.
  await new Promise(r => setTimeout(r, 25))
  // Prior-turn root idle: must be dropped, the Activity must NOT close.
  sub.push({ method: 'session.status', params: { sessionId: sid, status: 'idle' } })
  await new Promise(r => setTimeout(r, 25))
  // Receipt for THIS prompt: now forwarding opens.
  sub.push({
    method: 'session.event',
    params: { sessionId: sid, event: { type: 'agent/inbox/spliced', data: { inserted: [{ id: 'msg-1' }] } } },
  })
  // Live assistant + root idle close the Activity.
  sub.push({
    method: 'session.event',
    params: { sessionId: sid, event: { type: 'assistant/message', data: { message: { content: [{ type: 'text', text: 'hi' }] } } } },
  })
  sub.push({ method: 'session.status', params: { sessionId: sid, status: 'idle' } })

  const out = await reader
  const types = out.map((e: any) => e.type)
  assert.deepEqual(types, [
    'run.start',
    'agent/inbox/spliced',
    'assistant/message',
    'session.status',
    'run.end',
  ])
  const end = out.find((e: any) => e.type === 'run.end') as any
  assert.ok(end, 'run.end present')
  assert.equal(end.reason, 'idle')
  await bridge.close()
})

test('POST /sessions/:id/run ignores spliced receipts with a foreign messageId', async () => {
  const sub = makeFakeSubscription()
  const runtime = makeFakeRuntime({
    onSubscribe: () => sub,
    onPrompt: () => 'msg-real',
  })
  const bridge = new Bridge({ port: 0, runtimeFactory: () => runtime })
  await bridge.listen()
  const base = `http://127.0.0.1:${(bridge as any).server.address().port}`
  const sid = (await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })).json.sessionId

  const ctl = new AbortController()
  const reader = readSSE(`${base}/sessions/${sid}/run`, ctl.signal, { method: 'POST', body: { contentBlocks: [{ type: 'text', text: 'go' }] } })

  await new Promise(r => setTimeout(r, 25))
  // Foreign receipt: must be dropped, Activity stays open.
  sub.push({
    method: 'session.event',
    params: { sessionId: sid, event: { type: 'agent/inbox/spliced', data: { inserted: [{ id: 'msg-other' }] } } },
  })
  await new Promise(r => setTimeout(r, 25))
  // Matching receipt: opens forwarding.
  sub.push({
    method: 'session.event',
    params: { sessionId: sid, event: { type: 'agent/inbox/spliced', data: { inserted: [{ id: 'msg-real' }] } } },
  })
  sub.push({
    method: 'session.event',
    params: { sessionId: sid, event: { type: 'assistant/message', data: { message: { content: [{ type: 'text', text: 'hi' }] } } } },
  })
  sub.push({ method: 'session.status', params: { sessionId: sid, status: 'idle' } })

  const out = await reader
  const types = out.map((e: any) => e.type)
  // Only one spliced frame (the matching one).
  const splicedCount = types.filter(t => t === 'agent/inbox/spliced').length
  assert.equal(splicedCount, 1, 'exactly one matching spliced should be forwarded')
  assert.ok(types.includes('run.end'))
  const end = out.find((e: any) => e.type === 'run.end') as any
  assert.equal(end.reason, 'idle')
  await bridge.close()
})

test('POST /sessions/:id/run 404 on unknown session', async () => {
  const runtime = makeFakeRuntime({})
  const bridge = new Bridge({ port: 0, runtimeFactory: () => runtime })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`

  const r = await fetch(`${base}/sessions/s-bogus/run`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ contentBlocks: [{ type: 'text', text: 'hi' }] }),
  })
  assert.equal(r.status, 404)
  await bridge.close()
})
