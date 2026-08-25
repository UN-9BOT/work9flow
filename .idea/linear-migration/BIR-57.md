# Goal
Create the minimal repository/runtime foundation for **work9flow** and lock the architectural boundaries before feature work begins.

## Context
work9flow is a local-first workflow orchestrator on top of DeepSeek Harness. The primary UI will be a Go TUI, but the TUI must remain a client. Durable orchestration lives outside the UI.

Target boundary:
* `work9flowd` runtime/controller: workflow state, registry, persistence, DSH adapter, artifacts, Attention, event normalization;
* DeepSeek Harness: agent/model/tool/session/subagent execution kernel;
* `work9flow` Go TUI: client only.

## Before implementation
1. Read all root/applicable `AGENTS.md`, `CLAUDE.md`, repo instructions and relevant skills.
2. Inspect current repository structure and package conventions.
3. Search for reusable server/event/persistence/config abstractions before creating new ones.
4. Inspect git history for active structural migrations.
5. Inspect the actual installed/current DeepSeek Harness package layout and upstream docs/source. Do not assume remembered API signatures.

## Work
* Establish package/module layout for runtime vs Go TUI vs shared protocol/schema assets.
* Add the minimum build/dev commands needed to run runtime and TUI independently.
* Add configuration loading for: repository/workspace path; model/provider role configuration; local runtime endpoint; work9flow state directory; iteration limits.
* Define dependency direction so Go UI does not import DSH-specific concepts.
* Create an initial architecture document showing process boundaries and ownership.
* Decide and document the MVP local transport. Prefer localhost HTTP JSON + WebSocket event stream unless existing architecture strongly favors another.
* Make runtime startable even before workflow behavior exists; expose a simple health/version capability.

## Deliverables
* runnable runtime skeleton;
* runnable Go TUI skeleton that can connect/check runtime health;
* configuration structure;
* architecture/process-boundary documentation;
* basic development setup docs.

## Acceptance criteria
* runtime and TUI are separate processes/components;
* terminating the TUI does not define/own workflow lifecycle;
* no DSH/Cordis types leak into the Go client contract;
* local transport decision is documented;
* project builds/tests through documented commands;
* architecture is minimal: do not implement workflow stages, Attention, or rich TUI screens yet.

## Out of scope
No Web UI, remote auth, Slack, full CLI, multiple workflows, plugin marketplace, hosted mode, or other BIR-56 post-MVP features.

---
**Source:** Linear BIR-57 — https://linear.app/vibeeer/issue/BIR-57/mvp-01-bootstrap-work9flow-runtime-and-lock-architecture-boundaries
**Parent epic:** BIR-55
