# work9flow — MVP

## Product goal

Build **work9flow**: a local-first orchestration layer on top of DeepSeek Harness for running durable, multi-stage AI engineering workflows and supervising them from a Go TUI.

The first MVP workflow is `feature-development`: repository discovery → planning → plan review / user clarification loop → implementation → parallel implementation review → correction loop → done.

The core product is not a single coding agent. It is a workflow runtime that coordinates multiple specialized agents, models/providers, artifacts, human gates, loops, and durable state.

## Core architectural decision

The TUI must **not own orchestration**. Split the system into:

1. `work9flowd` / runtime — durable workflow controller, workflow registry, DSH adapter, run state, artifacts, attention items, event normalization.
2. DeepSeek Harness — execution kernel for agent sessions, model/tool calls, subagents, sandbox/tools, session/event stream, steer/followup.
3. `work9flow` Go TUI — thin interactive client that subscribes to work9flow state/events and sends commands.

The TUI may attach/detach/restart without losing an active workflow. Workflow state must live in the runtime, not in terminal UI memory.

For MVP, use a local stable work9flow protocol (localhost HTTP JSON + WebSocket event stream is preferred unless repository constraints strongly favor another local transport). Do not expose raw DSH/Cordis types as the public client contract; normalize them behind a DSH adapter because Harness APIs are still evolving.

## MVP scope

### 1. Generic workflow runtime

A workflow is a versioned state machine composed of reusable agent roles/stages. Runtime responsibilities:

* workflow registry;
* create/run/resume/cancel workflow runs;
* durable run state;
* deterministic stage transitions;
* loops with iteration limits;
* per-stage inputs/outputs;
* artifact references/versioning;
* human `QUESTION`, `DECISION`, `APPROVAL` gates;
* per-role model/provider policy;
* event log and replay;
* ability to attach to a running agent;
* ability to send `steer` during an active turn and `followup` as the next turn;
* failure propagation and terminal states.

LLMs reason; normal code owns state, routing, limits, permissions, versioning, and invariants.

### 2. First workflow: `feature-development`

State model:

`NEW → DISCOVERY → PLANNING → PLAN_REVIEW → {WAITING_FOR_USER | PLANNING | IMPLEMENTING} → IMPLEMENTATION_REVIEW → {IMPLEMENTING | PLANNING/PLAN_REVIEW | WAITING_FOR_USER | DONE}`

Include explicit max iteration limits for planning/review/implementation loops so agents cannot debate forever.

### 3. Agent roles

#### Agent 1 — Repository Scout
Goal: collect evidence, not design a solution. Inspects user-provided references, repository entry points, tests/config/docs, related symbols, repo instructions, and skills. Produces breadcrumbs, repository map, sources, skills. Must not propose architecture or edit production code.

#### Agent 2 — Planner / Architect
Consumes task + clarifications + Scout artifacts + repo instructions + git history. Produces two versioned artifacts:
- `feature-spec.md` (Problem, Goal, Requirements, Acceptance criteria, etc.)
- `implementation-plan.md` (Architecture, Repository evidence, Selected approach, Files to change, Tests, Ordered steps, Traceability)

#### Agent 3 — Plan Gatekeeper
Independent reviewer of task + plans. Detects ambiguity, unsupported assumptions, contradictions, missing requirements, mismatches between task/plan/spec. Outcomes: `approved`, `revise_plan`, `user_input_required`. Does not silently redesign.

Review repeats until pass or iteration limit.

### 4. Artifacts are the protocol between agents
Versioned artifacts (feature-spec.vN, implementation-plan.vN). Original task immutable. Clarifications append-only.

### 5. Human interaction / Attention
First-class workflow events: `QUESTION`, `DECISION`, `APPROVAL`, `NOTIFICATION`. Blocking attention transitions to `WAITING_FOR_USER`.

### 6. Live observability and interaction
Normalize DSH/Harness events into stable work9flow event model. TUI allows drilling workflow → stage → agent and supports `steer`, `followup`, cancel, answer Attention, replay after reconnect.

## Model/provider policy
Per-role provider/model configuration. Provider-specific behavior stays behind adapters.

## Repository implementation rules for every subtask
Read AGENTS.md/CLAUDE.md, inspect repo structure, search for reusable code, inspect git history, verify DSH APIs against actual installed version, keep DSH types behind adapter boundary, add deterministic tests, update docs.

## MVP acceptance criteria
A user can:
1. start `feature-development` against a repository and task;
2. watch stages/agents live from the Go TUI;
3. inspect agent operational activity, tool calls/results and artifacts;
4. send steer/followup to the selected active agent;
5. receive and answer blocking questions/decisions/approvals;
6. detach/restart the TUI and reconnect to the same durable run;
7. see Agent 1 discovery artifacts and versioned Agent 2 plan artifacts;
8. run plan-review/user clarification loops;
9. approve/reach implementation, run review fan-out/fan-in, route findings, and loop correctly;
10. end in durable `DONE` or explicit failure/canceled state with an inspectable event history.

## Explicitly not in MVP (see BIR-56)
Web UI, Slack/Telegram/email, IDE integrations, remote authenticated API, cloud deployment, full first-class CLI, workflow visual editor, marketplace, advanced workflows, scheduling, RBAC, cost dashboards, broad provider matrix.

## Definition of done
Complete `feature-development` workflow runnable end-to-end from the TUI with durable state, real Harness-backed agents, user intervention, plan/implementation loops, parallel review, reconnect/replay, deterministic tests on the workflow controller, and documented local setup.

---
**Source:** Linear BIR-55 — https://linear.app/vibeeer/issue/BIR-55/work9flow
**Subtasks:** BIR-57, BIR-58, BIR-59, BIR-60, BIR-61, BIR-62, BIR-63
