// Package featuredev is the work9flow MVP feature-development workflow.
// Stages 1-3 (Discovery, Planning, PlanReview) drive real DSH-backed
// agents through internal/agents.Runner. The remaining stages stay
// stubbed until MVP 06 lands.
package featuredev

import (
	"context"
	"encoding/json"

	"github.com/unbot/work9flow/internal/agents"
	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/engine"
)

// Role configuration: one role per stage that runs a DSH agent.
// Provider / model wiring lands in MVP 06 — for now each agent uses
// the default model the DSH server is configured with.
const (
	roleScout       = "scout"
	rolePlanner     = "planner"
	roleGatekeeper  = "gatekeeper"
)

// scoutArtifacts is the contract Scout must satisfy: four evidence
// files captured in a single agent.completed payload.
var scoutArtifacts = []string{
	"breadcrumbs.json",
	"repository-map.md",
	"sources.json",
	"skills.json",
}

// plannerArtifacts is what Planner produces: a versioned business spec
// and a versioned technical plan. AddArtifact handles monotonic versioning
// per (kind, name).
var plannerArtifacts = []string{
	"feature-spec.md",
	"implementation-plan.md",
}

// Workflow returns the registered feature-development workflow. The
// agents.Runner is injected so the same workflow definition can be
// reused with mock DSH servers in tests.
func Workflow(ar *agents.Runner) *engine.WorkflowDef {
	if ar == nil {
		panic("featuredev: nil agents.Runner")
	}
	return &engine.WorkflowDef{
		Workflow: domain.Workflow{
			ID:             "feature-development",
			Name:           "feature-development",
			Version:        "v1",
			InitialState:   domain.RunNew,
			InitialStage:   "discovery",
			TerminalStates: []domain.RunState{domain.RunDone, domain.RunFailed, domain.RunCanceled},
			Limits:         map[string]int{"default": 5},
		},
		Stages: map[string]engine.StageDef{
			"discovery": {
				State:    domain.RunDiscovery,
				StageKey: "discovery",
				Runner:   scoutRunner(ar),
				Transition: func(_ context.Context, _ *engine.StageInput, _ engine.StageResult) (domain.RunState, error) {
					return domain.RunPlanning, nil
				},
			},
			"planning": {
				State:    domain.RunPlanning,
				StageKey: "planning",
				Runner:   plannerRunner(ar),
				Transition: func(_ context.Context, _ *engine.StageInput, _ engine.StageResult) (domain.RunState, error) {
					return domain.RunPlanReview, nil
				},
			},
			"plan_review": {
				State:    domain.RunPlanReview,
				StageKey: "plan_review",
				Runner:   gatekeeperRunner(ar),
				Transition: gatekeeperTransition,
			},
			"waiting_for_user": {
				State:    domain.RunWaitingForUser,
				StageKey: "waiting_for_user",
				Runner: func(_ context.Context, _ *engine.StageInput) (engine.StageResult, error) {
					return engine.StageResult{Kind: "wait_user"}, nil
				},
				Transition: func(_ context.Context, _ *engine.StageInput, _ engine.StageResult) (domain.RunState, error) {
					return domain.RunImplementing, nil
				},
			},
			"implementing": {
				State:    domain.RunImplementing,
				StageKey: "implementing",
				Runner:   stubRunner,
				Transition: func(_ context.Context, _ *engine.StageInput, _ engine.StageResult) (domain.RunState, error) {
					return domain.RunImplementationReview, nil
				},
			},
			"implementation_review": {
				State:    domain.RunImplementationReview,
				StageKey: "implementation_review",
				Runner:   stubRunner,
				Transition: func(_ context.Context, _ *engine.StageInput, r engine.StageResult) (domain.RunState, error) {
					switch r.Kind {
					case "revise":
						return domain.RunImplementing, nil
					case "wait_user":
						return domain.RunWaitingForUser, nil
					case "done":
						return domain.RunDone, nil
					default:
						return domain.RunImplementing, nil
					}
				},
			},
		},
	}
}

// scoutRunner drives the Repository Scout agent. It expects the
// scriptable DSH mock (or a real Scout) to emit agent.completed with
// outcome="advance" and an "artifacts" array carrying the four
// evidence files. Any non-advance outcome is treated as a stage
// failure that surfaces to the engine as Kind="failed".
func scoutRunner(ar *agents.Runner) func(context.Context, *engine.StageInput) (engine.StageResult, error) {
	return func(ctx context.Context, in *engine.StageInput) (engine.StageResult, error) {
		out, err := ar.Run(ctx, in.Run, roleScout, "", agents.Instructions{
			Message: "scout: gather evidence",
			Payload: taskPayload(in.Run, "discovery"),
		})
		if err != nil {
			return engine.StageResult{}, err
		}
		if out.Kind != "advance" {
			return engine.StageResult{Kind: "failed", TerminalReason: "scout: " + out.Kind}, nil
		}
		return engine.StageResult{Kind: "advance"}, nil
	}
}

// plannerRunner runs the Planner / Architect agent and produces a
// new version of feature-spec.md and implementation-plan.md.
func plannerRunner(ar *agents.Runner) func(context.Context, *engine.StageInput) (engine.StageResult, error) {
	return func(ctx context.Context, in *engine.StageInput) (engine.StageResult, error) {
		out, err := ar.Run(ctx, in.Run, rolePlanner, "", agents.Instructions{
			Message: "planner: produce spec + plan",
			Payload: taskPayload(in.Run, "planning"),
		})
		if err != nil {
			return engine.StageResult{}, err
		}
		if out.Kind != "advance" {
			return engine.StageResult{Kind: "failed", TerminalReason: "planner: " + out.Kind}, nil
		}
		return engine.StageResult{Kind: "advance"}, nil
	}
}

// gatekeeperRunner runs the Plan Gatekeeper agent. The agent decides
// "approve" / "revise" / "wait_user"; the runner maps that directly
// onto engine.StageResult so the engine can route.
func gatekeeperRunner(ar *agents.Runner) func(context.Context, *engine.StageInput) (engine.StageResult, error) {
	return func(ctx context.Context, in *engine.StageInput) (engine.StageResult, error) {
		out, err := ar.Run(ctx, in.Run, roleGatekeeper, "", agents.Instructions{
			Message: "gatekeeper: review plan",
			Payload: taskPayload(in.Run, "plan_review"),
		})
		if err != nil {
			return engine.StageResult{}, err
		}
		switch out.Kind {
		case "approve":
			return engine.StageResult{Kind: "advance"}, nil
		case "revise":
			return engine.StageResult{Kind: "revise"}, nil
		case "wait_user":
			r := engine.StageResult{Kind: "wait_user"}
			if len(out.Questions) > 0 {
				r.Attention = &domain.Attention{
					Kind:   domain.AttentionQuestion,
					Title:  "plan-review clarification",
					Status: domain.AttentionOpen,
				}
				r.Attention.Options = out.Questions
				if len(out.Findings) > 0 {
					r.Attention.Context = out.Findings
				}
			}
			return r, nil
		default:
			return engine.StageResult{Kind: "failed", TerminalReason: "gatekeeper: " + out.Kind}, nil
		}
	}
}

// gatekeeperTransition routes based on the StageResult.Kind emitted
// by gatekeeperRunner. "advance" (approve) -> WAITING_FOR_USER.
// "revise" -> PLANNING (Planner is re-entered). "wait_user" ->
// WAITING_FOR_USER (engine handles Attention via the stage result).
// "failed" -> FAILED (terminal; engine marks the run terminal).
func gatekeeperTransition(_ context.Context, _ *engine.StageInput, r engine.StageResult) (domain.RunState, error) {
	switch r.Kind {
	case "advance":
		return domain.RunWaitingForUser, nil
	case "revise":
		return domain.RunPlanning, nil
	case "wait_user":
		return domain.RunWaitingForUser, nil
	case "failed":
		return domain.RunFailed, nil
	default:
		return domain.RunPlanning, nil
	}
}

// taskPayload is the structured input we send to each agent. Real
// DSH agents would expand this to include Scout evidence / Planner
// artifacts / user clarifications; for MVP 05 we send a minimal
// envelope and let the scripted (or real) agent react.
func taskPayload(run domain.WorkflowRun, stage string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{
		"run_id":        run.ID,
		"workflow_id":   run.WorkflowID,
		"stage":         stage,
		"original_task": run.OriginalTask,
		"repo_path":     run.RepoPath,
	})
	return b
}

// stubRunner is the placeholder runner for stages that MVP 06 will
// replace (implementing, implementation_review, waiting_for_user).
func stubRunner(_ context.Context, _ *engine.StageInput) (engine.StageResult, error) {
	return engine.StageResult{Kind: "advance"}, nil
}

// Compile-time witness that we expose the same role constants the
// tests rely on.
var _ = []string{roleScout, rolePlanner, roleGatekeeper}
var _ = scoutArtifacts
var _ = plannerArtifacts
