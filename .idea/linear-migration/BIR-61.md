# Goal
Implement the first half of the MVP `feature-development` workflow using real Harness-backed specialized agents and explicit versioned artifacts.

## Workflow stages in this task
`NEW → DISCOVERY → PLANNING → PLAN_REVIEW → {WAITING_FOR_USER | PLANNING | READY_FOR_IMPLEMENTATION}`

Implementation/review after approval is covered by BIR-62.

## Shared input contract
Every stage receives only the explicit data it needs from durable work9flow state. Do not use entire prior agent conversations as the primary contract.

Shared sources: immutable original user task; append-only user clarifications; user-provided files/URLs/docs/curl/issues; repository; applicable AGENTS.md/CLAUDE.md; applicable skill metadata/content; stage artifacts from prior agents; git history where required.

## Agent 1 — Repository Scout
Collect evidence; do not propose architecture. Inspect user-provided references first-class; repository entry points and related symbols; tests/config/docs; direct callers/callees/usages; repository instructions and skills; likely relevant history locations.

Required artifacts: `breadcrumbs.json` (with provenance per item); `repository-map.md` (factual current implementation); `sources.json` (user-provided and discovered references); `skills.json` (applicable skills with rationale).

Capability constraint: Scout must run read-only for production code. Enforce through Harness preset/capability configuration where possible.

## Agent 2 — Planner / Architect
Consumes task + clarifications + Scout artifacts + repo instructions/skills. Must additionally inspect repository evidence and **git history** for the areas driving the plan. Search for analogous/reusable implementations before inventing new patterns.

Artifact 1: `feature-spec.md` — Problem, Goal, User/business value, Current behavior, Desired behavior, Functional requirements, Inputs/outputs, Business rules, User-visible/API behavior, Edge cases, Error handling, Compatibility requirements, Security/permissions, Performance, Observability, Out of scope, Acceptance criteria, Test scenarios, Assumptions, Open questions.

Artifact 2: `implementation-plan.md` — Relevant existing architecture, Repository evidence, Git history / architectural direction, Existing patterns, Candidate approaches with trade-offs, Selected approach and rationale, Files/symbols to change, New files/components, Data/API/control-flow/dependency changes, Migration/backward compatibility, Tests, Validation strategy, Risks, Explicit non-changes, Ordered implementation steps, Traceability.

Version both artifacts. Never silently overwrite prior versions. Keep requirements, assumptions and open questions explicitly separate.

## Agent 3 — Plan Gatekeeper
Independent review of original task + clarifications + both active plan artifacts + relevant evidence. Detect: ambiguity; unsupported assumptions; contradiction; missing requirements/tests; scope creep; task ↔ feature-spec mismatch; feature-spec ↔ implementation-plan mismatch; architecture inconsistent with repository evidence or migration direction.

Structured outcomes: `approved`; `revise_plan` with findings/evidence; `user_input_required` with blocking questions/decisions. Agent 3 does not silently rewrite the plan itself.

## Human clarification loop
When `user_input_required`:
1. create durable Attention items;
2. transition to wait state;
3. collect user answer from work9flow client;
4. append answer as user clarification;
5. re-run Planner to produce new artifact versions;
6. re-run Gatekeeper;
7. repeat until approved or iteration limit.

When Gatekeeper returns `revise_plan` without human input, route directly back to Planner with the structured findings.

## Role/model configuration
Agent roles must use role-level provider/model configuration. Allow Gatekeeper to be configured independently from Planner to reduce correlated failure.

## Testing
Fixture repositories/fake adapter outputs for deterministic tests plus at least one real Harness-backed integration flow. Cover: discovery artifact schemas; user-provided reference propagation; skill/instruction discovery; plan artifact versioning; git-history requirement in Planner input; Gatekeeper approve/revise/question outcomes; user clarification append/replan loop; iteration limit.

## Acceptance criteria
Given a task and repository, work9flow can reach `READY_FOR_IMPLEMENTATION` with: inspectable Scout evidence artifacts; versioned business feature spec; versioned technical implementation plan; independent review verdict; durable user clarification loop when needed; no production code changes performed by Scout/Planner/Gatekeeper stages.

---
**Source:** Linear BIR-61 — https://linear.app/vibeeer/issue/BIR-61/mvp-05-implement-feature-development-discovery-planning-and-plan
**Parent epic:** BIR-55
**Depends on:** BIR-58, BIR-59, BIR-60
