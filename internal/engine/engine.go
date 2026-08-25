// Package engine is the work9flow state machine: workflow registry,
// per-run step driver, iteration enforcement, and Attention helpers.
//
// Engine is deterministic orchestration code. It owns the workflow
// definitions, validates transitions via domain.CanTransition, calls
// the storage.Repo for durable state, and records stage/agent events.
// LLM-issued outcomes flow through StageResult and are validated by
// the engine before they can mutate state.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/storage"
)

// ErrUnknownWorkflow is returned for workflow IDs not in the registry.
var ErrUnknownWorkflow = errors.New("engine: unknown workflow")

// ErrTerminalRun is returned when Step is called on a terminal run.
var ErrTerminalRun = errors.New("engine: run is in a terminal state")

// ErrIterationLimit is returned when a stage would exceed the
// configured max iterations.
var ErrIterationLimit = errors.New("engine: iteration limit exceeded")

// Options configures an Engine.
type Options struct {
	Repo           storage.Repo
	IterationLimit int // default 5; applied per stage when a workflow has none
}

// WorkflowDef extends domain.Workflow with the runners and
// explicit transition outcomes the controller routes on. Keep this
// in Go code (not YAML) per MVP 04 design.
type WorkflowDef struct {
	domain.Workflow
	Stages map[string]StageDef
}

// StageDef is one stage's runner and the explicit next-state routing.
type StageDef struct {
	// State is the RunState the run enters when this stage starts.
	State domain.RunState
	// Runner computes a StageResult for the current state. May be
	// nil for "passive" stages (the controller just transitions).
	Runner func(ctx context.Context, s *StageInput) (StageResult, error)
	// Transition returns the next RunState given a StageResult.
	Transition func(ctx context.Context, s *StageInput, r StageResult) (domain.RunState, error)
	// StageKey is the human-friendly name recorded on WorkflowRun.Stage
	// when this stage is active.
	StageKey string
}

// StageInput is the read-only context passed to Runner / Transition.
type StageInput struct {
	Run        domain.WorkflowRun
	Iteration  int
	StageKey   string
}

// StageResult is what a runner returns. The engine routes on Kind.
type StageResult struct {
	Kind           string           // "advance" | "wait_user" | "done" | "failed" | "revise"
	NextStage      string           // StageKey for the next active stage
	TerminalState  domain.RunState  // if Kind == "done" or "failed"
	TerminalReason string
	Attention      *domain.Attention // populated when Kind == "wait_user"
}

// Engine is the work9flow controller.
type Engine struct {
	opts   Options
	mu     sync.RWMutex
	flows  map[string]*WorkflowDef // key = ID + "@" + Version
	repo   storage.Repo
	now    func() time.Time
}

// New returns a new Engine with opts.
func New(opts Option) *Engine {
	if opts.IterationLimit <= 0 {
		opts.IterationLimit = 5
	}
	return &Engine{
		opts:  opts.toOptions(),
		flows: map[string]*WorkflowDef{},
		repo:  opts.Repo,
		now:   time.Now,
	}
}

// Option is the fluent-style constructor variant; kept for future
// expansion (no functional difference from Options today).
type Option struct {
	Repo           storage.Repo
	IterationLimit int
}

func (o Option) toOptions() Options { return Options{Repo: o.Repo, IterationLimit: o.IterationLimit} }

// Repo returns the underlying storage.Repo so HTTP handlers and tests
// can read state directly when needed.
func (e *Engine) Repo() storage.Repo { return e.repo }

// RegisterWorkflow adds wf to the registry under (ID, Version).
func (e *Engine) RegisterWorkflow(wf *WorkflowDef) error {
	if wf.ID == "" {
		return errors.New("engine: workflow ID required")
	}
	key := wf.ID
	if wf.Version != "" {
		key = wf.ID + "@" + wf.Version
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, exists := e.flows[key]; exists {
		return fmt.Errorf("engine: workflow %q already registered", key)
	}
	e.flows[key] = wf
	return nil
}

// GetWorkflow looks up a registered workflow by id (or id@version).
func (e *Engine) GetWorkflow(id string) (*WorkflowDef, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if def, ok := e.flows[id]; ok {
		return def, nil
	}
	if def, ok := e.flows[id+"@v1"]; ok {
		return def, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknownWorkflow, id)
}

// CreateRunInput is the input to CreateRun.
type CreateRunInput struct {
	WorkflowID      string
	WorkflowVersion string
	RepoPath        string
	OriginalTask    string
}

// CreateRun starts a new workflow run. The run begins in the
// workflow's initial state.
func (e *Engine) CreateRun(ctx context.Context, in CreateRunInput) (domain.WorkflowRun, error) {
	wf, err := e.GetWorkflow(in.WorkflowID)
	if err != nil {
		return domain.WorkflowRun{}, err
	}
	now := e.now().UTC()
	run := domain.WorkflowRun{
		ID:              newRunID(now),
		WorkflowID:      wf.ID,
		WorkflowVersion: wf.Version,
		RepoPath:        in.RepoPath,
		OriginalTask:    in.OriginalTask,
		State:           wf.InitialState,
		Stage:           "",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if run.State == "" {
		run.State = domain.RunNew
	}
	if err := e.repo.CreateRun(ctx, run); err != nil {
		return domain.WorkflowRun{}, fmt.Errorf("engine: create run: %w", err)
	}
	if _, err := e.repo.AppendEvent(ctx, run.ID, domain.EventKindWorkflowCreated, now, nil); err != nil {
		return domain.WorkflowRun{}, err
	}
	return run, nil
}

// Step drives a run forward by one stage. It:
//   1. Reads the current run state;
//   2. Looks up the active stage's runner+transition;
//   3. Increments the stage's iteration counter (max from opts.IterationLimit);
//   4. Calls runner, then transition;
//   5. Persists the next state via storage (which re-validates
//      via domain.CanTransition) and records a stage.* event.
func (e *Engine) Step(ctx context.Context, runID string) error {
	run, err := e.repo.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if domain.IsTerminal(run.State) {
		return ErrTerminalRun
	}
	wf, err := e.GetWorkflow(run.WorkflowID)
	if err != nil {
		return err
	}
	stageKey := run.Stage
	if stageKey == "" {
		stageKey = wf.InitialStage
	}
	if stageKey == "" {
		// No active stage; nothing to step.
		return nil
	}
	st, ok := wf.Stages[stageKey]
	if !ok {
		return fmt.Errorf("engine: workflow %q has no stage %q", wf.ID, stageKey)
	}
	// If the run hasn't entered this stage's state yet, transition first.
	if st.State != "" && run.State != st.State {
		if !domain.CanTransition(run.State, st.State) {
			return fmt.Errorf("engine: cannot enter stage %q from state %q", stageKey, run.State)
		}
		if err := e.repo.UpdateRunState(ctx, runID, st.State, stageKey, ""); err != nil {
			return err
		}
		run.State = st.State
		run.Stage = stageKey
	}

	// Iteration budget.
	iter, err := e.repo.IncrementIteration(ctx, runID, stageKey)
	if err != nil {
		return err
	}
	if iter > e.opts.IterationLimit {
		_ = e.repo.UpdateRunState(ctx, runID, domain.RunFailed, stageKey, "iteration limit exceeded")
		_, _ = e.repo.AppendEvent(ctx, runID, domain.EventKindStageFailed, e.now().UTC(), mustJSON(map[string]int{"iteration": iter}))
		return ErrIterationLimit
	}

	input := &StageInput{Run: run, Iteration: iter, StageKey: stageKey}

	var result StageResult
	if st.Runner != nil {
		result, err = st.Runner(ctx, input)
		if err != nil {
			return fmt.Errorf("engine: runner %s: %w", stageKey, err)
		}
	}

	if result.Attention != nil {
		result.Attention.RunID = runID
		result.Attention.OriginatingStage = stageKey
		if result.Attention.CreatedAt.IsZero() {
			result.Attention.CreatedAt = e.now().UTC()
		}
		if err := e.repo.CreateAttention(ctx, *result.Attention); err != nil {
			return err
		}
		_, _ = e.repo.AppendEvent(ctx, runID, domain.EventKindAttentionRequired, e.now().UTC(), mustJSON(map[string]string{"attention_id": result.Attention.ID, "kind": string(result.Attention.Kind)}))
	}

	if st.Transition != nil {
		next, err := st.Transition(ctx, input, result)
		if err != nil {
			return err
		}
		if !domain.CanTransition(run.State, next) {
			return fmt.Errorf("engine: stage %s returned invalid transition %s -> %s", stageKey, run.State, next)
		}
		if domain.IsTerminal(next) {
			reason := result.TerminalReason
			if reason == "" {
				reason = string(next)
			}
			if err := e.repo.UpdateRunState(ctx, runID, next, stageKey, reason); err != nil {
				return err
			}
			evKind := domain.EventKindWorkflowCompleted
			if next == domain.RunFailed {
				evKind = domain.EventKindWorkflowFailed
			}
			if next == domain.RunCanceled {
				evKind = domain.EventKindWorkflowCanceled
			}
			_, _ = e.repo.AppendEvent(ctx, runID, evKind, e.now().UTC(), nil)
			return nil
		}
		nextStage := resolveNextStage(wf, stageKey, next, result.NextStage)
	if err := e.repo.UpdateRunState(ctx, runID, next, nextStage, ""); err != nil {
		return err
	}
	evKind := domain.EventKindStageStarted
	if result.Kind == "revise" {
		evKind = domain.EventKindStageCompleted
	}
	_, _ = e.repo.AppendEvent(ctx, runID, evKind, e.now().UTC(), nil)
	}
	return nil
}

// RaiseAttention creates an attention item on a run.
func (e *Engine) RaiseAttention(ctx context.Context, runID string, a domain.Attention) error {
	if _, err := e.repo.GetRun(ctx, runID); err != nil {
		return err
	}
	a.RunID = runID
	if a.CreatedAt.IsZero() {
		a.CreatedAt = e.now().UTC()
	}
	if a.Status == "" {
		a.Status = domain.AttentionOpen
	}
	if err := e.repo.CreateAttention(ctx, a); err != nil {
		return err
	}
	_, _ = e.repo.AppendEvent(ctx, runID, domain.EventKindAttentionRequired, e.now().UTC(), mustJSON(map[string]string{"kind": string(a.Kind)}))
	return nil
}

// AnswerTransition maps an attention answer into a state transition.
type AnswerTransition struct {
	From   domain.RunState
	To     domain.RunState
	Stage  string
}

// AnswerAttention answers an open attention item and returns the
// derived transition (if the engine can route it).
func (e *Engine) AnswerAttention(ctx context.Context, attentionID string, answer json.RawMessage) (AnswerTransition, error) {
	a, err := e.repo.GetAttention(ctx, attentionID)
	if err != nil {
		return AnswerTransition{}, err
	}
	if err := e.repo.AnswerAttention(ctx, attentionID, answer, e.now().UTC()); err != nil {
		return AnswerTransition{}, err
	}
	_, _ = e.repo.AppendEvent(ctx, a.RunID, domain.EventKindAttentionResolved, e.now().UTC(), mustJSON(map[string]string{"attention_id": attentionID}))
	run, err := e.repo.GetRun(ctx, a.RunID)
	if err != nil {
		return AnswerTransition{}, err
	}
	return AnswerTransition{From: run.State, To: run.State, Stage: run.Stage}, nil
}

// AppendClarification appends a clarification to a run's log.
func (e *Engine) AppendClarification(ctx context.Context, runID string, c domain.Clarification) error {
	return e.repo.AppendClarification(ctx, runID, c)
}

// Cancel marks the run as CANCELED with a reason. Persists a
// workflow.canceled event.
func (e *Engine) Cancel(ctx context.Context, runID, reason string) error {
	if err := e.repo.UpdateRunState(ctx, runID, domain.RunCanceled, "", reason); err != nil {
		return err
	}
	_, _ = e.repo.AppendEvent(ctx, runID, domain.EventKindWorkflowCanceled, e.now().UTC(), nil)
	return nil
}

// ListRuns returns runs in created_at DESC order.
func (e *Engine) ListRuns(ctx context.Context) ([]domain.WorkflowRun, error) {
	return e.repo.ListRuns(ctx)
}

// GetRun is a convenience wrapper.
func (e *Engine) GetRun(ctx context.Context, runID string) (domain.WorkflowRun, error) {
	return e.repo.GetRun(ctx, runID)
}


// resolveNextStage picks the stage key for the post-transition state.
// Explicit runner.NextStage wins; otherwise the engine looks up a stage
// whose StageDef.State matches the new state. Falls back to the current
// stage key (preserves previous behaviour when no mapping is found).
func resolveNextStage(wf *WorkflowDef, current string, nextState domain.RunState, runnerHint string) string {
	if runnerHint != "" {
		return runnerHint
	}
	for key, st := range wf.Stages {
		if st.State == nextState {
			return key
		}
	}
	return current
}

func newRunID(now time.Time) string {
	return fmt.Sprintf("run-%d", now.UnixNano())
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
