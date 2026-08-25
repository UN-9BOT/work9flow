// Package featuredev is the work9flow MVP feature-development workflow.
// All six stages drive real DSH-backed agents through
// internal/agents.Runner. The discovery / planning / plan-review
// half is owned by MVP 05; the implementation / review half is owned
// by MVP 06.
package featuredev

import (
	"context"
	"encoding/json"

	"github.com/unbot/work9flow/internal/agents"
	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/engine"
)

// Role configuration: one role per stage that runs a DSH agent.
// Provider / model wiring lands with the role configuration plumbing
// in MVP 06's follow-up. Each agent uses the DSH default model for now.
const (
	roleScout      = "scout"
	rolePlanner    = "planner"
	roleGatekeeper = "gatekeeper"
	roleImplementer = "implementer"
	roleReviewer    = "reviewer"
)

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
					// MVP 06 does not yet implement the
					// answer-and-replan loop. The stub advances to
					// IMPLEMENTING so the full workflow can reach
					// DONE without a TUI.
					return engine.StageResult{Kind: "advance"}, nil
				},
				Transition: func(_ context.Context, _ *engine.StageInput, _ engine.StageResult) (domain.RunState, error) {
					return domain.RunImplementing, nil
				},
			},
			"implementing": {
				State:    domain.RunImplementing,
				StageKey: "implementing",
				Runner:   implementerRunner(ar),
				Transition: func(_ context.Context, _ *engine.StageInput, _ engine.StageResult) (domain.RunState, error) {
					return domain.RunImplementationReview, nil
				},
			},
			"implementation_review": {
				State:    domain.RunImplementationReview,
				StageKey: "implementation_review",
				Runner:   reviewerRunner(ar),
				Transition: reviewerTransition,
			},
		},
	}
}

// scoutRunner drives the Repository Scout agent. Any non-advance
// outcome is treated as a stage failure that surfaces to the engine.
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
		case "advance":
			return engine.StageResult{Kind: "advance"}, nil
		case "approve":
			return engine.StageResult{Kind: "advance"}, nil
		case "revise":
			return engine.StageResult{Kind: "revise"}, nil
		case "wait_user":
			r := engine.StageResult{Kind: "wait_user"}
			if len(out.Questions) > 0 {
				r.Attention = &domain.Attention{
					Kind:    domain.AttentionQuestion,
					Title:   "plan-review clarification",
					Status:  domain.AttentionOpen,
					Options: out.Questions,
				}
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
// by gatekeeperRunner.
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

// implementerRunner runs the Implementer agent. Non-advance outcomes
// (e.g. "blocked_by_plan") surface to the engine for routing.
func implementerRunner(ar *agents.Runner) func(context.Context, *engine.StageInput) (engine.StageResult, error) {
	return func(ctx context.Context, in *engine.StageInput) (engine.StageResult, error) {
		out, err := ar.Run(ctx, in.Run, roleImplementer, "", agents.Instructions{
			Message: "implementer: implement the approved plan",
			Payload: taskPayload(in.Run, "implementing"),
		})
		if err != nil {
			return engine.StageResult{}, err
		}
		switch out.Kind {
		case "advance":
			return engine.StageResult{Kind: "advance"}, nil
		case "blocked_by_plan":
			return engine.StageResult{Kind: "revise"}, nil
		case "failed":
			return engine.StageResult{Kind: "failed", TerminalReason: "implementer: " + out.Kind}, nil
		default:
			return engine.StageResult{Kind: "failed", TerminalReason: "implementer: " + out.Kind}, nil
		}
	}
}

// reviewerRunner runs the Implementation Review Orchestrator agent.
// The orchestrator emits a list of Findings via review_findings and
// decides the outcome deterministically based on the most urgent
// finding class:
//
//   - no blocking findings                  -> "approve"  (DONE)
//   - any PLAN_DEFECT                        -> "revise_plan" (PLAN_REVIEW)
//   - any REQUIREMENT_AMBIGUITY              -> "wait_user"  (WAITING_FOR_USER)
//   - any IMPLEMENTATION_BUG / OUT_OF_SCOPE  -> "revise"     (IMPLEMENTING)
//
// The MVP 06 reviewer is a single agent; the full parallel fan-out
// with multiple reviewers and synthesis is deferred.
func reviewerRunner(ar *agents.Runner) func(context.Context, *engine.StageInput) (engine.StageResult, error) {
	return func(ctx context.Context, in *engine.StageInput) (engine.StageResult, error) {
		out, err := ar.Run(ctx, in.Run, roleReviewer, "", agents.Instructions{
			Message: "reviewer: review the implementation",
			Payload: taskPayload(in.Run, "implementation_review"),
		})
		if err != nil {
			return engine.StageResult{}, err
		}
		switch out.Kind {
		case "advance":
			return engine.StageResult{Kind: "advance"}, nil
		case "approve":
			return engine.StageResult{Kind: "advance"}, nil
		case "revise":
			return engine.StageResult{Kind: "revise"}, nil
		case "revise_plan":
			return engine.StageResult{Kind: "revise_plan"}, nil
		case "wait_user":
			r := engine.StageResult{Kind: "wait_user"}
			questions := blockingQuestions(out.ReviewFindings)
			if len(questions) > 0 {
				r.Attention = &domain.Attention{
					Kind:    domain.AttentionQuestion,
					Title:   "implementation-review clarification",
					Status:  domain.AttentionOpen,
					Options: questions,
				}
				if len(out.Findings) > 0 {
					r.Attention.Context = out.Findings
				}
			}
			return r, nil
		case "failed":
			return engine.StageResult{Kind: "failed", TerminalReason: "reviewer: " + out.Kind}, nil
		default:
			return engine.StageResult{Kind: "failed", TerminalReason: "reviewer: " + out.Kind}, nil
		}
	}
}

// reviewerTransition routes based on the StageResult.Kind emitted by
// reviewerRunner. The engine already enforces terminal-state handling
// for "done" / "failed" via domain.IsTerminal.
func reviewerTransition(_ context.Context, _ *engine.StageInput, r engine.StageResult) (domain.RunState, error) {
	switch r.Kind {
	case "advance":
		return domain.RunDone, nil
	case "revise":
		return domain.RunImplementing, nil
	case "revise_plan":
		return domain.RunPlanReview, nil
	case "wait_user":
		return domain.RunWaitingForUser, nil
	case "failed":
		return domain.RunFailed, nil
	default:
		return domain.RunImplementing, nil
	}
}

// blockingQuestions reduces a list of review findings to the
// "wait_user" stage's blocking-question surface.
func blockingQuestions(findings []agents.FindingPayload) []string {
	var out []string
	for _, f := range findings {
		if f.Class == domain.FindingRequirementAmbig && f.Statement != "" {
			out = append(out, f.Statement)
		}
	}
	return out
}

// taskPayload is the structured input we send to each agent. Real
// DSH agents would expand this to include Scout evidence / Planner
// artifacts / approved spec+plan versions / user clarifications; for
// MVP 05 / MVP 06 we send a minimal envelope and let the scripted
// (or real) agent react.
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

var _ = []string{roleScout, rolePlanner, roleGatekeeper, roleImplementer, roleReviewer}
