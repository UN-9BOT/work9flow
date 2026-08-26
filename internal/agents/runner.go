// Package agents bridges work9flow stages to a DSH (DeepSeek Harness)
// HTTP endpoint. Each stage runs a single specialised agent (Scout,
// Planner, Gatekeeper, ...) by spinning up a DSH session, sending
// structured instructions, persisting the event stream as work9flow
// events, and reducing the final assistant response to an Outcome.
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
// array of the agent's final assistant response. We persist every
// entry via storage.Repo.AddArtifact so downstream stages see
// versioned artifacts.
type ArtifactPayload struct {
	Kind       domain.ArtifactKind `json:"kind"`
	Name       string              `json:"name"`
	Stage      string              `json:"stage,omitempty"`
	ContentRef string              `json:"content_ref,omitempty"`
	Content    string              `json:"content,omitempty"`
	Approved   bool                `json:"approved,omitempty"`
	Metadata   map[string]string   `json:"metadata,omitempty"`
}

// FindingPayload is one of the optional entries under the "findings"
// array of the agent's final assistant response. Reviewers emit one
// entry per observation; Runner.persistFindings routes each into
// storage.
type FindingPayload struct {
	Class     domain.FindingClass `json:"class"`
	Statement string              `json:"statement"`
	Evidence  string              `json:"evidence,omitempty"`
	Reference string              `json:"reference,omitempty"`
	Rationale string              `json:"rationale,omitempty"`
	Action    string              `json:"action,omitempty"`
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
	// Artifacts are extracted from the agent's final assistant response
	// "artifacts" array and persisted via storage.AddArtifact. The
	// engine can ignore this field; it exists so the agent contract
	// is self-describing.
	Artifacts []ArtifactPayload
	// ReviewFindings are extracted from the agent's final assistant
	// response "findings" array and persisted via storage.AddFinding.
	// Reviewers emit one entry per observation; the engine routes on
	// Class.
	ReviewFindings []FindingPayload
}

// ErrSessionIncomplete is returned when the bridge closes the Activity
// stream without a natural `run.end{reason: idle}` — i.e. the upstream
// never reached root session.status=idle inside the caller's deadline.
// Per upstream contract the activity's natural close is the root
// session going idle; we do NOT fabricate a terminal event from EOF
// or from `turn/end` / `assistant/message`.
var ErrSessionIncomplete = errors.New("agents: Activity interval ended without session.status=idle")

// Runner wraps a DSH client with work9flow persistence so stage runners
// can execute one DSH-backed agent end-to-end.
type Runner struct {
	// DSH is the typed HTTP client to runtime/dsh-bridge/. The bridge
	// owns the upstream SDK and the JSON-RPC wire details; work9flow
	// only sees the normalized surface (Health / CreateSession /
	// Run / Shutdown).
	DSH *dsh.Bridge
	// Repo persists every normalized event on the run's event log
	// and routes artifacts / findings through the durable record.
	Repo storage.Repo
	// Provider is the upstream provider route (e.g. "deepseek"). The
	// bridge pins provider+model on the initialize handshake when the
	// session is first created, so we surface it explicitly instead of
	// guessing from the model name.
	Provider string
	// DefaultModel is the upstream model route used when a per-call model
	// argument is empty. Real role routing lands with work9flow-4v1.13
	// (RoleConfig resolution); this default keeps the engine tests alive
	// without inventing per-stage models.
	DefaultModel string
	Now          func() time.Time
}

// New returns a Runner with sensible defaults. Override Now via the
// struct field after construction in tests.
func New(c *dsh.Bridge, repo storage.Repo) *Runner {
	return &Runner{
		DSH:          c,
		Repo:         repo,
		Provider:     "deepseek",
		DefaultModel: "deepseek-chat",
		Now:          time.Now,
	}
}

// Run starts a DSH session for (run, role, model), sends instructions
// as the initial followup, drives one owned Activity interval via
// dsh.Run (subscribe-before-prompt + idle-bound), persists every
// normalized event on the run's event log, and returns the reduced
// Outcome.
//
// The Outcome is extracted from the LAST upstream `assistant/message`
// event in the activity — upstream has NO invented `agent.completed`,
// no invented `turn/end` synonym for activity end. The activity's
// natural close is root `session.status=idle`, surfaced by the bridge
// as `run.end{reason=idle}`. The Runner reads the assistant message
// text as the agent contract (JSON with `outcome` plus optional
// `findings` / `questions` / `summary` / `artifacts` /
// `review_findings` fields — same as before).
func (r *Runner) Run(ctx context.Context, run domain.WorkflowRun, role, model string, instructions Instructions) (Outcome, error) {
	if r.DSH == nil {
		return Outcome{}, errors.New("agents: nil DSH client")
	}
	if r.Repo == nil {
		return Outcome{}, errors.New("agents: nil repo")
	}
	if r.Provider == "" {
		return Outcome{}, errors.New("agents: Runner.Provider not set")
	}
	provider := r.Provider
	cwd := run.RepoPath
	if cwd == "" {
		cwd = "/"
	}
	if model == "" {
		model = r.DefaultModel
	}
	if model == "" {
		return Outcome{}, errors.New("agents: Runner.Run: model required (no DefaultModel set)")
	}
	ref, err := r.DSH.CreateSession(ctx, dsh.CreateSessionRequest{
		Cwd:      cwd,
		Provider: provider,
		Model:    model,
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("agents: create session: %w", err)
	}
	sessionID := ref.ID
	if _, err := r.Repo.AppendEvent(ctx, run.ID, domain.EventKindAgentStarted, r.now(), mustJSON(map[string]string{
		"role":       role,
		"model":      model,
		"session_id": sessionID,
	})); err != nil {
		return Outcome{}, err
	}

	// Drive one owned Activity interval. The bridge handles
	// subscribe-before-prompt + await agent/inbox/spliced(messageId) +
	// close on root session.status=idle. The stream is bounded by
	// `run.end`; we no longer poll SnapshotEvents or invent
	// `agent.completed`.
	evCh, errCh := r.DSH.Run(ctx, sessionID, []dsh.ContentBlock{
		{Type: "text", Text: instructions.Message},
	})
	var collected []dsh.NormalizedEvent
	for ev := range evCh {
		collected = append(collected, ev)
	}
	select {
	case err := <-errCh:
		if err != nil {
			return Outcome{}, fmt.Errorf("agents: run: %w", err)
		}
	default:
	}

	r.persistEvents(ctx, run.ID, collected)

	final := findFinalAssistant(collected)
	if final == nil {
		return Outcome{}, ErrSessionIncomplete
	}
	out := reduceOutcome(final.Data)
	if err := r.persistArtifacts(ctx, run.ID, role, out.Artifacts); err != nil {
		return Outcome{}, err
	}
	if err := r.persistFindings(ctx, run.ID, role, out.ReviewFindings); err != nil {
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

func (r *Runner) persistEvents(ctx context.Context, runID string, raw []dsh.NormalizedEvent) {
	for _, n := range raw {
		_, _ = r.Repo.AppendEvent(ctx, runID, n.Kind, n.At, n.Data)
	}
}

// findFinalAssistant walks the activity in reverse and returns the
// LAST upstream `assistant/message` event, or nil. Per upstream
// HarnessSession.run, this is the agent's final response — the
// work9flow contract JSON (outcome / findings / etc.) is embedded in
// the assistant text. There is no upstream `agent.completed`; we
// never invent one.
func findFinalAssistant(raw []dsh.NormalizedEvent) *dsh.NormalizedEvent {
	for i := len(raw) - 1; i >= 0; i-- {
		ev := raw[i]
		if ev.Kind != domain.EventKindRawPassthrough {
			continue
		}
		if !isAssistantMessageData(ev.Data) {
			continue
		}
		return &ev
	}
	return nil
}

// isAssistantMessageData probes for an upstream `assistant/message`
// frame in the persisted Data blob. The bridge normalizes these to
// `EventKindRawPassthrough` and the original frame is preserved
// verbatim — including the upstream `type` discriminator.
func isAssistantMessageData(data json.RawMessage) bool {
	if len(data) == 0 {
		return false
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	return probe.Type == "assistant/message"
}

// reduceOutcome interprets the agent's final assistant response into
// an Outcome.
//
// The agent MUST emit a JSON object whose top-level "outcome" field is
// one of the recognised Kind values ("advance" / "approve" / "revise"
// / "wait_user" / "done" / "failed"). Anything we cannot parse becomes
// a generic "advance" so the engine does not stall on a misbehaving
// agent.
func reduceOutcome(data json.RawMessage) Outcome {
	text := extractAssistantText(data)
	if text == "" {
		return Outcome{Kind: "advance"}
	}
	var probe struct {
		Outcome   string            `json:"outcome"`
		Findings  json.RawMessage   `json:"findings"`
		Questions []string          `json:"questions"`
		Summary   string            `json:"summary"`
		Artifacts []ArtifactPayload `json:"artifacts"`
		ReviewFindings []FindingPayload `json:"review_findings"`
	}
	if err := json.Unmarshal([]byte(text), &probe); err != nil {
		return Outcome{Kind: "advance", Summary: text}
	}
	kind := probe.Outcome
	switch kind {
	case "approve", "revise", "revise_plan", "wait_user", "done", "failed", "advance", "blocked_by_plan":
	default:
		kind = "advance"
	}
	var artifacts []ArtifactPayload
	if len(probe.Artifacts) > 0 {
		artifacts = probe.Artifacts
	}
	return Outcome{
		Kind:           kind,
		Findings:       probe.Findings,
		Questions:      probe.Questions,
		Summary:        probe.Summary,
		Artifacts:      artifacts,
		ReviewFindings: probe.ReviewFindings,
	}
}

// extractAssistantText pulls the last text content block from the
// upstream `assistant/message` envelope. Mirrors upstream's
// `finalResponse` accumulator (concat text blocks in order, take the
// last one that exists — we walk in reverse for the same effect).
func extractAssistantText(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var probe struct {
		Type string `json:"type"`
		Data struct {
			Message struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return ""
	}
	if probe.Type != "assistant/message" {
		// Legacy / synthetic path: the agent pumped a raw JSON
		// outcome directly without the upstream content-block shape.
		// Treat the persisted Data as plain text.
		return string(data)
	}
	for i := len(probe.Data.Message.Content) - 1; i >= 0; i-- {
		block := probe.Data.Message.Content[i]
		if block.Type == "text" {
			return block.Text
		}
	}
	return ""
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

func (r *Runner) persistFindings(ctx context.Context, runID, role string, items []FindingPayload) error {
	for _, item := range items {
		if item.Statement == "" {
			continue
		}
		if item.Class == "" {
			item.Class = domain.FindingImplementationBug
		}
		f := domain.Finding{
			RunID:      runID,
			ReviewerID: role,
			Class:      item.Class,
			Blocking:   item.Class.IsBlocking(),
			Statement:  item.Statement,
			Evidence:   item.Evidence,
			Reference:  item.Reference,
			Rationale:  item.Rationale,
			Action:     item.Action,
			CreatedAt:  r.now(),
		}
		if err := r.Repo.AddFinding(ctx, f); err != nil {
			return fmt.Errorf("agents: persist finding %s: %w", item.Class, err)
		}
	}
	return nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
