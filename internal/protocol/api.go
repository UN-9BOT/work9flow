// Package protocol defines the wire contract between work9flowd
// (runtime) and work9flow (TUI) and any future clients.
//
// DTOs here are JSON-serialisable, contain no DSH types, no
// workflow-internal state, and no client UI concepts. Anything in
// here is part of the public protocol.
package protocol

import (
	"encoding/json"
	"time"

	"github.com/unbot/work9flow/internal/domain"
)

// Version is the runtime version string. Bump on protocol changes.
const Version = "0.2.0-mvp02"

// HealthResponse is returned by GET /v1/health.
type HealthResponse struct {
	Status  string `json:"status"` // "ok" | "starting" | "degraded"
	Version string `json:"version"`
	UptimeS int64  `json:"uptime_s"`
}

// VersionResponse is returned by GET /v1/version.
type VersionResponse struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Build   string `json:"build,omitempty"`
}

// RunSummary is a minimal WorkflowRun projection for list views.
type RunSummary struct {
	ID         string `json:"id"`
	WorkflowID string `json:"workflow_id"`
	State      string `json:"state"`
	Stage      string `json:"stage,omitempty"`
	Title      string `json:"title,omitempty"`
	Pending    int    `json:"pending_attention"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
}

// RunListResponse is returned by GET /v1/runs.
type RunListResponse struct {
	Runs []RunSummary `json:"runs"`
}

// RunDetail is the full client-facing projection of a WorkflowRun.
// It embeds the underlying run plus derived counters so the TUI
// never has to make multiple round-trips to render a screen.
type RunDetail struct {
	ID                     string            `json:"id"`
	WorkflowID             string            `json:"workflow_id"`
	WorkflowVersion        string            `json:"workflow_version,omitempty"`
	RepoPath               string            `json:"repo_path"`
	OriginalTask           string            `json:"original_task"`
	State                  string            `json:"state"`
	Stage                  string            `json:"stage,omitempty"`
	TerminalReason         string            `json:"terminal_reason,omitempty"`
	ActiveAgentIDs         []string          `json:"active_agent_ids,omitempty"`
	ActiveArtifactVersions map[string]int    `json:"active_artifact_versions,omitempty"`
	IterationCounters      map[string]int    `json:"iteration_counters,omitempty"`
	PendingAttention       int               `json:"pending_attention"`
	EventCount             int               `json:"event_count"`
	ArtifactCount          int               `json:"artifact_count"`
	CreatedAt              int64             `json:"created_at"`
	UpdatedAt              int64             `json:"updated_at"`
}

// RunCreateRequest is the body of POST /v1/runs.
type RunCreateRequest struct {
	WorkflowID      string `json:"workflow_id"`
	WorkflowVersion string `json:"workflow_version,omitempty"`
	RepoPath        string `json:"repo_path"`
	OriginalTask    string `json:"original_task"`
}

// RunCreateResponse is returned by POST /v1/runs.
type RunCreateResponse struct {
	Run RunDetail `json:"run"`
}

// RunGetResponse is returned by GET /v1/runs/{id}.
type RunGetResponse struct {
	Run RunDetail `json:"run"`
}

// EventDTO is the wire shape of a single event.
type EventDTO struct {
	RunID   string          `json:"run_id"`
	Seq     int64           `json:"seq"`
	PrevSeq int64           `json:"prev_seq"`
	Kind    string          `json:"kind"`
	At      int64           `json:"at"` // unix seconds
	Data    json.RawMessage `json:"data,omitempty"`
}

// EventListResponse is returned by GET /v1/runs/{id}/events?after=N.
type EventListResponse struct {
	Events []EventDTO `json:"events"`
	// LatestSeq is the highest Seq present in Events. The client
	// passes it back as `after` on the next poll to skip already
	// delivered events.
	LatestSeq int64 `json:"latest_seq"`
}

// ArtifactDTO is the wire shape of a single artifact.
type ArtifactDTO struct {
	ID         string            `json:"id"`
	RunID      string            `json:"run_id"`
	Kind       string            `json:"kind"`
	Name       string            `json:"name"`
	Version    int               `json:"version"`
	Stage      string            `json:"stage,omitempty"`
	AgentID    string            `json:"agent_id,omitempty"`
	ContentRef string            `json:"content_ref"`
	Approved   bool              `json:"approved"`
	CreatedAt  int64             `json:"created_at"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// ArtifactListResponse is returned by GET /v1/runs/{id}/artifacts.
type ArtifactListResponse struct {
	Artifacts []ArtifactDTO `json:"artifacts"`
}

// AttentionDTO is the wire shape of a single attention item.
type AttentionDTO struct {
	ID               string          `json:"id"`
	RunID            string          `json:"run_id"`
	Kind             string          `json:"kind"`
	Status           string          `json:"status"`
	Blocking         bool            `json:"blocking"`
	Title            string          `json:"title"`
	Context          json.RawMessage `json:"context,omitempty"`
	Options          []string        `json:"options,omitempty"`
	OriginatingStage string          `json:"originating_stage,omitempty"`
	OriginatingAgent string          `json:"originating_agent,omitempty"`
	Answer           json.RawMessage `json:"answer,omitempty"`
	CreatedAt        int64           `json:"created_at"`
	AnsweredAt       int64           `json:"answered_at,omitempty"`
}

// AttentionListResponse is returned by GET /v1/runs/{id}/attentions.
type AttentionListResponse struct {
	Attentions []AttentionDTO `json:"attentions"`
}

// AttentionAnswerRequest is the body of POST /v1/attentions/{id}/answer.
type AttentionAnswerRequest struct {
	Answer json.RawMessage `json:"answer"`
}

// AttentionAnswerResponse is returned by POST /v1/attentions/{id}/answer.
type AttentionAnswerResponse struct {
	Attention AttentionDTO `json:"attention"`
}

// SteerRequest is the body of POST /v1/runs/{id}/steer. Steer is
// injected into the currently active turn; if no agent is active the
// request fails with 409 Conflict.
type SteerRequest struct {
	AgentID string          `json:"agent_id"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// FollowupRequest is the body of POST /v1/runs/{id}/followup.
// Followup is queued as the agent's next turn.
type FollowupRequest struct {
	AgentID string          `json:"agent_id"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// SteerFollowupResponse is returned by both steer and followup.
type SteerFollowupResponse struct {
	EventSeq int64 `json:"event_seq"`
}

// ErrorResponse is the canonical error envelope.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// ---------- converters ----------

// FromRun converts a domain.WorkflowRun into its wire RunDetail.
// Counts are passed in (computed by the storage layer) so this stays
// a pure mapping.
func FromRun(r domain.WorkflowRun, pendingAttn, eventCount, artifactCount int) RunDetail {
	return RunDetail{
		ID:                     r.ID,
		WorkflowID:             r.WorkflowID,
		WorkflowVersion:        r.WorkflowVersion,
		RepoPath:               r.RepoPath,
		OriginalTask:           r.OriginalTask,
		State:                  string(r.State),
		Stage:                  r.Stage,
		TerminalReason:         r.TerminalReason,
		ActiveAgentIDs:         r.ActiveAgentIDs,
		ActiveArtifactVersions: r.ActiveArtifactVersions,
		IterationCounters:      r.IterationCounters,
		PendingAttention:       pendingAttn,
		EventCount:             eventCount,
		ArtifactCount:          artifactCount,
		CreatedAt:              r.CreatedAt.Unix(),
		UpdatedAt:              r.UpdatedAt.Unix(),
	}
}

// SummaryFromRun builds a RunSummary from a WorkflowRun with a count.
func SummaryFromRun(r domain.WorkflowRun, pendingAttn int) RunSummary {
	return RunSummary{
		ID:         r.ID,
		WorkflowID: r.WorkflowID,
		State:      string(r.State),
		Stage:      r.Stage,
		Title:      r.OriginalTask,
		Pending:    pendingAttn,
		CreatedAt:  r.CreatedAt.Unix(),
		UpdatedAt:  r.UpdatedAt.Unix(),
	}
}

// FromEvent converts a domain.Event into its wire EventDTO.
func FromEvent(e domain.Event) EventDTO {
	return EventDTO{
		RunID:   e.RunID,
		Seq:     e.Seq,
		PrevSeq: e.PrevSeq,
		Kind:    string(e.Kind),
		At:      e.At.Unix(),
		Data:    e.Data,
	}
}

// FromArtifact converts a domain.Artifact into its wire ArtifactDTO.
func FromArtifact(a domain.Artifact) ArtifactDTO {
	return ArtifactDTO{
		ID:         a.ID,
		RunID:      a.RunID,
		Kind:       string(a.Kind),
		Name:       a.Name,
		Version:    a.Version,
		Stage:      a.Stage,
		AgentID:    a.AgentID,
		ContentRef: a.ContentRef,
		Approved:   a.Approved,
		CreatedAt:  a.CreatedAt.Unix(),
		Metadata:   a.Metadata,
	}
}

// FromAttention converts a domain.Attention into its wire AttentionDTO.
func FromAttention(a domain.Attention) AttentionDTO {
	dto := AttentionDTO{
		ID:               a.ID,
		RunID:            a.RunID,
		Kind:             string(a.Kind),
		Status:           string(a.Status),
		Blocking:         a.Blocking,
		Title:            a.Title,
		Context:          a.Context,
		Options:          a.Options,
		OriginatingStage: a.OriginatingStage,
		OriginatingAgent: a.OriginatingAgent,
		Answer:           a.Answer,
		CreatedAt:        a.CreatedAt.Unix(),
	}
	if !a.AnsweredAt.IsZero() {
		dto.AnsweredAt = a.AnsweredAt.Unix()
	}
	return dto
}

// AtToTime is a small helper that maps the wire unix-seconds back to
// time.Time for tests.
func AtToTime(unixSec int64) time.Time {
	return time.Unix(unixSec, 0).UTC()
}
