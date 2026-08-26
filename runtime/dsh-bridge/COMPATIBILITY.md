# Compatibility manifest — dsh-bridge

This document records the release provenance for the dsh-bridge that
the runtime CANNOT enforce via the `initialize.serverInfo` handshake.
The initialize handshake only carries the runtime's PROTOCOL identity
(name + protocol version) — it does NOT carry the npm release pin,
the upstream SDK source revision, or the runtime artifact's build
provenance.

That provenance lives here, and is enforced out-of-band by:
- the npm lockfile pinning `@deepseek-ai/dsh-sdk-*` versions,
- the runtime artifact being built from the exact upstream commit
  recorded below,
- the real-assembled smoke test (tracked in
  bead `work9flow-7dh`) which spawns a runtime built from this commit
  and observes a full prompt cycle against a real LLM.

If any of these change, all three must be bumped in lockstep:
`COMPATIBILITY.md` ← this file, `package.json` ← npm pins,
`src/server.ts` ← `EXPECTED_SERVER_INFO` (only when upstream
intentionally bumps the protocol identity).

## Current pins (2026-08-26)

| Slot                    | Value                                          |
| ----------------------- | ---------------------------------------------- |
| `sdkClient`             | `@deepseek-ai/dsh-sdk-client@0.1.1-rc.2`       |
| `sdkProtocol`           | `@deepseek-ai/dsh-sdk-protocol@0.1.1-rc.2`     |
| `upstreamCommit`        | `b150a551` (TODO: pin full SHA when verified)  |
| `runtimeServer.name`    | `deepseek-harness-sdk-runtime`                 |
| `runtimeServer.version` | `0.0.1` (matches `EXPECTED_SERVER_INFO`)       |

## Why two version surfaces

| Surface                | Meaning                          | Where enforced                  |
| ---------------------- | -------------------------------- | ------------------------------- |
| `sdkClient/Protocol`   | wire shape: which JSON-RPC       | npm install + lockfile          |
|                        | methods exist, what their        |                                 |
|                        | payloads look like               |                                 |
| `runtimeServer.version`| protocol identity: what the      | `validateServerIdentity` in     |
|                        | running process claims to be     | `src/server.ts` (initialize)    |
| `upstreamCommit`       | source-of-truth: exact upstream  | manual: build the runtime       |
|                        | commit the artifact was built    | from this commit + smoke test   |
|                        | from                             |                                 |

Mixing them up — e.g. treating `0.1.1-rc.2` as the runtime protocol
identity — is what the previous version-check did and what the
reviewer rejected (REQUEST_CHANGES on commit `ff4517e`).

## Bumping policy

1. If upstream `@deepseek-ai/dsh-sdk-*` release notes say
   "protocol identity bumped", update `runtimeServer.version` here
   AND `EXPECTED_SERVER_INFO` in `src/server.ts`.
2. If upstream `@deepseek-ai/dsh-sdk-*` release notes say "wire
   shape changed", update the npm pins (`sdkClient`, `sdkProtocol`)
   and the lockfile; the runtimeServer.version may stay.
3. Always update `upstreamCommit` when the runtime artifact is
   rebuilt from a new upstream commit, and re-run the assembled
   smoke test (`work9flow-7dh`).

## Out of scope for this manifest

- The runtime's full build hash (sha256 of the executable). That
  belongs on the runtime artifact itself, enforced by the operator's
  supply chain. The bridge only checks the protocol identity and
  trusts the npm pins.
