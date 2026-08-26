/**
 * Pin the project-local npm registry to canonical npmjs.com.
 *
 * Per user decision (reviewer P1 #4 — work9flow-8w0 feedback) and
 * foundation policy: dependencies with shell/filesystem capabilities
 * (the @deepseek-ai/dsh-sdk-* packages) MUST NOT be silently swapped
 * across registries. Operator-local ~/.npmrc may point anywhere
 * (e.g. registry.npmmirror.com), but the project's runtime/dsh-bridge
 * sub-package pins its OWN .npmrc to canonical registry.npmjs.org so
 * a fresh `npm install` is reproducible across machines.
 *
 * Note: this bead does NOT regenerate package-lock.json — the existing
 * lockfile's `resolved` URLs may still point at a different mirror if
 * they were generated when ~/.npmrc was set to that mirror. Future
 * `npm install` runs from this directory will rewrite those URLs to
 * canonical. That regeneration is tracked separately.
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

test('.npmrc pins registry to canonical npmjs.com', () => {
  const content = readFileSync(npmrcPath, 'utf8')
  assert.match(
    content,
    /^registry\s*=\s*https:\/\/registry\.npmjs\.org\//m,
    '.npmrc must pin registry=https://registry.npmjs.org/ — see work9flow-azy',
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
