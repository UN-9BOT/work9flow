package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/engine"
	"github.com/unbot/work9flow/internal/engine/featuredev"
	"github.com/unbot/work9flow/internal/storage"
)

func newTestEngine(t *testing.T) (*engine.Engine, storage.Repo) {
	t.Helper()
	repo, err := storage.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	eng := engine.New(engine.Option{Repo: repo})
	if err := eng.RegisterWorkflow(featuredev.Workflow()); err != nil {
		t.Fatal(err)
	}
	return eng, repo
}

func mustCreateRun(t *testing.T, eng *engine.Engine, workflowID, task string) domain.WorkflowRun {
	t.Helper()
	run, err := eng.CreateRun(context.Background(), engine.CreateRunInput{
		WorkflowID:   workflowID,
		RepoPath:     "/tmp/repo",
		OriginalTask: task,
	})
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestRegistryRegistersAndLooksUp(t *testing.T) {
	repo, _ := storage.OpenSQLite(":memory:")
	defer repo.Close()
	eng := engine.New(engine.Option{Repo: repo})
	if err := eng.RegisterWorkflow(featuredev.Workflow()); err != nil {
		t.Fatal(err)
	}
	def, err := eng.GetWorkflow("feature-development")
	if err != nil {
		t.Fatal(err)
	}
	if def.ID != "feature-development" {
		t.Errorf("ID = %q", def.ID)
	}
}

func TestRegistryRejectsDuplicate(t *testing.T) {
	repo, _ := storage.OpenSQLite(":memory:")
	defer repo.Close()
	eng := engine.New(engine.Option{Repo: repo})
	wf := featuredev.Workflow()
	if err := eng.RegisterWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	if err := eng.RegisterWorkflow(wf); err == nil {
		t.Fatal("expected duplicate registration error")
	}
}

func TestCreateRunStartsAtInitialState(t *testing.T) {
	eng, _ := newTestEngine(t)
	run := mustCreateRun(t, eng, "feature-development", "implement X")
	if run.State != domain.RunNew {
		t.Errorf("state = %q", run.State)
	}
}

func TestCreateRunUnknownWorkflowFails(t *testing.T) {
	eng, _ := newTestEngine(t)
	_, err := eng.CreateRun(context.Background(), engine.CreateRunInput{
		WorkflowID:   "nope",
		OriginalTask: "x",
	})
	if err == nil {
		t.Fatal("expected error for unknown workflow")
	}
}

func TestStepAdvancesToDiscovery(t *testing.T) {
	eng, _ := newTestEngine(t)
	run := mustCreateRun(t, eng, "feature-development", "x")
	if err := eng.Step(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := eng.Repo().GetRun(context.Background(), run.ID)
	if got.State != domain.RunPlanning {
		t.Errorf("state = %q, want PLANNING", got.State)
	}
}

func TestStepPersistsEvents(t *testing.T) {
	eng, repo := newTestEngine(t)
	run := mustCreateRun(t, eng, "feature-development", "x")
	if err := eng.Step(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	evs, _ := repo.EventsAfter(context.Background(), run.ID, 0)
	if len(evs) < 2 {
		t.Errorf("events = %d", len(evs))
	}
}

func TestStepRejectsTerminal(t *testing.T) {
	eng, repo := newTestEngine(t)
	run := mustCreateRun(t, eng, "feature-development", "x")
	_ = repo.UpdateRunState(context.Background(), run.ID, domain.RunCanceled, "", "manual")
	if err := eng.Step(context.Background(), run.ID); err == nil {
		t.Fatal("expected error stepping terminal run")
	}
}

func TestIterationLimitEnforced(t *testing.T) {
	// Register a workflow whose only stage always revises itself. With
	// IterationLimit=2, the third Step must fail with ErrIterationLimit
	// and the run must end in FAILED with reason "iteration limit exceeded".
	repo, _ := storage.OpenSQLite(":memory:")
	defer repo.Close()
	eng := engine.New(engine.Option{Repo: repo, IterationLimit: 2})
	// Three-stage loop: warmup (DISCOVERY) -> plan (PLANNING) -> revise (PLAN_REVIEW)
	// -> plan ... Each pass through "plan" increments its counter; IterationLimit=2
	// forces the third visit to "plan" to fail with ErrIterationLimit.
	loopWf := &engine.WorkflowDef{
		Workflow: domain.Workflow{
			ID:           "loop",
			Name:         "loop",
			Version:      "v1",
			InitialState: domain.RunNew,
			InitialStage: "warmup",
		},
		Stages: map[string]engine.StageDef{
			"warmup": {
				State:    domain.RunDiscovery,
				StageKey: "warmup",
				Runner: func(_ context.Context, _ *engine.StageInput) (engine.StageResult, error) {
					return engine.StageResult{Kind: "advance"}, nil
				},
				Transition: func(_ context.Context, _ *engine.StageInput, _ engine.StageResult) (domain.RunState, error) {
					return domain.RunPlanning, nil
				},
			},
			"plan": {
				State:    domain.RunPlanning,
				StageKey: "plan",
				Runner: func(_ context.Context, _ *engine.StageInput) (engine.StageResult, error) {
					return engine.StageResult{Kind: "advance"}, nil
				},
				Transition: func(_ context.Context, _ *engine.StageInput, _ engine.StageResult) (domain.RunState, error) {
					return domain.RunPlanReview, nil
				},
			},
			"revise": {
				State:    domain.RunPlanReview,
				StageKey: "revise",
				Runner: func(_ context.Context, _ *engine.StageInput) (engine.StageResult, error) {
					return engine.StageResult{Kind: "revise"}, nil
				},
				Transition: func(_ context.Context, _ *engine.StageInput, _ engine.StageResult) (domain.RunState, error) {
					return domain.RunPlanning, nil
				},
			},
		},
	}
	if err := eng.RegisterWorkflow(loopWf); err != nil {
		t.Fatal(err)
	}
	run, err := eng.CreateRun(context.Background(), engine.CreateRunInput{
		WorkflowID:   "loop",
		OriginalTask: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	var lastErr error
	for i := 0; i < 8; i++ {
		lastErr = eng.Step(context.Background(), run.ID)
		if lastErr != nil {
			break
		}
	}
	if lastErr == nil {
		t.Fatal("expected iteration limit error")
	}
	if !errors.Is(lastErr, engine.ErrIterationLimit) {
		t.Errorf("last err = %v, want ErrIterationLimit", lastErr)
	}
	got, _ := repo.GetRun(context.Background(), run.ID)
	if got.State != domain.RunFailed {
		t.Errorf("state = %q, want FAILED", got.State)
	}
	if got.TerminalReason != "iteration limit exceeded" {
		t.Errorf("reason = %q", got.TerminalReason)
	}
	if got.IterationCounters["plan"] < 2 {
		t.Errorf("iteration counter = %d, want >=2", got.IterationCounters["loop"])
	}
}

func TestAttentionLifecycleThroughEngine(t *testing.T) {
	eng, _ := newTestEngine(t)
	run := mustCreateRun(t, eng, "feature-development", "x")
	if err := eng.RaiseAttention(context.Background(), run.ID, domain.Attention{
		Kind:    domain.AttentionQuestion,
		Title:   "which DB?",
		Options: []string{"postgres", "sqlite"},
	}); err != nil {
		t.Fatal(err)
	}
	atts, _ := eng.Repo().ListAttention(context.Background(), run.ID)
	if len(atts) != 1 || atts[0].Status != domain.AttentionOpen {
		t.Errorf("attentions = %+v", atts)
	}
	if _ , err := eng.AnswerAttention(context.Background(), atts[0].ID,
		json.RawMessage(`"postgres"`)); err != nil {
		t.Fatal(err)
	}
	got, _ := eng.Repo().GetAttention(context.Background(), atts[0].ID)
	if got.Status != domain.AttentionAnswered {
		t.Errorf("status = %q", got.Status)
	}
}

func TestAppendClarificationViaEngine(t *testing.T) {
	eng, _ := newTestEngine(t)
	run := mustCreateRun(t, eng, "feature-development", "x")
	if err := eng.AppendClarification(context.Background(), run.ID, domain.Clarification{
		Body:     "user clarified",
		FromUser: true,
	}); err != nil {
		t.Fatal(err)
	}
	all, _ := eng.Repo().Clarifications(context.Background(), run.ID)
	if len(all) != 1 || all[0].Body != "user clarified" {
		t.Errorf("clarifications = %+v", all)
	}
}

func TestEngineSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "work9flow.db")

	r1, _ := storage.OpenSQLite(dbPath)
	eng1 := engine.New(engine.Option{Repo: r1})
	_ = eng1.RegisterWorkflow(featuredev.Workflow())
	run, _ := eng1.CreateRun(context.Background(), engine.CreateRunInput{
		WorkflowID: "feature-development", OriginalTask: "x",
	})
	_ = eng1.Step(context.Background(), run.ID)
	r1.Close()

	r2, _ := storage.OpenSQLite(dbPath)
	defer r2.Close()
	got, _ := r2.GetRun(context.Background(), run.ID)
	if got.State != domain.RunPlanning {
		t.Errorf("after restart state = %q, want PLANNING", got.State)
	}
	if got.OriginalTask != "x" {
		t.Errorf("OriginalTask = %q", got.OriginalTask)
	}
}

func TestCancelViaEngine(t *testing.T) {
	eng, repo := newTestEngine(t)
	run := mustCreateRun(t, eng, "feature-development", "x")
	if err := eng.Cancel(context.Background(), run.ID, "user changed mind"); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.GetRun(context.Background(), run.ID)
	if got.State != domain.RunCanceled {
		t.Errorf("state = %q", got.State)
	}
	if got.TerminalReason != "user changed mind" {
		t.Errorf("reason = %q", got.TerminalReason)
	}
}
