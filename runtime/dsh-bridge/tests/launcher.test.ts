/**
 * Pure tests for the launcher module — no real runtime spawned.
 */
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { resolveLaunch, type LaunchSpec } from '../src/launcher.ts'

function tmpFile(): { dir: string; cordis: string } {
  const dir = mkdtempSync(join(tmpdir(), 'dsh-bridge-test-'))
  const cordis = join(dir, 'cordis.yml')
  writeFileSync(cordis, '# stub\n')
  return { dir, cordis }
}

test('resolveLaunch: defaults to exe mode when no env/mode set', () => {
  const { dir, cordis } = tmpFile()
  try {
    const fakeExe = join(dir, 'fake-exe')
    writeFileSync(fakeExe, '#!/bin/sh\nexit 0\n', { mode: 0o755 })
    delete process.env.DSH_RUNTIME_MODE
    delete process.env.DSH_RUNTIME_EXE
    delete process.env.DSH_NODE_BIN_JS
    const res = resolveLaunch({ cordisConfig: cordis, exePath: fakeExe })
    assert.equal(res.mode, 'exe')
    assert.equal(res.command, fakeExe)
    assert.equal(res.args[0], cordis)
  } finally { rmSync(dir, { recursive: true, force: true }) }
})

test('resolveLaunch: DSH_RUNTIME_MODE=node forces node mode and reports missing bin', () => {
  const { dir, cordis } = tmpFile()
  try {
    const prev = process.env.DSH_RUNTIME_MODE
    process.env.DSH_RUNTIME_MODE = 'node'
    delete process.env.DSH_NODE_BIN_JS
    assert.throws(
      () => resolveLaunch({ cordisConfig: cordis }),
      /DSH node-mode bin not found/,
    )
    if (prev === undefined) delete process.env.DSH_RUNTIME_MODE
    else process.env.DSH_RUNTIME_MODE = prev
  } finally { rmSync(dir, { recursive: true, force: true }) }
})

test('resolveLaunch: explicit exePath is honoured', () => {
  const { dir, cordis } = tmpFile()
  try {
    const fakeExe = join(dir, 'fake-runtime')
    writeFileSync(fakeExe, '#!/bin/sh\nexit 0\n', { mode: 0o755 })
    const res = resolveLaunch({ cordisConfig: cordis, exePath: fakeExe })
    assert.equal(res.command, fakeExe)
    assert.deepEqual(res.args, [cordis])
  } finally { rmSync(dir, { recursive: true, force: true }) }
})

test('resolveLaunch: explicit nodeBinJs is honoured in node mode', () => {
  const { dir, cordis } = tmpFile()
  try {
    const fakeBin = join(dir, 'packaged-bin.js')
    writeFileSync(fakeBin, '// stub\n')
    const res = resolveLaunch({ mode: 'node', cordisConfig: cordis, nodeBinJs: fakeBin })
    assert.equal(res.mode, 'node')
    assert.equal(res.command, process.execPath)
    assert.equal(res.args[0], fakeBin)
    assert.equal(res.args[1], cordis)
  } finally { rmSync(dir, { recursive: true, force: true }) }
})

test('resolveLaunch: extraArgs are appended after cordisConfig', () => {
  const { dir, cordis } = tmpFile()
  try {
    const fakeExe = join(dir, 'fake-runtime')
    writeFileSync(fakeExe, '#!/bin/sh\nexit 0\n', { mode: 0o755 })
    const res = resolveLaunch({ cordisConfig: cordis, exePath: fakeExe, extraArgs: ['--quiet'] })
    assert.deepEqual(res.args, [cordis, '--quiet'])
  } finally { rmSync(dir, { recursive: true, force: true }) }
})
