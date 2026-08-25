// Package featuredev is the work9flow MVP feature-development workflow.
package featuredev

import (
	"context"

	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/engine"
)

// Workflow returns the registered feature-development workflow.
func Workflow() *engine.WorkflowDef {
	advance := func(_ context.Context, _ *engine.StageInput, _ engine.StageResult) (domain.RunState, error) {
		return domain.RunPlanning, nil // placeholder; overridden per-stage
	}
	_ = advance
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
				Runner:   stubRunner,
				Transition: func(_ context.Context, _ *engine.StageInput, _ engine.StageResult) (domain.RunState, error) {
					return domain.RunPlanning, nil
				},
			},
			"planning": {
				State:    domain.RunPlanning,
				StageKey: "planning",
				Runner:   stubRunner,
				Transition: func(_ context.Context, _ *engine.StageInput, _ engine.StageResult) (domain.RunState, error) {
					return domain.RunPlanReview, nil
				},
			},
			"plan_review": {
				State:    domain.RunPlanReview,
				StageKey: "plan_review",
				Runner:   stubRunner,
				Transition: func(_ context.Context, _ *engine.StageInput, r engine.StageResult) (domain.RunState, error) {
					switch r.Kind {
					case "wait_user":
						return domain.RunWaitingForUser, nil
					case "revise":
						return domain.RunPlanning, nil
					default:
						return domain.RunImplementing, nil
					}
				},
			},
			"waiting_for_user": {
				State:    domain.RunWaitingForUser,
				StageKey: "waiting_for_user",
				Runner:   stubRunner,
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

// stubRunner is the placeholder runner used until MVP 05/06 land
// real DSH-driven agent executions.
func stubRunner(_ context.Context, _ *engine.StageInput) (engine.StageResult, error) {
	return engine.StageResult{Kind: "advance"}, nil
}
