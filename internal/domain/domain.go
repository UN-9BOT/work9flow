// Package domain defines the work9flow value types: Workflow,
// WorkflowRun, AgentRun, Artifact, Attention and Event. These are the
// business objects the runtime, protocol and (eventually) workflow
// engine reason about.
//
// Boundaries:
//   * no transport (lives in internal/protocol, internal/runtime);
//   * no persistence (lives in internal/storage);
//   * no UI (lives in cmd/work9flow).
package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// RunState is the high-level state of a WorkflowRun. State transitions
// are validated by CanTransition; IsTerminal reports absorbing states.
type RunState string

const (
	RunNew                  RunState = "NEW"
	RunDiscovery            RunState = "DISCOVERY"
	RunPlanning             RunState = "PLANNING"
	RunPlanReview           RunState = "PLAN_REVIEW"
	RunWaitingForUser       RunState = "WAITING_FOR_USER"
	RunImplementing         RunState = "IMPLEMENTING"
	RunImplementationReview RunState = "IMPLEMENTATION_REVIEW"
	RunDone                 RunState = "DONE"
	RunFailed               RunState = "FAILED"
	RunCanceled             RunState = "CANCELED"
)

// IsTerminal returns true if s is a terminal absorbing state.
func IsTerminal(s RunState) bool {
	switch s {
	case RunDone, RunFailed, RunCanceled:
		return true
	}
	return false
}

// CanTransition validates a state machine move. Terminal states are
// absorbing; otherwise the allowed graph mirrors the MVP state model
// from BIR-55 (feature-development workflow).
func CanTransition(from, to RunState) bool {
	if IsTerminal(from) {
		return false
	}
	switch from {
	case RunNew:
		return to == RunDiscovery || to == RunCanceled || to == RunFailed
	case RunDiscovery:
		return to == RunPlanning || to == RunFailed || to == RunCanceled
	case RunPlanning:
		return to == RunPlanReview || to == RunFailed || to == RunCanceled
	case RunPlanReview:
		// revise_plan loop, user-clarification path, or move forward.
		return to == RunPlanning || to == RunWaitingForUser ||
			to == RunImplementing || to == RunFailed || to == RunCanceled
	case RunWaitingForUser:
		return to == RunImplementing || to == RunPlanning ||
			to == RunPlanReview || to == RunFailed || to == RunCanceled
	case RunImplementing:
		return to == RunImplementationReview || to == RunFailed || to == RunCanceled
	case RunImplementationReview:
		// correct-and-retry, hand back to planning, escalate, or done.
		return to == RunImplementing || to == RunPlanning ||
			to == RunPlanReview || to == RunWaitingForUser ||
			to == RunDone || to == RunFailed || to == RunCanceled
	}
	return false
}

// Workflow is a workflow definition. Runs reference it by ID/version.
type Workflow struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Version    string            `json:"version"`
	Stages     []Stage           `json:"stages"`
	Limits     map[string]int    `json:"limits"`
	AgentRoles []string          `json:"agent_roles"`
}

// Stage is one node of a workflow definition.
type Stage struct {
	Name   string   `json:"name"`
	Next   []string `json:"next"`
	Agents []string `json:"agents"`
}

// WorkflowRun is a single execution of a workflow against a repo/task.
//
// OriginalTask is immutable after creation; mutating it would break
// the protocol's promise of an audit-grade task snapshot.
type WorkflowRun struct {
	ID                     string         `json:"id"`
	WorkflowID             string         `json:"workflow_id"`
	WorkflowVersion        string         `json:"workflow_version,omitempty"`
	RepoPath               string         `json:"repo_path"`
	OriginalTask           string         `json:"original_task"`
	State                  RunState       `json:"state"`
	Stage                  string         `json:"stage,omitempty"`
	TerminalReason         string         `json:"terminal_reason,omitempty"`
	ActiveAgentIDs         []string       `json:"active_agent_ids,omitempty"`
	ActiveArtifactVersions map[string]int `json:"active_artifact_versions,omitempty"`
	IterationCounters      map[string]int `json:"iteration_counters,omitempty"`
	CreatedAt              time.Time      `json:"created_at"`
	UpdatedAt              time.Time      `json:"updated_at"`
}

// AgentStatus is the lifecycle of a single AgentRun.
type AgentStatus string

const (
	AgentPending   AgentStatus = "PENDING"
	AgentRunning   AgentStatus = "RUNNING"
	AgentCompleted AgentStatus = "COMPLETED"
	AgentFailed    AgentStatus = "FAILED"
	AgentCanceled  AgentStatus = "CANCELED"
)

// AgentRun is a single agent execution inside a workflow run.
type AgentRun struct {
	ID            string            `json:"id"`
	RunID         string            `json:"run_id"`
	Role          string            `json:"role"`
	Provider      string            `json:"provider,omitempty"`
	Model         string            `json:"model,omitempty"`
	Stage         string            `json:"stage"`
	Status        AgentStatus       `json:"status"`
	SessionRef    string            `json:"session_ref,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	StartedAt     time.Time         `json:"started_at"`
	CompletedAt   time.Time         `json:"completed_at,omitempty"`
	FailedAt      time.Time         `json:"failed_at,omitempty"`
	CanceledAt    time.Time         `json:"canceled_at,omitempty"`
	FailureReason string            `json:"failure_reason,omitempty"`
}

// ArtifactKind groups artifacts by purpose.
type ArtifactKind string

const (
	ArtifactPlan   ArtifactKind = "plan"
	ArtifactSpec   ArtifactKind = "spec"
	ArtifactReview ArtifactKind = "review"
	ArtifactOther  ArtifactKind = "other"
)

// Artifact is a versioned artifact owned by a run/stage/agent.
//
// IDs are stable per (RunID, Kind, Name); versions increment on Add.
// Prior versions are retained: Add must never overwrite prior
// versions silently.
type Artifact struct {
	ID         string            `json:"id"`
	RunID      string            `json:"run_id"`
	Kind       ArtifactKind      `json:"kind"`
	Name       string            `json:"name"`
	Version    int               `json:"version"`
	Stage      string            `json:"stage,omitempty"`
	AgentID    string            `json:"agent_id,omitempty"`
	ContentRef string            `json:"content_ref"`
	Approved   bool              `json:"approved"`
	CreatedAt  time.Time         `json:"created_at"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// RunArtifacts owns the in-memory artifact list for one run. Storage
// is responsible for persisting it; this type enforces monotonic
// versioning and active-pointer semantics.
type RunArtifacts struct {
	RunID  string
	items  []Artifact
	latest map[string]int // key = kind + "|" + name; value = index into items
}

// NewRunArtifacts returns an empty RunArtifacts for runID.
func NewRunArtifacts(runID string) *RunArtifacts {
	return &RunArtifacts{
		RunID:  runID,
		latest: map[string]int{},
	}
}

func artifactKey(k ArtifactKind, name string) string {
	return string(k) + "|" + name
}

// Add assigns ID/version/timestamps if blank and appends a new entry.
// Version is monotonic per (Kind, Name). The new entry becomes Active
// for that key. Prior versions are retained in items.
func (r *RunArtifacts) Add(a Artifact) Artifact {
	if a.RunID == "" {
		a.RunID = r.RunID
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	key := artifactKey(a.Kind, a.Name)
	last := 0
	for _, it := range r.items {
		if it.Kind == a.Kind && it.Name == a.Name && it.Version > last {
			last = it.Version
		}
	}
	a.Version = last + 1
	if a.ID == "" {
		a.ID = fmt.Sprintf("art-%s-%s-v%d", r.RunID, a.Kind, a.Version)
	}
	r.items = append(r.items, a)
	r.latest[key] = len(r.items) - 1
	return a
}

// History returns all artifact versions for (kind, name) in version order.
func (r *RunArtifacts) History(k ArtifactKind, name string) []Artifact {
	var out []Artifact
	for _, it := range r.items {
		if it.Kind == k && it.Name == name {
			out = append(out, it)
		}
	}
	// items are appended in insertion order; insertion order == version
	// order because versions are monotonic.
	return out
}

// Active returns the latest artifact for (kind, name) or nil.
func (r *RunArtifacts) Active(k ArtifactKind, name string) *Artifact {
	key := artifactKey(k, name)
	idx, ok := r.latest[key]
	if !ok {
		return nil
	}
	cp := r.items[idx]
	return &cp
}

// Clarification is an append-only entry in a run's clarification log.
type Clarification struct {
	Seq      int64     `json:"seq"`
	Body     string    `json:"body"`
	FromUser bool      `json:"from_user"`
	At       time.Time `json:"at"`
}

// RunClarifications holds append-only clarifications for one run.
type RunClarifications struct {
	RunID string
	Items []Clarification
}

// NewRunClarifications returns an empty log for runID.
func NewRunClarifications(runID string) *RunClarifications {
	return &RunClarifications{RunID: runID}
}

// Add appends a clarification; seq is assigned if zero.
func (r *RunClarifications) Add(c Clarification) Clarification {
	if c.At.IsZero() {
		c.At = time.Now().UTC()
	}
	c.Seq = int64(len(r.Items) + 1)
	r.Items = append(r.Items, c)
	return c
}

// All returns all clarifications in insertion order.
func (r *RunClarifications) All() []Clarification {
	out := make([]Clarification, len(r.Items))
	copy(out, r.Items)
	return out
}

// AttentionKind classifies an attention item. Question/Decision/Approval
// are blocking; Notification is non-blocking.
type AttentionKind string

const (
	AttentionQuestion     AttentionKind = "QUESTION"
	AttentionDecision     AttentionKind = "DECISION"
	AttentionApproval     AttentionKind = "APPROVAL"
	AttentionNotification AttentionKind = "NOTIFICATION"
)

// IsBlocking reports whether k must be resolved before the run proceeds.
func (k AttentionKind) IsBlocking() bool {
	switch k {
	case AttentionQuestion, AttentionDecision, AttentionApproval:
		return true
	}
	return false
}

// AttentionStatus is the lifecycle of an attention item.
type AttentionStatus string

const (
	AttentionOpen     AttentionStatus = "OPEN"
	AttentionAnswered AttentionStatus = "ANSWERED"
	AttentionCanceled AttentionStatus = "CANCELED"
)

// CanTransitionAttention validates an attention status move.
// Once answered or canceled, an attention item does not reopen.
func CanTransitionAttention(from, to AttentionStatus) bool {
	if from == AttentionOpen {
		return to == AttentionAnswered || to == AttentionCanceled
	}
	return false
}

// Attention is a single item raised during a run.
type Attention struct {
	ID               string          `json:"id"`
	RunID            string          `json:"run_id"`
	Kind             AttentionKind   `json:"kind"`
	Status           AttentionStatus `json:"status"`
	Blocking         bool            `json:"blocking"`
	Title            string          `json:"title"`
	Context          json.RawMessage `json:"context,omitempty"`
	Options          []string        `json:"options,omitempty"`
	OriginatingStage string          `json:"originating_stage,omitempty"`
	OriginatingAgent string          `json:"originating_agent,omitempty"`
	Answer           json.RawMessage `json:"answer,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	AnsweredAt       time.Time       `json:"answered_at,omitempty"`
}

// EventKind is a stable, normalized event label. DSH-internal event
// names are translated into these by the DSH adapter.
type EventKind string

const (
	EventKindWorkflowCreated         EventKind = "workflow.created"
	EventKindWorkflowStarted         EventKind = "workflow.started"
	EventKindWorkflowCompleted       EventKind = "workflow.completed"
	EventKindWorkflowFailed          EventKind = "workflow.failed"
	EventKindWorkflowCanceled        EventKind = "workflow.canceled"
	EventKindStageStarted            EventKind = "stage.started"
	EventKindStageCompleted          EventKind = "stage.completed"
	EventKindStageFailed             EventKind = "stage.failed"
	EventKindAgentStarted            EventKind = "agent.started"
	EventKindAgentStatus             EventKind = "agent.status"
	EventKindAgentCompleted          EventKind = "agent.completed"
	EventKindAgentFailed             EventKind = "agent.failed"
	EventKindAgentCanceled           EventKind = "agent.canceled"
	EventKindToolStarted             EventKind = "tool.started"
	EventKindToolCompleted           EventKind = "tool.completed"
	EventKindToolFailed              EventKind = "tool.failed"
	EventKindArtifactCreated         EventKind = "artifact.created"
	EventKindArtifactVersionSelected EventKind = "artifact.version_selected"
	EventKindAttentionRequired       EventKind = "attention.required"
	EventKindAttentionResolved       EventKind = "attention.resolved"
	EventKindSteerSent               EventKind = "user.steer"
	EventKindFollowupSent            EventKind = "user.followup"
)

// Event is one entry in a run's append-only event log.
//
// Seq is monotonic within a run starting from 1. PrevSeq is the seq of
// the event directly preceding this one (0 for the first event). It
// exists so a client can detect gaps in delivery.
type Event struct {
	RunID   string          `json:"run_id"`
	Seq     int64           `json:"seq"`
	PrevSeq int64           `json:"prev_seq"`
	Kind    EventKind       `json:"kind"`
	At      time.Time       `json:"at"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// EventLog is an in-memory append-only event log for one run.
// Storage is responsible for persisting it.
type EventLog struct {
	RunID   string
	entries []Event
}

// NewEventLog returns an empty log for runID.
func NewEventLog(runID string) *EventLog {
	return &EventLog{RunID: runID}
}

// Append assigns Seq/PrevSeq, stamps At if zero, and appends.
// Returns the persisted event (with assigned Seq).
func (l *EventLog) Append(kind EventKind, at time.Time, data json.RawMessage) Event {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	prev := int64(0)
	if n := len(l.entries); n > 0 {
		prev = l.entries[n-1].Seq
	}
	e := Event{
		RunID:   l.RunID,
		Seq:     int64(len(l.entries) + 1),
		PrevSeq: prev,
		Kind:    kind,
		At:      at,
		Data:    data,
	}
	l.entries = append(l.entries, e)
	return e
}

// Events returns a copy of the log in order.
func (l *EventLog) Events() []Event {
	out := make([]Event, len(l.entries))
	copy(out, l.entries)
	return out
}

// After returns events with Seq > from in order. Used to resume a
// disconnected subscriber from its last seen cursor.
func (l *EventLog) After(from int64) []Event {
	var out []Event
	for _, e := range l.entries {
		if e.Seq > from {
			out = append(out, e)
		}
	}
	return out
}
