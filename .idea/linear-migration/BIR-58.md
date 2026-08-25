# Goal
Define the stable work9flow contract that sits between the runtime and all clients, and implement durable storage for workflow state/events/artifacts.

## Why this task comes first
The Go TUI, DSH adapter, workflow engine and future clients must depend on a stable work9flow model instead of Harness internals. This task creates that seam.

## Inputs / references
* BIR-55 product/MVP specification;
* architecture decisions from BIR-57;
* existing repository conventions for API schemas, persistence and eventing;
* current DSH session/event semantics only as an implementation input, not as the public contract.

## Domain model
Define at minimum: Workflow, WorkflowRun, AgentRun, Artifact, Attention, Events.

### Workflow
workflow definition ID/name/version; stage definitions and transitions; iteration limits; reusable agent-role references.

### WorkflowRun
run ID; workflow ID/version; repository/workspace context; immutable original task; current state/stage; terminal state/reason; active agent IDs; active artifact versions; clarifications; pending Attention items; iteration counters; timestamps.

### AgentRun
agent ID; role; provider/model metadata; stage/parent relationship; runtime status; Harness session/activation reference kept internal to adapter-facing data; started/completed/failed/canceled timestamps.

### Artifact
ID/type/name/version; owning run/stage/agent; path/content reference; created/replaced timestamp; provenance/metadata; active/approved marker where relevant.

### Attention
Kinds: `QUESTION`, `DECISION`, `APPROVAL`, non-blocking `NOTIFICATION`. Include blocking flag, title/context, options, originating stage/agent, status, answer, timestamps.

### Events
Stable normalized events: workflow created/started/completed/failed/canceled; stage started/completed/failed; agent started/status/completed/failed/canceled; agent tool started/completed/failed; artifact created/version-selected; attention required/resolved; user steer/followup; reconnect/replay boundaries. Every persisted event needs monotonic ordering/sequence within a run and timestamp.

## Persistence requirements
MVP is local-first. SQLite is a strong default if no existing local persistence solution, but follow repository conventions. Persist enough data to: restart work9flowd and list runs; resume a run from `WAITING_FOR_USER`; replay events to a reconnected TUI from a sequence cursor; reconstruct current state; inspect completed runs. Original task immutable. Clarifications append-only. Artifact plan versions not silently overwritten.

## Local protocol
Implement minimum stable client API: list/get runs; create run; cancel run; get run events after sequence N; get artifacts/artifact metadata; get pending Attention; answer Attention; steer/followup selected agent; live event subscription. Define explicit request/response/event schemas and version the protocol if practical.

## Testing
Deterministic tests for: event ordering; persistence/reload; append-only clarifications; artifact version retention; Attention lifecycle; replay from event cursor; invalid state mutation rejection.

## Acceptance criteria
* Go client can be generated/implemented from work9flow contracts without importing DSH packages;
* runtime restart preserves run/event/attention/artifact state;
* reconnect can request only events after last seen sequence;
* original task and prior artifact versions cannot be accidentally overwritten;
* contracts are documented with examples;
* no workflow-specific business logic is embedded in generic protocol/storage layers.

---
**Source:** Linear BIR-58 — https://linear.app/vibeeer/issue/BIR-58/mvp-02-define-work9flow-protocol-domain-model-event-log-and-durable
**Parent epic:** BIR-55
**Depends on:** BIR-57
