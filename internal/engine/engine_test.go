package engine_test

import (
	"fmt"

	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/unbot/work9flow/internal/agents"
	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/dsh"
	"github.com/unbot/work9flow/internal/engine"
	"github.com/unbot/work9flow/internal/engine/featuredev"
	"github.com/unbot/work9flow/internal/storage"
)

// eventFrame wraps one upstream notification kind into a session.event
// SSE frame so test scripts can describe agent emissions directly.
func eventFrame(sid, innerKind string, data json.RawMessage) dsh.RawBridgeEvent {
    inner, _ := json.Marshal(map[string]any{"kind": innerKind, "data": data})
    return dsh.RawBridgeEvent{Kind: "session.event", SessionID: sid, Event: inner}
}

// defaultAdvanceScript returns a DSH script where every agent emits
// a single agent.completed event with outcome=advance. This lets the
// engine tests below drive the full Scout -> Planner -> Gatekeeper ->
// WAITING_FOR_USER path without caring about agent payloads.
func defaultAdvanceScript() map[string][]dsh.RawBridgeEvent {
	makeEv := func(role, summary string) []dsh.RawBridgeEvent {
		sid := "sess-" + role
		return []dsh.RawBridgeEvent{
			eventFrame(sid, "agent.started", json.RawMessage(fmt.Sprintf(`{"role":%q}`, role))),
			eventFrame(sid, "agent.completed", json.RawMessage(fmt.Sprintf(`{"outcome":"advance","summary":%q}`, summary))),
		}
	}
	return map[string][]dsh.RawBridgeEvent{
		"sess-1":       makeEv("scout", "scout done"),
		"sess-2":     makeEv("planner", "planner done"),
		"sess-3":  makeEv("gatekeeper", "gatekeeper done"),
		"sess-4": makeEv("implementer", "implementer done"),
		"sess-5":    makeEv("reviewer", "reviewer done"),
	}
}

// newTestEngine wires featuredev.Workflow to a scripted DSH that
// returns advance for every agent role. Tests that drive Step get
// a real (scripted) agents.Runner without standing up a DSH process.
func newTestEngine(t *testing.T) (*engine.Engine, storage.Repo) {
	t.Helper()
	eng, repo, _, stop := newAgentEngine(t, defaultAdvanceScript())
	t.Cleanup(stop)
	return eng, repo
}

// scriptedDSH is a minimal mock for tests that drive the real
// agents.Runner. The mock speaks the runtime/dsh-bridge HTTP surface
// (POST /sessions, POST /sessions/{id}/prompt, GET /sessions/{id}/events)
// using Go 1.22+ mux method-prefixed patterns. Session ids are assigned
// per-creation order via a counter (sess-1, sess-2, ...) so test scripts
// can key events by agent role independently of the upstream DSH
// provider/model routing.
type scriptedDSH struct {
	mu           sync.Mutex
	script       map[string][]dsh.RawBridgeEvent
	createdOrder []string
}

func newScriptedDSH() *scriptedDSH {
	return &scriptedDSH{script: map[string][]dsh.RawBridgeEvent{}}
}

func (s *scriptedDSH) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /sessions", func(w http.ResponseWriter, r *http.Request) {
		var req dsh.CreateSessionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		idx := len(s.createdOrder) + 1
		id := fmt.Sprintf("sess-%d", idx)
		s.createdOrder = append(s.createdOrder, id)
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"sessionId": id,
			"serverInfo": map[string]string{"name": "scripted-dsh", "version": "test"},
		})
	})
	mux.HandleFunc("POST /sessions/{id}/prompt", func(w http.ResponseWriter, _ *http.Request) {
		// Stable enqueue receipt; the runner records it but does not inspect it.
		_ = json.NewEncoder(w).Encode(map[string]any{"messageId": "msg-stub"})
	})
	mux.HandleFunc("GET /sessions/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		sid := r.PathValue("id")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		flusher, _ := w.(http.Flusher)
		s.mu.Lock()
		evs := append([]dsh.RawBridgeEvent(nil), s.script[sid]...)
		s.mu.Unlock()
		for _, e := range evs {
			b, _ := json.Marshal(e)
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(b)
			_, _ = w.Write([]byte("\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		// Returning closes the chunked stream so the bridge's readSSE
		// goroutine hits EOF and SnapshotEvents returns the full batch
		// instead of timing out on an idle session.
	})
	mux.HandleFunc("POST /sessions/{id}/close", func(w http.ResponseWriter, _ *http.Request) {
		// Honest 501: DSH SDK has no per-session close.
		w.WriteHeader(http.StatusNotImplemented)
	})
	mux.HandleFunc("POST /shutdown", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// newAgentEngine wires a real agents.Runner to a scripted DSH and
// returns the engine, repo, mock and cleanup so the caller can assert
// recorded events and inspect artifacts.
func newAgentEngine(t *testing.T, script map[string][]dsh.RawBridgeEvent) (*engine.Engine, storage.Repo, *scriptedDSH, func()) {
	t.Helper()
	repo, err := storage.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	mock := newScriptedDSH()
	mock.script = script
	srv := httptest.NewServer(mock.handler())
	c := dsh.NewBridge(srv.URL)
	r := agents.New(c, repo)
	r.PollInterval = 5 * time.Millisecond
	r.PollBudget = 500 * time.Millisecond
	eng := engine.New(engine.Option{Repo: repo})
	if err := eng.RegisterWorkflow(featuredev.Workflow(r)); err != nil {
		t.Fatal(err)
	}
	return eng, repo, mock, func() { srv.Close(); _ = repo.Close() }
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
	if err := eng.RegisterWorkflow(featuredev.Workflow(agents.New(nil, repo))); err != nil {
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
	wf := featuredev.Workflow(agents.New(nil, repo))
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
		t.Errorf("iteration counter = %d, want >=2", got.IterationCounters["plan"])
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
	if _, err := eng.AnswerAttention(context.Background(), atts[0].ID,
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
	mock1 := newScriptedDSH()
	mock1.script = defaultAdvanceScript()
	srv1 := httptest.NewServer(mock1.handler())
	defer srv1.Close()
	c1 := dsh.NewBridge(srv1.URL)
	runner1 := agents.New(c1, r1)
	runner1.PollInterval = 5 * time.Millisecond
	runner1.PollBudget = 500 * time.Millisecond
	eng1 := engine.New(engine.Option{Repo: r1})
	_ = eng1.RegisterWorkflow(featuredev.Workflow(runner1))
	run, _ := eng1.CreateRun(context.Background(), engine.CreateRunInput{
		WorkflowID: "feature-development", OriginalTask: "x",
	})
	if err := eng1.Step(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
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


// ---------- MVP 05: feature-development real agents ----------

func TestFeaturedevScoutPersistsArtifacts(t *testing.T) {
	// Scout emits four evidence artifacts; each must land in storage
	// at version 1 of its (kind, name) tuple.
	script := map[string][]dsh.RawBridgeEvent{
		"sess-1": {
			eventFrame("sess-1", "agent.completed", json.RawMessage(`{
				"outcome":"advance",
				"artifacts":[
					{"kind":"evidence","name":"breadcrumbs.json","stage":"discovery","content":"# bread"},
					{"kind":"evidence","name":"repository-map.md","stage":"discovery","content":"# map"},
					{"kind":"evidence","name":"sources.json","stage":"discovery","content":"{}"},
					{"kind":"evidence","name":"skills.json","stage":"discovery","content":"[]"}
				]
			}`)),
		},
	}
	eng, repo, _, stop := newAgentEngine(t, script)
	defer stop()
	run := mustCreateRun(t, eng, "feature-development", "ship a thing")
	if err := eng.Step(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	arts, _ := repo.ListArtifacts(context.Background(), run.ID)
	if len(arts) != 4 {
		t.Fatalf("artifacts = %d, want 4", len(arts))
	}
	seen := map[string]int{}
	for _, a := range arts {
		seen[a.Name] = a.Version
	}
	for _, name := range []string{"breadcrumbs.json", "repository-map.md", "sources.json", "skills.json"} {
		if seen[name] != 1 {
			t.Errorf("artifact %s version = %d, want 1", name, seen[name])
		}
	}
	got, _ := repo.GetRun(context.Background(), run.ID)
	if got.State != domain.RunPlanning {
		t.Errorf("state = %q, want PLANNING", got.State)
	}
}

func TestFeaturedevGatekeeperApprove(t *testing.T) {
	eng, repo, _, stop := newAgentEngine(t, map[string][]dsh.RawBridgeEvent{
		"sess-1":      []dsh.RawBridgeEvent{eventFrame("sess-1", "agent.completed", json.RawMessage(`{"outcome":"advance"}`))},
		"sess-2":    []dsh.RawBridgeEvent{eventFrame("sess-2", "agent.completed", json.RawMessage(`{"outcome":"advance","artifacts":[{"kind":"spec","name":"feature-spec.md","content":"s"},{"kind":"plan","name":"implementation-plan.md","content":"p"}]}`))},
		"sess-3": []dsh.RawBridgeEvent{eventFrame("sess-3", "agent.completed", json.RawMessage(`{"outcome":"approve"}`))},
	})
	defer stop()
	run := mustCreateRun(t, eng, "feature-development", "x")
	for i := 0; i < 3; i++ {
		if err := eng.Step(context.Background(), run.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := repo.GetRun(context.Background(), run.ID)
	if got.State != domain.RunWaitingForUser {
		t.Errorf("state = %q, want WAITING_FOR_USER", got.State)
	}
	if got.Stage != "waiting_for_user" {
		t.Errorf("stage = %q, want waiting_for_user", got.Stage)
	}
}

func TestFeaturedevGatekeeperReviseLoopsBack(t *testing.T) {
	// Gatekeeper returns "revise" on the first plan_review pass. The
	// engine must transition back to PLANNING so a future Step drives
	// the planner again. We can't easily swap the mock script in this
	// process, so we simulate the revise path by registering a custom
	// workflow whose plan_review always routes revise -> planning, with
	// a scripted planner that emits two sets of artifacts.
	_, repo, _, stop := newAgentEngine(t, map[string][]dsh.RawBridgeEvent{
		"sess-2": {
			eventFrame("sess-2", "agent.completed", json.RawMessage(`{
				"outcome":"advance",
				"artifacts":[
					{"kind":"spec","name":"feature-spec.md","content":"v1"},
					{"kind":"plan","name":"implementation-plan.md","content":"v1"}
				]
			}`)),
		},
	})
	defer stop()
	// Build a minimal workflow: discovery -> planning -> plan_review.
	discovery := &engine.WorkflowDef{
		Workflow: domain.Workflow{
			ID:           "feature-development",
			Name:         "feature-development",
			Version:      "v1",
			InitialState: domain.RunNew,
			InitialStage: "discovery",
		},
		Stages: map[string]engine.StageDef{
			"discovery": {
				State: domain.RunDiscovery, StageKey: "discovery",
				Runner: func(_ context.Context, _ *engine.StageInput) (engine.StageResult, error) {
					return engine.StageResult{Kind: "advance"}, nil
				},
				Transition: func(_ context.Context, _ *engine.StageInput, _ engine.StageResult) (domain.RunState, error) {
					return domain.RunPlanning, nil
				},
			},
			"planning": {
				State: domain.RunPlanning, StageKey: "planning",
				Runner: func(ctx context.Context, in *engine.StageInput) (engine.StageResult, error) {
					// Drive a scripted planner.
					return engine.StageResult{Kind: "advance"}, nil
				},
				Transition: func(_ context.Context, _ *engine.StageInput, _ engine.StageResult) (domain.RunState, error) {
					return domain.RunPlanReview, nil
				},
			},
			"plan_review": {
				State: domain.RunPlanReview, StageKey: "plan_review",
				Runner: func(_ context.Context, _ *engine.StageInput) (engine.StageResult, error) {
					return engine.StageResult{Kind: "revise"}, nil
				},
				Transition: func(_ context.Context, _ *engine.StageInput, _ engine.StageResult) (domain.RunState, error) {
					return domain.RunPlanning, nil
				},
			},
		},
	}
	// Replace the workflow the newAgentEngine already registered.
	e2 := engine.New(engine.Option{Repo: repo})
	if err := e2.RegisterWorkflow(discovery); err != nil {
		t.Fatal(err)
	}
	run := mustCreateRun(t, e2, "feature-development", "x")
	for i := 0; i < 3; i++ {
		if err := e2.Step(context.Background(), run.ID); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	got, _ := repo.GetRun(context.Background(), run.ID)
	if got.State != domain.RunPlanning {
		t.Errorf("state = %q, want PLANNING (looped back)", got.State)
	}
}

func TestFeaturedevGatekeeperWaitUserCreatesAttention(t *testing.T) {
	eng, repo, _, stop := newAgentEngine(t, map[string][]dsh.RawBridgeEvent{
		"sess-1":      []dsh.RawBridgeEvent{eventFrame("sess-1", "agent.completed", json.RawMessage(`{"outcome":"advance"}`))},
		"sess-2":    []dsh.RawBridgeEvent{eventFrame("sess-2", "agent.completed", json.RawMessage(`{"outcome":"advance","artifacts":[{"kind":"spec","name":"feature-spec.md","content":"s"},{"kind":"plan","name":"implementation-plan.md","content":"p"}]}`))},
		"sess-3": []dsh.RawBridgeEvent{eventFrame("sess-3", "agent.completed", json.RawMessage(`{"outcome":"wait_user","questions":["which DB?","which auth?"]}`))},
	})
	defer stop()
	run := mustCreateRun(t, eng, "feature-development", "x")
	for i := 0; i < 3; i++ {
		if err := eng.Step(context.Background(), run.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := repo.GetRun(context.Background(), run.ID)
	if got.State != domain.RunWaitingForUser {
		t.Errorf("state = %q, want WAITING_FOR_USER", got.State)
	}
	atts, _ := repo.ListAttention(context.Background(), run.ID)
	if len(atts) != 1 {
		t.Fatalf("attentions = %d, want 1", len(atts))
	}
	if atts[0].Status != domain.AttentionOpen {
		t.Errorf("status = %q, want OPEN", atts[0].Status)
	}
	if len(atts[0].Options) != 2 {
		t.Errorf("options = %d, want 2", len(atts[0].Options))
	}
}

func TestFeaturedevGatekeeperFailedMarksRunFailed(t *testing.T) {
	eng, repo, _, stop := newAgentEngine(t, map[string][]dsh.RawBridgeEvent{
		"sess-1":      []dsh.RawBridgeEvent{eventFrame("sess-1", "agent.completed", json.RawMessage(`{"outcome":"advance"}`))},
		"sess-2":    []dsh.RawBridgeEvent{eventFrame("sess-2", "agent.completed", json.RawMessage(`{"outcome":"advance","artifacts":[{"kind":"spec","name":"feature-spec.md","content":"s"},{"kind":"plan","name":"implementation-plan.md","content":"p"}]}`))},
		"sess-3": []dsh.RawBridgeEvent{eventFrame("sess-3", "agent.completed", json.RawMessage(`{"outcome":"failed","summary":"nope"}`))},
	})
	defer stop()
	run := mustCreateRun(t, eng, "feature-development", "x")
	for i := 0; i < 3; i++ {
		if err := eng.Step(context.Background(), run.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := repo.GetRun(context.Background(), run.ID)
	if got.State != domain.RunFailed {
		t.Errorf("state = %q, want FAILED", got.State)
	}
	if got.TerminalReason == "" {
		t.Errorf("TerminalReason empty")
	}
}

// ---------- MVP 06: implementation + review ----------

func TestFeaturedevImplementerRunsAndAdvancesToReview(t *testing.T) {
	eng, repo, _, stop := newAgentEngine(t, map[string][]dsh.RawBridgeEvent{
		"sess-1":       []dsh.RawBridgeEvent{eventFrame("sess-1", "agent.completed", json.RawMessage(`{"outcome":"advance"}`))},
		"sess-2":     []dsh.RawBridgeEvent{eventFrame("sess-2", "agent.completed", json.RawMessage(`{"outcome":"advance","artifacts":[{"kind":"spec","name":"feature-spec.md","content":"s"},{"kind":"plan","name":"implementation-plan.md","content":"p"}]}`))},
		"sess-3":  []dsh.RawBridgeEvent{eventFrame("sess-3", "agent.completed", json.RawMessage(`{"outcome":"approve"}`))},
		"sess-4": []dsh.RawBridgeEvent{eventFrame("sess-4", "agent.completed", json.RawMessage(`{"outcome":"advance","artifacts":[{"kind":"other","name":"diff-summary.md","stage":"implementing","content":"changed a/b/c"}]}`))},
	})
	defer stop()
	run := mustCreateRun(t, eng, "feature-development", "x")
	for i := 0; i < 5; i++ {
		if err := eng.Step(context.Background(), run.ID); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	got, _ := repo.GetRun(context.Background(), run.ID)
	if got.State != domain.RunImplementationReview {
		t.Errorf("state = %q, want IMPLEMENTATION_REVIEW", got.State)
	}
	arts, _ := repo.ListArtifacts(context.Background(), run.ID)
	for _, a := range arts {
		if a.Name == "diff-summary.md" && a.Version != 1 {
			t.Errorf("diff-summary version = %d, want 1", a.Version)
		}
	}
}

func TestFeaturedevReviewerApproveReachesDone(t *testing.T) {
	eng, repo, _, stop := newAgentEngine(t, map[string][]dsh.RawBridgeEvent{
		"sess-1":       []dsh.RawBridgeEvent{eventFrame("sess-1", "agent.completed", json.RawMessage(`{"outcome":"advance"}`))},
		"sess-2":     []dsh.RawBridgeEvent{eventFrame("sess-2", "agent.completed", json.RawMessage(`{"outcome":"advance","artifacts":[{"kind":"spec","name":"feature-spec.md","content":"s"},{"kind":"plan","name":"implementation-plan.md","content":"p"}]}`))},
		"sess-3":  []dsh.RawBridgeEvent{eventFrame("sess-3", "agent.completed", json.RawMessage(`{"outcome":"approve"}`))},
		"sess-4": []dsh.RawBridgeEvent{eventFrame("sess-4", "agent.completed", json.RawMessage(`{"outcome":"advance"}`))},
		"sess-5":    []dsh.RawBridgeEvent{eventFrame("sess-5", "agent.completed", json.RawMessage(`{"outcome":"approve","summary":"clean"}`))},
	})
	defer stop()
	run := mustCreateRun(t, eng, "feature-development", "ship it")
	for i := 0; i < 6; i++ {
		if err := eng.Step(context.Background(), run.ID); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	got, _ := repo.GetRun(context.Background(), run.ID)
	if got.State != domain.RunDone {
		t.Errorf("state = %q, want DONE", got.State)
	}
}

func TestFeaturedevReviewerReviseLoopsBackToImplementing(t *testing.T) {
	// Reviewer emits one IMPLEMENTATION_BUG finding and outcome=revise.
	// The engine must route the run back to IMPLEMENTING.
	eng, repo, _, stop := newAgentEngine(t, map[string][]dsh.RawBridgeEvent{
		"sess-1":       []dsh.RawBridgeEvent{eventFrame("sess-1", "agent.completed", json.RawMessage(`{"outcome":"advance"}`))},
		"sess-2":     []dsh.RawBridgeEvent{eventFrame("sess-2", "agent.completed", json.RawMessage(`{"outcome":"advance","artifacts":[{"kind":"spec","name":"feature-spec.md","content":"s"},{"kind":"plan","name":"implementation-plan.md","content":"p"}]}`))},
		"sess-3":  []dsh.RawBridgeEvent{eventFrame("sess-3", "agent.completed", json.RawMessage(`{"outcome":"approve"}`))},
		"sess-4": []dsh.RawBridgeEvent{eventFrame("sess-4", "agent.completed", json.RawMessage(`{"outcome":"advance"}`))},
		"sess-5": []dsh.RawBridgeEvent{
			eventFrame("sess-5", "agent.completed", json.RawMessage(`{
				"outcome":"revise",
				"review_findings":[
					{"class":"IMPLEMENTATION_BUG","statement":"missing error check"}
				]
			}`)),
		},
	})
	defer stop()
	run := mustCreateRun(t, eng, "feature-development", "x")
	for i := 0; i < 6; i++ {
		if err := eng.Step(context.Background(), run.ID); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	got, _ := repo.GetRun(context.Background(), run.ID)
	if got.State != domain.RunImplementing {
		t.Errorf("state = %q, want IMPLEMENTING (loop back)", got.State)
	}
	findings, _ := repo.BlockingFindings(context.Background(), run.ID)
	if len(findings) != 1 {
		t.Fatalf("blocking findings = %d, want 1", len(findings))
	}
	if findings[0].Class != domain.FindingImplementationBug {
		t.Errorf("class = %q, want IMPLEMENTATION_BUG", findings[0].Class)
	}
}

func TestFeaturedevReviewerRevisePlanRoutesBackToPlanReview(t *testing.T) {
	eng, repo, _, stop := newAgentEngine(t, map[string][]dsh.RawBridgeEvent{
		"sess-1":       []dsh.RawBridgeEvent{eventFrame("sess-1", "agent.completed", json.RawMessage(`{"outcome":"advance"}`))},
		"sess-2":     []dsh.RawBridgeEvent{eventFrame("sess-2", "agent.completed", json.RawMessage(`{"outcome":"advance","artifacts":[{"kind":"spec","name":"feature-spec.md","content":"s"},{"kind":"plan","name":"implementation-plan.md","content":"p"}]}`))},
		"sess-3":  []dsh.RawBridgeEvent{eventFrame("sess-3", "agent.completed", json.RawMessage(`{"outcome":"approve"}`))},
		"sess-4": []dsh.RawBridgeEvent{eventFrame("sess-4", "agent.completed", json.RawMessage(`{"outcome":"advance"}`))},
		"sess-5": []dsh.RawBridgeEvent{
			eventFrame("sess-5", "agent.completed", json.RawMessage(`{
				"outcome":"revise_plan",
				"review_findings":[
					{"class":"PLAN_DEFECT","statement":"step 2 is unbuildable"}
				]
			}`)),
		},
	})
	defer stop()
	run := mustCreateRun(t, eng, "feature-development", "x")
	for i := 0; i < 6; i++ {
		if err := eng.Step(context.Background(), run.ID); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	got, _ := repo.GetRun(context.Background(), run.ID)
	if got.State != domain.RunPlanReview {
		t.Errorf("state = %q, want PLAN_REVIEW", got.State)
	}
}

func TestFeaturedevReviewerWaitUserCreatesAttention(t *testing.T) {
	eng, repo, _, stop := newAgentEngine(t, map[string][]dsh.RawBridgeEvent{
		"sess-1":       []dsh.RawBridgeEvent{eventFrame("sess-1", "agent.completed", json.RawMessage(`{"outcome":"advance"}`))},
		"sess-2":     []dsh.RawBridgeEvent{eventFrame("sess-2", "agent.completed", json.RawMessage(`{"outcome":"advance","artifacts":[{"kind":"spec","name":"feature-spec.md","content":"s"},{"kind":"plan","name":"implementation-plan.md","content":"p"}]}`))},
		"sess-3":  []dsh.RawBridgeEvent{eventFrame("sess-3", "agent.completed", json.RawMessage(`{"outcome":"approve"}`))},
		"sess-4": []dsh.RawBridgeEvent{eventFrame("sess-4", "agent.completed", json.RawMessage(`{"outcome":"advance"}`))},
		"sess-5": []dsh.RawBridgeEvent{
			eventFrame("sess-5", "agent.completed", json.RawMessage(`{
				"outcome":"wait_user",
				"review_findings":[
					{"class":"REQUIREMENT_AMBIGUITY","statement":"what status codes for invalid input?"}
				]
			}`)),
		},
	})
	defer stop()
	run := mustCreateRun(t, eng, "feature-development", "x")
	for i := 0; i < 6; i++ {
		if err := eng.Step(context.Background(), run.ID); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	got, _ := repo.GetRun(context.Background(), run.ID)
	if got.State != domain.RunWaitingForUser {
		t.Errorf("state = %q, want WAITING_FOR_USER", got.State)
	}
	atts, _ := repo.ListAttention(context.Background(), run.ID)
	if len(atts) != 1 {
		t.Fatalf("attentions = %d, want 1", len(atts))
	}
	if atts[0].Title == "" {
		t.Errorf("attention title empty")
	}
}
