/**
 * Resolve and spawn the dsh-jsonrpc-agent runtime subprocess.
 *
 * Two carriers (per upstream python/sdk-runtime README):
 *
 *  - exe (production): a single-file `dsh-jsonrpc-agent-pkg-<platform>-<arch>`
 *    executable plus a matching ripgrep `-rg` sidecar. Shipped in the
 *    `deepseek-harness-runtime-bin` wheel; no Node required on the host.
 *
 *  - node (dev-only): the full deploy closure at
 *    `runtime/node/node_modules/@deepseek-ai/dsh-sdk-jsonrpc-demo/lib/packaged-bin.js`,
 *    executed as `node <bin.js> <cordis.yml>`. Never selected automatically
 *    in production; must be opted into explicitly via DSH_RUNTIME_MODE=node
 *    or `mode: 'node'` in LaunchSpec.
 *
 * This module is intentionally tiny: it does NOT inspect the runtime's
 * stdio (that is the SDK client's job). It only picks the argv tuple.
 */
import { existsSync } from 'node:fs'
import { resolve } from 'node:path'

export type LaunchMode = 'exe' | 'node'

export interface LaunchSpec {
  /** Force a carrier; otherwise resolved from DSH_RUNTIME_MODE env, then auto. */
  mode?: LaunchMode
  /** Path to the cordis config (positional argv for the runtime). */
  cordisConfig: string
  /** Optional explicit exe path (overrides default lookup). */
  exePath?: string
  /** Optional explicit node bin.js path (overrides default lookup). */
  nodeBinJs?: string
  /** Working directory for the runtime process. */
  cwd?: string
  /** Additional args after cordisConfig. */
  extraArgs?: string[]
  /** Env passed to the child. undefined inherits the parent. */
  env?: NodeJS.ProcessEnv
}

export interface ResolvedLaunch {
  command: string
  args: string[]
  cwd?: string
  env?: NodeJS.ProcessEnv
  mode: LaunchMode
}

/**
 * Resolve the launch argv without spawning.
 *
 * Default exe name follows upstream: `dsh-jsonrpc-agent-pkg-<platform>-<arch>`.
 * The runtime wheel places it on PATH or in a venv bin dir; we look first
 * on PATH, then a conventional `./.dsh-runtime/` next to the bridge.
 */
export function resolveLaunch(spec: LaunchSpec): ResolvedLaunch {
  const mode = spec.mode
    ?? (process.env.DSH_RUNTIME_MODE === 'node' ? 'node' : 'exe')

  if (mode === 'node') {
    const binJs = spec.nodeBinJs
      ?? process.env.DSH_NODE_BIN_JS
      ?? resolve('runtime/node/node_modules/@deepseek-ai/dsh-sdk-jsonrpc-demo/lib/packaged-bin.js')
    if (!existsSync(binJs)) {
      throw new Error(`DSH node-mode bin not found: ${binJs}. ` +
        `Build it via deepseek-harness' scripts/build-exe-for-python-sdk.ts in dev mode.`)
    }
    return {
      command: process.execPath,
      args: [binJs, spec.cordisConfig, ...(spec.extraArgs ?? [])],
      cwd: spec.cwd,
      env: spec.env,
      mode: 'node',
    }
  }

  // exe mode
  const exe = spec.exePath
    ?? process.env.DSH_RUNTIME_EXE
    ?? `dsh-jsonrpc-agent-pkg-${process.platform}-${process.arch}`
  if (!existsSync(exe)) {
    throw new Error(`DSH runtime exe not found: ${exe}. ` +
      `Install the deepseek-harness-runtime-bin wheel for ${process.platform}/${process.arch}, ` +
      `or set DSH_RUNTIME_EXE/DshBridge.runtimeExe to the executable path.`)
  }
  return {
    command: exe,
    args: [spec.cordisConfig, ...(spec.extraArgs ?? [])],
    cwd: spec.cwd,
    env: spec.env,
    mode: 'exe',
  }
}
