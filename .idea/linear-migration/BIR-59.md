# Goal
Implement the adapter between work9flow and the actual DeepSeek Harness runtime so the rest of work9flow can create/control agents and consume normalized live events without depending on DSH internals.

## Required research before coding
* Read BIR-55 and BIR-58 contracts.
* Read repository instructions/skills.
* Inspect the exact DeepSeek Harness version/package source used by the project.
* Verify current APIs for sessions, agent lifecycle, events, subagents/workflows, `steer`, `followup`, cancellation and resume.
* Inspect upstream docs/source where the installed package is unclear.
* Record important DSH version assumptions in adapter docs/tests.

Do not implement from prior conversation memory alone.

## Adapter responsibilities
Expose a work9flow-facing interface capable of:
* create/start an agent session with role/model/preset/context;
* observe lifecycle/status;
* subscribe to session/agent/workflow events;
* normalize tool call start/result/failure events;
* normalize model/user-visible output events useful to the TUI;
* send `steer` to a running agent;
* send `followup` as a queued/next turn;
* cancel an agent/session;
* resume/reconnect to a persisted Harness session where supported/needed;
* launch continuable subagents/review workers needed later;
* retain provider/model/session identifiers as adapter metadata without leaking raw DSH types into public protocol.

## Event normalization
Map Harness events to work9flow events from BIR-58. Preserve enough metadata to inspect an agent operationally: agent/session/stage identity; timestamps/order; tool name; safe arguments or summary; result summary/status; parent/child relationship; model/provider metadata; artifacts when emitted/known. Do not depend on hidden chain-of-thought.

## Agent interaction semantics
Implement and test distinct commands:
* `steer`: guidance for a currently running agent, applied at the next supported model-step boundary;
* `followup`: normal next user turn / continuation;
* runtime-internal context injection only if needed by later workflow implementation; keep separate from user-visible steer/followup.

The adapter must return explicit errors when the target agent/session cannot accept the requested interaction; do not silently drop messages.

## Capability / role setup
Provide the minimal mechanism for later tasks to choose role-specific Harness configuration/presets and provider/model. The Scout role must be able to run read-only when configured that way; Implementer will later receive write/test capabilities.

## Testing
Deterministic adapter tests/mocks plus at least one integration smoke path against the real Harness runtime where feasible. Cover: create/run/complete lifecycle; event normalization/order; tool event mapping; steer while running; followup continuation; cancellation; reconnect/resume behavior; child/subagent identity propagation.

## Acceptance criteria
* work9flow core can control a Harness-backed agent only through adapter interfaces;
* Go/public protocol contains no raw DSH/Cordis type dependency;
* live tool/activity events reach the normalized event stream;
* steer and followup work with distinct semantics;
* adapter behavior is documented with DSH version assumptions;
* failures are explicit and persisted/observable by the controller.

---
**Source:** Linear BIR-59 — https://linear.app/vibeeer/issue/BIR-59/mvp-03-integrate-deepseek-harness-behind-the-work9flow-adapter
**Parent epic:** BIR-55
**Depends on:** BIR-58
