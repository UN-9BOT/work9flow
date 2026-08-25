// Package runtime hosts work9flowd's HTTP server, lifecycle and the
// MVP-level handlers. It owns no workflow business logic — only
// transport. All state lives behind a storage.Repo.
package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/log"

	"github.com/unbot/work9flow/internal/domain"
	"github.com/unbot/work9flow/internal/protocol"
	"github.com/unbot/work9flow/internal/storage"
)

// Options configures a Server.
type Options struct {
	Name    string
	Version string
	Addr    string // "host:port" or ":port"; ":0" picks a free port.
	Logger  *log.Logger
	Repo    storage.Repo // optional; nil means the legacy in-memory contract
	Now     func() time.Time // injectable clock; defaults to time.Now
}

// Server is the work9flowd HTTP frontend.
type Server struct {
	opts   Options
	mux    *http.ServeMux
	http   *http.Server
	mu     sync.Mutex
	addr   string
	bootAt time.Time
	logger *log.Logger
	now    func() time.Time
}

// New returns a Server configured from opts.
func New(opts Options) *Server {
	logger := opts.Logger
	if logger == nil {
		logger = log.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	mux := http.NewServeMux()
	s := &Server{
		opts:   opts,
		mux:    mux,
		bootAt: now(),
		logger: logger,
		now:    now,
	}
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/version", s.handleVersion)
	mux.HandleFunc("GET /v1/runs", s.handleRunsList)
	mux.HandleFunc("POST /v1/runs", s.handleRunsCreate)
	mux.HandleFunc("GET /v1/runs/{id}", s.handleRunGet)
	mux.HandleFunc("DELETE /v1/runs/{id}", s.handleRunCancel)
	mux.HandleFunc("GET /v1/runs/{id}/events", s.handleRunEvents)
	mux.HandleFunc("GET /v1/runs/{id}/artifacts", s.handleRunArtifacts)
	mux.HandleFunc("GET /v1/runs/{id}/attentions", s.handleRunAttentions)
	mux.HandleFunc("POST /v1/attentions/{id}/answer", s.handleAttentionAnswer)
	mux.HandleFunc("POST /v1/runs/{id}/steer", s.handleRunSteer)
	mux.HandleFunc("POST /v1/runs/{id}/followup", s.handleRunFollowup)
	mux.Handle("/", http.HandlerFunc(jsonNotFound))
	return s
}

// Listen binds the configured address.
func (s *Server) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", s.opts.Addr, err)
	}
	s.mu.Lock()
	s.addr = ln.Addr().String()
	s.mu.Unlock()
	return ln, nil
}

// Serve runs the HTTP loop on ln until Shutdown is called.
func (s *Server) Serve(ln net.Listener) error {
	s.http = &http.Server{
		Handler:      s.mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}
	s.logger.Info("runtime listening", "addr", ln.Addr().String())
	err := s.http.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Addr returns the bound address or "" if not listening yet.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

// Shutdown stops the server and waits for in-flight requests.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.http == nil {
		return nil
	}
	return s.http.Shutdown(ctx)
}

// Repo exposes the underlying storage for callers (e.g. the DSH
// adapter) that need to persist events. Returns nil when no Repo
// was wired.
func (s *Server) Repo() storage.Repo {
	return s.opts.Repo
}

// ---------- handlers ----------

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, protocol.HealthResponse{
		Status:  "ok",
		Version: s.opts.Version,
		UptimeS: int64(s.now().Sub(s.bootAt).Seconds()),
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, protocol.VersionResponse{
		Name:    s.opts.Name,
		Version: s.opts.Version,
	})
}

func (s *Server) handleRunsList(w http.ResponseWriter, r *http.Request) {
	if s.opts.Repo == nil {
		writeJSON(w, http.StatusOK, protocol.RunListResponse{Runs: nil})
		return
	}
	runs, err := s.opts.Repo.ListRuns(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_runs_failed", err)
		return
	}
	out := protocol.RunListResponse{Runs: make([]protocol.RunSummary, 0, len(runs))}
	for _, run := range runs {
		pending, _, _, err := s.runCounts(r.Context(), run.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "counts_failed", err)
			return
		}
		out.Runs = append(out.Runs, protocol.SummaryFromRun(run, pending))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRunsCreate(w http.ResponseWriter, r *http.Request) {
	if s.opts.Repo == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage", errors.New("runtime has no storage wired"))
		return
	}
	var req protocol.RunCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err)
		return
	}
	if req.WorkflowID == "" || req.OriginalTask == "" {
		writeError(w, http.StatusBadRequest, "missing_field", errors.New("workflow_id and original_task are required"))
		return
	}
	now := s.now().UTC()
	run := domain.WorkflowRun{
		ID:              newRunID(now),
		WorkflowID:      req.WorkflowID,
		WorkflowVersion: req.WorkflowVersion,
		RepoPath:        req.RepoPath,
		OriginalTask:    req.OriginalTask,
		State:           domain.RunNew,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.opts.Repo.CreateRun(r.Context(), run); err != nil {
		writeError(w, http.StatusInternalServerError, "create_run_failed", err)
		return
	}
	if _, err := s.opts.Repo.AppendEvent(r.Context(), run.ID, domain.EventKindWorkflowCreated, now, nil); err != nil {
		writeError(w, http.StatusInternalServerError, "append_event_failed", err)
		return
	}
	detail := protocol.FromRun(run, 0, 1, 0)
	writeJSON(w, http.StatusCreated, protocol.RunCreateResponse{Run: detail})
}

func (s *Server) handleRunGet(w http.ResponseWriter, r *http.Request) {
	if s.opts.Repo == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage", errors.New("runtime has no storage wired"))
		return
	}
	id := r.PathValue("id")
	run, err := s.opts.Repo.GetRun(r.Context(), id)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err)
			return
		}
		writeError(w, http.StatusInternalServerError, "get_run_failed", err)
		return
	}
	pending, events, artifacts, err := s.runCounts(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "counts_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.RunGetResponse{Run: protocol.FromRun(run, pending, events, artifacts)})
}

func (s *Server) handleRunCancel(w http.ResponseWriter, r *http.Request) {
	if s.opts.Repo == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage", errors.New("runtime has no storage wired"))
		return
	}
	id := r.PathValue("id")
	if err := s.opts.Repo.UpdateRunState(r.Context(), id, domain.RunCanceled, "", "user canceled"); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err)
			return
		}
		writeError(w, http.StatusConflict, "cancel_rejected", err)
		return
	}
	if _, err := s.opts.Repo.AppendEvent(r.Context(), id, domain.EventKindWorkflowCanceled, s.now().UTC(), nil); err != nil {
		writeError(w, http.StatusInternalServerError, "append_event_failed", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request) {
	if s.opts.Repo == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage", errors.New("runtime has no storage wired"))
		return
	}
	id := r.PathValue("id")
	var after int64
	if q := r.URL.Query().Get("after"); q != "" {
		v, err := strconv.ParseInt(q, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_cursor", err)
			return
		}
		after = v
	}
	events, err := s.opts.Repo.EventsAfter(r.Context(), id, after)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_events_failed", err)
		return
	}
	out := protocol.EventListResponse{Events: make([]protocol.EventDTO, 0, len(events))}
	var latest int64
	for _, e := range events {
		out.Events = append(out.Events, protocol.FromEvent(e))
		if e.Seq > latest {
			latest = e.Seq
		}
	}
	out.LatestSeq = latest
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRunArtifacts(w http.ResponseWriter, r *http.Request) {
	if s.opts.Repo == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage", errors.New("runtime has no storage wired"))
		return
	}
	id := r.PathValue("id")
	arts, err := s.opts.Repo.ListArtifacts(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_artifacts_failed", err)
		return
	}
	out := protocol.ArtifactListResponse{Artifacts: make([]protocol.ArtifactDTO, 0, len(arts))}
	for _, a := range arts {
		out.Artifacts = append(out.Artifacts, protocol.FromArtifact(a))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRunAttentions(w http.ResponseWriter, r *http.Request) {
	if s.opts.Repo == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage", errors.New("runtime has no storage wired"))
		return
	}
	id := r.PathValue("id")
	atts, err := s.opts.Repo.ListAttention(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_attentions_failed", err)
		return
	}
	out := protocol.AttentionListResponse{Attentions: make([]protocol.AttentionDTO, 0, len(atts))}
	for _, a := range atts {
		out.Attentions = append(out.Attentions, protocol.FromAttention(a))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleAttentionAnswer(w http.ResponseWriter, r *http.Request) {
	if s.opts.Repo == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage", errors.New("runtime has no storage wired"))
		return
	}
	id := r.PathValue("id")
	var req protocol.AttentionAnswerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err)
		return
	}
	if err := s.opts.Repo.AnswerAttention(r.Context(), id, req.Answer, s.now().UTC()); err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			writeError(w, http.StatusNotFound, "not_found", err)
			return
		}
		writeError(w, http.StatusConflict, "answer_rejected", err)
		return
	}
	got, err := s.opts.Repo.GetAttention(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get_attention_failed", err)
		return
	}
	if _, err := s.opts.Repo.AppendEvent(r.Context(), got.RunID, domain.EventKindAttentionResolved, s.now().UTC(), mustJSON(map[string]string{"attention_id": id})); err != nil {
		writeError(w, http.StatusInternalServerError, "append_event_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, protocol.AttentionAnswerResponse{Attention: protocol.FromAttention(got)})
}

func (s *Server) handleRunSteer(w http.ResponseWriter, r *http.Request) {
	if s.opts.Repo == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage", errors.New("runtime has no storage wired"))
		return
	}
	id := r.PathValue("id")
	var req protocol.SteerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err)
		return
	}
	if req.Message == "" {
		writeError(w, http.StatusBadRequest, "missing_field", errors.New("message is required"))
		return
	}
	seq, err := s.opts.Repo.AppendEvent(r.Context(), id, domain.EventKindSteerSent, s.now().UTC(), mustJSON(req))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "append_event_failed", err)
		return
	}
	writeJSON(w, http.StatusAccepted, protocol.SteerFollowupResponse{EventSeq: seq})
}

func (s *Server) handleRunFollowup(w http.ResponseWriter, r *http.Request) {
	if s.opts.Repo == nil {
		writeError(w, http.StatusServiceUnavailable, "no_storage", errors.New("runtime has no storage wired"))
		return
	}
	id := r.PathValue("id")
	var req protocol.FollowupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err)
		return
	}
	if req.Message == "" {
		writeError(w, http.StatusBadRequest, "missing_field", errors.New("message is required"))
		return
	}
	seq, err := s.opts.Repo.AppendEvent(r.Context(), id, domain.EventKindFollowupSent, s.now().UTC(), mustJSON(req))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "append_event_failed", err)
		return
	}
	writeJSON(w, http.StatusAccepted, protocol.SteerFollowupResponse{EventSeq: seq})
}

// ---------- helpers ----------

// runCounts returns (pending_attention, event_count, artifact_count)
// for a run. All three are computed with cheap queries; the runtime
// uses them to render RunDetail without re-fetching large blobs.
func (s *Server) runCounts(ctx context.Context, runID string) (int, int, int, error) {
	atts, err := s.opts.Repo.ListAttention(ctx, runID)
	if err != nil {
		return 0, 0, 0, err
	}
	pending := 0
	for _, a := range atts {
		if a.Status == domain.AttentionOpen {
			pending++
		}
	}
	events, err := s.opts.Repo.EventsAfter(ctx, runID, 0)
	if err != nil {
		return 0, 0, 0, err
	}
	arts, err := s.opts.Repo.ListArtifacts(ctx, runID)
	if err != nil {
		return 0, 0, 0, err
	}
	return pending, len(events), len(arts), nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code string, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	writeJSON(w, status, protocol.ErrorResponse{Error: code, Message: msg})
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// newRunID returns a time-prefixed ID. Crypto-random suffix not needed
// at the bootstrap stage; we can swap to a UUID library later. The
// uniqueness guarantee comes from the storage PRIMARY KEY.
func newRunID(now time.Time) string {
	return fmt.Sprintf("run-%d", now.UnixNano())
}

// pathID is a small helper for routes that need to inspect the {id}
// segment; kept here so callers don't reach into http.Request fields.
func pathID(r *http.Request, key string) string {
	if v := r.PathValue(key); v != "" {
		return v
	}
	return strings.TrimPrefix(r.URL.Path, "/v1/runs/")
}
