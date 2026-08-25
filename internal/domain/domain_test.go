// Package domain defines the work9flow value types: Workflow,
// WorkflowRun, AgentRun, Artifact, Attention and Event. These are the
// business objects the runtime, protocol and (eventually) workflow
// engine reason about. The package holds no transport, persistence or
// UI concerns — those live in internal/protocol, internal/storage and
// the TUI respectively.
package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func TestRunStateTransitions(t *testing.T) {
	cases := []struct {
		from, to RunState
		ok       bool
	}{
		{RunNew, RunDiscovery, true},
		{RunDiscovery, RunPlanning, true},
		{RunPlanning, RunPlanReview, true},
		{RunPlanReview, RunWaitingForUser, true},
		{RunPlanReview, RunPlanning, true}, // revise_plan loop
		{RunWaitingForUser, RunImplementing, true},
		{RunImplementing, RunImplementationReview, true},
		{RunImplementationReview, RunImplementing, true},
		{RunImplementationReview, RunDone, true},
		// Terminal states are absorbing.
		{RunDone, RunDiscovery, false},
		{RunFailed, RunDiscovery, false},
		{RunCanceled, RunImplementing, false},
		// Cannot skip stages.
		{RunNew, RunImplementing, false},
		{RunDiscovery, RunDone, false},
	}
	for _, tc := range cases {
		if got := CanTransition(tc.from, tc.to); got != tc.ok {
			t.Errorf("CanTransition(%q -> %q) = %v, want %v", tc.from, tc.to, got, tc.ok)
		}
	}
}

func TestIsTerminalRunState(t *testing.T) {
	for _, s := range []RunState{RunDone, RunFailed, RunCanceled} {
		if !IsTerminal(s) {
			t.Errorf("IsTerminal(%q) = false", s)
		}
	}
	for _, s := range []RunState{RunNew, RunDiscovery, RunPlanning, RunPlanReview, RunWaitingForUser, RunImplementing, RunImplementationReview} {
		if IsTerminal(s) {
			t.Errorf("IsTerminal(%q) = true", s)
		}
	}
}

func TestAttentionLifecycle(t *testing.T) {
	cases := []struct {
		from, to AttentionStatus
		ok       bool
	}{
		{AttentionOpen, AttentionAnswered, true},
		{AttentionOpen, AttentionCanceled, true},
		{AttentionAnswered, AttentionOpen, false},
		{AttentionCanceled, AttentionAnswered, false},
	}
	for _, tc := range cases {
		if got := CanTransitionAttention(tc.from, tc.to); got != tc.ok {
			t.Errorf("CanTransitionAttention(%q -> %q) = %v, want %v", tc.from, tc.to, got, tc.ok)
		}
	}
}

func TestEventSequenceMonotonic(t *testing.T) {
	log := NewEventLog("run-1")
	e1 := log.Append(EventKindWorkflowCreated, time.Unix(1, 0).UTC(), json.RawMessage(`{}`))
	e2 := log.Append(EventKindStageStarted, time.Unix(2, 0).UTC(), json.RawMessage(`{}`))
	e3 := log.Append(EventKindAgentStarted, time.Unix(3, 0).UTC(), json.RawMessage(`{}`))
	if e1.Seq != 1 || e2.Seq != 2 || e3.Seq != 3 {
		t.Fatalf("Seq = %d,%d,%d; want 1,2,3", e1.Seq, e2.Seq, e3.Seq)
	}
	if e1.RunID != "run-1" {
		t.Errorf("RunID = %q", e1.RunID)
	}
	if e2.PrevSeq != 1 {
		t.Errorf("e2.PrevSeq = %d, want 1", e2.PrevSeq)
	}
}

func TestEventLogEvents(t *testing.T) {
	log := NewEventLog("r")
	log.Append(EventKindWorkflowCreated, time.Unix(1, 0).UTC(), nil)
	log.Append(EventKindStageStarted, time.Unix(2, 0).UTC(), nil)
	log.Append(EventKindAgentStarted, time.Unix(3, 0).UTC(), nil)
	got := log.Events()
	if len(got) != 3 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].Seq != 1 || got[2].Seq != 3 {
		t.Errorf("ordering broken")
	}
}

func TestEventLogAfter(t *testing.T) {
	log := NewEventLog("r")
	for i := 0; i < 5; i++ {
		log.Append(EventKindWorkflowCreated, time.Unix(int64(i+1), 0).UTC(), nil)
	}
	after := log.After(2)
	if len(after) != 3 {
		t.Fatalf("After(2) len = %d, want 3", len(after))
	}
	if after[0].Seq != 3 {
		t.Errorf("first.Seq = %d, want 3", after[0].Seq)
	}
}

func TestArtifactVersionMonotonic(t *testing.T) {
	run := NewRunArtifacts("run-1")
	a := run.Add(Artifact{Kind: ArtifactPlan, Name: "feature-spec", CreatedAt: time.Unix(1, 0).UTC()})
	b := run.Add(Artifact{Kind: ArtifactPlan, Name: "feature-spec", CreatedAt: time.Unix(2, 0).UTC()})
	if a.Version != 1 || b.Version != 2 {
		t.Fatalf("versions = %d,%d; want 1,2", a.Version, b.Version)
	}
	history := run.History(ArtifactPlan, "feature-spec")
	if len(history) != 2 {
		t.Fatalf("history len = %d", len(history))
	}
}

func TestArtifactActive(t *testing.T) {
	run := NewRunArtifacts("run-1")
	_ = run.Add(Artifact{Kind: ArtifactPlan, Name: "feature-spec", CreatedAt: time.Unix(1, 0).UTC()})
	b := run.Add(Artifact{Kind: ArtifactPlan, Name: "feature-spec", CreatedAt: time.Unix(2, 0).UTC()})
	active := run.Active(ArtifactPlan, "feature-spec")
	if active == nil || active.Version != b.Version {
		t.Fatalf("Active = %v, want v%d", active, b.Version)
	}
}

func TestClarificationsAppendOnly(t *testing.T) {
	run := NewRunClarifications("run-1")
	c1 := run.Add(Clarification{Body: "first", At: time.Unix(1, 0).UTC()})
	c2 := run.Add(Clarification{Body: "second", At: time.Unix(2, 0).UTC()})
	all := run.All()
	if len(all) != 2 {
		t.Fatalf("len = %d", len(all))
	}
	if c1.Seq != 1 || c2.Seq != 2 {
		t.Errorf("seqs = %d,%d; want 1,2", c1.Seq, c2.Seq)
	}
	if all[0].Body != "first" || all[1].Body != "second" {
		t.Errorf("ordering broken")
	}
}

func TestWorkflowRunJSONRoundTrip(t *testing.T) {
	in := WorkflowRun{
		ID:           "run-1",
		WorkflowID:   "feature-development",
		RepoPath:     "/tmp/repo",
		OriginalTask: "implement X",
		State:        RunPlanning,
		Stage:        "planning",
		CreatedAt:    time.Unix(1, 0).UTC(),
		UpdatedAt:    time.Unix(2, 0).UTC(),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out WorkflowRun
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ID != in.ID || out.OriginalTask != in.OriginalTask || out.State != in.State {
		t.Errorf("round trip mismatch: %+v vs %+v", out, in)
	}
}

func TestEventJSONRoundTrip(t *testing.T) {
	in := Event{
		RunID: "run-1",
		Seq:   42,
		Kind:  EventKindStageStarted,
		At:    time.Unix(100, 0).UTC(),
		Data:  json.RawMessage(`{"stage":"planning"}`),
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Event
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Seq != 42 || out.Kind != EventKindStageStarted || string(out.Data) != `{"stage":"planning"}` {
		t.Errorf("mismatch: %+v", out)
	}
}

func TestAttentionKindIsBlocking(t *testing.T) {
	for _, k := range []AttentionKind{AttentionQuestion, AttentionDecision, AttentionApproval} {
		if !k.IsBlocking() {
			t.Errorf("%s should be blocking", k)
		}
	}
	if AttentionNotification.IsBlocking() {
		t.Errorf("notification must not be blocking")
	}
}
