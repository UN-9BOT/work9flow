/**
 * Pin the project-local npm registry to npmmirror.com.
 *
 * Per bead work9flow-azy (dsh-A.10g, P1): the runtime/dsh-bridge
 * sub-package depends on @deepseek-ai/dsh-sdk-* (0.1.1-rc.2), which
 * carries shell/filesystem capabilities via the spawned
 * dsh-jsonrpc-agent subprocess. Pin the registry so a fresh
 * `npm install` is reproducible and doesn't silently drift when the
 * operator's ~/.npmrc changes.
 *
 * Operator decision (2026-08-26): pin to the registry that matches
 * the rest of the dependency snapshot already in package-lock.json
 * (registry.npmmirror.com — confirmed via grep on the lockfile).
 */
import { readFileSync, existsSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { test } from 'node:test'
import assert from 'node:assert/strict'

const here = dirname(fileURLToPath(import.meta.url))
const npmrcPath = join(here, '..', '.npmrc')

test('.npmrc exists in runtime/dsh-bridge/', () => {
  assert.ok(
    existsSync(npmrcPath),
    `expected project-local .npmrc at ${npmrcPath} — see work9flow-azy`,
  )
})

test('.npmrc pins registry to npmmirror.com', () => {
  const content = readFileSync(npmrcPath, 'utf8')
  assert.match(
    content,
    /^registry\s*=\s*https:\/\/registry\.npmmirror\.com\//m,
    '.npmrc must pin registry=https://registry.npmmirror.com/ — see work9flow-azy',
  )
})

test('.npmrc has exactly one registry= line (no later override)', () => {
  const content = readFileSync(npmrcPath, 'utf8')
  const registryLines = content
    .split('\n')
    .filter((l) => /^registry\s*=/.test(l))
  assert.equal(
    registryLines.length,
    1,
    `expected exactly one registry= line in .npmrc, got ${registryLines.length}`,
  )
})
