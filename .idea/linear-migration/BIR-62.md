# Goal
Implement the second half of the MVP `feature-development` workflow: approved-plan execution by Agent 4, parallel/specialized implementation review by Agent 5, deterministic finding routing, correction loops, and terminal completion.

## Workflow stages in this task
`READY_FOR_IMPLEMENTATION → IMPLEMENTING → IMPLEMENTATION_REVIEW → {IMPLEMENTING | PLANNING/PLAN_REVIEW | WAITING_FOR_USER | DONE}`

## Agent 4 — Implementer

### Inputs
* immutable original task;
* all user clarifications;
* approved active `feature-spec` version;
* approved active `implementation-plan` version;
* applicable repository instructions/skills;
* repository/worktree;
* any structured implementation-review findings routed back from a prior loop.

### Behavioral contract
Implement **the approved plan**, not a newly invented feature. Agent 4 may inspect additional repository context needed to perform the approved steps, but must not silently expand requirements or redesign the approved architecture.

If implementation uncovers a genuine plan defect/missing prerequisite, return structured `blocked_by_plan` with: concise issue; evidence (file/symbol/test/error/history); why it cannot be solved within the approved plan; suggested planning question/change if useful. Controller routes this back to plan review/planning.

### Implementation result
Persist/emit enough structured result metadata: changed files/symbols; tests added/changed; commands/tests run and status; known limitations/blockers; active plan/spec versions used.

## Agent 5 — Implementation Review Orchestrator
Coordinates independent review workers and synthesizes their output.

### Minimum review perspectives
1. correctness / requirement compliance;
2. architecture / plan compliance;
3. tests / edge cases / regressions;
4. scope / unintended changes.

Security review can be invoked when the feature/spec indicates security-sensitive behavior, but a full always-on security audit workflow is post-MVP. Use parallel Harness subagents/workflow fan-out where supported and appropriate.

### Reviewer inputs
Original task; user clarifications; approved feature spec; approved implementation plan; implementation diff/change summary; repository access/instructions/skills as needed. Reviewers must cite concrete evidence for blocking findings.

## Finding schema
Stable ID; severity/blocking flag; class; concise statement; requirement/plan reference when applicable; file/symbol/evidence when applicable; rationale; suggested action.

Classes: `IMPLEMENTATION_BUG`; `PLAN_DEFECT`; `REQUIREMENT_AMBIGUITY`; `OUT_OF_SCOPE`; `STYLE`; `FALSE_POSITIVE`. Agent 5 synthesizer must remove duplicates and obvious non-findings while retaining provenance.

## Deterministic routing
Owned by controller policy, not ad-hoc agent choice:
- `IMPLEMENTATION_BUG` → Agent 4, repeat implementation review.
- `PLAN_DEFECT` → Plan Gatekeeper/Planner. New plan versions required.
- `REQUIREMENT_AMBIGUITY` → blocking Attention for user. Persist answer as clarification, then replan/review.
- `OUT_OF_SCOPE` → Agent 4 to remove/revert unless it exposes a plan contradiction.
- `STYLE` → non-blocking by default unless repo instructions make it mandatory.
- `FALSE_POSITIVE` → discard from blocking routing but retain audit metadata.

Do not let Agent 4 be the sole judge of whether criticism against its own implementation should be ignored. Classification/synthesis/routing is owned by Agent 5 + controller policy.

## Iteration limits
Enforce configured maximum implementation/review loops. On exhaustion, stop with explicit failure/needs-human status and retain all findings/artifacts/events.

## Completion
`DONE` allowed only when: no blocking synthesized findings remain; approved active spec/plan versions match the implementation that was reviewed; required tests/validation from the implementation plan were executed or explicitly accounted for; workflow state and event history are durable.

## Testing
Cover with fake reviewers/implementer plus real Harness integration paths: successful implementation → clean review → done; implementation bug → implementer → repeat review → done; plan defect → Planner/Gatekeeper → new approved plan → Implementer → review; requirement ambiguity → Attention/user clarification → replan; duplicate reviewer findings deduplicated; conflicting reviewer findings represented/resolved safely; iteration limit exhaustion; active spec/plan version changes propagate to implementation/review inputs.

## Acceptance criteria
The entire backend `feature-development` workflow can reach durable `DONE` without a TUI by using the work9flow protocol/controller, with real Harness-backed implementation/review agents, parallel review fan-out/fan-in, correct routing loops, artifact/version traceability, and deterministic orchestration tests.

---
**Source:** Linear BIR-62 — https://linear.app/vibeeer/issue/BIR-62/mvp-06-implement-approved-plan-execution-parallel-implementation
**Parent epic:** BIR-55
**Depends on:** BIR-58, BIR-59, BIR-60, BIR-61
