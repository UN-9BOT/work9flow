/**
 * Initialize-time server identity validation.
 *
 * Upstream DSH runtime reports its identity via the `initialize`
 * handshake's `serverInfo`. That identity is the runtime's protocol
 * version (currently `0.0.1`), NOT the npm release pin of the SDK
 * (`@deepseek-ai/dsh-sdk-client` `0.1.1-rc.2`). The two are
 * intentionally separate:
 *
 *   - npm SDK pin   ↔ wire shape (which JSON-RPC methods exist)
 *   - serverInfo    ↔ runtime protocol identity (what the running
 *                    process claims to be)
 *
 * Release provenance (sdkClient, sdkProtocol, upstreamCommit) lives
 * separately in runtime/dsh-bridge/COMPATIBILITY.md and is enforced
 * out-of-band by the build, not by initialize.
 *
 * Per user decision (reviewer P1 #3, 2026-08-25): fail HARD on any
 * server identity mismatch — wrong name, wrong version, or malformed
 * serverInfo. No patch-level leniency.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'

import {
  validateServerIdentity,
  EXPECTED_SERVER_INFO,
} from '../src/server.ts'

test('validateServerIdentity: exact name + version match passes', () => {
  assert.doesNotThrow(() =>
    validateServerIdentity({ ...EXPECTED_SERVER_INFO }),
  )
})

test('validateServerIdentity: wrong server name throws', () => {
  assert.throws(
    () =>
      validateServerIdentity({
        name: 'some-other-runtime',
        version: EXPECTED_SERVER_INFO.version,
      }),
    /name/i,
  )
})

test('validateServerIdentity: wrong server protocol identity (version) throws', () => {
  assert.throws(
    () =>
      validateServerIdentity({
        name: EXPECTED_SERVER_INFO.name,
        version: '0.0.2',
      }),
    /version/i,
  )
})

test('validateServerIdentity: patch-level drift on runtime identity is also rejected', () => {
  assert.throws(
    () =>
      validateServerIdentity({
        name: EXPECTED_SERVER_INFO.name,
        version: '0.0.1-canary',
      }),
  )
})

test('validateServerIdentity: missing name throws (malformed serverInfo)', () => {
  assert.throws(
    // @ts-expect-error — intentionally malformed
    () => validateServerIdentity({ version: EXPECTED_SERVER_INFO.version }),
    /name/i,
  )
})

test('validateServerIdentity: missing version throws (malformed serverInfo)', () => {
  assert.throws(
    // @ts-expect-error — intentionally malformed
    () => validateServerIdentity({ name: EXPECTED_SERVER_INFO.name }),
    /version/i,
  )
})

test('validateServerIdentity: empty name throws (malformed serverInfo)', () => {
  assert.throws(
    () => validateServerIdentity({ name: '', version: EXPECTED_SERVER_INFO.version }),
    /name/i,
  )
})
