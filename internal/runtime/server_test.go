package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/protocol"
	"github.com/unbot/work9flow/internal/storage"
)

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	srv := New(Options{Name: "work9flowd-test", Version: "test", Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	ln, err := srv.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()

	deadline := time.Now().Add(2 * time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if srv.Addr() == "" {
		t.Fatal("run did not bind in time")
	}
	return srv, "http://" + srv.Addr()
}

func newTestServerWithRepo(t *testing.T) (*Server, string, storage.Repo) {
	t.Helper()
	repo, err := storage.OpenSQLite(":memory:")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	srv := New(Options{
		Name: "work9flowd-test", Version: "test", Addr: "127.0.0.1:0", Repo: repo,
	})
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	ln, err := srv.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	deadline := time.Now().Add(2 * time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return srv, "http://" + srv.Addr(), repo
}

// ---------- existing MVP 01 tests (kept) ----------

func TestHealthEndpoint(t *testing.T) {
	_, base := newTestServer(t)
	resp, err := http.Get(base + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got protocol.HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != "ok" {
		t.Errorf("Status = %q", got.Status)
	}
	if got.Version == "" {
		t.Error("Version must not be empty")
	}
	if got.UptimeS < 0 {
		t.Errorf("UptimeS = %d", got.UptimeS)
	}
}

func TestVersionEndpoint(t *testing.T) {
	_, base := newTestServer(t)
	resp, err := http.Get(base + "/v1/version")
	if err != nil {
		t.Fatalf("GET /v1/version: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, body)
	}
	var got protocol.VersionResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "work9flowd-test" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.Version != "test" {
		t.Errorf("Version = %q", got.Version)
	}
}

func TestUnknownRoute404(t *testing.T) {
	_, base := newTestServer(t)
	resp, err := http.Get(base + "/nope")
	if err != nil {
		t.Fatalf("GET /nope: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Errorf("expected json content-type, got %q", resp.Header.Get("Content-Type"))
	}
}

func TestRunsEndpointEmpty(t *testing.T) {
	_, base := newTestServer(t)
	resp, err := http.Get(base + "/v1/runs")
	if err != nil {
		t.Fatalf("GET /v1/runs: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got protocol.RunListResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Runs) != 0 {
		t.Errorf("expected 0 runs at bootstrap, got %d", len(got.Runs))
	}
}

func TestShutdownStopsServing(t *testing.T) {
	srv := New(Options{Name: "x", Version: "v", Addr: "127.0.0.1:0"})
	ln, err := srv.Listen()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	deadline := time.Now().Add(time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// ---------- MVP 02: storage-wired tests ----------

func TestCreateAndListRuns(t *testing.T) {
	_, base, _ := newTestServerWithRepo(t)
	body, _ := json.Marshal(protocol.RunCreateRequest{
		WorkflowID:   "feature-development",
		RepoPath:     "/tmp/repo",
		OriginalTask: "implement X",
	})
	resp, err := http.Post(base+"/v1/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var created protocol.RunCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.Run.OriginalTask != "implement X" {
		t.Errorf("OriginalTask = %q", created.Run.OriginalTask)
	}
	if created.Run.EventCount != 1 {
		t.Errorf("EventCount = %d, want 1 (workflow.created)", created.Run.EventCount)
	}

	// List should now have one run.
	r2, err := http.Get(base + "/v1/runs")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	var list protocol.RunListResponse
	if err := json.NewDecoder(r2.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Runs) != 1 {
		t.Fatalf("len = %d", len(list.Runs))
	}
	if list.Runs[0].ID != created.Run.ID {
		t.Errorf("ID = %q", list.Runs[0].ID)
	}
}

func TestCreateRunRejectsMissingFields(t *testing.T) {
	_, base, _ := newTestServerWithRepo(t)
	body, _ := json.Marshal(protocol.RunCreateRequest{OriginalTask: "no-workflow"})
	resp, err := http.Post(base+"/v1/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestGetRunEventsAndCursor(t *testing.T) {
	_, base, repo := newTestServerWithRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	id := "run-evt"
	if err := repo.CreateRun(context.Background(), domain.WorkflowRun{
		ID: id, WorkflowID: "w", OriginalTask: "t", State: domain.RunDiscovery,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for i, k := range []domain.EventKind{
		domain.EventKindWorkflowCreated,
		domain.EventKindStageStarted,
		domain.EventKindAgentStarted,
		domain.EventKindAgentCompleted,
	} {
		if _, err := repo.AppendEvent(context.Background(), id, k,
			now.Add(time.Duration(i+1)*time.Second), nil); err != nil {
			t.Fatal(err)
		}
	}

	// No cursor -> all events.
	r1, err := http.Get(base + "/v1/runs/" + id + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Body.Close()
	var all protocol.EventListResponse
	if err := json.NewDecoder(r1.Body).Decode(&all); err != nil {
		t.Fatal(err)
	}
	if len(all.Events) != 4 {
		t.Fatalf("events = %d", len(all.Events))
	}
	if all.LatestSeq != 4 {
		t.Errorf("LatestSeq = %d, want 4", all.LatestSeq)
	}

	// Cursor after seq=2 -> events 3 and 4.
	r2, err := http.Get(base + "/v1/runs/" + id + "/events?after=2")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	var after protocol.EventListResponse
	if err := json.NewDecoder(r2.Body).Decode(&after); err != nil {
		t.Fatal(err)
	}
	if len(after.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(after.Events))
	}
	if after.Events[0].Seq != 3 || after.Events[1].Seq != 4 {
		t.Errorf("seqs = %d,%d", after.Events[0].Seq, after.Events[1].Seq)
	}
}

func TestAttentionLifecycleHTTP(t *testing.T) {
	_, base, repo := newTestServerWithRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	id := "run-att"
	if err := repo.CreateRun(context.Background(), domain.WorkflowRun{
		ID: id, WorkflowID: "w", OriginalTask: "t", State: domain.RunDiscovery,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.CreateAttention(context.Background(), domain.Attention{
		ID: "att-1", RunID: id, Kind: domain.AttentionQuestion,
		Status: domain.AttentionOpen, Blocking: true, Title: "?",
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// List
	r1, err := http.Get(base + "/v1/runs/" + id + "/attentions")
	if err != nil { t.Fatal(err) }
	defer r1.Body.Close()
	var lst protocol.AttentionListResponse
	json.NewDecoder(r1.Body).Decode(&lst)
	if len(lst.Attentions) != 1 {
		t.Fatalf("attentions = %d", len(lst.Attentions))
	}

	// Answer
	body, _ := json.Marshal(protocol.AttentionAnswerRequest{Answer: json.RawMessage(`"postgres"`)})
	r2, err := http.Post(base+"/v1/attentions/att-1/answer", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", r2.StatusCode)
	}
	var ans protocol.AttentionAnswerResponse
	json.NewDecoder(r2.Body).Decode(&ans)
	if ans.Attention.Status != string(domain.AttentionAnswered) {
		t.Errorf("status = %q", ans.Attention.Status)
	}

	// Re-answering must be rejected with 409.
	r3, err := http.Post(base+"/v1/attentions/att-1/answer", "application/json", bytes.NewReader(body))
	if err != nil { t.Fatal(err) }
	defer r3.Body.Close()
	if r3.StatusCode != http.StatusConflict {
		t.Errorf("re-answer status = %d, want 409", r3.StatusCode)
	}

	// Run must have +1 event for attention.resolved.
	evs, _ := repo.EventsAfter(context.Background(), id, 0)
	if len(evs) < 1 {
		t.Errorf("expected at least 1 event, got %d", len(evs))
	}
}

func TestSteerAndFollowupAppendEvents(t *testing.T) {
	_, base, repo := newTestServerWithRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	id := "run-steer"
	if err := repo.CreateRun(context.Background(), domain.WorkflowRun{
		ID: id, WorkflowID: "w", OriginalTask: "t", State: domain.RunDiscovery,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(protocol.SteerRequest{AgentID: "a-1", Message: "nudge"})
	r1, err := http.Post(base+"/v1/runs/"+id+"/steer", "application/json", bytes.NewReader(body))
	if err != nil { t.Fatal(err) }
	defer r1.Body.Close()
	if r1.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", r1.StatusCode)
	}
	body2, _ := json.Marshal(protocol.FollowupRequest{AgentID: "a-1", Message: "follow-up"})
	r2, err := http.Post(base+"/v1/runs/"+id+"/followup", "application/json", bytes.NewReader(body2))
	if err != nil { t.Fatal(err) }
	defer r2.Body.Close()
	if r2.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", r2.StatusCode)
	}
	evs, _ := repo.EventsAfter(context.Background(), id, 0)
	if len(evs) != 2 {
		t.Fatalf("events = %d, want 2", len(evs))
	}
	if evs[0].Kind != domain.EventKindSteerSent || evs[1].Kind != domain.EventKindFollowupSent {
		t.Errorf("kinds = %s, %s", evs[0].Kind, evs[1].Kind)
	}
}

func TestCancelRunWritesEvent(t *testing.T) {
	_, base, repo := newTestServerWithRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	id := "run-can"
	if err := repo.CreateRun(context.Background(), domain.WorkflowRun{
		ID: id, WorkflowID: "w", OriginalTask: "t", State: domain.RunDiscovery,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodDelete, base+"/v1/runs/"+id, nil)
	if err != nil { t.Fatal(err) }
	r, err := http.DefaultClient.Do(req)
	if err != nil { t.Fatal(err) }
	defer r.Body.Close()
	if r.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d", r.StatusCode)
	}
	got, _ := repo.GetRun(context.Background(), id)
	if got.State != domain.RunCanceled {
		t.Errorf("state = %q", got.State)
	}
	evs, _ := repo.EventsAfter(context.Background(), id, 0)
	if len(evs) != 1 || evs[0].Kind != domain.EventKindWorkflowCanceled {
		t.Errorf("events = %+v", evs)
	}
}

func TestArtifactsEndpoint(t *testing.T) {
	_, base, repo := newTestServerWithRepo(t)
	now := time.Now().UTC().Truncate(time.Second)
	id := "run-art"
	if err := repo.CreateRun(context.Background(), domain.WorkflowRun{
		ID: id, WorkflowID: "w", OriginalTask: "t", State: domain.RunDiscovery,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		a := &domain.Artifact{
			RunID: id, Kind: domain.ArtifactPlan, Name: "impl-plan",
			ContentRef: "v1", CreatedAt: now.Add(time.Duration(i) * time.Second),
		}
		if err := repo.AddArtifact(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	r, err := http.Get(base + "/v1/runs/" + id + "/artifacts")
	if err != nil { t.Fatal(err) }
	defer r.Body.Close()
	var lst protocol.ArtifactListResponse
	json.NewDecoder(r.Body).Decode(&lst)
	if len(lst.Artifacts) != 3 {
		t.Fatalf("len = %d", len(lst.Artifacts))
	}
	if lst.Artifacts[2].Version != 3 {
		t.Errorf("latest version = %d", lst.Artifacts[2].Version)
	}
}

func TestNoRepoHealthWorks(t *testing.T) {
	// Health/version must work even without a Repo (the legacy contract).
	_, base := newTestServer(t)
	r, err := http.Get(base + "/v1/health")
	if err != nil { t.Fatal(err) }
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Errorf("status = %d", r.StatusCode)
	}
}

func TestNoRepoCreateReturns503(t *testing.T) {
	_, base := newTestServer(t)
	body, _ := json.Marshal(protocol.RunCreateRequest{WorkflowID: "w", OriginalTask: "t"})
	r, err := http.Post(base+"/v1/runs", "application/json", bytes.NewReader(body))
	if err != nil { t.Fatal(err) }
	defer r.Body.Close()
	if r.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", r.StatusCode)
	}
}
