/**
 * Pure argv resolver for the dsh-jsonrpc-agent runtime. The upstream
 * HarnessClient owns the actual subprocess lifecycle and spawns the
 * binary itself; the bridge never spawns directly.
 *
 * Two carriers are supported (per upstream python/sdk-runtime README):
 *
 *  - exe (production): a single-file `dsh-jsonrpc-agent-pkg-<os>-<arch>`
 *    executable plus matching ripgrep `-rg` and platform spawn-helper
 *    sidecars, shipped in the `deepseek-harness-runtime-bin` wheel.
 *    No auto-discovery: the operator MUST set DSH_RUNTIME_EXE (or
 *    pass LaunchSpec.exePath) to the absolute path of the runtime
 *    binary. Real resolver that handles upstream carrier names +
 *    sidecar binaries is tracked in bead 4i1.
 *
 *  - node (dev-only): the full deploy closure
 *    `runtime/node/node_modules/@deepseek-ai/dsh-sdk-jsonrpc-demo/lib/packaged-bin.js`,
 *    executed as `node <bin.js> <cordis.yml>`. Must be opted into
 *    explicitly via DSH_NODE_BIN_JS or LaunchSpec.nodeBinJs.
 */
import { existsSync } from 'node:fs'

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
 * The bridge is a pure resolver: it never searches PATH, never guesses
 * `<platform>-<arch>`, and never inspects `./.dsh-runtime/`. The operator
 * MUST set DSH_RUNTIME_EXE / LaunchSpec.exePath (or DSH_NODE_BIN_JS /
 * LaunchSpec.nodeBinJs for the dev-only node mode) to the absolute path
 * of the runtime binary. A real resolver that handles upstream carrier
 * names and sidecar binaries is tracked in bead work9flow-4a2.
 */
export function resolveLaunch(spec: LaunchSpec): ResolvedLaunch {
  const mode = spec.mode
    ?? (process.env.DSH_RUNTIME_MODE === 'node' ? 'node' : 'exe')

  if (mode === 'node') {
    // No auto-discovery: dev-mode node carrier must be wired by the
    // operator explicitly via DSH_NODE_BIN_JS (or LaunchSpec.nodeBinJs).
    const binJs = spec.nodeBinJs ?? process.env.DSH_NODE_BIN_JS
    if (!binJs) {
      throw new Error(
        `DSH node-mode requires an explicit nodeBinJs path: set ` +
        `DSH_NODE_BIN_JS or pass LaunchSpec.nodeBinJs. Real resolver ` +
        `is tracked in bead 4i1.`,
      )
    }
    if (!existsSync(binJs)) {
      throw new Error(`DSH node-mode bin not found: ${binJs}.`)
    }
    return {
      command: process.execPath,
      args: [binJs, spec.cordisConfig, ...(spec.extraArgs ?? [])],
      cwd: spec.cwd,
      env: spec.env,
      mode: 'node',
    }
  }

  // exe mode (production)
  // No platform-arch guessing, no PATH lookup: the operator MUST set
  // DSH_RUNTIME_EXE (or pass LaunchSpec.exePath) to the absolute path
  // of the dsh-jsonrpc-agent binary from the upstream runtime wheel.
  // Platform guessing mismatched upstream carrier names (Node reports
  // darwin, upstream uses -macos-) and never located the -rg /
  // platform spawn-helper sidecars the runtime wheel depends on.
  const exe = spec.exePath ?? process.env.DSH_RUNTIME_EXE
  if (!exe) {
    throw new Error(
      `DSH exe-mode requires an explicit DSH_RUNTIME_EXE (or ` +
      `LaunchSpec.exePath). Real resolver is tracked in bead 4i1.`,
    )
  }
  if (!existsSync(exe)) {
    throw new Error(`DSH runtime exe not found at: ${exe}.`)
  }
  return {
    command: exe,
    args: [spec.cordisConfig, ...(spec.extraArgs ?? [])],
    cwd: spec.cwd,
    env: spec.env,
    mode: 'exe',
  }
}
