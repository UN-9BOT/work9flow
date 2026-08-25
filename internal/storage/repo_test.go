// Package storage owns durable persistence for work9flow. It exposes
// a Repo interface and a SQLite implementation; the rest of work9flow
// (protocol, runtime, TUI) talks only to the interface so we can
// swap engines without changing call sites.
//
// Behaviour tests below cover the acceptance bullets from
// work9flow-19x.7: persistence/reload, event ordering, append-only
// clarifications, artifact version retention, attention lifecycle,
// replay from cursor, and rejection of invalid mutations.
package storage_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/storage"
)

// newTestRepo returns an in-memory SQLite-backed repo, plus a closer
// the caller must defer. Errors fail the test.
func newTestRepo(t *testing.T) storage.Repo {
	t.Helper()
	r, err := storage.OpenSQLite(":memory:")
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func mustCreateRun(t *testing.T, r storage.Repo, run domain.WorkflowRun) domain.WorkflowRun {
	t.Helper()
	if err := r.CreateRun(context.Background(), run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return run
}

func sampleRun() domain.WorkflowRun {
	return domain.WorkflowRun{
		ID:              "run-1",
		WorkflowID:      "feature-development",
		WorkflowVersion: "v1",
		RepoPath:        "/tmp/repo",
		OriginalTask:    "implement X",
		State:           domain.RunDiscovery,
		Stage:           "discovery",
		CreatedAt:       time.Unix(1, 0).UTC(),
		UpdatedAt:       time.Unix(1, 0).UTC(),
	}
}

func TestCreateAndGetRun(t *testing.T) {
	r := newTestRepo(t)
	mustCreateRun(t, r, sampleRun())

	got, err := r.GetRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.ID != "run-1" || got.OriginalTask != "implement X" || got.State != domain.RunDiscovery {
		t.Errorf("mismatch: %+v", got)
	}
}

func TestGetRunMissing(t *testing.T) {
	r := newTestRepo(t)
	if _, err := r.GetRun(context.Background(), "nope"); !errors.Is(err, storage.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestListRunsEmpty(t *testing.T) {
	r := newTestRepo(t)
	runs, err := r.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("len = %d", len(runs))
	}
}

func TestListRunsOrder(t *testing.T) {
	r := newTestRepo(t)
	mustCreateRun(t, r, domain.WorkflowRun{ID: "r-a", WorkflowID: "feature-development", OriginalTask: "a", State: domain.RunNew, CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC()})
	mustCreateRun(t, r, domain.WorkflowRun{ID: "r-b", WorkflowID: "feature-development", OriginalTask: "b", State: domain.RunNew, CreatedAt: time.Unix(2, 0).UTC(), UpdatedAt: time.Unix(2, 0).UTC()})
	runs, err := r.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("len = %d", len(runs))
	}
	// Sorted by created_at desc (newest first).
	if runs[0].ID != "r-b" || runs[1].ID != "r-a" {
		t.Errorf("order = %s,%s; want r-b,r-a", runs[0].ID, runs[1].ID)
	}
}

func TestUpdateRunStateRejectsInvalidTransition(t *testing.T) {
	r := newTestRepo(t)
	mustCreateRun(t, r, sampleRun())
	err := r.UpdateRunState(context.Background(), "run-1", domain.RunImplementing, "impl", "")
	if err == nil {
		t.Fatal("expected error for invalid transition NEW -> IMPLEMENTING")
	}
}

func TestUpdateRunStateHappyPath(t *testing.T) {
	r := newTestRepo(t)
	mustCreateRun(t, r, sampleRun())
	if err := r.UpdateRunState(context.Background(), "run-1", domain.RunPlanning, "", ""); err != nil {
		t.Fatalf("UpdateRunState: %v", err)
	}
	got, _ := r.GetRun(context.Background(), "run-1")
	if got.State != domain.RunPlanning {
		t.Errorf("state = %q", got.State)
	}
}

func TestEventOrderingAndReplay(t *testing.T) {
	r := newTestRepo(t)
	mustCreateRun(t, r, sampleRun())
	ctx := context.Background()

	seqs := []int64{}
	for i, k := range []domain.EventKind{
		domain.EventKindWorkflowCreated,
		domain.EventKindStageStarted,
		domain.EventKindAgentStarted,
	} {
		s, err := r.AppendEvent(ctx, "run-1", k, time.Unix(int64(i+1), 0).UTC(), json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("AppendEvent[%d]: %v", i, err)
		}
		seqs = append(seqs, s)
	}
	for i, s := range seqs {
		if s != int64(i+1) {
			t.Fatalf("seq[%d] = %d, want %d", i, s, i+1)
		}
	}

	after, err := r.EventsAfter(ctx, "run-1", 1)
	if err != nil {
		t.Fatalf("EventsAfter: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("len = %d, want 2", len(after))
	}
	if after[0].Seq != 2 || after[1].Seq != 3 {
		t.Errorf("seqs = %d,%d", after[0].Seq, after[1].Seq)
	}
}

func TestArtifactVersionRetention(t *testing.T) {
	r := newTestRepo(t)
	mustCreateRun(t, r, sampleRun())
	ctx := context.Background()

	addSpec := func(at time.Time, body string) domain.Artifact {
		a := domain.Artifact{
			RunID:      "run-1",
			Kind:       domain.ArtifactSpec,
			Name:       "feature-spec",
			ContentRef: body,
			CreatedAt:  at,
		}
		if err := r.AddArtifact(ctx, &a); err != nil {
			t.Fatalf("AddArtifact: %v", err)
		}
		return a
	}
	v1 := addSpec(time.Unix(1, 0).UTC(), "first")
	v2 := addSpec(time.Unix(2, 0).UTC(), "second")
	v3 := addSpec(time.Unix(3, 0).UTC(), "third")

	if v1.Version != 1 || v2.Version != 2 || v3.Version != 3 {
		t.Fatalf("versions = %d,%d,%d; want 1,2,3", v1.Version, v2.Version, v3.Version)
	}

	all, err := r.ListArtifacts(ctx, "run-1")
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3 (no silent overwrites)", len(all))
	}
}

func TestArtifactOverwriteRefusesSilentReplace(t *testing.T) {
	r := newTestRepo(t)
	mustCreateRun(t, r, sampleRun())
	ctx := context.Background()

	// Two Adds with same (Kind,Name) must produce different versions.
	a := domain.Artifact{RunID: "run-1", Kind: domain.ArtifactPlan, Name: "impl-plan", ContentRef: "v1", CreatedAt: time.Unix(1, 0).UTC()}
	b := domain.Artifact{RunID: "run-1", Kind: domain.ArtifactPlan, Name: "impl-plan", ContentRef: "v2", CreatedAt: time.Unix(2, 0).UTC()}
	if err := r.AddArtifact(ctx, &a); err != nil {
		t.Fatal(err)
	}
	if err := r.AddArtifact(ctx, &b); err != nil {
		t.Fatal(err)
	}
	all, _ := r.ListArtifacts(ctx, "run-1")
	if len(all) != 2 {
		t.Fatalf("silent overwrite: len = %d", len(all))
	}
	if all[0].ContentRef == all[1].ContentRef {
		t.Fatal("content_ref identical — overwrite happened")
	}
}

func TestClarificationsAppendOnly(t *testing.T) {
	r := newTestRepo(t)
	mustCreateRun(t, r, sampleRun())
	ctx := context.Background()
	for _, body := range []string{"first", "second", "third"} {
		c := domain.Clarification{Body: body, At: time.Unix(1, 0).UTC()}
		if err := r.AppendClarification(ctx, "run-1", c); err != nil {
			t.Fatalf("AppendClarification: %v", err)
		}
	}
	all, err := r.Clarifications(ctx, "run-1")
	if err != nil {
		t.Fatalf("Clarifications: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d", len(all))
	}
	if all[0].Seq != 1 || all[2].Seq != 3 {
		t.Errorf("seqs = %d,%d,%d", all[0].Seq, all[1].Seq, all[2].Seq)
	}
}

func TestAttentionLifecycle(t *testing.T) {
	r := newTestRepo(t)
	mustCreateRun(t, r, sampleRun())
	ctx := context.Background()
	a := domain.Attention{
		ID:        "att-1",
		RunID:     "run-1",
		Kind:      domain.AttentionQuestion,
		Status:    domain.AttentionOpen,
		Blocking:  true,
		Title:     "which DB?",
		CreatedAt: time.Unix(1, 0).UTC(),
	}
	if err := r.CreateAttention(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := r.AnswerAttention(ctx, "att-1", json.RawMessage(`{"choice":"postgres"}`), time.Unix(2, 0).UTC()); err != nil {
		t.Fatalf("AnswerAttention: %v", err)
	}
	got, err := r.GetAttention(ctx, "att-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.AttentionAnswered {
		t.Errorf("status = %q", got.Status)
	}
	if got.AnsweredAt.IsZero() {
		t.Error("answered_at not set")
	}
}

func TestAnswerClosedAttentionRejected(t *testing.T) {
	r := newTestRepo(t)
	mustCreateRun(t, r, sampleRun())
	ctx := context.Background()
	a := domain.Attention{ID: "att-1", RunID: "run-1", Kind: domain.AttentionDecision, Status: domain.AttentionOpen, Blocking: true, Title: "?", CreatedAt: time.Unix(1, 0).UTC()}
	_ = r.CreateAttention(ctx, a)
	if err := r.AnswerAttention(ctx, "att-1", json.RawMessage(`"a"`), time.Unix(2, 0).UTC()); err != nil {
		t.Fatal(err)
	}
	// Second answer must be rejected.
	if err := r.AnswerAttention(ctx, "att-1", json.RawMessage(`"b"`), time.Unix(3, 0).UTC()); err == nil {
		t.Fatal("expected error re-answering closed attention")
	}
}

func TestPersistenceReload(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "work9flow.db")

	r1, err := storage.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	mustCreateRun(t, r1, sampleRun())
	s, err := r1.AppendEvent(context.Background(), "run-1", domain.EventKindWorkflowCreated, time.Unix(1, 0).UTC(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if s != 1 {
		t.Fatalf("seq = %d, want 1", s)
	}
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}

	r2, err := storage.OpenSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	got, err := r2.GetRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("GetRun after reload: %v", err)
	}
	if got.OriginalTask != "implement X" {
		t.Errorf("OriginalTask = %q", got.OriginalTask)
	}
	ev, err := r2.EventsAfter(context.Background(), "run-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) != 1 || ev[0].Seq != 1 {
		t.Errorf("events = %+v", ev)
	}
}

func TestActiveArtifactPointer(t *testing.T) {
	r := newTestRepo(t)
	mustCreateRun(t, r, sampleRun())
	ctx := context.Background()
	if err := r.AddArtifact(ctx, &domain.Artifact{RunID: "run-1", Kind: domain.ArtifactPlan, Name: "x", ContentRef: "v1", CreatedAt: time.Unix(1, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := r.AddArtifact(ctx, &domain.Artifact{RunID: "run-1", Kind: domain.ArtifactPlan, Name: "x", ContentRef: "v2", CreatedAt: time.Unix(2, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	ver, err := r.ActiveArtifactVersion(ctx, "run-1", domain.ArtifactPlan, "x")
	if err != nil {
		t.Fatal(err)
	}
	if ver != 2 {
		t.Errorf("active version = %d, want 2", ver)
	}
}
