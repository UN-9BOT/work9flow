# dsh-bridge

A small TypeScript process that bridges work9flow (Go) to a DeepSeek Harness
runtime subprocess via the official
[`@deepseek-ai/dsh-sdk-client`](https://www.npmjs.com/package/@deepseek-ai/dsh-sdk-client).

The bridge is the only place where DSH SDK specifics (jsonrpc, agent/*
notifications, content-block encoding) live. work9flow talks plain HTTP
to it and sees normalized session lifecycle events.

## Layout

```
runtime/dsh-bridge/
├── package.json      pinned @deepseek-ai/dsh-sdk-client 0.1.1-rc.2
├── tsconfig.json
├── src/
│   ├── launcher.ts   spawn dsh-jsonrpc-agent (exe or dev node carrier)
│   ├── server.ts     HTTP server + HarnessClient wrapper
│   └── types.ts      wire types
└── tests/
    ├── launcher.test.ts  pure launcher tests
    └── server.test.ts    HTTP layer tests with a fake DeepSeekHarness
```

## Wire protocol (bridge ↔ work9flow)

| Method | Path                              | Purpose                                      |
| ------ | --------------------------------- | -------------------------------------------- |
| GET    | `/health`                         | bridge + initialize handshake status         |
| POST   | `/sessions`                       | create session (lazy initialize handshake)   |
| POST   | `/sessions/:id/prompt`            | enqueue prompt (returns durable messageId)   |
| GET    | `/sessions/:id/events`            | SSE stream of session.event + session.status + subagent.* |
| POST   | `/sessions/:id/close`             | **501** — upstream has no per-session close  |
| POST   | `/shutdown`                       | graceful shutdown of the bridge + runtime    |

SSE event frames are JSON with a discriminator `kind`:

```ts
type BridgeEvent =
  | { kind: 'session.event'; sessionId: string; event: Record<string, unknown> }
  | { kind: 'session.status'; sessionId: string; status: 'idle' | 'running' }
  | { kind: 'subagent.started'; parentSessionId: string; childSessionId: string }
  | { kind: 'subagent.finished'; ... }
  | { kind: 'bridge.error'; message: string }
```

## Build & Runtime resolution

Two carriers (see upstream
[python/sdk-runtime/README.md](https://github.com/deepseek-ai/deepseek-harness/blob/master/python/sdk-runtime/README.md)):

- **exe** (production): `dsh-jsonrpc-agent-pkg-<platform>-<arch>` from the
  `deepseek-harness-runtime-bin` wheel.
- **node** (dev-only): the source build at
  `runtime/node/node_modules/@deepseek-ai/dsh-sdk-jsonrpc-demo/lib/packaged-bin.js`.
  Must be opted into via `DSH_RUNTIME_MODE=node` or `BridgeOptions.launch.mode = 'node'`.

Resolution priority (explicit-only — bead 4i1 closed the auto-resolver):

1. `BridgeOptions.launch.exePath` / `nodeBinJs`
2. `DSH_RUNTIME_EXE` / `DSH_NODE_BIN_JS` env vars
3. `DSH_RUNTIME_MODE` env (`exe` | `node`); default = `exe`

The bridge NEVER searches PATH, never guesses `<platform>-<arch>`, and
never inspects `./.dsh-runtime/`. The operator MUST set the explicit
path or env var. When the lookup fails the bridge fails loudly with a
message naming both acquisition routes (install the wheel or set the
explicit path). This is a deliberate departure from any silent
fallback.

## What is NOT here

- No per-prompt cancel / no per-session close — upstream SDK has neither.
  The bridge surfaces the gap honestly (`501`) rather than emulating it.
- No invented `?since=` cursor on the events stream — upstream notifications
  are push-based via SDK subscriptions.
- No `localdsh`-style OpenAI-compatible passthrough — the bridge always
  goes through the real DSH runtime.

## Build & Run

```bash
cd runtime/dsh-bridge
npm install
DSH_CORDIS_CONFIG=/path/to/cordis.yml npm run build && npm start
```

(or, for dev with source-built runtime:)

```bash
DSH_RUNTIME_MODE=node \
DSH_NODE_BIN_JS=../deepseek-harness/runtime/node/node_modules/@deepseek-ai/dsh-sdk-jsonrpc-demo/lib/packaged-bin.js \
DSH_CORDIS_CONFIG=/path/to/cordis.yml \
npm start
```

## Test

```bash
npm test          # 26 tests, no runtime needed
```

The HTTP-layer tests use a `harnessFactory` injection — a fake
`DeepSeekHarness` whose `client.prompt` and `client.subscribeSessionTree`
behave like the real SDK's. No real runtime required.
