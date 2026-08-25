# Project Instructions for AI Agents

This file provides instructions and context for AI coding agents working on this project.

## work9flow architecture invariants

Hand-written rules from the foundation message. Any future change that
breaks one of these requires opening a migration bead first — these
are hard constraints, not guidelines. See `ARCHITECTURE.md` for
process + dependency layout this section assumes.

1. **orchestrator-not-runtime** — work9flowd orchestrates workflow
   runs (state machine, stages, agents); it does NOT host an LLM
   runtime. All LLM execution happens in the upstream DeepSeek
   Harness (DSH) Node process; work9flowd only speaks to it via
   internal/dsh.
2. **upstream-inspection** — before adding any DSH capability
   (session/create, prompt, events, steer, followup, cancel,
   subagent, content-block), inspect the upstream
   `packages/sdk/client/src/api.ts` and `protocol/src/types.ts`
   in `deepseek-ai/deepseek-harness` first. Capability gaps are
   declared honestly with `ErrNotSupported`, never papered over.
3. **bridge-as-only-boundary** — the ONLY Go-side boundary into DSH
   is `internal/dsh.Bridge`. No other package imports
   `@deepseek-ai/dsh-session`, JSON-RPC types, or hand-rolls a
   transport. The runtime/dsh-bridge TypeScript process owns the
   JSON-RPC transport and the upstream SDK.
4. **no-prod-mocks** — production code never carries a mock path
   (no `if dev { stub }`, no env-flag-gated fake DSH). Test doubles
   live under `*_test.go` and are wired only by tests.
5. **no-fork-no-vendor** — never fork the upstream DSH SDK; never
   vendor a third-party SDK in lieu of upstream. Upstream-version
   pinning is the supported upgrade path (see rule 14).
6. **artifact-protocol** — agents emit artifacts as structured
   `ArtifactPayload{Kind,Name,Stage,Content|ContentRef,Metadata}`
   inside `agent.completed.data.artifacts`. The engine persists via
   `storage.AddArtifact`; downstream stages consume via
   `storage.ListArtifacts`. No agent speaks file paths in messages.
7. **immutable-task** — `WorkflowRun.OriginalTask` and `RepoPath`
   are set at create-time and never mutated afterwards. Any
   re-scoping is a NEW run.
8. **llm-evidence-not-routing** — the LLM (agent) emits
   `review_findings[{class,statement,evidence,...}]` as evidence;
   the engine routes state transitions on those classes via
   `domain.FindingClass.IsBlocking()`. The agent never decides
   routing — only its evidence is consumed.
9. **fail-closed** — partial execution is never reported as
   success. If a stage runner returns an error, the run transitions
   to FAILED with the error wrapped in `TerminalReason`. No
   "outcome=advance" swallowed errors.
10. **blocking-attention** — when an agent emits
    `outcome=wait_user` (or a `REQUIREMENT_AMBIGUITY` /
    `BLOCKING_DECISION` finding), the engine MUST materialise a
    domain.Attention with status=OPEN before the run stops
    advancing. Attention close resumes the run, not raw
    `outcome=advance`.
11. **explicit-next-stage** — every `StageDef.Transition` returns
    the explicit next `domain.RunState`. No fall-through, no default
    "next in slice", no inferred ordering. The engine rejects an
    unknown state.
12. **persisted-then-published** — events are appended to storage
    BEFORE the WS publish is attempted (`storage.AppendEvent` is
    the source of truth). Publish is best-effort; replay covers
    missed subscribers.
13. **no-fake-capabilities** — features like steer, followup,
    cancel, per-session close, etc. that the upstream DSH SDK does
    NOT expose are NOT invented at the Go layer. We return
    `ErrNotSupported` (HTTP 501 from the bridge) and let the
    caller decide.
14. **upgrade-via-pinned-suite** — DSH upgrade means bumping the
    pinned versions in `runtime/dsh-bridge/package.json` (sdk
    + protocol + jsonrpc-agent wheel) AT THE SAME COMMIT as any
    work9flow surface adjustment. No partial drift.
15. **mocks-prove-controller-not-dsh** — `*_test.go` mocks prove
    the work9flow controller (engine state machine, agents.Runner
    wiring, bridge HTTP client parsing). They prove NOTHING about
    DSH itself; the only honest evidence for DSH is running
    `runtime/dsh-bridge/` against the upstream SDK with a real
    provider (DEEPSEEK_API_KEY smoke).

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:6cd5cc61 -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

**Architecture in one line:** issues live in a local Dolt DB; sync uses `refs/dolt/data` on your git remote; `.beads/issues.jsonl` is a passive export. See https://github.com/gastownhall/beads/blob/main/docs/SYNC_CONCEPTS.md for details and anti-patterns.

## Agent Context Profiles

The managed Beads block is task-tracking guidance, not permission to override repository, user, or orchestrator instructions.

- **Conservative (default)**: Use `bd` for task tracking. Do not run git commits, git pushes, or Dolt remote sync unless explicitly asked. At handoff, report changed files, validation, and suggested next commands.
- **Minimal**: Keep tool instruction files as pointers to `bd prime`; use the same conservative git policy unless active instructions say otherwise.
- **Team-maintainer**: Only when the repository explicitly opts in, agents may close beads, run quality gates, commit, and push as part of session close. A current "do not commit" or "do not push" instruction still wins.

## Session Completion

This protocol applies when ending a Beads implementation workflow. It is subordinate to explicit user, repository, and orchestrator instructions.

1. **File issues for remaining work** - Create beads for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **Handle git/sync by active profile**:
   ```bash
   # Conservative/minimal/default: report status and proposed commands; wait for approval.
   git status

   # Team-maintainer opt-in only, unless current instructions forbid it:
   git pull --rebase
   git push
   git status
   ```
5. **Hand off** - Summarize changes, validation, issue status, and any blocked sync/commit/push step

**Critical rules:**
- Explicit user or orchestrator instructions override this Beads block.
- Do not commit or push without clear authority from the active profile or the current user request.
- If a required sync or push is blocked, stop and report the exact command and error.
<!-- END BEADS INTEGRATION -->


## Build & Test

_Add your build and test commands here_

```bash
# Example:
# npm install
# npm test
```

## Architecture Overview

_Add a brief overview of your project architecture_

## Conventions & Patterns

_Add your project-specific conventions here_
