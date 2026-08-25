// Package agents bridges work9flow stages to a DSH (DeepSeek Harness)
// HTTP endpoint. Each stage runs a single specialised agent (Scout,
// Planner, Gatekeeper, ...) by spinning up a DSH session, sending
// structured instructions, persisting the event stream as work9flow
// events, and reducing the final agent.completed event to an Outcome.
//
// The package is engine-agnostic. The engine wires a *Runner into a
// StageDef.Runner closure; runner.Run returns Outcome and the stage's
// Transition fn maps it to the next RunState.
package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/dsh"
	"github.com/unbot/work9flow/internal/storage"
)

// ArtifactPayload is one of the optional entries under the "artifacts"
// array of an agent.completed event. We persist every entry via
// storage.Repo.AddArtifact so downstream stages see versioned artifacts.
type ArtifactPayload struct {
	Kind       domain.ArtifactKind `json:"kind"`
	Name       string              `json:"name"`
	Stage      string              `json:"stage,omitempty"`
	ContentRef string              `json:"content_ref,omitempty"`
	Content    string              `json:"content,omitempty"`
	Approved   bool                `json:"approved,omitempty"`
	Metadata   map[string]string   `json:"metadata,omitempty"`
}

// Outcome is the high-level verdict of one agent run.
type Outcome struct {
	// Kind is the canonical agent verdict. Recognised values:
	//   "advance"   — produce next artifact(s) and move on (Scout, Planner).
	//   "approve"   — Gatekeeper sign-off; ready for implementation.
	//   "revise"    — Gatekeeper wants a Planner revision; engine loops back.
	//   "wait_user" — Gatekeeper (or any agent) needs human input; engine
	//                 creates Attention items from the Questions.
	//   "done"      — terminal success.
	//   "failed"    — terminal failure.
	Kind string
	// Findings is the agent's free-form review/notes (Gatekeeper on revise).
	Findings json.RawMessage
	// Questions are blocking clarifications (Gatekeeper on wait_user).
	Questions []string
	// Summary is a one-line human description recorded on the workflow event.
	Summary string
	// Artifacts are extracted from the agent.completed "artifacts" array
	// and persisted via storage.AddArtifact. The engine can ignore this
	// field; it exists so the agent contract is self-describing.
	Artifacts []ArtifactPayload
}

// ErrSessionIncomplete is returned when DSH never emits agent.completed
// within the configured poll budget.
var ErrSessionIncomplete = errors.New("agents: session did not complete")

// Runner wraps a DSH client with work9flow persistence so stage runners
// can execute one DSH-backed agent end-to-end.
type Runner struct {
	DSH          *dsh.Client
	Repo         storage.Repo
	Now          func() time.Time
	PollInterval time.Duration
	PollBudget   time.Duration
}

// New returns a Runner with sensible defaults. Override Now / intervals
// in tests via the struct fields after construction.
func New(c *dsh.Client, repo storage.Repo) *Runner {
	return &Runner{
		DSH:          c,
		Repo:         repo,
		Now:          time.Now,
		PollInterval: 50 * time.Millisecond,
		PollBudget:   5 * time.Second,
	}
}

// Run starts a DSH session for (run, role, model), sends instructions
// as the initial followup, polls the event stream until agent.completed
// arrives (or the budget is exhausted), persists every normalised event
// on the run's event log, and returns the reduced Outcome.
func (r *Runner) Run(ctx context.Context, run domain.WorkflowRun, role, model string, instructions Instructions) (Outcome, error) {
	if r.DSH == nil {
		return Outcome{}, errors.New("agents: nil DSH client")
	}
	if r.Repo == nil {
		return Outcome{}, errors.New("agents: nil repo")
	}
	sessionID, err := r.DSH.CreateSession(ctx, dsh.SessionRequest{Role: role, Model: model})
	if err != nil {
		return Outcome{}, fmt.Errorf("agents: create session: %w", err)
	}
	if _, err := r.Repo.AppendEvent(ctx, run.ID, domain.EventKindAgentStarted, r.now(), mustJSON(map[string]string{
		"role":       role,
		"model":      model,
		"session_id": sessionID,
	})); err != nil {
		return Outcome{}, err
	}
	if err := r.DSH.Followup(ctx, sessionID, dsh.FollowupRequest{
		Message: instructions.Message,
		Data:    instructions.Payload,
	}); err != nil {
		return Outcome{}, fmt.Errorf("agents: followup: %w", err)
	}

	raw, err := r.pollEvents(ctx, sessionID)
	if err != nil {
		return Outcome{}, err
	}
	r.persistEvents(ctx, run.ID, sessionID, raw)

	final := findCompleted(raw)
	if final == nil {
		return Outcome{}, ErrSessionIncomplete
	}
	out := reduceOutcome(final.Data)
	if err := r.persistArtifacts(ctx, run.ID, role, out.Artifacts); err != nil {
		return Outcome{}, err
	}
	if out.Summary != "" {
		_, _ = r.Repo.AppendEvent(ctx, run.ID, domain.EventKindAgentCompleted, r.now(),
			mustJSON(map[string]string{"role": role, "session_id": sessionID, "summary": out.Summary}))
	} else {
		_, _ = r.Repo.AppendEvent(ctx, run.ID, domain.EventKindAgentCompleted, r.now(),
			mustJSON(map[string]string{"role": role, "session_id": sessionID}))
	}
	return out, nil
}

// Instructions is the structured payload sent to the agent's first turn.
// Message is the human-readable brief; Payload is the JSON data block
// DSH echoes back on each event (used by tests to script outcomes).
type Instructions struct {
	Message string
	Payload json.RawMessage
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// pollEvents drives the DSH Events endpoint in a loop until either the
// session emits agent.completed or the budget expires. The first call
// typically returns immediately; subsequent calls re-read the stream
// while the session is alive.
func (r *Runner) pollEvents(ctx context.Context, sessionID string) ([]dsh.RawEvent, error) {
	interval := r.PollInterval
	if interval <= 0 {
		interval = 50 * time.Millisecond
	}
	budget := r.PollBudget
	if budget <= 0 {
		budget = 5 * time.Second
	}
	deadline := time.Now().Add(budget)
	for {
		evs, err := r.DSH.Events(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("agents: events: %w", err)
		}
		for _, e := range evs {
			if e.Kind == "agent.completed" {
				return evs, nil
			}
		}
		if time.Now().After(deadline) {
			return nil, ErrSessionIncomplete
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (r *Runner) persistEvents(ctx context.Context, runID, sessionID string, raw []dsh.RawEvent) {
	for _, n := range dsh.Normalize(sessionID, raw) {
		_, _ = r.Repo.AppendEvent(ctx, runID, n.Kind, n.At, n.Data)
	}
}

// findCompleted returns the final agent.completed event, or nil.
func findCompleted(raw []dsh.RawEvent) *dsh.RawEvent {
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i].Kind == "agent.completed" {
			return &raw[i]
		}
	}
	return nil
}

// reduceOutcome interprets the agent.completed event data into an Outcome.
//
// DSH session agents MUST emit a JSON object on agent.completed whose
// top-level "outcome" field is one of the recognised Kind values
// ("advance" / "approve" / "revise" / "wait_user" / "done" / "failed").
// Anything we cannot parse becomes a generic "advance" so the engine
// does not stall on a misbehaving agent.
func reduceOutcome(data json.RawMessage) Outcome {
	if len(data) == 0 {
		return Outcome{Kind: "advance"}
	}
	var probe struct {
		Outcome   string            `json:"outcome"`
		Findings  json.RawMessage   `json:"findings"`
		Questions []string          `json:"questions"`
		Summary   string            `json:"summary"`
		Artifacts []ArtifactPayload `json:"artifacts"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return Outcome{Kind: "advance", Summary: string(data)}
	}
	kind := probe.Outcome
	switch kind {
	case "approve", "revise", "wait_user", "done", "failed", "advance":
	default:
		kind = "advance"
	}
	var artifacts []ArtifactPayload
	if len(probe.Artifacts) > 0 {
		artifacts = probe.Artifacts
	}
	return Outcome{
		Kind:      kind,
		Findings:  probe.Findings,
		Questions: probe.Questions,
		Summary:   probe.Summary,
		Artifacts: artifacts,
	}
}

// persistArtifacts writes each declared ArtifactPayload via
// storage.AddArtifact so the durable record reflects the agent's output.
// A blank ContentRef defaults to the content (inline form); ContentRef
// wins when both are present so callers can store off-runway blobs.
func (r *Runner) persistArtifacts(ctx context.Context, runID, role string, items []ArtifactPayload) error {
	for _, item := range items {
		if item.Name == "" {
			continue
		}
		if item.Kind == "" {
			item.Kind = domain.ArtifactOther
		}
		ref := item.ContentRef
		if ref == "" {
			ref = item.Content
		}
		if ref == "" {
			ref = fmt.Sprintf("inline://%s/%s", role, item.Name)
		}
		a := &domain.Artifact{
			RunID:      runID,
			Kind:       item.Kind,
			Name:       item.Name,
			Stage:      item.Stage,
			AgentID:    role,
			ContentRef: ref,
			Approved:   item.Approved,
			Metadata:   item.Metadata,
			CreatedAt:  r.now(),
		}
		if err := r.Repo.AddArtifact(ctx, a); err != nil {
			return fmt.Errorf("agents: persist artifact %s/%s: %w", item.Kind, item.Name, err)
		}
	}
	return nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
