# work9flow — Post-MVP extensions

This epic contains deliberate extensions that must not expand the MVP scope in BIR-55.

## Entry condition

Start only after the BIR-55 MVP is complete enough to run the `feature-development` workflow end-to-end with durable state, real Harness-backed agents, Go TUI observability/control, human attention gates, review loops, and reconnect/replay.

## Candidate expansion tracks

### Additional workflows
* bug investigation / reproduce → hypotheses → parallel investigators → root-cause synthesis → fix → regression verification;
* refactoring workflow with behavior baseline and incremental gated changes;
* security audit workflow with specialized parallel reviewers;
* dependency upgrade workflow;
* migration workflow;
* architecture review / research-only workflows;
* quick-fix workflow with reduced planning gates.

### Additional clients and notification channels
* full CLI as first-class UX/API client;
* Web UI;
* VS Code/IDE integration;
* Slack/Telegram/email notifications and Attention responses.

### Remote/team mode
* authenticated remote API;
* multi-user workflow control;
* team RBAC and permissions;
* centralized audit/history;
* hosted/cloud deployment and remote workers.

### Workflow authoring/productization
* declarative workflow DSL where appropriate;
* visual/no-code workflow editor;
* workflow templates/version management;
* plugin/workflow distribution and marketplace concepts;
* richer scheduling/automation/condition watches.

### Advanced model/runtime capabilities
* broad provider matrix and provider fallback;
* policy-based dynamic model routing;
* cost/token budgets and dashboards;
* model quality/cost evaluation;
* advanced sandbox backends and remote execution;
* cross-run reusable knowledge/indexing where justified.

## Scope rule
When an MVP task encounters a desirable capability from this list, do not silently add it to BIR-55. Record it here (or create a child issue under this epic) unless it is strictly required to satisfy an existing BIR-55 acceptance criterion.

---
**Source:** Linear BIR-56 — https://linear.app/vibeeer/issue/BIR-56/work9flow-post-mvp-extensions
**Depends on:** BIR-55 (MVP completion)
