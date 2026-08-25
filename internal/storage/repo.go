// Package storage owns durable persistence for work9flow. It exposes
// a Repo interface and a SQLite-backed implementation.
//
// The rest of work9flow (protocol, runtime, TUI) talks only to Repo,
// never to the concrete SQLite type, so storage engines can be
// swapped (memory-only, Postgres, etc.) without touching call sites.
//
// Design rules enforced here:
//   * OriginalTask is immutable after CreateRun (no UPDATE touches it);
//   * Artifacts are append-only: AddArtifact always inserts a new row
//     with version = max(version)+1 per (run, kind, name);
//   * Clarifications are append-only (no UPDATE / DELETE);
//   * Events are append-only with per-run monotonic Seq;
//   * Attention lifecycle is enforced: only OPEN -> ANSWERED|CANCELED.
package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/unbot/work9flow/internal/domain"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("storage: not found")

// Repo is the storage contract every work9flow caller depends on.
type Repo interface {
	CreateRun(ctx context.Context, run domain.WorkflowRun) error
	GetRun(ctx context.Context, id string) (domain.WorkflowRun, error)
	ListRuns(ctx context.Context) ([]domain.WorkflowRun, error)
	UpdateRunState(ctx context.Context, id string, state domain.RunState, stage, terminalReason string) error
	UpdateRunAgents(ctx context.Context, id string, agentIDs []string) error
	IncrementIteration(ctx context.Context, id, stage string) (int, error)

	AppendEvent(ctx context.Context, runID string, kind domain.EventKind, at time.Time, data json.RawMessage) (int64, error)
	EventsAfter(ctx context.Context, runID string, afterSeq int64) ([]domain.Event, error)

	AddArtifact(ctx context.Context, a *domain.Artifact) error
	ListArtifacts(ctx context.Context, runID string) ([]domain.Artifact, error)
	ActiveArtifactVersion(ctx context.Context, runID string, kind domain.ArtifactKind, name string) (int, error)

	CreateAttention(ctx context.Context, a domain.Attention) error
	GetAttention(ctx context.Context, id string) (domain.Attention, error)
	ListAttention(ctx context.Context, runID string) ([]domain.Attention, error)
	AnswerAttention(ctx context.Context, id string, answer json.RawMessage, at time.Time) error

	AppendClarification(ctx context.Context, runID string, c domain.Clarification) error
	Clarifications(ctx context.Context, runID string) ([]domain.Clarification, error)

	Close() error
}

// sqliteRepo implements Repo on top of SQLite (modernc.org/sqlite,
// pure-Go, no CGO).
type sqliteRepo struct {
	db *sql.DB
}

// OpenSQLite opens a SQLite-backed Repo. path may be ":memory:" for
// tests or a filesystem path under the runtime's state_dir.
func OpenSQLite(path string) (Repo, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("storage: open %s: %w", path, err)
	}
	if _, err := db.ExecContext(context.Background(), pragma); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("storage: pragma: %w", err)
	}
	r := &sqliteRepo{db: db}
	if err := r.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return r, nil
}

// pragma enables WAL + foreign keys. WAL allows concurrent readers
// while a writer is active; the runtime has one writer (the
// event/store pump) and many readers (HTTP handlers).
const pragma = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;
`

const schema = `
CREATE TABLE IF NOT EXISTS runs (
  id                   TEXT PRIMARY KEY,
  workflow_id          TEXT NOT NULL,
  workflow_version     TEXT NOT NULL DEFAULT '',
  repo_path            TEXT NOT NULL DEFAULT '',
  original_task        TEXT NOT NULL,
  state                TEXT NOT NULL,
  stage                TEXT NOT NULL DEFAULT '',
  terminal_reason      TEXT NOT NULL DEFAULT '',
  active_agent_ids     TEXT NOT NULL DEFAULT '[]',
  active_artifact_ver  TEXT NOT NULL DEFAULT '{}',
  iteration_counters   TEXT NOT NULL DEFAULT '{}',
  created_at           INTEGER NOT NULL,
  updated_at           INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
  run_id    TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  seq       INTEGER NOT NULL,
  prev_seq  INTEGER NOT NULL DEFAULT 0,
  kind      TEXT NOT NULL,
  at        INTEGER NOT NULL,
  data      BLOB NOT NULL DEFAULT '{}',
  PRIMARY KEY (run_id, seq)
);

CREATE TABLE IF NOT EXISTS artifacts (
  run_id      TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  id          TEXT NOT NULL,
  kind        TEXT NOT NULL,
  name        TEXT NOT NULL,
  version     INTEGER NOT NULL,
  stage       TEXT NOT NULL DEFAULT '',
  agent_id    TEXT NOT NULL DEFAULT '',
  content_ref TEXT NOT NULL,
  approved    INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  metadata    TEXT NOT NULL DEFAULT '{}',
  PRIMARY KEY (run_id, kind, name, version)
);

CREATE TABLE IF NOT EXISTS attentions (
  id                TEXT PRIMARY KEY,
  run_id            TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  kind              TEXT NOT NULL,
  status            TEXT NOT NULL,
  blocking          INTEGER NOT NULL DEFAULT 0,
  title             TEXT NOT NULL,
  context           BLOB,
  options           TEXT NOT NULL DEFAULT '[]',
  originating_stage TEXT NOT NULL DEFAULT '',
  originating_agent TEXT NOT NULL DEFAULT '',
  answer            BLOB,
  created_at        INTEGER NOT NULL,
  answered_at       INTEGER
);

CREATE TABLE IF NOT EXISTS clarifications (
  run_id    TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
  seq       INTEGER NOT NULL,
  body      TEXT NOT NULL,
  from_user INTEGER NOT NULL DEFAULT 0,
  at        INTEGER NOT NULL,
  PRIMARY KEY (run_id, seq)
);

CREATE INDEX IF NOT EXISTS events_run_seq ON events(run_id, seq);
CREATE INDEX IF NOT EXISTS artifacts_run ON artifacts(run_id);
CREATE INDEX IF NOT EXISTS attentions_run ON attentions(run_id);
`

func (r *sqliteRepo) migrate() error {
	_, err := r.db.Exec(schema)
	return err
}

func (r *sqliteRepo) Close() error {
	if r.db == nil {
		return nil
	}
	return r.db.Close()
}

// ---------- runs ----------

func (r *sqliteRepo) CreateRun(ctx context.Context, run domain.WorkflowRun) error {
	if run.ID == "" {
		return errors.New("storage: CreateRun: id required")
	}
	if run.OriginalTask == "" {
		return errors.New("storage: CreateRun: original_task required")
	}
	agentsJSON, _ := json.Marshal(run.ActiveAgentIDs)
	actArt, _ := json.Marshal(run.ActiveArtifactVersions)
	iter, _ := json.Marshal(run.IterationCounters)
	_, err := r.db.ExecContext(ctx, `
INSERT INTO runs(id, workflow_id, workflow_version, repo_path, original_task,
                 state, stage, terminal_reason, active_agent_ids,
                 active_artifact_ver, iteration_counters, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`,
		run.ID, run.WorkflowID, run.WorkflowVersion, run.RepoPath, run.OriginalTask,
		string(run.State), run.Stage, run.TerminalReason, string(agentsJSON),
		string(actArt), string(iter),
		run.CreatedAt.Unix(), run.UpdatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("storage: CreateRun %s: %w", run.ID, err)
	}
	return nil
}

func (r *sqliteRepo) GetRun(ctx context.Context, id string) (domain.WorkflowRun, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, workflow_id, workflow_version, repo_path, original_task,
       state, stage, terminal_reason, active_agent_ids,
       active_artifact_ver, iteration_counters, created_at, updated_at
FROM runs WHERE id = ?`, id)
	return scanRun(row)
}

type rowScanner interface {
	Scan(dest... any) error
}

func scanRun(s rowScanner) (domain.WorkflowRun, error) {
	var (
		run          domain.WorkflowRun
		state        string
		agentsJSON   string
		actArtJSON   string
		iterJSON     string
		createdAt    int64
		updatedAt    int64
	)
	if err := s.Scan(&run.ID, &run.WorkflowID, &run.WorkflowVersion, &run.RepoPath,
		&run.OriginalTask, &state, &run.Stage, &run.TerminalReason,
		&agentsJSON, &actArtJSON, &iterJSON, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.WorkflowRun{}, ErrNotFound
		}
		return domain.WorkflowRun{}, err
	}
	run.State = domain.RunState(state)
	run.CreatedAt = time.Unix(createdAt, 0).UTC()
	run.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	_ = json.Unmarshal([]byte(agentsJSON), &run.ActiveAgentIDs)
	_ = json.Unmarshal([]byte(actArtJSON), &run.ActiveArtifactVersions)
	_ = json.Unmarshal([]byte(iterJSON), &run.IterationCounters)
	return run, nil
}

func (r *sqliteRepo) ListRuns(ctx context.Context) ([]domain.WorkflowRun, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT id, workflow_id, workflow_version, repo_path, original_task,
       state, stage, terminal_reason, active_agent_ids,
       active_artifact_ver, iteration_counters, created_at, updated_at
FROM runs ORDER BY created_at DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.WorkflowRun
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, rows.Err()
}

func (r *sqliteRepo) UpdateRunState(ctx context.Context, id string, state domain.RunState, stage, terminalReason string) error {
	cur, err := r.GetRun(ctx, id)
	if err != nil {
		return err
	}
	if !domain.CanTransition(cur.State, state) {
		return fmt.Errorf("storage: invalid transition %s -> %s", cur.State, state)
	}
	if stage == "" {
		stage = cur.Stage
	}
	_, err = r.db.ExecContext(ctx, `
UPDATE runs SET state = ?, stage = ?, terminal_reason = ?, updated_at = ? WHERE id = ?`,
		string(state), stage, terminalReason, time.Now().UTC().Unix(), id)
	return err
}

func (r *sqliteRepo) UpdateRunAgents(ctx context.Context, id string, agentIDs []string) error {
	if _, err := r.GetRun(ctx, id); err != nil {
		return err
	}
	b, _ := json.Marshal(agentIDs)
	_, err := r.db.ExecContext(ctx,
		`UPDATE runs SET active_agent_ids = ?, updated_at = ? WHERE id = ?`,
		string(b), time.Now().UTC().Unix(), id)
	return err
}

func (r *sqliteRepo) IncrementIteration(ctx context.Context, id, stage string) (int, error) {
	cur, err := r.GetRun(ctx, id)
	if err != nil {
		return 0, err
	}
	if cur.IterationCounters == nil {
		cur.IterationCounters = map[string]int{}
	}
	cur.IterationCounters[stage]++
	b, _ := json.Marshal(cur.IterationCounters)
	if _, err := r.db.ExecContext(ctx,
		`UPDATE runs SET iteration_counters = ?, updated_at = ? WHERE id = ?`,
		string(b), time.Now().UTC().Unix(), id); err != nil {
		return 0, err
	}
	return cur.IterationCounters[stage], nil
}

// ---------- events ----------

func (r *sqliteRepo) AppendEvent(ctx context.Context, runID string, kind domain.EventKind, at time.Time, data json.RawMessage) (int64, error) {
	if _, err := r.GetRun(ctx, runID); err != nil {
		return 0, err
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var prev int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM events WHERE run_id = ?`, runID).Scan(&prev); err != nil {
		return 0, err
	}
	seq := prev + 1
	if _, err := tx.ExecContext(ctx, `
INSERT INTO events(run_id, seq, prev_seq, kind, at, data)
VALUES (?, ?, ?, ?, ?, ?)`,
		runID, seq, prev, string(kind), at.Unix(), []byte(data)); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

func (r *sqliteRepo) EventsAfter(ctx context.Context, runID string, afterSeq int64) ([]domain.Event, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT run_id, seq, prev_seq, kind, at, data FROM events
WHERE run_id = ? AND seq > ?
ORDER BY seq ASC`, runID, afterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		var (
			e    domain.Event
			kind string
			at   int64
			data []byte
		)
		if err := rows.Scan(&e.RunID, &e.Seq, &e.PrevSeq, &kind, &at, &data); err != nil {
			return nil, err
		}
		e.Kind = domain.EventKind(kind)
		e.At = time.Unix(at, 0).UTC()
		if len(data) > 0 {
			e.Data = json.RawMessage(data)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------- artifacts ----------

func (r *sqliteRepo) AddArtifact(ctx context.Context, a *domain.Artifact) error {
	if _, err := r.GetRun(ctx, a.RunID); err != nil {
		return err
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	if a.ID == "" {
		a.ID = fmt.Sprintf("art-%s-%s", a.RunID, randSuffix())
	}
	meta, _ := json.Marshal(orEmpty(a.Metadata))
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var nextVersion int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM artifacts WHERE run_id = ? AND kind = ? AND name = ?`,
		a.RunID, string(a.Kind), a.Name).Scan(&nextVersion); err != nil {
		return err
	}
	a.Version = nextVersion + 1
	approved := 0
	if a.Approved {
		approved = 1
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO artifacts(run_id, id, kind, name, version, stage, agent_id,
                      content_ref, approved, created_at, metadata)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.RunID, a.ID, string(a.Kind), a.Name, a.Version, a.Stage, a.AgentID,
		a.ContentRef, approved, a.CreatedAt.Unix(), string(meta)); err != nil {
		return err
	}

	var curActiveJSON string
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(active_artifact_ver, '{}') FROM runs WHERE id = ?`,
		a.RunID).Scan(&curActiveJSON); err != nil {
		return err
	}
	// Update active artifact pointer on the run. We rewrite the whole
	// JSON string in Go to avoid SQLite json_set path-quoting quirks
	// around dynamic kind/name keys.
	var active map[string]map[string]int
	if err := json.Unmarshal([]byte(curActiveJSON), &active); err != nil {
		return fmt.Errorf("storage: parse active_artifact_ver: %w", err)
	}
	if active == nil {
		active = map[string]map[string]int{}
	}
	if active[string(a.Kind)] == nil {
		active[string(a.Kind)] = map[string]int{}
	}
	active[string(a.Kind)][a.Name] = a.Version
	newActive, err := json.Marshal(active)
	if err != nil {
		return fmt.Errorf("storage: marshal active_artifact_ver: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE runs SET active_artifact_ver = ?, updated_at = ? WHERE id = ?`,
		string(newActive), time.Now().UTC().Unix(), a.RunID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *sqliteRepo) ListArtifacts(ctx context.Context, runID string) ([]domain.Artifact, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT run_id, id, kind, name, version, stage, agent_id, content_ref,
       approved, created_at, metadata
FROM artifacts WHERE run_id = ?
ORDER BY kind, name, version ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Artifact
	for rows.Next() {
		var (
			a        domain.Artifact
			kind     string
			approved int
			created  int64
			meta     string
		)
		if err := rows.Scan(&a.RunID, &a.ID, &kind, &a.Name, &a.Version, &a.Stage, &a.AgentID,
			&a.ContentRef, &approved, &created, &meta); err != nil {
			return nil, err
		}
		a.Kind = domain.ArtifactKind(kind)
		a.Approved = approved != 0
		a.CreatedAt = time.Unix(created, 0).UTC()
		if meta != "" {
			_ = json.Unmarshal([]byte(meta), &a.Metadata)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *sqliteRepo) ActiveArtifactVersion(ctx context.Context, runID string, kind domain.ArtifactKind, name string) (int, error) {
	var v int
	err := r.db.QueryRowContext(ctx, `
SELECT COALESCE(
  json_extract(active_artifact_ver, '$.' || ? || '.' || ?),
  0
) FROM runs WHERE id = ?`, string(kind), name, runID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return v, nil
}

// ---------- attentions ----------

func (r *sqliteRepo) CreateAttention(ctx context.Context, a domain.Attention) error {
	if _, err := r.GetRun(ctx, a.RunID); err != nil {
		return err
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	if a.Status == "" {
		a.Status = domain.AttentionOpen
	}
	if !domain.CanTransitionAttention("", a.Status) && a.Status != domain.AttentionOpen {
		return fmt.Errorf("storage: invalid initial attention status %q", a.Status)
	}
	blocking := 0
	if a.Blocking {
		blocking = 1
	}
	options, _ := json.Marshal(a.Options)
	var ctxBytes []byte
	if len(a.Context) > 0 {
		ctxBytes = []byte(a.Context)
	}
	_, err := r.db.ExecContext(ctx, `
INSERT INTO attentions(id, run_id, kind, status, blocking, title, context,
                       options, originating_stage, originating_agent,
                       created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.RunID, string(a.Kind), string(a.Status), blocking, a.Title,
		ctxBytes, string(options), a.OriginatingStage, a.OriginatingAgent,
		a.CreatedAt.Unix())
	return err
}

func (r *sqliteRepo) GetAttention(ctx context.Context, id string) (domain.Attention, error) {
	row := r.db.QueryRowContext(ctx, `
SELECT id, run_id, kind, status, blocking, title, context, options,
       originating_stage, originating_agent, answer, created_at, answered_at
FROM attentions WHERE id = ?`, id)
	var (
		a          domain.Attention
		kind       string
		status     string
		blocking   int
		options    string
		ctxRaw     []byte
		answerRaw  []byte
		createdAt  int64
		answeredAt sql.NullInt64
	)
	if err := row.Scan(&a.ID, &a.RunID, &kind, &status, &blocking, &a.Title,
		&ctxRaw, &options, &a.OriginatingStage, &a.OriginatingAgent,
		&answerRaw, &createdAt, &answeredAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Attention{}, ErrNotFound
		}
		return domain.Attention{}, err
	}
	a.Kind = domain.AttentionKind(kind)
	a.Status = domain.AttentionStatus(status)
	a.Blocking = blocking != 0
	a.CreatedAt = time.Unix(createdAt, 0).UTC()
	if answeredAt.Valid {
		a.AnsweredAt = time.Unix(answeredAt.Int64, 0).UTC()
	}
	if len(ctxRaw) > 0 {
		a.Context = json.RawMessage(ctxRaw)
	}
	if len(answerRaw) > 0 {
		a.Answer = json.RawMessage(answerRaw)
	}
	_ = json.Unmarshal([]byte(options), &a.Options)
	return a, nil
}

func (r *sqliteRepo) ListAttention(ctx context.Context, runID string) ([]domain.Attention, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT id FROM attentions WHERE run_id = ? ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Attention
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		a, err := r.GetAttention(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *sqliteRepo) AnswerAttention(ctx context.Context, id string, answer json.RawMessage, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	cur, err := r.GetAttention(ctx, id)
	if err != nil {
		return err
	}
	if !domain.CanTransitionAttention(cur.Status, domain.AttentionAnswered) {
		return fmt.Errorf("storage: attention %s already in terminal state %q", id, cur.Status)
	}
	if len(answer) == 0 {
		answer = json.RawMessage(`null`)
	}
	_, err = r.db.ExecContext(ctx, `
UPDATE attentions SET status = ?, answer = ?, answered_at = ? WHERE id = ?`,
		string(domain.AttentionAnswered), []byte(answer), at.Unix(), id)
	return err
}

// ---------- clarifications ----------

func (r *sqliteRepo) AppendClarification(ctx context.Context, runID string, c domain.Clarification) error {
	if _, err := r.GetRun(ctx, runID); err != nil {
		return err
	}
	if c.At.IsZero() {
		c.At = time.Now().UTC()
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prev int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM clarifications WHERE run_id = ?`, runID).Scan(&prev); err != nil {
		return err
	}
	c.Seq = prev + 1
	fromUser := 0
	if c.FromUser {
		fromUser = 1
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO clarifications(run_id, seq, body, from_user, at) VALUES (?, ?, ?, ?, ?)`,
		runID, c.Seq, c.Body, fromUser, c.At.Unix()); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *sqliteRepo) Clarifications(ctx context.Context, runID string) ([]domain.Clarification, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT seq, body, from_user, at FROM clarifications
WHERE run_id = ? ORDER BY seq ASC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Clarification
	for rows.Next() {
		var (
			c        domain.Clarification
			fromUser int
			at       int64
		)
		if err := rows.Scan(&c.Seq, &c.Body, &fromUser, &at); err != nil {
			return nil, err
		}
		c.FromUser = fromUser != 0
		c.At = time.Unix(at, 0).UTC()
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------- helpers ----------

func orEmpty(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
