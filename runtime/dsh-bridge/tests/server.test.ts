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
  serverInfo?: { name: string; version: string }
}): BridgeRuntime {
  const subscriptions = new Map<string, FakeSubscription>()
  let messageCounter = 0
  const client = {
    prompt: async (sessionId: string, blocks: ContentBlock[]) => {
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

async function readSSE(url: string, signal: AbortSignal): Promise<HarnessNotification[]> {
  const res = await fetch(url, { signal })
  if (res.status !== 200) throw new Error(`expected 200, got ${res.status}`)
  const reader = res.body!.getReader()
  const dec = new TextDecoder()
  let buf = ''
  const out: HarnessNotification[] = []
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
  let receivedReq: unknown = undefined
  const factory = (req: unknown) => { receivedReq = req; return runtime }
  const bridge = new Bridge({ port: 0, runtimeFactory: factory })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`

  const r = await jsonPost(`${base}/sessions`, {
    cwd: '/tmp/x', provider: 'deepseek-official', model: 'deepseek-v4-flash',
  })
  assert.equal(r.status, 200)
  assert.ok(typeof r.json.sessionId === 'string' && r.json.sessionId.length > 0)
  assert.ok(receivedReq, 'factory should receive request')
  const receivedBody = receivedReq as { provider?: string; cwd?: string; model?: string }
  assert.equal(receivedBody.provider, 'deepseek-official')

  await bridge.close()
})

test('POST /sessions 400 on missing fields', async () => {
  const bridge = new Bridge({ port: 0, runtimeFactory: () => makeFakeRuntime({}) })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`

  for (const body of [{}, { cwd: '/x' }, { cwd: '/x', provider: 'p' }, { provider: 'p', model: 'm' }]) {
    const r = await jsonPost(`${base}/sessions`, body)
    assert.equal(r.status, 400, `body=${JSON.stringify(body)}`)
  }
  await bridge.close()
})

test('POST /sessions/:id/prompt returns messageId from upstream', async () => {
  let lastId = ''
  let lastBlocks: ContentBlock[] = []
  const harness = makeFakeRuntime({
    onPrompt: (id, blocks) => { lastId = id; lastBlocks = blocks; return 'msg-42' },
  })
  const bridge = new Bridge({ port: 0, runtimeFactory: () => harness })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`

  const created = await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })
  const sid = created.json.sessionId
  const r = await jsonPost(`${base}/sessions/${sid}/prompt`, {
    contentBlocks: [{ type: 'text', text: 'hi' } as ContentBlock],
  })
  assert.equal(r.status, 200)
  assert.equal(r.json.messageId, 'msg-42')
  assert.equal(lastId, sid)
  assert.equal(lastBlocks.length, 1)

  await bridge.close()
})

test('POST /sessions/:id/prompt 404 on unknown session', async () => {
  const bridge = new Bridge({ port: 0, runtimeFactory: () => makeFakeRuntime({}) })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`

  const r = await jsonPost(`${base}/sessions/no-such-session/prompt`, { contentBlocks: [] })
  assert.equal(r.status, 404)

  await bridge.close()
})

test('POST /sessions/:id/close returns 501 (upstream has no per-session close)', async () => {
  const bridge = new Bridge({ port: 0, runtimeFactory: () => makeFakeRuntime({}) })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`
  const created = await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })
  const r = await jsonPost(`${base}/sessions/${created.json.sessionId}/close`, {})
  assert.equal(r.status, 501)
  assert.match(r.json.detail ?? '', /no per-session close/)
  await bridge.close()
})

test('GET /sessions/:id/events streams upstream notifications as SSE', async () => {
  const subs = new Map<string, FakeSubscription>()
  const make = () => makeFakeSubscription()
  const harness = makeFakeRuntime({
    onSubscribe: (id) => {
      let s = subs.get(id)
      if (!s) { s = make(); subs.set(id, s) }
      return s
    },
  })
  const bridge = new Bridge({ port: 0, runtimeFactory: () => harness })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`

  const created = await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })
  const sid = created.json.sessionId

  // Open SSE first; the bridge calls subscribeSessionTree in the GET handler,
  // and only THEN does our onSubscribe factory populate `subs`.
  const ctrl = new AbortController()
  const collected = readSSE(`${base}/sessions/${sid}/events`, ctrl.signal)
  // give the consumer a tick to subscribe
  await new Promise((r) => setTimeout(r, 50))
  const sub = subs.get(sid)
  assert.ok(sub, 'subscribe factory should have populated the test map')

  sub.push({ method: 'session.event', params: { sessionId: sid, event: { kind: 'turn/end', seq: 1 } } })
  sub.push({ method: 'session.status', params: { sessionId: sid, status: 'idle' } })
  sub.push({ method: 'subagent.started', params: { parentSessionId: sid, childSessionId: 'c-1' } })

  // give the stream a beat to deliver
  await new Promise((r) => setTimeout(r, 100))
  ctrl.abort()
  const out = await collected

  // Find each kind in the SSE output
  const event = out.find((e) => (e as any).kind === 'session.event')
  const status = out.find((e) => (e as any).kind === 'session.status')
  const started = out.find((e) => (e as any).kind === 'subagent.started')
  assert.ok(event, 'expected session.event SSE frame')
  assert.equal((event as any).sessionId, sid)
  assert.ok(status)
  assert.equal((status as any).status, 'idle')
  assert.ok(started)
  assert.equal((started as any).childSessionId, 'c-1')

  await bridge.close()
})

test('GET /health reflects lifecycle', async () => {
  const bridge = new Bridge({ port: 0, runtimeFactory: () => makeFakeRuntime({}) })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`

  const r1 = await (await fetch(`${base}/health`)).json() as any
  assert.equal(r1.status, 'starting')

  await jsonPost(`${base}/sessions`, { cwd: '/tmp', provider: 'p', model: 'm' })
  const r2 = await (await fetch(`${base}/health`)).json() as any
  assert.equal(r2.status, 'ready')

  await bridge.close()
})

test('GET /health and POST /sessions surface the runtime-reported serverInfo (not fabricated)', async () => {
  // The previous bridge fabricated `{ name: 'deepseek-harness-sdk-runtime', version: '0.0.1' }`
  // for /health and /sessions regardless of the actual runtime. Now the
  // runtimeFactory's serverInfo is surfaced honestly. The runtime is
  // created lazily on first /sessions, so we POST first, then /health.
  const wire = { name: 'deepseek-harness-sdk-runtime', version: '0.4.2-rc.7' }
  const bridge = new Bridge({
    port: 0,
    runtimeFactory: () => makeFakeRuntime({ serverInfo: wire }),
  })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`

  // Before any /sessions, the bridge has not initialised the runtime;
  // /health reports `starting` and omits serverInfo.
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
  // We force the bridge into the starting-but-no-runtime state by never
  // creating a session. The default runtimeFactory would spawn a real
  // process, so we inject one that throws on start() — that path leaves
  // `this.runtime` undefined and the bridge reports `status: starting`
  // with no serverInfo.
  const bridge = new Bridge({
    port: 0,
    runtimeFactory: () => makeFakeRuntime({ startShouldThrow: true }),
  })
  await bridge.listen()
  const addr = (bridge as any).server.address()
  const base = `http://127.0.0.1:${addr.port}`

  // /health before any session — runtime is undefined → status=starting, no serverInfo.
  const h = await (await fetch(`${base}/health`)).json() as any
  assert.equal(h.status, 'starting')
  assert.equal(h.serverInfo, undefined, 'must not fabricate serverInfo when runtime not ready')

  // POST /sessions will fail (start throws) — we just want to assert no
  // crash on the health path itself.
  await bridge.close()
})

test('POST /shutdown returns 204', async () => {
  // We deliberately do NOT assert harness.close() was called here: that
  // happens async after res.end() and server.close() completes, which
  // is observable behaviour rather than a contract. The 204 + server
  // no longer accepting new connections is what we promise to the caller.
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
  // Server should now refuse new connections.
  let refused = false
  try { await fetch(`${base}/health`) } catch { refused = true }
  assert.ok(refused, 'post-shutdown fetch should fail')
  await bridge.close()
})

// ---- toBridgeEvent mapping ----

test('toBridgeEvent maps session.event / session.status / subagent.*', () => {
  const ev = toBridgeEvent({ method: 'session.event', params: { sessionId: 's', event: { kind: 'turn/end' } } })
  assert.equal(ev!.kind, 'session.event')
  assert.equal((ev as any).sessionId, 's')

  const st = toBridgeEvent({ method: 'session.status', params: { sessionId: 's', status: 'running' } })
  assert.equal(st!.kind, 'session.status')
  assert.equal((st as any).status, 'running')

  const sa = toBridgeEvent({ method: 'subagent.started', params: { parentSessionId: 'p', childSessionId: 'c' } })
  assert.equal(sa!.kind, 'subagent.started')
  assert.equal((sa as any).childSessionId, 'c')

  const sf = toBridgeEvent({
    method: 'subagent.finished',
    params: { provider: 'p', agentId: 'c', parentSessionId: 'p', childSessionId: 'c', status: 'ok', stopReason: 'end_turn' },
  })
  assert.equal(sf!.kind, 'subagent.finished')
  assert.equal((sf as any).status, 'ok')

  const unk = toBridgeEvent({ method: 'mystery', params: {} })
  assert.equal(unk!.kind, 'bridge.error')
})
