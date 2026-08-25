# Goal
Build the primary MVP user interface: a Go TUI that acts as a thin client to `work9flowd` and provides a cockpit for running workflows, inspecting live agents, answering workflow questions, and intervening in active agent sessions.

## Architectural rule
The TUI must not own workflow execution/state. It subscribes to the work9flow protocol, renders state/events, and sends commands. Closing/restarting the TUI must not terminate or erase a running workflow unless the user explicitly sends cancel.

## Inputs
* work9flow protocol/domain/event contracts from BIR-58;
* live agent interaction semantics from BIR-59;
* workflow/Attention semantics from BIR-60;
* BIR-55 UX requirements.

Do not import DeepSeek Harness/Cordis packages or duplicate DSH event semantics in the Go UI.

## MVP screens / views

### 1. Runs dashboard
Workflow run ID; workflow type/version; task/title summary; current stage/status; running/waiting/done/failed indication; number of active agents; pending Attention indicator. Allow selecting a run and starting a new `feature-development` run.

### 2. Workflow detail
Ordered stages and current state, including loops/iterations. For current/past stages show agent status and important artifact/Attention indicators.

### 3. Agent list + agent inspector
Role; provider/model; parent/stage; status; start/end time; current/recent operational activity; tool calls/results summaries; emitted artifacts; explicit user steer/followup history. Provide an `attach`-style focused view. Do not attempt to display hidden chain-of-thought.

### 4. Live activity stream
Subscribe to normalized work9flow events over the configured live channel. Render readable activity: reading/searching files; git commands; tests/commands; tool failures; stage transitions; agent start/finish; artifacts created; user interventions. Allow opening a raw/details view for an event.

### 5. Artifacts
Discovery breadcrumbs/repository map/sources/skills; feature spec versions; implementation plan versions; review findings/summaries; implementation result metadata/diffs. Clearly mark active/approved plan versions.

### 6. Attention inbox / blocking questions
Free-text `QUESTION` answers; choosing `DECISION` options; explicit `APPROVAL` accept/reject/request-changes; non-blocking notifications. Submitting an answer must call runtime protocol and display the resulting workflow resume/state transition.

### 7. Agent interaction
`steer` while running; `followup`; cancel agent/run with confirmation. Make steer vs followup visibly distinct.

## Reconnect/replay
Client keeps last seen event sequence per attached run/stream. On reconnect: fetch durable current state; request missed events after last seen sequence; replay them in order; resume live subscription without duplicating visible state/events. The user should be able to stop the TUI, reopen it, and inspect the same running/waiting/completed workflow.

## Suggested interaction model
Keyboard-first. Preserve concepts: runs list; workflow/stage pane; agents pane; live activity/inspector pane; Attention indicator/inbox; artifacts/diff view; steer/followup actions. Do not over-invest in theme/plugin customization in MVP.

## Testing
Go unit/component tests and protocol fixtures: event ordering/render-state reducer; reconnect replay without duplicates; Attention form/action mapping; steer/followup command mapping; run/agent selection and status changes; error states when runtime unavailable/reconnects. Add at least one local integration path against the real runtime.

## Acceptance criteria
A user can perform the BIR-55 MVP supervision flow entirely from the TUI: start a feature-development run; see stages and active agents; inspect live tool/activity events; open generated artifacts; attach to a particular agent; steer/followup that agent; answer blocking Attention items; cancel when required; close/reopen TUI and continue observing the same durable run.

## Out of scope
No Web UI, Slack/Telegram/email, VS Code extension, remote multi-user auth, visual workflow editor or full first-class CLI. Those belong to BIR-56.

---
**Source:** Linear BIR-63 — https://linear.app/vibeeer/issue/BIR-63/mvp-07-build-go-tui-for-workflow-supervision-agent-inspection-and-live
**Parent epic:** BIR-55
**Depends on:** BIR-58, BIR-59, BIR-60
