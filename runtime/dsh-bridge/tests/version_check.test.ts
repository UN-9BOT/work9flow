/**
 * Startup version-check tests.
 *
 * The bridge pins @deepseek-ai/dsh-sdk-{client,protocol} 0.1.1-rc.2 in
 * runtime/dsh-bridge/package.json. The runtime's `initialize` handshake
 * reports its own version via serverInfo. If the runtime version drifts
 * from the SDK pin, the bridge must fail HARD (no warning, no
 * patch-level leniency) — this prevents silent protocol drift.
 *
 * Per user decision (reviewer P1 #3, 2026-08-25): fail-hard on any
 * mismatch, not just major/minor, not warn-only.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'

import { validateRuntimeVersion, PINNED_RUNTIME_VERSION } from '../src/server.ts'

test('validateRuntimeVersion: exact pin passes', () => {
  assert.doesNotThrow(() =>
    validateRuntimeVersion({ name: 'deepseek-harness-sdk-runtime', version: PINNED_RUNTIME_VERSION }),
  )
})

test('validateRuntimeVersion: any mismatch throws and surfaces both versions', () => {
  let caught: Error | undefined
  try {
    validateRuntimeVersion({ name: 'deepseek-harness-sdk-runtime', version: '0.0.1' })
  } catch (e) {
    caught = e as Error
  }
  assert.ok(caught, 'expected throw on version mismatch')
  assert.match(caught!.message, /version/i)
  assert.match(caught!.message, /0\.1\.1-rc\.2/)
  assert.match(caught!.message, /0\.0\.1/)
})

test('validateRuntimeVersion: patch-level drift is also rejected', () => {
  // Per user: NO patch-level leniency. 0.1.1-rc.3 is still a mismatch.
  assert.throws(() =>
    validateRuntimeVersion({ name: 'deepseek-harness-sdk-runtime', version: '0.1.1-rc.3' }),
  )
})
