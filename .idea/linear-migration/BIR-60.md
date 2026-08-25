# Goal
Implement the generic durable workflow controller used by all work9flow workflows. This is deterministic orchestration code, not an LLM-authored conversation loop.

## Inputs
* BIR-55 product specification;
* BIR-58 protocol/persistence contracts;
* BIR-59 Harness adapter;
* repository architecture/instructions/skills.

## Workflow definition model
A workflow definition must support:
* stable ID and version;
* initial state;
* terminal states;
* stages/states;
* stage runner/agent-role reference;
* explicit transition outcomes;
* iteration counters/limits;
* optional parallel child work handled by a stage runner;
* human-gated states;
* failure/cancel behavior.

Prefer code-defined workflows for MVP. Do not build a YAML/no-code DSL unless repository constraints make a tiny schema necessary. A visual/declarative authoring system belongs in BIR-56.

## Runtime behavior
Implement: workflow registry; create/start a run from workflow ID/version; persist every state transition; reconstruct state after runtime restart; execute one stage based on explicit input/output contracts; deterministic outcome → next-state routing; max iteration enforcement; terminal `DONE`, `FAILED`, `CANCELED` handling; stage and agent failure propagation; cancellation; safe resume from non-terminal durable states.

Do not let an LLM directly mutate the state machine or choose arbitrary next stages. Agents return structured outcomes; controller validates and routes them.

## Attention service
Implement first-class human interactions:

### `QUESTION`
Missing requirement/context. Can contain free-text question and optional suggestions.

### `DECISION`
Multiple legitimate approaches. Must carry concise options and trade-off context where provided by the stage.

### `APPROVAL`
Explicit user gate before continuing.

### `NOTIFICATION`
Non-blocking warning/info.

For blocking Attention:
1. persist Attention item;
2. persist workflow transition to `WAITING_FOR_USER` or equivalent wait state;
3. do not keep an unnecessary LLM execution alive while waiting;
4. accept answer through work9flow protocol;
5. persist answer as an event;
6. append task clarification when semantics require it;
7. resume from the workflow-defined target stage.

Original task remains immutable; clarifications are append-only.

## Agent control plumbing
Controller must be able to target active AgentRuns for: `steer`; `followup`; cancel. Persist these user actions as work9flow events before/with dispatch so history explains why an agent changed direction.

## Testing
Use fake stage runners/agents to test orchestration without model calls. Cover: normal linear transitions; revise loop; Attention wait → answer → resume; approval gate; iteration limit exhaustion; invalid outcome rejection; runtime restart during wait state; cancellation; agent failure; duplicate/replayed command safety where applicable.

## Acceptance criteria
* a synthetic workflow can run end-to-end without any Harness-specific code outside the adapter;
* wait states survive process restart;
* user answer deterministically resumes the correct stage;
* iteration limits are enforced in code;
* workflow definitions are reusable/versioned;
* all meaningful transitions/actions are represented in the persisted event stream;
* no feature-development-specific prompt/business logic is embedded in the generic engine.

---
**Source:** Linear BIR-60 — https://linear.app/vibeeer/issue/BIR-60/mvp-04-implement-generic-workflow-state-machine-registry-and-attention
**Parent epic:** BIR-55
**Depends on:** BIR-58, BIR-59
