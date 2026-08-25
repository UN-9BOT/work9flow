// Package protocol defines the wire contract between work9flowd
// (runtime) and work9flow (TUI) and any future clients.
//
// DTOs here are JSON-serialisable and contain no DSH types, no
// workflow-internal state, and no client UI concepts. Anything in
// here is part of the public protocol.
package protocol

// Version is the runtime version string. Bump on protocol changes.
const Version = "0.1.0-mvp01"

// HealthResponse is returned by GET /v1/health.
type HealthResponse struct {
	Status  string `json:"status"`  // "ok" | "starting" | "degraded"
	Version string `json:"version"` // runtime version
	UptimeS int64  `json:"uptime_s"`
}

// VersionResponse is returned by GET /v1/version.
type VersionResponse struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Build   string `json:"build,omitempty"`
}

// RunSummary is a minimal WorkflowRun projection safe to expose to
// clients. Full domain types live in internal/runtime.
type RunSummary struct {
	ID         string `json:"id"`
	WorkflowID string `json:"workflow_id"`
	State      string `json:"state"`
	Stage      string `json:"stage,omitempty"`
	Title      string `json:"title,omitempty"`
	Pending    int    `json:"pending_attention"`
}

// RunListResponse is returned by GET /v1/runs.
type RunListResponse struct {
	Runs []RunSummary `json:"runs"`
}
